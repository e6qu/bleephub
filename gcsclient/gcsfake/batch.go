package gcsfake

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
)

// batch is the JSON API's batch endpoint: a multipart/mixed body whose every
// part is a whole HTTP request, answered by one whose every part is a whole
// HTTP response. The batch answers 200 whatever became of the calls inside it;
// it is refused as a whole only where it is not a batch, or is too large a one.
//
// The answers are written in the reverse of the order the calls were made in.
// The documentation promises no order — "the server may perform your requests
// in any order" — and ties an answer to its call by Content-ID alone, so a
// client that matched them up by position would be wrong on the service some of
// the time and is wrong here all of the time.
// https://docs.cloud.google.com/storage/docs/batch
func (f *Server) batch(w http.ResponseWriter, r *http.Request, body []byte) {
	if refusal := f.authorize(r); refusal != nil {
		writeError(w, refusal)
		return
	}
	mediaType, parameters, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if r.Method != http.MethodPost || err != nil || mediaType != "multipart/mixed" || parameters["boundary"] == "" {
		writeError(w, &apiError{http.StatusBadRequest, "badRequest", "a batch is a POST of multipart/mixed with a boundary"})
		return
	}

	type call struct {
		contentID string
		answer    *http.Response
	}
	var calls []call
	reader := multipart.NewReader(bytes.NewReader(body), parameters["boundary"])
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeError(w, &apiError{http.StatusBadRequest, "badRequest", "the batch's body is not the multipart it says it is: " + err.Error()})
			return
		}
		if len(calls) == batchLimit {
			writeError(w, &apiError{http.StatusBadRequest, "badRequest", fmt.Sprintf("a batch holds no more than %d calls", batchLimit)})
			return
		}
		calls = append(calls, call{part.Header.Get("Content-ID"), f.perform(r, part)})
	}

	var answered bytes.Buffer
	writer := multipart.NewWriter(&answered)
	for i := len(calls) - 1; i >= 0; i-- {
		header := textproto.MIMEHeader{"Content-Type": {"application/http"}}
		if id := calls[i].contentID; id != "" {
			header.Set("Content-ID", "<response-"+strings.TrimPrefix(id, "<"))
		}
		part, err := writer.CreatePart(header)
		if err != nil {
			panic(err)
		}
		if err := calls[i].answer.Write(part); err != nil {
			panic(err)
		}
	}
	if err := writer.Close(); err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "multipart/mixed; boundary="+writer.Boundary())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(answered.Bytes())
}

// perform carries out one call of a batch and returns its answer. The batch's
// own headers apply to the call, the Content- ones aside, unless the call
// states its own; a call names only a path, and may not be an upload or a
// download.
func (f *Server) perform(outer *http.Request, part *multipart.Part) *http.Response {
	recorder := httptest.NewRecorder()
	refuse := func(message string) *http.Response {
		writeError(recorder, &apiError{http.StatusBadRequest, "badRequest", message})
		return recorder.Result()
	}
	if part.Header.Get("Content-Type") != "application/http" {
		return refuse("a call in a batch is a part of type application/http")
	}
	inner, err := http.ReadRequest(bufio.NewReader(part))
	if err != nil {
		return refuse("a call in a batch is a whole HTTP request: " + err.Error())
	}
	if inner.URL.IsAbs() || inner.URL.Host != "" {
		return refuse("a call in a batch names only the path of its URL")
	}
	if !strings.HasPrefix(inner.URL.EscapedPath(), "/storage/v1/") {
		return refuse("a batch carries calls of the JSON API, and neither uploads nor downloads")
	}
	for name, values := range outer.Header {
		if !strings.HasPrefix(name, "Content-") && inner.Header.Get(name) == "" {
			inner.Header[name] = values
		}
	}
	body, err := io.ReadAll(inner.Body)
	if err != nil {
		return refuse("a call's body could not be read: " + err.Error())
	}
	f.serveObjects(recorder, inner, body)
	answer := recorder.Result()
	// A recorded response does not know its own length, and one written without
	// it would be chunked, which the service's are not.
	answer.ContentLength = int64(recorder.Body.Len())
	return answer
}

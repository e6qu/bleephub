package gcsfake

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// upload is a resumable upload in progress.
type upload struct {
	bucket, name string
	contentType  string
	metadata     map[string]string
	// precondition is the ifGenerationMatch the upload was initiated with, held
	// until the upload completes, which is when there is a write to apply it to.
	precondition *int64
	// declared is the X-Upload-Content-Length the upload was initiated with, or
	// -1 where none was.
	declared int64
	data     []byte
}

// description is the object resource a client sends with an upload: the parts
// of it this fake keeps.
type description struct {
	Name        string            `json:"name"`
	ContentType string            `json:"contentType"`
	Metadata    map[string]string `json:"metadata"`
}

// serveUpload is objects.insert, at /upload/storage/v1/b/BUCKET/o, and the
// session a resumable upload goes on through, which is that same URL with an
// upload_id.
// https://docs.cloud.google.com/storage/docs/json_api/v1/objects/insert
// https://docs.cloud.google.com/storage/docs/performing-resumable-uploads
func (f *Server) serveUpload(w http.ResponseWriter, r *http.Request, body []byte) {
	parts, ok := segments(r.URL.EscapedPath(), "/upload/storage/v1/")
	if !ok || len(parts) != 3 || parts[0] != "b" || parts[2] != "o" {
		writeError(w, errNoSuchRoute)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, &apiError{http.StatusBadRequest, "badRequest", "the query string is malformed"})
		return
	}
	// A session URI is its own authorization: "it can be used by anyone to upload
	// data to the target bucket without any further authentication".
	// https://docs.cloud.google.com/storage/docs/resumable-uploads#session-uris
	if id := query.Get("upload_id"); id != "" {
		f.serveSession(w, r, id, body)
		return
	}
	if refusal := f.authorize(r); refusal != nil {
		writeError(w, refusal)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, errNoSuchRoute)
		return
	}
	if refusal := unknownParameter(query, "uploadType", "ifGenerationMatch", "fields"); refusal != nil {
		writeError(w, refusal)
		return
	}
	precondition, refusal := generationMatch(query)
	if refusal != nil {
		writeError(w, refusal)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.buckets[parts[1]]
	if !ok {
		writeError(w, errNoSuchBucket)
		return
	}
	switch query.Get("uploadType") {
	case "multipart":
		described, content, refusal := readMultipart(r.Header.Get("Content-Type"), body)
		if refusal != nil {
			writeError(w, refusal)
			return
		}
		written, refusal := f.write(held, described.Name, content, described.ContentType, described.Metadata, precondition)
		if refusal != nil {
			writeError(w, refusal)
			return
		}
		answer, refusal := selectFields(resource(parts[1], described.Name, written), query.Get("fields"))
		if refusal != nil {
			writeError(w, refusal)
			return
		}
		writeJSON(w, http.StatusOK, answer)
	case "resumable":
		f.initiate(w, r, parts[1], precondition, body)
	default:
		writeError(w, &apiError{http.StatusBadRequest, "invalidParameter", "gcsfake serves the multipart and resumable upload types"})
	}
}

// generationMatch reads ifGenerationMatch, whose zero means "no live object".
// https://docs.cloud.google.com/storage/docs/request-preconditions
func generationMatch(query url.Values) (*int64, *apiError) {
	if !query.Has("ifGenerationMatch") {
		return nil, nil
	}
	generation, err := strconv.ParseInt(query.Get("ifGenerationMatch"), 10, 64)
	if err != nil || generation < 0 {
		return nil, &apiError{http.StatusBadRequest, "invalid", "ifGenerationMatch is not a generation"}
	}
	return &generation, nil
}

// write makes an object if its precondition holds. An object that does not
// exist has no generation, which matches a precondition of zero and no other:
// the documentation says a request "proceeds if the generation of the target
// resource matches", and does not single out the object that is absent, so this
// is a reading of it — the one the fake-gcs-server emulator shares.
func (f *Server) write(held *bucketState, name string, data []byte, contentType string, metadata map[string]string, precondition *int64) (*object, *apiError) {
	if name == "" {
		return nil, &apiError{http.StatusBadRequest, "required", "Required: an object name"}
	}
	if precondition != nil {
		var live int64
		if existing, ok := held.objects[name]; ok {
			live = existing.generation
		}
		if live != *precondition {
			return nil, errCondition
		}
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return f.store(held, name, data, contentType, metadata), nil
}

// readMultipart takes apart a multipart/related upload: the object's
// description as JSON, then its content.
// https://docs.cloud.google.com/storage/docs/uploading-objects#json-api-multipart-upload
func readMultipart(contentType string, body []byte) (description, []byte, *apiError) {
	malformed := func(what string) (description, []byte, *apiError) {
		return description{}, nil, &apiError{http.StatusBadRequest, "badRequest", "a multipart upload " + what}
	}
	mediaType, parameters, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/related" || parameters["boundary"] == "" {
		return malformed("must be multipart/related with a boundary")
	}
	reader := multipart.NewReader(bytes.NewReader(body), parameters["boundary"])
	first, err := reader.NextRawPart()
	if err != nil {
		return malformed("has no first part")
	}
	if partType, _, err := mime.ParseMediaType(first.Header.Get("Content-Type")); err != nil || partType != "application/json" {
		return malformed("must begin with the object's description as application/json")
	}
	var described description
	if err := json.NewDecoder(first).Decode(&described); err != nil {
		return malformed("has a description that is not JSON: " + err.Error())
	}
	second, err := reader.NextRawPart()
	if err != nil {
		return malformed("has no second part")
	}
	content, err := io.ReadAll(second)
	if err != nil {
		return malformed("has a second part that does not end")
	}
	if _, err := reader.NextRawPart(); err != io.EOF {
		return malformed("has more than two parts, or does not close its last")
	}
	// The object's type is the description's where it names one, and otherwise
	// the media part's own.
	if described.ContentType == "" {
		described.ContentType = second.Header.Get("Content-Type")
	}
	return described, content, nil
}

// initiate opens a resumable upload and answers with its session URI. The
// precondition is not applied here: there is nothing yet to write. The caller
// holds the lock.
func (f *Server) initiate(w http.ResponseWriter, r *http.Request, bucket string, precondition *int64, body []byte) {
	var described description
	if err := json.Unmarshal(body, &described); err != nil || described.Name == "" {
		writeError(w, &apiError{http.StatusBadRequest, "required", "a resumable upload is initiated with the object's description, which names it"})
		return
	}
	if described.ContentType == "" {
		described.ContentType = r.Header.Get("X-Upload-Content-Type")
	}
	declared := int64(-1)
	if stated := r.Header.Get("X-Upload-Content-Length"); stated != "" {
		parsed, err := strconv.ParseInt(stated, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, &apiError{http.StatusBadRequest, "invalid", "X-Upload-Content-Length is not a length"})
			return
		}
		declared = parsed
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		panic(err)
	}
	id := hex.EncodeToString(random)
	f.uploads[id] = &upload{
		bucket:       bucket,
		name:         described.Name,
		contentType:  described.ContentType,
		metadata:     described.Metadata,
		precondition: precondition,
		declared:     declared,
	}
	w.Header().Set("Location", f.server.URL+"/upload/storage/v1/b/"+url.PathEscape(bucket)+"/o?uploadType=resumable&upload_id="+id)
	w.WriteHeader(http.StatusOK)
}

// serveSession takes one request of a resumable upload's session: a chunk, a
// question about how much is held, or a cancellation.
func (f *Server) serveSession(w http.ResponseWriter, r *http.Request, id string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelled[id] {
		w.WriteHeader(499)
		return
	}
	session, ok := f.uploads[id]
	if !ok {
		writeError(w, &apiError{http.StatusNotFound, "notFound", "no upload has the ID " + id + ", or it has completed"})
		return
	}
	switch r.Method {
	case http.MethodDelete:
		// https://docs.cloud.google.com/storage/docs/performing-resumable-uploads#cancel-upload
		if r.ContentLength != 0 || len(r.Header.Values("Content-Length")) == 0 {
			writeError(w, &apiError{http.StatusLengthRequired, "required", "an upload is cancelled by a DELETE that states Content-Length: 0"})
			return
		}
		delete(f.uploads, id)
		f.cancelled[id] = true
		w.WriteHeader(499)
	case http.MethodPut:
		f.takeChunk(w, id, session, r.Header.Get("Content-Range"), body)
	default:
		writeError(w, errNoSuchRoute)
	}
}

// takeChunk appends a chunk to an upload, and completes the upload when the
// chunk's Content-Range says it is the last. The caller holds the lock.
//
// A chunk must start where the last one ended, and unless it is the last must
// be a multiple of 256 KiB. "bytes */TOTAL" sends no data: it completes an
// upload that already holds TOTAL bytes, and otherwise asks how many are held.
// https://docs.cloud.google.com/storage/docs/performing-resumable-uploads#chunked-upload
func (f *Server) takeChunk(w http.ResponseWriter, id string, session *upload, contentRange string, body []byte) {
	refuse := func(format string, arguments ...any) {
		writeError(w, &apiError{http.StatusBadRequest, "invalid", fmt.Sprintf(format, arguments...)})
	}
	extent, total, found := strings.Cut(strings.TrimPrefix(contentRange, "bytes "), "/")
	if !strings.HasPrefix(contentRange, "bytes ") || !found {
		refuse("Content-Range %q is not \"bytes FIRST-LAST/TOTAL\" or \"bytes */TOTAL\"", contentRange)
		return
	}
	size := int64(-1)
	if total != "*" {
		parsed, err := strconv.ParseInt(total, 10, 64)
		if err != nil || parsed < 0 {
			refuse("Content-Range %q names a total that is not a length", contentRange)
			return
		}
		size = parsed
	}
	if size >= 0 && session.declared >= 0 && size != session.declared {
		refuse("Content-Range %q names a total other than the X-Upload-Content-Length of %d the upload was initiated with", contentRange, session.declared)
		return
	}

	held := int64(len(session.data))
	if extent == "*" {
		if len(body) != 0 {
			refuse("Content-Range %q names no bytes and the request carries %d", contentRange, len(body))
			return
		}
		if size >= 0 && held > size {
			refuse("Content-Range %q names a total of %d and the upload already holds %d bytes", contentRange, size, held)
			return
		}
	} else {
		first, last, err := parseExtent(extent)
		if err != nil || last-first+1 != int64(len(body)) {
			refuse("Content-Range %q does not describe the %d bytes the request carries", contentRange, len(body))
			return
		}
		if first != held {
			refuse("Content-Range %q starts at %d and the upload holds %d bytes: chunks go in order", contentRange, first, held)
			return
		}
		if size >= 0 && last >= size {
			refuse("Content-Range %q runs past its own total", contentRange)
			return
		}
		final := size >= 0 && last == size-1
		if !final && len(body)%chunkQuantum != 0 {
			refuse("a chunk of %d bytes is not a multiple of %d, and only the last may not be", len(body), chunkQuantum)
			return
		}
		session.data = append(session.data, body...)
		held = int64(len(session.data))
	}

	if size < 0 || held < size {
		if held > 0 {
			w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", held-1))
		}
		w.WriteHeader(http.StatusPermanentRedirect)
		return
	}
	// The upload is over whether or not the write it makes is let through.
	delete(f.uploads, id)
	bucket, ok := f.buckets[session.bucket]
	if !ok {
		writeError(w, errNoSuchBucket)
		return
	}
	written, refusal := f.write(bucket, session.name, session.data, session.contentType, session.metadata, session.precondition)
	if refusal != nil {
		writeError(w, refusal)
		return
	}
	writeJSON(w, http.StatusOK, resource(session.bucket, session.name, written))
}

func parseExtent(extent string) (first, last int64, err error) {
	from, to, found := strings.Cut(extent, "-")
	if !found {
		return 0, 0, fmt.Errorf("no dash")
	}
	if first, err = strconv.ParseInt(from, 10, 64); err != nil {
		return 0, 0, err
	}
	if last, err = strconv.ParseInt(to, 10, 64); err != nil {
		return 0, 0, err
	}
	if first < 0 || last < first {
		return 0, 0, fmt.Errorf("backwards")
	}
	return first, last, nil
}

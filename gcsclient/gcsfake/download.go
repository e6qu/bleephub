package gcsfake

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// download is the XML API's GET Object, path-style: /BUCKET/OBJECT, the object's
// "/" left bare. It is how gcsclient reads an object, because it is the download
// whose response is documented to carry the object's generation, modification
// time and metadata as headers, and it is what a signed URL addresses. It is
// authorized by an access token or by the signature in its query string.
// https://docs.cloud.google.com/storage/docs/xml-api/get-object-download
// https://docs.cloud.google.com/storage/docs/xml-api/reference-headers
func (f *Server) download(w http.ResponseWriter, r *http.Request) {
	bucket, name, ok := strings.Cut(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	bucket, bucketErr := url.PathUnescape(bucket)
	name, nameErr := url.PathUnescape(name)
	if !ok || bucket == "" || name == "" || bucketErr != nil || nameErr != nil || r.Method != http.MethodGet {
		writeXMLError(w, &xmlRefusal{http.StatusBadRequest, "InvalidURI", "gcsfake serves GET /BUCKET/OBJECT of the XML API and nothing else of it"})
		return
	}
	if r.URL.Query().Has("X-Goog-Algorithm") {
		if refusal := f.verifySignedURL(r); refusal != nil {
			writeXMLError(w, refusal)
			return
		}
	} else {
		if refusal := f.authorize(r); refusal != nil {
			writeXMLError(w, &xmlRefusal{refusal.status, "AuthenticationRequired", refusal.message})
			return
		}
		if len(r.URL.Query()) != 0 {
			writeXMLError(w, &xmlRefusal{http.StatusBadRequest, "InvalidArgument", "gcsfake implements no query parameter of a download but a signature's"})
			return
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.buckets[bucket]
	if !ok {
		writeXMLError(w, &xmlRefusal{http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist."})
		return
	}
	found, ok := held.objects[name]
	if !ok {
		writeXMLError(w, &xmlRefusal{http.StatusNotFound, "NoSuchKey", "The specified key does not exist."})
		return
	}

	size := int64(len(found.data))
	first, last, status := int64(0), size-1, http.StatusOK
	if requested := r.Header.Get("Range"); requested != "" {
		var ok bool
		first, last, ok = byteRange(requested, size)
		switch {
		case ok:
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, size))
		case f.emptyRangeAtEnd && strings.HasPrefix(requested, fmt.Sprintf("bytes=%d-", size)):
			first, last = size, size-1
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		default:
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			writeXMLError(w, &xmlRefusal{http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range cannot be satisfied."})
			return
		}
		status = http.StatusPartialContent
	}
	generation := strconv.FormatInt(found.generation, 10)
	w.Header().Set("Content-Type", found.contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(last-first+1, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Last-Modified", found.updated.Format(http.TimeFormat))
	w.Header().Set("ETag", `"`+generation+`"`)
	w.Header().Set("x-goog-generation", generation)
	w.Header().Set("x-goog-metageneration", "1")
	w.Header().Set("x-goog-stored-content-encoding", "identity")
	w.Header().Set("x-goog-stored-content-length", strconv.FormatInt(size, 10))
	for item, value := range found.metadata {
		w.Header().Set("x-goog-meta-"+item, value)
	}
	w.WriteHeader(status)
	_, _ = w.Write(found.data[first : last+1])
}

// byteRange reads "bytes=first-last" or "bytes=first-" against an object's
// size, cutting short a range that runs past the end and refusing one that
// starts there. The other forms a Range may take are refused with it: gcsclient
// sends none of them.
func byteRange(header string, size int64) (first, last int64, ok bool) {
	spec, found := strings.CutPrefix(header, "bytes=")
	from, to, dashed := strings.Cut(spec, "-")
	first, err := strconv.ParseInt(from, 10, 64)
	if !found || !dashed || err != nil || first < 0 || first >= size {
		return 0, 0, false
	}
	last = size - 1
	if to != "" {
		parsed, err := strconv.ParseInt(to, 10, 64)
		if err != nil || parsed < first {
			return 0, 0, false
		}
		last = min(last, parsed)
	}
	return first, last, true
}

// xmlErrorBody is the XML API's error document.
type xmlErrorBody struct {
	XMLName xml.Name `xml:"Error"`
	Code    string
	Message string
}

// writeXMLError answers in the XML API's error format.
// https://docs.cloud.google.com/storage/docs/xml-api/reference-status
func writeXMLError(w http.ResponseWriter, refusal *xmlRefusal) {
	document, err := xml.Marshal(xmlErrorBody{Code: refusal.code, Message: refusal.message})
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "application/xml; charset=UTF-8")
	w.WriteHeader(refusal.status)
	_, _ = w.Write(append([]byte("<?xml version='1.0' encoding='UTF-8'?>"), document...))
}

// Package s3fake is an in-process object store that speaks enough of the S3
// REST API for the minio-go client and counts every request and byte that
// crosses it. It is the measuring instrument for gitstore's tests and
// benchmarks, exported so a harness outside this module can point any
// S3-speaking implementation at the same instrument.
package s3fake

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Server is an in-process object store that speaks enough of the S3 REST API
// for the minio-go client, and counts every request and every byte that
// crosses it. Counting is the entire point: the cost this package exists to
// reduce is measured in S3 requests, and a request counter that lives inside
// the test binary is exact, whereas a counter derived from a real endpoint's
// access log is sampled and delayed.
//
// The MinIO container the server package uses is a conformance fixture — it
// answers whether bleephub speaks S3 correctly. It cannot answer how many
// requests an operation costs without parsing container logs, and it costs a
// docker pull. Both harnesses are wanted; this one is the measuring instrument.
type Server struct {
	server *httptest.Server

	mu      sync.Mutex
	objects map[string][]byte
	uploads map[string]map[int][]byte
	// metadata holds each object's x-amz-meta-* headers. Designs that keep a
	// fact about an object beside its bytes — a git object's type, say — read
	// it back from here, and would misbehave against a store that dropped it.
	metadata map[string]http.Header

	// latency is slept before answering each request, standing in for the
	// round trip to a real endpoint. A benchmark with zero latency measures
	// only CPU and hides the fact that request count is what dominates.
	latency time.Duration
	// failOn makes a chosen request fail, which is how a crash part way
	// through a multi-step publication is reproduced deterministically.
	failOn func(method, key string) bool
	// onRequest runs before a request is served, which is how another
	// replica's concurrent write or deletion is interleaved at an exact point
	// in this replica's work.
	onRequest func(method, key string)
	// trace sees every request whole, so an operator reading a trace can tell
	// one listing from another by its prefix and one ranged read by its extent.
	trace func(*http.Request)

	counts Counts
}

// Counts is the measurement. Requests are counted per operation because the
// operations have wildly different costs: a LIST returns up to a thousand keys
// for one round trip, a GET returns one object.
type Counts struct {
	Get         int64
	GetRanged   int64
	Head        int64
	Put         int64
	List        int64
	Delete      int64
	Copy        int64
	Multipart   int64
	BytesDown   int64
	BytesUp     int64
	NotFoundGet int64
}

// Total is the number of requests of every kind.
func (c Counts) Total() int64 {
	return c.Get + c.GetRanged + c.Head + c.Put + c.List + c.Delete + c.Copy + c.Multipart
}

// Sub returns the counts accrued since prev.
func (c Counts) Sub(prev Counts) Counts {
	return Counts{
		Get:         c.Get - prev.Get,
		GetRanged:   c.GetRanged - prev.GetRanged,
		Head:        c.Head - prev.Head,
		Put:         c.Put - prev.Put,
		List:        c.List - prev.List,
		Delete:      c.Delete - prev.Delete,
		Copy:        c.Copy - prev.Copy,
		Multipart:   c.Multipart - prev.Multipart,
		BytesDown:   c.BytesDown - prev.BytesDown,
		BytesUp:     c.BytesUp - prev.BytesUp,
		NotFoundGet: c.NotFoundGet - prev.NotFoundGet,
	}
}

func (c Counts) String() string {
	return fmt.Sprintf("total=%d get=%d ranged=%d head=%d put=%d list=%d delete=%d copy=%d multipart=%d down=%dB up=%dB 404=%d",
		c.Total(), c.Get, c.GetRanged, c.Head, c.Put, c.List, c.Delete, c.Copy, c.Multipart, c.BytesDown, c.BytesUp, c.NotFoundGet)
}

// New starts a server on a loopback port. The caller closes it.
func New() *Server {
	f := &Server{
		objects:  map[string][]byte{},
		uploads:  map[string]map[int][]byte{},
		metadata: map[string]http.Header{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// Client builds an object-store client wired to this fake. Credentials are
// static literals so nothing consults a credential file, an instance metadata
// endpoint or an operating system credential store; path-style addressing and a
// pinned region keep the client from ever issuing a bucket-location lookup that
// would pollute the request counters. Retries are disabled so an injected
// failure is observed exactly once.
func (f *Server) Client() *minio.Core {
	u, err := url.Parse(f.server.URL)
	if err != nil {
		panic(err)
	}
	core, err := minio.NewCore(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4("fake", "fake", ""),
		Secure:       false,
		Region:       "us-east-1",
		BucketLookup: minio.BucketLookupPath,
		MaxRetries:   1,
	})
	if err != nil {
		panic(err)
	}
	return core
}

// Listen starts a server on addr rather than a port of the system's choosing,
// for pointing a tool at the fake by hand. The caller closes it.
func Listen(addr string) (*Server, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	f := &Server{
		objects:  map[string][]byte{},
		uploads:  map[string]map[int][]byte{},
		metadata: map[string]http.Header{},
	}
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	_ = f.server.Listener.Close()
	f.server.Listener = listener
	f.server.Start()
	return f, nil
}

// URL is the endpoint the server listens on.
func (f *Server) URL() string { return f.server.URL }

// Close shuts the server down.
func (f *Server) Close() { f.server.Close() }

// Snapshot returns the counts so far.
func (f *Server) Snapshot() Counts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts
}

// Reset zeroes the counts.
func (f *Server) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts = Counts{}
}

// SetLatency sets the delay slept before answering each request.
func (f *Server) SetLatency(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latency = d
}

// SetFailOn makes the requests fail selects answer 500; nil clears it.
func (f *Server) SetFailOn(fail func(method, key string) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOn = fail
}

// SetTrace installs an observer handed every request before it is served; nil
// clears it. The observer must not read the request body.
func (f *Server) SetTrace(trace func(*http.Request)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trace = trace
}

// SetOnRequest installs a hook run before each request is served; nil clears it.
func (f *Server) SetOnRequest(hook func(method, key string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onRequest = hook
}

// KeysWithPrefix lists stored keys under prefix, sorted.
func (f *Server) KeysWithPrefix(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// Put stores an object directly, uncounted.
func (f *Server) Put(key string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = append([]byte(nil), data...)
}

// Get reads an object directly, uncounted.
func (f *Server) Get(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	return append([]byte(nil), data...), ok
}

// Remove deletes an object directly, uncounted.
func (f *Server) Remove(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	delete(f.metadata, key)
}

// etagOf is an object's entity tag: a function of its bytes alone, so equal
// tags mean equal contents and a rewrite with the same bytes keeps its tag,
// which is all that conditional writes and revalidation rely on. S3's own is an
// MD5 for a simple upload; clients treat the tag as opaque, so this one is a
// SHA-256 cut to the same 32 hex digits.
func etagOf(data []byte) string {
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// preconditionFails applies If-None-Match and If-Match to a write. Conditional
// writes are how designs with no lock service take a lock or swap a manifest,
// so a fake that ignored them would let every contender win. Must be called
// with f.mu held.
func (f *Server) preconditionFails(r *http.Request, key string) bool {
	existing, exists := f.objects[key]
	if match := r.Header.Get("If-None-Match"); match != "" {
		if exists && (match == "*" || sameETag(match, etagOf(existing))) {
			return true
		}
	}
	if match := r.Header.Get("If-Match"); match != "" {
		if !exists || (match != "*" && !sameETag(match, etagOf(existing))) {
			return true
		}
	}
	return false
}

// sameETag compares entity tags as S3 does: with or without their quotes.
// Clients differ — minio-go sends the tag quoted, the AWS SDK for Rust as the
// caller gave it — and a store that insisted on one form would fail the other's
// every compare-and-swap.
func sameETag(a, b string) bool {
	return strings.Trim(a, `"`) == strings.Trim(b, `"`)
}

// userMetadata picks out the headers S3 stores with an object and returns on
// every read of it.
func userMetadata(header http.Header) http.Header {
	kept := http.Header{}
	for name, values := range header {
		if strings.HasPrefix(name, "X-Amz-Meta-") {
			kept[name] = append([]string(nil), values...)
		}
	}
	return kept
}

// writeUserMetadata must be called with f.mu held.
func (f *Server) writeUserMetadata(w http.ResponseWriter, key string) {
	for name, values := range f.metadata[key] {
		w.Header()[name] = values
	}
}

// keyOf strips the leading "/bucket/" of a path-style request URL.
func keyOf(p string) (bucket, key string) {
	trimmed := strings.TrimPrefix(p, "/")
	bucket, key, _ = strings.Cut(trimmed, "/")
	return bucket, key
}

func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	latency := f.latency
	f.mu.Unlock()
	if latency > 0 {
		time.Sleep(latency)
	}

	_, key := keyOf(r.URL.Path)
	query := r.URL.Query()

	f.mu.Lock()
	fail := f.failOn
	hook := f.onRequest
	trace := f.trace
	f.mu.Unlock()
	if trace != nil {
		trace(r)
	}
	if hook != nil {
		hook(r.Method, key)
	}
	if fail != nil && fail(r.Method, key) {
		_, _ = io.Copy(io.Discard, r.Body)
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "injected failure")
		return
	}

	switch {
	case r.Method == http.MethodHead && key == "":
		// Every bucket exists: objects are keyed without regard to one, so a
		// client that checks for its bucket before using it must be told yes.
		f.mu.Lock()
		f.counts.Head++
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && key == "":
		f.serveList(w, query)
	case r.Method == http.MethodPost && query.Has("delete"):
		f.serveDeleteObjects(w, r)
	case r.Method == http.MethodPost && query.Has("uploads"):
		f.serveCreateMultipart(w, key)
	case r.Method == http.MethodPost && query.Has("uploadId"):
		f.serveCompleteMultipart(w, r, key, query.Get("uploadId"))
	case r.Method == http.MethodDelete && query.Has("uploadId"):
		f.serveAbortMultipart(w, query.Get("uploadId"))
	case r.Method == http.MethodPut && query.Has("uploadId"):
		f.serveUploadPart(w, r, query.Get("uploadId"), query.Get("partNumber"))
	case r.Method == http.MethodPut && r.Header.Get("x-amz-copy-source") != "":
		f.serveCopy(w, r, key)
	case r.Method == http.MethodPut:
		f.servePut(w, r, key)
	case r.Method == http.MethodHead:
		f.serveHead(w, key)
	case r.Method == http.MethodGet:
		f.serveGet(w, r, key)
	case r.Method == http.MethodDelete:
		f.serveDelete(w, key)
	default:
		http.Error(w, "unsupported", http.StatusBadRequest)
	}
}

// readObjectBody reads a request body, transparently decoding the
// aws-chunked streaming-signature framing minio-go wraps PutObject and
// PutObjectPart payloads in when talking to a non-TLS endpoint (the framing a
// real S3/MinIO server unwraps). Other requests carry an unframed body.
func readObjectBody(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING") &&
		!strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
		return raw, nil
	}
	return decodeAWSChunked(raw)
}

// decodeAWSChunked concatenates the data of every chunk in a
// STREAMING-AWS4-HMAC-SHA256-PAYLOAD body, dropping the per-chunk
// "<hexsize>;chunk-signature=..." headers, the CRLFs, and any trailer.
func decodeAWSChunked(raw []byte) ([]byte, error) {
	var out []byte
	for len(raw) > 0 {
		idx := bytes.Index(raw, []byte("\r\n"))
		if idx < 0 {
			break
		}
		header := raw[:idx]
		raw = raw[idx+2:]
		sizeField := header
		if semi := bytes.IndexByte(header, ';'); semi >= 0 {
			sizeField = header[:semi]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(string(sizeField)), 16, 64)
		if err != nil {
			return nil, fmt.Errorf("aws-chunked size %q: %w", sizeField, err)
		}
		if size == 0 {
			break
		}
		if int64(len(raw)) < size {
			return nil, fmt.Errorf("aws-chunked short chunk: want %d, have %d", size, len(raw))
		}
		out = append(out, raw[:size]...)
		raw = raw[size:]
		if len(raw) >= 2 && raw[0] == '\r' && raw[1] == '\n' {
			raw = raw[2:]
		}
	}
	return out, nil
}

// writeXML marshals a response body. Object keys reach these bodies verbatim
// from the request, and a key may hold any character XML reserves, so the
// encoder's escaping is what keeps the document well formed.
func writeXML(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(body)
}

type errorResult struct {
	XMLName xml.Name `xml:"Error"`
	Code    string
	Message string
}

func writeS3Error(w http.ResponseWriter, status int, code, message string) {
	writeXML(w, status, errorResult{Code: code, Message: message})
}

func (f *Server) serveGet(w http.ResponseWriter, r *http.Request, key string) {
	data, ok := f.Get(key)
	if !ok {
		f.mu.Lock()
		f.counts.Get++
		f.counts.NotFoundGet++
		f.mu.Unlock()
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
		return
	}

	rangeHeader := r.Header.Get("Range")
	start, end := int64(0), int64(len(data))
	ranged := false
	if rangeHeader != "" {
		var perr error
		start, end, perr = parseByteRange(rangeHeader, int64(len(data)))
		if perr != nil {
			writeS3Error(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", perr.Error())
			return
		}
		ranged = true
	}
	body := data[start:end]

	f.mu.Lock()
	if ranged {
		f.counts.GetRanged++
	} else {
		f.counts.Get++
	}
	f.counts.BytesDown += int64(len(body))
	f.writeUserMetadata(w, key)
	f.mu.Unlock()

	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Last-Modified", time.Unix(0, 0).UTC().Format(http.TimeFormat))
	w.Header().Set("ETag", etagOf(data))
	if ranged {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
	}
	_, _ = w.Write(body)
}

// parseByteRange handles the two forms the read path issues: an explicit
// closed range and an open-ended suffix from an offset.
func parseByteRange(header string, size int64) (int64, int64, error) {
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok {
		return 0, 0, fmt.Errorf("unsupported range %q", header)
	}
	first, last, _ := strings.Cut(spec, "-")
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("unsupported range %q", header)
	}
	if start >= size {
		return 0, 0, fmt.Errorf("range %q beyond size %d", header, size)
	}
	end := size
	if last != "" {
		parsed, err := strconv.ParseInt(last, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("unsupported range %q", header)
		}
		end = min(parsed+1, size)
	}
	if end <= start {
		return 0, 0, fmt.Errorf("empty range %q", header)
	}
	return start, end, nil
}

func (f *Server) serveHead(w http.ResponseWriter, key string) {
	data, ok := f.Get(key)
	f.mu.Lock()
	f.counts.Head++
	if ok {
		f.writeUserMetadata(w, key)
	}
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Last-Modified", time.Unix(0, 0).UTC().Format(http.TimeFormat))
	w.Header().Set("ETag", etagOf(data))
	w.WriteHeader(http.StatusOK)
}

func (f *Server) servePut(w http.ResponseWriter, r *http.Request, key string) {
	body, err := readObjectBody(r)
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	f.mu.Lock()
	f.counts.Put++
	f.counts.BytesUp += int64(len(body))
	if f.preconditionFails(r, key) {
		f.mu.Unlock()
		writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold")
		return
	}
	f.objects[key] = body
	f.metadata[key] = userMetadata(r.Header)
	f.mu.Unlock()
	w.Header().Set("ETag", etagOf(body))
	w.WriteHeader(http.StatusOK)
}

func (f *Server) serveDelete(w http.ResponseWriter, key string) {
	f.mu.Lock()
	f.counts.Delete++
	delete(f.objects, key)
	delete(f.metadata, key)
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *Server) serveCopy(w http.ResponseWriter, r *http.Request, key string) {
	source, err := url.PathUnescape(strings.TrimPrefix(r.Header.Get("x-amz-copy-source"), "/"))
	if err != nil {
		writeS3Error(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	_, sourceKey, _ := strings.Cut(source, "/")
	f.mu.Lock()
	f.counts.Copy++
	data, ok := f.objects[sourceKey]
	if ok {
		f.objects[key] = append([]byte(nil), data...)
		// S3's default metadata directive is COPY.
		f.metadata[key] = f.metadata[sourceKey].Clone()
	}
	f.mu.Unlock()
	if !ok {
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><CopyObjectResult><ETag>"fake"</ETag></CopyObjectResult>`)
}

type listBucketResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	Name                  string   `xml:"Name"`
	Prefix                string   `xml:"Prefix"`
	KeyCount              int      `xml:"KeyCount"`
	MaxKeys               int      `xml:"MaxKeys"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken,omitempty"`
	Contents              []listContents
	CommonPrefixes        []listCommonPrefix
}

type listContents struct {
	XMLName      xml.Name `xml:"Contents"`
	Key          string   `xml:"Key"`
	Size         int64    `xml:"Size"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

type listCommonPrefix struct {
	XMLName xml.Name `xml:"CommonPrefixes"`
	Prefix  string   `xml:"Prefix"`
}

// serveList implements ListObjectsV2 with the same thousand-key page size the
// real service uses, so a benchmark counts the same number of round trips it
// would against S3.
func (f *Server) serveList(w http.ResponseWriter, query url.Values) {
	const maxKeys = 1000
	prefix := query.Get("prefix")
	delimiter := query.Get("delimiter")
	after := query.Get("continuation-token")

	f.mu.Lock()
	f.counts.List++
	keys := make([]string, 0, len(f.objects))
	sizes := map[string]int64{}
	etags := map[string]string{}
	for key, data := range f.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
			sizes[key] = int64(len(data))
			etags[key] = etagOf(data)
		}
	}
	f.mu.Unlock()
	sort.Strings(keys)

	result := listBucketResult{Name: "bucket", Prefix: prefix, MaxKeys: maxKeys}
	seenPrefix := map[string]bool{}
	emitted := 0
	lastKey := ""
	for _, key := range keys {
		if after != "" && key <= after {
			continue
		}
		if emitted >= maxKeys {
			result.IsTruncated = true
			result.NextContinuationToken = lastKey
			break
		}
		lastKey = key
		if delimiter != "" {
			rest := strings.TrimPrefix(key, prefix)
			if idx := strings.Index(rest, delimiter); idx >= 0 {
				common := prefix + rest[:idx+len(delimiter)]
				if !seenPrefix[common] {
					seenPrefix[common] = true
					result.CommonPrefixes = append(result.CommonPrefixes, listCommonPrefix{Prefix: common})
					emitted++
				}
				continue
			}
		}
		result.Contents = append(result.Contents, listContents{
			Key:          key,
			Size:         sizes[key],
			LastModified: time.Unix(0, 0).UTC().Format(time.RFC3339),
			ETag:         etags[key],
		})
		emitted++
	}
	result.KeyCount = emitted

	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(result)
}

type deleteRequest struct {
	XMLName xml.Name `xml:"Delete"`
	Objects []struct {
		Key string `xml:"Key"`
	} `xml:"Object"`
}

func (f *Server) serveDeleteObjects(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	var req deleteRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", err.Error())
		return
	}
	f.mu.Lock()
	f.counts.Delete++
	for _, obj := range req.Objects {
		delete(f.objects, obj.Key)
		delete(f.metadata, obj.Key)
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><DeleteResult></DeleteResult>`)
}

func (f *Server) serveCreateMultipart(w http.ResponseWriter, key string) {
	f.mu.Lock()
	f.counts.Multipart++
	id := fmt.Sprintf("upload-%d", len(f.uploads)+1)
	f.uploads[id] = map[int][]byte{}
	f.mu.Unlock()
	writeXML(w, http.StatusOK, initiateMultipartUploadResult{Bucket: "bucket", Key: key, UploadID: id})
}

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string
	Key      string
	UploadID string `xml:"UploadId"`
}

type completeMultipartUploadResult struct {
	XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
	Bucket  string
	Key     string
	ETag    string
}

func (f *Server) serveUploadPart(w http.ResponseWriter, r *http.Request, uploadID, partNumber string) {
	body, err := readObjectBody(r)
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	part, err := strconv.Atoi(partNumber)
	if err != nil {
		writeS3Error(w, http.StatusBadRequest, "InvalidPart", err.Error())
		return
	}
	f.mu.Lock()
	f.counts.Multipart++
	f.counts.BytesUp += int64(len(body))
	parts, ok := f.uploads[uploadID]
	if ok {
		parts[part] = body
	}
	f.mu.Unlock()
	if !ok {
		writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "unknown upload")
		return
	}
	w.Header().Set("ETag", fmt.Sprintf(`"part-%d"`, part))
	w.WriteHeader(http.StatusOK)
}

func (f *Server) serveCompleteMultipart(w http.ResponseWriter, r *http.Request, key, uploadID string) {
	_, _ = io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	f.counts.Multipart++
	parts, ok := f.uploads[uploadID]
	if ok {
		numbers := make([]int, 0, len(parts))
		for number := range parts {
			numbers = append(numbers, number)
		}
		sort.Ints(numbers)
		var assembled bytes.Buffer
		for _, number := range numbers {
			assembled.Write(parts[number])
		}
		f.objects[key] = assembled.Bytes()
		delete(f.uploads, uploadID)
	}
	f.mu.Unlock()
	if !ok {
		writeS3Error(w, http.StatusNotFound, "NoSuchUpload", "unknown upload")
		return
	}
	writeXML(w, http.StatusOK, completeMultipartUploadResult{Bucket: "bucket", Key: key, ETag: `"fake"`})
}

func (f *Server) serveAbortMultipart(w http.ResponseWriter, uploadID string) {
	f.mu.Lock()
	f.counts.Multipart++
	delete(f.uploads, uploadID)
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

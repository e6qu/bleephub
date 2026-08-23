package gitstore

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeS3 is an in-process object store that speaks enough of the S3 REST API
// for the aws-sdk-go-v2 client, and counts every request and every byte that
// crosses it. Counting is the entire point: the cost this package exists to
// reduce is measured in S3 requests, and a request counter that lives inside
// the test binary is exact, whereas a counter derived from a real endpoint's
// access log is sampled and delayed.
//
// The MinIO container the server package uses is a conformance fixture — it
// answers whether bleephub speaks S3 correctly. It cannot answer how many
// requests an operation costs without parsing container logs, and it costs a
// docker pull. Both harnesses are wanted; this one is the measuring instrument.
type fakeS3 struct {
	server *httptest.Server

	mu      sync.Mutex
	objects map[string][]byte
	uploads map[string]map[int][]byte

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

	counts s3Counts
}

// s3Counts is the measurement. Requests are counted per operation because the
// operations have wildly different costs: a LIST returns up to a thousand keys
// for one round trip, a GET returns one object.
type s3Counts struct {
	get         int64
	getRanged   int64
	head        int64
	put         int64
	list        int64
	del         int64
	copy        int64
	multipart   int64
	bytesDown   int64
	bytesUp     int64
	notFoundGet int64
}

func (c s3Counts) total() int64 {
	return c.get + c.getRanged + c.head + c.put + c.list + c.del + c.copy + c.multipart
}

func (c s3Counts) sub(prev s3Counts) s3Counts {
	return s3Counts{
		get:         c.get - prev.get,
		getRanged:   c.getRanged - prev.getRanged,
		head:        c.head - prev.head,
		put:         c.put - prev.put,
		list:        c.list - prev.list,
		del:         c.del - prev.del,
		copy:        c.copy - prev.copy,
		multipart:   c.multipart - prev.multipart,
		bytesDown:   c.bytesDown - prev.bytesDown,
		bytesUp:     c.bytesUp - prev.bytesUp,
		notFoundGet: c.notFoundGet - prev.notFoundGet,
	}
}

func (c s3Counts) String() string {
	return fmt.Sprintf("total=%d get=%d ranged=%d head=%d put=%d list=%d delete=%d copy=%d multipart=%d down=%dB up=%dB 404=%d",
		c.total(), c.get, c.getRanged, c.head, c.put, c.list, c.del, c.copy, c.multipart, c.bytesDown, c.bytesUp, c.notFoundGet)
}

func newFakeS3(tb testing.TB) *fakeS3 {
	tb.Helper()
	f := &fakeS3{
		objects: map[string][]byte{},
		uploads: map[string]map[int][]byte{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	tb.Cleanup(f.server.Close)
	return f
}

// client builds an S3 client wired to this fake. Credentials are static
// literals so nothing consults an credential file, an instance metadata
// endpoint or an operating system credential store.
func (f *fakeS3) client() *s3.Client {
	return s3.New(s3.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(f.server.URL),
		UsePathStyle:     true,
		Credentials:      credentials.NewStaticCredentialsProvider("fake", "fake", ""),
		RetryMaxAttempts: 1,
	})
}

func (f *fakeS3) fs(bucket, prefix string) *S3FS {
	return &S3FS{
		client: f.client(),
		bucket: bucket,
		prefix: prefix,
		active: &s3ActiveFiles{files: map[string]*s3FileState{}},
		locks:  newS3KeyLocks(),
	}
}

func (f *fakeS3) snapshot() s3Counts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts
}

func (f *fakeS3) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts = s3Counts{}
}

func (f *fakeS3) setLatency(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latency = d
}

func (f *fakeS3) setFailOn(fail func(method, key string) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOn = fail
}

func (f *fakeS3) setOnRequest(hook func(method, key string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onRequest = hook
}

func (f *fakeS3) keysWithPrefix(prefix string) []string {
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

func (f *fakeS3) put(key string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = append([]byte(nil), data...)
}

func (f *fakeS3) get(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	return append([]byte(nil), data...), ok
}

func (f *fakeS3) remove(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
}

// keyOf strips the leading "/bucket/" of a path-style request URL.
func keyOf(p string) (bucket, key string) {
	trimmed := strings.TrimPrefix(p, "/")
	bucket, key, _ = strings.Cut(trimmed, "/")
	return bucket, key
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
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
	f.mu.Unlock()
	if hook != nil {
		hook(r.Method, key)
	}
	if fail != nil && fail(r.Method, key) {
		_, _ = io.Copy(io.Discard, r.Body)
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "injected failure")
		return
	}

	switch {
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

func writeS3Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, message)
}

func (f *fakeS3) serveGet(w http.ResponseWriter, r *http.Request, key string) {
	data, ok := f.get(key)
	if !ok {
		f.mu.Lock()
		f.counts.get++
		f.counts.notFoundGet++
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
		f.counts.getRanged++
	} else {
		f.counts.get++
	}
	f.counts.bytesDown += int64(len(body))
	f.mu.Unlock()

	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Last-Modified", time.Unix(0, 0).UTC().Format(http.TimeFormat))
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

func (f *fakeS3) serveHead(w http.ResponseWriter, key string) {
	data, ok := f.get(key)
	f.mu.Lock()
	f.counts.head++
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Last-Modified", time.Unix(0, 0).UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) servePut(w http.ResponseWriter, r *http.Request, key string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	f.mu.Lock()
	f.counts.put++
	f.counts.bytesUp += int64(len(body))
	f.objects[key] = body
	f.mu.Unlock()
	w.Header().Set("ETag", `"fake"`)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) serveDelete(w http.ResponseWriter, key string) {
	f.mu.Lock()
	f.counts.del++
	delete(f.objects, key)
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeS3) serveCopy(w http.ResponseWriter, r *http.Request, key string) {
	source, err := url.PathUnescape(strings.TrimPrefix(r.Header.Get("x-amz-copy-source"), "/"))
	if err != nil {
		writeS3Error(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	_, sourceKey, _ := strings.Cut(source, "/")
	f.mu.Lock()
	f.counts.copy++
	data, ok := f.objects[sourceKey]
	if ok {
		f.objects[key] = append([]byte(nil), data...)
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
func (f *fakeS3) serveList(w http.ResponseWriter, query url.Values) {
	const maxKeys = 1000
	prefix := query.Get("prefix")
	delimiter := query.Get("delimiter")
	after := query.Get("continuation-token")

	f.mu.Lock()
	f.counts.list++
	keys := make([]string, 0, len(f.objects))
	sizes := map[string]int64{}
	for key, data := range f.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
			sizes[key] = int64(len(data))
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
			ETag:         `"fake"`,
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

func (f *fakeS3) serveDeleteObjects(w http.ResponseWriter, r *http.Request) {
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
	f.counts.del++
	for _, obj := range req.Objects {
		delete(f.objects, obj.Key)
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><DeleteResult></DeleteResult>`)
}

func (f *fakeS3) serveCreateMultipart(w http.ResponseWriter, key string) {
	f.mu.Lock()
	f.counts.multipart++
	id := fmt.Sprintf("upload-%d", len(f.uploads)+1)
	f.uploads[id] = map[int][]byte{}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, key, id)
}

func (f *fakeS3) serveUploadPart(w http.ResponseWriter, r *http.Request, uploadID, partNumber string) {
	body, err := io.ReadAll(r.Body)
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
	f.counts.multipart++
	f.counts.bytesUp += int64(len(body))
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

func (f *fakeS3) serveCompleteMultipart(w http.ResponseWriter, r *http.Request, key, uploadID string) {
	_, _ = io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	f.counts.multipart++
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
	w.Header().Set("Content-Type", "application/xml")
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Bucket>bucket</Bucket><Key>%s</Key><ETag>"fake"</ETag></CompleteMultipartUploadResult>`, key)
}

func (f *fakeS3) serveAbortMultipart(w http.ResponseWriter, uploadID string) {
	f.mu.Lock()
	f.counts.multipart++
	delete(f.uploads, uploadID)
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

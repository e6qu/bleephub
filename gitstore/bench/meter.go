package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/e6qu/bleephub/gitstore/s3fake"
)

// Meter is a reverse proxy every driver reaches the object store through. It is
// what makes the comparison fair: a driver written in another language, or one
// that brings its own S3 client, cannot be instrumented from inside, but each
// must cross this proxy, so all are counted by one rule and pay one injected
// latency.
//
// The inbound Host header is forwarded untouched. SigV4 signs the host the
// client addressed — this proxy — and a path-style S3 endpoint verifies against
// the Host it receives, so a real MinIO or AWS endpoint accepts the forwarded
// request as signed.
type Meter struct {
	server  *httptest.Server
	store   string
	latency atomic.Int64

	mu     sync.Mutex
	counts s3fake.Counts
}

// NewMeter fronts the object store at target, a store of the kind given, whose
// requests it classifies by that store's API.
func NewMeter(target *url.URL, store string) *Meter {
	meter := &Meter{store: store}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			request.Out.Host = request.In.Host
		},
		ModifyResponse: func(response *http.Response) error {
			meter.classify(response.Request, response.StatusCode)
			response.Body = &countingBody{body: response.Body, add: meter.addDown}
			return nil
		},
		// Buffering a response before relaying it would bill a streamed ranged
		// read as one slow request and hide its time-to-first-byte.
		FlushInterval: -1,
	}
	meter.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if latency := time.Duration(meter.latency.Load()); latency > 0 {
			time.Sleep(latency)
		}
		if r.Body != nil {
			r.Body = &countingBody{body: r.Body, add: meter.addUp}
		}
		proxy.ServeHTTP(w, r)
	}))
	return meter
}

// URL is the endpoint drivers are pointed at.
func (m *Meter) URL() string { return m.server.URL }

// Close shuts the proxy down.
func (m *Meter) Close() { m.server.Close() }

// SetLatency sets the delay added to every request, standing in for the round
// trip to a remote region. Request count is the cost these designs compete on,
// and it is invisible at loopback speed.
func (m *Meter) SetLatency(latency time.Duration) { m.latency.Store(int64(latency)) }

// Snapshot returns the counts so far.
func (m *Meter) Snapshot() s3fake.Counts {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts
}

func (m *Meter) addDown(n int) {
	m.mu.Lock()
	m.counts.BytesDown += int64(n)
	m.mu.Unlock()
}

func (m *Meter) addUp(n int) {
	m.mu.Lock()
	m.counts.BytesUp += int64(n)
	m.mu.Unlock()
}

// classify files one request under the operation it bills as. The S3 rules
// mirror s3fake's, so a count means the same thing whichever instrument
// produced it, and the other stores' rules file each of their requests under
// the S3 operation it does the work of: a block or a resumable chunk is a part
// of an upload, a batch of deletes is a bulk delete.
func (m *Meter) classify(request *http.Request, status int) {
	var operation *int64
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.store {
	case storeAzure:
		operation = m.classifyAzure(request)
	case storeGCS:
		operation = m.classifyGCS(request)
	default:
		operation = m.classifyS3(request)
	}
	if operation == nil {
		return
	}
	*operation++
	if operation == &m.counts.Get && status == http.StatusNotFound {
		m.counts.NotFoundGet++
	}
}

func (m *Meter) classifyS3(request *http.Request) *int64 {
	query := request.URL.Query()
	_, key := splitBucketKey(request.URL.Path)
	switch {
	case query.Has("uploads"), query.Has("uploadId"):
		return &m.counts.Multipart
	case request.Method == http.MethodPost && query.Has("delete"):
		return &m.counts.Delete
	case request.Method == http.MethodGet && key == "":
		return &m.counts.List
	case request.Method == http.MethodGet && request.Header.Get("Range") != "":
		return &m.counts.GetRanged
	case request.Method == http.MethodGet:
		return &m.counts.Get
	case request.Method == http.MethodHead:
		return &m.counts.Head
	case request.Method == http.MethodPut && request.Header.Get("X-Amz-Copy-Source") != "":
		return &m.counts.Copy
	case request.Method == http.MethodPut:
		return &m.counts.Put
	case request.Method == http.MethodDelete:
		return &m.counts.Delete
	}
	return nil
}

// classifyAzure reads a Blob service request by its method and comp.
// https://learn.microsoft.com/en-us/rest/api/storageservices/blob-service-rest-api
func (m *Meter) classifyAzure(request *http.Request) *int64 {
	comp := request.URL.Query().Get("comp")
	switch {
	case request.Method == http.MethodPut && (comp == "block" || comp == "blocklist"):
		return &m.counts.Multipart
	case request.Method == http.MethodPost && comp == "batch", request.Method == http.MethodDelete:
		return &m.counts.Delete
	case request.Method == http.MethodGet && comp == "list":
		return &m.counts.List
	case request.Method == http.MethodGet && (request.Header.Get("x-ms-range") != "" || request.Header.Get("Range") != ""):
		return &m.counts.GetRanged
	case request.Method == http.MethodGet:
		return &m.counts.Get
	case request.Method == http.MethodHead:
		return &m.counts.Head
	case request.Method == http.MethodPut && request.Header.Get("x-ms-copy-source") != "":
		return &m.counts.Copy
	case request.Method == http.MethodPut:
		return &m.counts.Put
	}
	return nil
}

// classifyGCS reads a Cloud Storage request by its path: the JSON API under
// /storage/v1, /upload/storage/v1 and /batch/storage/v1, and the XML API's
// /bucket/object download.
// https://docs.cloud.google.com/storage/docs/json_api
func (m *Meter) classifyGCS(request *http.Request) *int64 {
	path, query := request.URL.Path, request.URL.Query()
	switch {
	case query.Get("uploadType") == "resumable", query.Has("upload_id"):
		return &m.counts.Multipart
	case strings.HasPrefix(path, "/upload/"):
		return &m.counts.Put
	case strings.HasPrefix(path, "/batch/"), request.Method == http.MethodDelete:
		return &m.counts.Delete
	case request.Method == http.MethodPost && (strings.Contains(path, "/rewriteTo/") || strings.Contains(path, "/copyTo/")):
		return &m.counts.Copy
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/storage/v1/b/") && strings.HasSuffix(path, "/o"):
		return &m.counts.List
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/storage/v1/") && query.Get("alt") != "media":
		// An object's description, which is what a HEAD asks S3 for.
		return &m.counts.Head
	case request.Method == http.MethodGet && request.Header.Get("Range") != "":
		return &m.counts.GetRanged
	case request.Method == http.MethodGet:
		return &m.counts.Get
	}
	return nil
}

// splitBucketKey splits a path-style request path into bucket and key.
func splitBucketKey(path string) (bucket, key string) {
	for len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	for i := range len(path) {
		if path[i] == '/' {
			return path[:i], path[i+1:]
		}
	}
	return path, ""
}

type countingBody struct {
	body io.ReadCloser
	add  func(int)
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.body.Read(p)
	if n > 0 {
		c.add(n)
	}
	return n, err
}

func (c *countingBody) Close() error { return c.body.Close() }

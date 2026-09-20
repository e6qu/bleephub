package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
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
	latency atomic.Int64

	mu     sync.Mutex
	counts s3fake.Counts
}

// NewMeter fronts the object store at target.
func NewMeter(target *url.URL) *Meter {
	meter := &Meter{}
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

// classify files one request under the operation it bills as. The rules mirror
// s3fake's, so a count means the same thing whichever instrument produced it.
func (m *Meter) classify(request *http.Request, status int) {
	query := request.URL.Query()
	_, key := splitBucketKey(request.URL.Path)

	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case query.Has("uploads"), query.Has("uploadId"):
		m.counts.Multipart++
	case request.Method == http.MethodPost && query.Has("delete"):
		m.counts.Delete++
	case request.Method == http.MethodGet && key == "":
		m.counts.List++
	case request.Method == http.MethodGet && request.Header.Get("Range") != "":
		m.counts.GetRanged++
	case request.Method == http.MethodGet:
		m.counts.Get++
		if status == http.StatusNotFound {
			m.counts.NotFoundGet++
		}
	case request.Method == http.MethodHead:
		m.counts.Head++
	case request.Method == http.MethodPut && request.Header.Get("X-Amz-Copy-Source") != "":
		m.counts.Copy++
	case request.Method == http.MethodPut:
		m.counts.Put++
	case request.Method == http.MethodDelete:
		m.counts.Delete++
	}
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

package bleephub

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gzipMinResponseSize is the smallest JSON body worth compressing. Below it the
// gzip header/trailer overhead and the extra CPU buy nothing (the bytes fit in
// one packet either way), so smaller responses are sent identity and keep their
// exact Content-Length.
const gzipMinResponseSize = 1 << 10

// gzipWriterPool recycles gzip writers across requests; constructing one
// allocates its whole deflate state, which is far too expensive per response.
var gzipWriterPool = sync.Pool{
	New: func() interface{} { return gzip.NewWriter(io.Discard) },
}

// compressionMiddleware negotiates gzip for dynamic JSON responses on the API
// surfaces (/api/ and the browser-only /ui-data/). Static UI assets are handled
// separately by the embedded SPA handler (ui_embed.go), which serves
// pre-compressed bytes instead of compressing per request.
//
// The response writer it installs stays undecided until it has seen the status,
// the Content-Type, and gzipMinResponseSize bytes of body, so:
//   - only application/json (and +json media types) is compressed — media,
//     octet-stream, tarballs and anything already carrying a Content-Encoding
//     pass through byte-for-byte;
//   - small responses stay identity with an exact Content-Length;
//   - a handler that calls Flush before the threshold is streaming
//     (long-poll/SSE-shaped) and is committed to identity — compressing would
//     hold its increments hostage in the deflate buffer;
//   - ETags stay computed over identity bytes (the ETag/304 layer in
//     ghResponseWriter sits above this writer), matching RFC 9110's
//     strong-validator semantics for the stored representation.
func compressionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet ||
			(!strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/ui-data/")) {
			next.ServeHTTP(w, r)
			return
		}
		// The representation varies by Accept-Encoding whether or not this
		// particular request negotiated one; caches need to know either way.
		w.Header().Add("Vary", "Accept-Encoding")
		// A Range request wants exact byte offsets into the identity
		// representation; compressing underneath it would corrupt the slice.
		if !acceptsGzip(r) || r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.finish()
		next.ServeHTTP(gw, r)
	})
}

// acceptsGzip reports whether the request's Accept-Encoding admits gzip,
// honouring an explicit q=0 refusal.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		coding, params, hasQ := strings.Cut(strings.TrimSpace(part), ";")
		coding = strings.TrimSpace(coding)
		if !strings.EqualFold(coding, "gzip") && coding != "*" {
			continue
		}
		if hasQ {
			q := strings.TrimSpace(params)
			if v, ok := strings.CutPrefix(q, "q="); ok {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f <= 0 {
					return false
				}
			}
		}
		return true
	}
	return false
}

// gzipEligibleResponse reports whether a response with these headers and this
// status may be compressed: a body-bearing status, no encoding already applied,
// and a JSON media type. Everything else — images, octet-stream downloads,
// tarballs, event streams — passes through identity.
func gzipEligibleResponse(h http.Header, code int) bool {
	switch code {
	case http.StatusNoContent, http.StatusResetContent, http.StatusNotModified:
		return false
	}
	if h.Get("Content-Encoding") != "" {
		return false
	}
	ct := h.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.ToLower(strings.TrimSpace(ct))
	return ct == "application/json" || strings.HasSuffix(ct, "+json")
}

const (
	gzipUndecided = iota // buffering until eligibility + size are known
	gzipIdentity         // committed to pass-through
	gzipActive           // committed to gzip; gz is live
)

// gzipResponseWriter defers the underlying WriteHeader until it knows whether
// the response is worth compressing, then either replays the buffered identity
// bytes or streams them through a pooled gzip writer.
type gzipResponseWriter struct {
	http.ResponseWriter
	state  int
	status int    // recorded status; 0 until WriteHeader or first Write
	buf    []byte // identity bytes held while undecided
	gz     *gzip.Writer
}

// Unwrap lets net/http's ResponseController reach optional interfaces on the
// real writer, mirroring the other wrappers in the pipeline.
func (gw *gzipResponseWriter) Unwrap() http.ResponseWriter { return gw.ResponseWriter }

func (gw *gzipResponseWriter) WriteHeader(code int) {
	if gw.state != gzipUndecided {
		gw.ResponseWriter.WriteHeader(code)
		return
	}
	if code < 200 {
		// Informational responses pass straight through; the final status is
		// still to come.
		gw.ResponseWriter.WriteHeader(code)
		return
	}
	if gw.status != 0 {
		// Duplicate WriteHeader while undecided: ignore it exactly as net/http
		// would (it logs and drops superfluous calls).
		return
	}
	gw.status = code
	if !gzipEligibleResponse(gw.Header(), code) {
		gw.commitIdentity()
	}
}

func (gw *gzipResponseWriter) Write(b []byte) (int, error) {
	switch gw.state {
	case gzipActive:
		return gw.gz.Write(b)
	case gzipIdentity:
		return gw.ResponseWriter.Write(b)
	}
	if gw.status == 0 {
		// Implicit 200: net/http semantics for a Write without WriteHeader.
		gw.status = http.StatusOK
		if !gzipEligibleResponse(gw.Header(), gw.status) {
			gw.commitIdentity()
			return gw.ResponseWriter.Write(b)
		}
	}
	gw.buf = append(gw.buf, b...)
	if len(gw.buf) >= gzipMinResponseSize {
		if err := gw.startGzip(); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

// Flush makes the writer safe under flusher-dependent handlers: a flush while
// undecided means the handler is streaming, so it is committed to identity and
// every increment reaches the client exactly when the handler pushes it.
func (gw *gzipResponseWriter) Flush() {
	switch gw.state {
	case gzipUndecided:
		gw.commitIdentity()
	case gzipActive:
		_ = gw.gz.Flush()
	}
	_ = http.NewResponseController(gw.ResponseWriter).Flush()
}

// commitIdentity writes the deferred header and replays any buffered bytes
// uncompressed.
func (gw *gzipResponseWriter) commitIdentity() {
	gw.state = gzipIdentity
	if gw.status == 0 {
		gw.status = http.StatusOK
	}
	gw.ResponseWriter.WriteHeader(gw.status)
	if len(gw.buf) > 0 {
		// This middleware is a transparent passthrough: it never originates
		// response bytes, only replays what the wrapped handler wrote.
		// Request-derived output is sanitized at the handlers (constant
		// http.Error messages, store-owned sub-request paths, html-escaped
		// echoes — see gh_meta_extras.go, gh_profile_readme.go,
		// gh_pulls_uidata.go). gosec's G705 attributes handler flows to this
		// shared sink nondeterministically (~50% of runs on an unchanged
		// tree, concurrency-independent), so the verdict names no fixable
		// source here.
		_, _ = gw.ResponseWriter.Write(gw.buf) // #nosec G705 -- passthrough replay of handler bytes; sources sanitized per-handler
		gw.buf = nil
	}
}

// startGzip commits to compression: any handler-declared identity length is no
// longer true, the encoding is declared, and the buffered bytes are fed through
// a pooled writer.
func (gw *gzipResponseWriter) startGzip() error {
	gw.state = gzipActive
	h := gw.Header()
	h.Del("Content-Length")
	h.Set("Content-Encoding", "gzip")
	gw.ResponseWriter.WriteHeader(gw.status)
	gw.gz = gzipWriterPool.Get().(*gzip.Writer)
	gw.gz.Reset(gw.ResponseWriter)
	_, err := gw.gz.Write(gw.buf)
	gw.buf = nil
	return err
}

// finish completes the response after the handler returns: an undecided (small
// or bodyless) response is sent identity, an active gzip stream gets its
// trailer, and the pooled writer goes back.
func (gw *gzipResponseWriter) finish() {
	switch gw.state {
	case gzipUndecided:
		gw.commitIdentity()
	case gzipActive:
		_ = gw.gz.Close()
		gzipWriterPool.Put(gw.gz)
		gw.gz = nil
	}
}

package bleephub

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gzipMinResponseSize is the smallest JSON body worth compressing; below it the
// header/trailer overhead buys nothing, so the response stays identity with an
// exact Content-Length.
const gzipMinResponseSize = 1 << 10

// gzipWriterPool recycles gzip writers; each allocates its whole deflate state.
var gzipWriterPool = sync.Pool{
	New: func() interface{} { return gzip.NewWriter(io.Discard) },
}

// compressionMiddleware negotiates gzip for dynamic JSON on /api/ and /ui-data/.
// Static assets are compressed ahead of time by the embedded SPA handler
// (ui_embed.go). The writer it installs stays undecided until it has seen the
// status, Content-Type, and gzipMinResponseSize bytes, so that:
//   - only application/json (and +json) is compressed; everything else passes
//     through byte-for-byte;
//   - small responses stay identity with an exact Content-Length;
//   - a Flush before the threshold marks a streaming handler and commits to
//     identity, so increments are not held in the deflate buffer;
//   - ETags stay computed over identity bytes (the ETag/304 layer sits above
//     this writer), per RFC 9110 strong-validator semantics.
func compressionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet ||
			(!strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/ui-data/")) {
			next.ServeHTTP(w, r)
			return
		}
		// The representation varies by Accept-Encoding either way.
		w.Header().Add("Vary", "Accept-Encoding")
		// A Range request needs exact identity byte offsets; compressing would
		// corrupt the slice.
		if !acceptsGzip(r) || r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.finish()
		next.ServeHTTP(gw, r)
	})
}

// acceptsGzip reports whether Accept-Encoding admits gzip, honouring q=0.
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

// gzipEligibleResponse reports whether a response may be compressed: a
// body-bearing status, no encoding already applied, and a JSON media type.
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
	gzipActive           // committed to gzip
)

// gzipResponseWriter defers WriteHeader until it knows whether to compress, then
// replays the buffered bytes identity or through a pooled gzip writer.
type gzipResponseWriter struct {
	http.ResponseWriter
	state  int
	status int    // recorded status; 0 until WriteHeader or first Write
	buf    []byte // identity bytes held while undecided
	gz     *gzip.Writer
}

// Unwrap lets net/http's ResponseController reach the real writer's optional
// interfaces.
func (gw *gzipResponseWriter) Unwrap() http.ResponseWriter { return gw.ResponseWriter }

func (gw *gzipResponseWriter) WriteHeader(code int) {
	if gw.state != gzipUndecided {
		gw.ResponseWriter.WriteHeader(code)
		return
	}
	if code < 200 {
		// Informational responses pass through; the final status is still to come.
		gw.ResponseWriter.WriteHeader(code)
		return
	}
	if gw.status != 0 {
		// Duplicate WriteHeader while undecided: drop it as net/http would.
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
		// Implicit 200: a Write without WriteHeader.
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

// Flush commits an undecided writer to identity: a flush marks a streaming
// handler, whose increments must reach the client as it pushes them.
func (gw *gzipResponseWriter) Flush() {
	switch gw.state {
	case gzipUndecided:
		gw.commitIdentity()
	case gzipActive:
		_ = gw.gz.Flush()
	}
	_ = http.NewResponseController(gw.ResponseWriter).Flush()
}

// commitIdentity writes the deferred header and replays buffered bytes
// uncompressed.
func (gw *gzipResponseWriter) commitIdentity() {
	gw.state = gzipIdentity
	if gw.status == 0 {
		gw.status = http.StatusOK
	}
	gw.ResponseWriter.WriteHeader(gw.status)
	if len(gw.buf) > 0 {
		// Transparent passthrough: this middleware only replays handler bytes,
		// which are sanitized at their handlers. gosec's G705 attributes those
		// flows to this shared sink nondeterministically, naming no fixable
		// source here.
		_, _ = gw.ResponseWriter.Write(gw.buf) // #nosec G705 -- passthrough replay of handler bytes; sources sanitized per-handler
		gw.buf = nil
	}
}

// startGzip commits to compression: drop any handler-declared Content-Length,
// declare the encoding, and feed the buffered bytes through a pooled writer.
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

// finish completes the response after the handler returns: an undecided response
// is sent identity, an active stream gets its trailer and returns the writer.
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

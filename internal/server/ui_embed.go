//go:build !noui

package bleephub

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
)

//go:embed all:dist
var uiAssets embed.FS

// registerUI mounts the embedded single-page app under /ui/.
//
// This is a deliberate, documented exception to the "every surface goes
// through s.route() so routePatterns/RegisteredRoutes() enumerates it"
// invariant. /ui/ is a static asset tree served by an SPA fallback handler,
// not an enumerable API operation: it has no method, no authz resource, and
// no OpenAPI/definition shape, so recording it in routePatterns would break
// the authz-matrix and api-definition tests that assume every registered
// pattern is a "METHOD /api/v3/..." operation. It is therefore registered
// directly on the mux and intentionally excluded from the route registry.
// The only other such exception is the no-embed build's empty stub below.
func (s *Server) registerUI() {
	sub, err := fs.Sub(uiAssets, "dist")
	if err != nil {
		s.logger.Warn().Err(err).Msg("failed to load embedded UI assets")
		return
	}
	s.mux.Handle("/ui/", spaHandler(sub, "/ui/"))
	// Redirect the bare root to the SPA. Registered here, alongside the /ui/
	// handler, so it exists only when /ui/ is actually served (CORE-012).
	s.route("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusTemporaryRedirect)
	})
	s.logger.Info().Msg("UI registered at /ui/")
}

// uiImmutableCacheControl matches the emoji asset route (gh_emoji_assets.go):
// everything under /ui/assets/ carries a content hash in its filename (Vite's
// name-HASH.ext), so a given URL's bytes can never change and browsers may
// cache them forever.
const uiImmutableCacheControl = "public, max-age=31536000, immutable"

// uiGzipMinSize is the smallest embedded file worth pre-compressing; below it
// the gzip framing overhead eats the gain.
const uiGzipMinSize = 512

// uiCompressibleTypes maps the text-shaped asset extensions the SPA build
// emits to their Content-Type. Only these are pre-compressed; images and other
// binary formats are already packed and go out identity via the file server.
var uiCompressibleTypes = map[string]string{
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".html": "text/html; charset=utf-8",
	".svg":  "image/svg+xml",
	".json": "application/json; charset=utf-8",
	".map":  "application/json; charset=utf-8",
	".txt":  "text/plain; charset=utf-8",
	".wasm": "application/wasm",
}

// uiDelivery caches gzipped copies of embedded assets, compressed once on
// first request rather than per request (the bytes are immutable for the life
// of the binary). A nil cached value records "not worth serving compressed"
// (too small, or the file did not shrink) so the decision is also made once.
type uiDelivery struct {
	fsys fs.FS
	mu   sync.Mutex
	gz   map[string][]byte
}

func (d *uiDelivery) gzipFor(reqPath string) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	if gz, ok := d.gz[reqPath]; ok {
		return gz
	}
	var gz []byte
	if data, err := fs.ReadFile(d.fsys, reqPath); err == nil && len(data) >= uiGzipMinSize {
		if compressed := gzipBytes(data); len(compressed) < len(data) {
			gz = compressed
		}
	}
	d.gz[reqPath] = gz
	return gz
}

// gzipBytes compresses data at the best ratio; startup-or-once cost, so CPU
// per byte does not matter the way it does on the per-request path.
func gzipBytes(data []byte) []byte {
	var buf bytes.Buffer
	gw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = gw.Write(data)
	_ = gw.Close()
	return buf.Bytes()
}

func spaHandler(fsys fs.FS, pathPrefix string) http.Handler {
	fileServer := http.StripPrefix(pathPrefix, http.FileServer(http.FS(fsys)))
	delivery := &uiDelivery{fsys: fsys, gz: map[string][]byte{}}

	// The shell is read once at startup: index.html is served under every SPA
	// route name, so it can never be cached by URL (Cache-Control: no-cache),
	// but its strong ETag lets a browser revalidate with a cheap 304 instead
	// of refetching the document on every navigation.
	shell, err := fs.ReadFile(fsys, "index.html")
	var shellETag string
	if err != nil {
		shell = nil
	} else {
		shellETag = fmt.Sprintf(`"%x"`, sha256.Sum256(shell))
	}

	serveShell := func(w http.ResponseWriter, r *http.Request) {
		if shell == nil {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Cache-Control", "no-cache")
		h.Set("ETag", shellETag)
		h.Add("Vary", "Accept-Encoding")
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, shellETag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		body := shell
		if acceptsGzip(r) {
			if gz := delivery.gzipFor("index.html"); gz != nil {
				h.Set("Content-Encoding", "gzip")
				body = gz
			}
		}
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqPath := r.URL.Path
		if len(reqPath) > len(pathPrefix) {
			reqPath = reqPath[len(pathPrefix):]
		} else {
			reqPath = "."
		}
		// The document itself is the shell wherever it is asked for by name.
		if reqPath == "." || reqPath == "index.html" {
			serveShell(w, r)
			return
		}

		f, err := fsys.Open(reqPath)
		if err == nil {
			stat, statErr := f.Stat()
			_ = f.Close()
			if statErr == nil && !stat.IsDir() {
				h := w.Header()
				if strings.HasPrefix(reqPath, "assets/") {
					// Content-hashed by the build: a changed file is a new URL.
					h.Set("Cache-Control", uiImmutableCacheControl)
				}
				contentType, compressible := uiCompressibleTypes[path.Ext(reqPath)]
				if compressible {
					h.Add("Vary", "Accept-Encoding")
					if r.Method == http.MethodGet && acceptsGzip(r) {
						if gz := delivery.gzipFor(reqPath); gz != nil {
							h.Set("Content-Type", contentType)
							h.Set("Content-Encoding", "gzip")
							h.Set("Content-Length", strconv.Itoa(len(gz)))
							_, _ = w.Write(gz)
							return
						}
					}
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		// Unknown path: client-side route, serve the shell.
		serveShell(w, r)
	})
}

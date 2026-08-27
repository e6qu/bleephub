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
)

//go:embed all:dist
var uiAssets embed.FS

// registerUI mounts the embedded single-page app under /ui/.
//
// Deliberate exception to the "every surface goes through s.route()" invariant:
// /ui/ is a static asset tree with no method, authz resource, or OpenAPI shape,
// so recording it in routePatterns would break the authz-matrix and api-definition
// tests. It is registered directly on the mux and excluded from the route registry.
func (s *Server) registerUI() {
	sub, err := fs.Sub(uiAssets, "dist")
	if err != nil {
		s.logger.Warn().Err(err).Msg("failed to load embedded UI assets")
		return
	}
	s.mux.Handle("/ui/", spaHandler(sub, "/ui/"))
	// Redirect the bare root to the SPA, registered here so it exists only when
	// /ui/ is actually served (CORE-012).
	s.route("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusTemporaryRedirect)
	})
	s.logger.Info().Msg("UI registered at /ui/")
}

// uiImmutableCacheControl: everything under /ui/assets/ carries a content hash in
// its filename (Vite name-HASH.ext), so a URL's bytes never change and browsers
// may cache them forever.
const uiImmutableCacheControl = "public, max-age=31536000, immutable"

// uiGzipMinSize is the smallest embedded file worth pre-compressing; below it the
// gzip framing overhead eats the gain.
const uiGzipMinSize = 512

// uiCompressibleTypes maps the text-shaped asset extensions to Content-Type; only
// these are pre-compressed (binary formats are already packed).
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

// uiDelivery holds gzipped copies of the embedded assets, compressed at
// construction (the bytes are immutable). Serving is a pure map read: no
// request-derived string reaches a file read, so response bytes provably come
// from the embed, never the request.
type uiDelivery struct {
	gz map[string][]byte
}

func newUIDelivery(fsys fs.FS) *uiDelivery {
	d := &uiDelivery{gz: map[string][]byte{}}
	_ = fs.WalkDir(fsys, ".", func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if _, compressible := uiCompressibleTypes[path.Ext(p)]; !compressible {
			return nil
		}
		data, readErr := fs.ReadFile(fsys, p)
		if readErr != nil || len(data) < uiGzipMinSize {
			return nil
		}
		if compressed := gzipBytes(data); len(compressed) < len(data) {
			d.gz[p] = compressed
		}
		return nil
	})
	return d
}

func (d *uiDelivery) gzipFor(reqPath string) []byte {
	return d.gz[reqPath]
}

// gzipBytes compresses data at the best ratio; a startup-once cost, not per-request.
func gzipBytes(data []byte) []byte {
	var buf bytes.Buffer
	gw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = gw.Write(data)
	_ = gw.Close()
	return buf.Bytes()
}

func spaHandler(fsys fs.FS, pathPrefix string) http.Handler {
	fileServer := http.StripPrefix(pathPrefix, http.FileServer(http.FS(fsys)))
	delivery := newUIDelivery(fsys)

	// index.html is served under every SPA route name, so it can never be cached
	// by URL (no-cache); its strong ETag still lets a browser revalidate with a 304.
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
		// Serve the shell wherever the document is asked for by name.
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

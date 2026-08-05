//go:build !noui

package bleephub

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
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

func spaHandler(fsys fs.FS, pathPrefix string) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	stripped := http.StripPrefix(pathPrefix, fileServer)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqPath := r.URL.Path
		if len(reqPath) > len(pathPrefix) {
			reqPath = reqPath[len(pathPrefix):]
		} else {
			reqPath = "."
		}

		f, err := fsys.Open(reqPath)
		if err == nil {
			stat, statErr := f.Stat()
			_ = f.Close()
			if statErr == nil && !stat.IsDir() {
				stripped.ServeHTTP(w, r)
				return
			}
		}

		indexFile, err := fsys.Open("index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer func() { _ = indexFile.Close() }()

		stat, err := indexFile.Stat()
		if err != nil {
			http.NotFound(w, r)
			return
		}

		type readSeeker interface {
			Read(p []byte) (n int, err error)
			Seek(offset int64, whence int) (int64, error)
		}
		rs, ok := indexFile.(readSeeker)
		if !ok {
			// embed.FS files always implement io.Seeker, but never panic if a
			// future FS backing doesn't; fall back to a plain copy.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if _, err := io.Copy(w, indexFile); err != nil {
				http.Error(w, "failed to serve index", http.StatusInternalServerError)
			}
			return
		}
		http.ServeContent(w, r, "index.html", stat.ModTime(), rs)
	})
}

//go:build noui

package bleephub

import "net/http"

// registerUI is the no-embed stub: no SPA to serve. It still claims the bare
// root so a request there gets an honest 404 instead of a 307 into an unserved
// /ui/ (CORE-012), keeping the route set identical to the UI-embedded build.
func (s *Server) registerUI() {
	s.route("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
}

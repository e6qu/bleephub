//go:build noui

package bleephub

import "net/http"

// registerUI is the no-embed stub: there is no SPA to serve. It still claims the
// bare root so a request there gets an honest 404 rather than a 307 into an
// unserved /ui/ (CORE-012). Registering it (instead of letting / fall through to
// the catch-all) also keeps the root out of the "unhandled request" warning path
// and keeps the registered route set identical to the UI-embedded build.
func (s *Server) registerUI() {
	s.route("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
}

package bleephub

import (
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/e6qu/bleephub/internal/emojiart"
)

// The instance's public origin ("scheme://host") for hypermedia rendered
// outside a request — webhook payloads must carry absolute *_url values a
// receiver on another host can resolve. BLEEPHUB_EXTERNAL_URL is authoritative;
// otherwise the origin is learned from served requests and remembered.

// rememberOrigin records the origin a request arrived on.
func (s *Server) rememberOrigin(r *http.Request) {
	if s.externalURL != "" {
		return
	}
	if r.Host == "" {
		return
	}
	// Derived exactly as (*Server).baseURL does, so webhook and REST renderings agree.
	origin := s.baseURL(r)
	s.observedOrigin.Store(&origin)
}

// publicOrigin is BLEEPHUB_EXTERNAL_URL when configured, otherwise the most
// recently served request's origin.
func (s *Server) publicOrigin() string {
	if s.externalURL != "" {
		return s.externalURL
	}
	if origin := s.observedOrigin.Load(); origin != nil {
		return *origin
	}
	return ""
}

// originMiddleware feeds rememberOrigin from every served request.
func (s *Server) originMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.rememberOrigin(r)
		next.ServeHTTP(w, r)
	})
}

// observedOriginBox stores the learned origin on the Server (see server.go).
type observedOriginBox = atomic.Pointer[string]

// registerAvatarRoutes registers the instance-hosted avatar images each
// account's avatar_url points at (GHES serves these from the instance, off /api/v3).
func (s *Server) registerAvatarRoutes() {
	s.route("GET /avatars/u/{account_id}", s.handleAccountAvatar)
}

// handleAccountAvatar serves one account's avatar, derived from its login and
// stable for the account's life; the advertised ?v=N cache-busts a rename, so
// the response is immutable.
func (s *Server) handleAccountAvatar(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("account_id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	login := s.accountLoginByID(id)
	if login == "" {
		http.NotFound(w, r)
		return
	}
	png, err := emojiart.BadgePNG(login)
	if err != nil {
		s.logger.Error().Err(err).Msg("avatar rendering failed")
		http.Error(w, "avatar unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(png)
}

// accountLoginByID resolves an account id to its login, for both users
// (including bots) and organizations.
func (s *Server) accountLoginByID(id int) string {
	if u := s.store.GetUserByID(id); u != nil {
		return u.Login
	}
	for _, org := range s.store.ListOrgsAll(0) {
		if org.ID == id {
			return org.Login
		}
	}
	return ""
}

package bleephub

import (
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/e6qu/bleephub/internal/emojiart"
)

// The instance's public origin — the "scheme://host" every hypermedia member
// of every rendered object is built from.
//
// Inside a request handler the origin comes from the request (see
// (*Server).baseURL). Payloads rendered outside a request have no request to
// derive it from: a webhook delivery is built by the emitting handler but read
// by a receiver on another host, and `simple-user` declares url, html_url,
// avatar_url and the eight *_url members required with format: uri, so a
// relative value is not a valid rendering anywhere — a receiver has nothing to
// resolve it against.
//
// BLEEPHUB_EXTERNAL_URL is authoritative when configured (the GitHub Enterprise
// Server "external URL" knob). When it is not, the origin is learned from the
// requests the instance actually serves and remembered, which is what lets a
// server bound to an ephemeral port render the same absolute hypermedia in a
// webhook body as it does in the REST response for the same object.

// rememberOrigin records the origin a request arrived on, so payloads rendered
// outside any request can name the instance.
func (s *Server) rememberOrigin(r *http.Request) {
	if s.externalURL != "" {
		return
	}
	if r.Host == "" {
		return
	}
	// Derived exactly as (*Server).baseURL derives it, so the origin a webhook
	// body names is the origin the REST response for the same object named.
	origin := s.baseURL(r)
	s.observedOrigin.Store(&origin)
}

// publicOrigin is the instance's origin for a payload rendered outside a
// request. It is BLEEPHUB_EXTERNAL_URL when configured, otherwise the origin of
// the most recent request the instance served.
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

// observedOriginBox is the storage type for the learned origin; it lives on the
// Server (see server.go) and is written by rememberOrigin.
type observedOriginBox = atomic.Pointer[string]

// registerAvatarRoutes registers the instance-hosted avatar images every
// account shape's avatar_url points at. GitHub Enterprise Server serves them
// from the instance itself at /avatars/u/{account_id}; on github.com the same
// path lives on avatars.githubusercontent.com. It is a top-level asset path,
// not part of /api/v3, exactly like the emoji images.
func (s *Server) registerAvatarRoutes() {
	s.route("GET /avatars/u/{account_id}", s.handleAccountAvatar)
}

// handleAccountAvatar serves one account's avatar. The image is derived from
// the account's login, so it is stable for the life of the account and
// identical on every machine; ?v=N in the advertised URL cache-busts a rename,
// which is why the response is immutable.
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

// accountLoginByID resolves an account id to its login, across both account
// kinds an avatar_url can name: users (including bots) and organizations.
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

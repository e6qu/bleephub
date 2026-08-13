package bleephub

import (
	"net/http"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// Pinned repositories power the profile Overview grid. GitHub exposes pins only
// over GraphQL (no REST API), so — like the wiki — these live under the
// browser-only /ui-data namespace rather than an invented /api/v3 path.
// `s.route` auto-wraps /ui-data with authenticateUIData.
func (s *Server) registerGHPinnedRoutes() {
	s.route("GET /ui-data/users/{username}/pinned", s.handleListPinnedRepos)
	s.route("PUT /ui-data/users/{username}/pinned", s.handleSetPinnedRepos)
}

// pinnedReposJSON resolves the user's pinned full names to repo JSON the viewer
// can read, preserving pin order.
func (s *Server) pinnedReposJSON(r *http.Request, u *store.User) []map[string]interface{} {
	names := s.store.ListPinnedRepos(u.ID)
	viewer := ghUserFromContext(r.Context())
	out := make([]map[string]interface{}, 0, len(names))
	for _, fullName := range names {
		parts := strings.SplitN(fullName, "/", 2)
		if len(parts) != 2 {
			continue
		}
		repo := s.store.GetRepo(parts[0], parts[1])
		if repo == nil || !s.viewerCanReadRepo(r.Context(), repo) {
			continue
		}
		out = append(out, fullRepoJSONForViewer(repo, s.store, s.baseURL(r), viewer))
	}
	return out
}

func (s *Server) handleListPinnedRepos(w http.ResponseWriter, r *http.Request) {
	u := s.store.LookupUserByLogin(r.PathValue("username"))
	if u == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.pinnedReposJSON(r, u))
}

func (s *Server) handleSetPinnedRepos(w http.ResponseWriter, r *http.Request) {
	u := s.store.LookupUserByLogin(r.PathValue("username"))
	if u == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// Only the account owner (or a site admin) may edit their pins.
	viewer := ghUserFromContext(r.Context())
	if viewer == nil || (viewer.ID != u.ID && !viewer.SiteAdmin) {
		writeGHError(w, http.StatusForbidden, "You can only change your own pinned repositories.")
		return
	}

	var req struct {
		Repos []string `json:"repos"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if len(req.Repos) > store.MaxPinnedRepos {
		store.WriteGHValidationError(w, "PinnedRepos", "repos", "too_many")
		return
	}
	if _, ok := s.store.SetPinnedRepos(u.ID, req.Repos); !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.pinnedReposJSON(r, u))
}

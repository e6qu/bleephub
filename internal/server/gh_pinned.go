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
	// Organization profiles pin repositories the same way (GraphQL/web-only on
	// real GitHub): any viewer may read the list, only an org owner may edit it.
	s.route("GET /ui-data/orgs/{org}/pinned", s.handleListOrgPinnedRepos)
	s.route("PUT /ui-data/orgs/{org}/pinned", s.handleSetOrgPinnedRepos)
}

// pinnedReposJSON resolves the user's pinned full names to repo JSON the viewer
// can read, preserving pin order.
func (s *Server) pinnedReposJSON(r *http.Request, u *store.User) []map[string]interface{} {
	return s.pinnedRepoListJSON(r, s.store.ListPinnedRepos(u.ID))
}

// pinnedRepoListJSON renders an ordered pinned full-name list as the repo JSON
// rows the viewer can read, preserving pin order.
func (s *Server) pinnedRepoListJSON(r *http.Request, names []string) []map[string]interface{} {
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

func (s *Server) handleListOrgPinnedRepos(w http.ResponseWriter, r *http.Request) {
	names, ok := s.store.ListOrgPinnedRepos(r.PathValue("org"))
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.pinnedRepoListJSON(r, names))
}

func (s *Server) handleSetOrgPinnedRepos(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// Only an organization owner (or a site admin) may edit the org's pins.
	viewer := ghUserFromContext(r.Context())
	if viewer == nil || !s.viewerCanAdminOrg(r.Context(), org.Login) {
		writeGHError(w, http.StatusForbidden, "Must be an organization owner.")
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
	names, ok := s.store.SetOrgPinnedRepos(org.Login, req.Repos)
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.pinnedRepoListJSON(r, names))
}

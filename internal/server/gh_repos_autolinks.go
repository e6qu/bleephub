package bleephub

import (
	"net/http"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHRepoAutolinkRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/autolinks", s.handleListAutolinks)
	s.route("POST /api/v3/repos/{owner}/{repo}/autolinks", s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleCreateAutolink))
	s.route("GET /api/v3/repos/{owner}/{repo}/autolinks/{autolink_id}", s.handleGetAutolink)
	s.route("DELETE /api/v3/repos/{owner}/{repo}/autolinks/{autolink_id}", s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleDeleteAutolink))
}

func (s *Server) handleListAutolinks(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// Autolink info is available only to repository administrators on GitHub
	// (on any repo, public or private); hide it from non-admins with a 404.
	if !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	autolinks := s.store.ListRepoAutolinks(repo.FullName)
	out := make([]map[string]interface{}, 0, len(autolinks))
	for _, a := range autolinks {
		out = append(out, autolinkJSON(a))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateAutolink(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have admin rights to Repository.")
		return
	}

	var req struct {
		KeyPrefix      string `json:"key_prefix"`
		URLTemplate    string `json:"url_template"`
		IsAlphanumeric *bool  `json:"is_alphanumeric"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.KeyPrefix == "" {
		store.WriteGHValidationError(w, "Autolink", "key_prefix", "missing_field")
		return
	}
	if req.URLTemplate == "" {
		store.WriteGHValidationError(w, "Autolink", "url_template", "missing_field")
		return
	}
	isAlpha := true
	if req.IsAlphanumeric != nil {
		isAlpha = *req.IsAlphanumeric
	}

	autolink := s.store.CreateRepoAutolink(repo.FullName, req.KeyPrefix, req.URLTemplate, isAlpha)
	writeJSON(w, http.StatusCreated, autolinkJSON(autolink))
}

func (s *Server) handleGetAutolink(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// Autolink info is available only to repository administrators on GitHub
	// (on any repo, public or private); hide it from non-admins with a 404.
	if !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	id, err := strconv.Atoi(r.PathValue("autolink_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	autolink := s.store.GetRepoAutolink(repo.FullName, id)
	if autolink == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, autolinkJSON(autolink))
}

func (s *Server) handleDeleteAutolink(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have admin rights to Repository.")
		return
	}

	id, err := strconv.Atoi(r.PathValue("autolink_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.store.DeleteRepoAutolink(repo.FullName, id) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func autolinkJSON(a *store.RepoAutolink) map[string]interface{} {
	return map[string]interface{}{
		"id":              a.ID,
		"node_id":         a.NodeID,
		"key_prefix":      a.KeyPrefix,
		"url_template":    a.URLTemplate,
		"is_alphanumeric": a.IsAlphanumeric,
		"created_at":      a.CreatedAt.Format(time.RFC3339),
	}
}

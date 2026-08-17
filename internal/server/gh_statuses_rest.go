package bleephub

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Commit Statuses API.
// Real GH endpoints:
//   GET  /repos/{o}/{r}/commits/{ref}/status        combined status
//   GET  /repos/{o}/{r}/commits/{ref}/statuses      list statuses
//   POST /repos/{o}/{r}/statuses/{sha}              create status
//
// Statuses are repo+ref scoped; the combined status endpoint derives the
// worst state across the latest status per context.

func (s *Server) registerGHStatusesRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/commits/{ref}/status", s.handleGetCombinedStatus)
	s.route("GET /api/v3/repos/{owner}/{repo}/commits/{ref}/statuses", s.handleListCommitStatuses)
	s.route("POST /api/v3/repos/{owner}/{repo}/statuses/{sha}", s.requirePerm(store.ScopeContents, store.PermWrite, s.handleCreateCommitStatus))
}

func (s *Server) handleGetCombinedStatus(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	ref := s.canonicalCommitStatusRef(repo, r.PathValue("ref"))
	state, total, statuses := s.store.CommitStatuses.Combined(repo.FullName, ref)
	page := paginateAndLink(w, r, statuses)
	out := make([]map[string]interface{}, 0, len(page))
	for _, st := range page {
		out = append(out, commitStatusToJSON(st, s.store, s.baseURL(r), repo.FullName))
	}
	base := s.baseURL(r)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"state":       state,
		"sha":         ref,
		"total_count": total,
		"statuses":    out,
		"repository":  store.RepoToJSON(repo, s.store, base),
		// url and commit_url are required members of combined-commit-status;
		// GitHub points them at the combined-status resource and the commit.
		"url":        fmt.Sprintf("%s/api/v3/repos/%s/commits/%s/status", base, repo.FullName, ref),
		"commit_url": fmt.Sprintf("%s/api/v3/repos/%s/commits/%s", base, repo.FullName, ref),
	})
}

func (s *Server) handleListCommitStatuses(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	ref := s.canonicalCommitStatusRef(repo, r.PathValue("ref"))
	statuses := s.store.CommitStatuses.List(repo.FullName, ref)
	page := paginateAndLink(w, r, statuses)
	out := make([]map[string]interface{}, 0, len(page))
	for _, st := range page {
		out = append(out, commitStatusToJSON(st, s.store, s.baseURL(r), repo.FullName))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateCommitStatus(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		State       string `json:"state"`
		TargetURL   string `json:"target_url"`
		Description string `json:"description"`
		Context     string `json:"context"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.State == "" {
		store.WriteGHValidationError(w, "Status", "state", "missing_field")
		return
	}
	switch strings.ToLower(req.State) {
	case "error", "failure", "pending", "success":
	default:
		store.WriteGHValidationError(w, "Status", "state", "invalid")
		return
	}
	sha := s.canonicalCommitStatusRef(repo, r.PathValue("sha"))
	st := s.store.CommitStatuses.Create(repo.FullName, sha, user.ID, req.State, req.TargetURL, req.Description, req.Context)
	s.emitWebhookEvent(repo.FullName, "status", "", map[string]interface{}{
		"id":          st.ID,
		"sha":         sha,
		"state":       st.State,
		"context":     st.Context,
		"description": st.Description,
		"target_url":  st.TargetURL,
		"repository":  store.RepoToJSON(repo, s.store, s.baseURL(r)),
		"sender":      store.UserToJSON(user),
	})
	statusJSON := commitStatusToJSON(st, s.store, s.baseURL(r), repo.FullName)
	writeJSONCreated(w, jsonStringField(statusJSON, "url"), statusJSON)
}

func (s *Server) canonicalCommitStatusRef(repo *store.Repo, ref string) string {
	if repo == nil {
		return ref
	}
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return ref
	}
	if stor := s.store.GetGitStorage(owner, name); stor != nil {
		if hash, err := store.ResolveGitRef(stor, ref); err == nil {
			return hash.String()
		}
	}
	return ref
}

func commitStatusToJSON(st *store.CommitStatus, stStore *store.Store, baseURL, repoKey string) map[string]interface{} {
	if st == nil {
		return nil
	}
	var creator map[string]interface{}
	stStore.Mu.RLock()
	if u := stStore.Users[st.CreatorID]; u != nil {
		creator = store.UserToJSON(u)
	}
	stStore.Mu.RUnlock()
	return map[string]interface{}{
		"id":          st.ID,
		"node_id":     st.NodeID,
		"state":       st.State,
		"description": st.Description,
		"target_url":  st.TargetURL,
		"context":     st.Context,
		"avatar_url":  "",
		"creator":     creator,
		"created_at":  st.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":  st.UpdatedAt.UTC().Format(time.RFC3339),
		"url":         fmt.Sprintf("%s/api/v3/repos/%s/statuses/%d", baseURL, repoKey, st.ID),
	}
}

package bleephub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// GitHub organization issue types: named, colored classifications (Bug, Epic,
// Task, ...) an organization defines once and assigns to issues in any of its
// repositories.

func (s *Server) registerGHIssueTypeRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/issue-types",
		s.requirePerm(store.ScopeIssues, store.PermRead, s.handleListRepoIssueTypes))
	s.route("GET /api/v3/orgs/{org}/issue-types",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.orgGated(s.handleListOrgIssueTypes)))
	s.route("POST /api/v3/orgs/{org}/issue-types",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleCreateOrgIssueType)))
	s.route("PUT /api/v3/orgs/{org}/issue-types/{issue_type_id}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleUpdateOrgIssueType)))
	s.route("DELETE /api/v3/orgs/{org}/issue-types/{issue_type_id}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleDeleteOrgIssueType)))
}

// handleListRepoIssueTypes returns the enabled issue types defined by the
// repository owner's organization. User-owned repositories have no issue-type
// catalog; private repositories remain concealed from unauthorized callers.
func (s *Server) handleListRepoIssueTypes(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	orgLogin := store.OrgLoginForIssueTypeRepo(repo)
	if orgLogin == "" {
		writeJSON(w, http.StatusOK, []map[string]interface{}{})
		return
	}
	types := s.store.ListIssueTypes(orgLogin)
	out := make([]map[string]interface{}, 0, len(types))
	for _, issueType := range types {
		if issueType.IsEnabled {
			out = append(out, issueTypeJSON(issueType))
		}
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

// writeGHValidationErrorSimple writes GitHub's validation-error-simple shape
// (a bare string per error). Many operations document exactly this form for
// their 422 — issue-types/issue-fields, repository activity, dependabot alert
// updates, topics, branch protection, runner labels, JIT config, credential
// revocation, sub-issue priority — where writeGHValidationError's object form
// would be a wire-format deviation (the shape gate flags it).
func writeGHValidationErrorSimple(w http.ResponseWriter, errs ...string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"message":           "Validation Failed",
		"documentation_url": "https://docs.github.com/rest",
		"errors":            errs,
	})
}

var issueTypeColors = map[string]bool{
	"gray": true, "blue": true, "green": true, "yellow": true,
	"orange": true, "red": true, "pink": true, "purple": true,
}

func (s *Server) handleListOrgIssueTypes(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	types := s.store.ListIssueTypes(org)
	out := make([]map[string]interface{}, 0, len(types))
	for _, it := range types {
		out = append(out, issueTypeJSON(it))
	}
	writeJSON(w, http.StatusOK, out)
}

type issueTypeRequest struct {
	Name        *string `json:"name"`
	IsEnabled   *bool   `json:"is_enabled"`
	Description *string `json:"description"`
	Color       *string `json:"color"`
}

func (req *issueTypeRequest) validate(w http.ResponseWriter) bool {
	if req.Name == nil || *req.Name == "" {
		writeGHValidationErrorSimple(w, "name is required")
		return false
	}
	if req.IsEnabled == nil {
		writeGHValidationErrorSimple(w, "is_enabled is required")
		return false
	}
	if req.Color != nil && !issueTypeColors[*req.Color] {
		writeGHValidationErrorSimple(w, fmt.Sprintf("color %q is not a supported issue type color", *req.Color))
		return false
	}
	return true
}

func (s *Server) handleCreateOrgIssueType(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	var req issueTypeRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !req.validate(w) {
		return
	}
	it := s.store.CreateIssueType(org, *req.Name, req.Description, req.Color, *req.IsEnabled)
	writeJSON(w, http.StatusOK, issueTypeJSON(it))
}

func (s *Server) handleUpdateOrgIssueType(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	id, err := strconv.Atoi(r.PathValue("issue_type_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req issueTypeRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !req.validate(w) {
		return
	}
	it := s.store.UpdateIssueType(org, id, *req.Name, req.Description, req.Color, *req.IsEnabled)
	if it == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, issueTypeJSON(it))
}

func (s *Server) handleDeleteOrgIssueType(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	id, err := strconv.Atoi(r.PathValue("issue_type_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.store.DeleteIssueType(org, id) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func issueTypeJSON(it *store.IssueType) map[string]interface{} {
	var desc interface{}
	if it.Description != nil {
		desc = *it.Description
	}
	var color interface{}
	if it.Color != nil {
		color = *it.Color
	}
	return map[string]interface{}{
		"id":          it.ID,
		"node_id":     it.NodeID,
		"name":        it.Name,
		"description": desc,
		"color":       color,
		"is_enabled":  it.IsEnabled,
		"created_at":  it.CreatedAt.Format(time.RFC3339),
		"updated_at":  it.UpdatedAt.Format(time.RFC3339),
	}
}

// findIssueTypeByNodeID moved to internal/graphqlapi with the GraphQL
// resolver layer (ARCH-003); it has no REST callers.

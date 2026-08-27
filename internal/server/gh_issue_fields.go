package bleephub

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Organization issue fields: custom attributes defined at the org level and
// assigned per-issue via the issue-field-values endpoints.

func (s *Server) registerGHIssueFieldRoutes() {
	s.route("GET /api/v3/orgs/{org}/issue-fields",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.orgGated(s.handleListOrgIssueFields)))
	s.route("POST /api/v3/orgs/{org}/issue-fields",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleCreateOrgIssueField)))
	s.route("PATCH /api/v3/orgs/{org}/issue-fields/{issue_field_id}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleUpdateOrgIssueField)))
	s.route("DELETE /api/v3/orgs/{org}/issue-fields/{issue_field_id}",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.orgGated(s.handleDeleteOrgIssueField)))

	// The per-issue GET route dispatches through the shared two-segment issue
	// GET handler (gh_labels_rest.go).
	s.route("POST /api/v3/repos/{owner}/{repo}/issues/{number}/issue-field-values",
		s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleAddIssueFieldValues))
	s.route("PUT /api/v3/repos/{owner}/{repo}/issues/{number}/issue-field-values",
		s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleSetIssueFieldValues))
}

var issueFieldDataTypes = map[string]bool{
	"text": true, "date": true, "single_select": true, "multi_select": true, "number": true,
}

func issueFieldIsSelect(dataType string) bool {
	return dataType == "single_select" || dataType == "multi_select"
}

func (s *Server) handleListOrgIssueFields(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	fields := s.store.ListIssueFields(org)
	out := make([]map[string]interface{}, 0, len(fields))
	for _, f := range fields {
		out = append(out, issueFieldJSON(f))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateOrgIssueField(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	var req struct {
		Name        *string                         `json:"name"`
		Description *string                         `json:"description"`
		DataType    *string                         `json:"data_type"`
		Visibility  *string                         `json:"visibility"`
		Options     []store.IssueFieldOptionRequest `json:"options"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name == nil || *req.Name == "" {
		writeGHValidationErrorSimple(w, "name is required")
		return
	}
	if req.DataType == nil || !issueFieldDataTypes[*req.DataType] {
		writeGHValidationErrorSimple(w, "data_type must be one of text, date, single_select, multi_select, number")
		return
	}
	visibility := "organization_members_only"
	if req.Visibility != nil {
		if *req.Visibility != "organization_members_only" && *req.Visibility != "all" {
			writeGHValidationErrorSimple(w, "visibility must be organization_members_only or all")
			return
		}
		visibility = *req.Visibility
	}
	if issueFieldIsSelect(*req.DataType) && len(req.Options) == 0 {
		writeGHValidationErrorSimple(w, "options are required for single_select and multi_select fields")
		return
	}
	if !issueFieldIsSelect(*req.DataType) && len(req.Options) > 0 {
		writeGHValidationErrorSimple(w, "options are only supported for single_select and multi_select fields")
		return
	}
	for _, opt := range req.Options {
		if opt.Name == nil || *opt.Name == "" {
			writeGHValidationErrorSimple(w, "option name is required")
			return
		}
		if opt.Color == nil || !issueTypeColors[*opt.Color] {
			writeGHValidationErrorSimple(w, "option color is required and must be a supported color")
			return
		}
	}
	f := s.store.CreateIssueField(org, *req.Name, req.Description, *req.DataType, visibility, req.Options)
	writeJSON(w, http.StatusOK, issueFieldJSON(f))
}

func (s *Server) handleUpdateOrgIssueField(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	id, err := strconv.Atoi(r.PathValue("issue_field_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Name        *string                         `json:"name"`
		Description *string                         `json:"description"`
		Visibility  *string                         `json:"visibility"`
		Options     []store.IssueFieldOptionRequest `json:"options"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Visibility != nil && *req.Visibility != "organization_members_only" && *req.Visibility != "all" {
		writeGHValidationErrorSimple(w, "visibility must be organization_members_only or all")
		return
	}
	existing := s.store.GetIssueField(org, id)
	if existing == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if req.Options != nil && !issueFieldIsSelect(existing.DataType) {
		writeGHValidationErrorSimple(w, "options are only supported for single_select and multi_select fields")
		return
	}
	for _, opt := range req.Options {
		if opt.Name == nil || *opt.Name == "" {
			writeGHValidationErrorSimple(w, "option name is required")
			return
		}
		if opt.Color == nil || !issueTypeColors[*opt.Color] {
			writeGHValidationErrorSimple(w, "option color is required and must be a supported color")
			return
		}
		if opt.Priority == nil {
			writeGHValidationErrorSimple(w, "option priority is required")
			return
		}
	}
	f := s.store.UpdateIssueField(org, id, req.Name, req.Description, req.Visibility, req.Options)
	if f == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, issueFieldJSON(f))
}

func (s *Server) handleDeleteOrgIssueField(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	id, err := strconv.Atoi(r.PathValue("issue_field_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.store.DeleteIssueField(org, id) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func issueFieldJSON(f *store.IssueField) map[string]interface{} {
	var desc interface{}
	if f.Description != nil {
		desc = *f.Description
	}
	out := map[string]interface{}{
		"id":          f.ID,
		"node_id":     f.NodeID,
		"name":        f.Name,
		"description": desc,
		"data_type":   f.DataType,
		"visibility":  f.Visibility,
		"created_at":  f.CreatedAt.Format(time.RFC3339),
		"updated_at":  f.UpdatedAt.Format(time.RFC3339),
	}
	if issueFieldIsSelect(f.DataType) {
		opts := make([]map[string]interface{}, 0, len(f.Options))
		for _, opt := range f.Options {
			var optDesc interface{}
			if opt.Description != nil {
				optDesc = *opt.Description
			}
			opts = append(opts, map[string]interface{}{
				"id":          opt.ID,
				"name":        opt.Name,
				"description": optDesc,
				"color":       opt.Color,
				"priority":    opt.Priority,
				"created_at":  opt.CreatedAt.Format(time.RFC3339),
				"updated_at":  opt.UpdatedAt.Format(time.RFC3339),
			})
		}
		out["options"] = opts
	}
	return out
}

// resolveIssueForFieldValues resolves the repo + issue, writing the error
// response on failure.
func (s *Server) resolveIssueForFieldValues(w http.ResponseWriter, r *http.Request) (*store.Repo, *store.Issue, bool) {
	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil, false
	}
	if !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil, false
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil, false
	}
	issue := s.store.GetIssueByNumber(repo.ID, number)
	if issue == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil, false
	}
	return repo, issue, true
}

// issueFieldsOrg returns the owning org login, or "" for a user-owned repo
// (which has no org issue fields).
func issueFieldsOrg(st *store.Store, repo *store.Repo) string {
	orgLogin, _, _ := strings.Cut(repo.FullName, "/")
	if st.GetOrg(orgLogin) == nil {
		return ""
	}
	return orgLogin
}

func (s *Server) handleListIssueFieldValues(w http.ResponseWriter, r *http.Request) {
	repo, issue, ok := s.resolveIssueForFieldValues(w, r)
	if !ok {
		return
	}
	org := issueFieldsOrg(s.store, repo)
	values := s.store.ListIssueFieldValues(org, issue.ID)
	values = paginateAndLink(w, r, values)
	writeJSON(w, http.StatusOK, values)
}

type issueFieldValueRequest struct {
	FieldID *int        `json:"field_id"`
	Value   interface{} `json:"value"`
}

func (s *Server) handleAddIssueFieldValues(w http.ResponseWriter, r *http.Request) {
	s.applyIssueFieldValues(w, r, false)
}

func (s *Server) handleSetIssueFieldValues(w http.ResponseWriter, r *http.Request) {
	s.applyIssueFieldValues(w, r, true)
}

func (s *Server) handleDeleteIssueFieldValue(w http.ResponseWriter, r *http.Request) {
	repo, issue, ok := s.resolveIssueForFieldValues(w, r)
	if !ok {
		return
	}
	if !s.viewerHasRepoPermission(r.Context(), repo, store.ScopeIssues, store.PermWrite) {
		writeGHError(w, http.StatusForbidden, "Must have push access to the repository.")
		return
	}
	fieldID, err := strconv.Atoi(r.PathValue("issue_field_id"))
	if err != nil || !s.store.DeleteIssueFieldValue(issue.ID, fieldID) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// applyIssueFieldValues implements both POST (merge; an empty array clears)
// and PUT (replace) for issue field values.
func (s *Server) applyIssueFieldValues(w http.ResponseWriter, r *http.Request, replace bool) {
	repo, issue, ok := s.resolveIssueForFieldValues(w, r)
	if !ok {
		return
	}
	if !s.viewerHasRepoPermission(r.Context(), repo, store.ScopeIssues, store.PermWrite) {
		writeGHError(w, http.StatusForbidden, "Must have push access to the repository.")
		return
	}
	var req struct {
		IssueFieldValues []issueFieldValueRequest `json:"issue_field_values"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	org := issueFieldsOrg(s.store, repo)

	updates := map[int]interface{}{}
	for _, v := range req.IssueFieldValues {
		if v.FieldID == nil {
			store.WriteGHValidationError(w, "IssueFieldValue", "field_id", "missing_field")
			return
		}
		field := s.store.GetIssueField(org, *v.FieldID)
		if field == nil {
			store.WriteGHValidationError(w, "IssueFieldValue", "field_id", "invalid")
			return
		}
		normalized, err := normalizeIssueFieldValue(field, v.Value)
		if err != nil {
			// GitHub returns the full validation-error shape here, unlike the
			// validation-error-simple the CRUD ops above use.
			writeGHValidationErrorMessage(w, "IssueFieldValue", "value", "invalid", err.Error())
			return
		}
		updates[field.ID] = normalized
	}
	// A POST with an empty array clears all values, like a PUT with nothing.
	if replace || len(req.IssueFieldValues) == 0 {
		s.store.SetIssueFieldValues(issue.ID, updates)
	} else {
		s.store.AddIssueFieldValues(issue.ID, updates)
	}
	writeJSON(w, http.StatusOK, s.store.ListIssueFieldValues(org, issue.ID))
}

// normalizeIssueFieldValue validates a raw JSON value against the field's
// data type and returns the canonical stored form.
func normalizeIssueFieldValue(field *store.IssueField, value interface{}) (interface{}, error) {
	switch field.DataType {
	case "text":
		str, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("field %q expects a string value", field.Name)
		}
		return str, nil
	case "number":
		num, ok := value.(float64)
		if !ok {
			return nil, fmt.Errorf("field %q expects a numeric value", field.Name)
		}
		return num, nil
	case "date":
		str, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("field %q expects an ISO 8601 date string", field.Name)
		}
		if _, err := time.Parse("2006-01-02", str); err != nil {
			if _, err := time.Parse(time.RFC3339, str); err != nil {
				return nil, fmt.Errorf("field %q expects an ISO 8601 date string", field.Name)
			}
		}
		return str, nil
	case "single_select":
		str, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("field %q expects an option name", field.Name)
		}
		for _, opt := range field.Options {
			if opt.Name == str {
				return str, nil
			}
		}
		return nil, fmt.Errorf("%q is not an option of field %q", str, field.Name)
	case "multi_select":
		raw, ok := value.([]interface{})
		if !ok {
			return nil, fmt.Errorf("field %q expects an array of option names", field.Name)
		}
		names := make([]string, 0, len(raw))
		for _, item := range raw {
			str, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("field %q expects an array of option names", field.Name)
			}
			found := false
			for _, opt := range field.Options {
				if opt.Name == str {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("%q is not an option of field %q", str, field.Name)
			}
			names = append(names, str)
		}
		return names, nil
	}
	return nil, fmt.Errorf("field %q has unsupported data type %q", field.Name, field.DataType)
}

// --- store ---

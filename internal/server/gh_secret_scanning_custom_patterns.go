package bleephub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"

	"github.com/e6qu/bleephub/internal/store"
)

func customPatternScope(kind, owner string) string { return kind + ":" + owner }

func validPatternRegexes(pattern string, start, end *string, must, mustNot []string) bool {
	values := []string{pattern}
	if start != nil {
		values = append(values, *start)
	}
	if end != nil {
		values = append(values, *end)
	}
	values = append(values, must...)
	values = append(values, mustNot...)
	for _, value := range values {
		if _, err := regexp.Compile(value); err != nil {
			return false
		}
	}
	return true
}

func (s *Server) repoCustomPatternScope(w http.ResponseWriter, r *http.Request, admin bool) (string, bool) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return "", false
	}
	if admin && !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have admin rights to Repository.")
		return "", false
	}
	return customPatternScope("repo", repo.FullName), true
}

func (s *Server) orgCustomPatternScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.store.GetOrg(r.PathValue("org")) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return "", false
	}
	return customPatternScope("org", r.PathValue("org")), true
}

func (s *Server) listCustomPatterns(w http.ResponseWriter, r *http.Request, scope string) {
	patterns := s.store.ListSecretScanningCustomPatterns(scope)
	// GitHub documents cursor pagination (before/after) on the org and repo
	// custom-pattern lists alongside page/per_page; the shared helper honours
	// the cursors and 422s an unparsable one.
	if r.URL.Query().Get("before") != "" || r.URL.Query().Get("after") != "" {
		page, ok := cursorPageByID(w, r, patterns, func(p *store.SecretScanningCustomPattern) int { return p.ID })
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, page)
		return
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, patterns))
}

func (s *Server) createCustomPatterns(w http.ResponseWriter, r *http.Request, scope string) {
	var request struct {
		Patterns []store.SecretScanningPatternCreate `json:"patterns"`
	}
	if !decodeJSONBody(w, r, &request) {
		return
	}
	// The documented 422 for this batch op is {message, validation_errors}
	// keyed by the zero-based index of the failing pattern, each carrying
	// coded errors — not the generic validation-error envelope.
	writePatternError := func(index int, code, message string) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{
			"message": "Validation Failed",
			"validation_errors": map[string]interface{}{
				strconv.Itoa(index): map[string]interface{}{
					"errors": []map[string]string{{"code": code, "message": message}},
				},
			},
		})
	}
	if len(request.Patterns) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{
			"message": "patterns is required",
		})
		return
	}
	existingNames := map[string]bool{}
	for _, pattern := range s.store.ListSecretScanningCustomPatterns(scope) {
		existingNames[pattern.Name] = true
	}
	for i, pattern := range request.Patterns {
		switch {
		case pattern.Name == "":
			writePatternError(i, "name", "name is required")
			return
		case existingNames[pattern.Name]:
			writePatternError(i, "name", fmt.Sprintf("a pattern named %q already exists", pattern.Name))
			return
		case pattern.Pattern == "":
			writePatternError(i, "invalid", "pattern is required")
			return
		case !validPatternRegexes(pattern.Pattern, pattern.StartDelimiter, pattern.EndDelimiter, pattern.MustMatch, pattern.MustNotMatch):
			writePatternError(i, "invalid", "pattern is not a valid regular expression")
			return
		}
		existingNames[pattern.Name] = true
	}
	created := s.store.CreateSecretScanningCustomPatterns(scope, request.Patterns)
	writeJSON(w, http.StatusCreated, map[string]interface{}{"created_patterns": created})
}

func (s *Server) deleteCustomPatterns(w http.ResponseWriter, r *http.Request, scope string) {
	var request struct {
		Patterns         []store.SecretScanningPatternDelete `json:"patterns"`
		PostDeleteAction string                              `json:"post_delete_action"`
	}
	if !decodeJSONBody(w, r, &request) {
		return
	}
	if len(request.Patterns) == 0 || len(request.Patterns) > 500 {
		store.WriteGHValidationError(w, "SecretScanningCustomPattern", "patterns", "invalid")
		return
	}
	if request.PostDeleteAction != "" && request.PostDeleteAction != "delete_alerts" && request.PostDeleteAction != "resolve_alerts" {
		store.WriteGHValidationError(w, "SecretScanningCustomPattern", "post_delete_action", "invalid")
		return
	}
	found, versionsOK := s.store.DeleteSecretScanningCustomPatterns(scope, request.Patterns)
	if !found {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !versionsOK {
		writeGHError(w, http.StatusPreconditionFailed, "Custom pattern version does not match.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateCustomPattern(w http.ResponseWriter, r *http.Request, scope string) {
	id, err := strconv.Atoi(r.PathValue("pattern_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var raw map[string]json.RawMessage
	if !decodeJSONBody(w, r, &raw) {
		return
	}
	allowed := map[string]bool{
		"pattern": true, "start_delimiter": true, "end_delimiter": true,
		"must_match": true, "must_not_match": true, "custom_pattern_version": true,
	}
	for name := range raw {
		if !allowed[name] {
			store.WriteGHValidationError(w, "SecretScanningCustomPattern", name, "invalid")
			return
		}
	}
	var update store.SecretScanningPatternUpdate
	body, _ := json.Marshal(raw)
	if err := json.Unmarshal(body, &update); err != nil || update.Version == "" {
		store.WriteGHValidationError(w, "SecretScanningCustomPattern", "custom_pattern_version", "missing_field")
		return
	}
	if update.Pattern == nil && update.StartDelimiter == nil && update.EndDelimiter == nil &&
		update.MustMatch == nil && update.MustNotMatch == nil {
		store.WriteGHValidationError(w, "SecretScanningCustomPattern", "pattern", "missing_field")
		return
	}
	var current *store.SecretScanningCustomPattern
	for _, pattern := range s.store.ListSecretScanningCustomPatterns(scope) {
		if pattern.ID == id {
			current = pattern
			break
		}
	}
	if current == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	patternText := current.Pattern
	if update.Pattern != nil {
		patternText = *update.Pattern
	}
	start, end := current.StartDelimiter, current.EndDelimiter
	if update.StartDelimiter != nil {
		start = update.StartDelimiter
	}
	if update.EndDelimiter != nil {
		end = update.EndDelimiter
	}
	must, mustNot := current.MustMatch, current.MustNotMatch
	if update.MustMatch != nil {
		must = *update.MustMatch
	}
	if update.MustNotMatch != nil {
		mustNot = *update.MustNotMatch
	}
	if !validPatternRegexes(patternText, start, end, must, mustNot) {
		store.WriteGHValidationError(w, "SecretScanningCustomPattern", "pattern", "invalid")
		return
	}
	updated, versionOK := s.store.UpdateSecretScanningCustomPattern(scope, id, update)
	if updated == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !versionOK {
		writeGHError(w, http.StatusPreconditionFailed, "Custom pattern version does not match.")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleListRepoSecretScanningCustomPatterns(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.repoCustomPatternScope(w, r, false)
	if ok {
		s.listCustomPatterns(w, r, scope)
	}
}
func (s *Server) handleCreateRepoSecretScanningCustomPatterns(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.repoCustomPatternScope(w, r, true)
	if ok {
		s.createCustomPatterns(w, r, scope)
	}
}
func (s *Server) handleDeleteRepoSecretScanningCustomPatterns(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.repoCustomPatternScope(w, r, true)
	if ok {
		s.deleteCustomPatterns(w, r, scope)
	}
}
func (s *Server) handleUpdateRepoSecretScanningCustomPattern(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.repoCustomPatternScope(w, r, true)
	if ok {
		s.updateCustomPattern(w, r, scope)
	}
}
func (s *Server) handleListOrgSecretScanningCustomPatterns(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.orgCustomPatternScope(w, r)
	if ok {
		s.listCustomPatterns(w, r, scope)
	}
}
func (s *Server) handleCreateOrgSecretScanningCustomPatterns(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.orgCustomPatternScope(w, r)
	if ok {
		s.createCustomPatterns(w, r, scope)
	}
}
func (s *Server) handleDeleteOrgSecretScanningCustomPatterns(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.orgCustomPatternScope(w, r)
	if ok {
		s.deleteCustomPatterns(w, r, scope)
	}
}
func (s *Server) handleUpdateOrgSecretScanningCustomPattern(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.orgCustomPatternScope(w, r)
	if ok {
		s.updateCustomPattern(w, r, scope)
	}
}

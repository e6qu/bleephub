package bleephub

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
)

type SecretScanningCustomPattern struct {
	ID                    int       `json:"id"`
	Name                  string    `json:"name"`
	Pattern               string    `json:"pattern"`
	Slug                  string    `json:"slug"`
	State                 string    `json:"state"`
	PushProtectionEnabled bool      `json:"push_protection_enabled"`
	StartDelimiter        *string   `json:"start_delimiter"`
	EndDelimiter          *string   `json:"end_delimiter"`
	MustMatch             []string  `json:"must_match"`
	MustNotMatch          []string  `json:"must_not_match"`
	Version               string    `json:"custom_pattern_version"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type secretScanningPatternCreate struct {
	Name           string   `json:"name"`
	Pattern        string   `json:"pattern"`
	StartDelimiter *string  `json:"start_delimiter"`
	EndDelimiter   *string  `json:"end_delimiter"`
	MustMatch      []string `json:"must_match"`
	MustNotMatch   []string `json:"must_not_match"`
}

type secretScanningPatternUpdate struct {
	Pattern        *string   `json:"pattern"`
	StartDelimiter *string   `json:"start_delimiter"`
	EndDelimiter   *string   `json:"end_delimiter"`
	MustMatch      *[]string `json:"must_match"`
	MustNotMatch   *[]string `json:"must_not_match"`
	Version        string    `json:"custom_pattern_version"`
}

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

func cloneSecretScanningCustomPattern(pattern *SecretScanningCustomPattern) *SecretScanningCustomPattern {
	if pattern == nil {
		return nil
	}
	copy := *pattern
	copy.MustMatch = append([]string(nil), pattern.MustMatch...)
	copy.MustNotMatch = append([]string(nil), pattern.MustNotMatch...)
	return &copy
}

func (st *Store) ListSecretScanningCustomPatterns(scope string) []*SecretScanningCustomPattern {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]*SecretScanningCustomPattern, 0, len(st.SecretScanningCustomPatterns[scope]))
	for _, pattern := range st.SecretScanningCustomPatterns[scope] {
		out = append(out, cloneSecretScanningCustomPattern(pattern))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (st *Store) CreateSecretScanningCustomPatterns(scope string, specs []secretScanningPatternCreate) []*SecretScanningCustomPattern {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.SecretScanningCustomPatterns[scope] == nil {
		st.SecretScanningCustomPatterns[scope] = map[int]*SecretScanningCustomPattern{}
	}
	now := st.currentTime()
	out := make([]*SecretScanningCustomPattern, 0, len(specs))
	for _, spec := range specs {
		start := spec.StartDelimiter
		if start == nil {
			value := `\A|[^0-9A-Za-z]`
			start = &value
		}
		end := spec.EndDelimiter
		if end == nil {
			value := `\z|[^0-9A-Za-z]`
			end = &value
		}
		pattern := &SecretScanningCustomPattern{
			ID: st.NextSecretScanningPatternID, Name: spec.Name, Pattern: spec.Pattern,
			Slug: slugify(spec.Name), State: "published", PushProtectionEnabled: false,
			StartDelimiter: start, EndDelimiter: end,
			MustMatch:    append([]string(nil), spec.MustMatch...),
			MustNotMatch: append([]string(nil), spec.MustNotMatch...),
			Version:      uuid.NewString(), CreatedAt: now, UpdatedAt: now,
		}
		st.NextSecretScanningPatternID++
		st.SecretScanningCustomPatterns[scope][pattern.ID] = pattern
		out = append(out, cloneSecretScanningCustomPattern(pattern))
	}
	if st.persist != nil {
		st.persist.MustPut("secret_scanning_custom_patterns", scope, st.SecretScanningCustomPatterns[scope])
	}
	return out
}

func (st *Store) UpdateSecretScanningCustomPattern(scope string, id int, update secretScanningPatternUpdate) (*SecretScanningCustomPattern, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	pattern := st.SecretScanningCustomPatterns[scope][id]
	if pattern == nil {
		return nil, false
	}
	if pattern.Version != update.Version {
		return cloneSecretScanningCustomPattern(pattern), false
	}
	if update.Pattern != nil {
		pattern.Pattern = *update.Pattern
	}
	if update.StartDelimiter != nil {
		pattern.StartDelimiter = update.StartDelimiter
	}
	if update.EndDelimiter != nil {
		pattern.EndDelimiter = update.EndDelimiter
	}
	if update.MustMatch != nil {
		pattern.MustMatch = append([]string(nil), (*update.MustMatch)...)
	}
	if update.MustNotMatch != nil {
		pattern.MustNotMatch = append([]string(nil), (*update.MustNotMatch)...)
	}
	pattern.Version = uuid.NewString()
	pattern.UpdatedAt = st.currentTime()
	if st.persist != nil {
		st.persist.MustPut("secret_scanning_custom_patterns", scope, st.SecretScanningCustomPatterns[scope])
	}
	return cloneSecretScanningCustomPattern(pattern), true
}

type secretScanningPatternDelete struct {
	PatternID int    `json:"pattern_id"`
	Version   string `json:"custom_pattern_version"`
}

func (st *Store) DeleteSecretScanningCustomPatterns(scope string, deletes []secretScanningPatternDelete) (found, versionsOK bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, request := range deletes {
		pattern := st.SecretScanningCustomPatterns[scope][request.PatternID]
		if pattern == nil {
			return false, true
		}
		if request.Version != "" && request.Version != pattern.Version {
			return true, false
		}
	}
	for _, request := range deletes {
		delete(st.SecretScanningCustomPatterns[scope], request.PatternID)
	}
	if st.persist != nil {
		st.persist.MustPut("secret_scanning_custom_patterns", scope, st.SecretScanningCustomPatterns[scope])
	}
	return true, true
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
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, patterns))
}

func (s *Server) createCustomPatterns(w http.ResponseWriter, r *http.Request, scope string) {
	var request struct {
		Patterns []secretScanningPatternCreate `json:"patterns"`
	}
	if !decodeJSONBody(w, r, &request) {
		return
	}
	if len(request.Patterns) == 0 {
		writeGHValidationError(w, "SecretScanningCustomPattern", "patterns", "missing_field")
		return
	}
	existingNames := map[string]bool{}
	for _, pattern := range s.store.ListSecretScanningCustomPatterns(scope) {
		existingNames[pattern.Name] = true
	}
	for _, pattern := range request.Patterns {
		if pattern.Name == "" || pattern.Pattern == "" || existingNames[pattern.Name] ||
			!validPatternRegexes(pattern.Pattern, pattern.StartDelimiter, pattern.EndDelimiter, pattern.MustMatch, pattern.MustNotMatch) {
			writeGHValidationError(w, "SecretScanningCustomPattern", "patterns", "invalid")
			return
		}
		existingNames[pattern.Name] = true
	}
	created := s.store.CreateSecretScanningCustomPatterns(scope, request.Patterns)
	writeJSON(w, http.StatusCreated, map[string]interface{}{"created_patterns": created})
}

func (s *Server) deleteCustomPatterns(w http.ResponseWriter, r *http.Request, scope string) {
	var request struct {
		Patterns         []secretScanningPatternDelete `json:"patterns"`
		PostDeleteAction string                        `json:"post_delete_action"`
	}
	if !decodeJSONBody(w, r, &request) {
		return
	}
	if len(request.Patterns) == 0 || len(request.Patterns) > 500 {
		writeGHValidationError(w, "SecretScanningCustomPattern", "patterns", "invalid")
		return
	}
	if request.PostDeleteAction != "" && request.PostDeleteAction != "delete_alerts" && request.PostDeleteAction != "resolve_alerts" {
		writeGHValidationError(w, "SecretScanningCustomPattern", "post_delete_action", "invalid")
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
			writeGHValidationError(w, "SecretScanningCustomPattern", name, "invalid")
			return
		}
	}
	var update secretScanningPatternUpdate
	body, _ := json.Marshal(raw)
	if err := json.Unmarshal(body, &update); err != nil || update.Version == "" {
		writeGHValidationError(w, "SecretScanningCustomPattern", "custom_pattern_version", "missing_field")
		return
	}
	if update.Pattern == nil && update.StartDelimiter == nil && update.EndDelimiter == nil &&
		update.MustMatch == nil && update.MustNotMatch == nil {
		writeGHValidationError(w, "SecretScanningCustomPattern", "pattern", "missing_field")
		return
	}
	var current *SecretScanningCustomPattern
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
		writeGHValidationError(w, "SecretScanningCustomPattern", "pattern", "invalid")
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

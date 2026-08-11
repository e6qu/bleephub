package bleephub

// GitHub code quality setup REST surface (repository-scoped): the
// stored configuration for periodic code quality analysis.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"
)

// CodeQualitySetup is a repository's code quality configuration. Empty
// strings model the null runner_type / runner_label / schedule members.
type CodeQualitySetup struct {
	RepoFullName string     `json:"repo_full_name"`
	State        string     `json:"state"`
	Languages    []string   `json:"languages"`
	RunnerType   string     `json:"runner_type"`
	RunnerLabel  string     `json:"runner_label"`
	Schedule     string     `json:"schedule"`
	UpdatedAt    *time.Time `json:"updated_at"`
}

type CodeQualityFinding struct {
	Number    int                        `json:"number"`
	RepoKey   string                     `json:"repo_key"`
	State     string                     `json:"state"`
	Rule      CodeQualityFindingRule     `json:"rule"`
	Location  CodeQualityFindingLocation `json:"location"`
	Message   CodeQualityFindingMessage  `json:"message"`
	CreatedAt time.Time                  `json:"created_at"`
}

type CodeQualityFindingRule struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Help        string `json:"help,omitempty"`
	Severity    string `json:"severity"`
	Category    string `json:"category"`
}

type CodeQualityFindingLocation struct {
	Path        string `json:"path"`
	StartLine   int    `json:"start_line,omitempty"`
	StartColumn int    `json:"start_column,omitempty"`
	EndLine     int    `json:"end_line,omitempty"`
	EndColumn   int    `json:"end_column,omitempty"`
}

type CodeQualityFindingMessage struct {
	Text     string `json:"text"`
	Markdown string `json:"markdown"`
}

func cloneCodeQualitySetup(setup *CodeQualitySetup) *CodeQualitySetup {
	if setup == nil {
		return nil
	}
	cp := *setup
	cp.Languages = append([]string(nil), setup.Languages...)
	if setup.UpdatedAt != nil {
		updatedAt := *setup.UpdatedAt
		cp.UpdatedAt = &updatedAt
	}
	return &cp
}

// GetCodeQualitySetup returns the repository's code quality setup, or
// the unconfigured default.
func (st *Store) GetCodeQualitySetup(repoFullName string) *CodeQualitySetup {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if setup, ok := st.CodeQualitySetups[repoFullName]; ok && setup != nil {
		return cloneCodeQualitySetup(setup)
	}
	return &CodeQualitySetup{RepoFullName: repoFullName, State: "not-configured", Languages: []string{}}
}

// SetCodeQualitySetup stores the repository's code quality setup.
func (st *Store) SetCodeQualitySetup(setup *CodeQualitySetup) {
	st.mu.Lock()
	defer st.mu.Unlock()
	stored := cloneCodeQualitySetup(setup)
	st.CodeQualitySetups[setup.RepoFullName] = stored
	if st.persist != nil {
		st.persist.MustPut("code_quality_setups", setup.RepoFullName, stored)
	}
}

// PutCodeQualityFinding is the ingestion seam used by code-quality analysis.
// Finding numbers are repository-scoped and stable across persistence reloads.
func (st *Store) PutCodeQualityFinding(finding *CodeQualityFinding) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.CodeQualityFindings[finding.RepoKey] == nil {
		st.CodeQualityFindings[finding.RepoKey] = map[int]*CodeQualityFinding{}
	}
	copy := *finding
	if copy.CreatedAt.IsZero() {
		copy.CreatedAt = time.Now().UTC()
	}
	st.CodeQualityFindings[finding.RepoKey][finding.Number] = &copy
	if st.persist != nil {
		st.persist.MustPut("code_quality_findings", finding.RepoKey, st.CodeQualityFindings[finding.RepoKey])
	}
}

func (st *Store) ListCodeQualityFindings(repoKey, state string) []*CodeQualityFinding {
	st.mu.RLock()
	defer st.mu.RUnlock()
	var out []*CodeQualityFinding
	for _, finding := range st.CodeQualityFindings[repoKey] {
		if state == "" || finding.State == state {
			copy := *finding
			out = append(out, &copy)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return snapshotSlice(out)
}

func (st *Store) GetCodeQualityFinding(repoKey string, number int) *CodeQualityFinding {
	st.mu.RLock()
	defer st.mu.RUnlock()
	finding := st.CodeQualityFindings[repoKey][number]
	if finding == nil {
		return nil
	}
	copy := *finding
	return &copy
}

func (s *Server) registerGHCodeQualityRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/code-quality/setup", s.handleGetCodeQualitySetup)
	s.route("PATCH /api/v3/repos/{owner}/{repo}/code-quality/setup", s.handleUpdateCodeQualitySetup)
	s.route("GET /api/v3/repos/{owner}/{repo}/code-quality/findings", s.handleListCodeQualityFindings)
	s.route("GET /api/v3/repos/{owner}/{repo}/code-quality/findings/{finding_number}", s.handleGetCodeQualityFinding)
}

func (s *Server) codeQualityFindingJSON(r *http.Request, repo *Repo, finding *CodeQualityFinding) map[string]interface{} {
	return map[string]interface{}{
		"number":     finding.Number,
		"state":      finding.State,
		"url":        fmt.Sprintf("%s/api/v3/repos/%s/code-quality/findings/%d", s.baseURL(r), repo.FullName, finding.Number),
		"rule":       finding.Rule,
		"location":   finding.Location,
		"message":    finding.Message,
		"created_at": finding.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func (s *Server) handleListCodeQualityFindings(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	state := r.URL.Query().Get("state")
	if state != "" && state != "open" && state != "dismissed" {
		writeGHValidationError(w, "CodeQualityFinding", "state", "invalid")
		return
	}
	findings := s.store.ListCodeQualityFindings(repo.FullName, state)
	page := paginateAndLink(w, r, findings)
	out := make([]map[string]interface{}, 0, len(page))
	for _, finding := range page {
		out = append(out, s.codeQualityFindingJSON(r, repo, finding))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetCodeQualityFinding(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	number, err := strconv.Atoi(r.PathValue("finding_number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	finding := s.store.GetCodeQualityFinding(repo.FullName, number)
	if finding == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.codeQualityFindingJSON(r, repo, finding))
}

func codeQualitySetupJSON(setup *CodeQualitySetup) map[string]interface{} {
	nullable := func(v string) interface{} {
		if v == "" {
			return nil
		}
		return v
	}
	var updatedAt interface{}
	if setup.UpdatedAt != nil {
		updatedAt = setup.UpdatedAt.Format(time.RFC3339)
	}
	languages := setup.Languages
	if languages == nil {
		languages = []string{}
	}
	return map[string]interface{}{
		"state":        setup.State,
		"languages":    languages,
		"runner_type":  nullable(setup.RunnerType),
		"runner_label": nullable(setup.RunnerLabel),
		"updated_at":   updatedAt,
		"schedule":     nullable(setup.Schedule),
	}
}

func (s *Server) handleGetCodeQualitySetup(w http.ResponseWriter, r *http.Request) {
	if ghUserFromContext(r.Context()) == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	writeJSON(w, http.StatusOK, codeQualitySetupJSON(s.store.GetCodeQualitySetup(repo.FullName)))
}

var codeQualityUpdateLanguages = []string{"csharp", "go", "java-kotlin", "javascript-typescript", "python", "ruby"}

func (s *Server) handleUpdateCodeQualitySetup(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have admin rights to Repository.")
		return
	}

	// The update schema forbids unknown members and requires at least
	// one of state / runner_type / runner_label / languages.
	var raw map[string]json.RawMessage
	if !decodeJSONBody(w, r, &raw) {
		return
	}
	if len(raw) == 0 {
		writeGHError(w, http.StatusUnprocessableEntity,
			"At least one of state, runner_type, runner_label, or languages must be provided.")
		return
	}
	for key := range raw {
		switch key {
		case "state", "runner_type", "runner_label", "languages":
		default:
			writeGHError(w, http.StatusUnprocessableEntity, fmt.Sprintf("Value of %s is invalid", key))
			return
		}
	}

	setup := s.store.GetCodeQualitySetup(repo.FullName)
	if v, ok := raw["state"]; ok {
		var state string
		if err := json.Unmarshal(v, &state); err != nil || (state != "configured" && state != "not-configured") {
			writeGHError(w, http.StatusUnprocessableEntity, "Value of state is invalid")
			return
		}
		setup.State = state
	}
	if v, ok := raw["runner_type"]; ok {
		var runnerType string
		if err := json.Unmarshal(v, &runnerType); err != nil || (runnerType != "standard" && runnerType != "labeled") {
			writeGHError(w, http.StatusUnprocessableEntity, "Value of runner_type is invalid")
			return
		}
		setup.RunnerType = runnerType
		if runnerType == "standard" {
			setup.RunnerLabel = ""
		}
	}
	if v, ok := raw["runner_label"]; ok {
		var label *string
		if err := json.Unmarshal(v, &label); err != nil {
			writeGHError(w, http.StatusUnprocessableEntity, "Value of runner_label is invalid")
			return
		}
		if label == nil {
			setup.RunnerLabel = ""
		} else {
			setup.RunnerLabel = *label
		}
	}
	if v, ok := raw["languages"]; ok {
		var languages []string
		if err := json.Unmarshal(v, &languages); err != nil {
			writeGHError(w, http.StatusUnprocessableEntity, "Value of languages is invalid")
			return
		}
		for _, lang := range languages {
			if !slices.Contains(codeQualityUpdateLanguages, lang) {
				writeGHError(w, http.StatusUnprocessableEntity, "Value of languages is invalid")
				return
			}
		}
		setup.Languages = languages
	}
	if setup.RunnerType == "labeled" && setup.RunnerLabel == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "runner_label is required")
		return
	}
	// The periodic analysis schedule exists exactly while the setup is
	// configured; "weekly" is the only schedule GitHub offers.
	if setup.State == "configured" {
		setup.Schedule = "weekly"
	} else {
		setup.Schedule = ""
	}
	now := time.Now().UTC()
	setup.UpdatedAt = &now
	s.store.SetCodeQualitySetup(setup)
	// bleephub applies the configuration synchronously and runs no
	// analysis workflow, so the documented plain-success response (200
	// with an empty object) is the honest one — 202 is reserved for an
	// update that scheduled an analysis run.
	writeJSON(w, http.StatusOK, map[string]interface{}{})
}

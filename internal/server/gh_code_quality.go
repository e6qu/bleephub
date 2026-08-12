package bleephub

// GitHub code quality setup REST surface (repository-scoped): the
// stored configuration for periodic code quality analysis.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"
)

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

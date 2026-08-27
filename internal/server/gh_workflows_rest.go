package bleephub

// Workflow-file REST surface (`/api/v3/repos/{o}/{r}/actions/workflows`) —
// the YAML files themselves, as opposed to the run-level state at
// `actions/runs`.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/e6qu/bleephub/internal/actions"
	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHWorkflowsRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/workflows", s.handleListGHWorkflows)
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/workflows/{workflow_id}", s.handleGetGHWorkflow)
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/workflows/{workflow_id}/runs", s.handleListWorkflowFileRuns)
	s.route("POST /api/v3/repos/{owner}/{repo}/actions/workflows/{workflow_id}/dispatches",
		s.requirePerm(store.ScopeActions, store.PermWrite, s.handleDispatchWorkflow))
	s.route("PUT /api/v3/repos/{owner}/{repo}/actions/workflows/{workflow_id}/enable",
		s.requirePerm(store.ScopeActions, store.PermWrite, s.handleSetWorkflowState("active")))
	s.route("PUT /api/v3/repos/{owner}/{repo}/actions/workflows/{workflow_id}/disable",
		s.requirePerm(store.ScopeActions, store.PermWrite, s.handleSetWorkflowState("disabled_manually")))
}

// handleSetWorkflowState backs PUT .../workflows/{id}/{enable,disable}: it
// flips the workflow file's persisted state and 204s. Disabled workflows
// neither trigger nor dispatch.
func (s *Server) handleSetWorkflowState(state string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := repoFullName(r)
		s.store.DiscoverWorkflowFilesFromGit(repo)
		wf := s.resolveWorkflowFile(repo, r.PathValue("workflow_id"))
		if wf == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		// wf is a detached snapshot (STORE-021); mutate the live row through the
		// keyed writer so the change is observed in memory, not lost to a clone.
		s.store.SetWorkflowFileState(wf.RepoFullName, wf.Path, state)
		w.WriteHeader(http.StatusNoContent)
	}
}

// workflowFileJSON renders a WorkflowFile in GitHub's `Workflow` shape.
func workflowFileJSON(wf *store.WorkflowFile, baseURL, repoName string) map[string]any {
	repoPath := repoName
	if wf.RepoFullName != "" {
		repoPath = wf.RepoFullName
	}
	apiBase := fmt.Sprintf("%s/api/v3/repos/%s", baseURL, repoPath)
	htmlBase := fmt.Sprintf("%s/%s", baseURL, repoPath)
	badge := fmt.Sprintf("%s/actions/workflows/%s/badge.svg", htmlBase, lastPathSegment(wf.Path))
	return map[string]any{
		"id":         wf.ID,
		"node_id":    wf.NodeID,
		"name":       wf.Name,
		"path":       wf.Path,
		"state":      wf.State,
		"created_at": wf.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"updated_at": wf.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"url":        fmt.Sprintf("%s/actions/workflows/%d", apiBase, wf.ID),
		"html_url":   fmt.Sprintf("%s/actions/workflows/%s", htmlBase, lastPathSegment(wf.Path)),
		"badge_url":  badge,
	}
}

func lastPathSegment(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// handleListGHWorkflows lists every WorkflowFile registered for the repo,
// re-discovering from git storage first so push-time updates are visible.
func (s *Server) handleListGHWorkflows(w http.ResponseWriter, r *http.Request) {
	// Repo-scoped content: gate on repo read visibility.
	if s.lookupReadableRepoFromPath(w, r) == nil {
		return
	}
	repo := repoFullName(r)
	s.store.DiscoverWorkflowFilesFromGit(repo)
	files := s.store.ListWorkflowFiles(repo)
	page := paginateAndLink(w, r, files)
	base := s.baseURL(r)
	workflows := make([]map[string]any, 0, len(page))
	for _, f := range page {
		workflows = append(workflows, workflowFileJSON(f, base, repo))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count": len(files),
		"workflows":   workflows,
	})
}

// handleGetGHWorkflow resolves `workflow_id` (numeric ID or file path, per
// GitHub) and returns the workflow.
func (s *Server) handleGetGHWorkflow(w http.ResponseWriter, r *http.Request) {
	repo := repoFullName(r)
	s.store.DiscoverWorkflowFilesFromGit(repo)
	wf := s.resolveWorkflowFile(repo, r.PathValue("workflow_id"))
	if wf == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, workflowFileJSON(wf, s.baseURL(r), repo))
}

// resolveWorkflowFile matches a `workflow_id` (numeric ID, exact path, or
// basename) to a WorkflowFile, or nil.
func (s *Server) resolveWorkflowFile(repoFullName, idOrPath string) *store.WorkflowFile {
	if id, err := strconv.ParseInt(idOrPath, 10, 64); err == nil {
		if wf := s.store.GetWorkflowFile(repoFullName, id); wf != nil {
			return wf
		}
	}
	for _, wf := range s.store.ListWorkflowFiles(repoFullName) {
		if wf.Path == idOrPath {
			return wf
		}
		if lastPathSegment(wf.Path) == idOrPath {
			return wf
		}
	}
	return nil
}

// handleListWorkflowFileRuns lists the runs for one workflow file, filtered by
// repo + file path (falling back to workflow name for runs with no recorded
// file).
func (s *Server) handleListWorkflowFileRuns(w http.ResponseWriter, r *http.Request) {
	repo := repoFullName(r)
	s.store.DiscoverWorkflowFilesFromGit(repo)
	wf := s.resolveWorkflowFile(repo, r.PathValue("workflow_id"))
	if wf == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	statusFilter := r.URL.Query().Get("status")
	branchFilter := r.URL.Query().Get("branch")
	eventFilter := r.URL.Query().Get("event")

	s.store.Mu.RLock()
	matching := []*store.Workflow{}
	for _, run := range s.store.Workflows {
		if run.RepoFullName != "" && run.RepoFullName != repo {
			continue
		}
		// Attribute by file path, not name: two files both named `CI` are
		// different workflows. The name compare is only for runs recorded
		// before a backing file existed (no path).
		if run.WorkflowFilePath != "" {
			if run.WorkflowFilePath != wf.Path {
				continue
			}
		} else if run.Name != wf.Name {
			continue
		}
		if statusFilter != "" && !runMatchesStatusFilter(run, statusFilter) {
			continue
		}
		if branchFilter != "" && headBranchOf(run) != branchFilter {
			continue
		}
		if eventFilter != "" && eventOf(run) != eventFilter {
			continue
		}
		matching = append(matching, run)
	}
	s.store.Mu.RUnlock()

	sortRunsNewestFirst(matching)
	page := paginateAndLink(w, r, matching)
	base := s.baseURL(r)
	runRepoJSON := s.runRepoJSON(repo, base)
	runs := make([]map[string]any, 0, len(page))
	for _, run := range page {
		runs = append(runs, workflowRunJSON(run, base, repo, runRepoJSON))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count":   len(matching),
		"workflow_runs": runs,
	})
}

// handleDispatchWorkflow re-submits the cached workflow YAML with the caller's
// ref + inputs and 204s. Body: { "ref": "main", "inputs": {...} }. Uncached
// YAML (e.g. a discovered file with empty body) is a 422, not an empty submit.
func (s *Server) handleDispatchWorkflow(w http.ResponseWriter, r *http.Request) {
	repo := repoFullName(r)
	s.store.DiscoverWorkflowFilesFromGit(repo)
	wf := s.resolveWorkflowFile(repo, r.PathValue("workflow_id"))
	if wf == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if strings.HasPrefix(wf.State, "disabled") {
		writeGHError(w, http.StatusForbidden, "Workflow is disabled")
		return
	}
	if wf.YAML == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "workflow YAML body not cached for this file (re-push to git or re-submit via /api/v3/bleephub/workflow)")
		return
	}

	var req struct {
		Ref    string            `json:"ref"`
		Inputs map[string]string `json:"inputs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	if req.Ref == "" {
		defaultBranch := "main"
		if repoObj := s.store.GetRepoByFullName(repo); repoObj != nil && repoObj.DefaultBranch != "" {
			defaultBranch = repoObj.DefaultBranch
		}
		req.Ref = defaultBranch
	}
	repoParts := actions.SplitRepoKeyParts(repo)
	stor := s.store.GetGitStorage(repoParts[0], repoParts[1])
	if stor == nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Repository git storage is not available")
		return
	}
	resolvedRef, sha := resolveGitHubRefInput(stor, req.Ref)
	if sha == "0000000000000000000000000000000000000000" {
		writeGHError(w, http.StatusUnprocessableEntity, "No ref found for: "+req.Ref)
		return
	}
	req.Ref = resolvedRef

	// Validate inputs against the workflow's workflow_dispatch declarations
	// (unknown/required/defaults/choice/boolean), matching GitHub's 422s.
	on, err := actions.ParseWorkflowOn([]byte(wf.YAML))
	if err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, "parse workflow on: "+err.Error())
		return
	}
	dispatchDef, hasDispatch := on["workflow_dispatch"]
	if !hasDispatch {
		writeGHError(w, http.StatusUnprocessableEntity, "Workflow does not have 'workflow_dispatch' trigger")
		return
	}
	inputs, typedInputs, errMsg := resolveDispatchInputs(dispatchDef, req.Inputs)
	if errMsg != "" {
		writeGHError(w, http.StatusUnprocessableEntity, errMsg)
		return
	}
	req.Inputs = inputs

	def, err := store.ParseWorkflow([]byte(wf.YAML))
	if err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, "parse workflow YAML: "+err.Error())
		return
	}
	def = actions.ExpandMatrixJobs(def)
	if def.Env == nil {
		def.Env = map[string]string{}
	}
	serverURL := s.baseURL(r)
	def.Env["__serverURL"] = serverURL
	def.Env["__defaultImage"] = ""

	eventInputs := make(map[string]interface{}, len(req.Inputs))
	for k, v := range req.Inputs {
		eventInputs[k] = v
	}
	payload := map[string]interface{}{
		"inputs":   eventInputs,
		"ref":      req.Ref,
		"workflow": wf.Path,
	}
	if user := ghUserFromContext(r.Context()); user != nil {
		payload["sender"] = senderPayload(user, s.baseURL(r))
	}
	if repoObj := s.store.GetRepoByFullName(repo); repoObj != nil {
		payload["repository"] = repoPayload(repoObj, s.baseURL(r))
	}

	meta := actions.WorkflowEventMeta{
		EventName:   "workflow_dispatch",
		Ref:         req.Ref,
		Sha:         sha,
		Repo:        repo,
		Inputs:      req.Inputs,
		TypedInputs: typedInputs,
		Payload:     payload,
	}
	if _, err := s.actions.SubmitWorkflow(r.Context(), serverURL, def, "", &meta); err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, "submit: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolveDispatchInputs validates caller inputs against the workflow_dispatch
// declarations and applies defaults. It returns the string form
// (github.event.inputs), the typed form (the `inputs` expression context), and
// a GitHub-cased wire error message ("" when valid).
func resolveDispatchInputs(td *actions.TriggerDef, given map[string]string) (map[string]string, map[string]interface{}, string) {
	inputs := make(map[string]string, len(given))
	var declared map[string]*store.WorkflowInputDef
	if td != nil {
		declared = td.Inputs
	}
	for name, val := range given {
		if _, ok := declared[name]; !ok {
			return nil, nil, fmt.Sprintf("Unexpected inputs provided: [%q]", name)
		}
		inputs[name] = val
	}
	typed := make(map[string]interface{}, len(declared))
	for name, def := range declared {
		val, gotten := inputs[name]
		if !gotten {
			if def.Default != nil {
				val = store.ExprToString(store.NormalizeYAMLValue(def.Default))
				inputs[name] = val
			} else if def.Required {
				return nil, nil, fmt.Sprintf("Required input %q not provided", name)
			} else {
				if def.Type == "boolean" {
					// Undefaulted booleans are false on real GitHub.
					val = "false"
					inputs[name] = val
				} else {
					typed[name] = ""
					continue
				}
			}
		}
		switch def.Type {
		case "boolean":
			switch val {
			case "true":
				typed[name] = true
			case "false":
				typed[name] = false
			default:
				return nil, nil, fmt.Sprintf("Input %q must be 'true' or 'false'", name)
			}
		case "number":
			f, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return nil, nil, fmt.Sprintf("Input %q must be a number", name)
			}
			typed[name] = f
		case "choice":
			ok := false
			for _, opt := range def.Options {
				if store.ExprToString(store.NormalizeYAMLValue(opt)) == val {
					ok = true
					break
				}
			}
			if !ok {
				return nil, nil, fmt.Sprintf("Input %q does not match any of the allowed options", name)
			}
			typed[name] = val
		default:
			typed[name] = val
		}
	}
	return inputs, typed, ""
}

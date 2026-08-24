package bleephub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Checks API.
// CheckRun + CheckSuite are App-owned: real GitHub limits Create/Update to
// GitHub App installation tokens. Bleephub permission-gates by "checks"
// scope (read for reads, write for create/update).

func (s *Server) registerGHChecksRoutes() {
	s.route("POST /api/v3/repos/{owner}/{repo}/check-runs", s.requirePerm(store.ScopeChecks, store.PermWrite, s.handleCreateCheckRun))
	s.route("GET /api/v3/repos/{owner}/{repo}/check-runs/{id}", s.requirePerm(store.ScopeChecks, store.PermRead, s.handleGetCheckRun))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/check-runs/{id}", s.requirePerm(store.ScopeChecks, store.PermWrite, s.handleUpdateCheckRun))
	s.route("GET /api/v3/repos/{owner}/{repo}/check-runs/{id}/annotations", s.requirePerm(store.ScopeChecks, store.PermRead, s.handleListCheckRunAnnotations))
	s.route("GET /api/v3/repos/{owner}/{repo}/commits/{sha}/check-runs", s.requirePerm(store.ScopeChecks, store.PermRead, s.handleListCheckRunsForCommit))
	s.route("GET /api/v3/repos/{owner}/{repo}/commits/{sha}/check-suites", s.requirePerm(store.ScopeChecks, store.PermRead, s.handleListCheckSuitesForCommit))
	s.route("POST /api/v3/repos/{owner}/{repo}/check-suites", s.requirePerm(store.ScopeChecks, store.PermWrite, s.handleCreateCheckSuite))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/check-suites/preferences", s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleUpdateCheckSuitePrefs))
	s.route("GET /api/v3/repos/{owner}/{repo}/check-suites/{id}", s.requirePerm(store.ScopeChecks, store.PermRead, s.handleGetCheckSuite))
	s.route("GET /api/v3/repos/{owner}/{repo}/check-suites/{id}/check-runs", s.requirePerm(store.ScopeChecks, store.PermRead, s.handleListCheckRunsForSuite))
	s.route("POST /api/v3/repos/{owner}/{repo}/check-runs/{id}/rerequest", s.requirePerm(store.ScopeChecks, store.PermWrite, s.handleRerequestCheckRun))
	s.route("POST /api/v3/repos/{owner}/{repo}/check-suites/{id}/rerequest", s.requirePerm(store.ScopeChecks, store.PermWrite, s.handleRerequestCheckSuite))
}

// handleRerequestCheckRun resets a completed check run to queued and fires
// the check_run "rerequested" webhook, asking the owning app to run it again.
func (s *Server) handleRerequestCheckRun(w http.ResponseWriter, r *http.Request) {
	cr := s.checkRunInRepo(w, r)
	if cr == nil {
		return
	}
	if cr.Status != "completed" {
		writeGHError(w, http.StatusUnprocessableEntity, "This check run is not yet completed and cannot be rerequested.")
		return
	}
	s.store.UpdateCheckRun(cr.ID, func(c *store.CheckRun) {
		c.Status = "queued"
		c.Conclusion = ""
		c.CompletedAt = nil
	})
	s.CheckRunEvent(cr.RepoKey, cr.ID, "rerequested")
	writeJSON(w, http.StatusCreated, map[string]interface{}{})
}

// handleRerequestCheckSuite marks a check suite queued and fires the
// check_suite "rerequested" webhook, asking apps to re-create their runs.
func (s *Server) handleRerequestCheckSuite(w http.ResponseWriter, r *http.Request) {
	suite := s.checkSuiteInRepo(w, r)
	if suite == nil {
		return
	}
	s.store.UpdateCheckSuite(suite.ID, func(cs *store.CheckSuite) {
		cs.Status = "queued"
		cs.Conclusion = ""
	})
	s.CheckSuiteEvent(suite.RepoKey, suite.ID, "rerequested")
	writeJSON(w, http.StatusCreated, map[string]interface{}{})
}

func (s *Server) handleCreateCheckRun(w http.ResponseWriter, r *http.Request) {
	repoKey := r.PathValue("owner") + "/" + r.PathValue("repo")
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Name        string                `json:"name"`
		HeadSHA     string                `json:"head_sha"`
		Status      string                `json:"status"`
		Conclusion  string                `json:"conclusion"`
		ExternalID  string                `json:"external_id"`
		DetailsURL  string                `json:"details_url"`
		StartedAt   *time.Time            `json:"started_at"`
		CompletedAt *time.Time            `json:"completed_at"`
		Output      *store.CheckRunOutput `json:"output"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name == "" {
		store.WriteGHValidationError(w, "CheckRun", "name", "missing_field")
		return
	}
	if req.HeadSHA == "" {
		store.WriteGHValidationError(w, "CheckRun", "head_sha", "missing_field")
		return
	}
	appID := appIDFromContext(r.Context())
	cr := s.store.CreateCheckRun(repoKey, req.HeadSHA, req.Name, appID, 0)
	s.store.UpdateCheckRun(cr.ID, func(c *store.CheckRun) {
		if req.Status != "" {
			c.Status = req.Status
		}
		if req.Conclusion != "" {
			c.Conclusion = req.Conclusion
		}
		c.ExternalID = req.ExternalID
		c.DetailsURL = req.DetailsURL
		if req.StartedAt != nil {
			c.StartedAt = *req.StartedAt
		}
		if req.CompletedAt != nil {
			c.CompletedAt = req.CompletedAt
		}
		if req.Output != nil {
			c.Output = req.Output
			c.Output.AnnotationsCount = len(req.Output.Annotations)
		}
	})
	user := ghUserFromContext(r.Context())
	actor := ""
	if user != nil {
		actor = user.Login
	}
	s.recordAuditEvent("check_run.create", actor, "", map[string]interface{}{"repo": repoKey, "check_run_id": cr.ID})
	// A check run created already-completed can be the condition an armed
	// auto-merge was waiting for.
	if run := s.store.GetCheckRun(cr.ID); run != nil && run.Status == "completed" {
		s.maybeAutoMergeHeadSHA(repo, run.HeadSHA)
	}
	checkRunJSON := s.checkRunToJSON(s.store.GetCheckRun(cr.ID), s.baseURL(r))
	writeJSONCreated(w, jsonStringField(checkRunJSON, "url"), checkRunJSON)
}

// checkRunInRepo resolves the check run named by {id}, answering 404 unless it
// belongs to the repository in the URL path. Check run ids are global, so the
// path repository is the only thing that ties the id to a tenant.
func (s *Server) checkRunInRepo(w http.ResponseWriter, r *http.Request) *store.CheckRun {
	repoKey := r.PathValue("owner") + "/" + r.PathValue("repo")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	cr := s.store.GetCheckRun(id)
	if cr == nil || cr.RepoKey != repoKey {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return cr
}

// checkSuiteInRepo is checkRunInRepo for check suites.
func (s *Server) checkSuiteInRepo(w http.ResponseWriter, r *http.Request) *store.CheckSuite {
	repoKey := r.PathValue("owner") + "/" + r.PathValue("repo")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	suite := s.store.GetCheckSuite(id)
	if suite == nil || suite.RepoKey != repoKey {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return suite
}

func (s *Server) handleGetCheckRun(w http.ResponseWriter, r *http.Request) {
	cr := s.checkRunInRepo(w, r)
	if cr == nil {
		return
	}
	writeJSON(w, http.StatusOK, s.checkRunToJSON(cr, s.baseURL(r)))
}

func (s *Server) handleUpdateCheckRun(w http.ResponseWriter, r *http.Request) {
	existing := s.checkRunInRepo(w, r)
	if existing == nil {
		return
	}
	id := existing.ID
	var req struct {
		Name        *string               `json:"name"`
		Status      *string               `json:"status"`
		Conclusion  *string               `json:"conclusion"`
		DetailsURL  *string               `json:"details_url"`
		ExternalID  *string               `json:"external_id"`
		StartedAt   *time.Time            `json:"started_at"`
		CompletedAt *time.Time            `json:"completed_at"`
		Output      *store.CheckRunOutput `json:"output"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	found := s.store.UpdateCheckRun(id, func(cr *store.CheckRun) {
		if req.Name != nil {
			cr.Name = *req.Name
		}
		if req.Status != nil {
			cr.Status = *req.Status
		}
		if req.Conclusion != nil {
			cr.Conclusion = *req.Conclusion
		}
		if req.DetailsURL != nil {
			cr.DetailsURL = *req.DetailsURL
		}
		if req.ExternalID != nil {
			cr.ExternalID = *req.ExternalID
		}
		if req.StartedAt != nil {
			cr.StartedAt = *req.StartedAt
		}
		if req.CompletedAt != nil {
			cr.CompletedAt = req.CompletedAt
		}
		if req.Output != nil {
			cr.Output = req.Output
			cr.Output.AnnotationsCount = len(req.Output.Annotations)
		}
	})
	if !found {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// A check run transitioning to completed can clear the condition an
	// armed auto-merge was waiting for.
	if run := s.store.GetCheckRun(id); run != nil && run.Status == "completed" {
		s.maybeAutoMergeHeadSHA(s.store.GetRepoByFullName(run.RepoKey), run.HeadSHA)
	}
	writeJSON(w, http.StatusOK, s.checkRunToJSON(s.store.GetCheckRun(id), s.baseURL(r)))
}

func (s *Server) handleListCheckRunAnnotations(w http.ResponseWriter, r *http.Request) {
	cr := s.checkRunInRepo(w, r)
	if cr == nil {
		return
	}
	out := []*store.CheckAnnotation{}
	if cr.Output != nil {
		out = cr.Output.Annotations
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleListCheckRunsForCommit(w http.ResponseWriter, r *http.Request) {
	repoKey := r.PathValue("owner") + "/" + r.PathValue("repo")
	sha := r.PathValue("sha")
	q := r.URL.Query()
	status := q.Get("status")
	filter := q.Get("filter")
	if filter == "" {
		filter = "latest"
	}
	if filter != "latest" && filter != "all" {
		store.WriteGHValidationError(w, "CheckRun", "filter", "invalid")
		return
	}
	appID := 0
	if raw := q.Get("app_id"); raw != "" {
		var err error
		appID, err = strconv.Atoi(raw)
		if err != nil || appID < 1 {
			store.WriteGHValidationError(w, "CheckRun", "app_id", "invalid")
			return
		}
	}
	runs := s.store.ListCheckRunsForCommit(repoKey, sha, status, "", appID)
	runs = filterCheckRuns(runs, q.Get("check_name"), "")
	if filter == "latest" {
		runs = latestCheckRuns(runs)
	}
	page := paginateAndLink(w, r, runs)
	out := make([]map[string]interface{}, 0, len(page))
	for _, cr := range page {
		out = append(out, s.checkRunToJSON(cr, s.baseURL(r)))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count": len(runs),
		"check_runs":  out,
	})
}

// filterCheckRuns applies the documented check_name and status filters, which
// keep the runs carrying exactly that name and exactly that status. Either sent
// empty narrows nothing.
func filterCheckRuns(runs []*store.CheckRun, name, status string) []*store.CheckRun {
	if name == "" && status == "" {
		return runs
	}
	out := make([]*store.CheckRun, 0, len(runs))
	for _, run := range runs {
		if name != "" && run.Name != name {
			continue
		}
		if status != "" && run.Status != status {
			continue
		}
		out = append(out, run)
	}
	return out
}

// latestCheckRuns implements the documented default filter: reruns remain
// addressable with filter=all, while the normal listing exposes only the most
// recent run of each named check.
//
// A check is identified by its name within its suite — an app re-reporting a
// name it has already reported supersedes the earlier run rather than adding a
// second one. So the run kept per (suite, name) is the newest, and a suite that
// reports several differently-named checks keeps all of them: keying on the
// suite alone would let one check's rerun hide every sibling check beside it,
// and would collapse a whole suite's listing to a single run.
func latestCheckRuns(runs []*store.CheckRun) []*store.CheckRun {
	type checkKey struct {
		suiteID int64
		name    string
	}
	latest := make(map[checkKey]*store.CheckRun, len(runs))
	for _, run := range runs {
		key := checkKey{suiteID: run.SuiteID, name: run.Name}
		if current := latest[key]; current == nil || run.ID > current.ID {
			latest[key] = run
		}
	}
	out := make([]*store.CheckRun, 0, len(latest))
	for _, run := range latest {
		out = append(out, run)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Server) handleListCheckSuitesForCommit(w http.ResponseWriter, r *http.Request) {
	repoKey := r.PathValue("owner") + "/" + r.PathValue("repo")
	sha := r.PathValue("sha")
	suites := s.store.ListCheckSuitesForCommit(repoKey, sha, 0)
	page := paginateAndLink(w, r, suites)
	out := make([]map[string]interface{}, 0, len(page))
	for _, ss := range page {
		out = append(out, s.checkSuiteToJSON(ss, s.baseURL(r)))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count":  len(suites),
		"check_suites": out,
	})
}

func (s *Server) handleCreateCheckSuite(w http.ResponseWriter, r *http.Request) {
	repoKey := r.PathValue("owner") + "/" + r.PathValue("repo")
	var req struct {
		HeadSHA string `json:"head_sha"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.HeadSHA == "" {
		store.WriteGHValidationError(w, "CheckSuite", "head_sha", "missing_field")
		return
	}
	appID := appIDFromContext(r.Context())
	suite := s.store.CreateCheckSuite(repoKey, "", req.HeadSHA, appID)
	suiteJSON := s.checkSuiteToJSON(suite, s.baseURL(r))
	writeJSONCreated(w, jsonStringField(suiteJSON, "url"), suiteJSON)
}

func (s *Server) handleUpdateCheckSuitePrefs(w http.ResponseWriter, r *http.Request) {
	repoKey := r.PathValue("owner") + "/" + r.PathValue("repo")
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		AutoTriggerChecks []*store.CheckSuitePref `json:"auto_trigger_checks"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	s.store.SetCheckSuitePreferences(repoKey, req.AutoTriggerChecks)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"preferences": map[string]interface{}{
			"auto_trigger_checks": jsonArray(req.AutoTriggerChecks),
		},
		"repository": store.RepoToJSONForViewer(repo, s.store, s.baseURL(r), ghUserFromContext(r.Context())),
	})
}

func (s *Server) handleGetCheckSuite(w http.ResponseWriter, r *http.Request) {
	suite := s.checkSuiteInRepo(w, r)
	if suite == nil {
		return
	}
	writeJSON(w, http.StatusOK, s.checkSuiteToJSON(suite, s.baseURL(r)))
}

func (s *Server) handleListCheckRunsForSuite(w http.ResponseWriter, r *http.Request) {
	suite := s.checkSuiteInRepo(w, r)
	if suite == nil {
		return
	}
	// The suite listing takes the same documented filters as the commit
	// listing, minus app_id — a suite already belongs to one app.
	q := r.URL.Query()
	filter := q.Get("filter")
	if filter == "" {
		filter = "latest"
	}
	if filter != "latest" && filter != "all" {
		store.WriteGHValidationError(w, "CheckRun", "filter", "invalid")
		return
	}
	runs := s.store.ListCheckRunsForSuite(suite.ID)
	runs = filterCheckRuns(runs, q.Get("check_name"), q.Get("status"))
	if filter == "latest" {
		runs = latestCheckRuns(runs)
	}
	page := paginateAndLink(w, r, runs)
	out := make([]map[string]interface{}, 0, len(page))
	for _, cr := range page {
		out = append(out, s.checkRunToJSON(cr, s.baseURL(r)))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count": len(runs),
		"check_runs":  out,
	})
}

// appIDFromContext returns the AppID associated with the request's auth.
// Returns 0 for PAT auth (no App context).
func appIDFromContext(ctx interface {
	Value(any) any
},
) int {
	if t, _ := ctx.Value(ctxInstallationToken).(*store.InstallationToken); t != nil {
		return t.AppID
	}
	if t, _ := ctx.Value(ctxUserToServerToken).(*store.UserToServerToken); t != nil {
		return t.AppID
	}
	if a, _ := ctx.Value(ctxApp).(*store.App); a != nil {
		return a.ID
	}
	return 0
}

// checkAppJSON renders the check's owning GitHub App as the integration
// shape, or null for PAT-created checks (AppID 0), matching real GitHub's
// nullable app member.
func (s *Server) checkAppJSON(appID int) interface{} {
	if appID == 0 {
		return nil
	}
	app := s.store.GetApp(appID)
	if app == nil {
		return nil
	}
	return appToJSON(s.store, app, false, s.publicOrigin())
}

// checkRunToJSON renders the GitHub check-run shape. base is the external
// base URL ("" for webhook payloads, which carry relative API paths like the
// other event payload builders).
func (s *Server) checkRunToJSON(cr *store.CheckRun, base string) map[string]interface{} {
	if cr == nil {
		return nil
	}
	api := base + "/api/v3/repos/" + cr.RepoKey
	var conclusion interface{}
	if cr.Conclusion != "" {
		conclusion = cr.Conclusion
	}
	var completedAt interface{}
	if cr.CompletedAt != nil {
		completedAt = cr.CompletedAt.UTC().Format(time.RFC3339)
	}
	output := map[string]interface{}{
		"title":             nil,
		"summary":           nil,
		"text":              nil,
		"annotations_count": 0,
		"annotations_url":   fmt.Sprintf("%s/check-runs/%d/annotations", api, cr.ID),
	}
	if cr.Output != nil {
		output["title"] = store.NilOrString(cr.Output.Title)
		output["summary"] = store.NilOrString(cr.Output.Summary)
		output["text"] = store.NilOrString(cr.Output.Text)
		output["annotations_count"] = cr.Output.AnnotationsCount
	}
	return map[string]interface{}{
		"id":            cr.ID,
		"node_id":       cr.NodeID,
		"head_sha":      cr.HeadSHA,
		"name":          cr.Name,
		"status":        cr.Status,
		"conclusion":    conclusion,
		"started_at":    cr.StartedAt.UTC().Format(time.RFC3339),
		"completed_at":  completedAt,
		"external_id":   cr.ExternalID,
		"url":           fmt.Sprintf("%s/check-runs/%d", api, cr.ID),
		"html_url":      fmt.Sprintf("%s/%s/runs/%d", base, cr.RepoKey, cr.ID),
		"details_url":   cr.DetailsURL,
		"app":           s.checkAppJSON(cr.AppID),
		"check_suite":   map[string]interface{}{"id": cr.SuiteID},
		"output":        output,
		"pull_requests": []interface{}{},
	}
}

// checkSuiteToJSON renders the GitHub check-suite shape, resolving the head
// commit from the repository's real git storage and embedding the repository
// as a minimal-repository.
func (s *Server) checkSuiteToJSON(suite *store.CheckSuite, base string) map[string]interface{} {
	if suite == nil {
		return nil
	}
	api := base + "/api/v3/repos/" + suite.RepoKey
	var conclusion interface{}
	if suite.Conclusion != "" {
		conclusion = suite.Conclusion
	}
	var headBranch interface{}
	if suite.HeadBranch != "" {
		headBranch = suite.HeadBranch
	}

	var repository interface{}
	var headCommit interface{}
	if owner, name, ok := store.SplitRepoFullName(suite.RepoKey); ok {
		if repo := s.store.GetRepo(owner, name); repo != nil {
			repository = store.RepoToJSON(repo, s.store, base)
		}
		if stor := s.store.GetGitStorage(owner, name); stor != nil {
			if commit, err := object.GetCommit(stor, plumbing.NewHash(suite.HeadSHA)); err == nil {
				headCommit = map[string]interface{}{
					"id":        commit.Hash.String(),
					"tree_id":   commit.TreeHash.String(),
					"message":   strings.TrimSpace(commit.Message),
					"timestamp": commit.Committer.When.UTC().Format(time.RFC3339),
					"author": map[string]interface{}{
						"name":  commit.Author.Name,
						"email": commit.Author.Email,
					},
					"committer": map[string]interface{}{
						"name":  commit.Committer.Name,
						"email": commit.Committer.Email,
					},
				}
			}
		}
	}
	// head_commit is required and non-nullable. When the commit object can't be
	// loaded (missing storage, unresolved SHA), fall back to a minimal
	// simple-commit built from the suite's head SHA rather than emitting null.
	if headCommit == nil {
		headCommit = map[string]interface{}{
			"id":        suite.HeadSHA,
			"tree_id":   suite.HeadSHA,
			"message":   "",
			"timestamp": suite.CreatedAt.UTC().Format(time.RFC3339),
			"author":    map[string]interface{}{"name": "bleephub", "email": "checks@bleephub"},
			"committer": map[string]interface{}{"name": "bleephub", "email": "checks@bleephub"},
		}
	}

	return map[string]interface{}{
		"id":                      suite.ID,
		"node_id":                 suite.NodeID,
		"head_branch":             headBranch,
		"head_sha":                suite.HeadSHA,
		"status":                  suite.Status,
		"conclusion":              conclusion,
		"url":                     fmt.Sprintf("%s/check-suites/%d", api, suite.ID),
		"before":                  nil,
		"after":                   nil,
		"pull_requests":           []interface{}{},
		"app":                     s.checkAppJSON(suite.AppID),
		"repository":              repository,
		"head_commit":             headCommit,
		"latest_check_runs_count": len(s.store.ListCheckRunsForSuite(suite.ID)),
		"check_runs_url":          fmt.Sprintf("%s/check-suites/%d/check-runs", api, suite.ID),
		"created_at":              suite.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":              suite.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

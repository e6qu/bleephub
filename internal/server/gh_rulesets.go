package bleephub

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) registerGHRulesetRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/rulesets", s.requirePerm(scopeAdministration, permRead, s.handleListRulesets))
	s.route("GET /api/v3/repos/{owner}/{repo}/rulesets/rule-suites", s.requirePerm(scopeAdministration, permRead, s.handleListRepoRuleSuites))
	s.route("POST /api/v3/repos/{owner}/{repo}/rulesets", s.requirePerm(scopeAdministration, permWrite, s.handleCreateRuleset))
	s.route("GET /api/v3/repos/{owner}/{repo}/rulesets/{ruleset_id}", s.requirePerm(scopeAdministration, permRead, s.handleGetRuleset))
	s.route("PUT /api/v3/repos/{owner}/{repo}/rulesets/{ruleset_id}", s.requirePerm(scopeAdministration, permWrite, s.handleUpdateRuleset))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/rulesets/{ruleset_id}", s.requirePerm(scopeAdministration, permWrite, s.handleDeleteRuleset))
	s.route("GET /api/v3/repos/{owner}/{repo}/rules/branches/{branch}", s.requirePerm(scopeMetadata, permRead, s.handleListBranchRules))
	// /rulesets/{ruleset_id}/history and /rulesets/rule-suites/{rule_suite_id}
	// both occupy two segments after /rulesets and cannot both be registered
	// directly with Go 1.22's mux; dispatch on the literal segments.
	s.route("GET /api/v3/repos/{owner}/{repo}/rulesets/{p1}/{p2}", s.requirePerm(scopeAdministration, permRead, s.handleRepoRulesetTwoSegDispatch))
	s.route("GET /api/v3/repos/{owner}/{repo}/rulesets/{ruleset_id}/history/{version_id}", s.requirePerm(scopeAdministration, permRead, s.handleGetRulesetVersion))

	s.registerGHOrgRulesetRoutes()
}

func (s *Server) handleListRepoRuleSuites(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.resolveRepo(w, r)
	if repo == nil {
		return
	}
	if !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	suites, ok := filterRulesetSuites(w, r, s.store.ListRepoRulesetSuites(repo.ID), false, s.currentTime())
	if !ok {
		return
	}
	suites = paginateAndLink(w, r, suites)
	out := make([]map[string]interface{}, len(suites))
	for i := range suites {
		out[i] = rulesetSuiteToJSON(&suites[i], false)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRepoRulesetTwoSegDispatch(w http.ResponseWriter, r *http.Request) {
	p1 := r.PathValue("p1")
	p2 := r.PathValue("p2")
	switch {
	case p1 == "rule-suites":
		r.SetPathValue("rule_suite_id", p2)
		s.handleGetRepoRuleSuite(w, r)
	case p2 == "history":
		r.SetPathValue("ruleset_id", p1)
		s.handleListRulesetHistory(w, r)
	default:
		writeGHError(w, http.StatusNotFound, "Not Found")
	}
}

// handleGetRepoRuleSuite serves GET /repos/{owner}/{repo}/rulesets/rule-suites/{rule_suite_id}.
func (s *Server) handleGetRepoRuleSuite(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.resolveRepo(w, r)
	if repo == nil {
		return
	}
	if !s.viewerMayActOnRepo(r.Context(), repo, scopeAdministration, permRead, permWrite) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	suiteID, err := strconv.Atoi(r.PathValue("rule_suite_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	suite := s.store.GetRepoRulesetSuite(repo.ID, suiteID)
	if suite == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, rulesetSuiteToJSON(suite, true))
}

func (s *Server) registerGHOrgRulesetRoutes() {
	s.route("GET /api/v3/orgs/{org}/rulesets", s.requireOrgAdmin(scopeOrgAdministration, permRead, s.handleListOrgRulesets))
	s.route("POST /api/v3/orgs/{org}/rulesets", s.requireOrgAdmin(scopeOrgAdministration, permWrite, s.handleCreateOrgRuleset))
	s.route("GET /api/v3/orgs/{org}/rulesets/rule-suites", s.requireOrgAdmin(scopeOrgAdministration, permRead, s.handleListOrgRuleSuites))
	s.route("GET /api/v3/orgs/{org}/rulesets/{ruleset_id}", s.requireOrgAdmin(scopeOrgAdministration, permRead, s.handleGetOrgRuleset))
	s.route("PUT /api/v3/orgs/{org}/rulesets/{ruleset_id}", s.requireOrgAdmin(scopeOrgAdministration, permWrite, s.handleUpdateOrgRuleset))
	s.route("DELETE /api/v3/orgs/{org}/rulesets/{ruleset_id}", s.requireOrgAdmin(scopeOrgAdministration, permWrite, s.handleDeleteOrgRuleset))
	s.route("GET /api/v3/orgs/{org}/rulesets/{p1}/{p2}", s.requireOrgAdmin(scopeOrgAdministration, permRead, s.handleOrgRulesetTwoSegDispatch("GET")))
	s.route("GET /api/v3/orgs/{org}/rulesets/{p1}/{p2}/{p3}", s.requireOrgAdmin(scopeOrgAdministration, permRead, s.handleOrgRulesetThreeSegDispatch("GET")))
}

func (s *Server) handleOrgRulesetTwoSegDispatch(method string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p1 := r.PathValue("p1")
		p2 := r.PathValue("p2")
		switch {
		case p1 == "rule-suites":
			r.SetPathValue("rule_suite_id", p2)
			s.handleGetOrgRuleSuite(w, r)
		case p2 == "history":
			r.SetPathValue("ruleset_id", p1)
			s.handleListOrgRulesetHistory(w, r)
		default:
			writeGHError(w, http.StatusNotFound, "Not Found")
		}
	}
}

func (s *Server) handleOrgRulesetThreeSegDispatch(method string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p1 := r.PathValue("p1")
		p2 := r.PathValue("p2")
		p3 := r.PathValue("p3")
		if p2 == "history" {
			r.SetPathValue("ruleset_id", p1)
			r.SetPathValue("version_id", p3)
			s.handleGetOrgRulesetVersion(w, r)
			return
		}
		writeGHError(w, http.StatusNotFound, "Not Found")
	}
}

// requireOrgAdmin enforces an organization-administration permission and
// verifies the caller is an admin of the target organization.
func (s *Server) requireOrgAdmin(scope permScope, level permLevel, next http.HandlerFunc) http.HandlerFunc {
	return s.requirePerm(scope, level, func(w http.ResponseWriter, r *http.Request) {
		org := s.store.GetOrg(r.PathValue("org"))
		if org == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		if !s.viewerCanAdminOrg(r.Context(), org.Login) {
			writeGHError(w, http.StatusForbidden, "Must have admin rights to Organization.")
			return
		}
		next(w, r)
	})
}

func (s *Server) resolveRepo(w http.ResponseWriter, r *http.Request) *Repo {
	owner, repoName := r.PathValue("owner"), r.PathValue("repo")
	repo := s.store.GetRepoByFullName(owner + "/" + repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return repo
}

func (s *Server) handleListRulesets(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.resolveRepo(w, r)
	if repo == nil {
		return
	}
	if !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	includeParents := true
	if raw, present := r.URL.Query()["includes_parents"]; present {
		if len(raw) != 1 || (raw[0] != "true" && raw[0] != "false") {
			writeGHValidationError(w, "Ruleset", "includes_parents", "invalid")
			return
		}
		includeParents = raw[0] == "true"
	}
	targets, ok := rulesetTargetFilter(w, r)
	if !ok {
		return
	}
	rulesets := filterRulesetsByTarget(s.store.ListRulesetsForRepository(repo, includeParents), targets)
	if field := invalidRESTPaginationQuery(r); field != "" {
		writeGHValidationError(w, "Pagination", field, "invalid")
		return
	}
	rulesets = paginateAndLink(w, r, rulesets)
	out := make([]map[string]interface{}, len(rulesets))
	for i, rs := range rulesets {
		out[i] = rulesetToJSON(rs, false)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateRuleset(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.resolveRepo(w, r)
	if repo == nil {
		return
	}
	if !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var body Ruleset
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if body.Name == "" {
		writeGHValidationError(w, "ruleset", "name", "missing_field")
		return
	}
	rs := s.store.CreateRuleset(repo, &body)
	writeJSON(w, http.StatusCreated, rulesetToJSON(rs, true))
}

func (s *Server) handleGetRuleset(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.resolveRepo(w, r)
	if repo == nil {
		return
	}
	if !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	rs := s.lookupRuleset(w, r, repo)
	if rs == nil {
		return
	}
	writeJSON(w, http.StatusOK, rulesetToJSON(rs, true))
}

func (s *Server) handleUpdateRuleset(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.resolveRepo(w, r)
	if repo == nil {
		return
	}
	if !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	rs := s.lookupRuleset(w, r, repo)
	if rs == nil {
		return
	}
	var body Ruleset
	if !decodeJSONBody(w, r, &body) {
		return
	}
	updated := s.store.UpdateRuleset(repo, rs, &body, user.ID)
	writeJSON(w, http.StatusOK, rulesetToJSON(updated, true))
}

func (s *Server) handleDeleteRuleset(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.resolveRepo(w, r)
	if repo == nil {
		return
	}
	if !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	rs := s.lookupRuleset(w, r, repo)
	if rs == nil {
		return
	}
	s.store.DeleteRuleset(rs.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListBranchRules(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.resolveRepo(w, r)
	if repo == nil {
		return
	}
	if !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	branch := r.PathValue("branch")
	out := s.store.ListRulesForBranch(repo, branch)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListRulesetHistory(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.resolveRepo(w, r)
	if repo == nil {
		return
	}
	if !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	rs := s.lookupRuleset(w, r, repo)
	if rs == nil {
		return
	}
	versions := s.store.GetRulesetHistory(rs)
	out := make([]map[string]interface{}, len(versions))
	for i, v := range versions {
		out[i] = rulesetVersionJSON(v, false)
	}
	writeJSON(w, http.StatusOK, out)
}

// rulesetVersionJSON renders the GitHub ruleset-version shape (plus the
// ruleset snapshot as `state` for the single-version endpoints).
func rulesetVersionJSON(v RulesetVersion, withState bool) map[string]interface{} {
	actor := map[string]interface{}{}
	if v.ActorID != 0 {
		actor["id"] = v.ActorID
		actor["type"] = "User"
	}
	out := map[string]interface{}{
		"version_id": v.VersionID,
		"actor":      actor,
		"updated_at": v.CreatedAt.UTC().Format(time.RFC3339),
	}
	if withState {
		out["state"] = rulesetToJSON(&v.Ruleset, true)
	}
	return out
}

func (s *Server) handleGetRulesetVersion(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.resolveRepo(w, r)
	if repo == nil {
		return
	}
	if !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	rs := s.lookupRuleset(w, r, repo)
	if rs == nil {
		return
	}
	versionID, err := strconv.Atoi(r.PathValue("version_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	version := s.store.GetRulesetVersion(rs, versionID)
	if version == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, rulesetVersionJSON(*version, true))
}

func (s *Server) lookupRuleset(w http.ResponseWriter, r *http.Request, repo *Repo) *Ruleset {
	idStr := r.PathValue("ruleset_id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	rs := s.store.GetRuleset(id)
	if rs == nil || rs.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return rs
}

func rulesetToJSON(rs *Ruleset, includeBody bool) map[string]interface{} {
	m := map[string]interface{}{
		"id":                      rs.ID,
		"node_id":                 rs.NodeID,
		"name":                    rs.Name,
		"target":                  rs.Target,
		"source_type":             rs.SourceType,
		"source":                  rs.Source,
		"enforcement":             rs.Enforcement,
		"bypass_actors":           rs.BypassActors,
		"current_user_can_bypass": rs.CurrentUserCanBypass,
		"created_at":              rs.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":              rs.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if includeBody {
		m["conditions"] = rs.Conditions
		m["rules"] = rs.Rules
	}
	return m
}

func (s *Server) handleListOrgRulesets(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	targets, ok := rulesetTargetFilter(w, r)
	if !ok {
		return
	}
	if field := invalidRESTPaginationQuery(r); field != "" {
		writeGHValidationError(w, "Pagination", field, "invalid")
		return
	}
	rulesets := filterRulesetsByTarget(s.store.ListOrgRulesets(org.ID), targets)
	rulesets = paginateAndLink(w, r, rulesets)
	out := make([]map[string]interface{}, len(rulesets))
	for i, rs := range rulesets {
		out[i] = rulesetToJSON(rs, false)
	}
	writeJSON(w, http.StatusOK, out)
}

func rulesetTargetFilter(w http.ResponseWriter, r *http.Request) (map[string]bool, bool) {
	raw := r.URL.Query().Get("targets")
	if raw == "" {
		return nil, true
	}
	targets := map[string]bool{}
	for _, target := range strings.Split(raw, ",") {
		switch target {
		case "branch", "tag", "push", "repository":
			targets[target] = true
		default:
			writeGHValidationError(w, "Ruleset", "targets", "invalid")
			return nil, false
		}
	}
	return targets, true
}

func filterRulesetsByTarget(rulesets []*Ruleset, targets map[string]bool) []*Ruleset {
	if len(targets) == 0 {
		return rulesets
	}
	filtered := make([]*Ruleset, 0, len(rulesets))
	for _, ruleset := range rulesets {
		if targets[ruleset.Target] {
			filtered = append(filtered, ruleset)
		}
	}
	return filtered
}

func (s *Server) handleCreateOrgRuleset(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var body Ruleset
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if body.Name == "" {
		writeGHValidationError(w, "ruleset", "name", "missing_field")
		return
	}
	rs := s.store.CreateOrgRuleset(org.ID, body.Name, body.Target, body.Enforcement, body.Conditions, body.Rules)
	writeJSON(w, http.StatusCreated, rulesetToJSON(rs, true))
}

func (s *Server) handleGetOrgRuleset(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	rs := s.lookupOrgRuleset(w, r, org)
	if rs == nil {
		return
	}
	writeJSON(w, http.StatusOK, rulesetToJSON(rs, true))
}

func (s *Server) handleUpdateOrgRuleset(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	rs := s.lookupOrgRuleset(w, r, org)
	if rs == nil {
		return
	}
	var body Ruleset
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if !s.store.UpdateOrgRuleset(rs.ID, user.ID, func(rs *Ruleset) {
		if body.Name != "" {
			rs.Name = body.Name
		}
		if body.Target != "" {
			rs.Target = body.Target
		}
		if body.Enforcement != "" {
			rs.Enforcement = body.Enforcement
		}
		if body.BypassActors != nil {
			rs.BypassActors = body.BypassActors
		}
		if body.CurrentUserCanBypass != "" {
			rs.CurrentUserCanBypass = body.CurrentUserCanBypass
		}
		if len(body.Conditions.RefName.Include) > 0 || len(body.Conditions.RefName.Exclude) > 0 {
			rs.Conditions = body.Conditions
		}
		if body.Rules != nil {
			rs.Rules = body.Rules
		}
	}) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	updated := s.store.GetRuleset(rs.ID)
	writeJSON(w, http.StatusOK, rulesetToJSON(updated, true))
}

func (s *Server) handleDeleteOrgRuleset(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	rs := s.lookupOrgRuleset(w, r, org)
	if rs == nil {
		return
	}
	s.store.DeleteOrgRuleset(rs.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListOrgRuleSuites(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	suites, ok := filterRulesetSuites(w, r, s.store.ListOrgRulesetSuites(org.ID), true, s.currentTime())
	if !ok {
		return
	}
	suites = paginateAndLink(w, r, suites)
	out := make([]map[string]interface{}, len(suites))
	for i := range suites {
		out[i] = rulesetSuiteToJSON(&suites[i], false)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetOrgRuleSuite(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	suiteID, err := strconv.Atoi(r.PathValue("rule_suite_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	suite := s.store.GetOrgRulesetSuite(org.ID, suiteID)
	if suite == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, rulesetSuiteToJSON(suite, true))
}

func (s *Server) handleListOrgRulesetHistory(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	rs := s.lookupOrgRuleset(w, r, org)
	if rs == nil {
		return
	}
	versions := s.store.GetRulesetHistory(rs)
	out := make([]map[string]interface{}, len(versions))
	for i, v := range versions {
		out[i] = rulesetVersionJSON(v, false)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetOrgRulesetVersion(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	rs := s.lookupOrgRuleset(w, r, org)
	if rs == nil {
		return
	}
	versionID, err := strconv.Atoi(r.PathValue("version_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	version := s.store.GetRulesetVersion(rs, versionID)
	if version == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, rulesetVersionJSON(*version, true))
}

func (s *Server) lookupOrgRuleset(w http.ResponseWriter, r *http.Request, org *Org) *Ruleset {
	idStr := r.PathValue("ruleset_id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	rs := s.store.GetOrgRuleset(id)
	if rs == nil || rs.OrgID != org.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return rs
}

func rulesetSuiteToJSON(suite *RulesetSuite, detailed bool) map[string]interface{} {
	out := map[string]interface{}{
		"id":                suite.ID,
		"actor_id":          suite.ActorID,
		"actor_name":        suite.ActorName,
		"before_sha":        suite.BeforeSHA,
		"after_sha":         suite.AfterSHA,
		"ref":               suite.Ref,
		"repository_id":     suite.RepositoryID,
		"repository_name":   suite.RepositoryName,
		"pushed_at":         suite.PushedAt.UTC().Format(time.RFC3339),
		"result":            suite.Result,
		"evaluation_result": suite.EvaluationResult,
	}
	if detailed {
		out["rule_evaluations"] = suite.RuleEvaluations
	}
	return out
}

func filterRulesetSuites(w http.ResponseWriter, r *http.Request, suites []RulesetSuite, allowRepositoryName bool, now time.Time) ([]RulesetSuite, bool) {
	if field := invalidRESTPaginationQuery(r); field != "" {
		writeGHValidationError(w, "Pagination", field, "invalid")
		return nil, false
	}
	q := r.URL.Query()
	result := q.Get("rule_suite_result")
	if result != "" && result != "all" && result != "pass" && result != "fail" && result != "bypass" {
		writeGHValidationError(w, "RuleSuite", "rule_suite_result", "invalid")
		return nil, false
	}
	evaluateStatus := q.Get("evaluate_status")
	if evaluateStatus != "" && evaluateStatus != "all" && evaluateStatus != "active" && evaluateStatus != "evaluate" {
		writeGHValidationError(w, "RuleSuite", "evaluate_status", "invalid")
		return nil, false
	}
	timePeriod := q.Get("time_period")
	if timePeriod == "" {
		timePeriod = "day"
	}
	var duration time.Duration
	switch timePeriod {
	case "hour":
		duration = time.Hour
	case "day":
		duration = 24 * time.Hour
	case "week":
		duration = 7 * 24 * time.Hour
	case "month":
		duration = 30 * 24 * time.Hour
	default:
		writeGHValidationError(w, "RuleSuite", "time_period", "invalid")
		return nil, false
	}
	refFilter := q.Get("ref")
	if strings.ContainsAny(refFilter, "*?[") {
		writeGHValidationError(w, "RuleSuite", "ref", "invalid")
		return nil, false
	}
	cutoff := now.UTC().Add(-duration)
	filtered := make([]RulesetSuite, 0, len(suites))
	for _, suite := range suites {
		if suite.PushedAt.Before(cutoff) {
			continue
		}
		if result != "" && result != "all" && suite.Result != result {
			continue
		}
		if actor := q.Get("actor_name"); actor != "" && (suite.ActorName == nil || !strings.EqualFold(*suite.ActorName, actor)) {
			continue
		}
		if allowRepositoryName {
			if name := q.Get("repository_name"); name != "" && !strings.EqualFold(suite.RepositoryName, name) {
				continue
			}
		}
		if refFilter != "" && !rulesetSuiteRefMatches(suite.Ref, refFilter) {
			continue
		}
		if evaluateStatus != "" && evaluateStatus != "all" && !rulesetSuiteHasEnforcement(&suite, evaluateStatus) {
			continue
		}
		filtered = append(filtered, suite)
	}
	return filtered, true
}

func rulesetSuiteHasEnforcement(suite *RulesetSuite, enforcement string) bool {
	for _, evaluation := range suite.RuleEvaluations {
		if evaluation.Enforcement == enforcement {
			return true
		}
	}
	// Persisted suites created before per-rule evaluations were stored still
	// expose enough aggregate information to answer the filter faithfully.
	if len(suite.RuleEvaluations) == 0 {
		if enforcement == "evaluate" {
			return suite.EvaluationResult != nil
		}
		return enforcement == "active" && suite.EvaluationResult == nil
	}
	return false
}

func rulesetSuiteRefMatches(ref, filter string) bool {
	if strings.HasPrefix(filter, "refs/") {
		return ref == filter
	}
	return strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/") == filter
}

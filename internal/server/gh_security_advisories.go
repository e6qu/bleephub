package bleephub

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHSecurityAdvisoriesRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/security-advisories", s.requirePerm(store.ScopeSecurityEvents, store.PermRead, s.handleListSecurityAdvisories))
	s.route("POST /api/v3/repos/{owner}/{repo}/security-advisories", s.requirePerm(store.ScopeSecurityEvents, store.PermWrite, s.handleCreateSecurityAdvisory))
	s.route("GET /api/v3/repos/{owner}/{repo}/security-advisories/{ghsa_id}", s.requirePerm(store.ScopeSecurityEvents, store.PermRead, s.handleGetSecurityAdvisory))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/security-advisories/{ghsa_id}", s.requirePerm(store.ScopeSecurityEvents, store.PermWrite, s.handleUpdateSecurityAdvisory))
	s.route("POST /api/v3/repos/{owner}/{repo}/security-advisories/{ghsa_id}/cve", s.requirePerm(store.ScopeSecurityEvents, store.PermWrite, s.handleRequestCVE))
	s.route("POST /api/v3/repos/{owner}/{repo}/security-advisories/{ghsa_id}/forks", s.requirePerm(store.ScopeSecurityEvents, store.PermWrite, s.handleCreateTemporaryFork))
	// /reports collides with the {ghsa_id} wildcard in Go 1.22's mux, so the wildcard handler dispatches the /reports endpoint.
	s.route("POST /api/v3/repos/{owner}/{repo}/security-advisories/{ghsa_id}", s.requirePerm(store.ScopeSecurityEvents, store.PermWrite, s.handleSecurityAdvisoryReportsDispatch))
	s.route("GET /api/v3/orgs/{org}/security-advisories", s.requireOrgAdmin(store.ScopeSecurityEvents, store.PermRead, s.handleListOrgSecurityAdvisories))
}

// handleListOrgSecurityAdvisories lists the union of every advisory filed against the organization's repositories.
func (s *Server) handleListOrgSecurityAdvisories(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	type advisoryRow struct {
		advisory *store.SecurityAdvisory
		repo     *store.Repo
	}
	s.store.Mu.RLock()
	var rows []advisoryRow
	for repoKey, byGHSA := range s.store.SecurityAdvisoriesByRepo {
		repo := s.store.ReposByName[repoKey]
		if repo == nil || repo.OwnerType != "Organization" || repo.OwnerID != org.ID {
			continue
		}
		for _, a := range byGHSA {
			rows = append(rows, advisoryRow{advisory: a, repo: repo})
		}
	}
	s.store.Mu.RUnlock()

	if state := r.URL.Query().Get("state"); state != "" {
		kept := rows[:0]
		for _, row := range rows {
			if row.advisory.State == state {
				kept = append(kept, row)
			}
		}
		rows = kept
	}

	sortKey := r.URL.Query().Get("sort")
	asc := r.URL.Query().Get("direction") == "asc"
	sortTime := func(row advisoryRow) time.Time {
		switch sortKey {
		case "updated":
			return row.advisory.UpdatedAt
		case "published":
			if row.advisory.PublishedAt != nil {
				return *row.advisory.PublishedAt
			}
			return time.Time{}
		default: // created
			return row.advisory.CreatedAt
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ti, tj := sortTime(rows[i]), sortTime(rows[j])
		if ti.Equal(tj) {
			if asc {
				return rows[i].advisory.ID < rows[j].advisory.ID
			}
			return rows[i].advisory.ID > rows[j].advisory.ID
		}
		if asc {
			return ti.Before(tj)
		}
		return tj.Before(ti)
	})

	page := paginateAndLink(w, r, rows)
	baseURL := s.baseURL(r)
	out := make([]map[string]interface{}, len(page))
	for i, row := range page {
		out[i] = securityAdvisoryToJSON(row.advisory, row.repo, baseURL, s.store)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListSecurityAdvisories(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	state := r.URL.Query().Get("state")
	severity := r.URL.Query().Get("severity")

	// Callers without security access see only published advisories; unpublished ones are under embargo.
	publishedOnly := !s.viewerHasRepoSecurityAccess(r, repo)

	advisories := s.store.ListSecurityAdvisories(repo.ID)
	filtered := make([]*store.SecurityAdvisory, 0, len(advisories))
	for _, a := range advisories {
		if publishedOnly && !advisoryIsPublic(a) {
			continue
		}
		if state != "" && a.State != state {
			continue
		}
		if severity != "" && a.Severity != severity {
			continue
		}
		filtered = append(filtered, a)
	}

	page := paginateAndLink(w, r, filtered)
	baseURL := s.baseURL(r)
	out := make([]map[string]interface{}, len(page))
	for i, a := range page {
		out[i] = securityAdvisoryToJSON(a, repo, baseURL, s.store)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateSecurityAdvisory(w http.ResponseWriter, r *http.Request) {
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
	if !s.viewerMayActOnRepo(r.Context(), repo, store.ScopeSecurityEvents, store.PermWrite, store.PermAdmin) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var req store.CreateAdvisoryReq
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Summary == "" || req.Severity == "" {
		store.WriteGHValidationError(w, "SecurityAdvisory", "summary", "missing_field")
		return
	}
	if !store.ValidAdvisorySeverity(req.Severity) {
		store.WriteGHValidationError(w, "SecurityAdvisory", "severity", "invalid")
		return
	}
	if !s.validAdvisoryCredits(w, req.Credits) {
		return
	}

	adv, err := s.store.CreateSecurityAdvisoryE(repo.ID, user.ID, req)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if adv == nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	if req.StartPrivateFork {
		// start_private_fork mints the same temporary fork POST .../forks does.
		if fork := s.store.CreateTemporaryFork(repo.ID, adv.GHSAID); fork != nil {
			adv = s.store.GetSecurityAdvisoryByGHSA(repo.ID, adv.GHSAID)
		}
	}
	// A draft create produces neither alerts nor events; an already-published one runs the derivation.
	s.announceAdvisoryPublication(repo, adv, user, false)
	advJSON := securityAdvisoryToJSON(adv, repo, s.baseURL(r), s.store)
	writeJSONCreated(w, jsonStringField(advJSON, "url"), advJSON)
}

func (s *Server) handleGetSecurityAdvisory(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	adv := s.store.GetSecurityAdvisoryByGHSA(repo.ID, r.PathValue("ghsa_id"))
	// Answer an embargoed advisory 404, not 403: a 403 would confirm it exists.
	if adv == nil || (!advisoryIsPublic(adv) && !s.viewerHasRepoSecurityAccess(r, repo)) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, securityAdvisoryToJSON(adv, repo, s.baseURL(r), s.store))
}

// advisoryIsPublic reports whether an advisory has left embargo. A withdrawn advisory stays readable — it was public before withdrawal.
func advisoryIsPublic(a *store.SecurityAdvisory) bool {
	if a == nil || a.PublishedAt == nil {
		return false
	}
	return a.State == "published" || a.State == "withdrawn"
}

func (s *Server) handleUpdateSecurityAdvisory(w http.ResponseWriter, r *http.Request) {
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
	if !s.viewerMayActOnRepo(r.Context(), repo, store.ScopeSecurityEvents, store.PermWrite, store.PermAdmin) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	adv := s.store.GetSecurityAdvisoryByGHSA(repo.ID, r.PathValue("ghsa_id"))
	if adv == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var req struct {
		Summary     string  `json:"summary"`
		Description string  `json:"description"`
		Severity    string  `json:"severity"`
		CVEID       string  `json:"cve_id"`
		CVSSScore   float64 `json:"cvss_score"`
		// cvss_vector_string is the member repository-advisory-update declares.
		CVSSVector string   `json:"cvss_vector_string"`
		CWEs       []string `json:"cwe_ids"`
		State      string   `json:"state"`
		// Pointer distinguishes an absent credits member (keep stored) from a present one (replaces it; [] clears). Same for the collaborator lists.
		Credits            *[]store.SecurityAdvisoryCredit `json:"credits"`
		CollaboratingUsers *[]string                       `json:"collaborating_users"`
		CollaboratingTeams *[]string                       `json:"collaborating_teams"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	if req.State != "" && !store.ValidAdvisoryState(req.State) {
		store.WriteGHValidationError(w, "SecurityAdvisory", "state", "invalid")
		return
	}
	if req.Severity != "" && !store.ValidAdvisorySeverity(req.Severity) {
		store.WriteGHValidationError(w, "SecurityAdvisory", "severity", "invalid")
		return
	}
	if req.Credits != nil && !s.validAdvisoryCredits(w, *req.Credits) {
		return
	}

	publishedBefore := adv.PublishedAt != nil && adv.State == "published"
	if !s.store.UpdateSecurityAdvisory(adv.ID, func(a *store.SecurityAdvisory) {
		if req.Summary != "" {
			a.Summary = req.Summary
		}
		if req.Description != "" {
			a.Description = req.Description
		}
		if req.Severity != "" {
			a.Severity = req.Severity
		}
		if req.CVSSScore != 0 {
			a.CVSSScore = req.CVSSScore
		}
		if req.CVSSVector != "" {
			a.CVSSVector = req.CVSSVector
		}
		if req.CVEID != "" {
			a.CVEID = req.CVEID
		}
		if req.CollaboratingUsers != nil {
			a.CollaboratingUsers = append([]string(nil), (*req.CollaboratingUsers)...)
		}
		if req.CollaboratingTeams != nil {
			a.CollaboratingTeams = append([]string(nil), (*req.CollaboratingTeams)...)
		}
		if req.CWEs != nil {
			a.CWEs = req.CWEs
		}
		if req.State != "" {
			a.State = req.State
			if req.State == "published" && a.PublishedAt == nil {
				now := time.Now().UTC()
				a.PublishedAt = &now
			}
		}
		if req.Credits != nil {
			a.Credits = append([]store.SecurityAdvisoryCredit(nil), (*req.Credits)...)
		}
	}) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// GetSecurityAdvisoryByGHSA returns a detached snapshot (STORE-021), so the pre-update `adv` misses the keyed UpdateSecurityAdvisory's change; re-read.
	adv = s.store.GetSecurityAdvisoryByGHSA(repo.ID, r.PathValue("ghsa_id"))
	s.announceAdvisoryPublication(repo, adv, ghUserFromContext(r.Context()), publishedBefore)
	writeJSON(w, http.StatusOK, securityAdvisoryToJSON(adv, repo, s.baseURL(r), s.store))
}

func (s *Server) handleRequestCVE(w http.ResponseWriter, r *http.Request) {
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
	if !s.viewerMayActOnRepo(r.Context(), repo, store.ScopeSecurityEvents, store.PermWrite, store.PermAdmin) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	adv := s.store.GetSecurityAdvisoryByGHSA(repo.ID, r.PathValue("ghsa_id"))
	if adv == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	ok, err := s.store.RequestCVEE(adv.ID)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeGHError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleCreateTemporaryFork(w http.ResponseWriter, r *http.Request) {
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
	if !s.viewerMayActOnRepo(r.Context(), repo, store.ScopeSecurityEvents, store.PermWrite, store.PermAdmin) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	adv := s.store.GetSecurityAdvisoryByGHSA(repo.ID, r.PathValue("ghsa_id"))
	if adv == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	fork := s.store.CreateTemporaryFork(repo.ID, adv.GHSAID)
	if fork == nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	writeJSON(w, http.StatusAccepted, fullRepoJSON(fork, s.store, s.baseURL(r)))
}

func (s *Server) handleSecurityAdvisoryReportsDispatch(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("ghsa_id") != "reports" {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

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
	if !s.viewerHasRepoPermission(r.Context(), repo, store.ScopeSecurityEvents, store.PermWrite) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var req store.CreateAdvisoryReq
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Summary == "" || req.Severity == "" {
		store.WriteGHValidationError(w, "SecurityAdvisory", "summary", "missing_field")
		return
	}
	if !store.ValidAdvisorySeverity(req.Severity) {
		store.WriteGHValidationError(w, "SecurityAdvisory", "severity", "invalid")
		return
	}

	req.State = "triage"
	// private-vulnerability-report-create has no credits member, so never seed them even if the decoded body carried some.
	req.Credits = nil
	adv, err := s.store.CreateSecurityAdvisoryE(repo.ID, user.ID, req)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if adv == nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	s.store.CreateSecurityAdvisoryReport(store.SecurityAdvisoryReport{
		AdvisoryID:             adv.ID,
		ReporterID:             user.ID,
		Summary:                adv.Summary,
		Description:            adv.Description,
		Severity:               adv.Severity,
		CVSSScore:              adv.CVSSScore,
		CVSSVector:             adv.CVSSVector,
		CWEs:                   adv.CWEs,
		VulnerableVersionRange: adv.VulnerableVersionRange,
		CreatedAt:              time.Now().UTC(),
	})
	// Persist submission acceptance through the store mutator (under the lock)
	// rather than writing the returned snapshot: the getter now detaches, and the
	// old in-place write both raced the store and never persisted the flag.
	s.store.UpdateSecurityAdvisory(adv.ID, func(a *store.SecurityAdvisory) {
		a.SubmissionAccepted = true
	})
	adv.SubmissionAccepted = true // reflect it on the local copy for the event + JSON
	// A private vulnerability report is the one draft-stage transition that emits an event, so maintainers learn one was filed.
	s.emitRepositoryAdvisoryEvent(repo, adv, user, "reported")
	advJSON := securityAdvisoryToJSON(adv, repo, s.baseURL(r), s.store)
	writeJSONCreated(w, jsonStringField(advJSON, "url"), advJSON)
}

// validAdvisoryCredits requires each credit to name a known type and a login that resolves to a user (credits_detailed
// must render a non-null user). Writes the 422 and returns false on the first invalid credit.
func (s *Server) validAdvisoryCredits(w http.ResponseWriter, credits []store.SecurityAdvisoryCredit) bool {
	for _, c := range credits {
		if c.Login == "" || s.store.LookupUserByLogin(c.Login) == nil {
			store.WriteGHValidationError(w, "SecurityAdvisory", "credits.login", "invalid")
			return false
		}
		if !store.ValidAdvisoryCreditType(c.Type) {
			store.WriteGHValidationError(w, "SecurityAdvisory", "credits.type", "invalid")
			return false
		}
	}
	return true
}

// securityAdvisoryCreditsJSON renders both credit shapes: `credits` echoes the {login, type} members, `credits_detailed`
// resolves each login to its user. bleephub auto-accepts, so the state is always "accepted".
func securityAdvisoryCreditsJSON(a *store.SecurityAdvisory, st *store.Store, baseURL string) (credits, detailed []map[string]interface{}) {
	credits = []map[string]interface{}{}
	detailed = []map[string]interface{}{}
	for _, c := range a.Credits {
		u := st.LookupUserByLogin(c.Login)
		if u == nil {
			// Credited account gone; the schema requires a user object, so drop the row from both views.
			continue
		}
		credits = append(credits, map[string]interface{}{
			"login": c.Login,
			"type":  c.Type,
		})
		detailed = append(detailed, map[string]interface{}{
			"user":  store.UserToJSON(u, baseURL),
			"type":  c.Type,
			"state": "accepted",
		})
	}
	return credits, detailed
}

func securityAdvisoryToJSON(a *store.SecurityAdvisory, repo *store.Repo, baseURL string, st *store.Store) map[string]interface{} {
	apiURL := fmt.Sprintf("%s/api/v3/repos/%s/security-advisories/%s", baseURL, repo.FullName, a.GHSAID)
	htmlURL := fmt.Sprintf("%s/%s/security/advisories/%s", baseURL, repo.FullName, a.GHSAID)

	identifiers := []map[string]interface{}{
		{"type": "GHSA", "value": a.GHSAID},
	}
	if a.CVEID != "" {
		identifiers = append(identifiers, map[string]interface{}{"type": "CVE", "value": a.CVEID})
	}

	cwes := make([]map[string]interface{}, 0, len(a.CWEs))
	cweIDs := make([]string, 0, len(a.CWEs))
	for _, cwe := range a.CWEs {
		cwes = append(cwes, map[string]interface{}{"cwe_id": cwe, "name": cweName(cwe)})
		cweIDs = append(cweIDs, cwe)
	}

	var author interface{} = nil
	if u := st.GetUserByID(a.AuthorID); u != nil {
		author = store.UserToJSON(u, baseURL)
	}

	var publishedAt interface{} = nil
	if a.PublishedAt != nil {
		publishedAt = a.PublishedAt.UTC().Format(time.RFC3339)
	}

	// Derive the score from the vector when only a vector was supplied.
	cvssScore := interface{}(nil)
	if score, ok := store.AdvisoryCVSSScore(a); ok {
		cvssScore = score
	}

	var privateFork interface{} = nil
	if a.PrivateForkID != 0 {
		if fork := st.GetRepoByID(a.PrivateForkID); fork != nil {
			privateFork = minimalRepoJSON(fork, st, baseURL)
		}
	}

	vulnerabilities := []map[string]interface{}{}
	for _, v := range a.Vulnerabilities {
		firstPatched := interface{}(nil)
		if v.FirstPatchedVersion != "" {
			firstPatched = v.FirstPatchedVersion
		}
		vulnerabilities = append(vulnerabilities, map[string]interface{}{
			"package": map[string]interface{}{
				"ecosystem": v.PackageEcosystem,
				"name":      v.PackageName,
			},
			"vulnerable_version_range": v.VulnerableVersionRange,
			"patched_versions":         firstPatched,
			"vulnerable_functions":     advisoryVulnerableFunctions(v),
		})
	}
	creditsJSON, creditsDetailedJSON := securityAdvisoryCreditsJSON(a, st, baseURL)

	if len(vulnerabilities) == 0 && a.VulnerableVersionRange != "" {
		vulnerabilities = append(vulnerabilities, map[string]interface{}{
			// package is nullable; its ecosystem/name are not, so emit a null package rather than an object with null members.
			"package":                  nil,
			"vulnerable_version_range": a.VulnerableVersionRange,
			"patched_versions":         nil,
			"vulnerable_functions":     []string{},
		})
	}

	return map[string]interface{}{
		"ghsa_id":             a.GHSAID,
		"cve_id":              nullOrString(a.CVEID),
		"url":                 apiURL,
		"html_url":            htmlURL,
		"summary":             a.Summary,
		"description":         nullOrString(a.Description),
		"severity":            a.Severity,
		"author":              author,
		"publisher":           nil,
		"identifiers":         identifiers,
		"state":               a.State,
		"created_at":          a.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":          a.UpdatedAt.UTC().Format(time.RFC3339),
		"published_at":        publishedAt,
		"closed_at":           nil,
		"withdrawn_at":        nil,
		"submission":          map[string]interface{}{"accepted": a.SubmissionAccepted},
		"vulnerabilities":     vulnerabilities,
		"cvss":                map[string]interface{}{"vector_string": nullOrString(a.CVSSVector), "score": cvssScore},
		"cwes":                cwes,
		"cwe_ids":             cweIDs,
		"credits":             creditsJSON,
		"credits_detailed":    creditsDetailedJSON,
		"collaborating_users": advisoryCollaboratingUsers(a, st, baseURL),
		"collaborating_teams": advisoryCollaboratingTeams(a, st, baseURL),
		"private_fork":        privateFork,
	}
}

func cweName(cwe string) string {
	if strings.HasPrefix(cwe, "CWE-") {
		return cwe
	}
	return "CWE-" + cwe
}

// advisoryVulnerableFunctions renders affected functions as a non-null array — [] when none are named.
func advisoryVulnerableFunctions(v store.SecurityAdvisoryVulnerability) []string {
	if v.VulnerableFunctions == nil {
		return []string{}
	}
	return v.VulnerableFunctions
}

// advisoryCollaboratingUsers renders the accounts granted access to the advisory's private workspace,
// dropping any since-deleted account (a null entry would break a typed SDK).
func advisoryCollaboratingUsers(a *store.SecurityAdvisory, st *store.Store, baseURL string) []map[string]interface{} {
	users := make([]map[string]interface{}, 0, len(a.CollaboratingUsers))
	for _, login := range a.CollaboratingUsers {
		if user := st.LookupUserByLogin(login); user != nil {
			users = append(users, store.UserToJSON(user, baseURL))
		}
	}
	return users
}

// advisoryCollaboratingTeams renders the teams granted access to the advisory's private workspace. Only teams of the repo's
// owning org can hold the grant, so a user-owned repo has none. Rendering via teamToJSON keeps a team here byte-identical to the same team from /orgs/{org}/teams.
func advisoryCollaboratingTeams(a *store.SecurityAdvisory, st *store.Store, baseURL string) []map[string]interface{} {
	repo := st.GetRepoByID(a.RepoID)
	teams := make([]map[string]interface{}, 0, len(a.CollaboratingTeams))
	if repo == nil || repo.OwnerType != "Organization" {
		return teams
	}
	org := st.GetOrgByID(repo.OwnerID)
	if org == nil {
		return teams
	}
	for _, slug := range a.CollaboratingTeams {
		if team := st.GetTeam(org.Login, slug); team != nil {
			teams = append(teams, teamToJSON(team, org, st, baseURL))
		}
	}
	return teams
}

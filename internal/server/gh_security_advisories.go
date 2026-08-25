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
	// The literal /reports path conflicts with the {ghsa_id} wildcard in Go 1.22's mux,
	// so the wildcard dispatches to the real /security-advisories/reports endpoint.
	s.route("POST /api/v3/repos/{owner}/{repo}/security-advisories/{ghsa_id}", s.requirePerm(store.ScopeSecurityEvents, store.PermWrite, s.handleSecurityAdvisoryReportsDispatch))
	s.route("GET /api/v3/orgs/{org}/security-advisories", s.requireOrgAdmin(store.ScopeSecurityEvents, store.PermRead, s.handleListOrgSecurityAdvisories))
}

// handleListOrgSecurityAdvisories implements GET /orgs/{org}/security-advisories:
// the union of every advisory filed against the organization's repositories.
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
			// Tiebreak equal timestamps by ID for a stable order.
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

	// A published advisory is a public fact about a package; an unpublished
	// one is under embargo and belongs to the repository's security team.
	// Without this split, every draft advisory on a public repository was
	// listed to any account holding a token — the embargo existed only in
	// the state field.
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
		// The spec's start_private_fork member asks for the temporary private
		// fork maintainers develop the fix in, which is the same fork POST
		// .../forks mints — so the advisory carries one either way it was
		// asked for.
		if fork := s.store.CreateTemporaryFork(repo.ID, adv.GHSAID); fork != nil {
			adv = s.store.GetSecurityAdvisoryByGHSA(repo.ID, adv.GHSAID)
		}
	}
	// announceAdvisoryPublication runs the derivation for a create that is
	// already published; a draft create produces neither alerts nor events.
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
	// An embargoed advisory answers 404 rather than 403 to a caller without
	// security access: 403 would confirm the advisory exists, which is the
	// one thing an embargo is meant to withhold.
	if adv == nil || (!advisoryIsPublic(adv) && !s.viewerHasRepoSecurityAccess(r, repo)) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, securityAdvisoryToJSON(adv, repo, s.baseURL(r), s.store))
}

// advisoryIsPublic reports whether an advisory has left embargo. A withdrawn
// advisory was public before it was withdrawn, so it stays readable — the
// withdrawal is the news.
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
		// cvss_vector_string is the member repository-advisory-update
		// declares; the old cvss_vector spelling matched no GitHub client.
		CVSSVector string   `json:"cvss_vector_string"`
		CWEs       []string `json:"cwe_ids"`
		State      string   `json:"state"`
		// A pointer distinguishes an absent (or null) credits member — keep
		// the stored list — from a present one, which replaces it ([] clears).
		Credits *[]store.SecurityAdvisoryCredit `json:"credits"`
		// The same absent-versus-present distinction for the two collaborator
		// lists, which [] clears.
		CollaboratingUsers *[]string `json:"collaborating_users"`
		CollaboratingTeams *[]string `json:"collaborating_teams"`
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
	// GetSecurityAdvisoryByGHSA returns a detached snapshot (STORE-021), so the
	// pre-update `adv` no longer reflects the mutation applied by the keyed
	// UpdateSecurityAdvisory above; re-read the fresh state to publish and render.
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
	// The spec's private-vulnerability-report-create request has no credits
	// member (only repository-advisory-create/update carry one), so a report
	// never seeds credits even when the decoded body happened to include them.
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
	adv.SubmissionAccepted = true
	// A private vulnerability report is the one draft-stage transition that
	// does produce an event: the repository's maintainers have to learn that
	// somebody filed one, which is exactly what repository_advisory
	// "reported" tells them.
	s.emitRepositoryAdvisoryEvent(repo, adv, user, "reported")
	advJSON := securityAdvisoryToJSON(adv, repo, s.baseURL(r), s.store)
	writeJSONCreated(w, jsonStringField(advJSON, "url"), advJSON)
}

// validAdvisoryCredits enforces the spec's credit member constraints — a
// known credit type from the security-advisory-credit-types enum and a login
// that resolves to a user (credits_detailed must render a non-null user
// object). Writes the 422 and reports false when a credit is invalid.
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

// securityAdvisoryCreditsJSON renders the advisory's credits in both published
// shapes: `credits` echoes the {login, type} request members, and
// `credits_detailed` resolves each login to its user object with the credit
// state — bleephub auto-accepts, so the state is always "accepted".
func securityAdvisoryCreditsJSON(a *store.SecurityAdvisory, st *store.Store, baseURL string) (credits, detailed []map[string]interface{}) {
	credits = []map[string]interface{}{}
	detailed = []map[string]interface{}{}
	for _, c := range a.Credits {
		u := st.LookupUserByLogin(c.Login)
		if u == nil {
			// The credited account disappeared after storage; the response
			// schema requires a user object, so drop the row from both views.
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

	// The score is derived from the vector when the author supplied only a
	// vector, which is all repository-advisory-create accepts.
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
			// package itself is nullable; its ecosystem/name are not, so emit a
			// null package rather than a package object with null members.
			"package":                  nil,
			"vulnerable_version_range": a.VulnerableVersionRange,
			"patched_versions":         nil,
			// This branch renders an advisory that named a version range but
			// no package, so there is no vulnerability entry to read affected
			// functions from — and the member is a non-null array.
			"vulnerable_functions": []string{},
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

// advisoryVulnerableFunctions renders a vulnerability's affected functions.
// The member is a non-null array in the spec, so a vulnerability that names
// none renders [] rather than null.
func advisoryVulnerableFunctions(v store.SecurityAdvisoryVulnerability) []string {
	if v.VulnerableFunctions == nil {
		return []string{}
	}
	return v.VulnerableFunctions
}

// advisoryCollaboratingUsers renders the accounts granted access to an
// advisory's private drafting workspace, dropping any whose account has since
// been deleted — a collaborator list is a list of accounts, and a null entry
// in it is not something a typed SDK can decode.
func advisoryCollaboratingUsers(a *store.SecurityAdvisory, st *store.Store, baseURL string) []map[string]interface{} {
	users := make([]map[string]interface{}, 0, len(a.CollaboratingUsers))
	for _, login := range a.CollaboratingUsers {
		if user := st.LookupUserByLogin(login); user != nil {
			users = append(users, store.UserToJSON(user, baseURL))
		}
	}
	return users
}

// advisoryCollaboratingTeams renders the teams granted access to an
// advisory's private drafting workspace. A team is named by slug, and only
// teams of the repository's owning organization can hold the grant, so a repo
// owned by a user has no teams to grant it to.
//
// The rendering goes through teamToJSON so a team here is byte-identical to
// the same team served from /orgs/{org}/teams — a client decoding one into
// its Team type decodes the other.
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

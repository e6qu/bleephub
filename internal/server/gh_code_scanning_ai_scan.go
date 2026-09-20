package bleephub

// The code scanning AI Scan setting, at the organization and the repository.
// The two levels and how one bounds the other are described in
// internal/store/code_scanning_ai_scan.go.

import (
	"encoding/json"
	"net/http"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHCodeScanningAIScanRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/code-scanning/ai-scan",
		s.requirePerm(store.ScopeSecurityEvents, store.PermRead, s.handleGetRepoCodeScanningAIScan))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/code-scanning/ai-scan",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleUpdateRepoCodeScanningAIScan))
	// The organization setting is for owners and security managers, which no
	// permission scope describes: requirePerm would hold it to administrator
	// standing. The route's own token gate still demands an organization scope
	// of the credential, and the handlers check the standing.
	s.route("GET /api/v3/orgs/{org}/code-scanning/ai-scan", s.orgGated(s.handleGetOrgCodeScanningAIScan))
	s.route("PATCH /api/v3/orgs/{org}/code-scanning/ai-scan", s.orgGated(s.handleUpdateOrgCodeScanningAIScan))
}

// decodeAIScanUpdate reads the update body. The schema allows `pr_scan` and
// nothing else, and requires at least one member, so an empty object and an
// unknown member are both refused rather than accepted as a no-op.
func decodeAIScanUpdate(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body map[string]json.RawMessage
	if !decodeJSONBody(w, r, &body) {
		return "", false
	}
	raw, present := body["pr_scan"]
	if !present || len(body) != 1 {
		store.WriteGHValidationError(w, "CodeScanningAIScan", "pr_scan", "missing_field")
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil ||
		(value != store.CodeScanningAIScanEnabled && value != store.CodeScanningAIScanDisabled) {
		store.WriteGHValidationError(w, "CodeScanningAIScan", "pr_scan", "invalid")
		return "", false
	}
	return value, true
}

// viewerManagesOrgSecurity reports whether the caller is an owner of the
// organization or holds its security manager role, the two standings GitHub
// names for this setting.
func (s *Server) viewerManagesOrgSecurity(r *http.Request, orgLogin string) bool {
	if s.viewerCanAdminOrg(r.Context(), orgLogin) {
		return true
	}
	// The role is a person's. An installation or a workflow token acts for an
	// app, not for whoever holds the role, so only a user credential counts.
	user := ghUserFromContext(r.Context())
	if user == nil || ghInstallationTokenFromContext(r.Context()) != nil || ghJobTokenFromContext(r.Context()) != nil {
		return false
	}
	_, manager := s.store.ListUsersWithOrgRole(orgLogin, securityManagerOrgRoleID)[user.ID]
	return manager
}

// requireOrgSecurityManager answers a caller who may not manage the
// organization's security settings, reporting whether it did.
func (s *Server) requireOrgSecurityManager(w http.ResponseWriter, r *http.Request, orgLogin string) bool {
	if ghUserFromContext(r.Context()) == nil && ghInstallationTokenFromContext(r.Context()) == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return false
	}
	if !s.viewerManagesOrgSecurity(r, orgLogin) {
		writeGHError(w, http.StatusForbidden, "Must be an organization owner or security manager.")
		return false
	}
	return true
}

func (s *Server) handleGetOrgCodeScanningAIScan(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	if !s.requireOrgSecurityManager(w, r, org) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"pr_scan": s.store.OrgCodeScanningAIScan(org)})
}

func (s *Server) handleUpdateOrgCodeScanningAIScan(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	if !s.requireOrgSecurityManager(w, r, org) {
		return
	}
	value, ok := decodeAIScanUpdate(w, r)
	if !ok {
		return
	}
	if !s.store.UpdateOrg(org, func(o *store.Org) { o.CodeScanningAIScan = value }) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"pr_scan": s.store.OrgCodeScanningAIScan(org)})
}

func (s *Server) handleGetRepoCodeScanningAIScan(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	setting, _ := s.store.RepoCodeScanningAIScan(repo)
	writeJSON(w, http.StatusOK, map[string]interface{}{"pr_scan": setting})
}

func (s *Server) handleUpdateRepoCodeScanningAIScan(w http.ResponseWriter, r *http.Request) {
	repo := s.resolveRepo(w, r)
	if repo == nil {
		return
	}
	value, ok := decodeAIScanUpdate(w, r)
	if !ok {
		return
	}
	if _, disabledByOrg := s.store.RepoCodeScanningAIScan(repo); disabledByOrg && value == store.CodeScanningAIScanEnabled {
		writeGHValidationErrorMessage(w, "CodeScanningAIScan", "pr_scan", "invalid",
			"AI Scan is disabled by the organization and cannot be enabled for this repository.")
		return
	}
	owner, name := splitRepoFull(repo.FullName)
	if !s.store.UpdateRepo(owner, name, func(stored *store.Repo) { stored.CodeScanningAIScan = value }) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	setting, _ := s.store.RepoCodeScanningAIScan(repo)
	writeJSON(w, http.StatusOK, map[string]interface{}{"pr_scan": setting})
}

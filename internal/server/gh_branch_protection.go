package bleephub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// bpRequest is the sparse PUT body: a missing sub-object leaves its rule
// unchanged, an explicit null disables it.
type bpRequest struct {
	RequiredStatusChecks           *store.BPStatusChecks       `json:"required_status_checks"`
	RequiredPullRequestReviews     *store.BPPullRequestReviews `json:"required_pull_request_reviews"`
	EnforceAdmins                  *flexBool                   `json:"enforce_admins"`
	Restrictions                   *store.BPRestrictions       `json:"restrictions"`
	RequiredLinearHistory          *flexBool                   `json:"required_linear_history"`
	AllowForcePushes               *flexBool                   `json:"allow_force_pushes"`
	AllowDeletions                 *flexBool                   `json:"allow_deletions"`
	BlockCreations                 *flexBool                   `json:"block_creations"`
	RequiredConversationResolution *flexBool                   `json:"required_conversation_resolution"`
	RequiredSignatures             *flexBool                   `json:"required_signatures"`
	LockBranch                     *flexBool                   `json:"lock_branch"`
	AllowForkSyncing               *flexBool                   `json:"allow_fork_syncing"`
}

func (s *Server) registerGHBranchProtectionRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBranchProtectionGet))
	s.route("PUT /api/v3/repos/{owner}/{repo}/branches/{branch}/protection",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBranchProtectionPut))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBranchProtectionDelete))

	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPStatusChecksGet))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPStatusChecksPatch))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPStatusChecksDelete))

	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_signatures", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPRequiredSignaturesGet))
	s.route("POST /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_signatures",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRequiredSignaturesPost))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_signatures",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRequiredSignaturesDelete))

	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/apps", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPRestrictionsAppsGet))
	s.route("POST /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/apps",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsAppsPost))
	s.route("PUT /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/apps",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsAppsPut))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/apps",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsAppsDelete))

	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks/contexts", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPContextsGet))
	s.route("POST /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks/contexts",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPContextsPost))
	s.route("PUT /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks/contexts",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPContextsPut))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks/contexts",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPContextsDelete))

	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_pull_request_reviews", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPReviewsGet))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_pull_request_reviews",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPReviewsPatch))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_pull_request_reviews",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPReviewsDelete))

	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPRestrictionsGet))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsDelete))

	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/users", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPRestrictionsUsersGet))
	s.route("POST /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/users",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsUsersPost))
	s.route("PUT /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/users",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsUsersPut))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/users",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsUsersDelete))

	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/teams", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPRestrictionsTeamsGet))
	s.route("POST /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/teams",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsTeamsPost))
	s.route("PUT /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/teams",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsTeamsPut))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/teams",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsTeamsDelete))

	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/enforce_admins", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPEnforceAdminsGet))
	s.route("POST /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/enforce_admins",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPEnforceAdminsPost))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/enforce_admins",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPEnforceAdminsDelete))

}

func (s *Server) branchProtectionURL(baseURL, fullName, branch string) string {
	return baseURL + "/api/v3/repos/" + fullName + "/branches/" + branch + "/protection"
}

func (s *Server) branchProtectionSubURL(baseURL, fullName, branch, sub string) string {
	return s.branchProtectionURL(baseURL, fullName, branch) + "/" + sub
}

// branchProtectionShape derives the protected flag and protection members for
// branch responses. A branch is protected when a classic protection resource
// or an applicable ruleset protects it; deriving from both keeps the branch and
// protection APIs from contradicting each other.
func (s *Server) branchProtectionShape(repo *store.Repo, branch, baseURL string) (bool, map[string]interface{}, string) {
	bp := s.branchProtectionFor(repo.ID, branch)
	protected := bp != nil || s.store.BranchProtectedByRuleset(repo, branch)
	protectionURL := s.branchProtectionURL(baseURL, repo.FullName, branch)

	if bp == nil {
		return protected, map[string]interface{}{
			"enabled": protected,
			"required_status_checks": map[string]interface{}{
				"enforcement_level": "off",
				"contexts":          []string{},
				"checks":            []interface{}{},
			},
		}, protectionURL
	}

	hydrated := s.hydrateBranchProtectionURLs(bp, repo, branch, baseURL)
	raw, err := json.Marshal(hydrated)
	if err != nil {
		return protected, map[string]interface{}{"enabled": protected}, protectionURL
	}
	var protection map[string]interface{}
	if err := json.Unmarshal(raw, &protection); err != nil {
		return protected, map[string]interface{}{"enabled": protected}, protectionURL
	}
	protection["enabled"] = protected
	return protected, protection, protectionURL
}

// branchProtectionFor reads the protection table under Misc.Mu and returns a
// copy, so callers that edit the rule or fill in request-derived URLs never
// mutate the shared stored object.
func (s *Server) branchProtectionFor(repoID int, branch string) *store.BranchProtection {
	s.store.Misc.Mu.RLock()
	defer s.store.Misc.Mu.RUnlock()
	return cloneBranchProtection(s.store.Misc.BranchProtection[store.BpKey(repoID, branch)])
}

// effectiveBranchProtectionFor is the enforcement lookup: the exact-name rule,
// else the first matching web-only pattern rule. REST protection handlers read
// branchProtectionFor directly — GitHub's classic API addresses exact names only.
func (s *Server) effectiveBranchProtectionFor(repoID int, branch string) *store.BranchProtection {
	if bp := s.branchProtectionFor(repoID, branch); bp != nil {
		return bp
	}
	for _, rule := range s.store.ListBranchProtectionPatterns(repoID) {
		if rule.Protection != nil && store.MatchBranchPattern(rule.Pattern, branch) {
			return cloneBranchProtection(rule.Protection)
		}
	}
	return nil
}

// cloneBranchProtection deep-copies a rule so no caller holds a pointer into
// the stored table. TestBranchProtectionCloneCoversEveryField fails when a new
// field is added without being copied here.
func cloneBranchProtection(bp *store.BranchProtection) *store.BranchProtection {
	if bp == nil {
		return nil
	}
	out := *bp
	if bp.RequiredStatusChecks != nil {
		checks := *bp.RequiredStatusChecks
		checks.Contexts = append([]string(nil), bp.RequiredStatusChecks.Contexts...)
		checks.Checks = make([]store.BPCheck, len(bp.RequiredStatusChecks.Checks))
		for i, check := range bp.RequiredStatusChecks.Checks {
			check.AppID = store.ClonePointer(check.AppID)
			checks.Checks[i] = check
		}
		out.RequiredStatusChecks = &checks
	}
	if bp.RequiredPullRequestReviews != nil {
		reviews := *bp.RequiredPullRequestReviews
		reviews.DismissalRestrictions = cloneBPRestrictions(bp.RequiredPullRequestReviews.DismissalRestrictions)
		if bp.RequiredPullRequestReviews.BypassPullRequestAllowances != nil {
			allowances := store.BPBypassAllowances{
				Users: append([]store.BPActor(nil), bp.RequiredPullRequestReviews.BypassPullRequestAllowances.Users...),
				Teams: append([]store.BPActor(nil), bp.RequiredPullRequestReviews.BypassPullRequestAllowances.Teams...),
				Apps:  append([]store.BPActor(nil), bp.RequiredPullRequestReviews.BypassPullRequestAllowances.Apps...),
			}
			reviews.BypassPullRequestAllowances = &allowances
		}
		out.RequiredPullRequestReviews = &reviews
	}
	out.EnforceAdmins = store.ClonePointer(bp.EnforceAdmins)
	out.Restrictions = cloneBPRestrictions(bp.Restrictions)
	out.RequiredLinearHistory = store.ClonePointer(bp.RequiredLinearHistory)
	out.AllowForcePushes = store.ClonePointer(bp.AllowForcePushes)
	out.AllowDeletions = store.ClonePointer(bp.AllowDeletions)
	out.BlockCreations = store.ClonePointer(bp.BlockCreations)
	out.RequiredConversationResolution = store.ClonePointer(bp.RequiredConversationResolution)
	out.RequiredSignatures = store.ClonePointer(bp.RequiredSignatures)
	out.LockBranch = store.ClonePointer(bp.LockBranch)
	out.AllowForkSyncing = store.ClonePointer(bp.AllowForkSyncing)
	return &out
}

func cloneBPRestrictions(r *store.BPRestrictions) *store.BPRestrictions {
	if r == nil {
		return nil
	}
	out := *r
	out.Users = append([]store.BPActor(nil), r.Users...)
	out.Teams = append([]store.BPActor(nil), r.Teams...)
	out.Apps = append([]store.BPActor(nil), r.Apps...)
	return &out
}

func (s *Server) getBranchProtection(r *http.Request) (*store.Repo, string, *store.BranchProtection) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		return nil, "", nil
	}
	branch := r.PathValue("branch")
	return repo, branch, s.branchProtectionFor(repo.ID, branch)
}

// setBranchProtection stores a copy of the supplied rule so later caller edits
// cannot reach the stored table. Uses the same store method the GraphQL
// branch-protection mutations drive, so the two surfaces cannot drift.
func (s *Server) setBranchProtection(repo *store.Repo, branch string, bp *store.BranchProtection) {
	s.store.SetBranchProtection(repo.ID, branch, cloneBranchProtection(bp))
	// A protection change can clear the condition an armed auto-merge waited on.
	s.maybeAutoMergeBranch(repo, branch)
}

func (s *Server) branchProtectionNotFound(w http.ResponseWriter) {
	writeGHError(w, http.StatusNotFound, "Branch not protected")
}

// Top-level protection

func (s *Server) handleBranchProtectionGet(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil {
		s.branchProtectionNotFound(w)
		return
	}
	bp = s.hydrateBranchProtectionURLs(bp, repo, branch, s.baseURL(r))
	writeJSON(w, http.StatusOK, bp)
}

func (s *Server) handleBranchProtectionPut(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// An enterprise policy can reserve protection changes even from repo admins.
	if s.enterpriseForbidsProtectedBranchUpdate(w, r, repo) {
		return
	}
	branch := r.PathValue("branch")

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes))
	if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	if len(fields) == 0 {
		writeGHValidationErrorSimple(w, "required_status_checks, enforce_admins, required_pull_request_reviews, and restrictions are required")
		return
	}
	var req bpRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}

	key := store.BpKey(repo.ID, branch)
	s.store.Misc.Mu.Lock()
	existed := s.store.Misc.BranchProtection[key] != nil
	bp := cloneBranchProtection(s.store.Misc.BranchProtection[key])
	if bp == nil {
		bp = &store.BranchProtection{}
	}
	bp = s.applyBranchProtectionRequest(bp, &req)
	// A PUT protects the branch; only DELETE .../protection removes it.
	bp.Enabled = true
	for name, clear := range map[string]func(){
		"required_status_checks":        func() { bp.RequiredStatusChecks = nil },
		"required_pull_request_reviews": func() { bp.RequiredPullRequestReviews = nil },
		"restrictions":                  func() { bp.Restrictions = nil },
	} {
		if value, present := fields[name]; present && string(value) == "null" {
			clear()
		}
	}
	if bp.IsProtected() {
		s.store.Misc.BranchProtection[key] = cloneBranchProtection(bp)
		if s.store.Misc.Persist != nil {
			s.store.Misc.Persist.MustPut("branch_protection", key, bp)
		}
	} else {
		delete(s.store.Misc.BranchProtection, key)
		if s.store.Misc.Persist != nil {
			s.store.Misc.Persist.MustDelete("branch_protection", key)
		}
	}
	s.store.Misc.Mu.Unlock()

	bp = s.hydrateBranchProtectionURLs(bp, repo, branch, s.baseURL(r))
	if bp.IsProtected() {
		action := "created"
		if existed {
			action = "edited"
		}
		s.emitBranchProtectionRuleEvent(repo, branch, action, r)
	}
	// A protection change can clear the condition an armed auto-merge waited on.
	s.maybeAutoMergeBranch(repo, branch)
	writeJSON(w, http.StatusOK, bp)
}

func (s *Server) emitBranchProtectionRuleEvent(repo *store.Repo, branch, action string, r *http.Request) {
	s.emitWebhookEvent(repo.FullName, "branch_protection_rule", action, map[string]interface{}{
		"action":     action,
		"rule":       map[string]interface{}{"name": branch, "repository_id": repo.ID},
		"repository": repoPayload(repo, s.baseURL(r)),
		"sender":     store.UserToJSON(ghUserFromContext(r.Context()), s.baseURL(r)),
	})
}

// enterpriseForbidsProtectedBranchUpdate writes the refusal when the enterprise
// members-can-update-protected-branches policy forbids this change.
func (s *Server) enterpriseForbidsProtectedBranchUpdate(w http.ResponseWriter, r *http.Request, repo *store.Repo) bool {
	policy, enterprise := s.enterprisePolicyForRepo(repo)
	return s.refuseByEnterprisePolicy(w, r, enterprise, policy.MembersCanUpdateProtectedBranches,
		"Updating protected branches is disabled by an enterprise policy.")
}

func (s *Server) handleBranchProtectionDelete(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if s.enterpriseForbidsProtectedBranchUpdate(w, r, repo) {
		return
	}
	branch := r.PathValue("branch")
	s.store.Misc.Mu.RLock()
	existed := s.store.Misc.BranchProtection[store.BpKey(repo.ID, branch)] != nil
	s.store.Misc.Mu.RUnlock()
	s.setBranchProtection(repo, branch, nil)
	if existed {
		s.emitBranchProtectionRuleEvent(repo, branch, "deleted", r)
	}
	w.WriteHeader(http.StatusNoContent)
}

// applyBranchProtectionRequest merges a sparse PUT body into an existing rule.
func (s *Server) applyBranchProtectionRequest(bp *store.BranchProtection, req *bpRequest) *store.BranchProtection {
	if req.RequiredStatusChecks != nil {
		bp.RequiredStatusChecks = req.RequiredStatusChecks
		// The required set may arrive as `checks` or `contexts`; `checks` wins
		// when both are present, since it can also name the reporting app.
		if req.RequiredStatusChecks.Checks != nil {
			bp.RequiredStatusChecks.SetChecks(req.RequiredStatusChecks.Checks)
		} else {
			bp.RequiredStatusChecks.SetContexts(req.RequiredStatusChecks.Contexts)
		}
	}
	if req.RequiredPullRequestReviews != nil {
		// A zero approving-review count is a valid setting, not a delete signal.
		bp.RequiredPullRequestReviews = req.RequiredPullRequestReviews
	}
	if req.EnforceAdmins != nil {
		bp.EnforceAdmins = &store.BPEnforceAdmins{Enabled: bool(*req.EnforceAdmins)}
	}
	if req.Restrictions != nil {
		bp.Restrictions = req.Restrictions
	}
	if req.RequiredLinearHistory != nil {
		bp.RequiredLinearHistory = &store.BPEnabled{Enabled: bool(*req.RequiredLinearHistory)}
	}
	if req.AllowForcePushes != nil {
		bp.AllowForcePushes = &store.BPEnabled{Enabled: bool(*req.AllowForcePushes)}
	}
	if req.AllowDeletions != nil {
		bp.AllowDeletions = &store.BPEnabled{Enabled: bool(*req.AllowDeletions)}
	}
	if req.BlockCreations != nil {
		bp.BlockCreations = &store.BPEnabled{Enabled: bool(*req.BlockCreations)}
	}
	if req.RequiredConversationResolution != nil {
		bp.RequiredConversationResolution = &store.BPEnabled{Enabled: bool(*req.RequiredConversationResolution)}
	}
	if req.RequiredSignatures != nil {
		bp.RequiredSignatures = &store.BPEnabledURL{Enabled: bool(*req.RequiredSignatures)}
	}
	if req.LockBranch != nil {
		bp.LockBranch = &store.BPEnabled{Enabled: bool(*req.LockBranch)}
	}
	if req.AllowForkSyncing != nil {
		bp.AllowForkSyncing = &store.BPEnabled{Enabled: bool(*req.AllowForkSyncing)}
	}
	return bp
}

func (s *Server) hydrateBranchProtectionURLs(bp *store.BranchProtection, repo *store.Repo, branch, baseURL string) *store.BranchProtection {
	if bp == nil {
		return nil
	}
	// Enabled is stored state, not a rendered member; callers that publish one
	// derive it themselves. This runs on a detached copy.
	bp.Enabled = false
	bp.URL = s.branchProtectionURL(baseURL, repo.FullName, branch)
	if bp.RequiredStatusChecks != nil {
		bp.RequiredStatusChecks.URL = s.branchProtectionSubURL(baseURL, repo.FullName, branch, "required_status_checks")
		bp.RequiredStatusChecks.ContextsURL = s.branchProtectionSubURL(baseURL, repo.FullName, branch, "required_status_checks/contexts")
		if bp.RequiredStatusChecks.Contexts == nil {
			bp.RequiredStatusChecks.Contexts = []string{}
		}
		if bp.RequiredStatusChecks.Checks == nil {
			bp.RequiredStatusChecks.Checks = []store.BPCheck{}
		}
	}
	if bp.RequiredPullRequestReviews != nil {
		bp.RequiredPullRequestReviews.URL = s.branchProtectionSubURL(baseURL, repo.FullName, branch, "required_pull_request_reviews")
		if bp.RequiredPullRequestReviews.DismissalRestrictions != nil {
			s.hydrateRestrictionsURLs(bp.RequiredPullRequestReviews.DismissalRestrictions, baseURL, repo.FullName, branch, "dismissal_restrictions")
		}
	}
	if bp.EnforceAdmins != nil {
		bp.EnforceAdmins.URL = s.branchProtectionSubURL(baseURL, repo.FullName, branch, "enforce_admins")
	}
	if bp.Restrictions != nil {
		s.hydrateRestrictionsURLs(bp.Restrictions, baseURL, repo.FullName, branch, "restrictions")
	}
	if bp.RequiredSignatures != nil {
		bp.RequiredSignatures.URL = s.branchProtectionSubURL(baseURL, repo.FullName, branch, "required_signatures")
	}
	return bp
}

func (s *Server) hydrateRestrictionsURLs(r *store.BPRestrictions, baseURL, fullName, branch, sub string) {
	r.URL = s.branchProtectionSubURL(baseURL, fullName, branch, sub)
	r.UsersURL = s.branchProtectionSubURL(baseURL, fullName, branch, sub+"/users")
	r.TeamsURL = s.branchProtectionSubURL(baseURL, fullName, branch, sub+"/teams")
	r.AppsURL = s.branchProtectionSubURL(baseURL, fullName, branch, sub+"/apps")
	if r.Users == nil {
		r.Users = []store.BPActor{}
	}
	if r.Teams == nil {
		r.Teams = []store.BPActor{}
	}
	if r.Apps == nil {
		r.Apps = []store.BPActor{}
	}
}

// Required status checks

func (s *Server) handleBPStatusChecksGet(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.RequiredStatusChecks == nil {
		s.branchProtectionNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, s.statusCheckPolicyJSON(bp.RequiredStatusChecks, repo, branch, s.baseURL(r)))
}

// statusCheckPolicyJSON renders required_status_checks; contexts and checks are
// present even when empty.
func (s *Server) statusCheckPolicyJSON(sc *store.BPStatusChecks, repo *store.Repo, branch, baseURL string) map[string]interface{} {
	contexts := sc.Contexts
	if contexts == nil {
		contexts = []string{}
	}
	checks := make([]map[string]interface{}, 0, len(sc.Checks))
	for _, c := range sc.Checks {
		var appID interface{}
		if c.AppID != nil {
			appID = *c.AppID
		}
		checks = append(checks, map[string]interface{}{"context": c.Context, "app_id": appID})
	}
	return map[string]interface{}{
		"url":          s.branchProtectionSubURL(baseURL, repo.FullName, branch, "required_status_checks"),
		"strict":       sc.Strict,
		"contexts":     contexts,
		"checks":       checks,
		"contexts_url": s.branchProtectionSubURL(baseURL, repo.FullName, branch, "required_status_checks/contexts"),
	}
}

// handleBPStatusChecksPatch merges a partial update into an existing
// required_status_checks rule.
func (s *Server) handleBPStatusChecksPatch(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.RequiredStatusChecks == nil {
		s.branchProtectionNotFound(w)
		return
	}
	var req struct {
		Strict   *bool            `json:"strict"`
		Contexts *[]string        `json:"contexts"`
		Checks   *[]store.BPCheck `json:"checks"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Strict != nil {
		bp.RequiredStatusChecks.Strict = *req.Strict
	}
	if req.Checks != nil {
		bp.RequiredStatusChecks.SetChecks(*req.Checks)
	} else if req.Contexts != nil {
		bp.RequiredStatusChecks.SetContexts(*req.Contexts)
	}
	s.setBranchProtection(repo, branch, bp)
	writeJSON(w, http.StatusOK, s.statusCheckPolicyJSON(bp.RequiredStatusChecks, repo, branch, s.baseURL(r)))
}

func (s *Server) handleBPStatusChecksDelete(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil {
		s.branchProtectionNotFound(w)
		return
	}
	bp.RequiredStatusChecks = nil
	s.setBranchProtection(repo, branch, bp)
	w.WriteHeader(http.StatusNoContent)
}

// Contexts

func (s *Server) handleBPContextsGet(w http.ResponseWriter, r *http.Request) {
	repo, _, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.RequiredStatusChecks == nil {
		s.branchProtectionNotFound(w)
		return
	}
	contexts := bp.RequiredStatusChecks.Contexts
	if contexts == nil {
		contexts = []string{}
	}
	writeJSON(w, http.StatusOK, contexts)
}

func (s *Server) handleBPContextsPost(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.RequiredStatusChecks == nil {
		s.branchProtectionNotFound(w)
		return
	}
	var contexts []string
	if !decodeStringArrayBody(w, r, &contexts) {
		return
	}
	merged := append([]string(nil), bp.RequiredStatusChecks.Contexts...)
	for _, context := range contexts {
		if !stringSliceContains(merged, context) {
			merged = append(merged, context)
		}
	}
	bp.RequiredStatusChecks.SetContexts(merged)
	s.setBranchProtection(repo, branch, bp)
	writeJSON(w, http.StatusOK, bp.RequiredStatusChecks.Contexts)
}

func (s *Server) handleBPContextsPut(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.RequiredStatusChecks == nil {
		s.branchProtectionNotFound(w)
		return
	}
	var contexts []string
	if !decodeStringArrayBody(w, r, &contexts) {
		return
	}
	bp.RequiredStatusChecks.SetContexts(contexts)
	s.setBranchProtection(repo, branch, bp)
	writeJSON(w, http.StatusOK, bp.RequiredStatusChecks.Contexts)
}

func (s *Server) handleBPContextsDelete(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.RequiredStatusChecks == nil {
		s.branchProtectionNotFound(w)
		return
	}
	var contexts []string
	if !decodeStringArrayBody(w, r, &contexts) {
		return
	}
	bp.RequiredStatusChecks.SetContexts(removeStrings(bp.RequiredStatusChecks.Contexts, contexts))
	s.setBranchProtection(repo, branch, bp)
	writeJSON(w, http.StatusOK, bp.RequiredStatusChecks.Contexts)
}

func stringSliceContains(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func removeStrings(values, removed []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !stringSliceContains(removed, value) {
			out = append(out, value)
		}
	}
	return out
}

// Required pull request reviews

func (s *Server) handleBPReviewsGet(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.RequiredPullRequestReviews == nil {
		s.branchProtectionNotFound(w)
		return
	}
	rev := *bp.RequiredPullRequestReviews
	rev.URL = s.branchProtectionSubURL(s.baseURL(r), repo.FullName, branch, "required_pull_request_reviews")
	writeJSON(w, http.StatusOK, rev)
}

func (s *Server) handleBPReviewsPatch(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil {
		s.branchProtectionNotFound(w)
		return
	}
	if bp.RequiredPullRequestReviews == nil {
		bp.RequiredPullRequestReviews = &store.BPPullRequestReviews{}
	}
	// Pointer fields distinguish "absent" from a zero value so the PATCH merges
	// into the existing rule instead of resetting every unspecified field — GitHub
	// leaves omitted fields untouched, and a zero approving-review count is a valid
	// setting, not a signal to drop review protection (that is the DELETE handler).
	var req struct {
		DismissStaleReviews          *bool                     `json:"dismiss_stale_reviews"`
		RequireCodeOwnerReviews      *bool                     `json:"require_code_owner_reviews"`
		RequireLastPushApproval      *bool                     `json:"require_last_push_approval"`
		RequiredApprovingReviewCount *int                      `json:"required_approving_review_count"`
		DismissalRestrictions        *store.BPRestrictions     `json:"dismissal_restrictions"`
		BypassPullRequestAllowances  *store.BPBypassAllowances `json:"bypass_pull_request_allowances"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	rev := bp.RequiredPullRequestReviews
	if req.DismissStaleReviews != nil {
		rev.DismissStaleReviews = *req.DismissStaleReviews
	}
	if req.RequireCodeOwnerReviews != nil {
		rev.RequireCodeOwnerReviews = *req.RequireCodeOwnerReviews
	}
	if req.RequireLastPushApproval != nil {
		rev.RequireLastPushApproval = *req.RequireLastPushApproval
	}
	if req.RequiredApprovingReviewCount != nil {
		rev.RequiredApprovingReviewCount = *req.RequiredApprovingReviewCount
	}
	if req.DismissalRestrictions != nil {
		rev.DismissalRestrictions = req.DismissalRestrictions
	}
	if req.BypassPullRequestAllowances != nil {
		rev.BypassPullRequestAllowances = req.BypassPullRequestAllowances
	}
	s.setBranchProtection(repo, branch, bp)
	rev.URL = s.branchProtectionSubURL(s.baseURL(r), repo.FullName, branch, "required_pull_request_reviews")
	writeJSON(w, http.StatusOK, rev)
}

func (s *Server) handleBPReviewsDelete(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil {
		s.branchProtectionNotFound(w)
		return
	}
	bp.RequiredPullRequestReviews = nil
	s.setBranchProtection(repo, branch, bp)
	w.WriteHeader(http.StatusNoContent)
}

// Restrictions

func (s *Server) handleBPRestrictionsGet(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.Restrictions == nil {
		s.branchProtectionNotFound(w)
		return
	}
	res := *bp.Restrictions
	s.hydrateRestrictionsURLs(&res, s.baseURL(r), repo.FullName, branch, "restrictions")
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleBPRestrictionsDelete(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil {
		s.branchProtectionNotFound(w)
		return
	}
	bp.Restrictions = nil
	s.setBranchProtection(repo, branch, bp)
	w.WriteHeader(http.StatusNoContent)
}

// Restrictions users

func (s *Server) handleBPRestrictionsUsersGet(w http.ResponseWriter, r *http.Request) {
	repo, _, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.Restrictions == nil {
		s.branchProtectionNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, s.bpRestrictedUsersJSON(bp.Restrictions.Users, s.baseURL(r)))
}

func (s *Server) handleBPRestrictionsUsersPost(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.Restrictions == nil {
		s.branchProtectionNotFound(w)
		return
	}
	users, ok := s.decodeBPUserLogins(w, r)
	if !ok {
		return
	}
	for _, user := range users {
		if !bpActorSliceContains(bp.Restrictions.Users, user.ID) {
			bp.Restrictions.Users = append(bp.Restrictions.Users, user)
		}
	}
	s.setBranchProtection(repo, branch, bp)
	writeJSON(w, http.StatusOK, s.bpRestrictedUsersJSON(bp.Restrictions.Users, s.baseURL(r)))
}

func (s *Server) handleBPRestrictionsUsersPut(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.Restrictions == nil {
		s.branchProtectionNotFound(w)
		return
	}
	users, ok := s.decodeBPUserLogins(w, r)
	if !ok {
		return
	}
	bp.Restrictions.Users = users
	s.setBranchProtection(repo, branch, bp)
	writeJSON(w, http.StatusOK, s.bpRestrictedUsersJSON(users, s.baseURL(r)))
}

func (s *Server) handleBPRestrictionsUsersDelete(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.Restrictions == nil {
		s.branchProtectionNotFound(w)
		return
	}
	users, ok := s.decodeBPUserLogins(w, r)
	if !ok {
		return
	}
	bp.Restrictions.Users = removeBPActors(bp.Restrictions.Users, users)
	s.setBranchProtection(repo, branch, bp)
	writeJSON(w, http.StatusOK, s.bpRestrictedUsersJSON(bp.Restrictions.Users, s.baseURL(r)))
}

// Restrictions teams

func (s *Server) handleBPRestrictionsTeamsGet(w http.ResponseWriter, r *http.Request) {
	repo, _, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.Restrictions == nil {
		s.branchProtectionNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, s.bpRestrictedTeamsJSON(repo, bp.Restrictions.Teams, s.baseURL(r)))
}

func (s *Server) handleBPRestrictionsTeamsPost(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.Restrictions == nil {
		s.branchProtectionNotFound(w)
		return
	}
	teams, ok := s.decodeBPTeamSlugs(w, r, repo)
	if !ok {
		return
	}
	for _, team := range teams {
		if !bpActorSliceContains(bp.Restrictions.Teams, team.ID) {
			bp.Restrictions.Teams = append(bp.Restrictions.Teams, team)
		}
	}
	s.setBranchProtection(repo, branch, bp)
	writeJSON(w, http.StatusOK, s.bpRestrictedTeamsJSON(repo, bp.Restrictions.Teams, s.baseURL(r)))
}

func (s *Server) handleBPRestrictionsTeamsPut(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.Restrictions == nil {
		s.branchProtectionNotFound(w)
		return
	}
	teams, ok := s.decodeBPTeamSlugs(w, r, repo)
	if !ok {
		return
	}
	bp.Restrictions.Teams = teams
	s.setBranchProtection(repo, branch, bp)
	writeJSON(w, http.StatusOK, s.bpRestrictedTeamsJSON(repo, teams, s.baseURL(r)))
}

func (s *Server) handleBPRestrictionsTeamsDelete(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.Restrictions == nil {
		s.branchProtectionNotFound(w)
		return
	}
	teams, ok := s.decodeBPTeamSlugs(w, r, repo)
	if !ok {
		return
	}
	bp.Restrictions.Teams = removeBPActors(bp.Restrictions.Teams, teams)
	s.setBranchProtection(repo, branch, bp)
	writeJSON(w, http.StatusOK, s.bpRestrictedTeamsJSON(repo, bp.Restrictions.Teams, s.baseURL(r)))
}

func bpActorSliceContains(actors []store.BPActor, id int) bool {
	for _, actor := range actors {
		if actor.ID == id {
			return true
		}
	}
	return false
}

func removeBPActors(current, removed []store.BPActor) []store.BPActor {
	out := make([]store.BPActor, 0, len(current))
	for _, actor := range current {
		if !bpActorSliceContains(removed, actor.ID) {
			out = append(out, actor)
		}
	}
	return out
}

func (s *Server) decodeBPUserLogins(w http.ResponseWriter, r *http.Request) ([]store.BPActor, bool) {
	var req struct {
		Users *[]string `json:"users"`
	}
	if !decodeJSONBody(w, r, &req) {
		return nil, false
	}
	if req.Users == nil {
		store.WriteGHValidationError(w, "BranchRestriction", "users", "missing_field")
		return nil, false
	}
	actors := make([]store.BPActor, 0, len(*req.Users))
	for _, login := range *req.Users {
		s.store.Mu.RLock()
		user := s.store.UsersByLogin[login]
		s.store.Mu.RUnlock()
		if user == nil {
			writeGHError(w, http.StatusUnprocessableEntity, "Could not resolve to a user: "+login)
			return nil, false
		}
		actors = append(actors, store.BPActor{Login: user.Login, ID: user.ID, Type: "User"})
	}
	return actors, true
}

func (s *Server) bpRestrictedUsersJSON(actors []store.BPActor, baseURL string) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(actors))
	for _, actor := range actors {
		s.store.Mu.RLock()
		user := s.store.Users[actor.ID]
		var rendered map[string]interface{}
		if user != nil {
			rendered = store.UserToJSON(user, baseURL)
		}
		s.store.Mu.RUnlock()
		if rendered != nil {
			out = append(out, rendered)
		}
	}
	return out
}

func (s *Server) decodeBPTeamSlugs(w http.ResponseWriter, r *http.Request, repo *store.Repo) ([]store.BPActor, bool) {
	var req struct {
		Teams *[]string `json:"teams"`
	}
	if !decodeJSONBody(w, r, &req) {
		return nil, false
	}
	if req.Teams == nil {
		store.WriteGHValidationError(w, "BranchRestriction", "teams", "missing_field")
		return nil, false
	}
	orgLogin := ownerFromRepoFullName(repo.FullName)
	if s.store.GetOrg(orgLogin) == nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Team restrictions require an organization repository")
		return nil, false
	}
	actors := make([]store.BPActor, 0, len(*req.Teams))
	for _, slug := range *req.Teams {
		team := s.store.GetTeam(orgLogin, slug)
		if team == nil {
			writeGHError(w, http.StatusUnprocessableEntity, "Could not resolve to a team: "+slug)
			return nil, false
		}
		actors = append(actors, store.BPActor{Login: team.Slug, ID: team.ID, Type: "Team"})
	}
	return actors, true
}

func (s *Server) bpRestrictedTeamsJSON(repo *store.Repo, actors []store.BPActor, baseURL string) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(actors))
	orgLogin := ownerFromRepoFullName(repo.FullName)
	org := s.store.GetOrg(orgLogin)
	if org == nil {
		return out
	}
	for _, actor := range actors {
		if team := s.store.GetTeam(orgLogin, actor.Login); team != nil {
			out = append(out, teamSimpleJSON(team, org, s.store, baseURL))
		}
	}
	return out
}

// Enforce admins

func (s *Server) handleBPEnforceAdminsGet(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil {
		s.branchProtectionNotFound(w)
		return
	}
	ea := store.BPEnforceAdmins{}
	if bp.EnforceAdmins != nil {
		ea = *bp.EnforceAdmins
	}
	ea.URL = s.branchProtectionSubURL(s.baseURL(r), repo.FullName, branch, "enforce_admins")
	writeJSON(w, http.StatusOK, ea)
}

func (s *Server) handleBPEnforceAdminsPost(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil {
		s.branchProtectionNotFound(w)
		return
	}
	bp.EnforceAdmins = &store.BPEnforceAdmins{Enabled: true}
	s.setBranchProtection(repo, branch, bp)
	resp := *bp.EnforceAdmins
	resp.URL = s.branchProtectionSubURL(s.baseURL(r), repo.FullName, branch, "enforce_admins")
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleBPEnforceAdminsDelete(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil {
		s.branchProtectionNotFound(w)
		return
	}
	// This endpoint disables admin enforcement rather than clearing the member:
	// clearing it would 404 the GET and, if it was the last rule, drop the rest.
	bp.EnforceAdmins = &store.BPEnforceAdmins{Enabled: false}
	s.setBranchProtection(repo, branch, bp)
	w.WriteHeader(http.StatusNoContent)
}

// Required commit signatures

func (s *Server) requiredSignaturesJSON(bp *store.BranchProtection, repo *store.Repo, branch, baseURL string) map[string]interface{} {
	enabled := bp.RequiredSignatures != nil && bp.RequiredSignatures.Enabled
	return map[string]interface{}{
		"url":     s.branchProtectionSubURL(baseURL, repo.FullName, branch, "required_signatures"),
		"enabled": enabled,
	}
}

func (s *Server) handleBPRequiredSignaturesGet(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil {
		s.branchProtectionNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, s.requiredSignaturesJSON(bp, repo, branch, s.baseURL(r)))
}

func (s *Server) handleBPRequiredSignaturesPost(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil {
		s.branchProtectionNotFound(w)
		return
	}
	bp.RequiredSignatures = &store.BPEnabledURL{Enabled: true}
	s.setBranchProtection(repo, branch, bp)
	writeJSON(w, http.StatusOK, s.requiredSignaturesJSON(bp, repo, branch, s.baseURL(r)))
}

func (s *Server) handleBPRequiredSignaturesDelete(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil {
		s.branchProtectionNotFound(w)
		return
	}
	bp.RequiredSignatures = nil
	s.setBranchProtection(repo, branch, bp)
	w.WriteHeader(http.StatusNoContent)
}

// Restrictions apps

// bpRestrictedAppsJSON renders app actors as full GitHub App objects.
func (s *Server) bpRestrictedAppsJSON(actors []store.BPActor) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(actors))
	for _, actor := range actors {
		s.store.Mu.RLock()
		app := s.store.AppsBySlug[actor.Login]
		s.store.Mu.RUnlock()
		if app != nil {
			out = append(out, appToJSON(s.store, app, false, s.publicOrigin()))
		}
	}
	return out
}

// decodeBPAppSlugs decodes {"apps":[...]} and resolves each slug to a
// registered GitHub App, writing a 422 when one does not resolve.
func (s *Server) decodeBPAppSlugs(w http.ResponseWriter, r *http.Request) ([]store.BPActor, bool) {
	var req struct {
		Apps []string `json:"apps"`
	}
	if !decodeJSONBody(w, r, &req) {
		return nil, false
	}
	actors := make([]store.BPActor, 0, len(req.Apps))
	for _, slug := range req.Apps {
		s.store.Mu.RLock()
		app := s.store.AppsBySlug[slug]
		s.store.Mu.RUnlock()
		if app == nil {
			writeGHError(w, http.StatusUnprocessableEntity, "Could not resolve to a GitHub App: "+slug)
			return nil, false
		}
		actors = append(actors, store.BPActor{Login: app.Slug, ID: app.ID, Type: "App"})
	}
	return actors, true
}

func (s *Server) handleBPRestrictionsAppsGet(w http.ResponseWriter, r *http.Request) {
	repo, _, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.Restrictions == nil {
		s.branchProtectionNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, s.bpRestrictedAppsJSON(bp.Restrictions.Apps))
}

func (s *Server) handleBPRestrictionsAppsPost(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.Restrictions == nil {
		s.branchProtectionNotFound(w)
		return
	}
	actors, ok := s.decodeBPAppSlugs(w, r)
	if !ok {
		return
	}
	for _, actor := range actors {
		exists := false
		for _, cur := range bp.Restrictions.Apps {
			if cur.ID == actor.ID {
				exists = true
				break
			}
		}
		if !exists {
			bp.Restrictions.Apps = append(bp.Restrictions.Apps, actor)
		}
	}
	s.setBranchProtection(repo, branch, bp)
	writeJSON(w, http.StatusOK, s.bpRestrictedAppsJSON(bp.Restrictions.Apps))
}

func (s *Server) handleBPRestrictionsAppsPut(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.Restrictions == nil {
		s.branchProtectionNotFound(w)
		return
	}
	actors, ok := s.decodeBPAppSlugs(w, r)
	if !ok {
		return
	}
	bp.Restrictions.Apps = actors
	s.setBranchProtection(repo, branch, bp)
	writeJSON(w, http.StatusOK, s.bpRestrictedAppsJSON(bp.Restrictions.Apps))
}

func (s *Server) handleBPRestrictionsAppsDelete(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.Restrictions == nil {
		s.branchProtectionNotFound(w)
		return
	}
	actors, ok := s.decodeBPAppSlugs(w, r)
	if !ok {
		return
	}
	remaining := bp.Restrictions.Apps[:0]
	for _, cur := range bp.Restrictions.Apps {
		removed := false
		for _, actor := range actors {
			if cur.ID == actor.ID {
				removed = true
				break
			}
		}
		if !removed {
			remaining = append(remaining, cur)
		}
	}
	bp.Restrictions.Apps = remaining
	s.setBranchProtection(repo, branch, bp)
	writeJSON(w, http.StatusOK, s.bpRestrictedAppsJSON(bp.Restrictions.Apps))
}

// Helpers

// decodeStringArrayBody decodes a bare JSON array or {"contexts":[...]}.
func decodeStringArrayBody(w http.ResponseWriter, r *http.Request, out *[]string) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) == 0 {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return false
	}
	if trimmed[0] == '[' {
		if err := json.Unmarshal([]byte(trimmed), out); err != nil {
			writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
			return false
		}
		return true
	}
	var obj struct {
		Contexts []string `json:"contexts"`
	}
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return false
	}
	*out = obj.Contexts
	return true
}

// refWriteKind names the ref updates a protection rule governs with separate
// allowances.
type refWriteKind int

const (
	refForcePush refWriteKind = iota
	refDeletion
	refCreation
	// refFastForward: the target descends from the branch's current tip, so no
	// reachable commit is discarded.
	refFastForward
)

// protectedRefWriteRefusal decides one ref write against the branch's
// protection rule, returning the refusal or "" when allowed. Both git
// transports and the REST ref-write routes decide here, so a rule cannot bind
// one lane and be decoration on the other. Destructive allowances (force push,
// deletion, creation) are checked alongside the requirements a merge faces.
func (s *Server) protectedRefWriteRefusal(ctx context.Context, repo *store.Repo, stor storer.Storer, ref plumbing.ReferenceName, kind refWriteKind, target plumbing.Hash) string {
	if !ref.IsBranch() {
		return s.evaluateRulesetsForRefWrite(ctx, repo, stor, ref, kind, target)
	}
	rulesetRefusal := s.evaluateRulesetsForRefWrite(ctx, repo, stor, ref, kind, target)
	branch := ref.Short()
	bp := s.effectiveBranchProtectionFor(repo.ID, branch)
	if bp == nil {
		return rulesetRefusal
	}
	// lock_branch makes the branch read-only for everyone, admins included, and
	// is decided before the enforce_admins bypass to match the merge gate.
	if bp.LockBranch != nil && bp.LockBranch.Enabled {
		return "Cannot update this locked branch: " + branch + " is read-only."
	}
	// With enforce_admins off, an admin bypasses the whole rule.
	if (bp.EnforceAdmins == nil || !bp.EnforceAdmins.Enabled) && s.viewerCanAdminRepo(ctx, repo) {
		return rulesetRefusal
	}
	if bp.Restrictions != nil && !s.viewerIsRestrictedPusher(ctx, repo, bp.Restrictions) {
		return "You're not authorized to push to this branch."
	}
	switch kind {
	case refDeletion:
		if bp.AllowDeletions == nil || !bp.AllowDeletions.Enabled {
			return "Cannot delete protected branch " + branch + "."
		}
		// A deletion has no target, so target requirements do not apply.
		return rulesetRefusal
	case refForcePush:
		if bp.AllowForcePushes == nil || !bp.AllowForcePushes.Enabled {
			return "Cannot force-push to protected branch " + branch + "."
		}
	case refCreation:
		if bp.BlockCreations != nil && bp.BlockCreations.Enabled {
			return "Cannot create protected branch " + branch + "."
		}
	case refFastForward:
	}
	if refusal := s.protectedBranchTargetRefusal(repo, bp, stor, branch, target); refusal != "" {
		return refusal
	}
	return rulesetRefusal
}

// protectedBranchTargetRefusal enforces the requirements on what a protected
// branch may be moved to; a direct write faces them as a merge does.
func (s *Server) protectedBranchTargetRefusal(repo *store.Repo, bp *store.BranchProtection, stor storer.Storer, branch string, target plumbing.Hash) string {
	if bp.RequiredPullRequestReviews != nil {
		return "Changes must be made through a pull request."
	}
	if target.IsZero() {
		return ""
	}
	if state := s.evaluateChecksForMerge(repo, branch, target.String()); len(state.MissingRequired) > 0 {
		return fmt.Sprintf("Required status check %q is expected.", state.MissingRequired[0])
	}
	linear := bp.RequiredLinearHistory != nil && bp.RequiredLinearHistory.Enabled
	signed := bp.RequiredSignatures != nil && bp.RequiredSignatures.Enabled
	if !linear && !signed {
		return ""
	}
	added, err := commitsAddedToBranch(stor, branch, target)
	if err != nil {
		return "Cannot read the commits this update adds to " + branch + ": " + err.Error()
	}
	for _, commit := range added {
		if linear && commit.NumParents() > 1 {
			return "Cannot push a merge commit to " + branch + ", which requires linear history."
		}
		if signed && commit.PGPSignature == "" {
			return "Commits must have verified signatures."
		}
	}
	return ""
}

// commitsAddedToBranch returns the commits reachable from target but not from
// the branch's current tip.
func commitsAddedToBranch(stor storer.Storer, branch string, target plumbing.Hash) ([]*object.Commit, error) {
	if stor == nil {
		return nil, errors.New("no git storage for the repository")
	}
	head, err := object.GetCommit(stor, target)
	if err != nil {
		return nil, fmt.Errorf("read commit %s: %w", target, err)
	}
	seen := map[plumbing.Hash]bool{}
	if current, err := stor.Reference(plumbing.NewBranchReferenceName(branch)); err == nil {
		if base, err := object.GetCommit(stor, current.Hash()); err == nil {
			if err := object.NewCommitPreorderIter(base, nil, nil).ForEach(func(c *object.Commit) error {
				seen[c.Hash] = true
				return nil
			}); err != nil {
				return nil, fmt.Errorf("walk %s: %w", branch, err)
			}
		}
	}
	var added []*object.Commit
	err = object.NewCommitPreorderIter(head, seen, nil).ForEach(func(c *object.Commit) error {
		added = append(added, c)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", target, err)
	}
	return added, nil
}

// viewerIsRestrictedPusher reports whether the actor is allowed to write the
// branch under a restrictions rule.
func (s *Server) viewerIsRestrictedPusher(ctx context.Context, repo *store.Repo, restrictions *store.BPRestrictions) bool {
	user := ghUserFromContext(ctx)
	if user == nil {
		return false
	}
	for _, actor := range restrictions.Users {
		if strings.EqualFold(actor.Login, user.Login) || (actor.ID != 0 && actor.ID == user.ID) {
			return true
		}
	}
	orgLogin, _, ok := store.SplitRepoFullName(repo.FullName)
	if ok {
		for _, actor := range restrictions.Teams {
			if _, member := s.store.GetTeamMembership(orgLogin, actor.Login, user.ID); member {
				return true
			}
		}
	}
	app := ghAppFromContext(ctx)
	if app == nil {
		if token := ghInstallationTokenFromContext(ctx); token != nil {
			app = s.store.GetApp(token.AppID)
		}
	}
	if app != nil {
		for _, actor := range restrictions.Apps {
			if app.Slug == actor.Login || app.ID == actor.ID {
				return true
			}
		}
	}
	return false
}

// canMergePullRequest checks branch protection for a PR merge. ok==false with
// an empty message means the caller falls back to its required-status-check message.
// pullRequestBehindBase reports whether the PR's base-branch tip is NOT an
// ancestor of the PR head — the head has not incorporated the latest base.
// required_status_checks.strict ("require branches to be up to date before
// merging") forbids merging in that state, because the required checks never ran
// against the merged result. A git error or an unresolvable ref fails open (the
// required-checks and review gates still apply), never blocking a legitimate merge.
func (s *Server) pullRequestBehindBase(repo *store.Repo, pr *store.PullRequest) bool {
	stor, _ := store.PullRequestGitStorage(s.store, repo, pr)
	if stor == nil {
		return false
	}
	baseTip := store.ResolveBranchSha(stor, pr.BaseRefName)
	head := store.ResolveBranchSha(stor, pr.HeadRefName)
	if baseTip == "" || head == "" || baseTip == head {
		return false
	}
	upToDate, err := refUpdateIsFastForward(stor, plumbing.NewHash(baseTip), plumbing.NewHash(head))
	if err != nil {
		return false
	}
	return !upToDate
}

// strictUpToDateRequired reports whether the base branch requires the PR to be
// up to date (required_status_checks.strict) and the PR is currently behind.
func (s *Server) strictUpToDateRequired(bp *store.BranchProtection, repo *store.Repo, pr *store.PullRequest) bool {
	return bp != nil && bp.RequiredStatusChecks != nil && bp.RequiredStatusChecks.Strict &&
		s.pullRequestBehindBase(repo, pr)
}

func (s *Server) canMergePullRequest(ctx context.Context, repo *store.Repo, pr *store.PullRequest) (bool, string) {
	// A draft PR is never mergeable, regardless of branch protection (GitHub
	// refuses it on every merge path). Kept here so REST, GraphQL, auto-merge and
	// the merge queue all inherit the guard.
	if pr.IsDraft {
		return false, "Draft pull requests cannot be merged."
	}
	bp := s.effectiveBranchProtectionFor(repo.ID, pr.BaseRefName)
	if bp == nil {
		return true, ""
	}

	isAdmin := s.viewerCanAdminRepo(ctx, repo)

	// lock_branch is read-only for everyone (no enforce_admins carve-out) and is
	// refused before any bypass applies.
	if bp.LockBranch != nil && bp.LockBranch.Enabled {
		return false, "Cannot merge into locked branch " + pr.BaseRefName + "."
	}

	if isAdmin && (bp.EnforceAdmins == nil || !bp.EnforceAdmins.Enabled) {
		return true, ""
	}

	headSha := s.prHeadSha(repo, pr)
	if headSha != "" {
		st := s.evaluateChecksForMerge(repo, pr.BaseRefName, headSha)
		if len(st.MissingRequired) > 0 {
			return false, fmt.Sprintf("Required status check %q is expected.", st.MissingRequired[0])
		}
	}

	// required_status_checks.strict: the branch must be up to date with its base
	// before merging, so the required checks are validated against the merged result.
	if s.strictUpToDateRequired(bp, repo, pr) {
		return false, "Required status checks require the branch to be up to date with the base branch before merging."
	}

	if ok, msg := s.pullRequestReviewGatesSatisfied(repo, pr, bp); !ok {
		return false, msg
	}

	return true, ""
}

// pullRequestReviewGatesSatisfied enforces the branch-protection requirements
// that do NOT depend on the branch being up to date or on status checks:
// required approving reviews (never counting the author's own approval),
// code-owner reviews, last-push approval, and changes-requested. bp must be
// non-nil and the admin bypass already decided against. Shared by
// canMergePullRequest and mergeQueueEligible so the merge queue enforces the
// same review requirements as a direct merge.
func (s *Server) pullRequestReviewGatesSatisfied(repo *store.Repo, pr *store.PullRequest, bp *store.BranchProtection) (bool, string) {
	if bp.RequiredPullRequestReviews != nil && bp.RequiredPullRequestReviews.RequiredApprovingReviewCount > 0 {
		count := s.countApprovingReviews(pr.ID, pr.AuthorID)
		if count < bp.RequiredPullRequestReviews.RequiredApprovingReviewCount {
			return false, fmt.Sprintf("At least %d approving review is required by the branch protection rules.", bp.RequiredPullRequestReviews.RequiredApprovingReviewCount)
		}
	}

	// require_code_owner_reviews: every code owner of a changed file must have approved.
	if bp.RequiredPullRequestReviews != nil && bp.RequiredPullRequestReviews.RequireCodeOwnerReviews {
		if !s.codeOwnerApprovalsSatisfied(repo, pr) {
			return false, "Review by a code owner of the changed files is required by the branch protection rules."
		}
	}

	// require_last_push_approval: the most recent push must be approved by someone other than its pusher.
	if bp.RequiredPullRequestReviews != nil && bp.RequiredPullRequestReviews.RequireLastPushApproval {
		if !s.lastPushApproved(repo, pr) {
			return false, "The most recent push must be approved by someone other than the person who pushed it."
		}
	}

	if s.hasRequestedChanges(pr.ID) {
		return false, "Changes have been requested on this pull request."
	}

	return true, ""
}

// mergeQueueEligible reports whether a PR may sit in (and be merged from) the
// merge queue: it must not be a draft and must satisfy the review-family
// branch-protection requirements. It deliberately omits the up-to-date and
// status-check requirements — the queue itself brings the branch up to date and
// evaluates checks on the formed merge group. GitHub refuses to enqueue a PR
// that fails these, so enqueue rejects and the processor skips.
func (s *Server) mergeQueueEligible(ctx context.Context, repo *store.Repo, pr *store.PullRequest) (bool, string) {
	if pr.IsDraft {
		return false, "Draft pull requests cannot be merged."
	}
	bp := s.effectiveBranchProtectionFor(repo.ID, pr.BaseRefName)
	if bp == nil {
		return true, ""
	}
	if bp.LockBranch != nil && bp.LockBranch.Enabled {
		return false, "Cannot merge into locked branch " + pr.BaseRefName + "."
	}
	if s.viewerCanAdminRepo(ctx, repo) && (bp.EnforceAdmins == nil || !bp.EnforceAdmins.Enabled) {
		return true, ""
	}
	return s.pullRequestReviewGatesSatisfied(repo, pr, bp)
}

// lastPushApproved reports whether someone other than the head's last pusher
// holds a current APPROVED review. When the pusher cannot be resolved the PR
// author stands in, so the author cannot self-satisfy the gate.
func (s *Server) lastPushApproved(repo *store.Repo, pr *store.PullRequest) bool {
	pusherID := s.lastHeadPusherID(repo, pr)
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for userID, state := range s.latestGateReviewStatesLocked(pr.ID) {
		if state == "APPROVED" && userID != pusherID {
			return true
		}
	}
	return false
}

// lastHeadPusherID resolves the user behind the PR's current head commit,
// falling back to the PR author.
func (s *Server) lastHeadPusherID(repo *store.Repo, pr *store.PullRequest) int {
	headStor, _ := store.PullRequestGitStorage(s.store, repo, pr)
	if headStor != nil {
		if hash, err := store.ResolveGitRef(headStor, pr.HeadRefName); err == nil {
			if commit, err := object.GetCommit(headStor, hash); err == nil {
				if id := s.userIDForCommitIdentity(commit.Committer.Name, commit.Committer.Email); id != 0 {
					return id
				}
				if id := s.userIDForCommitIdentity(commit.Author.Name, commit.Author.Email); id != 0 {
					return id
				}
			}
		}
	}
	return pr.AuthorID
}

// userIDForCommitIdentity maps a git signature to a user: exact email match,
// then the email's local part as a login, then the name as a login.
func (s *Server) userIDForCommitIdentity(name, email string) int {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	if email != "" {
		for _, u := range s.store.Users {
			if u.Email != "" && strings.EqualFold(u.Email, email) {
				return u.ID
			}
		}
		if local, _, ok := strings.Cut(email, "@"); ok {
			if u := s.store.UsersByLogin[local]; u != nil {
				return u.ID
			}
		}
	}
	if u := s.store.UsersByLogin[name]; u != nil {
		return u.ID
	}
	return 0
}

// latestGateReviewStatesLocked returns each reviewer's most recent (by
// SubmittedAt) APPROVED or CHANGES_REQUESTED review; COMMENTED/PENDING/DISMISSED
// set no gate state, so a later COMMENTED review does not retract an APPROVED.
// Callers hold st.Mu.
func (s *Server) latestGateReviewStatesLocked(prID int) map[int]string {
	type ts struct {
		at    time.Time
		state string
	}
	latest := map[int]ts{}
	for _, rev := range s.store.PRReviews {
		if rev.PRID != prID {
			continue
		}
		if rev.State != "APPROVED" && rev.State != "CHANGES_REQUESTED" {
			continue
		}
		var at time.Time
		if rev.SubmittedAt != nil {
			at = *rev.SubmittedAt
		}
		if cur, ok := latest[rev.AuthorID]; !ok || at.After(cur.at) {
			latest[rev.AuthorID] = ts{at, rev.State}
		}
	}
	out := make(map[int]string, len(latest))
	for u, t := range latest {
		out[u] = t.state
	}
	return out
}

// countApprovingReviews counts the current APPROVED reviews that satisfy branch
// protection. The PR author's own approval never counts (GitHub excludes it, and
// forbids it at submission), so authorID is skipped.
func (s *Server) countApprovingReviews(prID, authorID int) int {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	count := 0
	for userID, state := range s.latestGateReviewStatesLocked(prID) {
		if state == "APPROVED" && userID != authorID {
			count++
		}
	}
	return count
}

func (s *Server) hasRequestedChanges(prID int) bool {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, state := range s.latestGateReviewStatesLocked(prID) {
		if state == "CHANGES_REQUESTED" {
			return true
		}
	}
	return false
}

// requiredCheckContexts returns the base branch's required status-check contexts.
func (s *Server) requiredCheckContexts(repoID int, baseBranch string) []string {
	bp := s.effectiveBranchProtectionFor(repoID, baseBranch)
	if bp == nil || bp.RequiredStatusChecks == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, c := range bp.RequiredStatusChecks.Contexts {
		add(c)
	}
	for _, c := range bp.RequiredStatusChecks.Checks {
		add(c.Context)
	}
	return out
}

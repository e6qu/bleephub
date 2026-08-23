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

// bpRequest is the PUT body for the top-level protection endpoint. GitHub
// accepts a sparse body: missing sub-objects leave existing rules unchanged,
// while an explicit null disables the corresponding rule.
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
	// Top-level branch protection
	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBranchProtectionGet))
	s.route("PUT /api/v3/repos/{owner}/{repo}/branches/{branch}/protection",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBranchProtectionPut))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBranchProtectionDelete))

	// Required status checks
	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPStatusChecksGet))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPStatusChecksPatch))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPStatusChecksDelete))

	// Required commit signatures
	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_signatures", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPRequiredSignaturesGet))
	s.route("POST /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_signatures",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRequiredSignaturesPost))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_signatures",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRequiredSignaturesDelete))

	// Restrictions apps
	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/apps", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPRestrictionsAppsGet))
	s.route("POST /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/apps",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsAppsPost))
	s.route("PUT /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/apps",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsAppsPut))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/apps",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsAppsDelete))

	// Required status checks contexts
	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks/contexts", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPContextsGet))
	s.route("POST /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks/contexts",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPContextsPost))
	s.route("PUT /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks/contexts",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPContextsPut))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks/contexts",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPContextsDelete))

	// Required pull request reviews
	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_pull_request_reviews", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPReviewsGet))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_pull_request_reviews",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPReviewsPatch))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/required_pull_request_reviews",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPReviewsDelete))

	// Restrictions
	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPRestrictionsGet))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsDelete))

	// Restrictions users
	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/users", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPRestrictionsUsersGet))
	s.route("POST /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/users",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsUsersPost))
	s.route("PUT /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/users",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsUsersPut))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/users",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsUsersDelete))

	// Restrictions teams
	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/teams", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPRestrictionsTeamsGet))
	s.route("POST /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/teams",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsTeamsPost))
	s.route("PUT /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/teams",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsTeamsPut))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/restrictions/teams",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPRestrictionsTeamsDelete))

	// Enforce admins
	s.route("GET /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/enforce_admins", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleBPEnforceAdminsGet))
	s.route("POST /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/enforce_admins",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPEnforceAdminsPost))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/branches/{branch}/protection/enforce_admins",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleBPEnforceAdminsDelete))

}

// branchProtectionURL returns the canonical URL for the top-level protection resource.
func (s *Server) branchProtectionURL(baseURL, fullName, branch string) string {
	return baseURL + "/api/v3/repos/" + fullName + "/branches/" + branch + "/protection"
}

func (s *Server) branchProtectionSubURL(baseURL, fullName, branch, sub string) string {
	return s.branchProtectionURL(baseURL, fullName, branch) + "/" + sub
}

// branchProtectionShape is the single source of truth for the protected flag
// and protection members embedded in branch responses. GitHub considers a
// branch protected when either a classic protection resource or an applicable
// ruleset protects it; deriving the flag from both prevents the branch and
// protection APIs from reporting contradictory state.
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

// branchProtectionFor is the single read path into the protection table; the
// map is written under Misc.mu and an unsynchronized read racing a write is a
// fatal error rather than a recoverable panic.
//
// The caller gets a copy. Handlers edit the rule they read and renderers fill
// request-derived URLs into it, and both would otherwise be writing the object
// every other request reads, without the lock and with this request's host.
func (s *Server) branchProtectionFor(repoID int, branch string) *store.BranchProtection {
	s.store.Misc.Mu.RLock()
	defer s.store.Misc.Mu.RUnlock()
	return cloneBranchProtection(s.store.Misc.BranchProtection[store.BpKey(repoID, branch)])
}

// effectiveBranchProtectionFor is the enforcement chokepoint's protection
// lookup: the exact-name rule when one exists, otherwise the first matching
// web-only pattern rule (/ui-data branch-protection-patterns). The REST
// protection resource handlers keep reading branchProtectionFor directly —
// GitHub's classic protection API addresses exact names only.
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

// cloneBranchProtection deep-copies a protection rule so that no caller holds a
// pointer into the stored table. Every field is copied explicitly and
// TestBranchProtectionCloneCoversEveryField fails when a new one is added
// without being copied here.
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

// setBranchProtection replaces the stored rule with a copy of the supplied one,
// so that whatever the caller does with its own rule afterwards — render it,
// hydrate URLs into it, edit it again — cannot reach the stored table.
func (s *Server) setBranchProtection(repo *store.Repo, branch string, bp *store.BranchProtection) {
	key := store.BpKey(repo.ID, branch)
	stored := cloneBranchProtection(bp)
	s.store.Misc.Mu.Lock()
	if stored == nil || !stored.IsProtected() {
		delete(s.store.Misc.BranchProtection, key)
		if s.store.Misc.Persist != nil {
			s.store.Misc.Persist.MustDelete("branch_protection", key)
		}
	} else {
		s.store.Misc.BranchProtection[key] = stored
		if s.store.Misc.Persist != nil {
			s.store.Misc.Persist.MustPut("branch_protection", key, stored)
		}
	}
	s.store.Misc.Mu.Unlock()
	// A protection state change can clear the condition an armed auto-merge
	// was waiting for (every protection sub-resource handler funnels here).
	s.maybeAutoMergeBranch(repo, branch)
}

func (s *Server) branchProtectionNotFound(w http.ResponseWriter) {
	writeGHError(w, http.StatusNotFound, "Branch not protected")
}

// --- Top-level protection ---

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
	// `branch_protection_rule` fires when a rule is established or updated, so
	// `on: branch_protection_rule` workflows run (ACT-026).
	if bp.IsProtected() {
		action := "created"
		if existed {
			action = "edited"
		}
		s.emitBranchProtectionRuleEvent(repo, branch, action, r)
	}
	// A protection state change can clear the condition an armed auto-merge
	// was waiting for.
	s.maybeAutoMergeBranch(repo, branch)
	writeJSON(w, http.StatusOK, bp)
}

// emitBranchProtectionRuleEvent fires GitHub's `branch_protection_rule` webhook.
func (s *Server) emitBranchProtectionRuleEvent(repo *store.Repo, branch, action string, r *http.Request) {
	s.emitWebhookEvent(repo.FullName, "branch_protection_rule", action, map[string]interface{}{
		"action":     action,
		"rule":       map[string]interface{}{"name": branch, "repository_id": repo.ID},
		"repository": repoPayload(repo),
		"sender":     store.UserToJSON(ghUserFromContext(r.Context())),
	})
}

func (s *Server) handleBranchProtectionDelete(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
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
// A present but null field clears the rule; an absent field leaves it unchanged.
func (s *Server) applyBranchProtectionRequest(bp *store.BranchProtection, req *bpRequest) *store.BranchProtection {
	if req.RequiredStatusChecks != nil {
		bp.RequiredStatusChecks = req.RequiredStatusChecks
	}
	if req.RequiredPullRequestReviews != nil {
		// An explicit review-policy object enables the rule even when its
		// approving-review count is zero. Zero is a valid GitHub setting, not
		// a sentinel for deleting the whole rule.
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

// --- Required status checks ---

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

// statusCheckPolicyJSON renders required_status_checks in the published
// status-check-policy shape: url, strict, contexts, checks, and contexts_url
// are all required members — contexts/checks are present even when empty.
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
// required_status_checks rule. Absent members are left unchanged; present
// members replace the stored value.
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
	if req.Contexts != nil {
		bp.RequiredStatusChecks.Contexts = *req.Contexts
	}
	if req.Checks != nil {
		bp.RequiredStatusChecks.Checks = *req.Checks
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

// --- Contexts ---

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
	writeJSON(w, http.StatusOK, bp.RequiredStatusChecks.Contexts)
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
	for _, context := range contexts {
		if !stringSliceContains(bp.RequiredStatusChecks.Contexts, context) {
			bp.RequiredStatusChecks.Contexts = append(bp.RequiredStatusChecks.Contexts, context)
		}
	}
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
	bp.RequiredStatusChecks.Contexts = contexts
	s.setBranchProtection(repo, branch, bp)
	writeJSON(w, http.StatusOK, contexts)
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
	bp.RequiredStatusChecks.Contexts = removeStrings(bp.RequiredStatusChecks.Contexts, contexts)
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

// --- Required pull request reviews ---

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
	var req store.BPPullRequestReviews
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.RequiredApprovingReviewCount > 0 || req.DismissStaleReviews || req.RequireCodeOwnerReviews || req.RequireLastPushApproval || req.DismissalRestrictions != nil || req.BypassPullRequestAllowances != nil {
		bp.RequiredPullRequestReviews = &req
	} else {
		bp.RequiredPullRequestReviews = nil
	}
	s.setBranchProtection(repo, branch, bp)
	req.URL = s.branchProtectionSubURL(s.baseURL(r), repo.FullName, branch, "required_pull_request_reviews")
	writeJSON(w, http.StatusOK, req)
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

// --- Restrictions ---

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

// --- Restrictions users ---

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
	writeJSON(w, http.StatusOK, s.bpRestrictedUsersJSON(bp.Restrictions.Users))
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
	writeJSON(w, http.StatusOK, s.bpRestrictedUsersJSON(bp.Restrictions.Users))
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
	writeJSON(w, http.StatusOK, s.bpRestrictedUsersJSON(users))
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
	writeJSON(w, http.StatusOK, s.bpRestrictedUsersJSON(bp.Restrictions.Users))
}

// --- Restrictions teams ---

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

func (s *Server) bpRestrictedUsersJSON(actors []store.BPActor) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(actors))
	for _, actor := range actors {
		s.store.Mu.RLock()
		user := s.store.Users[actor.ID]
		var rendered map[string]interface{}
		if user != nil {
			rendered = store.UserToJSON(user)
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

// --- Enforce admins ---

func (s *Server) handleBPEnforceAdminsGet(w http.ResponseWriter, r *http.Request) {
	repo, branch, bp := s.getBranchProtection(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if bp == nil || bp.EnforceAdmins == nil {
		s.branchProtectionNotFound(w)
		return
	}
	ea := *bp.EnforceAdmins
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
	bp.EnforceAdmins = nil
	s.setBranchProtection(repo, branch, bp)
	w.WriteHeader(http.StatusNoContent)
}

// --- Required commit signatures ---

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

// --- Restrictions apps ---

// bpRestrictedAppsJSON renders the restriction's app actors as full GitHub
// App (integration) objects, the shape the restrictions/apps endpoints
// return.
func (s *Server) bpRestrictedAppsJSON(actors []store.BPActor) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(actors))
	for _, actor := range actors {
		s.store.Mu.RLock()
		app := s.store.AppsBySlug[actor.Login]
		s.store.Mu.RUnlock()
		if app != nil {
			out = append(out, appToJSON(s.store, app, false))
		}
	}
	return out
}

// decodeBPAppSlugs decodes the {"apps": ["slug", ...]} body shared by the
// restrictions/apps mutation endpoints and resolves every slug to a
// registered GitHub App. Writes a 422 and returns nil when a slug does not
// resolve.
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

// --- Helpers ---

// decodeStringArrayBody decodes either a bare JSON array or {"contexts":[...]}.
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
	// refFastForward is an update whose target descends from what the branch
	// already points at, so no commit the branch reaches is discarded.
	refFastForward
)

// protectedRefWriteRefusal decides one ref write against the branch's
// protection rule and returns the refusal, or "" when the rule allows it. Both
// git transports and the REST ref-write routes decide here, so a rule cannot
// bind one lane and be decoration on the other.
//
// A push writes the ref rather than merging into it, so the destructive
// allowances — force push, deletion, creation — are decided here as well as the
// requirements a merge into the branch faces. Those come from the evaluator the
// merge gate itself uses, applied to the commit being pushed.
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
	// lock_branch makes the branch read-only for everyone, administrators
	// included (unlocking is how you write to it): every ref write — push,
	// force push, deletion, creation — is refused, on both git transports and
	// the REST ref-write routes, since they all decide here. Decided before
	// the enforce_admins bypass, matching the merge gate.
	if bp.LockBranch != nil && bp.LockBranch.Enabled {
		return "Cannot update this locked branch: " + branch + " is read-only."
	}
	// enforce_admins is the setting that decides whether the rule binds a
	// repository administrator; with it off an administrator bypasses the
	// whole rule, which is the same bypass the merge gate grants.
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
		// A deletion moves the branch nowhere, so the requirements on what
		// it may point at do not apply to it.
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

// protectedBranchTargetRefusal answers the requirements that govern what a
// protected branch may be moved to. A direct write faces them exactly as a
// merge into the branch does.
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

// commitsAddedToBranch returns the commits a write of target onto branch adds
// to it: those reachable from target but not from where the branch stands.
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

// viewerIsRestrictedPusher reports whether the request's actor is one of the
// actors a restrictions rule allows to write the branch.
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

// canMergePullRequest checks branch protection rules for a PR merge.
// It returns (ok, errorMessage). ok==false with empty message means the caller
// should fall back to the existing required-status-check message.
func (s *Server) canMergePullRequest(ctx context.Context, repo *store.Repo, pr *store.PullRequest) (bool, string) {
	bp := s.effectiveBranchProtectionFor(repo.ID, pr.BaseRefName)
	if bp == nil {
		return true, ""
	}

	isAdmin := s.viewerCanAdminRepo(ctx, repo)

	// A locked branch is read-only for everyone, admins included — GitHub's
	// lock_branch has no enforce_admins carve-out. A merge writes the base
	// branch, so it is refused before any bypass applies.
	if bp.LockBranch != nil && bp.LockBranch.Enabled {
		return false, "Cannot merge into locked branch " + pr.BaseRefName + "."
	}

	// Admin bypass only when enforce_admins is not enabled.
	if isAdmin && (bp.EnforceAdmins == nil || !bp.EnforceAdmins.Enabled) {
		return true, ""
	}

	// Required status checks
	headSha := s.prHeadSha(repo, pr)
	if headSha != "" {
		st := s.evaluateChecksForMerge(repo, pr.BaseRefName, headSha)
		if len(st.MissingRequired) > 0 {
			return false, fmt.Sprintf("Required status check %q is expected.", st.MissingRequired[0])
		}
	}

	// Required approving review count
	if bp.RequiredPullRequestReviews != nil && bp.RequiredPullRequestReviews.RequiredApprovingReviewCount > 0 {
		count := s.countApprovingReviews(pr.ID)
		if count < bp.RequiredPullRequestReviews.RequiredApprovingReviewCount {
			return false, fmt.Sprintf("At least %d approving review is required by the branch protection rules.", bp.RequiredPullRequestReviews.RequiredApprovingReviewCount)
		}
	}

	// require_code_owner_reviews: every code owner of a file this pull request
	// changes must have approved it. Enforced here, at the one gate the REST
	// merge, the GraphQL mergePullRequest mutation and auto-merge all pass
	// through, so no merge path can skip it.
	if bp.RequiredPullRequestReviews != nil && bp.RequiredPullRequestReviews.RequireCodeOwnerReviews {
		if !s.codeOwnerApprovalsSatisfied(repo, pr) {
			return false, "Review by a code owner of the changed files is required by the branch protection rules."
		}
	}

	// require_last_push_approval: the most recent reviewable push must be
	// approved by someone other than the person who pushed it.
	if bp.RequiredPullRequestReviews != nil && bp.RequiredPullRequestReviews.RequireLastPushApproval {
		if !s.lastPushApproved(repo, pr) {
			return false, "The most recent push must be approved by someone other than the person who pushed it."
		}
	}

	// Requested changes block merge
	if s.hasRequestedChanges(pr.ID) {
		return false, "Changes have been requested on this pull request."
	}

	return true, ""
}

// lastPushApproved reports whether somebody other than the head branch's
// last pusher holds a current APPROVED review on the pull request. The
// pusher of the latest head commit is resolved from the commit's committer
// identity (email match first, then the login-derived local part, then the
// committer name); when nothing resolves, the PR's author stands in — the
// author proposed the head and must not self-satisfy the gate.
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

// lastHeadPusherID resolves the user behind the PR's current head commit.
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

// userIDForCommitIdentity maps a git signature to a store user: exact email
// match, then the email's local part as a login, then the name as a login.
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

// latestGateReviewStatesLocked returns each reviewer's effective merge-gate
// review state for prID: their most recent (by SubmittedAt) APPROVED or
// CHANGES_REQUESTED review. COMMENTED / PENDING / DISMISSED reviews set no
// gate-affecting state, matching github (a later COMMENTED review does not
// retract a prior APPROVED). Callers hold st.Mu. Iterating the review map
// without ordering by time previously made "latest per user" nondeterministic.
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

func (s *Server) countApprovingReviews(prID int) int {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	count := 0
	for _, state := range s.latestGateReviewStatesLocked(prID) {
		if state == "APPROVED" {
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

// requiredCheckContexts returns the base branch's protected status-check
// contexts from the typed model.
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

// branchProtectionRuleForPR returns a GraphQL-shaped map for baseRef.branchProtectionRule.
func (s *Server) branchProtectionRuleForPR(repo *store.Repo, baseBranch string) map[string]interface{} {
	bp := s.branchProtectionFor(repo.ID, baseBranch)
	if bp == nil {
		return nil
	}
	strict := false
	count := 0
	if bp.RequiredStatusChecks != nil {
		strict = bp.RequiredStatusChecks.Strict
	}
	if bp.RequiredPullRequestReviews != nil {
		count = bp.RequiredPullRequestReviews.RequiredApprovingReviewCount
	}
	return map[string]interface{}{
		"requiresStrictStatusChecks":   strict,
		"requiredApprovingReviewCount": count,
	}
}

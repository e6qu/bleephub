package store

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// BranchProtection matches the GitHub REST API response shape for
// GET /repos/{owner}/{repo}/branches/{branch}/protection. All pointer
// sub-fields are omitempty in JSON so that an unset rule does not appear
// in the canonical response.
type BranchProtection struct {
	RequiredStatusChecks           *BPStatusChecks       `json:"required_status_checks,omitempty"`
	RequiredPullRequestReviews     *BPPullRequestReviews `json:"required_pull_request_reviews,omitempty"`
	EnforceAdmins                  *BPEnforceAdmins      `json:"enforce_admins,omitempty"`
	Restrictions                   *BPRestrictions       `json:"restrictions,omitempty"`
	RequiredLinearHistory          *BPEnabled            `json:"required_linear_history,omitempty"`
	AllowForcePushes               *BPEnabled            `json:"allow_force_pushes,omitempty"`
	AllowDeletions                 *BPEnabled            `json:"allow_deletions,omitempty"`
	BlockCreations                 *BPEnabled            `json:"block_creations,omitempty"`
	RequiredConversationResolution *BPEnabled            `json:"required_conversation_resolution,omitempty"`
	RequiredSignatures             *BPEnabledURL         `json:"required_signatures,omitempty"`
	LockBranch                     *BPEnabled            `json:"lock_branch,omitempty"`
	AllowForkSyncing               *BPEnabled            `json:"allow_fork_syncing,omitempty"`
	URL                            string                `json:"url,omitempty"`
	// Enabled is the documented `enabled` member of branch-protection, and the
	// record that the branch is protected at all. Protection is established by
	// PUT .../protection and removed by DELETE .../protection — never as a side
	// effect of turning one rule off. Without it, disabling the last remaining
	// rule (say, admin enforcement) silently dropped every other rule with it.
	Enabled bool `json:"enabled,omitempty"`
}

// IsProtected reports whether the branch has any protection rule enabled.
func (bp *BranchProtection) IsProtected() bool {
	if bp == nil {
		return false
	}
	return bp.Enabled ||
		bp.RequiredStatusChecks != nil ||
		bp.RequiredPullRequestReviews != nil ||
		bp.EnforceAdmins != nil ||
		bp.Restrictions != nil ||
		bp.RequiredLinearHistory != nil ||
		bp.AllowForcePushes != nil ||
		bp.AllowDeletions != nil ||
		bp.BlockCreations != nil ||
		bp.RequiredConversationResolution != nil ||
		bp.RequiredSignatures != nil ||
		bp.LockBranch != nil ||
		bp.AllowForkSyncing != nil
}

// BPEnabled is the shape used by required_linear_history, allow_force_pushes,
// allow_deletions, block_creations, and required_conversation_resolution.
type BPEnabled struct {
	Enabled bool `json:"enabled"`
}

// BPEnabledURL is the shape used by required_signatures which also carries a URL.
type BPEnabledURL struct {
	URL     string `json:"url,omitempty"`
	Enabled bool   `json:"enabled"`
}

// BPEnforceAdmins is the enforce_admins object.
type BPEnforceAdmins struct {
	URL     string `json:"url,omitempty"`
	Enabled bool   `json:"enabled"`
}

// BPPullRequestReviews is the required_pull_request_reviews object.
type BPPullRequestReviews struct {
	URL                          string              `json:"url,omitempty"`
	DismissStaleReviews          bool                `json:"dismiss_stale_reviews"`
	RequireCodeOwnerReviews      bool                `json:"require_code_owner_reviews"`
	RequireLastPushApproval      bool                `json:"require_last_push_approval"`
	RequiredApprovingReviewCount int                 `json:"required_approving_review_count"`
	DismissalRestrictions        *BPRestrictions     `json:"dismissal_restrictions,omitempty"`
	BypassPullRequestAllowances  *BPBypassAllowances `json:"bypass_pull_request_allowances,omitempty"`
}

// BPRestrictions is the restrictions object (push + dismissal). users,
// teams, and apps are required members of the published
// branch-restriction-policy shape, so they serialize even when empty.
type BPRestrictions struct {
	Users    []BPActor `json:"users"`
	Teams    []BPActor `json:"teams"`
	Apps     []BPActor `json:"apps"`
	URL      string    `json:"url,omitempty"`
	UsersURL string    `json:"users_url,omitempty"`
	TeamsURL string    `json:"teams_url,omitempty"`
	AppsURL  string    `json:"apps_url,omitempty"`
}

// BPStatusChecks is the required_status_checks object. contexts and checks
// are required members of the published status-check-policy shape, so they
// serialize even when empty (hydrateBranchProtectionURLs normalizes nil
// slices before responses are written).
type BPStatusChecks struct {
	URL              string    `json:"url,omitempty"`
	EnforcementLevel string    `json:"enforcement_level,omitempty"`
	Contexts         []string  `json:"contexts"`
	Checks           []BPCheck `json:"checks"`
	Strict           bool      `json:"strict"`
	ContextsURL      string    `json:"contexts_url,omitempty"`
}

// contexts and checks are two views of ONE set. The published
// status-check-policy schema requires both members, and GitHub keeps them in
// step: `contexts` is the legacy list of names, `checks` the same names paired
// with the app that may report them. Writing either therefore has to update the
// other, or the /contexts sub-resource reads back empty on a policy written
// through `checks` — and a merge gate built from one view disagrees with the
// policy a client reads from the other.

// SetChecks replaces the required-check set from the richer view, deriving
// contexts from it.
func (sc *BPStatusChecks) SetChecks(checks []BPCheck) {
	if checks == nil {
		checks = []BPCheck{}
	}
	sc.Checks = checks
	contexts := make([]string, 0, len(checks))
	for _, check := range checks {
		contexts = append(contexts, check.Context)
	}
	sc.Contexts = contexts
}

// SetContexts replaces the required-check set from the names view, deriving
// checks from it. A context that already named an app keeps that app: writing
// the legacy view must not silently widen which app may report a check.
func (sc *BPStatusChecks) SetContexts(contexts []string) {
	if contexts == nil {
		contexts = []string{}
	}
	apps := make(map[string]*int64, len(sc.Checks))
	for _, check := range sc.Checks {
		apps[check.Context] = check.AppID
	}
	sc.Contexts = contexts
	checks := make([]BPCheck, 0, len(contexts))
	for _, context := range contexts {
		checks = append(checks, BPCheck{Context: context, AppID: ClonePointer(apps[context])})
	}
	sc.Checks = checks
}

// BPActor is a lightweight user/team/app reference used in restrictions.
type BPActor struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

// BPBypassAllowances lists users/teams/apps that can bypass pull-request requirements.
type BPBypassAllowances struct {
	Users []BPActor `json:"users,omitempty"`
	Teams []BPActor `json:"teams,omitempty"`
	Apps  []BPActor `json:"apps,omitempty"`
}

// BPCheck is an entry in required_status_checks.checks. app_id is a
// required, nullable member — it serializes as null when no app is pinned.
type BPCheck struct {
	Context string `json:"context"`
	AppID   *int64 `json:"app_id"`
}

// ClonePointer copies the value behind a pointer field.
func ClonePointer[T any](p *T) *T {
	if p == nil {
		return nil
	}
	copied := *p
	return &copied
}

// BranchProtectionRuleNodeID is the global id of a branch protection rule.
// bleephub keys protection by repository and pattern rather than by a row id,
// so the node id encodes exactly that pair.
func BranchProtectionRuleNodeID(repoID int, pattern string) string {
	return "BPR_" + base64.RawURLEncoding.EncodeToString([]byte("branch-protection-rule:"+strconv.Itoa(repoID)+":"+pattern))
}

// ParseBranchProtectionRuleNodeID inverts BranchProtectionRuleNodeID.
func ParseBranchProtectionRuleNodeID(nodeID string) (repoID int, pattern string, ok bool) {
	rest, found := strings.CutPrefix(nodeID, "BPR_")
	if !found {
		return 0, "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return 0, "", false
	}
	body, found := strings.CutPrefix(string(decoded), "branch-protection-rule:")
	if !found {
		return 0, "", false
	}
	idText, pattern, found := strings.Cut(body, ":")
	if !found || pattern == "" {
		return 0, "", false
	}
	repoID, err = strconv.Atoi(idText)
	if err != nil || repoID <= 0 {
		return 0, "", false
	}
	return repoID, pattern, true
}

// BranchProtectionRuleExtras carries the members of a branch protection rule
// GitHub's GraphQL surface declares but its REST protection shape does not.
// They are stored beside the BranchProtection under the same BpKey so the
// REST responses — which serialize BranchProtection directly — never grow
// keys the published schema does not have.
type BranchProtectionRuleExtras struct {
	CreatorLogin                   string    `json:"creator_login,omitempty"`
	RequiresDeployments            bool      `json:"requires_deployments,omitempty"`
	RequiredDeploymentEnvironments []string  `json:"required_deployment_environments,omitempty"`
	BypassForcePushActors          []BPActor `json:"bypass_force_push_actors,omitempty"`
}

func cloneBranchProtectionRuleExtras(e *BranchProtectionRuleExtras) *BranchProtectionRuleExtras {
	if e == nil {
		return nil
	}
	c := *e
	c.RequiredDeploymentEnvironments = append([]string(nil), e.RequiredDeploymentEnvironments...)
	c.BypassForcePushActors = append([]BPActor(nil), e.BypassForcePushActors...)
	return &c
}

// deepCloneBranchProtection deep-copies a rule through its own JSON shape, so
// no caller holds a pointer into the stored table. The JSON round trip covers
// every field by construction — the same property the pattern-rule store
// relies on — so a field added to the struct cannot be silently shared.
func deepCloneBranchProtection(bp *BranchProtection) *BranchProtection {
	if bp == nil {
		return nil
	}
	raw, err := json.Marshal(bp)
	if err != nil {
		return nil
	}
	var out BranchProtection
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	// Enabled is omitempty-invisible only when false; carry it explicitly so
	// the clone cannot drop protection established by a bare PUT.
	out.Enabled = bp.Enabled
	return &out
}

// GetBranchProtection returns a detached copy of the repository's exact-name
// protection rule for branch, or nil when the branch carries none. It is the
// same record GET /repos/{owner}/{repo}/branches/{branch}/protection serves.
func (st *Store) GetBranchProtection(repoID int, branch string) *BranchProtection {
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	return deepCloneBranchProtection(st.Misc.BranchProtection[BpKey(repoID, branch)])
}

// SetBranchProtection replaces (or, for a nil or empty rule, removes) the
// stored exact-name protection rule — the write PUT and DELETE
// /branches/{branch}/protection perform. The stored rule is a detached copy.
func (st *Store) SetBranchProtection(repoID int, branch string, bp *BranchProtection) {
	key := BpKey(repoID, branch)
	stored := deepCloneBranchProtection(bp)
	st.Misc.Mu.Lock()
	defer st.Misc.Mu.Unlock()
	if stored == nil || !stored.IsProtected() {
		delete(st.Misc.BranchProtection, key)
		if st.Misc.Persist != nil {
			st.Misc.Persist.MustDelete("branch_protection", key)
		}
		return
	}
	st.Misc.BranchProtection[key] = stored
	if st.Misc.Persist != nil {
		st.Misc.Persist.MustPut("branch_protection", key, stored)
	}
}

// ListBranchProtectedBranches returns the branch names carrying an exact-name
// protection rule in this repository, sorted.
func (st *Store) ListBranchProtectedBranches(repoID int) []string {
	prefix := strconv.Itoa(repoID) + ":"
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	var out []string
	for key := range st.Misc.BranchProtection {
		if branch, ok := strings.CutPrefix(key, prefix); ok {
			out = append(out, branch)
		}
	}
	sort.Strings(out)
	return out
}

// GetBranchProtectionExtras returns a detached copy of the GraphQL-only
// members stored for the rule, or nil.
func (st *Store) GetBranchProtectionExtras(repoID int, pattern string) *BranchProtectionRuleExtras {
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	return cloneBranchProtectionRuleExtras(st.Misc.BranchProtectionExtras[BpKey(repoID, pattern)])
}

// SetBranchProtectionExtras stores (or, for nil, removes) the GraphQL-only
// members for the rule keyed by its pattern.
func (st *Store) SetBranchProtectionExtras(repoID int, pattern string, extras *BranchProtectionRuleExtras) {
	key := BpKey(repoID, pattern)
	stored := cloneBranchProtectionRuleExtras(extras)
	st.Misc.Mu.Lock()
	defer st.Misc.Mu.Unlock()
	if stored == nil {
		delete(st.Misc.BranchProtectionExtras, key)
		if st.Misc.Persist != nil {
			st.Misc.Persist.MustDelete("branch_protection_extras", key)
		}
		return
	}
	st.Misc.BranchProtectionExtras[key] = stored
	if st.Misc.Persist != nil {
		st.Misc.Persist.MustPut("branch_protection_extras", key, stored)
	}
}

// MoveBranchProtectionExtras carries a rule's GraphQL-only members from one
// pattern key to another (a pattern edit, or a branch rename).
func (st *Store) MoveBranchProtectionExtras(repoID int, oldPattern, newPattern string) {
	oldKey := BpKey(repoID, oldPattern)
	newKey := BpKey(repoID, newPattern)
	st.Misc.Mu.Lock()
	defer st.Misc.Mu.Unlock()
	extras, ok := st.Misc.BranchProtectionExtras[oldKey]
	if !ok {
		return
	}
	st.Misc.BranchProtectionExtras[newKey] = extras
	delete(st.Misc.BranchProtectionExtras, oldKey)
	if st.Misc.Persist != nil {
		st.Misc.Persist.MustPut("branch_protection_extras", newKey, extras)
		st.Misc.Persist.MustDelete("branch_protection_extras", oldKey)
	}
}

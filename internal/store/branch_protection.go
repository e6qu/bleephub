package store

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

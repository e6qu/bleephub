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
	URL                            string                `json:"url,omitempty"`
}

// IsProtected reports whether the branch has any protection rule enabled.
func (bp *BranchProtection) IsProtected() bool {
	if bp == nil {
		return false
	}
	return bp.RequiredStatusChecks != nil ||
		bp.RequiredPullRequestReviews != nil ||
		bp.EnforceAdmins != nil ||
		bp.Restrictions != nil ||
		bp.RequiredLinearHistory != nil ||
		bp.AllowForcePushes != nil ||
		bp.AllowDeletions != nil ||
		bp.BlockCreations != nil ||
		bp.RequiredConversationResolution != nil ||
		bp.RequiredSignatures != nil
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

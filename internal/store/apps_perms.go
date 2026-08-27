package store

// PermScope is a GitHub fine-grained permission name. The values are the
// exact keys used in installation-token Permissions maps and the App API;
// they must not change.
type PermScope string

const (
	ScopeMetadata          PermScope = "metadata"
	ScopeContents          PermScope = "contents"
	ScopeIssues            PermScope = "issues"
	ScopeDiscussions       PermScope = "discussions"
	ScopePullRequests      PermScope = "pull_requests"
	ScopeActions           PermScope = "actions"
	ScopeChecks            PermScope = "checks"
	ScopeSecrets           PermScope = "secrets"
	ScopeDeployments       PermScope = "deployments"
	ScopeAdministration    PermScope = "administration"
	ScopeMembers           PermScope = "members"
	ScopeOrgAdministration PermScope = "organization_administration"
	ScopeOrganizationHooks PermScope = "organization_hooks"
	ScopeSecurityEvents    PermScope = "security_events"
	ScopeDependabotSecrets PermScope = "dependabot_secrets" // #nosec G101 -- permission name, not a secret
	ScopeCodespaces        PermScope = "codespaces"
	ScopeReactions         PermScope = "reactions"
	ScopeProjects          PermScope = "projects"
	ScopePages             PermScope = "pages"
	ScopePATRequests       PermScope = "organization_personal_access_token_requests"
	ScopePATs              PermScope = "organization_personal_access_tokens"
	ScopeCopilotSpaces     PermScope = "copilot_spaces"
)

// PermLevel is the entitlement level a credential holds for a PermScope.
// Ordering: read < write < admin, each level implying the ones below it.
type PermLevel int

const (
	PermRead PermLevel = iota
	PermWrite
	PermAdmin
)

// AccountKind distinguishes the two account namespaces an entitlement can
// target. Users and organizations share one login space, so authorization
// checks must not match on login alone. AnyAccount covers resources that
// hang off either kind, such as a ProjectV2.
type AccountKind int

const (
	AnyAccount AccountKind = iota
	OrganizationAccount
)

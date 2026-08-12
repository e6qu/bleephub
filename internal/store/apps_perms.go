package store

// PermScope is a GitHub fine-grained permission name. The values are the
// exact keys used in an installation token's Permissions map and in the
// App API, so they must not change — but call sites reference the named
// constants, making a mistyped scope a compile error rather than a silent
// always-deny gate.
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
// Level ordering: read < write < admin. "admin" implies write; "write"
// implies read. (Moved from the server layer in ARCH-003 so the GraphQL
// resolver layer and the HTTP layer share one definition.)
type PermLevel int

const (
	PermRead PermLevel = iota
	PermWrite
	PermAdmin
)

// AccountKind distinguishes the two account namespaces an entitlement can
// target. Users and organizations are separate namespaces sharing one login
// space, so authorization checks must not match on login alone.
// AnyAccount is for resources that hang off an account of either kind,
// such as a ProjectV2. (Moved from the server layer in ARCH-003.)
type AccountKind int

const (
	AnyAccount AccountKind = iota
	OrganizationAccount
)

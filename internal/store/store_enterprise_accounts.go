package store

// Enterprise accounts: the account entity, its membership roll, organizations,
// invitations, support entitlements, SAML identity provider binding and IP
// allow list. This is the account layer GitHub's GraphQL schema models, distinct
// from the pre-existing GHES enterprise-settings singleton. Everything is keyed
// by enterprise id, so one enterprise's data is never reachable from another.
//
// STORE-021: every getter and List* returns a detached snapshot;
// FindEnterpriseByNodeID returns the live row (the write path's lookup).

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// EnterpriseNodeIDPrefix is the global-id prefix for Enterprise nodes.
const EnterpriseNodeIDPrefix = "E_kgDO"

// EnterpriseUserAccountNodeIDPrefix is the global-id prefix for the
// EnterpriseUserAccount projection of a user's membership. The suffix is the
// membership's own database id.
const EnterpriseUserAccountNodeIDPrefix = "EUA_kgDO"

// EnterpriseAdminInvitationNodeIDPrefix and EnterpriseMemberInvitationNodeIDPrefix
// are the global-id prefixes for the two enterprise invitation kinds.
const (
	EnterpriseAdminInvitationNodeIDPrefix  = "EAI_kgDO"
	EnterpriseMemberInvitationNodeIDPrefix = "EMI_kgDO"
	IPAllowListEntryNodeIDPrefix           = "IPALE_kgDO"
)

// IP allow list owner types. The user-scoped list has no GraphQL owner and is
// reachable only from the account's own /ui-data surface.
const (
	IPAllowListOwnerEnterprise   = "Enterprise"
	IPAllowListOwnerOrganization = "Organization"
	IPAllowListOwnerUser         = "User"
)

// EnterpriseRole is a principal's standing in an enterprise account, spelled
// with GitHub's enum values so the stored value is what the GraphQL enums serve.
type EnterpriseRole string

const (
	// EnterpriseRoleOwner has full administrative authority over the enterprise.
	EnterpriseRoleOwner EnterpriseRole = "OWNER"
	// EnterpriseRoleBillingManager may read and change billing only.
	EnterpriseRoleBillingManager EnterpriseRole = "BILLING_MANAGER"
	// EnterpriseRoleMember belongs to at least one of the enterprise's
	// organizations, or was added directly.
	EnterpriseRoleMember EnterpriseRole = "MEMBER"
	// EnterpriseRoleUnaffiliated is an invited principal belonging to none of
	// the enterprise's organizations.
	EnterpriseRoleUnaffiliated EnterpriseRole = "UNAFFILIATED"
)

// ValidEnterpriseAdministratorRole reports whether role is one GitHub's
// EnterpriseAdministratorRole enum admits.
func ValidEnterpriseAdministratorRole(role string) bool {
	switch EnterpriseRole(role) {
	case EnterpriseRoleOwner, EnterpriseRoleBillingManager, EnterpriseRoleUnaffiliated:
		return true
	}
	return false
}

// Policy-setting values, spelled with GitHub's enum values. A policy field's
// zero value is never served — the enterprise is created with GitHub's defaults
// so a non-null GraphQL field always has a value.
const (
	EnterprisePolicyEnabled  = "ENABLED"
	EnterprisePolicyDisabled = "DISABLED"
	EnterprisePolicyNoPolicy = "NO_POLICY"
	// EnterprisePolicyDisallowInsecure bans the second-factor methods classed
	// insecure.
	EnterprisePolicyDisallowInsecure = "INSECURE"
	// The two proof-of-presence requirements beside NO_POLICY: a fresh MFA
	// challenge, or a full re-authentication against the identity provider.
	EnterpriseProofOfPresenceMFA    = "MFA"
	EnterpriseProofOfPresenceReauth = "REAUTH"
)

// EnterprisePolicy is the enterprise-wide policy set. Each field is stored with
// GitHub's enum spelling and read by every consumer — REST handlers, GraphQL
// resolvers and the server-package enforcement predicates.
type EnterprisePolicy struct {
	// AllowPrivateRepositoryForking governs whether private/internal repos may be
	// forked at all; PolicyValue narrows where a permitted fork may land.
	AllowPrivateRepositoryForking            string `json:"allow_private_repository_forking"`
	AllowPrivateRepositoryForkingPolicyValue string `json:"allow_private_repository_forking_policy_value"`
	// DefaultRepositoryPermission is the ceiling on the base permission an
	// organization may grant its members.
	DefaultRepositoryPermission          string `json:"default_repository_permission"`
	MembersCanChangeRepositoryVisibility string `json:"members_can_change_repository_visibility"`
	// The three booleans are the per-visibility refinement GitHub applies when
	// MembersCanCreateRepositories is neither DISABLED nor NO_POLICY.
	MembersCanCreateRepositories         string `json:"members_can_create_repositories"`
	MembersCanCreatePublicRepositories   *bool  `json:"members_can_create_public_repositories"`
	MembersCanCreatePrivateRepositories  *bool  `json:"members_can_create_private_repositories"`
	MembersCanCreateInternalRepositories *bool  `json:"members_can_create_internal_repositories"`
	MembersCanDeleteIssues               string `json:"members_can_delete_issues"`
	MembersCanDeleteRepositories         string `json:"members_can_delete_repositories"`
	MembersCanInviteCollaborators        string `json:"members_can_invite_collaborators"`
	MembersCanMakePurchases              string `json:"members_can_make_purchases"`
	MembersCanUpdateProtectedBranches    string `json:"members_can_update_protected_branches"`
	MembersCanViewDependencyInsights     string `json:"members_can_view_dependency_insights"`
	OrganizationProjects                 string `json:"organization_projects"`
	RepositoryProjects                   string `json:"repository_projects"`
	RepositoryDeployKey                  string `json:"repository_deploy_key"`
	TeamDiscussions                      string `json:"team_discussions"`
	TwoFactorRequired                    string `json:"two_factor_required"`
	// TwoFactorDisallowedMethods bans insecure second factors (SMS).
	TwoFactorDisallowedMethods string `json:"two_factor_disallowed_methods"`
	// ProofOfPresenceRequired demands a fresh proof of presence before a
	// sensitive action.
	ProofOfPresenceRequired string `json:"proof_of_presence_required"`
	// NotificationDeliveryRestrictionEnabled restricts notification delivery to
	// the enterprise's verified domains.
	NotificationDeliveryRestrictionEnabled string `json:"notification_delivery_restriction_enabled"`
	IPAllowListEnabled                     string `json:"ip_allow_list_enabled"`
	IPAllowListForInstalledAppsEnabled     string `json:"ip_allow_list_for_installed_apps_enabled"`
	IPAllowListUserLevelEnforcementEnabled string `json:"ip_allow_list_user_level_enforcement_enabled"`
	// These report an unfinished enterprise-wide roll-out. bleephub applies a
	// policy to every organization in one batch write, so both are false outside
	// the window a mutation is mid-apply.
	IsUpdatingDefaultRepositoryPermission bool `json:"is_updating_default_repository_permission"`
	IsUpdatingTwoFactorRequirement        bool `json:"is_updating_two_factor_requirement"`
}

// defaultEnterprisePolicy is GitHub's out-of-the-box enterprise policy set.
func defaultEnterprisePolicy() EnterprisePolicy {
	return EnterprisePolicy{
		AllowPrivateRepositoryForking:          EnterprisePolicyNoPolicy,
		DefaultRepositoryPermission:            EnterprisePolicyNoPolicy,
		MembersCanChangeRepositoryVisibility:   EnterprisePolicyNoPolicy,
		MembersCanCreateRepositories:           EnterprisePolicyNoPolicy,
		MembersCanDeleteIssues:                 EnterprisePolicyNoPolicy,
		MembersCanDeleteRepositories:           EnterprisePolicyNoPolicy,
		MembersCanInviteCollaborators:          EnterprisePolicyNoPolicy,
		MembersCanMakePurchases:                EnterprisePolicyEnabled,
		MembersCanUpdateProtectedBranches:      EnterprisePolicyNoPolicy,
		MembersCanViewDependencyInsights:       EnterprisePolicyNoPolicy,
		OrganizationProjects:                   EnterprisePolicyNoPolicy,
		RepositoryProjects:                     EnterprisePolicyNoPolicy,
		RepositoryDeployKey:                    EnterprisePolicyNoPolicy,
		TeamDiscussions:                        EnterprisePolicyNoPolicy,
		TwoFactorRequired:                      EnterprisePolicyNoPolicy,
		TwoFactorDisallowedMethods:             EnterprisePolicyNoPolicy,
		ProofOfPresenceRequired:                EnterprisePolicyNoPolicy,
		NotificationDeliveryRestrictionEnabled: EnterprisePolicyDisabled,
		IPAllowListEnabled:                     EnterprisePolicyDisabled,
		IPAllowListForInstalledAppsEnabled:     EnterprisePolicyDisabled,
		IPAllowListUserLevelEnforcementEnabled: EnterprisePolicyDisabled,
	}
}

// EnterpriseSAMLIdentityProvider is an enterprise's SAML binding plus its
// one-time recovery codes. It describes the delegation to the external OIDC
// provider bleephub already authenticates against; it is not a second
// authentication mechanism.
type EnterpriseSAMLIdentityProvider struct {
	EnterpriseID    int       `json:"enterprise_id"`
	NodeID          string    `json:"node_id"`
	SSOURL          string    `json:"sso_url"`
	Issuer          string    `json:"issuer"`
	IDPCertificate  string    `json:"idp_certificate"`
	SignatureMethod string    `json:"signature_method"`
	DigestMethod    string    `json:"digest_method"`
	RecoveryCodes   []string  `json:"recovery_codes"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Enterprise is an enterprise account.
type Enterprise struct {
	ID                   int              `json:"id"`
	NodeID               string           `json:"node_id"`
	Slug                 string           `json:"slug"`
	Name                 string           `json:"name"`
	Description          string           `json:"description"`
	Location             string           `json:"location"`
	WebsiteURL           string           `json:"website_url"`
	AvatarURL            string           `json:"avatar_url"`
	BillingEmail         string           `json:"billing_email"`
	SecurityContactEmail string           `json:"security_contact_email"`
	Readme               string           `json:"readme"`
	Policy               EnterprisePolicy `json:"policy"`
	// IdentityProvider is nil until setEnterpriseIdentityProvider binds one.
	IdentityProvider *EnterpriseSAMLIdentityProvider `json:"identity_provider,omitempty"`
	// MigratorLogins is the set of user logins granted the migrator role on
	// every organization in the enterprise.
	MigratorLogins []string `json:"migrator_logins,omitempty"`
	// VerifiedDomains are the enterprise's verified domains, stored lower-cased
	// and without a leading "@". The notification-delivery restriction is
	// expressed against them.
	VerifiedDomains []string `json:"verified_domains,omitempty"`
	// Provisioned billing entitlements; usage is measured from actual
	// repositories and packages rather than stored.
	BillingBandwidthQuotaGB float64   `json:"billing_bandwidth_quota_gb"`
	BillingStorageQuotaGB   float64   `json:"billing_storage_quota_gb"`
	BillingTotalLicenses    int       `json:"billing_total_licenses"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}

// EnterpriseMembership binds a user to an enterprise with a role.
type EnterpriseMembership struct {
	ID           int            `json:"id"`
	EnterpriseID int            `json:"enterprise_id"`
	UserID       int            `json:"user_id"`
	Role         EnterpriseRole `json:"role"`
	// SupportEntitlement is set by addEnterpriseSupportEntitlement.
	SupportEntitlement bool      `json:"support_entitlement"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// EnterpriseOrganization binds an organization to the enterprise that owns
// it. An organization belongs to at most one enterprise.
type EnterpriseOrganization struct {
	EnterpriseID int       `json:"enterprise_id"`
	OrgID        int       `json:"org_id"`
	CreatedAt    time.Time `json:"created_at"`
}

// EnterpriseInvitation is an outstanding invitation to an enterprise. Kind
// distinguishes the admin invitation (which carries a role) from the member
// invitation (which does not); one record type keeps their lifecycles aligned.
type EnterpriseInvitation struct {
	ID           int    `json:"id"`
	NodeID       string `json:"node_id"`
	EnterpriseID int    `json:"enterprise_id"`
	// Kind is "admin" or "member".
	Kind string `json:"kind"`
	// InviterID is the enterprise owner who issued the invitation.
	InviterID int `json:"inviter_id"`
	// InviteeID is 0 when the invitation was addressed to an email address
	// that belongs to no account on this instance.
	InviteeID int    `json:"invitee_id"`
	Email     string `json:"email"`
	// Role is meaningful for Kind "admin" only.
	Role      EnterpriseRole `json:"role"`
	CreatedAt time.Time      `json:"created_at"`
}

// IPAllowListEntry is one CIDR on an IP allow list. OwnerType distinguishes
// enterprise from organization owners, whose ids come from different sequences.
type IPAllowListEntry struct {
	ID     int    `json:"id"`
	NodeID string `json:"node_id"`
	// OwnerType is "Enterprise" or "Organization".
	OwnerType      string    `json:"owner_type"`
	OwnerID        int       `json:"owner_id"`
	AllowListValue string    `json:"allow_list_value"`
	Name           string    `json:"name"`
	IsActive       bool      `json:"is_active"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// EnterpriseMembershipKey is the map key for enterprise/user membership.
func EnterpriseMembershipKey(enterpriseID, userID int) string {
	return strconv.Itoa(enterpriseID) + "/" + strconv.Itoa(userID)
}

// persistence

func (st *Store) persistEnterpriseLocked(e *Enterprise) {
	if st.Persist != nil {
		st.Persist.MustPut("enterprises", strconv.Itoa(e.ID), e)
	}
}

func (st *Store) persistEnterpriseMembershipLocked(m *EnterpriseMembership) {
	if st.Persist != nil {
		st.Persist.MustPut("enterprise_memberships", strconv.Itoa(m.ID), m)
	}
}

func (st *Store) persistEnterpriseInvitationLocked(inv *EnterpriseInvitation) {
	if st.Persist != nil {
		st.Persist.MustPut("enterprise_invitations", strconv.Itoa(inv.ID), inv)
	}
}

func (st *Store) persistIPAllowListEntryLocked(entry *IPAllowListEntry) {
	if st.Persist != nil {
		st.Persist.MustPut("ip_allow_list_entries", strconv.Itoa(entry.ID), entry)
	}
}

func enterpriseOrgKey(orgID int) string { return strconv.Itoa(orgID) }

func (st *Store) persistEnterpriseOrganizationLocked(link *EnterpriseOrganization) {
	if st.Persist != nil {
		st.Persist.MustPut("enterprise_organizations", enterpriseOrgKey(link.OrgID), link)
	}
}

// clones (STORE-021)

func cloneEnterprise(e *Enterprise) *Enterprise {
	if e == nil {
		return nil
	}
	c := *e
	c.Policy = e.Policy
	c.Policy.MembersCanCreatePublicRepositories = copyBoolPtr(e.Policy.MembersCanCreatePublicRepositories)
	c.Policy.MembersCanCreatePrivateRepositories = copyBoolPtr(e.Policy.MembersCanCreatePrivateRepositories)
	c.Policy.MembersCanCreateInternalRepositories = copyBoolPtr(e.Policy.MembersCanCreateInternalRepositories)
	if e.IdentityProvider != nil {
		idp := *e.IdentityProvider
		idp.RecoveryCodes = append([]string(nil), e.IdentityProvider.RecoveryCodes...)
		c.IdentityProvider = &idp
	}
	c.MigratorLogins = append([]string(nil), e.MigratorLogins...)
	c.VerifiedDomains = append([]string(nil), e.VerifiedDomains...)
	return &c
}

func copyBoolPtr(v *bool) *bool {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// lifecycle

// CreateEnterprise creates an enterprise account with GitHub's default policy
// set, or returns nil when the slug is taken.
func (st *Store) CreateEnterprise(slug, name, billingEmail string) *Enterprise {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	return cloneEnterprise(st.createEnterpriseLocked(slug, name, billingEmail))
}

// createEnterpriseLocked is the shared create path. Callers hold st.Mu.
func (st *Store) createEnterpriseLocked(slug, name, billingEmail string) *Enterprise {
	key := strings.ToLower(strings.TrimSpace(slug))
	if key == "" {
		return nil
	}
	if _, taken := st.EnterprisesBySlug[key]; taken {
		return nil
	}
	now := st.CurrentTime()
	id := st.NextEnterpriseID
	st.NextEnterpriseID++
	if name == "" {
		name = slug
	}
	e := &Enterprise{
		ID:                      id,
		NodeID:                  fmt.Sprintf("%s%08d", EnterpriseNodeIDPrefix, id),
		Slug:                    key,
		Name:                    name,
		BillingEmail:            billingEmail,
		AvatarURL:               "/enterprises/" + key + "/avatar",
		Policy:                  defaultEnterprisePolicy(),
		BillingBandwidthQuotaGB: DefaultEnterpriseBandwidthQuotaGB,
		BillingStorageQuotaGB:   DefaultEnterpriseStorageQuotaGB,
		BillingTotalLicenses:    DefaultEnterpriseTotalLicenses,
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	st.Enterprises[id] = e
	st.EnterprisesBySlug[key] = e
	st.persistEnterpriseLocked(e)
	return e
}

// Provisioned billing entitlements a new enterprise starts with.
const (
	DefaultEnterpriseBandwidthQuotaGB = 100.0
	DefaultEnterpriseStorageQuotaGB   = 50.0
	DefaultEnterpriseTotalLicenses    = 50
)

// EnsureEnterprise returns the enterprise with the given slug, creating it when
// absent. It brings the instance's own enterprise into being at boot.
func (st *Store) EnsureEnterprise(slug, name, billingEmail string) *Enterprise {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if existing := st.EnterprisesBySlug[strings.ToLower(strings.TrimSpace(slug))]; existing != nil {
		return cloneEnterprise(existing)
	}
	return cloneEnterprise(st.createEnterpriseLocked(slug, name, billingEmail))
}

// GetEnterprise returns a detached snapshot of the enterprise with the given
// slug, or nil.
func (st *Store) GetEnterprise(slug string) *Enterprise {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneEnterprise(st.EnterprisesBySlug[strings.ToLower(strings.TrimSpace(slug))])
}

// GetEnterpriseByID returns a detached snapshot of the enterprise with the
// given database id, or nil.
func (st *Store) GetEnterpriseByID(id int) *Enterprise {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneEnterprise(st.Enterprises[id])
}

// FindEnterpriseByNodeID resolves an enterprise global id to the LIVE row — the
// write path's lookup.
func FindEnterpriseByNodeID(st *Store, nodeID string) *Enterprise {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.findEnterpriseByNodeIDLocked(nodeID)
}

func (st *Store) findEnterpriseByNodeIDLocked(nodeID string) *Enterprise {
	if id, ok := DecodeNodeDBID(nodeID, EnterpriseNodeIDPrefix); ok {
		if e := st.Enterprises[id]; e != nil && e.NodeID == nodeID {
			return e
		}
	}
	for _, e := range st.Enterprises {
		if e.NodeID == nodeID {
			return e
		}
	}
	return nil
}

// ListEnterprises returns detached snapshots of every enterprise, ordered by
// slug.
func (st *Store) ListEnterprises() []*Enterprise {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := make([]*Enterprise, 0, len(st.Enterprises))
	for _, e := range st.Enterprises {
		out = append(out, cloneEnterprise(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// UpdateEnterpriseProfile applies the non-nil profile fields and returns a
// detached snapshot of the result, or nil when the enterprise is gone.
func (st *Store) UpdateEnterpriseProfile(enterpriseID int, name, description, location, websiteURL, securityContactEmail, billingEmail *string) *Enterprise {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	e := st.Enterprises[enterpriseID]
	if e == nil {
		return nil
	}
	if name != nil && strings.TrimSpace(*name) != "" {
		e.Name = *name
	}
	if description != nil {
		e.Description = *description
	}
	if location != nil {
		e.Location = *location
	}
	if websiteURL != nil {
		e.WebsiteURL = *websiteURL
	}
	if securityContactEmail != nil {
		e.SecurityContactEmail = *securityContactEmail
	}
	if billingEmail != nil {
		e.BillingEmail = *billingEmail
	}
	e.UpdatedAt = st.CurrentTime()
	st.persistEnterpriseLocked(e)
	return cloneEnterprise(e)
}

// UpdateEnterprisePolicy mutates the policy set under the store lock and returns
// a detached snapshot. mutate receives the live policy and must not retain it.
func (st *Store) UpdateEnterprisePolicy(enterpriseID int, mutate func(*EnterprisePolicy)) *Enterprise {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	e := st.Enterprises[enterpriseID]
	if e == nil {
		return nil
	}
	mutate(&e.Policy)
	e.UpdatedAt = st.CurrentTime()
	st.persistEnterpriseLocked(e)
	return cloneEnterprise(e)
}

// membership

// SetEnterpriseMembership creates or re-roles a user's membership and returns
// a detached snapshot. It returns nil when the enterprise does not exist.
func (st *Store) SetEnterpriseMembership(enterpriseID, userID int, role EnterpriseRole) *EnterpriseMembership {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	return snapshotEnterpriseMembership(st.setEnterpriseMembershipLocked(enterpriseID, userID, role))
}

func (st *Store) setEnterpriseMembershipLocked(enterpriseID, userID int, role EnterpriseRole) *EnterpriseMembership {
	m := st.applyEnterpriseMembershipLocked(enterpriseID, userID, role)
	if m != nil {
		st.persistEnterpriseMembershipLocked(m)
	}
	return m
}

// applyEnterpriseMembershipLocked installs the membership in memory without
// persisting. AcceptEnterpriseInvitation stages the write in the batch that
// consumes the invitation, so the two cannot diverge across a crash.
func (st *Store) applyEnterpriseMembershipLocked(enterpriseID, userID int, role EnterpriseRole) *EnterpriseMembership {
	if st.Enterprises[enterpriseID] == nil {
		return nil
	}
	now := st.CurrentTime()
	key := EnterpriseMembershipKey(enterpriseID, userID)
	if existing := st.EnterpriseMemberships[key]; existing != nil {
		existing.Role = role
		existing.UpdatedAt = now
		return existing
	}
	m := &EnterpriseMembership{
		ID:           st.NextEnterpriseMembershipID,
		EnterpriseID: enterpriseID,
		UserID:       userID,
		Role:         role,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	st.NextEnterpriseMembershipID++
	st.EnterpriseMemberships[key] = m
	return m
}

func snapshotEnterpriseMembership(m *EnterpriseMembership) *EnterpriseMembership {
	if m == nil {
		return nil
	}
	c := *m
	return &c
}

// GetEnterpriseMembership returns a detached snapshot of a user's membership
// in one enterprise, or nil when the user is not a member of it.
func (st *Store) GetEnterpriseMembership(enterpriseID, userID int) *EnterpriseMembership {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return snapshotEnterpriseMembership(st.EnterpriseMemberships[EnterpriseMembershipKey(enterpriseID, userID)])
}

// RemoveEnterpriseMembership drops a user's membership. It reports whether a
// membership was removed.
func (st *Store) RemoveEnterpriseMembership(enterpriseID, userID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	key := EnterpriseMembershipKey(enterpriseID, userID)
	m := st.EnterpriseMemberships[key]
	if m == nil {
		return false
	}
	delete(st.EnterpriseMemberships, key)
	if st.Persist != nil {
		st.Persist.MustDelete("enterprise_memberships", strconv.Itoa(m.ID))
	}
	return true
}

// ListEnterpriseMemberships returns detached snapshots of every membership in
// one enterprise, ordered by user id.
func (st *Store) ListEnterpriseMemberships(enterpriseID int) []*EnterpriseMembership {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.listEnterpriseMembershipsLocked(enterpriseID)
}

func (st *Store) listEnterpriseMembershipsLocked(enterpriseID int) []*EnterpriseMembership {
	var out []*EnterpriseMembership
	for _, m := range st.EnterpriseMemberships {
		if m.EnterpriseID == enterpriseID {
			out = append(out, snapshotEnterpriseMembership(m))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out
}

// ListEnterprisesForUser returns detached snapshots of every enterprise the
// user belongs to in any role, ordered by slug.
func (st *Store) ListEnterprisesForUser(userID int) []*Enterprise {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*Enterprise
	for _, m := range st.EnterpriseMemberships {
		if m.UserID != userID {
			continue
		}
		if e := st.Enterprises[m.EnterpriseID]; e != nil {
			out = append(out, cloneEnterprise(e))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// EnterpriseRoleOf reports a user's role in an enterprise, or "" when the
// user is not a member.
func (st *Store) EnterpriseRoleOf(enterpriseID, userID int) EnterpriseRole {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if m := st.EnterpriseMemberships[EnterpriseMembershipKey(enterpriseID, userID)]; m != nil {
		return m.Role
	}
	return ""
}

// SetEnterpriseSupportEntitlement grants or revokes a member's support
// entitlement. It reports whether the membership existed.
func (st *Store) SetEnterpriseSupportEntitlement(enterpriseID, userID int, entitled bool) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	m := st.EnterpriseMemberships[EnterpriseMembershipKey(enterpriseID, userID)]
	if m == nil {
		return false
	}
	m.SupportEntitlement = entitled
	m.UpdatedAt = st.CurrentTime()
	st.persistEnterpriseMembershipLocked(m)
	return true
}

// organizations

// AddEnterpriseOrganization binds an organization to an enterprise. It
// returns false when the organization already belongs to a different
// enterprise; re-adding to the same enterprise is a no-op that returns true.
func (st *Store) AddEnterpriseOrganization(enterpriseID, orgID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	return st.addEnterpriseOrganizationLocked(enterpriseID, orgID)
}

func (st *Store) addEnterpriseOrganizationLocked(enterpriseID, orgID int) bool {
	if st.Enterprises[enterpriseID] == nil {
		return false
	}
	if existing := st.EnterpriseOrgs[orgID]; existing != nil {
		return existing.EnterpriseID == enterpriseID
	}
	link := &EnterpriseOrganization{EnterpriseID: enterpriseID, OrgID: orgID, CreatedAt: st.CurrentTime()}
	st.EnterpriseOrgs[orgID] = link
	st.persistEnterpriseOrganizationLocked(link)
	return true
}

// TransferEnterpriseOrganization moves an organization to another
// enterprise. It reports whether the organization was bound to an enterprise
// to begin with.
func (st *Store) TransferEnterpriseOrganization(orgID, destinationEnterpriseID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.Enterprises[destinationEnterpriseID] == nil {
		return false
	}
	link := st.EnterpriseOrgs[orgID]
	if link == nil {
		return false
	}
	link.EnterpriseID = destinationEnterpriseID
	link.CreatedAt = st.CurrentTime()
	st.persistEnterpriseOrganizationLocked(link)
	return true
}

// RemoveEnterpriseOrganization unbinds an organization from its enterprise.
func (st *Store) RemoveEnterpriseOrganization(enterpriseID, orgID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	link := st.EnterpriseOrgs[orgID]
	if link == nil || link.EnterpriseID != enterpriseID {
		return false
	}
	delete(st.EnterpriseOrgs, orgID)
	if st.Persist != nil {
		st.Persist.MustDelete("enterprise_organizations", enterpriseOrgKey(orgID))
	}
	return true
}

// EnterpriseIDForOrg reports the enterprise an organization belongs to, or 0.
func (st *Store) EnterpriseIDForOrg(orgID int) int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.enterpriseIDForOrgLocked(orgID)
}

func (st *Store) enterpriseIDForOrgLocked(orgID int) int {
	if link := st.EnterpriseOrgs[orgID]; link != nil {
		return link.EnterpriseID
	}
	return 0
}

// ListEnterpriseOrgIDs returns the ids of the organizations in an
// enterprise, ascending.
func (st *Store) ListEnterpriseOrgIDs(enterpriseID int) []int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.listEnterpriseOrgIDsLocked(enterpriseID)
}

func (st *Store) listEnterpriseOrgIDsLocked(enterpriseID int) []int {
	var out []int
	for orgID, link := range st.EnterpriseOrgs {
		if link.EnterpriseID == enterpriseID {
			out = append(out, orgID)
		}
	}
	sort.Ints(out)
	return out
}

// invitations

// CreateEnterpriseInvitation records an invitation ("admin" or "member"; role
// applies to "admin" only) and returns a detached snapshot, or nil when the
// enterprise is gone or an equivalent invitation is already outstanding.
func (st *Store) CreateEnterpriseInvitation(enterpriseID, inviterID, inviteeID int, email, kind string, role EnterpriseRole) *EnterpriseInvitation {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.Enterprises[enterpriseID] == nil {
		return nil
	}
	for _, inv := range st.EnterpriseInvitations {
		if inv.EnterpriseID != enterpriseID || inv.Kind != kind {
			continue
		}
		if inviteeID != 0 && inv.InviteeID == inviteeID {
			return nil
		}
		if inviteeID == 0 && email != "" && strings.EqualFold(inv.Email, email) {
			return nil
		}
	}
	id := st.NextEnterpriseInvitationID
	st.NextEnterpriseInvitationID++
	prefix := EnterpriseMemberInvitationNodeIDPrefix
	if kind == "admin" {
		prefix = EnterpriseAdminInvitationNodeIDPrefix
	}
	inv := &EnterpriseInvitation{
		ID:           id,
		NodeID:       fmt.Sprintf("%s%08d", prefix, id),
		EnterpriseID: enterpriseID,
		Kind:         kind,
		InviterID:    inviterID,
		InviteeID:    inviteeID,
		Email:        email,
		Role:         role,
		CreatedAt:    st.CurrentTime(),
	}
	st.EnterpriseInvitations[id] = inv
	st.persistEnterpriseInvitationLocked(inv)
	return snapshotEnterpriseInvitation(inv)
}

func snapshotEnterpriseInvitation(inv *EnterpriseInvitation) *EnterpriseInvitation {
	if inv == nil {
		return nil
	}
	c := *inv
	return &c
}

// FindEnterpriseInvitationByNodeID resolves an invitation global id to the
// LIVE row.
func FindEnterpriseInvitationByNodeID(st *Store, nodeID string) *EnterpriseInvitation {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, prefix := range []string{EnterpriseAdminInvitationNodeIDPrefix, EnterpriseMemberInvitationNodeIDPrefix} {
		if id, ok := DecodeNodeDBID(nodeID, prefix); ok {
			if inv := st.EnterpriseInvitations[id]; inv != nil && inv.NodeID == nodeID {
				return inv
			}
		}
	}
	return nil
}

// GetEnterpriseInvitation returns a detached snapshot by database id.
func (st *Store) GetEnterpriseInvitation(id int) *EnterpriseInvitation {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return snapshotEnterpriseInvitation(st.EnterpriseInvitations[id])
}

// ListEnterpriseInvitations returns detached snapshots of the outstanding
// invitations of one kind for one enterprise, oldest first.
func (st *Store) ListEnterpriseInvitations(enterpriseID int, kind string) []*EnterpriseInvitation {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*EnterpriseInvitation
	for _, inv := range st.EnterpriseInvitations {
		if inv.EnterpriseID == enterpriseID && inv.Kind == kind {
			out = append(out, snapshotEnterpriseInvitation(inv))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// DeleteEnterpriseInvitation removes an invitation and reports whether one
// was removed.
func (st *Store) DeleteEnterpriseInvitation(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	return st.deleteEnterpriseInvitationLocked(id)
}

func (st *Store) deleteEnterpriseInvitationLocked(id int) bool {
	if st.EnterpriseInvitations[id] == nil {
		return false
	}
	delete(st.EnterpriseInvitations, id)
	if st.Persist != nil {
		st.Persist.MustDelete("enterprise_invitations", strconv.Itoa(id))
	}
	return true
}

// AcceptEnterpriseInvitation consumes an invitation and installs its membership
// in one batch write, so an accepted invitation cannot survive a crash without
// its membership. Returns the consumed snapshot, or nil when none exists.
func (st *Store) AcceptEnterpriseInvitation(id, userID int) *EnterpriseInvitation {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	inv := st.EnterpriseInvitations[id]
	if inv == nil {
		return nil
	}
	role := EnterpriseRoleMember
	if inv.Kind == "admin" {
		role = inv.Role
		if role == "" {
			role = EnterpriseRoleOwner
		}
	}
	accepted := snapshotEnterpriseInvitation(inv)
	membership := st.applyEnterpriseMembershipLocked(inv.EnterpriseID, userID, role)
	delete(st.EnterpriseInvitations, id)
	if st.Persist != nil {
		batch := NewPersistBatch(st.Persist)
		batch.Delete("enterprise_invitations", strconv.Itoa(id))
		if membership != nil {
			batch.Put("enterprise_memberships", strconv.Itoa(membership.ID), membership)
		}
		if err := batch.Commit(); err != nil {
			panic(&PersistenceFailure{Op: "batch", Bucket: "enterprise_invitations", Err: err})
		}
	}
	return accepted
}

// migrator role

// SetEnterpriseMigratorRole grants or revokes the organizations-migrator role
// for a login across the enterprise. It reports whether the enterprise exists.
func (st *Store) SetEnterpriseMigratorRole(enterpriseID int, login string, granted bool) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	e := st.Enterprises[enterpriseID]
	if e == nil {
		return false
	}
	kept := e.MigratorLogins[:0:0]
	for _, existing := range e.MigratorLogins {
		if !strings.EqualFold(existing, login) {
			kept = append(kept, existing)
		}
	}
	if granted {
		kept = append(kept, login)
	}
	sort.Strings(kept)
	e.MigratorLogins = kept
	e.UpdatedAt = st.CurrentTime()
	st.persistEnterpriseLocked(e)
	return true
}

// identity provider

// SetEnterpriseIdentityProvider binds (or rebinds) an enterprise's SAML identity
// provider and returns a detached snapshot. The caller generates recoveryCodes
// so the store stays free of randomness.
func (st *Store) SetEnterpriseIdentityProvider(enterpriseID int, ssoURL, issuer, certificate, signatureMethod, digestMethod string, recoveryCodes []string) *EnterpriseSAMLIdentityProvider {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	e := st.Enterprises[enterpriseID]
	if e == nil {
		return nil
	}
	now := st.CurrentTime()
	idp := e.IdentityProvider
	if idp == nil {
		idp = &EnterpriseSAMLIdentityProvider{
			EnterpriseID:  enterpriseID,
			NodeID:        fmt.Sprintf("EIDP_kgDO%08d", enterpriseID),
			RecoveryCodes: recoveryCodes,
			CreatedAt:     now,
		}
		e.IdentityProvider = idp
	}
	idp.SSOURL = ssoURL
	idp.Issuer = issuer
	idp.IDPCertificate = certificate
	idp.SignatureMethod = signatureMethod
	idp.DigestMethod = digestMethod
	idp.UpdatedAt = now
	e.UpdatedAt = now
	st.persistEnterpriseLocked(e)
	return cloneEnterprise(e).IdentityProvider
}

// RegenerateEnterpriseIdentityProviderRecoveryCodes replaces the binding's
// recovery codes and returns a detached snapshot, or nil when the enterprise
// has no identity provider.
func (st *Store) RegenerateEnterpriseIdentityProviderRecoveryCodes(enterpriseID int, codes []string) *EnterpriseSAMLIdentityProvider {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	e := st.Enterprises[enterpriseID]
	if e == nil || e.IdentityProvider == nil {
		return nil
	}
	e.IdentityProvider.RecoveryCodes = codes
	e.IdentityProvider.UpdatedAt = st.CurrentTime()
	e.UpdatedAt = e.IdentityProvider.UpdatedAt
	st.persistEnterpriseLocked(e)
	return cloneEnterprise(e).IdentityProvider
}

// RemoveEnterpriseIdentityProvider clears the binding and returns a detached
// snapshot of what was removed, or nil when there was none.
func (st *Store) RemoveEnterpriseIdentityProvider(enterpriseID int) *EnterpriseSAMLIdentityProvider {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	e := st.Enterprises[enterpriseID]
	if e == nil || e.IdentityProvider == nil {
		return nil
	}
	removed := cloneEnterprise(e).IdentityProvider
	e.IdentityProvider = nil
	e.UpdatedAt = st.CurrentTime()
	st.persistEnterpriseLocked(e)
	return removed
}

// IP allow list

// CreateIPAllowListEntry appends an entry to an owner's allow list.
func (st *Store) CreateIPAllowListEntry(ownerType string, ownerID int, allowListValue, name string, isActive bool) *IPAllowListEntry {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	now := st.CurrentTime()
	id := st.NextIPAllowListEntryID
	st.NextIPAllowListEntryID++
	entry := &IPAllowListEntry{
		ID:             id,
		NodeID:         fmt.Sprintf("%s%08d", IPAllowListEntryNodeIDPrefix, id),
		OwnerType:      ownerType,
		OwnerID:        ownerID,
		AllowListValue: allowListValue,
		Name:           name,
		IsActive:       isActive,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	st.IPAllowListEntries[id] = entry
	st.persistIPAllowListEntryLocked(entry)
	return snapshotIPAllowListEntry(entry)
}

func snapshotIPAllowListEntry(e *IPAllowListEntry) *IPAllowListEntry {
	if e == nil {
		return nil
	}
	c := *e
	return &c
}

// FindIPAllowListEntryByNodeID resolves an allow-list entry global id to the
// LIVE row.
func FindIPAllowListEntryByNodeID(st *Store, nodeID string) *IPAllowListEntry {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, IPAllowListEntryNodeIDPrefix); ok {
		if e := st.IPAllowListEntries[id]; e != nil && e.NodeID == nodeID {
			return e
		}
	}
	return nil
}

// UpdateIPAllowListEntry rewrites an entry and returns a detached snapshot.
func (st *Store) UpdateIPAllowListEntry(id int, allowListValue, name string, isActive bool) *IPAllowListEntry {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	entry := st.IPAllowListEntries[id]
	if entry == nil {
		return nil
	}
	entry.AllowListValue = allowListValue
	entry.Name = name
	entry.IsActive = isActive
	entry.UpdatedAt = st.CurrentTime()
	st.persistIPAllowListEntryLocked(entry)
	return snapshotIPAllowListEntry(entry)
}

// DeleteIPAllowListEntry removes an entry and returns a detached snapshot of
// what was removed.
func (st *Store) DeleteIPAllowListEntry(id int) *IPAllowListEntry {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	entry := st.IPAllowListEntries[id]
	if entry == nil {
		return nil
	}
	removed := snapshotIPAllowListEntry(entry)
	delete(st.IPAllowListEntries, id)
	if st.Persist != nil {
		st.Persist.MustDelete("ip_allow_list_entries", strconv.Itoa(id))
	}
	return removed
}

// ListIPAllowListEntries returns detached snapshots of one owner's entries,
// ordered by database id.
func (st *Store) ListIPAllowListEntries(ownerType string, ownerID int) []*IPAllowListEntry {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*IPAllowListEntry
	for _, entry := range st.IPAllowListEntries {
		if entry.OwnerType == ownerType && entry.OwnerID == ownerID {
			out = append(out, snapshotIPAllowListEntry(entry))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// loadEnterpriseAccountBuckets restores the enterprise account layer from
// persistence. It runs during store construction, before the store is
// reachable, so it takes no lock.
func (st *Store) loadEnterpriseAccountBuckets() error {
	if err := st.loadBucket("enterprises", func(raw []byte) error {
		var e Enterprise
		if err := LoadJSON(raw, &e); err != nil {
			return err
		}
		if e.Policy.AllowPrivateRepositoryForking == "" {
			// An older persisted row reads back with Go zero values that are not
			// enum members; fill from the defaults so every field is answerable.
			e.Policy = mergeEnterprisePolicyDefaults(e.Policy)
		}
		st.Enterprises[e.ID] = &e
		st.EnterprisesBySlug[strings.ToLower(e.Slug)] = &e
		if e.ID >= st.NextEnterpriseID {
			st.NextEnterpriseID = e.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("enterprise_memberships", func(raw []byte) error {
		var m EnterpriseMembership
		if err := LoadJSON(raw, &m); err != nil {
			return err
		}
		st.EnterpriseMemberships[EnterpriseMembershipKey(m.EnterpriseID, m.UserID)] = &m
		if m.ID >= st.NextEnterpriseMembershipID {
			st.NextEnterpriseMembershipID = m.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("enterprise_organizations", func(raw []byte) error {
		var link EnterpriseOrganization
		if err := LoadJSON(raw, &link); err != nil {
			return err
		}
		st.EnterpriseOrgs[link.OrgID] = &link
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("enterprise_invitations", func(raw []byte) error {
		var inv EnterpriseInvitation
		if err := LoadJSON(raw, &inv); err != nil {
			return err
		}
		st.EnterpriseInvitations[inv.ID] = &inv
		if inv.ID >= st.NextEnterpriseInvitationID {
			st.NextEnterpriseInvitationID = inv.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	if err := st.loadBucket("ip_allow_list_entries", func(raw []byte) error {
		var entry IPAllowListEntry
		if err := LoadJSON(raw, &entry); err != nil {
			return err
		}
		st.IPAllowListEntries[entry.ID] = &entry
		if entry.ID >= st.NextIPAllowListEntryID {
			st.NextIPAllowListEntryID = entry.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	return st.loadBucket("verifiable_domains", func(raw []byte) error {
		var row VerifiableDomain
		if err := LoadJSON(raw, &row); err != nil {
			return err
		}
		st.VerifiableDomains[row.ID] = &row
		if row.ID >= st.NextVerifiableDomainID {
			st.NextVerifiableDomainID = row.ID + 1
		}
		return nil
	})
}

// mergeEnterprisePolicyDefaults fills any empty policy field with GitHub's
// default, so a non-null GraphQL field always has an enum member to serve.
func mergeEnterprisePolicyDefaults(p EnterprisePolicy) EnterprisePolicy {
	defaults := defaultEnterprisePolicy()
	fill := func(dst *string, def string) {
		if *dst == "" {
			*dst = def
		}
	}
	fill(&p.AllowPrivateRepositoryForking, defaults.AllowPrivateRepositoryForking)
	fill(&p.DefaultRepositoryPermission, defaults.DefaultRepositoryPermission)
	fill(&p.MembersCanChangeRepositoryVisibility, defaults.MembersCanChangeRepositoryVisibility)
	fill(&p.MembersCanCreateRepositories, defaults.MembersCanCreateRepositories)
	fill(&p.MembersCanDeleteIssues, defaults.MembersCanDeleteIssues)
	fill(&p.MembersCanDeleteRepositories, defaults.MembersCanDeleteRepositories)
	fill(&p.MembersCanInviteCollaborators, defaults.MembersCanInviteCollaborators)
	fill(&p.MembersCanMakePurchases, defaults.MembersCanMakePurchases)
	fill(&p.MembersCanUpdateProtectedBranches, defaults.MembersCanUpdateProtectedBranches)
	fill(&p.MembersCanViewDependencyInsights, defaults.MembersCanViewDependencyInsights)
	fill(&p.OrganizationProjects, defaults.OrganizationProjects)
	fill(&p.RepositoryProjects, defaults.RepositoryProjects)
	fill(&p.RepositoryDeployKey, defaults.RepositoryDeployKey)
	fill(&p.TeamDiscussions, defaults.TeamDiscussions)
	fill(&p.TwoFactorRequired, defaults.TwoFactorRequired)
	fill(&p.TwoFactorDisallowedMethods, defaults.TwoFactorDisallowedMethods)
	fill(&p.ProofOfPresenceRequired, defaults.ProofOfPresenceRequired)
	fill(&p.NotificationDeliveryRestrictionEnabled, defaults.NotificationDeliveryRestrictionEnabled)
	fill(&p.IPAllowListEnabled, defaults.IPAllowListEnabled)
	fill(&p.IPAllowListForInstalledAppsEnabled, defaults.IPAllowListForInstalledAppsEnabled)
	fill(&p.IPAllowListUserLevelEnforcementEnabled, defaults.IPAllowListUserLevelEnforcementEnabled)
	return p
}

// PrimaryEnterpriseSlug names the instance's own enterprise account, configured
// via BLEEPHUB_ENTERPRISE_SLUG and set once at boot. It is the enterprise every
// account on the instance belongs to.
func (st *Store) PrimaryEnterpriseSlug() string {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.primaryEnterpriseSlug
}

// SetPrimaryEnterpriseSlug records which enterprise is the instance's own.
func (st *Store) SetPrimaryEnterpriseSlug(slug string) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	st.primaryEnterpriseSlug = strings.ToLower(strings.TrimSpace(slug))
}

// EffectiveEnterpriseRole reports the role a user holds in an enterprise. An
// explicit membership row wins; failing that, two enterprise-scoped derivations
// apply:
//
//   - In the instance's own (GHES) enterprise, every account is a member and
//     every site administrator is an owner.
//   - In any enterprise, a member of one of its organizations is a member, and
//     an owner of one is an owner.
//
// A user with neither an explicit membership nor an organization in the
// enterprise holds no role, keeping one enterprise's people out of another's.
func (st *Store) EffectiveEnterpriseRole(enterpriseID int, user *User) EnterpriseRole {
	if user == nil {
		return ""
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	e := st.Enterprises[enterpriseID]
	if e == nil {
		return ""
	}
	if m := st.EnterpriseMemberships[EnterpriseMembershipKey(enterpriseID, user.ID)]; m != nil {
		return m.Role
	}
	if st.primaryEnterpriseSlug != "" && e.Slug == st.primaryEnterpriseSlug {
		if user.SiteAdmin {
			return EnterpriseRoleOwner
		}
		return EnterpriseRoleMember
	}
	for orgID, link := range st.EnterpriseOrgs {
		if link.EnterpriseID != enterpriseID {
			continue
		}
		org := st.Orgs[orgID]
		if org == nil {
			continue
		}
		m := st.Memberships[MembershipKey(org.Login, user.ID)]
		if m == nil || m.State != MembershipStateActive {
			continue
		}
		if m.Role == OrgRoleAdmin {
			return EnterpriseRoleOwner
		}
		return EnterpriseRoleMember
	}
	return ""
}

// IsEnterpriseOwner, IsEnterpriseMember and IsEnterpriseBillingReader are the
// three standing questions every enterprise read and write is authorized against.
func (st *Store) IsEnterpriseOwner(enterpriseID int, user *User) bool {
	return st.EffectiveEnterpriseRole(enterpriseID, user) == EnterpriseRoleOwner
}

func (st *Store) IsEnterpriseMember(enterpriseID int, user *User) bool {
	switch st.EffectiveEnterpriseRole(enterpriseID, user) {
	case EnterpriseRoleOwner, EnterpriseRoleBillingManager, EnterpriseRoleMember:
		return true
	}
	return false
}

func (st *Store) IsEnterpriseBillingReader(enterpriseID int, user *User) bool {
	switch st.EffectiveEnterpriseRole(enterpriseID, user) {
	case EnterpriseRoleOwner, EnterpriseRoleBillingManager:
		return true
	}
	return false
}

// billing measurement
//
// Storage and bandwidth are measured from what the organizations actually hold
// and served, so EnterpriseBillingInfo reports this instance, not a constant.

// EnterpriseStorageBytes sums the release-asset and package-file bytes held by
// repositories owned by the given organizations.
func (st *Store) EnterpriseStorageBytes(orgIDs map[int]bool) int64 {
	repoIDs, packageOwners := st.enterpriseOwnedRepos(orgIDs)
	var total int64
	st.Releases.Mu.RLock()
	for repoID := range repoIDs {
		for _, release := range st.Releases.ByRepo[repoID] {
			for _, asset := range release.Assets {
				total += int64(asset.Size)
			}
		}
	}
	st.Releases.Mu.RUnlock()

	st.Mu.RLock()
	defer st.Mu.RUnlock()
	versionsByPackage := map[int]bool{}
	for _, pkg := range st.Packages {
		if pkg.Deleted || !packageOwners[strings.ToLower(pkg.OwnerKey)] {
			continue
		}
		versionsByPackage[pkg.ID] = true
	}
	includedVersions := map[int]bool{}
	for _, version := range st.PackageVersions {
		if versionsByPackage[version.PackageID] {
			includedVersions[version.ID] = true
		}
	}
	for _, file := range st.PackageFiles {
		if includedVersions[file.VersionID] {
			total += file.Size
		}
	}
	return total
}

// EnterpriseBandwidthBytes sums the bytes the enterprise's repositories served:
// each release asset's size times its download count.
func (st *Store) EnterpriseBandwidthBytes(orgIDs map[int]bool) int64 {
	repoIDs, _ := st.enterpriseOwnedRepos(orgIDs)
	var total int64
	st.Releases.Mu.RLock()
	defer st.Releases.Mu.RUnlock()
	for repoID := range repoIDs {
		for _, release := range st.Releases.ByRepo[repoID] {
			for _, asset := range release.Assets {
				total += int64(asset.Size) * int64(asset.DownloadCount)
			}
		}
	}
	return total
}

// enterpriseOwnedRepos returns the ids of the repositories owned by the given
// organizations, and the lower-cased package owner keys (org logins and
// "owner/repo" names) those organizations answer for.
func (st *Store) enterpriseOwnedRepos(orgIDs map[int]bool) (map[int]bool, map[string]bool) {
	repoIDs := map[int]bool{}
	owners := map[string]bool{}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for orgID := range orgIDs {
		if org := st.Orgs[orgID]; org != nil {
			owners[strings.ToLower(org.Login)] = true
		}
	}
	for _, repo := range st.Repos {
		if repo.OwnerType != "Organization" || !orgIDs[repo.OwnerID] {
			continue
		}
		repoIDs[repo.ID] = true
		owners[strings.ToLower(repo.FullName)] = true
	}
	return repoIDs, owners
}

// MaxAuditLogEntries bounds the in-memory audit log; an uncapped prepend-only
// slice would grow without limit and make each write O(n).
const MaxAuditLogEntries = 5000

// RecordAuditEntry appends an audit-log entry and returns it. Both the REST
// handlers and GraphQL resolvers write through it, so either surface's actions
// land in the same log with the same shape.
func (st *Store) RecordAuditEntry(action, actor, org string, data map[string]interface{}) *AuditEntry {
	timestamp := st.CurrentTime().Format(time.RFC3339Nano)
	st.Misc.Mu.Lock()
	defer st.Misc.Mu.Unlock()
	st.Misc.NextAuditID++
	entry := &AuditEntry{
		ID:        st.Misc.NextAuditID,
		Timestamp: timestamp,
		Action:    action,
		Actor:     actor,
		Org:       org,
		Data:      data,
		Version:   "1.1",
	}
	st.Misc.AuditLog = append([]*AuditEntry{entry}, st.Misc.AuditLog...)
	if len(st.Misc.AuditLog) > MaxAuditLogEntries {
		st.Misc.AuditLog = st.Misc.AuditLog[:MaxAuditLogEntries]
	}
	if st.Misc.Persist != nil {
		st.Misc.Persist.MustPut("audit_log", strconv.FormatInt(entry.ID, 10), entry)
	}
	return entry
}

// policy resolution

// EnterprisePolicyForOrg returns the policy governing an organization — that of
// the enterprise that owns it, or the instance's own enterprise for an unclaimed
// organization. The second return names the source enterprise so a caller can
// exempt its owners.
func (st *Store) EnterprisePolicyForOrg(orgID int) (EnterprisePolicy, *Enterprise) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.enterprisePolicyForOrgLocked(orgID)
}

func (st *Store) enterprisePolicyForOrgLocked(orgID int) (EnterprisePolicy, *Enterprise) {
	if link := st.EnterpriseOrgs[orgID]; link != nil {
		if e := st.Enterprises[link.EnterpriseID]; e != nil {
			return e.Policy, cloneEnterprise(e)
		}
	}
	if e := st.EnterprisesBySlug[st.primaryEnterpriseSlug]; e != nil {
		return e.Policy, cloneEnterprise(e)
	}
	return EnterprisePolicy{}, nil
}

// EnterprisePolicyForRepo returns the policy governing a repository through its
// owning organization; a user-owned repo is governed by the instance's own
// enterprise.
func (st *Store) EnterprisePolicyForRepo(repo *Repo) (EnterprisePolicy, *Enterprise) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if repo != nil && repo.OwnerType == "Organization" {
		return st.enterprisePolicyForOrgLocked(repo.OwnerID)
	}
	if e := st.EnterprisesBySlug[st.primaryEnterpriseSlug]; e != nil {
		return e.Policy, cloneEnterprise(e)
	}
	return EnterprisePolicy{}, nil
}

// EnterprisePolicyForbids reports whether a DISABLED policy blocks user. Blank
// and NO_POLICY impose nothing, ENABLED permits, and DISABLED blocks everyone
// but an owner of the enterprise that imposed it.
func (st *Store) EnterprisePolicyForbids(e *Enterprise, setting string, user *User) bool {
	if setting != EnterprisePolicyDisabled {
		return false
	}
	if e == nil {
		return true
	}
	return !st.IsEnterpriseOwner(e.ID, user)
}

// enterpriseClampedBasePermissionLocked returns the base repository permission
// an organization's members actually hold: the org's own setting, capped by the
// enterprise's default-repository-permission policy. Read on the access path, so
// tightening the ceiling takes effect on the next request. Callers hold st.Mu.
func (st *Store) enterpriseClampedBasePermissionLocked(org *Org) string {
	if org == nil {
		return ""
	}
	policy, _ := st.enterprisePolicyForOrgLocked(org.ID)
	ceiling, imposed := enterpriseBasePermissionCeiling(policy)
	if !imposed {
		return org.DefaultRepositoryPermission
	}
	if basePermissionRank(org.DefaultRepositoryPermission) > basePermissionRank(ceiling) {
		return ceiling
	}
	return org.DefaultRepositoryPermission
}

// enterpriseBasePermissionCeiling maps the policy enum onto the REST permission
// spelling, and reports whether a ceiling is imposed at all.
func enterpriseBasePermissionCeiling(policy EnterprisePolicy) (string, bool) {
	switch policy.DefaultRepositoryPermission {
	case "NONE":
		return "none", true
	case "READ":
		return "read", true
	case "WRITE":
		return "write", true
	case "ADMIN":
		return "admin", true
	}
	return "", false
}

// basePermissionRank orders the base repository permissions for comparison. An
// unset organization default ranks as "read".
func basePermissionRank(permission string) int {
	switch strings.ToLower(permission) {
	case "admin":
		return 3
	case "write", "push":
		return 2
	case "", "read", "pull":
		return 1
	}
	return 0
}

// EnterpriseClampedBasePermission is the exported base-permission clamp. The
// organization settings response reports this rather than the org's own setting,
// so what the API reports matches what the access check grants.
func (st *Store) EnterpriseClampedBasePermission(org *Org) string {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.enterpriseClampedBasePermissionLocked(org)
}

// ActiveEnterpriseIPAllowList returns the enterprise's active allow-list entries
// when its IP allow list is on, else nil. Keeps the per-request gate to one read
// lock and no allocation when the feature is off.
func (st *Store) ActiveEnterpriseIPAllowList() (values []string, forInstalledApps bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	e := st.EnterprisesBySlug[st.primaryEnterpriseSlug]
	if e == nil || e.Policy.IPAllowListEnabled != EnterprisePolicyEnabled {
		return nil, false
	}
	for _, entry := range st.IPAllowListEntries {
		if entry.IsActive && entry.OwnerType == IPAllowListOwnerEnterprise && entry.OwnerID == e.ID {
			values = append(values, entry.AllowListValue)
		}
	}
	return values, e.Policy.IPAllowListForInstalledAppsEnabled == EnterprisePolicyEnabled
}

// ActiveUserIPAllowList returns the active entries of one account's own IP allow
// list when the enterprise has turned user-level enforcement on, else nil. The
// enterprise decides whether it is enforced; the account decides its contents.
func (st *Store) ActiveUserIPAllowList(userID int) []string {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	e := st.EnterprisesBySlug[st.primaryEnterpriseSlug]
	if e == nil || e.Policy.IPAllowListUserLevelEnforcementEnabled != EnterprisePolicyEnabled {
		return nil
	}
	var values []string
	for _, entry := range st.IPAllowListEntries {
		if entry.IsActive && entry.OwnerType == IPAllowListOwnerUser && entry.OwnerID == userID {
			values = append(values, entry.AllowListValue)
		}
	}
	return values
}

// verified domains

// NormalizeVerifiedDomain reduces a domain to the stored form: lower case, no
// surrounding space, no leading "@", no trailing dot. A non-domain reduces to "".
func NormalizeVerifiedDomain(domain string) string {
	cleaned := strings.ToLower(strings.TrimSpace(domain))
	cleaned = strings.TrimPrefix(cleaned, "@")
	cleaned = strings.Trim(cleaned, ".")
	if cleaned == "" || strings.ContainsAny(cleaned, " \t/:") || !strings.Contains(cleaned, ".") {
		return ""
	}
	return cleaned
}

// SetEnterpriseVerifiedDomains replaces the verified domain list and returns a
// detached snapshot, or nil when the enterprise is gone. Non-domains are dropped
// so the notification-delivery check never compares against junk.
func (st *Store) SetEnterpriseVerifiedDomains(enterpriseID int, domains []string) *Enterprise {
	normalized := make([]string, 0, len(domains))
	seen := map[string]bool{}
	for _, domain := range domains {
		cleaned := NormalizeVerifiedDomain(domain)
		if cleaned == "" || seen[cleaned] {
			continue
		}
		seen[cleaned] = true
		normalized = append(normalized, cleaned)
	}
	sort.Strings(normalized)
	st.Mu.Lock()
	defer st.Mu.Unlock()
	e := st.Enterprises[enterpriseID]
	if e == nil {
		return nil
	}
	e.VerifiedDomains = normalized
	e.UpdatedAt = st.CurrentTime()
	st.persistEnterpriseLocked(e)
	// Keep the VerifiableDomain rows the GraphQL surface serves in sync with the
	// flat list; both are views of one fact.
	st.reconcileEnterpriseDomainRowsLocked(e.ID, normalized)
	return cloneEnterprise(e)
}

// EmailInVerifiedDomain reports whether an address's domain is one of the
// verified domains or a subdomain of one — an approved domain covers the hosts
// beneath it.
func EmailInVerifiedDomain(email string, domains []string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	host := NormalizeVerifiedDomain(email[at+1:])
	if host == "" {
		return false
	}
	for _, domain := range domains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// NotificationDeliveryRestriction reports whether the instance's enterprise
// restricts notification delivery to its verified domains, and which those are.
// A restriction with no verified domain restricts everything, as on GitHub.
func (st *Store) NotificationDeliveryRestriction() (bool, []string) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	e := st.EnterprisesBySlug[st.primaryEnterpriseSlug]
	if e == nil || e.Policy.NotificationDeliveryRestrictionEnabled != EnterprisePolicyEnabled {
		return false, nil
	}
	return true, append([]string(nil), e.VerifiedDomains...)
}

// ListIPAllowListEntryByID returns a detached snapshot of one entry by
// database id, or nil.
func (st *Store) ListIPAllowListEntryByID(id int) *IPAllowListEntry {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return snapshotIPAllowListEntry(st.IPAllowListEntries[id])
}

// UserNamespaceAccessGrant is an enterprise owner's temporary access to a
// user-namespace repository of a managed account. It admits its holder wherever
// a collaborator grant would, and nowhere else.
type UserNamespaceAccessGrant struct {
	ID           int       `json:"id"`
	EnterpriseID int       `json:"enterprise_id"`
	RepoID       int       `json:"repo_id"`
	GranteeID    int       `json:"grantee_id"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// GrantUserNamespaceAccess records the grant and returns its expiry. The window
// is GitHub's two hours.
func (st *Store) GrantUserNamespaceAccess(enterpriseID, repoID, granteeID int) time.Time {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	expires := st.CurrentTime().Add(2 * time.Hour)
	grant := &UserNamespaceAccessGrant{
		ID:           st.NextUserNamespaceGrantID,
		EnterpriseID: enterpriseID,
		RepoID:       repoID,
		GranteeID:    granteeID,
		ExpiresAt:    expires,
	}
	st.NextUserNamespaceGrantID++
	st.UserNamespaceGrants[grant.ID] = grant
	if st.Persist != nil {
		st.Persist.MustPut("user_namespace_grants", strconv.Itoa(grant.ID), grant)
	}
	return expires
}

// userNamespaceGrantAdmitsLocked reports whether an unexpired grant admits
// user to repo. Callers hold st.Mu.
func userNamespaceGrantAdmitsLocked(st *Store, user *User, repo *Repo) bool {
	if user == nil || repo == nil {
		return false
	}
	now := st.CurrentTime()
	for _, grant := range st.UserNamespaceGrants {
		if grant.RepoID == repo.ID && grant.GranteeID == user.ID && now.Before(grant.ExpiresAt) {
			return true
		}
	}
	return false
}

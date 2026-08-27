package store

import (
	"testing"
	"time"
)

// newEnterpriseTestUser adds an account directly. User creation lives in the
// HTTP layer, which the store package may not import, so the tests here build
// the row the way the seed path does.
func newEnterpriseTestUser(st *Store, login string) *User {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	now := st.CurrentTime()
	u := &User{
		ID:           st.ReserveGlobalID("next_user", &st.NextUser),
		Login:        login,
		Name:         login,
		Email:        login + "@example.test",
		Type:         "User",
		StarredRepos: map[string]time.Time{},
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	u.NodeID = "U_kgDO" + padNodeTestDigits(u.ID)
	st.Users[u.ID] = u
	st.UsersByLogin[u.Login] = u
	st.IndexUserLoginLocked(u.Login)
	return u
}

func padNodeTestDigits(n int) string {
	digits := []byte("00000000")
	for i := len(digits) - 1; i >= 0 && n > 0; i-- {
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits)
}

// newEnterpriseTestStore builds a store with a fixed clock so no test reads
// the wall clock.
func newEnterpriseTestStore(t *testing.T) *Store {
	t.Helper()
	st := NewStore()
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	st.ClockNow = func() time.Time { return at }
	return st
}

func TestCreateEnterpriseStartsFromGitHubsDefaultPolicy(t *testing.T) {
	st := newEnterpriseTestStore(t)
	e := st.CreateEnterprise("Acme", "Acme Inc", "billing@acme.test")
	if e == nil {
		t.Fatal("CreateEnterprise returned nil")
	}
	if e.Slug != "acme" {
		t.Errorf("slug = %q, want the lower-cased form", e.Slug)
	}
	if e.NodeID != "E_kgDO00000001" {
		t.Errorf("node id = %q", e.NodeID)
	}
	// Every non-null GraphQL policy field must have a value to serve from the
	// moment the enterprise exists.
	for name, got := range map[string]string{
		"AllowPrivateRepositoryForking":          e.Policy.AllowPrivateRepositoryForking,
		"DefaultRepositoryPermission":            e.Policy.DefaultRepositoryPermission,
		"MembersCanDeleteRepositories":           e.Policy.MembersCanDeleteRepositories,
		"TwoFactorRequired":                      e.Policy.TwoFactorRequired,
		"TeamDiscussions":                        e.Policy.TeamDiscussions,
		"NotificationDeliveryRestrictionEnabled": e.Policy.NotificationDeliveryRestrictionEnabled,
		"IPAllowListEnabled":                     e.Policy.IPAllowListEnabled,
	} {
		if got == "" {
			t.Errorf("policy %s is empty; a non-null GraphQL enum field has nothing to serve", name)
		}
	}
	if e.Policy.MembersCanMakePurchases != EnterprisePolicyEnabled {
		t.Errorf("MembersCanMakePurchases = %q, want ENABLED", e.Policy.MembersCanMakePurchases)
	}
	if st.CreateEnterprise("ACME", "Duplicate", "") != nil {
		t.Error("a second enterprise took the same slug")
	}
}

func TestEnterpriseGettersReturnDetachedSnapshots(t *testing.T) {
	st := newEnterpriseTestStore(t)
	created := st.CreateEnterprise("acme", "Acme", "")
	snapshot := st.GetEnterprise("acme")
	snapshot.Name = "mutated"
	snapshot.Policy.TeamDiscussions = EnterprisePolicyDisabled
	if again := st.GetEnterprise("acme"); again.Name != "Acme" || again.Policy.TeamDiscussions == EnterprisePolicyDisabled {
		t.Errorf("mutating a getter's result changed the store: %+v", again)
	}
	// FindEnterpriseByNodeID is the write path's lookup and returns the live
	// row, which is what lets a mutation read pre-mutation values off the same
	// object it updates.
	live := FindEnterpriseByNodeID(st, created.NodeID)
	if live == nil {
		t.Fatal("FindEnterpriseByNodeID did not resolve the enterprise")
	}
	live.Location = "Mars"
	if st.GetEnterprise("acme").Location != "Mars" {
		t.Error("FindEnterpriseByNodeID returned a copy; it must return the live row")
	}
}

func TestEnterpriseMembershipRolesAreScopedToOneEnterprise(t *testing.T) {
	st := newEnterpriseTestStore(t)
	acme := st.CreateEnterprise("acme", "Acme", "")
	globex := st.CreateEnterprise("globex", "Globex", "")
	owner := newEnterpriseTestUser(st, "owner")
	st.SetEnterpriseMembership(acme.ID, owner.ID, EnterpriseRoleOwner)

	if role := st.EffectiveEnterpriseRole(acme.ID, owner); role != EnterpriseRoleOwner {
		t.Errorf("role in acme = %q, want OWNER", role)
	}
	if role := st.EffectiveEnterpriseRole(globex.ID, owner); role != "" {
		t.Errorf("owning acme conferred %q in globex; enterprises must not leak roles", role)
	}
	if st.IsEnterpriseOwner(globex.ID, owner) {
		t.Error("an owner of one enterprise was admitted as an owner of another")
	}
	if st.IsEnterpriseMember(globex.ID, owner) {
		t.Error("an owner of one enterprise was admitted as a member of another")
	}
}

func TestPrimaryEnterpriseAdmitsEveryAccount(t *testing.T) {
	st := newEnterpriseTestStore(t)
	st.SetPrimaryEnterpriseSlug("bleephub")
	primary := st.CreateEnterprise("bleephub", "Bleephub", "")
	other := st.CreateEnterprise("other", "Other", "")
	admin := newEnterpriseTestUser(st, "admin")
	admin.SiteAdmin = true
	member := newEnterpriseTestUser(st, "member")

	if role := st.EffectiveEnterpriseRole(primary.ID, admin); role != EnterpriseRoleOwner {
		t.Errorf("site admin's role in the instance's own enterprise = %q, want OWNER", role)
	}
	if role := st.EffectiveEnterpriseRole(primary.ID, member); role != EnterpriseRoleMember {
		t.Errorf("ordinary account's role in the instance's own enterprise = %q, want MEMBER", role)
	}
	if role := st.EffectiveEnterpriseRole(other.ID, admin); role != "" {
		t.Errorf("site admin held %q in a second enterprise it was never added to", role)
	}
}

func TestEnterpriseOrganizationMembershipConfersEnterpriseMembership(t *testing.T) {
	st := newEnterpriseTestStore(t)
	acme := st.CreateEnterprise("acme", "Acme", "")
	founder := newEnterpriseTestUser(st, "founder")
	staff := newEnterpriseTestUser(st, "staff")
	stranger := newEnterpriseTestUser(st, "stranger")
	org := st.CreateOrg(founder, "acme-eng", "Acme Engineering", "")
	st.SetMembership(org.Login, staff.ID, OrgRoleMember, MembershipStateActive)

	if !st.AddEnterpriseOrganization(acme.ID, org.ID) {
		t.Fatal("AddEnterpriseOrganization refused a free organization")
	}
	if role := st.EffectiveEnterpriseRole(acme.ID, founder); role != EnterpriseRoleOwner {
		t.Errorf("an organization owner's enterprise role = %q, want OWNER", role)
	}
	if role := st.EffectiveEnterpriseRole(acme.ID, staff); role != EnterpriseRoleMember {
		t.Errorf("an organization member's enterprise role = %q, want MEMBER", role)
	}
	if role := st.EffectiveEnterpriseRole(acme.ID, stranger); role != "" {
		t.Errorf("an unaffiliated account held %q", role)
	}
	if got := st.EnterpriseIDForOrg(org.ID); got != acme.ID {
		t.Errorf("EnterpriseIDForOrg = %d, want %d", got, acme.ID)
	}
}

func TestEnterpriseOrganizationBelongsToAtMostOneEnterprise(t *testing.T) {
	st := newEnterpriseTestStore(t)
	acme := st.CreateEnterprise("acme", "Acme", "")
	globex := st.CreateEnterprise("globex", "Globex", "")
	founder := newEnterpriseTestUser(st, "founder")
	org := st.CreateOrg(founder, "shared", "Shared", "")

	if !st.AddEnterpriseOrganization(acme.ID, org.ID) {
		t.Fatal("the first claim was refused")
	}
	if st.AddEnterpriseOrganization(globex.ID, org.ID) {
		t.Error("a second enterprise claimed an organization that already belongs to one")
	}
	if !st.TransferEnterpriseOrganization(org.ID, globex.ID) {
		t.Fatal("transfer refused")
	}
	if got := st.EnterpriseIDForOrg(org.ID); got != globex.ID {
		t.Errorf("after transfer EnterpriseIDForOrg = %d, want %d", got, globex.ID)
	}
	if st.RemoveEnterpriseOrganization(acme.ID, org.ID) {
		t.Error("the former enterprise removed an organization it no longer owns")
	}
	if !st.RemoveEnterpriseOrganization(globex.ID, org.ID) {
		t.Error("the owning enterprise could not remove its organization")
	}
}

func TestAcceptEnterpriseInvitationInstallsTheMembershipItGrants(t *testing.T) {
	st := newEnterpriseTestStore(t)
	acme := st.CreateEnterprise("acme", "Acme", "")
	inviter := newEnterpriseTestUser(st, "inviter")
	invitee := newEnterpriseTestUser(st, "invitee")

	inv := st.CreateEnterpriseInvitation(acme.ID, inviter.ID, invitee.ID, "", "admin", EnterpriseRoleBillingManager)
	if inv == nil {
		t.Fatal("CreateEnterpriseInvitation returned nil")
	}
	if dup := st.CreateEnterpriseInvitation(acme.ID, inviter.ID, invitee.ID, "", "admin", EnterpriseRoleOwner); dup != nil {
		t.Error("a second outstanding invitation was created for the same invitee")
	}
	if accepted := st.AcceptEnterpriseInvitation(inv.ID, invitee.ID); accepted == nil {
		t.Fatal("AcceptEnterpriseInvitation returned nil")
	}
	if role := st.EffectiveEnterpriseRole(acme.ID, invitee); role != EnterpriseRoleBillingManager {
		t.Errorf("role after accepting = %q, want the invitation's BILLING_MANAGER", role)
	}
	if st.GetEnterpriseInvitation(inv.ID) != nil {
		t.Error("the accepted invitation is still outstanding")
	}
	if len(st.ListEnterpriseInvitations(acme.ID, "admin")) != 0 {
		t.Error("the accepted invitation still appears in the pending list")
	}
}

func TestEnterpriseInvitationsAreScopedToTheirEnterprise(t *testing.T) {
	st := newEnterpriseTestStore(t)
	acme := st.CreateEnterprise("acme", "Acme", "")
	globex := st.CreateEnterprise("globex", "Globex", "")
	inviter := newEnterpriseTestUser(st, "inviter")
	invitee := newEnterpriseTestUser(st, "invitee")
	st.CreateEnterpriseInvitation(acme.ID, inviter.ID, invitee.ID, "", "member", EnterpriseRoleMember)

	if got := len(st.ListEnterpriseInvitations(globex.ID, "member")); got != 0 {
		t.Errorf("globex sees %d of acme's invitations", got)
	}
	if got := len(st.ListEnterpriseInvitations(acme.ID, "admin")); got != 0 {
		t.Errorf("a member invitation appeared in the administrator list (%d)", got)
	}
}

func TestEnterpriseIdentityProviderLifecycle(t *testing.T) {
	st := newEnterpriseTestStore(t)
	acme := st.CreateEnterprise("acme", "Acme", "")
	codes := []string{"aaaaa-bbbbb", "ccccc-ddddd"}
	idp := st.SetEnterpriseIdentityProvider(acme.ID, "https://idp.test/sso", "https://idp.test", "CERT", "RSA_SHA256", "SHA256", codes)
	if idp == nil {
		t.Fatal("SetEnterpriseIdentityProvider returned nil")
	}
	if idp.SSOURL != "https://idp.test/sso" || idp.SignatureMethod != "RSA_SHA256" {
		t.Errorf("identity provider = %+v", idp)
	}
	idp.RecoveryCodes[0] = "mutated"
	if stored := st.GetEnterprise("acme").IdentityProvider; stored.RecoveryCodes[0] == "mutated" {
		t.Error("the returned recovery codes alias the stored slice")
	}
	rotated := st.RegenerateEnterpriseIdentityProviderRecoveryCodes(acme.ID, []string{"eeeee-fffff"})
	if rotated == nil || len(rotated.RecoveryCodes) != 1 {
		t.Fatalf("regenerated codes = %+v", rotated)
	}
	if removed := st.RemoveEnterpriseIdentityProvider(acme.ID); removed == nil {
		t.Fatal("RemoveEnterpriseIdentityProvider returned nil")
	}
	if st.GetEnterprise("acme").IdentityProvider != nil {
		t.Error("the identity provider survived removal")
	}
	if st.RegenerateEnterpriseIdentityProviderRecoveryCodes(acme.ID, []string{"x"}) != nil {
		t.Error("recovery codes were regenerated for an enterprise with no identity provider")
	}
}

func TestIPAllowListEntriesAreScopedToTheirOwner(t *testing.T) {
	st := newEnterpriseTestStore(t)
	acme := st.CreateEnterprise("acme", "Acme", "")
	globex := st.CreateEnterprise("globex", "Globex", "")
	entry := st.CreateIPAllowListEntry("Enterprise", acme.ID, "10.0.0.0/8", "office", true)
	if entry == nil || entry.NodeID == "" {
		t.Fatalf("CreateIPAllowListEntry = %+v", entry)
	}
	if got := len(st.ListIPAllowListEntries("Enterprise", globex.ID)); got != 0 {
		t.Errorf("globex sees %d of acme's allow-list entries", got)
	}
	if got := len(st.ListIPAllowListEntries("Enterprise", acme.ID)); got != 1 {
		t.Errorf("acme sees %d of its own entries, want 1", got)
	}
	if FindIPAllowListEntryByNodeID(st, entry.NodeID) == nil {
		t.Error("FindIPAllowListEntryByNodeID did not resolve the entry")
	}
	if updated := st.UpdateIPAllowListEntry(entry.ID, "192.168.0.0/16", "vpn", false); updated == nil || updated.IsActive {
		t.Errorf("UpdateIPAllowListEntry = %+v", updated)
	}
	if removed := st.DeleteIPAllowListEntry(entry.ID); removed == nil {
		t.Error("DeleteIPAllowListEntry returned nil")
	}
	if got := len(st.ListIPAllowListEntries("Enterprise", acme.ID)); got != 0 {
		t.Errorf("%d entries survived deletion", got)
	}
}

func TestEnterpriseSupportEntitlementNeedsAMembership(t *testing.T) {
	st := newEnterpriseTestStore(t)
	acme := st.CreateEnterprise("acme", "Acme", "")
	user := newEnterpriseTestUser(st, "member")
	if st.SetEnterpriseSupportEntitlement(acme.ID, user.ID, true) {
		t.Error("a support entitlement was granted to a non-member")
	}
	st.SetEnterpriseMembership(acme.ID, user.ID, EnterpriseRoleMember)
	if !st.SetEnterpriseSupportEntitlement(acme.ID, user.ID, true) {
		t.Fatal("a member could not be granted a support entitlement")
	}
	if m := st.GetEnterpriseMembership(acme.ID, user.ID); m == nil || !m.SupportEntitlement {
		t.Errorf("membership after granting = %+v", m)
	}
}

func TestEnterpriseMigratorRoleGrantIsIdempotentAndOrdered(t *testing.T) {
	st := newEnterpriseTestStore(t)
	acme := st.CreateEnterprise("acme", "Acme", "")
	if !st.SetEnterpriseMigratorRole(acme.ID, "zoe", true) {
		t.Fatal("grant refused")
	}
	st.SetEnterpriseMigratorRole(acme.ID, "adam", true)
	st.SetEnterpriseMigratorRole(acme.ID, "zoe", true)
	got := st.GetEnterprise("acme").MigratorLogins
	if len(got) != 2 || got[0] != "adam" || got[1] != "zoe" {
		t.Errorf("migrator logins = %v, want [adam zoe]", got)
	}
	st.SetEnterpriseMigratorRole(acme.ID, "adam", false)
	if got := st.GetEnterprise("acme").MigratorLogins; len(got) != 1 || got[0] != "zoe" {
		t.Errorf("after revoke migrator logins = %v", got)
	}
}

func TestUpdateEnterprisePolicyAndProfilePersistThroughTheGetter(t *testing.T) {
	st := newEnterpriseTestStore(t)
	acme := st.CreateEnterprise("acme", "Acme", "")
	name, location := "Acme Corporation", "Springfield"
	updated := st.UpdateEnterpriseProfile(acme.ID, &name, nil, &location, nil, nil, nil)
	if updated == nil || updated.Name != name || updated.Location != location {
		t.Fatalf("UpdateEnterpriseProfile = %+v", updated)
	}
	if updated.Description != "" {
		t.Errorf("a nil description field was written: %q", updated.Description)
	}
	policy := st.UpdateEnterprisePolicy(acme.ID, func(p *EnterprisePolicy) {
		p.MembersCanDeleteRepositories = EnterprisePolicyDisabled
	})
	if policy == nil || policy.Policy.MembersCanDeleteRepositories != EnterprisePolicyDisabled {
		t.Fatalf("UpdateEnterprisePolicy = %+v", policy)
	}
	if st.GetEnterprise("acme").Policy.MembersCanDeleteRepositories != EnterprisePolicyDisabled {
		t.Error("the policy write did not reach the store")
	}
	if st.UpdateEnterprisePolicy(9999, func(*EnterprisePolicy) {}) != nil {
		t.Error("a policy write against a missing enterprise reported success")
	}
}

func TestEnsureEnterpriseIsIdempotent(t *testing.T) {
	st := newEnterpriseTestStore(t)
	first := st.EnsureEnterprise("bleephub", "Bleephub", "")
	second := st.EnsureEnterprise("bleephub", "Renamed", "")
	if first == nil || second == nil || first.ID != second.ID {
		t.Fatalf("EnsureEnterprise minted two enterprises: %+v %+v", first, second)
	}
	if second.Name != "Bleephub" {
		t.Errorf("EnsureEnterprise renamed the existing enterprise to %q", second.Name)
	}
	if got := len(st.ListEnterprises()); got != 1 {
		t.Errorf("ListEnterprises returned %d enterprises", got)
	}
}

func TestMergeEnterprisePolicyDefaultsFillsAnOlderRow(t *testing.T) {
	// A row persisted before a policy field existed reads back with the Go
	// zero value, which is not a member of GitHub's enums.
	filled := mergeEnterprisePolicyDefaults(EnterprisePolicy{MembersCanDeleteIssues: EnterprisePolicyDisabled})
	if filled.MembersCanDeleteIssues != EnterprisePolicyDisabled {
		t.Errorf("an existing value was overwritten: %q", filled.MembersCanDeleteIssues)
	}
	if filled.TwoFactorRequired != EnterprisePolicyNoPolicy || filled.IPAllowListEnabled != EnterprisePolicyDisabled {
		t.Errorf("defaults were not filled in: %+v", filled)
	}
}

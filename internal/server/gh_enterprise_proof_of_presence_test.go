package bleephub

import (
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestEnterpriseProofOfPresenceSettingIsOwnerOnlyAndGuardsItself covers the
// setting that replaced GitHub's withdrawn GraphQL mutation: an enterprise
// owner reads and changes the requirement, a plain member may only read it, an
// anonymous caller is turned away, an unknown enterprise or value is refused —
// and once a requirement is in force, relaxing it demands the very proof of
// presence it requires. (Every account is a member of the primary enterprise,
// so there is no signed-in stranger to it.)
func TestEnterpriseProofOfPresenceSettingIsOwnerOnlyAndGuardsItself(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	enterprise := s.store.GetEnterprise(s.enterpriseSlug())
	path := "/ui-data/enterprises/" + enterprise.Slug + "/proof-of-presence"

	owner := s.newSudoFixture(t, "pop-owner")
	s.store.SetEnterpriseMembership(enterprise.ID, owner.user.ID, store.EnterpriseRoleOwner)
	member := s.newSudoFixture(t, "pop-member")
	s.store.SetEnterpriseMembership(enterprise.ID, member.user.ID, store.EnterpriseRoleMember)

	// Permitted: the owner reads the default and sets a requirement.
	initial := decodeBody(t, s.doWithCookie(t, http.MethodGet, path, owner.cookie, nil), http.StatusOK)
	if initial["requirement"] != store.EnterprisePolicyNoPolicy {
		t.Fatalf("default requirement = %v, want %s", initial["requirement"], store.EnterprisePolicyNoPolicy)
	}
	set := decodeBody(t, s.doWithCookie(t, http.MethodPut, path, owner.cookie,
		map[string]string{"requirement": store.EnterpriseProofOfPresenceReauth}), http.StatusOK)
	if set["requirement"] != store.EnterpriseProofOfPresenceReauth {
		t.Fatalf("requirement after the update = %v", set["requirement"])
	}
	if got := s.store.GetEnterprise(enterprise.Slug).Policy.ProofOfPresenceRequired; got != store.EnterpriseProofOfPresenceReauth {
		t.Fatalf("stored requirement = %q", got)
	}

	// Refused: a member reads but cannot write; nobody unauthenticated gets in;
	// a slug that names no enterprise is simply not found.
	expectStatus(t, s.doWithCookie(t, http.MethodGet, path, member.cookie, nil),
		http.StatusOK, "member reads the requirement")
	expectStatus(t, s.doWithCookie(t, http.MethodPut, path, member.cookie,
		map[string]string{"requirement": store.EnterprisePolicyNoPolicy}),
		http.StatusForbidden, "member changes the requirement")
	expectStatus(t, s.doWithCookie(t, http.MethodGet, path, "no-such-session", nil),
		http.StatusUnauthorized, "anonymous caller reads the requirement")
	expectStatus(t, s.doWithCookie(t, http.MethodPut, "/ui-data/enterprises/no-such-enterprise/proof-of-presence", owner.cookie,
		map[string]string{"requirement": store.EnterprisePolicyNoPolicy}),
		http.StatusNotFound, "owner changes the requirement of an unknown enterprise")

	// The requirement now guards its own relaxation.
	refused := s.doWithCookie(t, http.MethodPut, path, owner.cookie,
		map[string]string{"requirement": store.EnterprisePolicyNoPolicy})
	if got := refused.Header.Get("X-GitHub-Sudo"); got == "" {
		t.Error("relaxing the requirement from a stale session carried no sudo challenge")
	}
	expectStatus(t, refused, http.StatusForbidden, "owner relaxes the requirement without a proof of presence")
	if got := s.store.GetEnterprise(enterprise.Slug).Policy.ProofOfPresenceRequired; got != store.EnterpriseProofOfPresenceReauth {
		t.Fatalf("a refused update changed the requirement to %q", got)
	}

	expectStatus(t, s.doWithCookie(t, http.MethodPost, "/ui-data/user/sudo", owner.cookie,
		map[string]string{"password": owner.password}), http.StatusOK, "owner proves presence")
	expectStatus(t, s.doWithCookie(t, http.MethodPut, path, owner.cookie,
		map[string]string{"requirement": "SOMETIMES"}),
		http.StatusUnprocessableEntity, "owner sets an unknown requirement")
	relaxed := decodeBody(t, s.doWithCookie(t, http.MethodPut, path, owner.cookie,
		map[string]string{"requirement": store.EnterprisePolicyNoPolicy}), http.StatusOK)
	if relaxed["requirement"] != store.EnterprisePolicyNoPolicy {
		t.Fatalf("requirement after relaxing = %v", relaxed["requirement"])
	}
}

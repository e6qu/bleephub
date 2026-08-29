package bleephub

// Each formerly-inert enterprise policy is exercised in BOTH directions: a
// refusal-only test would pass against a permanently broken feature, so every
// case pins the permitted side, not just the refused one.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/e6qu/bleephub/internal/store"
)

// membersCanMakePurchases

// TestEnterprisePolicyGovernsMarketplacePurchases: the Marketplace write paths
// that spend money — the initial purchase and a later plan change — answer to
// the enterprise's members-can-make-purchases setting.
func TestEnterprisePolicyGovernsMarketplacePurchases(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sink.Close()
	listing := s.publishMarketplaceGitHubApp(t, "Purchase Policy App", sink.URL)
	_, buyerToken := s.userSurfaceUser(t, "purchase-policy-buyer")
	_, secondToken := s.userSurfaceUser(t, "purchase-policy-buyer-two")
	enterprise := s.store.GetEnterprise(s.enterpriseSlug())
	if enterprise == nil {
		t.Fatal("the instance's own enterprise account was not seeded")
	}
	purchasePath := "/ui-data/marketplace/listings/" + listing.slug + "/purchase"
	changePath := "/ui-data/marketplace/listings/" + listing.slug + "/subscription"

	// Permitted: GitHub's default for this setting is ENABLED.
	expectStatus(t, s.post(t, purchasePath, buyerToken,
		map[string]interface{}{"plan_id": listing.freePlanID, "billing_cycle": "monthly"}),
		http.StatusCreated, "purchase while the enterprise permits purchases")

	if s.store.UpdateEnterprisePolicy(enterprise.ID, func(p *store.EnterprisePolicy) {
		p.MembersCanMakePurchases = store.EnterprisePolicyDisabled
	}) == nil {
		t.Fatal("UpdateEnterprisePolicy failed")
	}

	// Refused: a fresh purchase, and an upgrade of the one already held.
	expectStatus(t, s.post(t, purchasePath, secondToken,
		map[string]interface{}{"plan_id": listing.freePlanID, "billing_cycle": "monthly"}),
		http.StatusForbidden, "purchase under a disabling policy")
	expectStatus(t, s.patch(t, changePath, buyerToken,
		map[string]interface{}{"plan_id": listing.paidPlanID, "billing_cycle": "monthly"}),
		http.StatusForbidden, "plan change under a disabling policy")

	second := s.store.UsersByLogin["purchase-policy-buyer-two"]
	if second != nil && s.store.GetMarketplacePurchase(listing.slug, "User", second.ID) != nil {
		t.Fatal("the refused purchase was still recorded")
	}
	buyer := s.store.UsersByLogin["purchase-policy-buyer"]
	if held := s.store.GetMarketplacePurchase(listing.slug, "User", buyer.ID); held == nil || held.PlanID != listing.freePlanID {
		t.Fatalf("the refused plan change still moved the subscription: %+v", held)
	}
}

// organizationProjects

const createProjectV2Mutation = `mutation($ownerId: ID!, $title: String!) {
  createProjectV2(input: {ownerId: $ownerId, title: $title}) { projectV2 { id title } }
}`

// TestEnterprisePolicyGovernsOrganizationProjects: creating a ProjectV2 under
// an organization answers to the enterprise's organization-projects setting,
// while a project under a personal account — which is not an organization
// project — does not.
func TestEnterprisePolicyGovernsOrganizationProjects(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newEnterprisePolicyFixture(t, "orgproj")

	created := s.gqlDataAs(t, f.ownerToken, createProjectV2Mutation,
		map[string]interface{}{"ownerId": f.org.NodeID, "title": "Permitted board"})
	payload, _ := created["createProjectV2"].(map[string]interface{})
	if payload == nil || payload["projectV2"] == nil {
		t.Fatalf("createProjectV2 while the enterprise permits organization projects = %v", created)
	}

	s.setEnterprisePolicy(t, f, func(p *store.EnterprisePolicy) {
		p.OrganizationProjects = store.EnterprisePolicyDisabled
	})

	refused := s.gqlDoAs(t, f.ownerToken, createProjectV2Mutation,
		map[string]interface{}{"ownerId": f.org.NodeID, "title": "Refused board"})
	errs, _ := refused["errors"].([]interface{})
	if len(errs) == 0 {
		t.Fatalf("createProjectV2 under a disabling policy succeeded: %v", refused)
	}
	for _, project := range s.store.ProjectsV2.ListProjectsForOwner(f.org.ID, "Organization") {
		if project.Title == "Refused board" {
			t.Fatal("the refused mutation still created the organization project")
		}
	}

	// The policy governs organization projects, not personal ones. The fixture
	// user is built straight in the store, so give it the global id GraphQL
	// resolves an owner by.
	s.store.Mu.Lock()
	f.owner.NodeID = "U_kgDOorgproj"
	s.store.Mu.Unlock()
	personal := s.gqlDataAs(t, f.ownerToken, createProjectV2Mutation,
		map[string]interface{}{"ownerId": f.owner.NodeID, "title": "Personal board"})
	personalPayload, _ := personal["createProjectV2"].(map[string]interface{})
	if personalPayload == nil || personalPayload["projectV2"] == nil {
		t.Fatalf("a personal project must be unaffected by the organization policy: %v", personal)
	}
}

// proofOfPresenceRequired (sudo mode)

// sudoFixture is an account with a local password and a live browser session,
// which is the only principal sudo mode governs.
type sudoFixture struct {
	user     *store.User
	token    string
	cookie   string
	password string
}

func (s *isolatedServer) newSudoFixture(t *testing.T, login string) *sudoFixture {
	t.Helper()
	f := &sudoFixture{password: "sudo-fixture-password-1", cookie: "cookie-" + login}
	f.user, f.token = s.newUser(t, login)
	hash, err := bcrypt.GenerateFromPassword([]byte(f.password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	s.store.Mu.Lock()
	f.user.PasswordHash = string(hash)
	s.store.Mu.Unlock()
	if err := s.store.PutLoginSession(f.cookie, &store.LoginSession{
		UserID:    f.user.ID,
		Handle:    "handle-" + login,
		CreatedAt: s.currentTime(),
		// Well beyond the sudo window, so the expiry case under test is the
		// grant lapsing rather than the session itself ending.
		ExpiresAt: s.currentTime().Add(24 * time.Hour),
		UserAgent: "Test/1.0",
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// doWithCookie issues a request authenticated by a browser session rather than
// a bearer token. Sudo mode governs the cookie surface, so the both-directions
// case has to be driven through it.
func (s *isolatedServer) doWithCookie(t *testing.T, method, path, cookie string, body interface{}) *http.Response {
	t.Helper()
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, s.baseURL+path, payload)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestEnterpriseProofOfPresenceGuardsSensitiveActions: with a
// proof-of-presence requirement in force, a session that has not
// re-authenticated recently cannot change the account's security settings; one
// that answers the challenge can, and the grant lapses with the window.
func TestEnterpriseProofOfPresenceGuardsSensitiveActions(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	clock := newMovableClock(s, fixedTestTime)
	f := s.newSudoFixture(t, "sudo-reauth")
	enterprise := s.store.GetEnterprise(s.enterpriseSlug())
	const enrollment = "/ui-data/user/two-factor/enrollment"

	// Permitted: the enterprise imposes no requirement, so an unelevated
	// session performs the sensitive action.
	expectStatus(t, s.doWithCookie(t, http.MethodPost, enrollment, f.cookie, nil),
		http.StatusCreated, "begin enrolment with no proof-of-presence policy")
	expectStatus(t, s.doWithCookie(t, http.MethodDelete, enrollment, f.cookie, nil),
		http.StatusOK, "cancel the enrolment again")

	if s.store.UpdateEnterprisePolicy(enterprise.ID, func(p *store.EnterprisePolicy) {
		p.ProofOfPresenceRequired = store.EnterpriseProofOfPresenceReauth
	}) == nil {
		t.Fatal("UpdateEnterprisePolicy failed")
	}

	// Refused: the session has never proved presence.
	refused := s.doWithCookie(t, http.MethodPost, enrollment, f.cookie, nil)
	if got := refused.Header.Get("X-GitHub-Sudo"); got != "required; password" {
		t.Errorf("sudo challenge header = %q, want %q", got, "required; password")
	}
	expectStatus(t, refused, http.StatusForbidden, "begin enrolment without a proof of presence")

	// A wrong password does not elevate the session.
	expectStatus(t, s.doWithCookie(t, http.MethodPost, "/ui-data/user/sudo", f.cookie,
		map[string]string{"password": "not-the-password"}),
		http.StatusUnprocessableEntity, "sudo challenge with the wrong password")
	expectStatus(t, s.doWithCookie(t, http.MethodPost, enrollment, f.cookie, nil),
		http.StatusForbidden, "begin enrolment after a failed challenge")

	// The right password does, and the sensitive action then proceeds.
	state := decodeBody(t, s.doWithCookie(t, http.MethodPost, "/ui-data/user/sudo", f.cookie,
		map[string]string{"password": f.password}), http.StatusOK)
	if state["active"] != true {
		t.Fatalf("sudo state after a successful challenge = %v", state)
	}
	expectStatus(t, s.doWithCookie(t, http.MethodPost, enrollment, f.cookie, nil),
		http.StatusCreated, "begin enrolment with a live proof of presence")
	expectStatus(t, s.doWithCookie(t, http.MethodDelete, enrollment, f.cookie, nil),
		http.StatusOK, "cancel the enrolment again")

	// The grant expires with the window rather than lasting the session.
	clock.advance(sudoModeWindow + time.Minute)
	expectStatus(t, s.doWithCookie(t, http.MethodPost, enrollment, f.cookie, nil),
		http.StatusForbidden, "begin enrolment after the sudo window elapsed")

	// A bearer credential is not a browser session; sudo mode does not
	// interpose on it, exactly as on GitHub.
	expectStatus(t, s.post(t, enrollment, f.token, nil),
		http.StatusCreated, "begin enrolment with a token credential")
}

// TestEnterpriseProofOfPresenceMFARequiresASecondFactor: the MFA requirement is
// not satisfied by a password, only by a second factor.
func TestEnterpriseProofOfPresenceMFARequiresASecondFactor(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	clock := newMovableClock(s, fixedTestTime)
	f := s.newSudoFixture(t, "sudo-mfa")
	enterprise := s.store.GetEnterprise(s.enterpriseSlug())

	// Enrol a second factor through the token surface, which sudo does not gate.
	secret := beginEnrollment(t, s, f.token)
	confirmed := decodeBody(t, s.post(t, "/ui-data/user/two-factor/enrollment/confirm", f.token,
		map[string]string{"code": codeFor(t, secret, clock.now())}), http.StatusOK)
	if confirmed["two_factor"].(map[string]interface{})["enabled"] != true {
		t.Fatalf("enrolment did not enable a second factor: %v", confirmed)
	}

	if s.store.UpdateEnterprisePolicy(enterprise.ID, func(p *store.EnterprisePolicy) {
		p.ProofOfPresenceRequired = store.EnterpriseProofOfPresenceMFA
	}) == nil {
		t.Fatal("UpdateEnterprisePolicy failed")
	}

	challenge := "/ui-data/user/sudo"
	// A correct password is a re-authentication, but it is not an MFA proof.
	expectStatus(t, s.doWithCookie(t, http.MethodPost, challenge, f.cookie,
		map[string]string{"password": f.password}), http.StatusForbidden,
		"password answering an MFA proof-of-presence requirement")
	expectStatus(t, s.doWithCookie(t, http.MethodDelete, "/ui-data/user/sessions/handle-sudo-mfa", f.cookie, nil),
		http.StatusForbidden, "revoke a session without an MFA proof")

	// The authenticator's code is the proof the policy asked for.
	clock.advance(store.TOTPPeriod)
	state := decodeBody(t, s.doWithCookie(t, http.MethodPost, challenge, f.cookie,
		map[string]string{"code": codeFor(t, secret, clock.now())}), http.StatusOK)
	if state["active"] != true || state["confirmed_with_second_factor"] != true {
		t.Fatalf("sudo state after an MFA challenge = %v", state)
	}
	expectStatus(t, s.doWithCookie(t, http.MethodDelete, "/ui-data/user/sessions/handle-sudo-mfa", f.cookie, nil),
		http.StatusNoContent, "revoke a session with an MFA proof")
}

// ipAllowListUserLevelEnforcementEnabled

// TestEnterpriseUserLevelIPAllowListEnforcement: an account's own allow list
// binds its API requests only while the enterprise turns user-level
// enforcement on.
func TestEnterpriseUserLevelIPAllowListEnforcement(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	_, token := s.newUser(t, "user-ip-allow-list")
	enterprise := s.store.GetEnterprise(s.enterpriseSlug())

	created := decodeBody(t, s.post(t, "/ui-data/user/ip-allow-list", token, map[string]interface{}{
		"allow_list_value": "198.51.100.0/24", "name": "office",
	}), http.StatusCreated)
	entryID, _ := created["id"].(float64)
	if entryID == 0 {
		t.Fatalf("created allow-list entry = %v", created)
	}

	// Permitted: the enterprise has not turned user-level enforcement on, so a
	// list the request's address does not match binds nothing.
	expectStatus(t, s.get(t, "/api/v3/user", token), http.StatusOK,
		"API request while user-level enforcement is off")

	if s.store.UpdateEnterprisePolicy(enterprise.ID, func(p *store.EnterprisePolicy) {
		p.IPAllowListUserLevelEnforcementEnabled = store.EnterprisePolicyEnabled
	}) == nil {
		t.Fatal("UpdateEnterprisePolicy failed")
	}

	// Refused: the loopback address the test client calls from is outside the
	// only active entry.
	expectStatus(t, s.get(t, "/api/v3/user", token), http.StatusForbidden,
		"API request from an address the account's allow list excludes")

	// The management surface stays reachable, so the account can correct a
	// range that locked it out.
	listed := decodeBody(t, s.get(t, "/ui-data/user/ip-allow-list", token), http.StatusOK)
	if listed["enforced"] != true {
		t.Fatalf("allow-list page did not report enforcement: %v", listed)
	}
	expectStatus(t, s.patch(t, "/ui-data/user/ip-allow-list/"+itoa(int(entryID)), token,
		map[string]interface{}{"allow_list_value": "127.0.0.0/8"}), http.StatusOK,
		"widen the account's own allow list")

	// Permitted again: the address is now inside the list.
	expectStatus(t, s.get(t, "/api/v3/user", token), http.StatusOK,
		"API request from an address the account's allow list admits")

	// A deactivated entry stops binding, the same as an empty list.
	expectStatus(t, s.patch(t, "/ui-data/user/ip-allow-list/"+itoa(int(entryID)), token,
		map[string]interface{}{"allow_list_value": "198.51.100.0/24", "is_active": false}),
		http.StatusOK, "deactivate the entry")
	expectStatus(t, s.get(t, "/api/v3/user", token), http.StatusOK,
		"API request with no active entry on the account's allow list")
}

// TestUserIPAllowListIsPrivateToItsAccount: one account's entries are not
// reachable from another's settings surface.
func TestUserIPAllowListIsPrivateToItsAccount(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	_, ownerToken := s.newUser(t, "ip-list-owner")
	_, otherToken := s.newUser(t, "ip-list-stranger")
	created := decodeBody(t, s.post(t, "/ui-data/user/ip-allow-list", ownerToken, map[string]interface{}{
		"allow_list_value": "203.0.113.4", "name": "home",
	}), http.StatusCreated)
	id := itoa(int(created["id"].(float64)))

	expectStatus(t, s.patch(t, "/ui-data/user/ip-allow-list/"+id, otherToken,
		map[string]interface{}{"is_active": false}), http.StatusNotFound,
		"another account patching the entry")
	expectStatus(t, s.delete(t, "/ui-data/user/ip-allow-list/"+id, otherToken), http.StatusNotFound,
		"another account deleting the entry")
	expectStatus(t, s.delete(t, "/ui-data/user/ip-allow-list/"+id, ownerToken), http.StatusNoContent,
		"the owning account deleting its own entry")
}

// twoFactorDisallowedMethods

// TestEnterpriseDisallowsInsecureTwoFactorMethods: with the setting at
// INSECURE, the second-factor method bleephub classes insecure — the printed
// recovery code, a static secret valid until spent — stops authenticating,
// while the authenticator's time-based code still does.
func TestEnterpriseDisallowsInsecureTwoFactorMethods(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	clock := newMovableClock(s, fixedTestTime)
	user, token := s.newUser(t, "insecure-2fa")
	enterprise := s.store.GetEnterprise(s.enterpriseSlug())

	secret := beginEnrollment(t, s, token)
	confirmed := decodeBody(t, s.post(t, "/ui-data/user/two-factor/enrollment/confirm", token,
		map[string]string{"code": codeFor(t, secret, clock.now())}), http.StatusOK)
	codes, _ := confirmed["recovery_codes"].([]interface{})
	if len(codes) < 3 {
		t.Fatalf("enrolment returned %d recovery codes", len(codes))
	}

	// Permitted: with no policy, a recovery code is a valid second factor.
	if result := s.store.VerifySecondFactor(user.ID, codes[0].(string), clock.now()); result != store.SecurityOK {
		t.Fatalf("recovery code under NO_POLICY = %v, want SecurityOK", result)
	}

	if s.store.UpdateEnterprisePolicy(enterprise.ID, func(p *store.EnterprisePolicy) {
		p.TwoFactorDisallowedMethods = store.EnterprisePolicyDisallowInsecure
	}) == nil {
		t.Fatal("UpdateEnterprisePolicy failed")
	}

	// Refused: the same kind of credential no longer answers a challenge, and
	// the unspent code is NOT burned by the refusal.
	expectStatus(t, s.post(t, "/ui-data/user/two-factor/recovery-codes", token,
		map[string]string{"code": codes[1].(string)}), http.StatusForbidden,
		"recovery code under a disallow-insecure policy")
	if result := s.store.VerifySecondFactor(user.ID, codes[1].(string), clock.now()); result != store.SecurityOK {
		t.Fatalf("the refused recovery code was spent anyway: %v", result)
	}

	// Permitted: the authenticator's code is not an insecure method.
	clock.advance(store.TOTPPeriod)
	regenerated := decodeBody(t, s.post(t, "/ui-data/user/two-factor/recovery-codes", token,
		map[string]string{"code": codeFor(t, secret, clock.now())}), http.StatusOK)
	if fresh, _ := regenerated["recovery_codes"].([]interface{}); len(fresh) == 0 {
		t.Fatalf("regenerating with a TOTP code returned no codes: %v", regenerated)
	}

	// The account page reports the catalogue truthfully rather than claiming a
	// factor this instance does not implement.
	settings := decodeBody(t, s.get(t, "/ui-data/user/authentication", token), http.StatusOK)
	methods, _ := settings["two_factor_methods"].([]interface{})
	if len(methods) != 2 {
		t.Fatalf("two-factor method catalogue = %v", methods)
	}
	seen := map[string]bool{}
	for _, entry := range methods {
		row := entry.(map[string]interface{})
		seen[row["method"].(string)] = row["disallowed"] == true
	}
	if seen["totp"] || !seen["recovery_code"] {
		t.Fatalf("catalogue did not report the disallowed method: %v", methods)
	}
}

// notificationDeliveryRestrictionEnabled

// TestEnterpriseNotificationDeliveryRestriction: with delivery restricted to
// the enterprise's verified domains, an account whose address is outside them
// cannot select email delivery and is not served preferences that claim it;
// once its domain is verified, the same request is accepted.
func TestEnterpriseNotificationDeliveryRestriction(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	user, token := s.newUser(t, "delivery-restricted")
	s.store.Mu.Lock()
	user.Email = "delivery-restricted@outside.test"
	s.store.Mu.Unlock()
	enterprise := s.store.GetEnterprise(s.enterpriseSlug())
	const path = "/ui-data/user/notification-settings"
	emailOn := map[string]interface{}{
		"participating":                    map[string]bool{"email": true, "web": true},
		"watching":                         map[string]bool{"email": true, "web": true},
		"automatically_watch_repositories": true,
	}

	// Permitted: no restriction, so email delivery may be selected.
	saved := decodeBody(t, s.put(t, path, token, emailOn), http.StatusOK)
	if saved["participating"].(map[string]interface{})["email"] != true {
		t.Fatalf("email delivery was not saved: %v", saved)
	}

	if s.store.UpdateEnterprisePolicy(enterprise.ID, func(p *store.EnterprisePolicy) {
		p.NotificationDeliveryRestrictionEnabled = store.EnterprisePolicyEnabled
	}) == nil {
		t.Fatal("UpdateEnterprisePolicy failed")
	}
	if s.store.SetEnterpriseVerifiedDomains(enterprise.ID, []string{"corp.test"}) == nil {
		t.Fatal("SetEnterpriseVerifiedDomains failed")
	}

	// Refused: the address is outside every verified domain.
	expectStatus(t, s.put(t, path, token, emailOn), http.StatusForbidden,
		"selecting email delivery for an unverified domain")
	// And the already-saved selection is not served as though it still held.
	served := decodeBody(t, s.get(t, path, token), http.StatusOK)
	if served["email_delivery_restricted"] != true {
		t.Fatalf("served preferences did not report the restriction: %v", served)
	}
	if served["participating"].(map[string]interface{})["email"] != false {
		t.Fatalf("served preferences still claimed email delivery: %v", served)
	}
	// The web inbox is untouched: the restriction is about email delivery.
	if served["watching"].(map[string]interface{})["web"] != true {
		t.Fatalf("the restriction suppressed web delivery too: %v", served)
	}

	// Permitted again once the account's domain is verified.
	if s.store.SetEnterpriseVerifiedDomains(enterprise.ID, []string{"corp.test", "outside.test"}) == nil {
		t.Fatal("SetEnterpriseVerifiedDomains failed")
	}
	restored := decodeBody(t, s.put(t, path, token, emailOn), http.StatusOK)
	if restored["participating"].(map[string]interface{})["email"] != true {
		t.Fatalf("email delivery was refused for a verified domain: %v", restored)
	}
	if _, reported := restored["email_delivery_restricted"]; reported {
		t.Fatalf("a deliverable account was still marked restricted: %v", restored)
	}
}

// TestEnterpriseVerifiedDomainsAreOwnerWritable: the domain list the
// restriction is expressed against is the enterprise owners' to set.
func TestEnterpriseVerifiedDomainsAreOwnerWritable(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	_, memberToken := s.newUser(t, "domain-member")
	path := "/ui-data/enterprises/" + s.enterpriseSlug() + "/verified-domains"

	expectStatus(t, s.put(t, path, memberToken, map[string]interface{}{"domains": []string{"corp.test"}}),
		http.StatusForbidden, "an ordinary member setting verified domains")
	expectStatus(t, s.put(t, path, defaultToken, map[string]interface{}{"domains": []string{"not a domain"}}),
		http.StatusUnprocessableEntity, "an invalid domain")

	saved := decodeBody(t, s.put(t, path, defaultToken,
		map[string]interface{}{"domains": []string{"@Corp.Test", "corp.test", "eng.corp.test"}}), http.StatusOK)
	domains, _ := saved["domains"].([]interface{})
	if len(domains) != 2 || domains[0] != "corp.test" || domains[1] != "eng.corp.test" {
		t.Fatalf("verified domains = %v", domains)
	}
	read := decodeBody(t, s.get(t, path, memberToken), http.StatusOK)
	if got, _ := read["domains"].([]interface{}); len(got) != 2 {
		t.Fatalf("member read of verified domains = %v", read)
	}
}

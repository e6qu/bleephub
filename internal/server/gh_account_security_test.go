package bleephub

import (
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- RFC 6238 defines TOTP over HMAC-SHA1; this test computes the standard's own vectors
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/e6qu/bleephub/internal/store"
)

// movableClock installs a clock the test can advance. TOTP is a function of
// time, so verifying drift tolerance and single-use enforcement needs the clock
// to move — but it must never be the wall clock, and the closure is read from
// the server's goroutine, so the instant lives in an atomic.
type movableClock struct{ unix atomic.Int64 }

func newMovableClock(s *isolatedServer, at time.Time) *movableClock {
	clock := &movableClock{}
	clock.unix.Store(at.Unix())
	s.replaceClockNow(func() time.Time { return time.Unix(clock.unix.Load(), 0).UTC() })
	return clock
}

func (c *movableClock) now() time.Time          { return time.Unix(c.unix.Load(), 0).UTC() }
func (c *movableClock) advance(d time.Duration) { c.unix.Add(int64(d / time.Second)) }

func decodeBody(t *testing.T, resp *http.Response, want int) map[string]interface{} {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, want, raw)
	}
	if len(raw) == 0 {
		return nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode body %s: %v", raw, err)
	}
	return out
}

func rawBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(raw)
}

// beginEnrollment starts an enrolment and returns the provisioning secret.
func beginEnrollment(t *testing.T, s *isolatedServer, token string) string {
	t.Helper()
	body := decodeBody(t, s.post(t, "/ui-data/user/two-factor/enrollment", token, nil), http.StatusCreated)
	secret, _ := body["secret"].(string)
	if secret == "" {
		t.Fatalf("enrollment response carried no secret: %v", body)
	}
	return secret
}

// codeFor computes the code an authenticator app holding `secret` would show
// at `at`. It is written out here, independently of the store's own generator,
// so the tests cross-check the implementation instead of comparing it with
// itself; TestTOTPGeneratorMatchesRFC6238Vectors pins THIS generator to the
// standard's published vectors, which closes the loop.
func codeFor(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatalf("decode two-factor secret: %v", err)
	}
	return totpFromKey(key, at.UTC().Unix()/30, 6)
}

// totpFromKey is RFC 6238 §4 / RFC 4226 §5.4 spelled out: HMAC-SHA1 over the
// big-endian counter, then dynamic truncation.
func totpFromKey(key []byte, counter int64, digits int) string {
	var message [8]byte
	binary.BigEndian.PutUint64(message[:], uint64(counter))
	// #nosec G401 -- RFC 6238 defines TOTP over HMAC-SHA1.
	mac := hmac.New(sha1.New, key)
	mac.Write(message[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	truncated := uint32(sum[offset]&0x7f)<<24 | uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 | uint32(sum[offset+3])
	modulus := uint32(1)
	for i := 0; i < digits; i++ {
		modulus *= 10
	}
	return fmt.Sprintf("%0*d", digits, truncated%modulus)
}

// TestTOTPGeneratorMatchesRFC6238Vectors pins the test-side generator to the
// SHA-1 vectors in RFC 6238 appendix B. Every other two-factor test derives its
// codes from this generator and feeds them to the server, so anchoring it here
// makes those tests an assertion about RFC conformance rather than about
// internal consistency.
func TestTOTPGeneratorMatchesRFC6238Vectors(t *testing.T) {
	t.Parallel()
	key := []byte("12345678901234567890")
	vectors := []struct {
		unix int64
		code string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, vector := range vectors {
		if got := totpFromKey(key, vector.unix/30, 8); got != vector.code {
			t.Errorf("TOTP at T=%d = %s, want %s", vector.unix, got, vector.code)
		}
	}
}

func twoFactorSection(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	section, ok := body["two_factor"].(map[string]interface{})
	if !ok {
		t.Fatalf("response has no two_factor section: %v", body)
	}
	return section
}

// TestTwoFactorEnrollmentIsNotAToggle is the whole point of the feature: a
// provisioned secret protects nothing until the user proves their authenticator
// holds it. Enrolment must therefore leave the account unprotected, reject a
// wrong code, and only then flip on.
func TestTwoFactorEnrollmentIsNotAToggle(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	clock := newMovableClock(s, fixedTestTime)

	secret := beginEnrollment(t, s, defaultToken)

	pending := decodeBody(t, s.get(t, "/ui-data/user/authentication", defaultToken), http.StatusOK)
	section := twoFactorSection(t, pending)
	if section["enabled"] != false {
		t.Fatal("starting enrollment must not enable two-factor authentication")
	}
	if section["pending_enrollment"] != true {
		t.Fatalf("expected a pending enrollment, got %v", section)
	}

	rejected := s.post(t, "/ui-data/user/two-factor/enrollment/confirm", defaultToken, map[string]string{"code": "000000"})
	if rejected.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("confirming with a wrong code = %d, want 422: %s", rejected.StatusCode, rawBody(t, rejected))
	}
	rejected.Body.Close()
	if s.store.TwoFactorEnabled(s.store.LookupUserByLogin("admin").ID) {
		t.Fatal("a rejected code must not enable two-factor authentication")
	}

	confirmed := decodeBody(t, s.post(t, "/ui-data/user/two-factor/enrollment/confirm", defaultToken,
		map[string]string{"code": codeFor(t, secret, clock.now())}), http.StatusOK)
	if twoFactorSection(t, confirmed)["enabled"] != true {
		t.Fatalf("a valid code must enable two-factor authentication: %v", confirmed)
	}
	codes, _ := confirmed["recovery_codes"].([]interface{})
	if len(codes) != 16 {
		t.Fatalf("got %d recovery codes, want 16", len(codes))
	}
	for _, code := range codes {
		if !strings.Contains(code.(string), "-") || len(code.(string)) != 11 {
			t.Fatalf("recovery code %q does not have the xxxxx-xxxxx shape", code)
		}
	}
}

// TestTwoFactorReadEndpointsNeverReturnSecrets holds the line the audit was
// about: after enrolment, nothing that can be replayed as a credential may come
// back out of a read.
func TestTwoFactorReadEndpointsNeverReturnSecrets(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	clock := newMovableClock(s, fixedTestTime)

	secret := beginEnrollment(t, s, defaultToken)
	confirmed := decodeBody(t, s.post(t, "/ui-data/user/two-factor/enrollment/confirm", defaultToken,
		map[string]string{"code": codeFor(t, secret, clock.now())}), http.StatusOK)
	recoveryCodes, _ := confirmed["recovery_codes"].([]interface{})

	body := rawBody(t, s.get(t, "/ui-data/user/authentication", defaultToken))
	if strings.Contains(body, secret) {
		t.Fatal("the authentication read endpoint returned the TOTP secret")
	}
	for _, code := range recoveryCodes {
		if strings.Contains(body, code.(string)) {
			t.Fatalf("the authentication read endpoint returned recovery code %v", code)
		}
	}
	// The private user view reports the state, never the material.
	private := rawBody(t, s.get(t, "/api/v3/user", defaultToken))
	if strings.Contains(private, secret) {
		t.Fatal("GET /user returned the TOTP secret")
	}
	if !strings.Contains(private, `"two_factor_authentication":true`) {
		t.Fatalf("GET /user should report two-factor as enabled: %s", private)
	}
}

// TestTOTPCodeIsSingleUseAndToleratesDrift covers the two properties RFC 6238
// leaves to the implementation: a spent counter cannot be replayed, and a code
// one step either side of ours is still accepted.
func TestTOTPCodeIsSingleUseAndToleratesDrift(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	clock := newMovableClock(s, fixedTestTime)
	admin := s.store.LookupUserByLogin("admin")

	secret := beginEnrollment(t, s, defaultToken)
	used := codeFor(t, secret, clock.now())
	decodeBody(t, s.post(t, "/ui-data/user/two-factor/enrollment/confirm", defaultToken,
		map[string]string{"code": used}), http.StatusOK)

	if result := s.store.VerifySecondFactor(admin.ID, used, clock.now()); result != store.SecurityInvalidCode {
		t.Fatalf("replaying the code that completed enrollment = %v, want rejection", result)
	}

	// A code from the previous step is inside the drift window but already
	// behind the spent counter, so it is refused too.
	stale := codeFor(t, secret, clock.now().Add(-store.TOTPPeriod))
	if result := s.store.VerifySecondFactor(admin.ID, stale, clock.now()); result != store.SecurityInvalidCode {
		t.Fatalf("a code below the spent counter = %v, want rejection", result)
	}

	// One step ahead is accepted: the phone's clock may run fast.
	ahead := codeFor(t, secret, clock.now().Add(store.TOTPPeriod))
	if result := s.store.VerifySecondFactor(admin.ID, ahead, clock.now()); result != store.SecurityOK {
		t.Fatalf("a code one step ahead = %v, want acceptance", result)
	}

	// Two steps ahead is outside the window.
	clock.advance(-store.TOTPPeriod)
	far := codeFor(t, secret, clock.now().Add(3*store.TOTPPeriod))
	if result := s.store.VerifySecondFactor(admin.ID, far, clock.now()); result != store.SecurityInvalidCode {
		t.Fatalf("a code three steps ahead = %v, want rejection", result)
	}
}

// TestRecoveryCodesAreSingleUseAndRegenerable exercises the fallback path end
// to end: a recovery code stands in for a TOTP code exactly once, and
// regenerating invalidates the whole previous set.
func TestRecoveryCodesAreSingleUseAndRegenerable(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	clock := newMovableClock(s, fixedTestTime)

	secret := beginEnrollment(t, s, defaultToken)
	confirmed := decodeBody(t, s.post(t, "/ui-data/user/two-factor/enrollment/confirm", defaultToken,
		map[string]string{"code": codeFor(t, secret, clock.now())}), http.StatusOK)
	codes, _ := confirmed["recovery_codes"].([]interface{})
	first := codes[0].(string)

	// One recovery code buys one privileged operation.
	regenerated := decodeBody(t, s.post(t, "/ui-data/user/two-factor/recovery-codes", defaultToken,
		map[string]string{"code": first}), http.StatusOK)
	fresh, _ := regenerated["recovery_codes"].([]interface{})
	if len(fresh) != 16 {
		t.Fatalf("regenerated %d codes, want 16", len(fresh))
	}
	status := twoFactorSection(t, regenerated)
	if status["recovery_codes_remaining"] != float64(16) {
		t.Fatalf("regeneration should restore a full set: %v", status)
	}

	// The spent code is gone, and so is every other code from the old set.
	replay := s.post(t, "/ui-data/user/two-factor/recovery-codes", defaultToken, map[string]string{"code": first})
	if replay.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("replaying a spent recovery code = %d, want 422", replay.StatusCode)
	}
	replay.Body.Close()
	survivor := s.post(t, "/ui-data/user/two-factor/recovery-codes", defaultToken, map[string]string{"code": codes[1].(string)})
	if survivor.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("an unused code from the replaced set = %d, want 422", survivor.StatusCode)
	}
	survivor.Body.Close()

	// A code from the new set works, proving regeneration really issued them.
	decodeBody(t, s.post(t, "/ui-data/user/two-factor/disable", defaultToken,
		map[string]string{"code": fresh[0].(string)}), http.StatusOK)
	if s.store.TwoFactorEnabled(s.store.LookupUserByLogin("admin").ID) {
		t.Fatal("disabling with a valid recovery code should have turned two-factor off")
	}
}

// TestDisablingTwoFactorRequiresProof: a click is not enough. Turning the
// protection off is exactly what a stolen session wants to do.
func TestDisablingTwoFactorRequiresProof(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	clock := newMovableClock(s, fixedTestTime)

	secret := beginEnrollment(t, s, defaultToken)
	decodeBody(t, s.post(t, "/ui-data/user/two-factor/enrollment/confirm", defaultToken,
		map[string]string{"code": codeFor(t, secret, clock.now())}), http.StatusOK)

	for _, attempt := range []map[string]string{{}, {"code": ""}, {"code": "123456"}} {
		resp := s.post(t, "/ui-data/user/two-factor/disable", defaultToken, attempt)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("disable with %v = %d, want 422", attempt, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if !s.store.TwoFactorEnabled(s.store.LookupUserByLogin("admin").ID) {
		t.Fatal("failed disable attempts must leave two-factor enabled")
	}

	clock.advance(store.TOTPPeriod)
	decodeBody(t, s.post(t, "/ui-data/user/two-factor/disable", defaultToken,
		map[string]string{"code": codeFor(t, secret, clock.now())}), http.StatusOK)
	if s.store.TwoFactorEnabled(s.store.LookupUserByLogin("admin").ID) {
		t.Fatal("a valid code should have disabled two-factor authentication")
	}
}

// TestPendingEnrollmentExpires: an abandoned pairing must not leave a live
// secret sitting on the account.
func TestPendingEnrollmentExpires(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	clock := newMovableClock(s, fixedTestTime)

	secret := beginEnrollment(t, s, defaultToken)
	clock.advance(20 * time.Minute)
	resp := s.post(t, "/ui-data/user/two-factor/enrollment/confirm", defaultToken,
		map[string]string{"code": codeFor(t, secret, clock.now())})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("confirming an expired enrollment = %d, want 409: %s", resp.StatusCode, rawBody(t, resp))
	}
	resp.Body.Close()

	status := twoFactorSection(t, decodeBody(t, s.get(t, "/ui-data/user/authentication", defaultToken), http.StatusOK))
	if status["enabled"] != false || status["pending_enrollment"] != false {
		t.Fatalf("an expired enrollment should be cleared: %v", status)
	}
}

// TestCancelTwoFactorEnrollment: abandoning the flow discards the secret.
func TestCancelTwoFactorEnrollment(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	newMovableClock(s, fixedTestTime)

	beginEnrollment(t, s, defaultToken)
	body := decodeBody(t, s.delete(t, "/ui-data/user/two-factor/enrollment", defaultToken), http.StatusOK)
	if twoFactorSection(t, body)["pending_enrollment"] != false {
		t.Fatalf("cancelling should clear the pending enrollment: %v", body)
	}
	again := s.delete(t, "/ui-data/user/two-factor/enrollment", defaultToken)
	if again.StatusCode != http.StatusConflict {
		t.Fatalf("cancelling with nothing pending = %d, want 409", again.StatusCode)
	}
	again.Body.Close()
}

// TestExternalAccountsGetAnHonestAnswer: an account whose credentials belong to
// an identity provider is told so, instead of being shown controls that cannot
// work.
func TestExternalAccountsGetAnHonestAnswer(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	newMovableClock(s, fixedTestTime)
	user, token := s.newUser(t, "federated")
	s.store.Mu.Lock()
	user.ExternalIdentities = []store.ExternalIdentity{{Issuer: "https://idp.example", Subject: "subject-1"}}
	s.store.Mu.Unlock()

	view := decodeBody(t, s.get(t, "/ui-data/user/authentication", token), http.StatusOK)
	authentication, _ := view["authentication"].(map[string]interface{})
	if authentication["kind"] != string(store.AccountAuthExternal) {
		t.Fatalf("expected an external account, got %v", authentication)
	}
	providers, _ := authentication["providers"].([]interface{})
	if len(providers) != 1 || providers[0] != "https://idp.example" {
		t.Fatalf("the page must name the identity provider: %v", authentication)
	}

	enroll := s.post(t, "/ui-data/user/two-factor/enrollment", token, nil)
	if enroll.StatusCode != http.StatusConflict {
		t.Fatalf("enrolling on a federated account = %d, want 409", enroll.StatusCode)
	}
	enroll.Body.Close()

	password := s.put(t, "/ui-data/user/password", token,
		map[string]string{"current_password": "", "new_password": "a-perfectly-fine-password"})
	if password.StatusCode != http.StatusConflict {
		t.Fatalf("changing the password of a federated account = %d, want 409", password.StatusCode)
	}
	password.Body.Close()
}

// TestPasswordChange covers the local case: the current password is required,
// the new one has to clear github.com's own strength rule, and the change
// takes effect for sign-in.
func TestPasswordChange(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	newMovableClock(s, fixedTestTime)
	user, token := s.newUser(t, "local-account")
	existing, err := bcrypt.GenerateFromPassword([]byte("original-password-1"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	s.store.Mu.Lock()
	user.PasswordHash = string(existing)
	s.store.Mu.Unlock()

	wrong := s.put(t, "/ui-data/user/password", token,
		map[string]string{"current_password": "not-it", "new_password": "replacement-password-1"})
	if wrong.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a wrong current password = %d, want 422", wrong.StatusCode)
	}
	wrong.Body.Close()

	weak := s.put(t, "/ui-data/user/password", token,
		map[string]string{"current_password": "original-password-1", "new_password": "short"})
	if weak.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a too-short password = %d, want 422", weak.StatusCode)
	}
	weak.Body.Close()

	decodeBody(t, s.put(t, "/ui-data/user/password", token,
		map[string]string{"current_password": "original-password-1", "new_password": "replacement-password-1"}), http.StatusOK)

	hash, _ := s.store.UserPasswordHash(user.ID)
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("replacement-password-1")) != nil {
		t.Fatal("the stored hash does not verify the new password")
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("original-password-1")) == nil {
		t.Fatal("the old password still verifies")
	}
}

func TestPasswordStrengthRule(t *testing.T) {
	t.Parallel()
	cases := []struct {
		password   string
		acceptable bool
	}{
		{"short", false},
		{"PASSWORD", false},           // eight characters but no number and no lowercase
		{"passwordd", false},          // nine lowercase, no number
		{"password1", true},           // eight-plus with a number and a lowercase letter
		{"ALLUPPERCASE1234567", true}, // fifteen or more needs nothing else
	}
	for _, testCase := range cases {
		if _, ok := passwordAcceptable(testCase.password); ok != testCase.acceptable {
			t.Errorf("passwordAcceptable(%q) = %v, want %v", testCase.password, ok, testCase.acceptable)
		}
	}
}

// TestChangingThePasswordRevokesOtherSessions: a password change made because
// of a suspected compromise has to actually evict the intruder.
func TestChangingThePasswordRevokesOtherSessions(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	clock := newMovableClock(s, fixedTestTime)
	user, token := s.newUser(t, "session-owner")
	existing, err := bcrypt.GenerateFromPassword([]byte("original-password-1"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	s.store.Mu.Lock()
	user.PasswordHash = string(existing)
	s.store.Mu.Unlock()

	for _, handle := range []string{"session-a", "session-b"} {
		if err := s.store.PutLoginSession("cookie-"+handle, &store.LoginSession{
			UserID:    user.ID,
			Handle:    handle,
			CreatedAt: clock.now(),
			ExpiresAt: clock.now().Add(time.Hour),
			UserAgent: "Test/1.0",
		}); err != nil {
			t.Fatal(err)
		}
	}
	sessions, err := s.store.ListLoginSessionsForUser(user.ID, clock.now())
	if err != nil || len(sessions) != 2 {
		t.Fatalf("expected two sessions, got %d (%v)", len(sessions), err)
	}

	decodeBody(t, s.put(t, "/ui-data/user/password", token,
		map[string]string{"current_password": "original-password-1", "new_password": "replacement-password-1"}), http.StatusOK)

	remaining, err := s.store.ListLoginSessionsForUser(user.ID, clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("password change left %d session(s) alive: %v", len(remaining), remaining)
	}
}

// TestSessionListAndRevocation: the list names sessions by a handle that is not
// the cookie, and revoking one by handle ends it.
func TestSessionListAndRevocation(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	clock := newMovableClock(s, fixedTestTime)
	user, token := s.newUser(t, "session-lister")

	if err := s.store.PutLoginSession("secret-cookie-value", &store.LoginSession{
		UserID:     user.ID,
		Handle:     "handle-1",
		CreatedAt:  clock.now(),
		ExpiresAt:  clock.now().Add(time.Hour),
		UserAgent:  "Mozilla/5.0 (Test)",
		SignedInIP: "192.0.2.7",
	}); err != nil {
		t.Fatal(err)
	}
	// An expired session is not "active".
	if err := s.store.PutLoginSession("expired-cookie", &store.LoginSession{
		UserID:    user.ID,
		Handle:    "handle-expired",
		CreatedAt: clock.now().Add(-2 * time.Hour),
		ExpiresAt: clock.now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	listed := rawBody(t, s.get(t, "/ui-data/user/sessions", token))
	if strings.Contains(listed, "secret-cookie-value") {
		t.Fatal("the sessions list disclosed the session cookie value")
	}
	if !strings.Contains(listed, "handle-1") || !strings.Contains(listed, "Mozilla/5.0 (Test)") || !strings.Contains(listed, "192.0.2.7") {
		t.Fatalf("the sessions list is missing its own row: %s", listed)
	}
	if strings.Contains(listed, "handle-expired") {
		t.Fatalf("an expired session was listed as active: %s", listed)
	}

	revoked := s.delete(t, "/ui-data/user/sessions/handle-1", token)
	if revoked.StatusCode != http.StatusNoContent {
		t.Fatalf("revoking a session = %d, want 204", revoked.StatusCode)
	}
	revoked.Body.Close()
	if session, err := s.store.GetLoginSession("secret-cookie-value"); err != nil || session != nil {
		t.Fatalf("the revoked session is still live (%v, %v)", session, err)
	}

	missing := s.delete(t, "/ui-data/user/sessions/handle-1", token)
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("revoking an unknown handle = %d, want 404", missing.StatusCode)
	}
	missing.Body.Close()
}

// TestOneAccountCannotRevokeAnothersSession: handles are scoped to their owner.
func TestOneAccountCannotRevokeAnothersSession(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	clock := newMovableClock(s, fixedTestTime)
	victim, _ := s.newUser(t, "session-victim")
	_, attackerToken := s.newUser(t, "session-attacker")

	if err := s.store.PutLoginSession("victim-cookie", &store.LoginSession{
		UserID:    victim.ID,
		Handle:    "victim-handle",
		CreatedAt: clock.now(),
		ExpiresAt: clock.now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	resp := s.delete(t, "/ui-data/user/sessions/victim-handle", attackerToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revoking another account's session = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	if session, _ := s.store.GetLoginSession("victim-cookie"); session == nil {
		t.Fatal("another account's session was revoked")
	}
}

// TestSignInRequiresTheSecondFactorOnceEnrolled: the enrolment has to mean
// something at the door, or it is decoration.
func TestSignInRequiresTheSecondFactorOnceEnrolled(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	clock := newMovableClock(s, fixedTestTime)
	user, token := s.newUser(t, "gated-signin")
	existing, err := bcrypt.GenerateFromPassword([]byte("original-password-1"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	s.store.Mu.Lock()
	user.PasswordHash = string(existing)
	s.store.Mu.Unlock()

	secret := beginEnrollment(t, s, token)
	decodeBody(t, s.post(t, "/ui-data/user/two-factor/enrollment/confirm", token,
		map[string]string{"code": codeFor(t, secret, clock.now())}), http.StatusOK)

	withoutCode := s.post(t, "/auth/local", "", map[string]string{"login": "gated-signin", "password": "original-password-1"})
	if withoutCode.StatusCode != http.StatusUnauthorized {
		t.Fatalf("signing in without a code = %d, want 401", withoutCode.StatusCode)
	}
	if got := withoutCode.Header.Get("X-GitHub-OTP"); got != "required; app" {
		t.Fatalf("X-GitHub-OTP = %q, want \"required; app\"", got)
	}
	if len(withoutCode.Cookies()) != 0 {
		t.Fatal("a sign-in missing the second factor must not set a session cookie")
	}
	withoutCode.Body.Close()

	clock.advance(store.TOTPPeriod)
	withCode := s.post(t, "/auth/local", "", map[string]string{
		"login":    "gated-signin",
		"password": "original-password-1",
		"otp":      codeFor(t, secret, clock.now()),
	})
	if withCode.StatusCode != http.StatusOK {
		t.Fatalf("signing in with a code = %d, want 200: %s", withCode.StatusCode, rawBody(t, withCode))
	}
	withCode.Body.Close()
}

// TestTokenExchangeRequiresTheSecondFactor: a personal access token must not be
// a way around the second factor the account owner just enrolled.
func TestTokenExchangeRequiresTheSecondFactor(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	clock := newMovableClock(s, fixedTestTime)

	secret := beginEnrollment(t, s, defaultToken)
	decodeBody(t, s.post(t, "/ui-data/user/two-factor/enrollment/confirm", defaultToken,
		map[string]string{"code": codeFor(t, secret, clock.now())}), http.StatusOK)

	blocked := s.post(t, "/auth/token", defaultToken, nil)
	if blocked.StatusCode != http.StatusUnauthorized {
		t.Fatalf("exchanging a token without a code = %d, want 401", blocked.StatusCode)
	}
	blocked.Body.Close()

	clock.advance(store.TOTPPeriod)
	request, err := http.NewRequest(http.MethodPost, s.baseURL+"/auth/token", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "token "+defaultToken)
	request.Header.Set("X-GitHub-OTP", codeFor(t, secret, clock.now()))
	allowed, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if allowed.StatusCode != http.StatusOK {
		t.Fatalf("exchanging a token with X-GitHub-OTP = %d, want 200: %s", allowed.StatusCode, rawBody(t, allowed))
	}
	allowed.Body.Close()
}

// TestProvisioningURIIsScannable checks the enrolment response carries a
// standards-shaped otpauth URI and a QR matrix that decodes back to it — a QR
// code the user cannot scan is the same as no QR code.
func TestProvisioningURIIsScannable(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	newMovableClock(s, fixedTestTime)

	body := decodeBody(t, s.post(t, "/ui-data/user/two-factor/enrollment", defaultToken, nil), http.StatusCreated)
	uri, _ := body["otpauth_uri"].(string)
	secret, _ := body["secret"].(string)
	if !strings.HasPrefix(uri, "otpauth://totp/") {
		t.Fatalf("provisioning URI %q is not an otpauth TOTP URI", uri)
	}
	for _, want := range []string{"secret=" + secret, "algorithm=SHA1", "digits=6", "period=30", "issuer="} {
		if !strings.Contains(uri, want) {
			t.Errorf("provisioning URI %q is missing %q", uri, want)
		}
	}
	qr, ok := body["qr"].(map[string]interface{})
	if !ok {
		t.Fatal("enrollment response carried no QR matrix")
	}
	rawModules, _ := qr["modules"].([]interface{})
	rows := make([]string, len(rawModules))
	for i, row := range rawModules {
		rows[i] = row.(string)
	}
	decoded, err := decodeQRForTest(rows)
	if err != nil {
		t.Fatalf("decode the enrollment QR code: %v", err)
	}
	if decoded != uri {
		t.Fatalf("the QR code encodes %q, want the provisioning URI %q", decoded, uri)
	}
}

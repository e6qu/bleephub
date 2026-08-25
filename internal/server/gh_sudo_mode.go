package bleephub

// Sudo mode: GitHub's proof-of-presence gate.
//
// An enterprise sets ProofOfPresenceRequirement (GitHub's enum: NO_POLICY,
// MFA, REAUTH). With a requirement in force, a sensitive action — changing the
// account's security settings, deleting a repository or an organization,
// managing keys or tokens — is refused unless the browser session behind it
// proved presence recently.
//
// Three rules follow from GitHub's own semantics.
//
// First, the elevation belongs to the *session*, not the account: proving
// presence in one browser must not unlock a sensitive action in another, which
// is why the timestamp lives on LoginSession rather than on User.
//
// Second, MFA and REAUTH are different proofs. REAUTH is any fresh
// authentication — a password re-entry, or simply having just signed in.
// MFA additionally requires that the proof carried a second factor, so a
// password alone never satisfies it.
//
// Third, sudo mode governs the browser surface. A request authenticated by a
// personal access token or an app installation presents a credential that was
// itself issued deliberately; GitHub does not interpose a sudo challenge on
// it, and neither does this. The gate is therefore inert for API-token traffic
// and live for cookie traffic, which is exactly where the sensitive settings
// pages live.

import (
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/e6qu/bleephub/internal/store"
)

// sudoModeWindow is how long one proof of presence lasts. GitHub keeps a sudo
// elevation alive for a few hours rather than for the session's whole life, so
// an unattended browser stops being able to perform sensitive actions well
// before the session itself expires.
const sudoModeWindow = 3 * time.Hour

func (s *Server) registerGHSudoModeRoutes() {
	s.route("GET /ui-data/user/sudo", s.handleGetSudoState)
	// The challenge consumes a password or a one-time code, so it shares the
	// auth-flow limiter with the other credential-verifying endpoints.
	s.route("POST /ui-data/user/sudo", s.rateLimitAuthFlow(s.handleCreateSudoGrant))
}

// proofOfPresenceRequirement reports the requirement the instance's enterprise
// imposes on a principal: "" / NO_POLICY when it imposes none.
//
// Unlike the prohibiting policies, an enterprise owner is not exempt. A
// proof-of-presence requirement is not a restriction on what a member may do —
// it is a demand that whoever is acting prove they are present — and exempting
// the accounts with the most authority would invert its purpose.
func (s *Server) proofOfPresenceRequirement(user *store.User) string {
	if user == nil {
		return store.EnterprisePolicyNoPolicy
	}
	e := s.primaryEnterprise()
	if e == nil {
		return store.EnterprisePolicyNoPolicy
	}
	switch e.Policy.ProofOfPresenceRequired {
	case store.EnterpriseProofOfPresenceMFA:
		return store.EnterpriseProofOfPresenceMFA
	case store.EnterpriseProofOfPresenceReauth:
		return store.EnterpriseProofOfPresenceReauth
	}
	return store.EnterprisePolicyNoPolicy
}

// browserSessionForRequest returns the login session a request is actually
// authenticated by, or nil. A request that offered an Authorization header is
// judged on that credential alone (the same rule authenticateRequest applies),
// so a stray cookie cannot make a token request look like a browser one.
func (s *Server) browserSessionForRequest(r *http.Request) *store.LoginSession {
	if r.Header.Get("Authorization") != "" {
		return nil
	}
	return s.sessionFromRequest(r)
}

// proofOfPresenceGate resolves the requirement in force, the session it would
// be checked against, and whether it applies to this request at all. It is the
// single place that decides "does sudo mode govern this?", so the gate and the
// state endpoint can never disagree about it.
func (s *Server) proofOfPresenceGate(r *http.Request) (requirement string, session *store.LoginSession, applies bool) {
	requirement = s.proofOfPresenceRequirement(ghUserFromContext(r.Context()))
	if requirement == store.EnterprisePolicyNoPolicy {
		return requirement, nil, false
	}
	session = s.browserSessionForRequest(r)
	return requirement, session, session != nil
}

// sudoGrantSatisfies reports whether a session's recorded proof meets the
// requirement at time now.
func sudoGrantSatisfies(session *store.LoginSession, requirement string, now time.Time) bool {
	if session == nil || session.SudoAt.IsZero() {
		return false
	}
	if now.Sub(session.SudoAt) >= sudoModeWindow {
		return false
	}
	return requirement != store.EnterpriseProofOfPresenceMFA || session.SudoMFA
}

// requireProofOfPresence refuses a sensitive action whose session has not
// proved presence recently, and reports whether it wrote the refusal. The
// response names the challenge endpoint so a client knows what to do next
// rather than being told only that it may not proceed.
func (s *Server) requireProofOfPresence(w http.ResponseWriter, r *http.Request) bool {
	requirement, session, applies := s.proofOfPresenceGate(r)
	if !applies || sudoGrantSatisfies(session, requirement, s.currentTime()) {
		return false
	}
	w.Header().Set("X-GitHub-Sudo", "required; "+sudoChallengeKind(requirement))
	writeGHError(w, http.StatusForbidden,
		"This action requires a recent re-authentication. Confirm your access at /ui-data/user/sudo and try again.")
	return true
}

// sudoChallengeKind names the proof the challenge will accept, in the shape
// GitHub's own OTP header uses: a token the client can branch on.
func sudoChallengeKind(requirement string) string {
	if requirement == store.EnterpriseProofOfPresenceMFA {
		return "mfa"
	}
	return "password"
}

// sudoStateJSON is the read view: what is required, whether this session has
// already proved it, and when that proof lapses.
func (s *Server) sudoStateJSON(r *http.Request) map[string]interface{} {
	requirement, session, applies := s.proofOfPresenceGate(r)
	state := map[string]interface{}{
		"requirement": requirement,
		// `required` is what this request must satisfy, not what the enterprise
		// asks of the browser: a token credential is outside sudo mode, and
		// saying otherwise would describe a challenge it can never answer.
		"required":       applies,
		"challenge":      sudoChallengeKind(requirement),
		"window_seconds": int(sudoModeWindow.Seconds()),
		"active":         !applies || sudoGrantSatisfies(session, requirement, s.currentTime()),
	}
	if session != nil && !session.SudoAt.IsZero() {
		state["confirmed_at"] = session.SudoAt
		state["expires_at"] = session.SudoAt.Add(sudoModeWindow)
		state["confirmed_with_second_factor"] = session.SudoMFA
	}
	return state
}

func (s *Server) handleGetSudoState(w http.ResponseWriter, r *http.Request) {
	if ghUserFromContext(r.Context()) == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	writeJSON(w, http.StatusOK, s.sudoStateJSON(r))
}

// handleCreateSudoGrant is the challenge itself. It takes whichever proof the
// requirement asks for — a one-time code for MFA, the account password for
// REAUTH — verifies it, and stamps the session.
//
// The grant is written against the cookie the request presented, so a client
// that cannot produce the cookie cannot elevate somebody else's session.
func (s *Server) handleCreateSudoGrant(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if s.crossSiteBrowserPost(r) {
		writeGHError(w, http.StatusForbidden, "cross-origin request denied")
		return
	}
	cookie := s.sessionCookieFromRequest(r)
	if cookie == nil || s.browserSessionForRequest(r) == nil {
		writeGHError(w, http.StatusForbidden,
			"Proof of presence applies to a browser session; this request has none to elevate.")
		return
	}
	var req struct {
		Code     string `json:"code"`
		Password string `json:"password"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	requirement := s.proofOfPresenceRequirement(viewer)
	withSecondFactor, ok := s.verifyProofOfPresence(w, viewer, requirement, req.Code, req.Password)
	if !ok {
		return
	}
	granted, err := s.store.MarkLoginSessionSudo(cookie.Value, s.currentTime(), withSecondFactor)
	if err != nil {
		s.logger.Error().Err(err).Msg("record proof of presence")
		writeGHError(w, http.StatusServiceUnavailable, "browser sessions are unavailable")
		return
	}
	if !granted {
		writeGHError(w, http.StatusForbidden,
			"Proof of presence applies to a browser session; this request has none to elevate.")
		return
	}
	writeJSON(w, http.StatusOK, s.sudoStateJSON(r))
}

// verifyProofOfPresence checks the proof the caller supplied against what the
// requirement demands. It returns whether the proof carried a second factor,
// and whether it verified at all; a failure has already been written.
//
// A code is always accepted when the account has a second factor, whatever the
// requirement: proving possession of the authenticator is a strictly stronger
// answer than re-entering a password, and refusing it would push people onto
// the weaker proof.
func (s *Server) verifyProofOfPresence(w http.ResponseWriter, viewer *store.User, requirement, code, password string) (bool, bool) {
	if code != "" {
		result, _ := s.store.VerifySecondFactorExcluding(viewer.ID, code, s.currentTime(),
			s.enterpriseDisallowedTwoFactorMethods(viewer))
		if result != store.SecurityOK {
			writeAccountSecurityError(w, result)
			return false, false
		}
		return true, true
	}
	if requirement == store.EnterpriseProofOfPresenceMFA {
		// The enterprise asked for an MFA re-authentication; no password can
		// stand in for the proof that was demanded. Which of the two things is
		// wrong — no code supplied, or no authenticator to supply one with —
		// decides which answer is useful.
		if s.store.TwoFactorEnabled(viewer.ID) {
			writeGHError(w, http.StatusForbidden,
				"An enterprise policy requires a second factor for this action. Enter your authenticator's code.")
		} else {
			writeGHError(w, http.StatusForbidden,
				"An enterprise policy requires a second factor for this action. Enroll an authenticator app first.")
		}
		return false, false
	}
	hash, ok := s.store.UserPasswordHash(viewer.ID)
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return false, false
	}
	if hash == "" {
		// Credentials this instance does not hold cannot be re-checked here;
		// the fresh authentication that proves presence is a new sign-in, which
		// stamps the session on its way through.
		writeGHError(w, http.StatusConflict,
			"This account has no local password. Sign in again to re-authenticate.")
		return false, false
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Your password is incorrect.")
		return false, false
	}
	return false, true
}

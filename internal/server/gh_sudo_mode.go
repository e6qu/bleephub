package bleephub

// Sudo mode: GitHub's proof-of-presence gate. An enterprise's
// ProofOfPresenceRequirement (NO_POLICY, MFA, REAUTH) makes sensitive actions
// refuse unless the browser session proved presence recently. Three invariants:
// the elevation lives on the session, not the account (timestamp on
// LoginSession); MFA demands a second factor while REAUTH accepts any fresh
// authentication; and the gate governs cookie traffic only — token and
// installation credentials, issued deliberately, are never challenged.

import (
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/e6qu/bleephub/internal/store"
)

// sudoModeWindow is how long one proof of presence lasts — shorter than the
// session's lifetime, so an unattended browser loses sensitive access first.
const sudoModeWindow = 3 * time.Hour

func (s *Server) registerGHSudoModeRoutes() {
	s.route("GET /ui-data/user/sudo", s.handleGetSudoState)
	// The challenge verifies a credential, so it shares the auth-flow limiter.
	s.route("POST /ui-data/user/sudo", s.rateLimitAuthFlow(s.handleCreateSudoGrant))
}

// proofOfPresenceRequirement reports the enterprise requirement on a principal
// (NO_POLICY when none). Enterprise owners are not exempt: the requirement
// demands presence, not authority, so exempting the most privileged accounts
// would invert its purpose.
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

// browserSessionForRequest returns the login session a request authenticates
// by, or nil. A request bearing an Authorization header is judged on that
// credential alone, so a stray cookie cannot make a token request look browser-based.
func (s *Server) browserSessionForRequest(r *http.Request) *store.LoginSession {
	if r.Header.Get("Authorization") != "" {
		return nil
	}
	return s.sessionFromRequest(r)
}

// proofOfPresenceGate resolves the requirement in force, the session to check
// it against, and whether it applies. The single decision point, so the gate
// and the state endpoint cannot disagree.
func (s *Server) proofOfPresenceGate(r *http.Request) (requirement string, session *store.LoginSession, applies bool) {
	requirement = s.proofOfPresenceRequirement(ghUserFromContext(r.Context()))
	if requirement == store.EnterprisePolicyNoPolicy {
		return requirement, nil, false
	}
	session = s.browserSessionForRequest(r)
	return requirement, session, session != nil
}

// sudoGrantSatisfies reports whether a session's recorded proof meets the
// requirement at now.
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
// proved presence recently, naming the challenge endpoint, and reports whether
// it wrote the refusal.
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

// sudoChallengeKind names the proof the challenge accepts, as a token the
// client can branch on.
func sudoChallengeKind(requirement string) string {
	if requirement == store.EnterpriseProofOfPresenceMFA {
		return "mfa"
	}
	return "password"
}

// sudoStateJSON is the read view: what is required, whether this session proved
// it, and when the proof lapses.
func (s *Server) sudoStateJSON(r *http.Request) map[string]interface{} {
	requirement, session, applies := s.proofOfPresenceGate(r)
	state := map[string]interface{}{
		"requirement": requirement,
		// `required` is what this request must satisfy: a token credential is
		// outside sudo mode and could never answer the challenge.
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

// handleCreateSudoGrant verifies the required proof (an MFA code or the account
// password) and stamps the session. The grant is written against the request's
// cookie, so a client without it cannot elevate another session.
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

// verifyProofOfPresence checks the supplied proof and returns (carriedSecond
// factor, verified); a failure has already been written. A code is accepted
// whenever the account has a second factor, whatever the requirement, since it
// is a strictly stronger answer than the password.
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
		// MFA was demanded; no password substitutes. Whether an authenticator
		// exists decides which message helps.
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
		// With no local password, the presence proof is a new sign-in, which
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

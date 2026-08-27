package bleephub

import (
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"

	"github.com/e6qu/bleephub/internal/store"
)

// Account security ("Password and authentication") routes: TOTP enrolment,
// recovery codes, password change, and active browser sessions. Enrolment is
// two-phase — a secret provisions as pending and only protects the account once
// the user proves a valid code; disabling likewise costs a code. Accounts backed
// by an identity provider have no local password or second factor to manage.
func (s *Server) registerGHAccountSecurityRoutes() {
	s.route("GET /ui-data/user/authentication", s.handleGetAuthenticationSettings)
	// Code-verifying endpoints share the auth-flow limiter: a six-digit code is
	// guessable at HTTP speed if unbounded.
	s.route("POST /ui-data/user/two-factor/enrollment", s.handleBeginTwoFactorEnrollment)
	s.route("DELETE /ui-data/user/two-factor/enrollment", s.handleCancelTwoFactorEnrollment)
	s.route("POST /ui-data/user/two-factor/enrollment/confirm", s.rateLimitAuthFlow(s.handleConfirmTwoFactorEnrollment))
	s.route("POST /ui-data/user/two-factor/disable", s.rateLimitAuthFlow(s.handleDisableTwoFactor))
	s.route("POST /ui-data/user/two-factor/recovery-codes", s.rateLimitAuthFlow(s.handleRegenerateRecoveryCodes))
	s.route("PUT /ui-data/user/password", s.rateLimitAuthFlow(s.handleChangePassword))
	s.route("GET /ui-data/user/sessions", s.handleListLoginSessions)
	s.route("DELETE /ui-data/user/sessions/{handle}", s.handleRevokeLoginSession)
}

// otpRequest carries a TOTP code or one recovery code in the same field; the
// verifier accepts either.
type otpRequest struct {
	Code string `json:"code"`
}

// authenticationSettingsJSON is the page read view; it carries no secret or
// recovery code.
func (s *Server) authenticationSettingsJSON(userID int) (map[string]interface{}, bool) {
	authentication, ok := s.store.AccountAuthenticationFor(userID)
	if !ok {
		return nil, false
	}
	status, ok := s.store.TwoFactorStatusFor(userID, s.currentTime())
	if !ok {
		return nil, false
	}
	return map[string]interface{}{
		"authentication":     authentication,
		"two_factor":         status,
		"two_factor_methods": s.twoFactorMethodsJSON(s.store.GetUserByID(userID)),
	}, true
}

// twoFactorMethodsJSON reports the supported second factors and which the
// enterprise policy currently disallows.
func (s *Server) twoFactorMethodsJSON(user *store.User) []map[string]interface{} {
	disallowed := s.enterpriseDisallowedTwoFactorMethods(user)
	described := store.SupportedTwoFactorMethods()
	out := make([]map[string]interface{}, 0, len(described))
	for _, method := range described {
		banned := false
		for _, name := range disallowed {
			if name == method.Method {
				banned = true
			}
		}
		out = append(out, map[string]interface{}{
			"method":     string(method.Method),
			"insecure":   method.Insecure,
			"summary":    method.Summary,
			"disallowed": banned,
		})
	}
	return out
}

func (s *Server) handleGetAuthenticationSettings(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	payload, ok := s.authenticationSettingsJSON(viewer.ID)
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// writeAccountSecurityError maps a store outcome to the matching GitHub error
// response, with a message the page shows verbatim.
func writeAccountSecurityError(w http.ResponseWriter, result store.AccountSecurityResult) {
	switch result {
	case store.SecurityUnknownUser:
		writeGHError(w, http.StatusNotFound, "Not Found")
	case store.SecurityExternalAccount:
		writeGHError(w, http.StatusConflict,
			"Authentication for this account is managed by its identity provider.")
	case store.SecurityTwoFactorAlreadyEnabled:
		writeGHError(w, http.StatusConflict,
			"Two-factor authentication is already enabled. Disable it before enrolling a new authenticator.")
	case store.SecurityTwoFactorNotEnabled:
		writeGHError(w, http.StatusConflict, "Two-factor authentication is not enabled for this account.")
	case store.SecurityNoPendingEnrollment:
		writeGHError(w, http.StatusConflict,
			"There is no two-factor enrollment in progress. Start again to scan a new code.")
	case store.SecurityInvalidCode:
		// Plain message, not a field-validation envelope: shown to someone who
		// mistyped six digits.
		writeGHError(w, http.StatusUnprocessableEntity,
			"That code is not valid. Check your authenticator app — or use a recovery code — and try again.")
	case store.SecurityMethodDisallowed:
		writeGHError(w, http.StatusForbidden,
			"That second-factor method is disallowed by an enterprise policy. Use your authenticator app instead.")
	case store.SecurityInternalError:
		writeGHError(w, http.StatusServiceUnavailable, "Two-factor enrollment is temporarily unavailable.")
	default:
		writeGHError(w, http.StatusInternalServerError, "Unexpected account security result")
	}
}

// handleBeginTwoFactorEnrollment provisions a pending secret and returns the
// otpauth:// URI, QR modules, and the secret for manual entry. This is the only
// response that carries the secret, and the account is not yet protected.
func (s *Server) handleBeginTwoFactorEnrollment(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if s.crossSiteBrowserPost(r) {
		writeGHError(w, http.StatusForbidden, "cross-origin request denied")
		return
	}
	// Sudo-mode guard: changing authentication is a sensitive action.
	if s.requireProofOfPresence(w, r) {
		return
	}
	secret, result := s.store.BeginTwoFactorEnrollment(viewer.ID, s.currentTime())
	if result != store.SecurityOK {
		writeAccountSecurityError(w, result)
		return
	}
	uri := store.OTPAuthURI(s.twoFactorIssuer(), viewer.Login, secret)
	modules, err := qrEncode(uri)
	if err != nil {
		// Fall back to manual entry rather than failing the enrolment.
		s.logger.Error().Err(err).Msg("render two-factor provisioning QR code")
		modules = nil
	}
	payload := map[string]interface{}{
		"secret":      secret,
		"otpauth_uri": uri,
		"digits":      store.TOTPDigits,
		"period":      int(store.TOTPPeriod.Seconds()),
		"algorithm":   "SHA1",
		"account":     viewer.Login,
		"issuer":      s.twoFactorIssuer(),
	}
	if modules != nil {
		payload["qr"] = map[string]interface{}{"size": len(modules), "modules": modules}
	}
	writeJSON(w, http.StatusCreated, payload)
}

// twoFactorIssuer names this deployment in the authenticator label so a user
// enrolled on several instances can tell them apart.
func (s *Server) twoFactorIssuer() string {
	external := strings.TrimSpace(s.externalURL)
	if external == "" {
		return "bleephub"
	}
	trimmed := strings.TrimPrefix(strings.TrimPrefix(external, "https://"), "http://")
	trimmed = strings.TrimSuffix(trimmed, "/")
	if host, _, err := net.SplitHostPort(trimmed); err == nil && host != "" {
		trimmed = host
	}
	if trimmed == "" {
		return "bleephub"
	}
	// A colon would split the otpauth label into issuer and account.
	return "bleephub (" + strings.ReplaceAll(trimmed, ":", "-") + ")"
}

func (s *Server) handleCancelTwoFactorEnrollment(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if s.crossSiteBrowserPost(r) {
		writeGHError(w, http.StatusForbidden, "cross-origin request denied")
		return
	}
	// Sudo-mode guard: changing authentication is a sensitive action.
	if s.requireProofOfPresence(w, r) {
		return
	}
	if result := s.store.CancelTwoFactorEnrollment(viewer.ID, s.currentTime()); result != store.SecurityOK {
		writeAccountSecurityError(w, result)
		return
	}
	payload, _ := s.authenticationSettingsJSON(viewer.ID)
	writeJSON(w, http.StatusOK, payload)
}

// handleConfirmTwoFactorEnrollment activates the pending secret against a valid
// code. Recovery codes are returned here and nowhere else.
func (s *Server) handleConfirmTwoFactorEnrollment(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if s.crossSiteBrowserPost(r) {
		writeGHError(w, http.StatusForbidden, "cross-origin request denied")
		return
	}
	// Sudo-mode guard: changing authentication is a sensitive action.
	if s.requireProofOfPresence(w, r) {
		return
	}
	var req otpRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	codes, status, result := s.store.ConfirmTwoFactorEnrollment(viewer.ID, req.Code, s.currentTime())
	if result != store.SecurityOK {
		writeAccountSecurityError(w, result)
		return
	}
	authentication, _ := s.store.AccountAuthenticationFor(viewer.ID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"authentication": authentication,
		"two_factor":     status,
		"recovery_codes": codes,
	})
}

func (s *Server) handleDisableTwoFactor(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if s.crossSiteBrowserPost(r) {
		writeGHError(w, http.StatusForbidden, "cross-origin request denied")
		return
	}
	// Sudo-mode guard: changing authentication is a sensitive action.
	if s.requireProofOfPresence(w, r) {
		return
	}
	var req otpRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	// An enterprise that requires 2FA does not let a non-owner member disable it.
	if e := s.primaryEnterprise(); e != nil && e.Policy.TwoFactorRequired == store.EnterprisePolicyEnabled &&
		!enterpriseOwnerRole(s.enterpriseRoleOfUser(e, viewer)) {
		writeGHError(w, http.StatusForbidden,
			"Two-factor authentication is required by an enterprise policy and cannot be disabled.")
		return
	}
	if result := s.store.DisableTwoFactor(viewer.ID, req.Code, s.currentTime(),
		s.enterpriseDisallowedTwoFactorMethods(viewer)); result != store.SecurityOK {
		writeAccountSecurityError(w, result)
		return
	}
	payload, _ := s.authenticationSettingsJSON(viewer.ID)
	writeJSON(w, http.StatusOK, payload)
}

// handleRegenerateRecoveryCodes replaces the whole set and shows the new codes once.
func (s *Server) handleRegenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if s.crossSiteBrowserPost(r) {
		writeGHError(w, http.StatusForbidden, "cross-origin request denied")
		return
	}
	// Sudo-mode guard: changing authentication is a sensitive action.
	if s.requireProofOfPresence(w, r) {
		return
	}
	var req otpRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	codes, status, result := s.store.RegenerateRecoveryCodes(viewer.ID, req.Code, s.currentTime(),
		s.enterpriseDisallowedTwoFactorMethods(viewer))
	if result != store.SecurityOK {
		writeAccountSecurityError(w, result)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"two_factor":     status,
		"recovery_codes": codes,
	})
}

// ─── Password ───────────────────────────────────────────────────────────────

// passwordAcceptable applies github.com's rule: at least 15 characters, or at
// least 8 including a number and a lowercase letter.
func passwordAcceptable(password string) (string, bool) {
	runes := []rune(password)
	if len(runes) >= 15 {
		return "", true
	}
	if len(runes) < 8 {
		return "Password is too short (minimum is 8 characters, or 15 without a number and a lowercase letter).", false
	}
	hasNumber, hasLower := false, false
	for _, r := range runes {
		if unicode.IsDigit(r) {
			hasNumber = true
		}
		if unicode.IsLower(r) {
			hasLower = true
		}
	}
	if hasNumber && hasLower {
		return "", true
	}
	return "Password needs at least one number and one lowercase letter, or 15 characters.", false
}

// handleChangePassword rotates the account password. It requires the current
// password even though the session already authenticates the caller, and revokes
// every other browser session so a rotation after a suspected compromise evicts
// the intruder.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if s.crossSiteBrowserPost(r) {
		writeGHError(w, http.StatusForbidden, "cross-origin request denied")
		return
	}
	// Sudo-mode guard: changing authentication is a sensitive action.
	if s.requireProofOfPresence(w, r) {
		return
	}
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	authentication, ok := s.store.AccountAuthenticationFor(viewer.ID)
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if authentication.Kind != store.AccountAuthLocal {
		writeAccountSecurityError(w, store.SecurityExternalAccount)
		return
	}
	current, _ := s.store.UserPasswordHash(viewer.ID)
	if current != "" {
		if bcrypt.CompareHashAndPassword([]byte(current), []byte(req.CurrentPassword)) != nil {
			writeGHError(w, http.StatusUnprocessableEntity, "Your current password is incorrect.")
			return
		}
	} else if req.CurrentPassword != "" {
		// The account has no password yet, so a supplied "current" one is rejected
		// rather than silently ignored.
		writeGHError(w, http.StatusUnprocessableEntity, "This account has no password yet, so there is no current password to confirm.")
		return
	}
	if message, acceptable := passwordAcceptable(req.NewPassword); !acceptable {
		writeGHError(w, http.StatusUnprocessableEntity, message)
		return
	}
	encoded, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "could not secure the new password")
		return
	}
	if result := s.store.SetUserPasswordHash(viewer.ID, string(encoded), s.currentTime()); result != store.SecurityOK {
		writeAccountSecurityError(w, result)
		return
	}
	s.revokeOtherLoginSessions(r, viewer.ID)
	payload, _ := s.authenticationSettingsJSON(viewer.ID)
	writeJSON(w, http.StatusOK, payload)
}

// revokeOtherLoginSessions ends every session except the caller's own. Failures
// are logged, not surfaced: the password change already succeeded.
func (s *Server) revokeOtherLoginSessions(r *http.Request, userID int) {
	now := s.currentTime()
	current := s.sessionFromRequest(r)
	sessions, err := s.store.ListLoginSessionsForUser(userID, now)
	if err != nil {
		s.logger.Error().Err(err).Msg("list sessions to revoke after password change")
		return
	}
	for _, session := range sessions {
		if current != nil && session.Handle == current.Handle {
			continue
		}
		if _, err := s.store.DeleteLoginSessionByHandle(userID, session.Handle, now); err != nil {
			s.logger.Error().Err(err).Msg("revoke session after password change")
		}
	}
}

// ─── Active sessions ────────────────────────────────────────────────────────

// nullableSessionTime renders a zero timestamp as JSON null.
func nullableSessionTime(at time.Time) interface{} {
	if at.IsZero() {
		return nil
	}
	return at
}

func (s *Server) handleListLoginSessions(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	sessions, err := s.store.ListLoginSessionsForUser(viewer.ID, s.currentTime())
	if err != nil {
		s.logger.Error().Err(err).Msg("list browser sessions")
		writeGHError(w, http.StatusServiceUnavailable, "browser sessions are unavailable")
		return
	}
	current := s.sessionFromRequest(r)
	out := make([]map[string]interface{}, 0, len(sessions))
	for _, session := range sessions {
		row := map[string]interface{}{
			"handle":     session.Handle,
			"created_at": nullableSessionTime(session.CreatedAt),
			"expires_at": session.ExpiresAt,
			"user_agent": session.UserAgent,
			"ip":         session.SignedInIP,
			"provider":   session.Provider,
			"current":    current != nil && current.Handle != "" && session.Handle == current.Handle,
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"sessions": out})
}

func (s *Server) handleRevokeLoginSession(w http.ResponseWriter, r *http.Request) {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if s.crossSiteBrowserPost(r) {
		writeGHError(w, http.StatusForbidden, "cross-origin request denied")
		return
	}
	// Sudo-mode guard: changing authentication is a sensitive action.
	if s.requireProofOfPresence(w, r) {
		return
	}
	handle := r.PathValue("handle")
	revoked, err := s.store.DeleteLoginSessionByHandle(viewer.ID, handle, s.currentTime())
	if err != nil {
		s.logger.Error().Err(err).Msg("revoke browser session")
		writeGHError(w, http.StatusServiceUnavailable, "browser sessions are unavailable")
		return
	}
	if !revoked {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// Revoking your own session signs you out; clear the cookie too.
	if current := s.sessionFromRequest(r); current != nil && current.Handle == handle {
		if err := s.clearSessionCookies(w, r); err != nil {
			s.logger.Error().Err(err).Msg("clear cookies after revoking the current session")
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

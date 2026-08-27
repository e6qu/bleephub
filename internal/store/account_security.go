package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Account security: TOTP enrolment, recovery codes, the account password, and
// the credential source that governs the account. Plaintext secret material
// crosses the store boundary only at enrolment and recovery-code generation,
// each once; every read path returns secret-free TwoFactorStatus.

const (
	// recoveryCodeCount matches github.com's set size.
	recoveryCodeCount = 16
	// Ten symbols from a 32-symbol alphabet is 50 bits, so a plain SHA-256 (not
	// a password KDF) suffices to digest them.
	recoveryCodeGroup  = 5
	recoveryCodeGroups = 2
	// recoveryCodeAlphabet omits characters misread off a printout: i, l, o, 0, 1.
	recoveryCodeAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	// enrollmentWindow bounds how long a provisioned-but-unconfirmed secret stays
	// enrollable.
	enrollmentWindow = 15 * time.Minute
)

// RecoveryCode is one single-use fallback credential; only its digest is
// retained. UsedAt is zero while unused.
type RecoveryCode struct {
	Hash   string    `json:"hash"`
	UsedAt time.Time `json:"used_at,omitempty"`
}

func (c RecoveryCode) Used() bool { return !c.UsedAt.IsZero() }

// TwoFactorConfig is the stored second-factor state for one account.
type TwoFactorConfig struct {
	Secret string `json:"secret,omitempty"`
	// Pending marks a provisioned-but-unproved secret: the account is NOT
	// protected until a code confirms the authenticator holds it.
	Pending      bool      `json:"pending,omitempty"`
	PendingSince time.Time `json:"pending_since,omitempty"`
	Enabled      bool      `json:"enabled,omitempty"`
	EnrolledAt   time.Time `json:"enrolled_at,omitempty"`
	// LastStep is the highest TOTP counter already spent; replaying one inside
	// its validity window is refused.
	LastStep                 int64          `json:"last_step,omitempty"`
	RecoveryCodes            []RecoveryCode `json:"recovery_codes,omitempty"`
	RecoveryCodesGeneratedAt time.Time      `json:"recovery_codes_generated_at,omitempty"`
}

// TwoFactorStatus is the secret-free second-factor view a read endpoint returns.
type TwoFactorStatus struct {
	Enabled                  bool      `json:"enabled"`
	PendingEnrollment        bool      `json:"pending_enrollment"`
	EnrolledAt               time.Time `json:"enrolled_at,omitzero"`
	RecoveryCodesTotal       int       `json:"recovery_codes_total"`
	RecoveryCodesRemaining   int       `json:"recovery_codes_remaining"`
	RecoveryCodesGeneratedAt time.Time `json:"recovery_codes_generated_at,omitzero"`
}

func twoFactorStatusOf(config *TwoFactorConfig, now time.Time) TwoFactorStatus {
	if config == nil {
		return TwoFactorStatus{}
	}
	status := TwoFactorStatus{
		Enabled:                  config.Enabled,
		PendingEnrollment:        config.Pending && !enrollmentExpired(config, now),
		EnrolledAt:               config.EnrolledAt,
		RecoveryCodesTotal:       len(config.RecoveryCodes),
		RecoveryCodesGeneratedAt: config.RecoveryCodesGeneratedAt,
	}
	for _, code := range config.RecoveryCodes {
		if !code.Used() {
			status.RecoveryCodesRemaining++
		}
	}
	return status
}

func enrollmentExpired(config *TwoFactorConfig, now time.Time) bool {
	return config.PendingSince.IsZero() || now.After(config.PendingSince.Add(enrollmentWindow))
}

// AccountSecurityResult is the outcome of an enrolment or verification attempt;
// callers map it to a status code.
type AccountSecurityResult int

const (
	SecurityOK AccountSecurityResult = iota
	SecurityUnknownUser
	SecurityInvalidCode
	SecurityTwoFactorNotEnabled
	SecurityTwoFactorAlreadyEnabled
	// SecurityNoPendingEnrollment: no live provisioned secret to confirm (never
	// started, or the enrolment window elapsed).
	SecurityNoPendingEnrollment
	// SecurityExternalAccount: credentials are governed by an external identity
	// provider, so there is no second factor to enrol here.
	SecurityExternalAccount
	// SecurityInternalError: the entropy source failed.
	SecurityInternalError
	// SecurityMethodDisallowed: a genuine code arrived through a method an
	// enterprise policy bans; distinct from SecurityInvalidCode because the
	// remedy is to use the other factor.
	SecurityMethodDisallowed
)

// TwoFactorStatusFor returns a detached snapshot of the account's second-factor
// state; the second result is false when the user does not exist.
func (st *Store) TwoFactorStatusFor(userID int, now time.Time) (TwoFactorStatus, bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	user := st.Users[userID]
	if user == nil {
		return TwoFactorStatus{}, false
	}
	return twoFactorStatusOf(user.TwoFactor, now), true
}

// TwoFactorEnabled reports whether the account has a confirmed second factor;
// a pending (unconfirmed) enrolment is deliberately not "enabled".
func (st *Store) TwoFactorEnabled(userID int) bool {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	user := st.Users[userID]
	return user != nil && user.TwoFactor != nil && user.TwoFactor.Enabled
}

// BeginTwoFactorEnrollment provisions a fresh TOTP secret and returns it — the
// one moment it is legible outside the store. The secret stays pending until
// ConfirmTwoFactorEnrollment sees a code computed from it. Restarting replaces
// any previous pending secret.
func (st *Store) BeginTwoFactorEnrollment(userID int, now time.Time) (string, AccountSecurityResult) {
	secret, err := NewTOTPSecret()
	if err != nil {
		return "", SecurityInternalError
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()
	user := st.Users[userID]
	if user == nil {
		return "", SecurityUnknownUser
	}
	if !localCredentialAccountLocked(user) {
		return "", SecurityExternalAccount
	}
	if user.TwoFactor != nil && user.TwoFactor.Enabled {
		return "", SecurityTwoFactorAlreadyEnabled
	}
	user.TwoFactor = &TwoFactorConfig{Secret: secret, Pending: true, PendingSince: now}
	user.UpdatedAt = now
	st.persistUserLocked(user)
	return secret, SecurityOK
}

// ConfirmTwoFactorEnrollment completes enrolment when code is a valid TOTP for
// the pending secret. It returns the generated recovery codes in the clear —
// the single time they are legible.
func (st *Store) ConfirmTwoFactorEnrollment(userID int, code string, now time.Time) ([]string, TwoFactorStatus, AccountSecurityResult) {
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, TwoFactorStatus{}, SecurityInternalError
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()
	user := st.Users[userID]
	if user == nil {
		return nil, TwoFactorStatus{}, SecurityUnknownUser
	}
	config := user.TwoFactor
	if config == nil || !config.Pending || config.Enabled {
		return nil, twoFactorStatusOf(config, now), SecurityNoPendingEnrollment
	}
	if enrollmentExpired(config, now) {
		user.TwoFactor = nil
		user.UpdatedAt = now
		st.persistUserLocked(user)
		return nil, TwoFactorStatus{}, SecurityNoPendingEnrollment
	}
	step, ok := verifyTOTP(config.Secret, code, now, config.LastStep)
	if !ok {
		return nil, twoFactorStatusOf(config, now), SecurityInvalidCode
	}
	config.Pending = false
	config.PendingSince = time.Time{}
	config.Enabled = true
	config.EnrolledAt = now
	config.LastStep = step
	config.RecoveryCodes = hashes
	config.RecoveryCodesGeneratedAt = now
	user.UpdatedAt = now
	st.persistUserLocked(user)
	return codes, twoFactorStatusOf(config, now), SecurityOK
}

// TwoFactorMethod names one second factor bleephub implements. The set is
// closed and reported verbatim by the authentication view.
type TwoFactorMethod string

const (
	TwoFactorMethodTOTP TwoFactorMethod = "totp"
	// TwoFactorMethodRecoveryCode is a static single-use secret the user retypes,
	// so — unlike a TOTP — a phished code stays valid until spent. This is the
	// insecure method GitHub's "insecure 2FA methods" policy targets.
	TwoFactorMethodRecoveryCode TwoFactorMethod = "recovery_code"
)

// TwoFactorMethodDescription is one catalogue entry: the method, whether it is
// classed insecure, and why.
type TwoFactorMethodDescription struct {
	Method   TwoFactorMethod `json:"method"`
	Insecure bool            `json:"insecure"`
	Summary  string          `json:"summary"`
}

// SupportedTwoFactorMethods is the truthful catalogue of second factors bleephub
// implements. GitHub's insecure method is SMS; this instance has no telephone
// factor, so it is absent rather than listed and unenforced.
func SupportedTwoFactorMethods() []TwoFactorMethodDescription {
	return []TwoFactorMethodDescription{
		{
			Method:  TwoFactorMethodTOTP,
			Summary: "Time-based one-time password from an authenticator app.",
		},
		{
			Method:   TwoFactorMethodRecoveryCode,
			Insecure: true,
			Summary:  "Printed single-use recovery code: a static secret the user retypes, valid until spent.",
		},
	}
}

// InsecureTwoFactorMethods is the subset of the catalogue an enterprise
// disallows when its two-factor-disallowed-methods policy is INSECURE.
func InsecureTwoFactorMethods() []TwoFactorMethod {
	var out []TwoFactorMethod
	for _, described := range SupportedTwoFactorMethods() {
		if described.Insecure {
			out = append(out, described.Method)
		}
	}
	return out
}

func methodDisallowed(method TwoFactorMethod, disallowed []TwoFactorMethod) bool {
	for _, banned := range disallowed {
		if banned == method {
			return true
		}
	}
	return false
}

// VerifySecondFactor spends one TOTP code or one unused recovery code.
// Verification and consumption happen under the same lock.
func (st *Store) VerifySecondFactor(userID int, code string, now time.Time) AccountSecurityResult {
	result, _ := st.VerifySecondFactorExcluding(userID, code, now, nil)
	return result
}

// VerifySecondFactorExcluding is VerifySecondFactor with a set of policy-banned
// methods. A code verifying only through a banned method is refused as
// SecurityMethodDisallowed and NOT spent, so a policy change cannot burn a
// user's recovery codes. Returns the method that answered.
func (st *Store) VerifySecondFactorExcluding(userID int, code string, now time.Time, disallowed []TwoFactorMethod) (AccountSecurityResult, TwoFactorMethod) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	return st.verifySecondFactorLocked(userID, code, now, disallowed)
}

func (st *Store) verifySecondFactorLocked(userID int, code string, now time.Time, disallowed []TwoFactorMethod) (AccountSecurityResult, TwoFactorMethod) {
	user := st.Users[userID]
	if user == nil {
		return SecurityUnknownUser, ""
	}
	config := user.TwoFactor
	if config == nil || !config.Enabled {
		return SecurityTwoFactorNotEnabled, ""
	}
	if step, ok := verifyTOTP(config.Secret, code, now, config.LastStep); ok {
		if methodDisallowed(TwoFactorMethodTOTP, disallowed) {
			return SecurityMethodDisallowed, TwoFactorMethodTOTP
		}
		config.LastStep = step
		user.UpdatedAt = now
		st.persistUserLocked(user)
		return SecurityOK, TwoFactorMethodTOTP
	}
	if index, ok := matchRecoveryCode(config.RecoveryCodes, code); ok {
		if methodDisallowed(TwoFactorMethodRecoveryCode, disallowed) {
			return SecurityMethodDisallowed, TwoFactorMethodRecoveryCode
		}
		config.RecoveryCodes[index].UsedAt = now
		user.UpdatedAt = now
		st.persistUserLocked(user)
		return SecurityOK, TwoFactorMethodRecoveryCode
	}
	return SecurityInvalidCode, ""
}

// DisableTwoFactor turns the second factor off, only on proof of possession.
func (st *Store) DisableTwoFactor(userID int, code string, now time.Time, disallowed []TwoFactorMethod) AccountSecurityResult {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if result, _ := st.verifySecondFactorLocked(userID, code, now, disallowed); result != SecurityOK {
		return result
	}
	user := st.Users[userID]
	user.TwoFactor = nil
	user.UpdatedAt = now
	st.persistUserLocked(user)
	return SecurityOK
}

// CancelTwoFactorEnrollment discards a pending (unconfirmed) secret; no proof
// is required since the account was never protected by it.
func (st *Store) CancelTwoFactorEnrollment(userID int, now time.Time) AccountSecurityResult {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	user := st.Users[userID]
	if user == nil {
		return SecurityUnknownUser
	}
	if user.TwoFactor == nil || !user.TwoFactor.Pending {
		return SecurityNoPendingEnrollment
	}
	if user.TwoFactor.Enabled {
		return SecurityTwoFactorAlreadyEnabled
	}
	user.TwoFactor = nil
	user.UpdatedAt = now
	st.persistUserLocked(user)
	return SecurityOK
}

// RegenerateRecoveryCodes replaces the whole set, invalidating every previous
// code, and returns the new codes once. Costs a valid second factor.
func (st *Store) RegenerateRecoveryCodes(userID int, code string, now time.Time, disallowed []TwoFactorMethod) ([]string, TwoFactorStatus, AccountSecurityResult) {
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, TwoFactorStatus{}, SecurityInternalError
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if result, _ := st.verifySecondFactorLocked(userID, code, now, disallowed); result != SecurityOK {
		status := TwoFactorStatus{}
		if user := st.Users[userID]; user != nil {
			status = twoFactorStatusOf(user.TwoFactor, now)
		}
		return nil, status, result
	}
	user := st.Users[userID]
	user.TwoFactor.RecoveryCodes = hashes
	user.TwoFactor.RecoveryCodesGeneratedAt = now
	user.UpdatedAt = now
	st.persistUserLocked(user)
	return codes, twoFactorStatusOf(user.TwoFactor, now), SecurityOK
}

// newRecoveryCodes draws a fresh set, returning the printable codes and the
// digests to retain.
func newRecoveryCodes() ([]string, []RecoveryCode, error) {
	codes := make([]string, 0, recoveryCodeCount)
	stored := make([]RecoveryCode, 0, recoveryCodeCount)
	for i := 0; i < recoveryCodeCount; i++ {
		code, err := newRecoveryCode()
		if err != nil {
			return nil, nil, err
		}
		codes = append(codes, code)
		stored = append(stored, RecoveryCode{Hash: hashRecoveryCode(code)})
	}
	return codes, stored, nil
}

func newRecoveryCode() (string, error) {
	limit := big.NewInt(int64(len(recoveryCodeAlphabet)))
	var builder strings.Builder
	for group := 0; group < recoveryCodeGroups; group++ {
		if group > 0 {
			builder.WriteByte('-')
		}
		for i := 0; i < recoveryCodeGroup; i++ {
			index, err := rand.Int(rand.Reader, limit)
			if err != nil {
				return "", fmt.Errorf("draw recovery code: %w", err)
			}
			builder.WriteByte(recoveryCodeAlphabet[index.Int64()])
		}
	}
	return builder.String(), nil
}

// normalizeRecoveryCode makes matching insensitive to separators and case,
// without widening the alphabet.
func normalizeRecoveryCode(code string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return -1
		}
	}, code)
}

func hashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(normalizeRecoveryCode(code)))
	return hex.EncodeToString(sum[:])
}

// matchRecoveryCode finds an unused code matching the candidate. Every digest is
// compared in constant time and the loop is not cut short, so timing does not
// reveal which code matched.
func matchRecoveryCode(codes []RecoveryCode, candidate string) (int, bool) {
	normalized := normalizeRecoveryCode(candidate)
	if len(normalized) != recoveryCodeGroup*recoveryCodeGroups {
		return 0, false
	}
	digest := hashRecoveryCode(normalized)
	found, index := false, 0
	for i, code := range codes {
		if code.Used() {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(code.Hash), []byte(digest)) == 1 && !found {
			found, index = true, i
		}
	}
	return index, found
}

// AccountAuthKind names where an account's credentials live.
type AccountAuthKind string

const (
	AccountAuthLocal AccountAuthKind = "local"
	// AccountAuthExternal is bound to a federated identity; its password and
	// second factor belong to the identity provider.
	AccountAuthExternal AccountAuthKind = "external"
)

// AccountAuthentication describes the credential source of one account.
type AccountAuthentication struct {
	Kind AccountAuthKind `json:"kind"`
	// Providers lists the issuers bound to the account (external accounts only).
	Providers   []string `json:"providers,omitempty"`
	PasswordSet bool     `json:"password_set"`
}

// localCredentialAccountLocked reports whether this instance, rather than an
// identity provider, is the authority for the account's credentials. Callers
// hold st.Mu.
func localCredentialAccountLocked(user *User) bool {
	return len(user.ExternalIdentities) == 0
}

// AccountAuthenticationFor returns a detached description of the account's
// credential source; the second result is false when the user does not exist.
func (st *Store) AccountAuthenticationFor(userID int) (AccountAuthentication, bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	user := st.Users[userID]
	if user == nil {
		return AccountAuthentication{}, false
	}
	if localCredentialAccountLocked(user) {
		return AccountAuthentication{Kind: AccountAuthLocal, PasswordSet: user.PasswordHash != ""}, true
	}
	providers := make([]string, 0, len(user.ExternalIdentities))
	for _, identity := range user.ExternalIdentities {
		providers = append(providers, identity.Issuer)
	}
	return AccountAuthentication{Kind: AccountAuthExternal, Providers: providers, PasswordSet: user.PasswordHash != ""}, true
}

// UserPasswordHash returns the account's stored bcrypt hash, empty when the
// account has no password.
func (st *Store) UserPasswordHash(userID int) (string, bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	user := st.Users[userID]
	if user == nil {
		return "", false
	}
	return user.PasswordHash, true
}

// SetUserPasswordHash replaces the account password. Refused on a federated
// account, which would otherwise gain a second credential path the identity
// provider knows nothing about.
func (st *Store) SetUserPasswordHash(userID int, hash string, now time.Time) AccountSecurityResult {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	user := st.Users[userID]
	if user == nil {
		return SecurityUnknownUser
	}
	if !localCredentialAccountLocked(user) {
		return SecurityExternalAccount
	}
	user.PasswordHash = hash
	user.UpdatedAt = now
	st.persistUserLocked(user)
	return SecurityOK
}

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

// Account security state behind github.com's Settings → "Password and
// authentication" page: TOTP enrolment, recovery codes, the account password,
// and which credential source actually governs the account.
//
// The secret and the recovery codes live here and never leave: the only two
// moments a plaintext value crosses the store boundary are enrolment (the
// provisioning secret) and recovery-code generation, each exactly once. Every
// read path returns TwoFactorStatus, which carries counts and timestamps but
// no secret material.

const (
	// recoveryCodeCount matches github.com's set size.
	recoveryCodeCount = 16
	// recoveryCodeGroup/recoveryCodeGroups give the xxxxx-xxxxx shape GitHub
	// prints. Ten symbols from a 32-symbol alphabet is 50 bits — far beyond
	// guessing, which is why a plain SHA-256 (not a password KDF) is the right
	// digest for them.
	recoveryCodeGroup  = 5
	recoveryCodeGroups = 2
	// recoveryCodeAlphabet omits the characters people misread when copying a
	// code off a printout: i, l, o, 0, 1.
	recoveryCodeAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	// enrollmentWindow bounds how long a provisioned-but-unconfirmed secret
	// stays valid. An abandoned enrolment must not leave a live secret sitting
	// on the account indefinitely.
	enrollmentWindow = 15 * time.Minute
)

// RecoveryCode is one single-use fallback credential. Only its digest is
// retained; UsedAt is the moment it was spent (zero while unused).
type RecoveryCode struct {
	Hash   string    `json:"hash"`
	UsedAt time.Time `json:"used_at,omitempty"`
}

// Used reports whether the code has already been spent.
func (c RecoveryCode) Used() bool { return !c.UsedAt.IsZero() }

// TwoFactorConfig is the stored second-factor state for one account.
type TwoFactorConfig struct {
	// Secret is the base32 TOTP shared secret. It is set while an enrolment is
	// pending and for as long as two-factor is enabled.
	Secret string `json:"secret,omitempty"`
	// Pending marks a secret that has been provisioned but not yet proved: the
	// account is NOT protected until a real code confirms the authenticator
	// actually holds it.
	Pending      bool      `json:"pending,omitempty"`
	PendingSince time.Time `json:"pending_since,omitempty"`
	Enabled      bool      `json:"enabled,omitempty"`
	EnrolledAt   time.Time `json:"enrolled_at,omitempty"`
	// LastStep is the highest TOTP counter already spent. A code is single-use:
	// replaying one inside its own validity window is refused.
	LastStep                 int64          `json:"last_step,omitempty"`
	RecoveryCodes            []RecoveryCode `json:"recovery_codes,omitempty"`
	RecoveryCodesGeneratedAt time.Time      `json:"recovery_codes_generated_at,omitempty"`
}

// TwoFactorStatus is the complete second-factor view a read endpoint may
// return: what is on, since when, and how much recovery capacity is left.
// Nothing here can be replayed as a credential.
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

// AccountSecurityResult is the outcome of an enrolment or verification attempt.
// Callers map it to a status code; the store never decides HTTP semantics.
type AccountSecurityResult int

const (
	// SecurityOK means the operation succeeded.
	SecurityOK AccountSecurityResult = iota
	// SecurityUnknownUser means no such account.
	SecurityUnknownUser
	// SecurityInvalidCode means the code was wrong, expired, already spent,
	// or not a code at all.
	SecurityInvalidCode
	// SecurityTwoFactorNotEnabled means the account has no confirmed second factor.
	SecurityTwoFactorNotEnabled
	// SecurityTwoFactorAlreadyEnabled means enrolment was attempted while a confirmed
	// second factor is already in place.
	SecurityTwoFactorAlreadyEnabled
	// SecurityNoPendingEnrollment means there is no live provisioned secret to
	// confirm (never started, or the enrolment window elapsed).
	SecurityNoPendingEnrollment
	// SecurityExternalAccount means the account's credentials are governed
	// elsewhere (an external identity provider), so this instance has no second
	// factor to enrol.
	SecurityExternalAccount
	// SecurityInternalError means the operation could not be completed for a
	// reason unrelated to the caller — the entropy source failed.
	SecurityInternalError
)

// TwoFactorStatusFor returns a detached snapshot of the account's second-factor
// state. The second result is false when the user does not exist.
func (st *Store) TwoFactorStatusFor(userID int, now time.Time) (TwoFactorStatus, bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	user := st.Users[userID]
	if user == nil {
		return TwoFactorStatus{}, false
	}
	return twoFactorStatusOf(user.TwoFactor, now), true
}

// TwoFactorEnabled reports whether the account has a confirmed second factor.
// It is the predicate the sign-in path and the REST private-user view read;
// a pending (unconfirmed) enrolment is deliberately not "enabled".
func (st *Store) TwoFactorEnabled(userID int) bool {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	user := st.Users[userID]
	return user != nil && user.TwoFactor != nil && user.TwoFactor.Enabled
}

// BeginTwoFactorEnrollment provisions a fresh TOTP secret for the account and
// returns it — the one moment it is legible outside the store. The account is
// NOT protected yet: the secret stays pending until ConfirmTwoFactorEnrollment
// sees a code computed from it, so a user who never completes the pairing is
// never told they have a second factor they cannot produce.
//
// Restarting enrolment replaces any previous pending secret, so a user who
// closed the page mid-pairing simply scans a new code.
func (st *Store) BeginTwoFactorEnrollment(userID int, now time.Time) (string, AccountSecurityResult) {
	secret, err := NewTOTPSecret()
	if err != nil {
		// Failing closed is the only safe option: a predictable secret is worse
		// than no second factor.
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

// ConfirmTwoFactorEnrollment completes enrolment when `code` is a valid TOTP
// for the pending secret, and only then. It returns the generated recovery
// codes in the clear — the single time they are legible — alongside the new
// status.
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
		// Drop the stale secret rather than leaving it enrollable forever.
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

// VerifySecondFactor spends one second factor: a TOTP code, or one unused
// recovery code. Verification and consumption happen under the same lock, so
// two concurrent requests cannot both spend the same code.
func (st *Store) VerifySecondFactor(userID int, code string, now time.Time) AccountSecurityResult {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	return st.verifySecondFactorLocked(userID, code, now)
}

func (st *Store) verifySecondFactorLocked(userID int, code string, now time.Time) AccountSecurityResult {
	user := st.Users[userID]
	if user == nil {
		return SecurityUnknownUser
	}
	config := user.TwoFactor
	if config == nil || !config.Enabled {
		return SecurityTwoFactorNotEnabled
	}
	if step, ok := verifyTOTP(config.Secret, code, now, config.LastStep); ok {
		config.LastStep = step
		user.UpdatedAt = now
		st.persistUserLocked(user)
		return SecurityOK
	}
	if index, ok := matchRecoveryCode(config.RecoveryCodes, code); ok {
		config.RecoveryCodes[index].UsedAt = now
		user.UpdatedAt = now
		st.persistUserLocked(user)
		return SecurityOK
	}
	return SecurityInvalidCode
}

// DisableTwoFactor turns the second factor off, and only on proof of
// possession: switching protection off is exactly the operation an attacker
// holding a stolen session wants, so it costs a code like every other
// second-factor operation.
func (st *Store) DisableTwoFactor(userID int, code string, now time.Time) AccountSecurityResult {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if result := st.verifySecondFactorLocked(userID, code, now); result != SecurityOK {
		return result
	}
	user := st.Users[userID]
	user.TwoFactor = nil
	user.UpdatedAt = now
	st.persistUserLocked(user)
	return SecurityOK
}

// CancelTwoFactorEnrollment discards a pending (unconfirmed) secret. There is
// nothing to prove — the account was never protected by it.
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
// code including unused ones, and returns the new codes once. It costs a valid
// second factor for the same reason disabling does.
func (st *Store) RegenerateRecoveryCodes(userID int, code string, now time.Time) ([]string, TwoFactorStatus, AccountSecurityResult) {
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, TwoFactorStatus{}, SecurityInternalError
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if result := st.verifySecondFactorLocked(userID, code, now); result != SecurityOK {
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

// normalizeRecoveryCode makes matching insensitive to the separators and case
// a user may retype, without widening the alphabet.
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

// matchRecoveryCode finds an unused code matching the candidate. Every stored
// digest is compared in constant time and the loop is not cut short, so the
// time taken does not reveal which code matched.
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

// ─── Which credential source governs the account ───────────────────────────

// AccountAuthKind names where an account's credentials actually live.
type AccountAuthKind string

const (
	// AccountAuthLocal is an account this instance authenticates itself, so a
	// password and a second factor are ours to manage.
	AccountAuthLocal AccountAuthKind = "local"
	// AccountAuthExternal is an account bound to a federated identity. Its
	// password and second factor belong to the identity provider; offering
	// controls for them here would be a lie.
	AccountAuthExternal AccountAuthKind = "external"
)

// AccountAuthentication describes the credential source of one account.
type AccountAuthentication struct {
	Kind AccountAuthKind `json:"kind"`
	// Providers lists the issuers bound to the account (external accounts only).
	Providers []string `json:"providers,omitempty"`
	// PasswordSet reports whether a local password exists to change.
	PasswordSet bool `json:"password_set"`
}

// localCredentialAccountLocked reports whether this instance — rather than an
// identity provider — is the authority for the account's credentials. Callers
// hold st.Mu.
func localCredentialAccountLocked(user *User) bool {
	return len(user.ExternalIdentities) == 0
}

// AccountAuthenticationFor returns a detached description of the account's
// credential source. The second result is false when the user does not exist.
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

// UserPasswordHash returns the account's stored bcrypt hash (empty when the
// account has no password). Strings are immutable, so the result is detached.
func (st *Store) UserPasswordHash(userID int) (string, bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	user := st.Users[userID]
	if user == nil {
		return "", false
	}
	return user.PasswordHash, true
}

// SetUserPasswordHash replaces the account password. Only a local account has
// one to replace: rewriting the hash on a federated account would create a
// second, weaker way into it that the identity provider knows nothing about.
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

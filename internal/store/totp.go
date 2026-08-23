package store

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- RFC 6238 defines TOTP over HMAC-SHA1; every authenticator app implements that construction
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RFC 6238 time-based one-time passwords, with the parameter set every
// mainstream authenticator app (Google Authenticator, 1Password, Aegis, Duo)
// assumes when a provisioning URI omits them: HMAC-SHA1, 6 digits, a 30-second
// step. They are spelled out in the otpauth:// URI anyway so a scanner never
// has to guess.
const (
	// TOTPDigits is the code length (RFC 6238 §5.3 truncation).
	TOTPDigits = 6
	// TOTPPeriod is the time step.
	TOTPPeriod = 30 * time.Second
	// TOTPDriftSteps is how many steps either side of the current one are
	// accepted, absorbing clock skew between the server and the phone. One step
	// is the RFC 6238 §5.2 recommendation: it bounds the validity window at
	// three steps (90s) instead of letting a code live indefinitely.
	TOTPDriftSteps = 1
	// totpSecretBytes is the shared-secret length. RFC 4226 §4 requires at
	// least 128 bits and recommends 160 — the HMAC-SHA1 output width.
	totpSecretBytes = 20
)

// base32NoPad is the alphabet authenticator apps expect in the `secret`
// parameter of an otpauth:// URI: RFC 4648 base32, unpadded.
var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret draws a fresh shared secret and returns its unpadded base32
// encoding. It is the only value that must ever leave the store in the clear,
// and only once, at enrolment.
func NewTOTPSecret() (string, error) {
	raw := make([]byte, totpSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("draw two-factor secret: %w", err)
	}
	return base32NoPad.EncodeToString(raw), nil
}

// totpStep is the RFC 6238 counter T for an instant: the number of whole
// periods since the Unix epoch (T0 = 0).
func totpStep(at time.Time) int64 {
	return at.UTC().Unix() / int64(TOTPPeriod/time.Second)
}

// totpCodeAt computes the code for one counter value. An unparseable secret is
// an error rather than a silently wrong code.
func totpCodeAt(secret string, counter int64) (string, error) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return "", err
	}
	if counter < 0 {
		// RFC 4226 counts an unsigned 8-byte counter from the epoch. A negative
		// value means the clock is set before 1970; refusing is better than
		// wrapping it into a huge counter and returning a confidently wrong code.
		return "", fmt.Errorf("two-factor counter %d precedes the epoch", counter)
	}
	var message [8]byte
	binary.BigEndian.PutUint64(message[:], uint64(counter))
	mac := hmac.New(sha1.New, key)
	mac.Write(message[:])
	sum := mac.Sum(nil)
	// RFC 4226 §5.4 dynamic truncation.
	offset := sum[len(sum)-1] & 0x0f
	truncated := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])
	modulus := uint32(1)
	for i := 0; i < TOTPDigits; i++ {
		modulus *= 10
	}
	return fmt.Sprintf("%0*d", TOTPDigits, truncated%modulus), nil
}

func decodeTOTPSecret(secret string) ([]byte, error) {
	normalized := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "=", "").Replace(secret))
	key, err := base32NoPad.DecodeString(normalized)
	if err != nil {
		return nil, fmt.Errorf("decode two-factor secret: %w", err)
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("two-factor secret is empty")
	}
	return key, nil
}

// normalizeOTPInput strips the separators authenticator apps and password
// managers paste in ("123 456") so a correct code is not rejected on
// formatting.
func normalizeOTPInput(code string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '\t' {
			return -1
		}
		return r
	}, strings.TrimSpace(code))
}

// verifyTOTP reports whether code is valid for the secret at `at`, and returns
// the counter it matched so the caller can burn that step. Steps at or below
// minStep are refused: a code is single-use, so replaying an intercepted one
// inside its own 90-second window does not authenticate a second time.
//
// The digit comparison is constant-time. The loop over the drift window is not
// secret-dependent (the window is a public constant), so it leaks nothing.
func verifyTOTP(secret, code string, at time.Time, minStep int64) (int64, bool) {
	candidate := normalizeOTPInput(code)
	if len(candidate) != TOTPDigits {
		return 0, false
	}
	for _, digit := range candidate {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	current := totpStep(at)
	for delta := int64(-TOTPDriftSteps); delta <= TOTPDriftSteps; delta++ {
		step := current + delta
		if step <= minStep {
			continue
		}
		expected, err := totpCodeAt(secret, step)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(expected), []byte(candidate)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// OTPAuthURI renders the standard provisioning URI an authenticator app reads
// from a QR code. Every parameter is stated explicitly rather than left to the
// scanner's defaults.
//
//	otpauth://totp/Issuer:account?secret=…&issuer=Issuer&algorithm=SHA1&digits=6&period=30
func OTPAuthURI(issuer, account, secret string) string {
	issuer = strings.TrimSpace(issuer)
	if issuer == "" {
		issuer = "bleephub"
	}
	label := account
	if issuer != "" {
		label = issuer + ":" + account
	}
	query := url.Values{}
	query.Set("secret", secret)
	query.Set("issuer", issuer)
	query.Set("algorithm", "SHA1")
	query.Set("digits", strconv.Itoa(TOTPDigits))
	query.Set("period", strconv.Itoa(int(TOTPPeriod/time.Second)))
	// The label is a path segment: url.URL.String escapes it correctly, which
	// url.Values.Encode would not (it would turn ':' into %3A in a query).
	uri := url.URL{Scheme: "otpauth", Host: "totp", Path: "/" + label, RawQuery: query.Encode()}
	return uri.String()
}

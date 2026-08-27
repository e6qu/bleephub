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

// RFC 6238 TOTP with the defaults every mainstream authenticator app assumes:
// HMAC-SHA1, 6 digits, 30-second step. Stated explicitly in the otpauth:// URI
// so a scanner never has to guess.
const (
	// TOTPDigits is the code length (RFC 6238 §5.3).
	TOTPDigits = 6
	// TOTPPeriod is the time step.
	TOTPPeriod = 30 * time.Second
	// TOTPDriftSteps accepts codes this many steps either side of now, absorbing
	// clock skew (RFC 6238 §5.2). One step bounds validity to 90s.
	TOTPDriftSteps = 1
	// totpSecretBytes is the shared-secret length: 160 bits, the HMAC-SHA1 width
	// (RFC 4226 §4 requires ≥128, recommends 160).
	totpSecretBytes = 20
)

// base32NoPad is the encoding of the otpauth:// `secret` parameter: RFC 4648 base32, unpadded.
var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret draws a fresh shared secret as unpadded base32. It is the only
// value that leaves the store in the clear, and only once, at enrolment.
func NewTOTPSecret() (string, error) {
	raw := make([]byte, totpSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("draw two-factor secret: %w", err)
	}
	return base32NoPad.EncodeToString(raw), nil
}

// totpStep is the RFC 6238 counter T: whole periods since the Unix epoch.
func totpStep(at time.Time) int64 {
	return at.UTC().Unix() / int64(TOTPPeriod/time.Second)
}

// totpCodeAt computes the code for one counter value; an unparseable secret errors.
func totpCodeAt(secret string, counter int64) (string, error) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return "", err
	}
	if counter < 0 {
		// RFC 4226's counter is unsigned; a negative value (clock before 1970)
		// would wrap into a huge counter and a confidently wrong code.
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

// normalizeOTPInput strips separators apps paste in ("123 456") so formatting
// doesn't reject a correct code.
func normalizeOTPInput(code string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '\t' {
			return -1
		}
		return r
	}, strings.TrimSpace(code))
}

// verifyTOTP reports whether code is valid at `at` and returns the matched
// counter so the caller can burn that step. Steps ≤ minStep are refused to keep
// a code single-use (no replay within its 90s window). The digit compare is
// constant-time; the drift loop is over a public constant, so it leaks nothing.
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

// OTPAuthURI renders the provisioning URI an authenticator reads from a QR
// code, stating every parameter explicitly:
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
	// The label is a path segment; url.URL escapes it correctly where a query
	// encoder would turn ':' into %3A.
	uri := url.URL{Scheme: "otpauth", Host: "totp", Path: "/" + label, RawQuery: query.Encode()}
	return uri.String()
}

package gcsfake

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// jwtBearerGrant is the grant a service account asks for a token by.
	// https://developers.google.com/identity/protocols/oauth2/service-account#httprest
	jwtBearerGrant = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	// tokenLifetime is how long the service's access tokens last.
	tokenLifetime = time.Hour
	// maximumSignedSeconds is the longest a V4 signed URL may last.
	// https://docs.cloud.google.com/storage/docs/authentication/canonical-requests#required-query-parameters
	maximumSignedSeconds = 604800
)

// writeScopes are the OAuth2 scopes that let a token read and write objects.
// https://docs.cloud.google.com/storage/docs/oauth-scopes
var writeScopes = map[string]bool{
	"https://www.googleapis.com/auth/devstorage.read_write":   true,
	"https://www.googleapis.com/auth/devstorage.full_control": true,
	"https://www.googleapis.com/auth/cloud-platform":          true,
}

// issueToken is the OAuth2 token endpoint for a service account: it takes a JWT
// the account signed and answers with an access token. The assertion is
// verified in full — signature, signer, audience, expiry — because composing it
// is the client's work, or its OAuth2 library's on its behalf, and a fake that
// took any assertion would pass a client that signed with the wrong key or
// asked the wrong endpoint.
func (f *Server) issueToken(w http.ResponseWriter, r *http.Request, body []byte) {
	form, err := url.ParseQuery(string(body))
	if r.Method != http.MethodPost || err != nil || form.Get("grant_type") != jwtBearerGrant {
		writeTokenError(w, "unsupported_grant_type", "want a POST of grant_type="+jwtBearerGrant)
		return
	}
	scope, problem := f.verifyAssertion(form.Get("assertion"))
	if problem != "" {
		writeTokenError(w, "invalid_grant", problem)
		return
	}
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		panic(err)
	}
	token := "gcsfake." + hex.EncodeToString(random)
	f.mu.Lock()
	f.tokens[token] = scope
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   int(tokenLifetime / time.Second),
	})
}

func writeTokenError(w http.ResponseWriter, code, description string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": code, "error_description": description})
}

// verifyAssertion checks a service account's JWT and returns the scope it asks
// for, or what is wrong with it.
func (f *Server) verifyAssertion(assertion string) (scope, problem string) {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return "", "the assertion is not a JWT"
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	var claims struct {
		Issuer   string `json:"iss"`
		Scope    string `json:"scope"`
		Audience string `json:"aud"`
		Expiry   int64  `json:"exp"`
	}
	if !decodeSegment(parts[0], &header) || !decodeSegment(parts[1], &claims) {
		return "", "the assertion's header or claims are not base64url JSON"
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", "the assertion's signature is not base64url"
	}
	if header.Algorithm != "RS256" || header.KeyID != f.keyID {
		return "", "the assertion is not signed RS256 with the key " + f.keyID
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, digest[:], signature) != nil {
		return "", "Invalid JWT Signature."
	}
	if claims.Issuer != serviceAccount {
		return "", "the assertion's issuer is not " + serviceAccount
	}
	if claims.Audience != f.tokenURL() {
		return "", "the assertion's audience is not this token endpoint"
	}
	if !time.Unix(claims.Expiry, 0).After(time.Now()) {
		return "", "the assertion has expired"
	}
	if claims.Scope == "" {
		return "", "the assertion asks for no scope"
	}
	return claims.Scope, ""
}

func decodeSegment(segment string, into any) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(segment)
	return err == nil && json.Unmarshal(decoded, into) == nil
}

// authorize lets through a request that carries an access token this server
// issued, for a scope that reads and writes objects. The caller holds no lock.
func (f *Server) authorize(r *http.Request) *apiError {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return &apiError{http.StatusUnauthorized, "required", "Anonymous caller does not have storage.objects access to the Google Cloud Storage bucket."}
	}
	f.mu.Lock()
	scope, issued := f.tokens[token]
	f.mu.Unlock()
	if !issued {
		return &apiError{http.StatusUnauthorized, "authError", "Invalid Credentials"}
	}
	for _, granted := range strings.Fields(scope) {
		if writeScopes[granted] {
			return nil
		}
	}
	return &apiError{http.StatusForbidden, "insufficientPermissions", "Request had insufficient authentication scopes."}
}

// xmlRefusal is a refusal in the XML API's terms, which are what a download and
// a signed URL are answered in.
// https://docs.cloud.google.com/storage/docs/xml-api/reference-status
type xmlRefusal struct {
	status  int
	code    string
	message string
}

// verifySignedURL checks a V4 signed URL as the documentation says one is made:
// the canonical request is rebuilt from the request as it arrived, so a URL
// signed for another object, another verb, another host or another expiry does
// not verify, and the signature is checked against the service account's public
// key.
// https://docs.cloud.google.com/storage/docs/authentication/canonical-requests
// https://docs.cloud.google.com/storage/docs/authentication/signatures
func (f *Server) verifySignedURL(r *http.Request) *xmlRefusal {
	malformed := func(what string) *xmlRefusal {
		return &xmlRefusal{http.StatusBadRequest, "AuthenticationRequired", what}
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return malformed("the query string is malformed")
	}
	if query.Get("X-Goog-Algorithm") != "GOOG4-RSA-SHA256" {
		return malformed("X-Goog-Algorithm is not GOOG4-RSA-SHA256")
	}
	timestamp := query.Get("X-Goog-Date")
	signedAt, err := time.Parse("20060102T150405Z", timestamp)
	if err != nil {
		return malformed("X-Goog-Date is not in the ISO 8601 basic format")
	}
	lifetime, err := strconv.ParseInt(query.Get("X-Goog-Expires"), 10, 64)
	if err != nil || lifetime < 1 || lifetime > maximumSignedSeconds {
		return malformed("X-Goog-Expires is not between 1 and 604800 seconds")
	}
	scope := timestamp[:8] + "/auto/storage/goog4_request"
	if query.Get("X-Goog-Credential") != serviceAccount+"/"+scope {
		return malformed("X-Goog-Credential is not " + serviceAccount + "/" + scope)
	}
	signature, err := hex.DecodeString(query.Get("X-Goog-Signature"))
	if err != nil {
		return malformed("X-Goog-Signature is not hexadecimal")
	}

	signedHeaders := strings.Split(query.Get("X-Goog-SignedHeaders"), ";")
	if !slices.IsSorted(signedHeaders) || !slices.Contains(signedHeaders, "host") {
		return malformed("X-Goog-SignedHeaders must be sorted and must name host")
	}
	var canonicalHeaders strings.Builder
	for _, name := range signedHeaders {
		value := strings.Join(r.Header.Values(name), ",")
		if name == "host" {
			value = r.Host
		}
		canonicalHeaders.WriteString(name + ":" + strings.TrimSpace(value) + "\n")
	}

	var parameters []string
	for name, values := range query {
		if name == "X-Goog-Signature" {
			continue
		}
		for _, value := range values {
			parameters = append(parameters, percentEncode(name)+"="+percentEncode(value))
		}
	}
	sort.Strings(parameters)

	canonicalRequest := strings.Join([]string{
		r.Method,
		r.URL.EscapedPath(),
		strings.Join(parameters, "&"),
		canonicalHeaders.String(),
		strings.Join(signedHeaders, ";"),
		"UNSIGNED-PAYLOAD",
	}, "\n")
	hashed := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{"GOOG4-RSA-SHA256", timestamp, scope, hex.EncodeToString(hashed[:])}, "\n")
	digest := sha256.Sum256([]byte(stringToSign))
	if rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, digest[:], signature) != nil {
		return &xmlRefusal{http.StatusForbidden, "SignatureDoesNotMatch", "Access denied. The request signature we calculated does not match the signature you provided. The canonical request was:\n" + canonicalRequest}
	}

	// The signature is checked before the dates so that a URL tampered with to
	// extend its life is refused for the tampering.
	now := time.Now()
	if now.Before(signedAt) {
		return &xmlRefusal{http.StatusForbidden, "AccessDenied", fmt.Sprintf("the signed URL is not usable before %s", timestamp)}
	}
	if !now.Before(signedAt.Add(time.Duration(lifetime) * time.Second)) {
		return &xmlRefusal{http.StatusBadRequest, "ExpiredToken", "The provided token has expired."}
	}
	return nil
}

// percentEncode leaves the URI unreserved characters as they are and encodes
// every other byte with upper-case hex digits, which is the canonical form of a
// query string's names and values.
func percentEncode(text string) string {
	const hexDigits = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		b := text[i]
		if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-' || b == '.' || b == '_' || b == '~' {
			out.WriteByte(b)
			continue
		}
		out.WriteByte('%')
		out.WriteByte(hexDigits[b>>4])
		out.WriteByte(hexDigits[b&0x0f])
	}
	return out.String()
}

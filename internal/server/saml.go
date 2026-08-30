package bleephub

import (
	"bytes"
	"compress/flate"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"

	"github.com/e6qu/bleephub/internal/store"
)

// SAML 2.0 service-provider sign-in. This mirrors the OIDC/shauth flow
// (identity.go): GET /auth/saml redirects the browser to the identity provider
// with a signed-assertion-expecting AuthnRequest, and POST /saml/consume is the
// assertion consumer service. The identity provider's X.509 certificate signs
// the response or its assertion; we validate that signature, then reuse the
// same session machinery (upsertExternalUser + createOIDCBrowserSession) OIDC
// uses. GET /saml/metadata publishes the SP descriptor.

const (
	samlNSProtocol  = "urn:oasis:names:tc:SAML:2.0:protocol"
	samlNSAssertion = "urn:oasis:names:tc:SAML:2.0:assertion"
	samlStatusOK    = "urn:oasis:names:tc:SAML:2.0:status:Success"
	// samlClockSkew tolerates small clock differences between the SP and the IdP
	// when checking assertion time conditions.
	samlClockSkew = 3 * time.Minute
)

// samlIDPCertificate parses the configured identity-provider certificate. It
// accepts a PEM block or a bare base64 DER certificate (as it appears inside
// IdP metadata <ds:X509Certificate>).
func (c identityConfig) samlIDPCertificate() (*x509.Certificate, error) {
	raw := strings.TrimSpace(c.samlIDPCertPEM)
	if raw == "" {
		return nil, errors.New("no certificate configured")
	}
	if block, _ := pem.Decode([]byte(raw)); block != nil {
		return x509.ParseCertificate(block.Bytes)
	}
	// Bare base64 DER: strip whitespace the metadata form often carries.
	compact := strings.NewReplacer("\n", "", "\r", "", " ", "", "\t", "").Replace(raw)
	der, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return nil, fmt.Errorf("certificate is neither PEM nor base64 DER: %w", err)
	}
	return x509.ParseCertificate(der)
}

// samlOrigin is the absolute origin the SP advertises to the IdP, from
// BLEEPHUB_EXTERNAL_URL when set, else reconstructed from the request.
func (s *Server) samlOrigin(r *http.Request) string {
	if s.externalURL != "" {
		return strings.TrimRight(s.externalURL, "/")
	}
	return requestOrigin(r)
}

func (s *Server) samlSPEntityID(r *http.Request) string {
	if id := strings.TrimSpace(s.identity.samlSPEntityID); id != "" {
		return id
	}
	return s.samlOrigin(r)
}

func (s *Server) samlACSURL(r *http.Request) string {
	return s.samlOrigin(r) + "/saml/consume"
}

// samlRequestID derives the AuthnRequest ID from the CSRF state. A SAML ID is an
// xs:ID (NCName) and must not start with a digit, so the hex state is prefixed.
func samlRequestID(state string) string { return "_" + state }

func (s *Server) handleSAMLLogin(w http.ResponseWriter, r *http.Request) {
	if !s.identity.samlConfigured() {
		writeGHError(w, http.StatusServiceUnavailable, "SAML sign-in is not configured")
		return
	}
	state, err := randomIdentityState()
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "could not start SAML sign-in")
		return
	}
	returnTo := safeIdentityReturnTo(r.URL.Query().Get("return_to"))
	if err := s.setIdentityState(w, r, identityState{Provider: "saml", State: state, ReturnTo: returnTo, ExpiresAt: time.Now().Add(10 * time.Minute)}); err != nil {
		writeGHError(w, http.StatusInternalServerError, "could not start SAML sign-in")
		return
	}

	authnRequest := s.buildSAMLAuthnRequest(r, samlRequestID(state))
	encoded, err := deflateAndEncode(authnRequest)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "could not encode SAML request")
		return
	}
	redirectURL, err := url.Parse(s.identity.samlIDPSSOURL)
	if err != nil {
		writeGHError(w, http.StatusBadGateway, "SAML identity-provider URL is invalid")
		return
	}
	query := redirectURL.Query()
	query.Set("SAMLRequest", encoded)
	query.Set("RelayState", state)
	redirectURL.RawQuery = query.Encode()
	http.Redirect(w, r, redirectURL.String(), http.StatusFound)
}

func (s *Server) buildSAMLAuthnRequest(r *http.Request, id string) string {
	doc := etree.NewDocument()
	req := doc.CreateElement("samlp:AuthnRequest")
	req.CreateAttr("xmlns:samlp", samlNSProtocol)
	req.CreateAttr("xmlns:saml", samlNSAssertion)
	req.CreateAttr("ID", id)
	req.CreateAttr("Version", "2.0")
	req.CreateAttr("IssueInstant", s.currentTime().UTC().Format(time.RFC3339))
	req.CreateAttr("Destination", s.identity.samlIDPSSOURL)
	req.CreateAttr("AssertionConsumerServiceURL", s.samlACSURL(r))
	req.CreateAttr("ProtocolBinding", "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST")
	issuer := req.CreateElement("saml:Issuer")
	issuer.SetText(s.samlSPEntityID(r))
	policy := req.CreateElement("samlp:NameIDPolicy")
	policy.CreateAttr("Format", "urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified")
	policy.CreateAttr("AllowCreate", "true")
	out, _ := doc.WriteToString()
	return out
}

// deflateAndEncode applies the HTTP-Redirect binding transform: raw DEFLATE,
// then standard base64 (the caller URL-encodes via url.Values).
func deflateAndEncode(xml string) (string, error) {
	var buf bytes.Buffer
	writer, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		return "", err
	}
	if _, err := writer.Write([]byte(xml)); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func (s *Server) handleSAMLConsume(w http.ResponseWriter, r *http.Request) {
	if !s.identity.samlConfigured() {
		writeGHError(w, http.StatusServiceUnavailable, "SAML sign-in is not configured")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeGHError(w, http.StatusBadRequest, "invalid SAML response encoding")
		return
	}
	rawResponse := r.PostFormValue("SAMLResponse")
	relayState := r.PostFormValue("RelayState")
	if rawResponse == "" {
		writeGHError(w, http.StatusBadRequest, "SAML response is missing")
		return
	}
	responseXML, err := base64.StdEncoding.DecodeString(rawResponse)
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "SAML response is not valid base64")
		return
	}

	// The state cookie is present for a service-provider-initiated flow on a
	// same-site POST; a cross-site IdP POST (SameSite=Lax) or an IdP-initiated
	// flow arrives without it. When present it binds InResponseTo and carries the
	// return target, laundered across the request boundary by the signed cookie.
	// When absent, the signed assertion still stands on its own, but the return
	// target falls back to the dashboard: RelayState is raw request input and must
	// not steer the post-login redirect (open-redirect guard).
	returnTo := "/ui/"
	expectedInResponseTo := ""
	if pending, stateErr := s.consumeIdentityState(w, r, "saml", relayState); stateErr == nil {
		returnTo = pending.ReturnTo
		expectedInResponseTo = samlRequestID(pending.State)
	}

	assertion, err := s.samlValidatedAssertion(responseXML)
	if err != nil {
		s.logger.Warn().Err(err).Msg("SAML response validation failed")
		writeGHError(w, http.StatusUnauthorized, "SAML response could not be validated")
		return
	}
	claims, err := s.parseSAMLAssertion(r, assertion, expectedInResponseTo)
	if err != nil {
		s.logger.Warn().Err(err).Msg("SAML assertion rejected")
		writeGHError(w, http.StatusUnauthorized, "SAML assertion was rejected")
		return
	}

	user, err := s.upsertExternalUser(s.identity.samlIDPEntityID, claims.nameID, claims.login, claims.name, claims.email, "", claims.admin, true)
	if err != nil {
		writeGHError(w, http.StatusForbidden, "SAML account cannot be provisioned on this instance")
		return
	}
	expiresAt := s.currentTime().Add(12 * time.Hour)
	if !claims.notOnOrAfter.IsZero() && claims.notOnOrAfter.Before(expiresAt) {
		expiresAt = claims.notOnOrAfter
	}
	if err := s.createOIDCBrowserSession(w, r, user, store.LoginSession{
		OIDCProvider: "saml",
		OIDCIssuer:   s.identity.samlIDPEntityID,
		OIDCSubject:  claims.nameID,
		OIDCSID:      claims.sessionIndex,
		ExpiresAt:    expiresAt,
	}); err != nil {
		s.logger.Error().Err(err).Msg("create browser session")
		writeGHError(w, http.StatusServiceUnavailable, "browser session is unavailable")
		return
	}
	redirectTarget := "/ui/"
	if strings.HasPrefix(returnTo, "/") && !strings.HasPrefix(returnTo, "//") && !strings.Contains(returnTo, "\\") {
		redirectTarget = returnTo
	}
	http.Redirect(w, r, redirectTarget, http.StatusFound)
}

// samlValidatedAssertion validates the enveloped XML-DSig signature on the SAML
// response or its assertion against the configured IdP certificate, returning
// the validated assertion element (signature stripped). Either the whole
// response or the assertion itself may be signed; at least one must be.
func (s *Server) samlValidatedAssertion(responseXML []byte) (*etree.Element, error) {
	cert, err := s.identity.samlIDPCertificate()
	if err != nil {
		return nil, err
	}
	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(responseXML); err != nil {
		return nil, fmt.Errorf("parse SAML response: %w", err)
	}
	root := doc.Root()
	if root == nil || root.Tag != "Response" {
		return nil, errors.New("SAML response root is not a Response element")
	}
	if status := samlStatusCode(root); status != samlStatusOK {
		return nil, fmt.Errorf("SAML response status is %q", status)
	}

	certStore := &dsig.MemoryX509CertificateStore{Roots: []*x509.Certificate{cert}}
	ctx := dsig.NewDefaultValidationContext(certStore)
	ctx.Clock = dsig.NewFakeClockAt(s.currentTime())

	if childByLocal(root, "Signature") != nil {
		validated, err := ctx.Validate(root)
		if err != nil {
			return nil, fmt.Errorf("response signature: %w", err)
		}
		assertion := childByLocal(validated, "Assertion")
		if assertion == nil {
			return nil, errors.New("signed SAML response carries no assertion")
		}
		return assertion, nil
	}
	assertion := childByLocal(root, "Assertion")
	if assertion == nil {
		return nil, errors.New("SAML response carries no assertion")
	}
	if childByLocal(assertion, "Signature") == nil {
		return nil, errors.New("neither the SAML response nor its assertion is signed")
	}
	validated, err := ctx.Validate(assertion)
	if err != nil {
		return nil, fmt.Errorf("assertion signature: %w", err)
	}
	return validated, nil
}

type samlClaims struct {
	nameID       string
	login        string
	name         string
	email        string
	admin        bool
	sessionIndex string
	notOnOrAfter time.Time
}

// parseSAMLAssertion validates the assertion's issuer, audience, recipient,
// InResponseTo, and time conditions, then extracts the subject and attributes.
func (s *Server) parseSAMLAssertion(r *http.Request, assertion *etree.Element, expectedInResponseTo string) (samlClaims, error) {
	var claims samlClaims
	now := s.currentTime()

	if issuer := textOfChild(assertion, "Issuer"); issuer != s.identity.samlIDPEntityID {
		return claims, fmt.Errorf("assertion issuer %q is not the configured identity provider", issuer)
	}

	subject := childByLocal(assertion, "Subject")
	if subject == nil {
		return claims, errors.New("assertion carries no Subject")
	}
	claims.nameID = strings.TrimSpace(textOfChild(subject, "NameID"))
	if claims.nameID == "" {
		return claims, errors.New("assertion Subject carries no NameID")
	}
	if confirmation := childByLocal(subject, "SubjectConfirmation"); confirmation != nil {
		if data := childByLocal(confirmation, "SubjectConfirmationData"); data != nil {
			if expectedInResponseTo != "" {
				if got := data.SelectAttrValue("InResponseTo", ""); got != expectedInResponseTo {
					return claims, errors.New("assertion InResponseTo does not match the sign-in request")
				}
			}
			if recipient := data.SelectAttrValue("Recipient", ""); recipient != "" && recipient != s.samlACSURL(r) {
				return claims, errors.New("assertion Recipient is not this service provider")
			}
			if notOnOrAfter := parseSAMLTime(data.SelectAttrValue("NotOnOrAfter", "")); !notOnOrAfter.IsZero() && !now.Add(-samlClockSkew).Before(notOnOrAfter) {
				return claims, errors.New("assertion subject confirmation has expired")
			}
		}
	}

	if conditions := childByLocal(assertion, "Conditions"); conditions != nil {
		if notBefore := parseSAMLTime(conditions.SelectAttrValue("NotBefore", "")); !notBefore.IsZero() && now.Add(samlClockSkew).Before(notBefore) {
			return claims, errors.New("assertion is not yet valid")
		}
		notOnOrAfter := parseSAMLTime(conditions.SelectAttrValue("NotOnOrAfter", ""))
		if !notOnOrAfter.IsZero() {
			if !now.Add(-samlClockSkew).Before(notOnOrAfter) {
				return claims, errors.New("assertion has expired")
			}
			claims.notOnOrAfter = notOnOrAfter
		}
		if !s.samlAudienceMatches(r, conditions) {
			return claims, errors.New("assertion audience does not include this service provider")
		}
	}

	if authn := childByLocal(assertion, "AuthnStatement"); authn != nil {
		claims.sessionIndex = authn.SelectAttrValue("SessionIndex", "")
	}

	attrs := samlAttributes(assertion)
	claims.login = samlAttrValue(attrs, "username", "login", "user.username", "uid", "urn:oid:0.9.2342.19200300.100.1.1")
	if claims.login == "" {
		claims.login = claims.nameID
	}
	claims.name = samlAttrValue(attrs, "full_name", "name", "displayname", "cn", "urn:oid:2.16.840.1.113730.3.1.241")
	claims.email = samlAttrValue(attrs, "emails", "email", "mail", "user.email", "urn:oid:0.9.2342.19200300.100.1.3")
	admin := samlAttrValue(attrs, "administrator", "admin", "siteadmin")
	claims.admin = strings.EqualFold(admin, "true") || admin == "1"
	if login := strings.TrimSpace(claims.login); login == "" {
		return claims, errors.New("assertion resolves no login")
	}
	return claims, nil
}

// samlAudienceMatches reports whether any AudienceRestriction/Audience equals the
// SP entityID. When the assertion declares no audience, it is not restricted.
func (s *Server) samlAudienceMatches(r *http.Request, conditions *etree.Element) bool {
	want := s.samlSPEntityID(r)
	sawAudience := false
	for _, restriction := range childrenByLocal(conditions, "AudienceRestriction") {
		for _, audience := range childrenByLocal(restriction, "Audience") {
			sawAudience = true
			if strings.TrimSpace(audience.Text()) == want {
				return true
			}
		}
	}
	return !sawAudience
}

func (s *Server) handleSAMLMetadata(w http.ResponseWriter, r *http.Request) {
	if !s.identity.samlConfigured() {
		writeGHError(w, http.StatusServiceUnavailable, "SAML sign-in is not configured")
		return
	}
	doc := etree.NewDocument()
	doc.CreateProcInst("xml", `version="1.0" encoding="UTF-8"`)
	ed := doc.CreateElement("md:EntityDescriptor")
	ed.CreateAttr("xmlns:md", "urn:oasis:names:tc:SAML:2.0:metadata")
	ed.CreateAttr("entityID", s.samlSPEntityID(r))
	sso := ed.CreateElement("md:SPSSODescriptor")
	sso.CreateAttr("AuthnRequestsSigned", "false")
	sso.CreateAttr("WantAssertionsSigned", "true")
	sso.CreateAttr("protocolSupportEnumeration", samlNSProtocol)
	acs := sso.CreateElement("md:AssertionConsumerService")
	acs.CreateAttr("Binding", "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST")
	acs.CreateAttr("Location", s.samlACSURL(r))
	acs.CreateAttr("index", "0")
	out, err := doc.WriteToString()
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "could not render SAML metadata")
		return
	}
	w.Header().Set("Content-Type", "application/samlmetadata+xml")
	_, _ = w.Write([]byte(out))
}

// --- small etree helpers (namespace-prefix-agnostic: match on local name) ---

func childByLocal(parent *etree.Element, local string) *etree.Element {
	for _, child := range parent.ChildElements() {
		if child.Tag == local {
			return child
		}
	}
	return nil
}

func childrenByLocal(parent *etree.Element, local string) []*etree.Element {
	var out []*etree.Element
	for _, child := range parent.ChildElements() {
		if child.Tag == local {
			out = append(out, child)
		}
	}
	return out
}

func textOfChild(parent *etree.Element, local string) string {
	if child := childByLocal(parent, local); child != nil {
		return strings.TrimSpace(child.Text())
	}
	return ""
}

func samlStatusCode(response *etree.Element) string {
	if status := childByLocal(response, "Status"); status != nil {
		if code := childByLocal(status, "StatusCode"); code != nil {
			return code.SelectAttrValue("Value", "")
		}
	}
	return ""
}

// samlAttributes flattens the AttributeStatement into a lower-cased name→value
// map (first value wins), so lookups tolerate the casing IdPs vary on.
func samlAttributes(assertion *etree.Element) map[string]string {
	out := map[string]string{}
	statement := childByLocal(assertion, "AttributeStatement")
	if statement == nil {
		return out
	}
	for _, attr := range childrenByLocal(statement, "Attribute") {
		name := attr.SelectAttrValue("Name", "")
		if name == "" {
			name = attr.SelectAttrValue("FriendlyName", "")
		}
		if name == "" {
			continue
		}
		value := textOfChild(attr, "AttributeValue")
		key := strings.ToLower(strings.TrimSpace(name))
		if _, exists := out[key]; !exists {
			out[key] = value
		}
	}
	return out
}

func samlAttrValue(attrs map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(attrs[strings.ToLower(key)]); value != "" {
			return value
		}
	}
	return ""
}

func parseSAMLTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

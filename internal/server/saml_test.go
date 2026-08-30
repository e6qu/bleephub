package bleephub

import (
	"bytes"
	"compress/flate"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
)

// TestSAMLServiceProviderLogin drives the whole SP flow: initiate at /auth/saml,
// build a signed SAML assertion the way an identity provider would, POST it to
// the assertion consumer service, and confirm a browser session is issued and
// the account provisioned. It exercises the real XML-DSig validation path.
func TestSAMLServiceProviderLogin(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	now := s.currentTime()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-idp"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	const idpEntityID = "https://idp.example.com/entity"
	s.identity.samlIDPSSOURL = "https://idp.example.com/sso"
	s.identity.samlIDPEntityID = idpEntityID
	s.identity.samlIDPCertPEM = string(certPEM)
	// Pin the SP origin so ACS URL and audience are stable across the round-trip.
	s.externalURL = s.baseURL
	spEntityID := s.baseURL
	acsURL := s.baseURL + "/saml/consume"

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// 1. Initiate — capture the state cookie, RelayState, and AuthnRequest ID.
	initReq, _ := http.NewRequest(http.MethodGet, s.baseURL+"/auth/saml?return_to=/ui/repos", nil)
	initResp, err := client.Do(initReq)
	if err != nil {
		t.Fatal(err)
	}
	initResp.Body.Close()
	if initResp.StatusCode != http.StatusFound {
		t.Fatalf("/auth/saml status = %d, want 302", initResp.StatusCode)
	}
	loc, err := url.Parse(initResp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	relayState := loc.Query().Get("RelayState")
	requestID := decodeAuthnRequestID(t, loc.Query().Get("SAMLRequest"))
	if relayState == "" || requestID != "_"+relayState {
		t.Fatalf("RelayState=%q requestID=%q inconsistent", relayState, requestID)
	}

	// 2. Build the signed response an IdP would POST back.
	responseXML := buildSignedSAMLResponse(t, samlResponseParams{
		key: key, certDER: certDER, idpEntityID: idpEntityID, spEntityID: spEntityID,
		acsURL: acsURL, inResponseTo: requestID, login: "alice", email: "alice@example.com",
		name: "Alice Example", admin: true, now: now,
	})
	form := url.Values{
		"SAMLResponse": {base64.StdEncoding.EncodeToString([]byte(responseXML))},
		"RelayState":   {relayState},
	}

	// 3. Consume.
	acsReq, _ := http.NewRequest(http.MethodPost, acsURL, strings.NewReader(form.Encode()))
	acsReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range initResp.Cookies() {
		acsReq.AddCookie(c)
	}
	acsResp, err := client.Do(acsReq)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(acsResp.Body)
	acsResp.Body.Close()
	if acsResp.StatusCode != http.StatusFound {
		t.Fatalf("/saml/consume status = %d, want 302; body=%s", acsResp.StatusCode, body)
	}
	if got := acsResp.Header.Get("Location"); got != "/ui/" {
		t.Fatalf("redirect = %q, want /ui/", got)
	}
	sessionSet := false
	for _, c := range acsResp.Cookies() {
		if strings.Contains(c.Name, "_gh_sess") && c.Value != "" {
			sessionSet = true
		}
	}
	if !sessionSet {
		t.Fatalf("no session cookie issued; cookies=%v", acsResp.Cookies())
	}

	user := s.store.UsersByLogin["alice"]
	if user == nil {
		t.Fatal("SAML login did not provision the alice account")
	}
	if !user.SiteAdmin {
		t.Errorf("administrator attribute did not grant SiteAdmin")
	}
	if user.Name != "Alice Example" || user.Email != "alice@example.com" {
		t.Errorf("attributes not mapped: name=%q email=%q", user.Name, user.Email)
	}
}

// TestSAMLConsumeRejectsUnsignedResponse ensures an assertion without a valid
// signature is refused.
func TestSAMLConsumeRejectsUnsignedResponse(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	now := s.currentTime()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "idp"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour)}
	certDER, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	s.identity.samlIDPSSOURL = "https://idp.example.com/sso"
	s.identity.samlIDPEntityID = "https://idp.example.com/entity"
	s.identity.samlIDPCertPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))
	s.externalURL = s.baseURL

	unsigned := `<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="_r" Version="2.0" IssueInstant="` + now.UTC().Format(time.RFC3339) + `">` +
		`<saml:Issuer>https://idp.example.com/entity</saml:Issuer>` +
		`<samlp:Status><samlp:StatusCode Value="urn:oasis:names:tc:SAML:2.0:status:Success"/></samlp:Status>` +
		`<saml:Assertion ID="_a" Version="2.0" IssueInstant="` + now.UTC().Format(time.RFC3339) + `"><saml:Issuer>https://idp.example.com/entity</saml:Issuer>` +
		`<saml:Subject><saml:NameID>mallory</saml:NameID></saml:Subject></saml:Assertion></samlp:Response>`
	form := url.Values{"SAMLResponse": {base64.StdEncoding.EncodeToString([]byte(unsigned))}}
	req, _ := http.NewRequest(http.MethodPost, s.baseURL+"/saml/consume", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned response status = %d, want 401", resp.StatusCode)
	}
	if s.store.UsersByLogin["mallory"] != nil {
		t.Fatal("unsigned SAML response provisioned an account")
	}
}

func decodeAuthnRequestID(t *testing.T, samlRequest string) string {
	t.Helper()
	deflated, err := base64.StdEncoding.DecodeString(samlRequest)
	if err != nil {
		t.Fatalf("decode SAMLRequest: %v", err)
	}
	reader := flate.NewReader(bytes.NewReader(deflated))
	xmlBytes, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("inflate SAMLRequest: %v", err)
	}
	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(xmlBytes); err != nil {
		t.Fatalf("parse AuthnRequest: %v", err)
	}
	return doc.Root().SelectAttrValue("ID", "")
}

type samlResponseParams struct {
	key                                           *rsa.PrivateKey
	certDER                                       []byte
	idpEntityID, spEntityID, acsURL, inResponseTo string
	login, email, name                            string
	admin                                         bool
	now                                           time.Time
}

func buildSignedSAMLResponse(t *testing.T, p samlResponseParams) string {
	t.Helper()
	instant := p.now.UTC().Format(time.RFC3339)
	notBefore := p.now.Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	notOnOrAfter := p.now.Add(5 * time.Minute).UTC().Format(time.RFC3339)

	// The assertion self-declares its namespace so it stays valid once embedded.
	assertion := etree.NewElement("saml:Assertion")
	assertion.CreateAttr("xmlns:saml", samlNSAssertion)
	assertion.CreateAttr("ID", "_assertion1")
	assertion.CreateAttr("Version", "2.0")
	assertion.CreateAttr("IssueInstant", instant)
	assertion.CreateElement("saml:Issuer").SetText(p.idpEntityID)

	subject := assertion.CreateElement("saml:Subject")
	nameID := subject.CreateElement("saml:NameID")
	nameID.CreateAttr("Format", "urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified")
	nameID.SetText(p.login)
	confirmation := subject.CreateElement("saml:SubjectConfirmation")
	confirmation.CreateAttr("Method", "urn:oasis:names:tc:SAML:2.0:cm:bearer")
	scd := confirmation.CreateElement("saml:SubjectConfirmationData")
	scd.CreateAttr("InResponseTo", p.inResponseTo)
	scd.CreateAttr("Recipient", p.acsURL)
	scd.CreateAttr("NotOnOrAfter", notOnOrAfter)

	conditions := assertion.CreateElement("saml:Conditions")
	conditions.CreateAttr("NotBefore", notBefore)
	conditions.CreateAttr("NotOnOrAfter", notOnOrAfter)
	conditions.CreateElement("saml:AudienceRestriction").CreateElement("saml:Audience").SetText(p.spEntityID)

	authn := assertion.CreateElement("saml:AuthnStatement")
	authn.CreateAttr("AuthnInstant", instant)
	authn.CreateAttr("SessionIndex", "_session1")
	authn.CreateElement("saml:AuthnContext").CreateElement("saml:AuthnContextClassRef").
		SetText("urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport")

	stmt := assertion.CreateElement("saml:AttributeStatement")
	addAttr := func(name, value string) {
		attr := stmt.CreateElement("saml:Attribute")
		attr.CreateAttr("Name", name)
		attr.CreateElement("saml:AttributeValue").SetText(value)
	}
	addAttr("username", p.login)
	addAttr("email", p.email)
	addAttr("full_name", p.name)
	if p.admin {
		addAttr("administrator", "true")
	}

	tlsCert := tls.Certificate{PrivateKey: p.key, Certificate: [][]byte{p.certDER}}
	signingCtx := dsig.NewDefaultSigningContext(dsig.TLSCertKeyStore(tlsCert))
	// Exclusive canonicalization keeps the signature valid once the assertion is
	// re-parented under the Response, as real SAML requires.
	signingCtx.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
	signed, err := signingCtx.SignEnveloped(assertion)
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}

	doc := etree.NewDocument()
	resp := doc.CreateElement("samlp:Response")
	resp.CreateAttr("xmlns:samlp", samlNSProtocol)
	resp.CreateAttr("xmlns:saml", samlNSAssertion)
	resp.CreateAttr("ID", "_response1")
	resp.CreateAttr("Version", "2.0")
	resp.CreateAttr("IssueInstant", instant)
	resp.CreateAttr("Destination", p.acsURL)
	resp.CreateAttr("InResponseTo", p.inResponseTo)
	resp.CreateElement("saml:Issuer").SetText(p.idpEntityID)
	resp.CreateElement("samlp:Status").CreateElement("samlp:StatusCode").CreateAttr("Value", samlStatusOK)
	resp.AddChild(signed)

	out, err := doc.WriteToString()
	if err != nil {
		t.Fatalf("serialize SAML response: %v", err)
	}
	return out
}

package bleephub

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http/httptest"
	"testing"
	"time"
)

// samlFuzzCertPEM is an immutable self-signed IdP certificate built once at
// package init (not in a fuzz body), so the per-execution server the fuzz target
// builds can be configured without regenerating an RSA key every input.
var samlFuzzCertPEM = func() string {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fuzz-idp"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}()

// FuzzSAMLResponse feeds arbitrary bytes to the SAML assertion consumer's
// validation path. A malformed, adversarial, or oversized SAML response must
// surface as an error (unsigned / bad signature / malformed XML), never a panic
// — and the XML parser must not expand external or recursive entities. The
// server is built inside the closure (per execution) so no state is shared
// across fuzz inputs.
func FuzzSAMLResponse(f *testing.F) {
	f.Add([]byte(``))
	f.Add([]byte(`<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol"/>`))
	f.Add([]byte(`<Response><Assertion><Signature/></Assertion></Response>`))
	f.Add([]byte(`<!DOCTYPE r [<!ENTITY a "x">]><Response>&a;</Response>`))
	f.Add([]byte("<Response" + string(make([]byte, 64)) + ">"))

	f.Fuzz(func(t *testing.T, xml []byte) {
		s := newTestServer()
		s.identity.samlIDPSSOURL = "https://idp.example.com/sso"
		s.identity.samlIDPEntityID = "https://idp.example.com/entity"
		s.identity.samlIDPCertPEM = samlFuzzCertPEM
		s.externalURL = "https://sp.example.com"

		assertion, err := s.samlValidatedAssertion(xml)
		if err != nil || assertion == nil {
			return
		}
		req := httptest.NewRequest("POST", "https://sp.example.com/saml/consume", nil)
		_, _ = s.parseSAMLAssertion(req, assertion, "")
	})
}

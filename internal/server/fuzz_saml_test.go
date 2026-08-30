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

// samlFuzzServer builds one SAML-configured server the fuzz body reuses. The
// validation path it exercises (samlValidatedAssertion / parseSAMLAssertion) is
// read-only on the server, so a single shared instance is safe under the
// fuzzer's concurrent workers.
func samlFuzzServer(tb testing.TB) *Server {
	tb.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		tb.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fuzz-idp"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		tb.Fatal(err)
	}
	s := newTestServer()
	s.identity.samlIDPSSOURL = "https://idp.example.com/sso"
	s.identity.samlIDPEntityID = "https://idp.example.com/entity"
	s.identity.samlIDPCertPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	s.externalURL = "https://sp.example.com"
	return s
}

// FuzzSAMLResponse feeds arbitrary bytes to the SAML assertion consumer's
// validation path. A malformed, adversarial, or oversized SAML response must
// surface as an error (unsigned / bad signature / malformed XML), never a panic
// — and the XML parser must not expand external or recursive entities.
func FuzzSAMLResponse(f *testing.F) {
	s := samlFuzzServer(f)
	req := httptest.NewRequest("POST", "https://sp.example.com/saml/consume", nil)

	f.Add([]byte(``))
	f.Add([]byte(`<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol"/>`))
	f.Add([]byte(`<Response><Assertion><Signature/></Assertion></Response>`))
	f.Add([]byte(`<!DOCTYPE r [<!ENTITY a "x">]><Response>&a;</Response>`))
	f.Add([]byte("<Response" + string(make([]byte, 64)) + ">"))

	f.Fuzz(func(t *testing.T, xml []byte) {
		assertion, err := s.samlValidatedAssertion(xml)
		if err != nil || assertion == nil {
			return
		}
		// A validly-signed assertion is spec-impossible from random bytes, but if
		// one is ever produced its attribute/condition parsing must also not panic.
		_, _ = s.parseSAMLAssertion(req, assertion, "")
	})
}

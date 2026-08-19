package bleephub

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// newUntrustedTLSServer starts a loopback TLS receiver whose self-signed
// certificate is deliberately NOT in webhookDeliveryTestRoots, so the
// verifying webhook client must refuse it and only insecure_ssl=1 delivers.
func newUntrustedTLSServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "insecure-hook-test"},
		// Fixed window covering any realistic run date: TLS validation uses
		// the real clock, and the wall-clock gate forbids time.Now in tests.
		NotBefore:    time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	ts := httptest.NewUnstartedServer(handler)
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

// TestWebhookDeliveryHonorsInsecureSSL pins that config.insecure_ssl selects
// the delivery client: "1" delivers to a self-signed receiver the verifying
// client must refuse, "0" fails against the same receiver.
func TestWebhookDeliveryHonorsInsecureSSL(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)
	receiver := newUntrustedTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	createHook := func(insecureSSL string) int {
		t.Helper()
		hook := decodeJSONWithStatus(t, s.post(t, repo.path()+"/hooks", defaultToken, map[string]interface{}{
			"config": map[string]interface{}{
				"url":          receiver.URL,
				"content_type": "json",
				"insecure_ssl": insecureSSL,
			},
			"events": []string{"push"},
		}), http.StatusCreated)
		return int(hook["id"].(float64))
	}
	waitForDelivery := func(hookID int) *store.WebhookDelivery {
		t.Helper()
		for attempt := 0; attempt < 200; attempt++ {
			if deliveries := s.store.ListDeliveries(hookID); len(deliveries) > 0 {
				return deliveries[0]
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("hook %d produced no delivery record", hookID)
		return nil
	}

	// Active hooks ping on creation; the ping is the delivery under test.
	insecureHook := createHook("1")
	if d := waitForDelivery(insecureHook); d.StatusCode != http.StatusOK {
		t.Errorf("insecure_ssl=1 delivery status = %d, want 200 (TLS verification skipped)", d.StatusCode)
	}
	verifyingHook := createHook("0")
	if d := waitForDelivery(verifyingHook); d.StatusCode != 0 {
		t.Errorf("insecure_ssl=0 delivery status = %d, want 0 (self-signed peer refused)", d.StatusCode)
	}
}

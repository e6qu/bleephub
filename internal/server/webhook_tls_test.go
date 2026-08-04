package bleephub

import (
	"crypto/x509"
	"net/http"
	"net/http/httptest"
)

// installWebhookTestTLSRoots makes the verifying webhook delivery transport
// trust the certificate that every httptest.NewTLSServer shares, so the
// https-only delivery path can be exercised against loopback test receivers
// without opting each hook into insecure_ssl. Production leaves
// webhookDeliveryTestRoots nil and uses the system root store.
func installWebhookTestTLSRoots() {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer ts.Close()
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	webhookDeliveryTestRoots = pool
}

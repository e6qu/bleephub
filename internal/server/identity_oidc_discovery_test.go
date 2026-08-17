package bleephub

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coreos/go-oidc/v3/oidc"
)

// TestOIDCDiscoveryReachesCoLocatedIssuer covers issue #168: OpenID Connect
// discovery to the operator-configured provider must not be refused just because
// the provider resolves to a private/loopback address, which is normal for a
// co-located Shauth. Routing discovery through the webhook SSRF gate 502'd every
// sign-in in that topology. The provider is not a user-supplied destination, so
// it uses an ordinary client while the webhook guard stays scoped to webhooks.
func TestOIDCDiscoveryReachesCoLocatedIssuer(t *testing.T) {
	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"jwks_uri":%q}`,
			issuer, issuer+"/auth", issuer+"/token", issuer+"/jwks")
	})
	ts := httptest.NewServer(mux) // binds 127.0.0.1 — a loopback (non-public) address
	defer ts.Close()
	issuer = ts.URL

	s := newTestServer()
	// OIDC discovery uses an ordinary client for the operator-configured IdP, not
	// the webhook SSRF gate, so a co-located (loopback/container) issuer resolves.
	if _, err := oidc.NewProvider(s.oidcClientContext(context.Background()), issuer); err != nil {
		t.Fatalf("issue #168: OIDC discovery to a co-located (loopback) issuer was refused: %v", err)
	}
}

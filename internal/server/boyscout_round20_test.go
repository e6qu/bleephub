package bleephub

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOIDCSubjectCannotBeForgedViaQuery pins that the OIDC token's subject and
// claims come from the job token's authoritative run context, not from
// runner-supplied query parameters. A job actually running on a feature branch
// with no environment cannot mint a token claiming :ref:refs/heads/main or
// :environment:production to assume a cloud role it isn't entitled to.
func TestOIDCSubjectCannotBeForgedViaQuery(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.seedRepo(t, "oidc-forge", false)
	repo := "admin/oidc-forge"

	// The authoritative run: a feature branch, no environment.
	jobToken, _ := testJobTokenWithOIDC(t, s.Server, repo, &oidcRunClaims{
		Ref: "refs/heads/feature", Sha: "0123456789abcdef0123456789abcdef01234567",
		RunID: "42", RunNumber: "7", RunAttempt: "1",
		EventName: "push", Workflow: "CI", WorkflowFile: "ci.yml",
	})

	// The runner forges main + production in the request query.
	forged := "repo=" + repo +
		"&ref=refs/heads/main&environment=production" +
		"&sha=ffffffffffffffffffffffffffffffffffffffff&run_id=9999" +
		"&run_number=1&run_attempt=1&workflow=Evil&workflow_file=evil.yml&event_name=push"
	req := httptest.NewRequest("GET", "/token?"+forged, nil)
	req.Header.Set("Authorization", "Bearer "+jobToken)
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/token = %d, body %s", w.Code, w.Body.String())
	}
	claims := decodeForgedOIDC(t, w.Body.Bytes())

	// The subject and claims reflect the real run, not the forged query.
	if claims["sub"] != "repo:admin/oidc-forge:ref:refs/heads/feature" {
		t.Fatalf("sub = %v, want repo:admin/oidc-forge:ref:refs/heads/feature (the real run, not the forged main/production)", claims["sub"])
	}
	if claims["ref"] != "refs/heads/feature" {
		t.Errorf("ref = %v, want refs/heads/feature", claims["ref"])
	}
	if claims["environment"] != "" {
		t.Errorf("environment = %v, want empty (the job targets no environment)", claims["environment"])
	}
	if claims["run_id"] != "42" {
		t.Errorf("run_id = %v, want 42 (the real run)", claims["run_id"])
	}
	if claims["sha"] == "ffffffffffffffffffffffffffffffffffffffff" {
		t.Errorf("sha was taken from the forged query")
	}
}

func decodeForgedOIDC(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("token envelope: %v", err)
	}
	parts := strings.Split(env.Value, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", env.Value)
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(pb, &claims); err != nil {
		t.Fatalf("payload JSON: %v", err)
	}
	return claims
}

package bleephub

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/rs/zerolog"
)

// TestSeedPreRegisteredApp proves the coordinate-only consumer flow of issue
// #559: after an operator seeds an App at startup, a consumer drives it holding
// only base URL + app id + private key against the standard /api/v3/app surface,
// with zero bleephub-specific client configuration.
func TestSeedPreRegisteredApp(t *testing.T) {
	// The private key the consumer "already holds", as on real GitHub where the App is registered out of band.
	pemKey := testSeedPrivateKeyPEM(t)

	const appID = 4242
	const accountLogin = "admin"
	const instID = 9001

	seed, _ := json.Marshal([]map[string]any{{
		"id":              appID,
		"slug":            "ci-bot",
		"name":            "CI Bot",
		"private_key_pem": pemKey,
		"owner":           accountLogin,
		"permissions":     map[string]string{"contents": "read"},
		"events":          []string{"push"},
		"installations": []map[string]any{{
			"id":          instID,
			"account":     accountLogin,
			"target_type": "User",
		}},
	}})
	// Operator-only config; the consumer's client never sees this.
	t.Setenv("BLEEPHUB_SEED_APPS", string(seed))

	srv := NewServer("127.0.0.1:0", zerolog.Nop())
	useFixedTestClock(srv)
	// Mirror ListenAndServe's handler chain so the app-JWT auth middleware runs; httptest serves the bare mux otherwise.
	handler := srv.requestHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	if a := srv.store.GetApp(appID); a == nil {
		t.Fatalf("app %d was not seeded into the store", appID)
	}

	// ---- From here on, act ONLY as a coordinate-only client. ----
	jwt, err := signAppJWT(pemKey, appID, fixedTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.ParseAndVerifyAppJWT(jwt); err != nil {
		t.Fatalf("seeded key does not verify the app JWT: %v", err)
	}
	bearer := func(method, path string) *http.Response {
		req, _ := http.NewRequest(method, ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	appResp := bearer("GET", "/api/v3/app")
	defer appResp.Body.Close()
	if appResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /app with seeded JWT: got %d, want 200", appResp.StatusCode)
	}
	var appObj map[string]any
	json.NewDecoder(appResp.Body).Decode(&appObj)
	if int(appObj["id"].(float64)) != appID {
		t.Errorf("GET /app id = %v, want %d", appObj["id"], appID)
	}
	if appObj["slug"] != "ci-bot" {
		t.Errorf("GET /app slug = %v, want ci-bot", appObj["slug"])
	}

	instResp := bearer("GET", "/api/v3/app/installations")
	body, _ := io.ReadAll(instResp.Body)
	instResp.Body.Close()
	if instResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /app/installations: got %d, want 200", instResp.StatusCode)
	}
	var insts []map[string]any
	json.Unmarshal(body, &insts)
	if len(insts) != 1 || int(insts[0]["id"].(float64)) != instID {
		t.Fatalf("installations = %s, want one with id %d", body, instID)
	}
	acct, _ := insts[0]["account"].(map[string]any)
	if acct["login"] != accountLogin {
		t.Errorf("installation account = %v, want %s", acct["login"], accountLogin)
	}

	// The consumer mints an installation token by coordinates alone.
	tokResp := bearer("POST", "/api/v3/app/installations/9001/access_tokens")
	tokBody, _ := io.ReadAll(tokResp.Body)
	tokResp.Body.Close()
	if tokResp.StatusCode != http.StatusCreated {
		t.Fatalf("create installation token: got %d, want 201 (%s)", tokResp.StatusCode, tokBody)
	}
	var tok map[string]any
	json.Unmarshal(tokBody, &tok)
	if s, _ := tok["token"].(string); s == "" {
		t.Fatalf("installation token response missing token: %s", tokBody)
	}
}

func TestSeedPreRegisteredAppRequiresExplicitExistingOwner(t *testing.T) {
	pemKey := testSeedPrivateKeyPEM(t)
	seed, _ := json.Marshal([]map[string]any{{
		"id":              5001,
		"slug":            "ownerless",
		"name":            "Ownerless",
		"private_key_pem": pemKey,
	}})
	t.Setenv("BLEEPHUB_SEED_APPS", string(seed))

	srv := &Server{store: store.NewStore(), logger: zerolog.Nop()}
	srv.store.SeedDefaultUser()
	err := srv.seedConfiguredApps()
	if err == nil || err.Error() != `seed app "Ownerless": owner is required` {
		t.Fatalf("seedConfiguredApps error = %v, want explicit owner requirement", err)
	}
	if app := srv.store.GetApp(5001); app != nil {
		t.Fatalf("app was created despite missing owner: %#v", app)
	}
}

func TestSeedPreRegisteredAppRejectsUnknownInstallationAccount(t *testing.T) {
	pemKey := testSeedPrivateKeyPEM(t)
	seed, _ := json.Marshal([]map[string]any{{
		"id":              5002,
		"slug":            "unknown-install",
		"name":            "Unknown Install",
		"private_key_pem": pemKey,
		"owner":           "admin",
		"installations": []map[string]any{{
			"id":      7001,
			"account": "missing-org",
		}},
	}})
	t.Setenv("BLEEPHUB_SEED_APPS", string(seed))

	srv := &Server{store: store.NewStore(), logger: zerolog.Nop()}
	srv.store.SeedDefaultUser()
	err := srv.seedConfiguredApps()
	if err == nil || err.Error() != `seed app "Unknown Install": installation account "missing-org" does not resolve to an existing user or organization` {
		t.Fatalf("seedConfiguredApps error = %v, want unknown installation account failure", err)
	}
	if org := srv.store.GetOrg("missing-org"); org != nil {
		t.Fatalf("missing installation account was auto-created: %#v", org)
	}
	if inst := srv.store.GetInstallation(7001); inst != nil {
		t.Fatalf("installation was created despite missing account: %#v", inst)
	}
}

// TestSeedAppIdempotentAndBadKey covers the two guards: re-seeding the same id is a no-op, and a malformed PEM is rejected loud rather than producing an unusable App.
func TestSeedAppIdempotentAndBadKey(t *testing.T) {
	st := store.NewStore()
	st.SeedDefaultUser()

	pemKey := testSeedPrivateKeyPEM(t)
	spec := store.AppSeedSpec{ID: 77, Name: "Dup App"}

	app1, created1, err := st.SeedApp(spec, pemKey, "admin")
	if err != nil || !created1 || app1.ID != 77 {
		t.Fatalf("first seed: created=%v err=%v id=%d", created1, err, app1.ID)
	}
	app2, created2, err := st.SeedApp(spec, pemKey, "admin")
	if err != nil || created2 || app2.ID != 77 {
		t.Fatalf("re-seed must be a no-op: created=%v err=%v", created2, err)
	}

	if _, _, err := st.SeedApp(store.AppSeedSpec{ID: 78, Name: "Bad"}, "not-a-pem", "admin"); err == nil {
		t.Fatal("seed with a malformed key must error, not produce an unusable App")
	}
}

func testSeedPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

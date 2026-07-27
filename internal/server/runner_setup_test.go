package bleephub

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
)

// jitRunnerPrivateKey reads `.credentials_rsaparams` the way the runner does —
// as .NET's RSAParameters, whose members are named in the serialized form and
// whose byte widths are fixed by the modulus length. A component trimmed to
// its significant bytes is rejected by .NET outright, so the widths are
// asserted here rather than left to a runner nobody can run in a unit test.
func jitRunnerPrivateKey(t *testing.T, params []byte) *rsa.PrivateKey {
	t.Helper()
	var stored struct {
		D        []byte `json:"d"`
		DP       []byte `json:"dp"`
		DQ       []byte `json:"dq"`
		Exponent []byte `json:"exponent"`
		InverseQ []byte `json:"inverseQ"`
		Modulus  []byte `json:"modulus"`
		P        []byte `json:"p"`
		Q        []byte `json:"q"`
	}
	if err := json.Unmarshal(params, &stored); err != nil {
		t.Fatalf(".credentials_rsaparams is not RSAParameters JSON: %v", err)
	}
	size := len(stored.Modulus)
	if size == 0 {
		t.Fatal(".credentials_rsaparams carries no modulus")
	}
	if len(stored.D) != size {
		t.Errorf(".credentials_rsaparams d is %d bytes, want the modulus length %d; .NET refuses any other width",
			len(stored.D), size)
	}
	for name, component := range map[string][]byte{
		"dp": stored.DP, "dq": stored.DQ, "inverseQ": stored.InverseQ, "p": stored.P, "q": stored.Q,
	} {
		if len(component) != size/2 {
			t.Errorf(".credentials_rsaparams %s is %d bytes, want half the modulus length %d; .NET refuses any other width",
				name, len(component), size/2)
		}
	}

	key := &rsa.PrivateKey{
		PublicKey: rsa.PublicKey{
			N: new(big.Int).SetBytes(stored.Modulus),
			E: int(new(big.Int).SetBytes(stored.Exponent).Int64()),
		},
		D:      new(big.Int).SetBytes(stored.D),
		Primes: []*big.Int{new(big.Int).SetBytes(stored.P), new(big.Int).SetBytes(stored.Q)},
	}
	if err := key.Validate(); err != nil {
		t.Fatalf(".credentials_rsaparams is not a usable RSA key: %v", err)
	}
	key.Precompute()
	return key
}

// TestRegistrationTokenRandom verifies the repo registration token is a
// random per-request opaque value (not the old hardcoded constant) with a
// near-term expiry, and that an authenticated caller gets 201.
func TestRegistrationTokenRandom(t *testing.T) {
	ensureSeededRepo(testServer, "admin/regtok")
	mint := func() (string, string) {
		resp := ghPost(t, "/api/v3/repos/admin/regtok/actions/runners/registration-token", defaultToken, map[string]interface{}{})
		if resp.StatusCode != 201 {
			resp.Body.Close()
			t.Fatalf("registration-token = %d, want 201", resp.StatusCode)
		}
		var body struct {
			Token     string `json:"token"`
			ExpiresAt string `json:"expires_at"`
		}
		json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		return body.Token, body.ExpiresAt
	}

	t1, exp1 := mint()
	t2, _ := mint()
	if t1 == "" || t2 == "" {
		t.Fatal("registration token must be non-empty")
	}
	if t1 == "BLEEPHUB_REG_TOKEN" {
		t.Error("registration token must not be the hardcoded constant")
	}
	if t1 == t2 {
		t.Error("registration token must be random per request, got identical values")
	}
	if exp1 == "" || exp1 == "2099-01-01T00:00:00Z" {
		t.Errorf("expires_at must be a near-term TTL, got %q", exp1)
	}
}

func TestAgentRSAPublicKeyRequiresProtocolStandardBase64(t *testing.T) {
	pub, err := agentRSAPublicKey(&AgentPublicKey{
		Modulus:  base64.StdEncoding.EncodeToString([]byte{0x01, 0x02, 0x03}),
		Exponent: base64.StdEncoding.EncodeToString([]byte{0x01, 0x00, 0x01}),
	})
	if err != nil {
		t.Fatalf("standard-base64 public key rejected: %v", err)
	}
	if pub.E != 65537 {
		t.Fatalf("exponent = %d, want 65537", pub.E)
	}

	for name, pk := range map[string]*AgentPublicKey{
		"url-safe modulus": {
			Modulus:  base64.URLEncoding.EncodeToString([]byte{0xff, 0xff}),
			Exponent: base64.StdEncoding.EncodeToString([]byte{0x01, 0x00, 0x01}),
		},
		"raw standard modulus": {
			Modulus:  base64.RawStdEncoding.EncodeToString([]byte{0xff, 0xff}),
			Exponent: base64.StdEncoding.EncodeToString([]byte{0x01, 0x00, 0x01}),
		},
		"raw url-safe exponent": {
			Modulus:  base64.StdEncoding.EncodeToString([]byte{0x01, 0x02, 0x03}),
			Exponent: base64.RawURLEncoding.EncodeToString([]byte{0xff, 0xff}),
		},
	} {
		if _, err := agentRSAPublicKey(pk); err == nil {
			t.Fatalf("%s was accepted; runner public keys must use protocol-standard base64", name)
		}
	}
}

// TestRemoveToken verifies the repo removal token endpoint returns the
// {token, expires_at} shape with 201 for an authenticated caller.
func TestRemoveToken(t *testing.T) {
	ensureSeededRepo(testServer, "admin/rmtok")
	resp := ghPost(t, "/api/v3/repos/admin/rmtok/actions/runners/remove-token", defaultToken, map[string]interface{}{})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("remove-token = %d, want 201", resp.StatusCode)
	}
	var body struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body.Token == "" {
		t.Error("remove token must be non-empty")
	}
	if body.ExpiresAt == "" {
		t.Error("remove token must carry expires_at")
	}
}

// TestGenerateJITConfig verifies the repo generate-jitconfig endpoint mints
// a runner + a decodable base64 JIT config, validates required fields, and
// registers the runner so it appears in the runners list.
func TestGenerateJITConfig(t *testing.T) {
	ensureSeededRepo(testServer, "admin/jit")
	// Missing required fields → 422.
	bad := ghPost(t, "/api/v3/repos/admin/jit/actions/runners/generate-jitconfig", defaultToken,
		map[string]interface{}{"name": "jit-runner"})
	if bad.StatusCode != 422 {
		bad.Body.Close()
		t.Fatalf("jitconfig missing fields = %d, want 422", bad.StatusCode)
	}
	bad.Body.Close()

	rgid := 1
	resp := ghPost(t, "/api/v3/repos/admin/jit/actions/runners/generate-jitconfig", defaultToken,
		map[string]interface{}{
			"name":            "jit-runner",
			"runner_group_id": rgid,
			"labels":          []string{"self-hosted", "linux"},
		})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("jitconfig = %d, want 201", resp.StatusCode)
	}
	var body struct {
		Runner struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		} `json:"runner"`
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()

	if body.Runner.ID == 0 || body.Runner.Name != "jit-runner" {
		t.Errorf("runner = %+v, want id>0 name=jit-runner", body.Runner)
	}
	if len(body.Runner.Labels) != 2 {
		t.Errorf("runner labels = %d, want 2", len(body.Runner.Labels))
	}
	if body.EncodedJITConfig == "" {
		t.Fatal("encoded_jit_config must be non-empty")
	}
	// The runner must be able to consume the JIT config. It deserializes the
	// decoded blob as a map of file name to the base64 of that file's
	// contents and writes each entry into its root directory verbatim, so
	// anything that is not a string of base64 fails to deserialize and the
	// runner terminates before it configures itself.
	raw, err := base64.StdEncoding.DecodeString(body.EncodedJITConfig)
	if err != nil {
		t.Fatalf("encoded_jit_config is not valid base64: %v", err)
	}
	var blob map[string]string
	if err := json.Unmarshal(raw, &blob); err != nil {
		t.Fatalf("decoded JIT config does not deserialize as a file-name-to-contents map, which is the only shape the runner accepts: %v; got %s", err, raw)
	}
	files := map[string][]byte{}
	for name, encoded := range blob {
		contents, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("JIT config entry %s is not base64 file contents: %v", name, err)
		}
		files[name] = contents
	}
	for _, name := range []string{".runner", ".credentials", ".credentials_rsaparams"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("JIT config carries no %s; the runner writes each entry as a file and cannot configure without this one: %s", name, raw)
		}
	}

	var settings struct {
		AgentID   int    `json:"agentId"`
		AgentName string `json:"agentName"`
		Ephemeral bool   `json:"ephemeral"`
		ServerURL string `json:"serverUrl"`
	}
	if err := json.Unmarshal(files[".runner"], &settings); err != nil {
		t.Fatalf(".runner is not the runner's settings JSON: %v", err)
	}
	if int64(settings.AgentID) != body.Runner.ID || settings.AgentName != "jit-runner" || !settings.Ephemeral || settings.ServerURL == "" {
		t.Errorf(".runner = %+v, want agent %d named jit-runner, ephemeral, with a server url", settings, body.Runner.ID)
	}

	var credentials struct {
		Scheme string            `json:"scheme"`
		Data   map[string]string `json:"data"`
	}
	if err := json.Unmarshal(files[".credentials"], &credentials); err != nil {
		t.Fatalf(".credentials is not the runner's credential JSON: %v", err)
	}
	if credentials.Scheme != "OAuth" || credentials.Data["clientId"] == "" || credentials.Data["authorizationUrl"] == "" {
		t.Fatalf(".credentials = %+v, want the OAuth scheme with a clientId and an authorizationUrl", credentials)
	}

	// The private key the runner signs its client_assertion with comes from
	// the JIT config too — it generates none on this path — so the server must
	// have recorded the matching public half against the agent. Signing with
	// the key it was handed is the only proof of that.
	key := jitRunnerPrivateKey(t, files[".credentials_rsaparams"])
	form := runnerTokenExchangeForm(signTestAssertion(t, key, credentials.Data["clientId"]))
	tokenResp, err := http.Post(credentials.Data["authorizationUrl"],
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("JIT token exchange: %v", err)
	}
	defer tokenResp.Body.Close()
	if tokenResp.StatusCode != 200 {
		payload, _ := io.ReadAll(tokenResp.Body)
		t.Fatalf("JIT runner token exchange = %d, want 200; body=%s", tokenResp.StatusCode, payload)
	}
	var issued struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&issued); err != nil {
		t.Fatalf("decode JIT session token: %v", err)
	}
	if issued.AccessToken == "" {
		t.Fatal("JIT runner token exchange issued no access_token")
	}

	// The minted runner must be registered (visible in the runners list).
	listResp := ghGet(t, "/api/v3/repos/admin/jit/actions/runners", defaultToken)
	if listResp.StatusCode != 200 {
		listResp.Body.Close()
		t.Fatalf("list runners = %d, want 200", listResp.StatusCode)
	}
	var list struct {
		Runners []struct {
			ID int64 `json:"id"`
		} `json:"runners"`
	}
	json.NewDecoder(listResp.Body).Decode(&list)
	listResp.Body.Close()
	found := false
	for _, rr := range list.Runners {
		if rr.ID == body.Runner.ID {
			found = true
			break
		}
	}
	if !found {
		t.Error("JIT-minted runner not present in runners list")
	}
}

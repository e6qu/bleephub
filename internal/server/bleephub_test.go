package bleephub

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/crypto/ssh"

	"github.com/e6qu/bleephub/internal/server/testutil"
	"github.com/e6qu/bleephub/internal/store"
)

var (
	testBaseURL string
	testServer  *Server
	testSSHAddr string
	testSSHKey  ed25519.PrivateKey
)

var fixedTestTime = time.Date(2042, time.July, 15, 12, 0, 0, 0, time.UTC)

func useFixedTestClock(server *Server) {
	clockNow := func() time.Time { return fixedTestTime }
	server.replaceClockNow(clockNow)
}

// authedGet issues a GET against the live test server with the admin
// token, the way the bleephub UI authenticates against /internal/*.
func authedGet(t *testing.T, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", testBaseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+defaultToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// authedPost mirrors http.Post but adds the admin token, for the /internal/*
// sim-control endpoints which the internal-auth middleware gates. The path is
// relative to testBaseURL; signature matches http.Post for drop-in use.
func authedPost(path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest("POST", testBaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+defaultToken)
	return http.DefaultClient.Do(req)
}

// serveTestRequest is the single request-building core the package's various
// do*Req helpers adapt to their own signatures (TEST-018): it builds a request
// (JSON Content-Type when body is non-nil), applies authHeader if given, serves
// it through the full middleware chain, and returns the recorder. It is distinct
// from authedGet/authedPost, which drive the live httptest listener over HTTP.
func serveTestRequest(s *Server, authHeader, method, path string, body []byte) *httptest.ResponseRecorder {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	return w
}

// jsonBodyBytes maps a string request body to serveTestRequest's []byte: an
// empty string means "no body" (nil), matching the do*Req helpers that took a
// string body.
func jsonBodyBytes(body string) []byte {
	if body == "" {
		return nil
	}
	return []byte(body)
}

// bearerHeader formats a Bearer credential, or "" for no Authorization header.
func bearerHeader(token string) string {
	if token == "" {
		return ""
	}
	return "Bearer " + token
}

func TestMain(m *testing.M) {
	// The fuzz targets build isolated in-memory servers when they need one.
	// Starting the package-wide HTTP/SSH harness in the coordinator and every
	// fuzz worker both wastes a listener per process and can prevent baseline
	// collection from ever completing. Keep the one environment value those
	// isolated fixtures require, then leave fuzz processes to m.Run.
	os.Setenv("BLEEPHUB_ADMIN_TOKEN", defaultToken)
	os.Setenv("BLEEPHUB_PERSISTENCE_ENCRYPTION_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if fuzzProcess(os.Args[1:]) {
		os.Exit(m.Run())
	}

	// Clear MinIO containers left by a suite that did not reach its own
	// cleanup — a timeout panic, an interrupt, a kill. This runs on every
	// ordinary suite, including one whose S3 tests are filtered out, because a
	// run that never starts a server is exactly the run that would otherwise
	// let abandoned ones accumulate unnoticed. Fuzz workers were returned
	// above because they must not coordinate shared external state.
	reapAbandonedS3Servers()

	// The admin token has no default — every consumer (incl. the test harness)
	// must set it explicitly. defaultToken is the non-PAT value the tests use.
	// Webhook delivery accepts http and https; many tests still use TLS
	// httptest.NewTLSServer receivers, so trust the shared certificate here to
	// let those loopback receivers verify without insecure_ssl.
	installWebhookTestTLSRoots()
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate SSH host key: %v\n", err)
		os.Exit(1)
	}
	hostKeyBlock, err := ssh.MarshalPrivateKey(hostKey, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal SSH host key: %v\n", err)
		os.Exit(1)
	}
	os.Setenv("BLEEPHUB_SSH_HOST_KEY", string(pem.EncodeToMemory(hostKeyBlock)))
	sshListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "find SSH port: %v\n", err)
		os.Exit(1)
	}
	testSSHAddr = sshListener.Addr().String()
	sshListener.Close()
	os.Setenv("BLEEPHUB_SSH_ADDR", testSSHAddr)
	_, testSSHKey, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate SSH client key: %v\n", err)
		os.Exit(1)
	}

	// Individual logging tests construct their own captured logger. The
	// package-wide fixture serves thousands of requests, and debug access logs
	// otherwise bury the actual failing assertion in hundreds of kilobytes.
	logger := zerolog.Nop()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to find free port: %v\n", err)
		os.Exit(1)
	}
	addr := ln.Addr().String()
	ln.Close()

	testBaseURL = "http://" + addr

	srv := NewServer(addr, logger)
	testServer = srv
	useFixedTestClock(testServer)
	// Typed-nil source ratchet (see graphql_source_audit_test.go): every
	// GraphQL request the suite makes doubles as an audit of the source maps
	// the resolvers build.
	instrumentGraphQLSourceAudit(srv.graphql.Schema())
	// The package-wide fixture intentionally reuses one credential across
	// thousands of otherwise independent tests. Keep that synthetic principal
	// from coupling late tests to early request volume; dedicated rate-limit
	// tests use isolated servers and the production 5,000-request budget.
	rateRequest, err := http.NewRequest(http.MethodGet, testBaseURL+"/api/v3/user", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build test rate-limit request: %v\n", err)
		os.Exit(1)
	}
	rateRequest.Header.Set("Authorization", "Bearer "+defaultToken)
	anonymousRateRequest, err := http.NewRequest(http.MethodGet, testBaseURL+"/api/v3/user", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build anonymous test rate-limit request: %v\n", err)
		os.Exit(1)
	}
	anonymousRateRequest.RemoteAddr = "127.0.0.1:1"
	for index, identity := range []string{apiRateIdentity(rateRequest), apiRateIdentity(anonymousRateRequest)} {
		for resource := range apiRateResourceLimits {
			limit := apiRateResourceLimits[resource]
			if index == 1 && resource == "core" {
				limit = 60
			}
			testServer.rateLimits[identity+"\x1f"+resource] = &apiRateWindow{
				Limit:     limit,
				Reset:     testRateLimitReset,
				unbounded: true,
			}
		}
	}

	// Give the shared test server a real on-disk packages directory so
	// package-file upload/download tests exercise real bytes.
	packageDataDir, err := os.MkdirTemp("", "bleephub-packages-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create packages temp dir: %v\n", err)
		os.Exit(1)
	}
	testServer.store.PackageDataDir = packageDataDir

	// Response-shape validation against the vendored GitHub OpenAPI
	// description rides the shared server; the ratchet runs after m.Run()
	// (see openapi_shape_validator_test.go).
	validator, err := newShapeValidator()
	if err != nil {
		fmt.Fprintf(os.Stderr, "openapi shape validator: %v\n", err)
		os.Exit(1)
	}
	apiShapeValidator = validator
	srv.responseObserver = validator.Observe

	// The shared harness server lives for the whole run; shutdown is exercised
	// against its own server in lifecycle_test.go rather than here.
	go srv.ListenAndServe(context.Background())

	// Wait for server to be ready without coupling correctness to one fixed
	// sleep that is either unnecessarily slow or too short on a loaded host.
	ready := testutil.TestEventually(2500*time.Millisecond, 50*time.Millisecond, func() bool {
		resp, err := http.Get(testBaseURL + "/health")
		if err == nil {
			_ = resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		}
		return false
	})
	if !ready {
		fmt.Fprintln(os.Stderr, "shared test server did not become ready")
		os.Exit(1)
	}

	code := m.Run()

	if s3ServerContainer != "" {
		if output, err := boundedDockerCleanupOutput("rm", "--force", s3ServerContainer); err != nil {
			fmt.Fprintf(os.Stderr, "remove MinIO S3 test server: %v\n%s", err, output)
			if code == 0 {
				code = 1
			}
		}
	}
	_ = os.RemoveAll(packageDataDir)

	// Coverage floor (PAR-011): on a full run the shape-parity gate must have
	// validated a substantial number of /api/v3 responses. A collapse toward
	// zero — the observer unwired, or nearly every exchange skipped — makes a
	// green ratchet meaningless, so fail loudly instead. Only on a full run: a
	// `-run <subset>` (or the `-run ^$` fuzz pass) legitimately observes few.
	if isFullTestRun() {
		validated, exchanges := apiShapeValidator.coverage()
		// Always report the count so a near-floor result is diagnosable from any
		// run's log (a floor breach is otherwise the only time the number is
		// visible, which makes an intermittent dip look like a mystery).
		fmt.Fprintf(os.Stderr, "openapi-shape coverage: %d/%d /api/v3 responses validated (floor %d)\n", validated, exchanges, minShapeCoverage)
		if validated < minShapeCoverage {
			fmt.Fprintf(os.Stderr, "openapi-shape coverage floor: only %d /api/v3 response(s) validated against the OpenAPI description (floor %d) — the parity gate has gone vacuous\n", validated, minShapeCoverage)
			if code == 0 {
				code = 1
			}
		}
	}

	if report := graphqlSourceAuditReport(); report != "" {
		fmt.Fprint(os.Stderr, report)
		if code == 0 {
			code = 1
		}
	}

	if newKeys, total := apiShapeValidator.ratchet(); len(newKeys) > 0 {
		fmt.Fprintf(os.Stderr, "\nopenapi-shape ratchet: %d NEW response-shape violation(s) vs ../../third_party/github-openapi.json.gz (total observed: %d):\n", len(newKeys), total)
		for _, key := range newKeys {
			fmt.Fprintf(os.Stderr, "  %s\n", key)
		}
		fmt.Fprintf(os.Stderr, "Fix the response shape, or file a BUG and add the key to openapi-violation-allowlist.txt with its BUG ID.\n")
		if code == 0 {
			code = 1
		}
	}

	// Dead-entry sweep (PAR-022): on a full run every allowlisted deviation
	// should have been triggered by the endpoint it cites. An entry no observed
	// violation matched is a dead suppression that only inflates the ledger, so
	// fail and name it for removal. Guarded to full runs — a subset legitimately
	// exercises few of the cited endpoints.
	if isFullTestRun() {
		if unused, err := apiShapeValidator.unusedAllowlistEntries(); err != nil {
			fmt.Fprintf(os.Stderr, "openapi-shape allowlist dead-entry sweep skipped: %v\n", err)
		} else if len(unused) > 0 {
			fmt.Fprintf(os.Stderr, "\nopenapi-shape allowlist: %d dead entry/entries never triggered by any exercised endpoint (PAR-022) — remove them and lower the ceiling:\n", len(unused))
			for _, key := range unused {
				fmt.Fprintf(os.Stderr, "  %s\n", key)
			}
			if code == 0 {
				code = 1
			}
		}
	}

	os.Exit(code)
}

func fuzzProcess(args []string) bool {
	for _, arg := range args {
		if arg == "-test.fuzzworker" ||
			strings.HasPrefix(arg, "-test.fuzzworker=") ||
			strings.HasPrefix(arg, "-test.fuzz=") {
			return true
		}
	}
	return false
}

func TestHealth(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp, err := http.Get(s.baseURL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestConnectionData(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp, err := http.Get(s.baseURL + "/_apis/connectionData")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var data map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&data)

	instanceID, _ := data["instanceId"].(string)
	if instanceID == "" {
		t.Fatal("missing instanceId")
	}

	locData, _ := data["locationServiceData"].(map[string]interface{})
	defs, _ := locData["serviceDefinitions"].([]interface{})
	if len(defs) == 0 {
		t.Fatal("no service definitions")
	}
}

func TestOAuthToken(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	// Register a runner with an RSA public key, then exchange a signed
	// client_assertion JWT for an access token — the real Azure DevOps
	// agent OAuth2 jwt-bearer flow the actions/runner uses.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	mod := base64.StdEncoding.EncodeToString(key.N.Bytes())
	exp := base64.StdEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
	regBody := fmt.Sprintf(`{"name":"oauth-test","version":"2.0","authorization":{"publicKey":{"modulus":%q,"exponent":%q}}}`, mod, exp)
	regToken := mustRunnerRegistrationToken(t, store.RunnerScope{Repo: "oauth-owner/oauth-repo"})
	regResp := runnerDo(t, "POST", s.baseURL+"/_apis/v1/Agent/1", regToken, regBody)
	defer regResp.Body.Close()
	if regResp.StatusCode != 200 {
		t.Fatalf("agent register: expected 200, got %d", regResp.StatusCode)
	}
	var agent struct {
		ID            int `json:"id"`
		Authorization struct {
			ClientID string `json:"clientId"`
		} `json:"authorization"`
	}
	if err := json.NewDecoder(regResp.Body).Decode(&agent); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	if agent.Authorization.ClientID == "" {
		t.Fatal("missing clientId on registered agent")
	}

	for _, algorithm := range []string{"RS256", "PS256"} {
		t.Run(algorithm, func(t *testing.T) {
			form := runnerTokenExchangeForm(signTestAssertionWithAlgorithm(t, key, agent.Authorization.ClientID, algorithm))
			resp, err := http.Post(s.baseURL+"/_apis/v1/auth/", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
			}

			var data map[string]interface{}
			if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if data["access_token"] == nil {
				t.Fatal("missing access_token")
			}
		})
	}
}

func TestOAuthTokenRejectsMissingAssertion(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp, err := http.Post(s.baseURL+"/_apis/v1/auth/", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 for empty body, got %d", resp.StatusCode)
	}
}

func TestOAuthTokenRejectsUnknownClient(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	form := runnerTokenExchangeForm(signTestAssertion(t, key, "00000000-0000-0000-0000-000000000000"))
	resp, err := http.Post(s.baseURL+"/_apis/v1/auth/", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 for unregistered clientId, got %d", resp.StatusCode)
	}
}

func signTestAssertion(t *testing.T, key *rsa.PrivateKey, clientID string) string {
	t.Helper()
	return signTestAssertionWithAlgorithm(t, key, clientID, "RS256")
}

func signTestAssertionWithAlgorithm(t *testing.T, key *rsa.PrivateKey, clientID, algorithm string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"alg":%q,"typ":"JWT"}`, algorithm)))
	now := fixedTestTime.Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
		`{"iss":%q,"iat":%d,"exp":%d}`, clientID, now, now+300,
	)))
	signInput := header + "." + payload
	hash := sha256.Sum256([]byte(signInput))
	var (
		sig []byte
		err error
	)
	switch algorithm {
	case "PS256":
		sig, err = rsa.SignPSS(rand.Reader, key, crypto.SHA256, hash[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	default:
		sig, err = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	}
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// runnerTokenExchangeForm is the body the runner posts to its authorizationUrl:
// the client credentials grant, with the client authenticated by an RSA
// assertion rather than a secret.
func runnerTokenExchangeForm(assertion string) url.Values {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
	form.Set("client_assertion", assertion)
	return form
}

func TestRunnerRegistration(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	body := `{"url":"http://localhost","runner_event":"register"}`
	// config.sh presents the administration:write-minted registration token.
	regToken := mustRunnerRegistrationToken(t, store.RunnerScope{Repo: "reg-owner/reg-repo"})

	if unauth := runnerDo(t, "POST", s.baseURL+"/api/v3/actions/runner-registration", "", body); true {
		unauth.Body.Close()
		if unauth.StatusCode != 401 {
			t.Fatalf("unauthenticated runner-registration: expected 401, got %d", unauth.StatusCode)
		}
	}

	resp := runnerDo(t, "POST", s.baseURL+"/api/v3/actions/runner-registration", regToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var data map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&data)

	if data["token"] == nil {
		t.Fatal("missing token")
	}
	if data["token_schema"] != "OAuthAccessToken" {
		t.Fatalf("unexpected token_schema: %v", data["token_schema"])
	}
}

func TestListPools(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	sessionToken, _ := testAgentSession(t, s.Server, store.RunnerScope{Repo: "pool-owner/pool-repo"})
	resp := runnerDo(t, "GET", s.baseURL+"/_apis/v1/AgentPools", sessionToken, "")
	defer resp.Body.Close()

	var data map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&data)

	count, _ := data["count"].(float64)
	if count != 1 {
		t.Fatalf("expected 1 pool, got %v", data["count"])
	}
}

func TestAgentLifecycle(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	// Register agent with the credential config.sh was given.
	agentBody := `{"name":"test-runner","version":"3.0.0","labels":[{"name":"self-hosted","type":"system"}]}`
	regToken := mustRunnerRegistrationToken(t, store.RunnerScope{Repo: "life-owner/life-repo"})
	resp := runnerDo(t, "POST", s.baseURL+"/_apis/v1/Agent/1", regToken, agentBody)
	defer resp.Body.Close()

	var agent struct {
		ID            int    `json:"id"`
		Name          string `json:"name"`
		Authorization struct {
			ClientID string `json:"clientId"`
		} `json:"authorization"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&agent); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	agentID := agent.ID
	if agentID == 0 {
		t.Fatal("agent ID should be non-zero")
	}
	if agent.Name != "test-runner" {
		t.Fatalf("unexpected name: %v", agent.Name)
	}

	// From here the runner holds the session token its client_assertion
	// exchange returns.
	sessionToken := makeJWT(agent.Authorization.ClientID, runnerAudSession)

	resp2 := runnerDo(t, "GET", s.baseURL+"/_apis/v1/Agent/1", sessionToken, "")
	defer resp2.Body.Close()

	var list map[string]interface{}
	json.NewDecoder(resp2.Body).Decode(&list)

	agents := list["value"].([]interface{})
	if len(agents) == 0 {
		t.Fatal("expected at least 1 agent")
	}

	resp3 := runnerDo(t, "GET", fmt.Sprintf("%s/_apis/v1/Agent/1/%d", s.baseURL, agentID), sessionToken, "")
	defer resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Fatalf("get agent: expected 200, got %d", resp3.StatusCode)
	}

	resp4 := runnerDo(t, "DELETE", fmt.Sprintf("%s/_apis/v1/Agent/1/%d", s.baseURL, agentID), sessionToken, "")
	defer resp4.Body.Close()
	if resp4.StatusCode != 200 {
		t.Fatalf("delete agent: expected 200, got %d", resp4.StatusCode)
	}

	// Verify deleted. The deleted runner's own token no longer resolves to an
	// agent, so the check runs as another runner registered for the same
	// scope.
	observerToken, _ := testAgentSession(t, s.Server, store.RunnerScope{Repo: "life-owner/life-repo"})
	resp5 := runnerDo(t, "GET", fmt.Sprintf("%s/_apis/v1/Agent/1/%d", s.baseURL, agentID), observerToken, "")
	defer resp5.Body.Close()
	if resp5.StatusCode != 404 {
		t.Fatalf("expected 404 after delete, got %d", resp5.StatusCode)
	}
}

func TestSessionAndMessage(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	sessionToken, agent := testAgentSession(t, s.Server, store.RunnerScope{Repo: "sess-owner/sess-repo"})
	sessionBody := fmt.Sprintf(`{"ownerName":"RUNNER","agent":{"id":%d,"name":"test"}}`, agent.ID)
	resp := runnerDo(t, "POST", s.baseURL+"/_apis/v1/AgentSession/1", sessionToken, sessionBody)
	defer resp.Body.Close()

	var session map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&session)

	sessionID, _ := session["sessionId"].(string)
	if sessionID == "" {
		t.Fatal("missing sessionId")
	}

	jobBody := `{"image":"alpine:latest","steps":[{"run":"echo hello"}]}`
	resp2, err := s.authedPost("/internal/exec/submit", "application/json", bytes.NewBufferString(jobBody))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()

	// Poll for the message — just submitted, so it arrives immediately.
	resp3 := runnerDo(t, "GET", s.baseURL+"/_apis/v1/Message/1?sessionId="+sessionID, sessionToken, "")
	defer resp3.Body.Close()

	body, _ := io.ReadAll(resp3.Body)
	if len(body) == 0 {
		t.Fatal("expected a message, got empty response")
	}

	var msg store.TaskAgentMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("failed to parse message: %v", err)
	}

	if msg.MessageType != "PipelineAgentJobRequest" {
		t.Fatalf("unexpected message type: %s", msg.MessageType)
	}

	runnerDo(t, "DELETE", s.baseURL+"/_apis/v1/AgentSession/1/"+sessionID, sessionToken, "").Body.Close()
}

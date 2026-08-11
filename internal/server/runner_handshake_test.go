package bleephub

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The configuration handshake of the official actions/runner, step for step,
// against the live test server.
//
// Every gate on the runner control plane was verified to reject a caller with
// no credential. What that cannot show is the other half — that a runner
// holding exactly the credentials `config.sh` has at each point still gets
// through. A gate one credential too tight fails identically to a missing
// route: 401, before the runner has configured itself at all. So each step
// here asserts both directions, and the sequence is the one the runner
// actually performs:
//
//	operator  POST /api/v3/repos/{owner}/{repo}/actions/runners/registration-token
//	           (or the {org} form) — administration:write mints the token
//	           config.sh is invoked with
//	runner    POST /api/v3/actions/runner-registration   RemoteAuth <token>
//	           exchanges it for the tenant URL and tenant token
//	runner    GET  /_apis/connectionData                 no credential
//	runner    GET  /_apis/v1/AgentPools                  tenant token
//	runner    GET  /_apis/v1/Agent/{poolId}?agentName=   tenant token
//	           GetAgentsAsync: config.sh looks its own name up before it
//	           decides between adding and replacing
//	runner    POST /_apis/v1/Agent/{poolId}              tenant token
//	           AddAgentAsync, carrying the RSA public key it just generated
//	           (or PUT /_apis/v1/Agent/{poolId}/{agentId} for --replace)
//	runner    GET  /_apis/connectionData                 no credential
//	           the credential test: the runner reconnects on the OAuth
//	           credential it just saved, and its HTTP stack sends the first
//	           request of that session with nothing attached
//	runner    GET  /_apis/v1/AgentPools?poolType=…       no credential
//	           answered with a Bearer challenge, which is the only thing that
//	           makes the runner ask for a token at all
//	runner    POST /_apis/v1/auth/                       RSA client_assertion
//	           exchanged for the agent session bearer
//	runner    GET  /_apis/v1/AgentPools?poolType=…       session bearer
//	           the retry that ends configuration with "Runner connection is good"
//	runner    POST /_apis/v1/AgentSession/{poolId}       session bearer
//	runner    GET  /_apis/v1/Message/{poolId}?sessionId= session bearer

// handshakeRunner carries the state one runner accumulates while configuring
// itself: the key pair it generates, the tenant token it exchanges its
// registration token for, and the session bearer it ends up holding.
type handshakeRunner struct {
	srv        *isolatedServer
	t          *testing.T
	name       string
	scopePath  string // "/owner/repo" or "/org", the --url path config.sh is given
	key        *rsa.PrivateKey
	setupToken string
	agentID    int
	clientID   string
	session    string
	sessionID  string
}

// handshakeCall issues one request against the live test server and returns
// its status, response headers and body. authorization is sent verbatim, so a
// step can present the RemoteAuth scheme the runner uses.
func handshakeCall(t *testing.T, baseURL, method, path, authorization, contentType, body string) (int, http.Header, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return resp.StatusCode, resp.Header, payload
}

// handshakeStep runs one step of the handshake and fails with the step's name
// when it does not succeed, so an over-tight gate names itself.
func handshakeStep(t *testing.T, baseURL, step, method, path, authorization, contentType, body string, want int) []byte {
	t.Helper()
	status, _, payload := handshakeCall(t, baseURL, method, path, authorization, contentType, body)
	if status != want {
		t.Fatalf("%s: %s %s = %d, want %d; body=%s", step, method, path, status, want, payload)
	}
	return payload
}

// refuseAnonymous asserts the same route is unreachable without a credential.
// Every step of the handshake is paired with one of these: the point of the
// test is that widening a gate for the runner did not open it to everyone.
//
// A runner protocol route has to refuse in the runner's own terms as well. The
// runner opens every session by sending a request with no credential attached
// and asks for one only when the refusal names the Bearer scheme, so a 401
// without that challenge ends configuration instead of starting the token
// exchange.
func refuseAnonymous(t *testing.T, baseURL, step, method, path, contentType, body string) {
	t.Helper()
	status, header, payload := handshakeCall(t, baseURL, method, path, "", contentType, body)
	if status != http.StatusUnauthorized {
		t.Errorf("%s: anonymous %s %s = %d, want 401; body=%s", step, method, path, status, payload)
		return
	}
	if !runnerRouteChallenges(method, path) {
		return
	}
	if challenge := header.Get("WWW-Authenticate"); !strings.Contains(challenge, "Bearer") {
		t.Errorf("%s: anonymous %s %s answered 401 with WWW-Authenticate %q, which names no Bearer challenge; the runner will never ask for a token",
			step, method, path, challenge)
	}
}

// runnerRouteChallenges reports whether a 401 from this route has to name the
// Bearer scheme. Every runner protocol route that accepts an agent session
// does, because the runner's OAuth credential asks for a token only when it is
// challenged for one. Agent registration is the exception: it accepts the
// token config.sh was handed and no session at all, so challenging there would
// advertise a credential the route refuses.
func runnerRouteChallenges(method, path string) bool {
	if !strings.HasPrefix(path, "/_apis/") {
		return false
	}
	return !(method == "POST" && strings.HasPrefix(path, "/_apis/v1/Agent/"))
}

func (s *isolatedServer) newHandshakeRunner(t *testing.T, name, scopePath string) *handshakeRunner {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate runner key: %v", err)
	}
	return &handshakeRunner{srv: s, t: t, name: name, scopePath: scopePath, key: key}
}

// publicKeyJSON encodes the runner's RSA public key the way the agent protocol
// carries it: standard-base64 modulus and exponent.
func (h *handshakeRunner) publicKeyJSON() string {
	return fmt.Sprintf(`{"exponent":%q,"modulus":%q}`,
		base64.StdEncoding.EncodeToString(big.NewInt(int64(h.key.E)).Bytes()),
		base64.StdEncoding.EncodeToString(h.key.N.Bytes()))
}

// mintRegistrationToken is the operator's half: an administration:write
// caller mints the token `config.sh --token` is invoked with.
func (h *handshakeRunner) mintRegistrationToken(path string) string {
	h.t.Helper()
	refuseAnonymous(h.t, h.srv.baseURL, "mint registration token", "POST", path, "application/json", "{}")
	payload := handshakeStep(h.t, h.srv.baseURL, "mint registration token", "POST", path,
		"token "+defaultToken, "application/json", "{}", http.StatusCreated)
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(payload, &minted); err != nil {
		h.t.Fatalf("decode registration token: %v", err)
	}
	if minted.Token == "" {
		h.t.Fatal("registration token response carried no token")
	}
	return minted.Token
}

// exchangeTenantCredential is GetTenantCredential: config.sh trades the
// registration token for the tenant URL and the credential it uses for the
// rest of configuration.
func (h *handshakeRunner) exchangeTenantCredential(registrationToken, runnerEvent string) {
	h.t.Helper()
	const path = "/api/v3/actions/runner-registration"
	body := fmt.Sprintf(`{"url":%q,"runner_event":%q}`, h.srv.baseURL+h.scopePath, runnerEvent)
	refuseAnonymous(h.t, h.srv.baseURL, "tenant credential", "POST", path, "application/json", body)

	payload := handshakeStep(h.t, h.srv.baseURL, "tenant credential", "POST", path,
		"RemoteAuth "+registrationToken, "application/json", body, http.StatusOK)
	var tenant struct {
		URL         string `json:"url"`
		TokenSchema string `json:"token_schema"`
		Token       string `json:"token"`
	}
	if err := json.Unmarshal(payload, &tenant); err != nil {
		h.t.Fatalf("decode tenant credential: %v", err)
	}
	if tenant.Token == "" {
		h.t.Fatal("tenant credential response carried no token")
	}
	if tenant.TokenSchema != "OAuthAccessToken" {
		h.t.Fatalf("token_schema = %q, want OAuthAccessToken", tenant.TokenSchema)
	}
	if !strings.HasSuffix(tenant.URL, h.scopePath) {
		h.t.Fatalf("tenant url = %q, want the configured scope path %q preserved", tenant.URL, h.scopePath)
	}
	h.setupToken = tenant.Token
}

func (h *handshakeRunner) connectionData(authorization string) {
	h.t.Helper()
	// Service discovery is read before the runner holds any credential and
	// again with the session bearer once it has one; both must answer.
	handshakeStep(h.t, h.srv.baseURL, "connectionData", "GET", "/_apis/connectionData", authorization, "", "", http.StatusOK)
}

func (h *handshakeRunner) listPools() {
	h.t.Helper()
	refuseAnonymous(h.t, h.srv.baseURL, "list pools", "GET", "/_apis/v1/AgentPools", "", "")
	handshakeStep(h.t, h.srv.baseURL, "list pools", "GET", "/_apis/v1/AgentPools",
		"Bearer "+h.setupToken, "", "", http.StatusOK)
}

// testConnection is the step that runs once the pool has accepted the
// registration: the runner reconnects on the OAuth credential it derives from
// the authorization record it was handed, and reads the pool list a second
// time to force that credential into use.
//
// It holds no token at this point, so the call goes out bare, is challenged,
// and is only then exchanged and retried. Nothing before this step exercises
// the OAuth credential — the tenant token carried everything up to here — so
// the whole exchange lives or dies on this one call being challenged.
func (h *handshakeRunner) testConnection() {
	h.t.Helper()
	const path = "/_apis/v1/AgentPools?poolType=Automation"
	h.connectionData("")
	refuseAnonymous(h.t, h.srv.baseURL, "credential test", "GET", path, "", "")
	h.exchangeSessionToken()
	handshakeStep(h.t, h.srv.baseURL, "credential test", "GET", path, "Bearer "+h.session, "", "", http.StatusOK)
	h.connectionData("Bearer " + h.session)
}

// lookUpOwnName is GetAgentsAsync — the call config.sh makes to decide
// between adding a registration and replacing one. It runs on the tenant
// token, long before any agent session exists.
func (h *handshakeRunner) lookUpOwnName() []Agent {
	h.t.Helper()
	path := "/_apis/v1/Agent/1?agentName=" + url.QueryEscape(h.name)
	refuseAnonymous(h.t, h.srv.baseURL, "look up own name", "GET", path, "", "")
	payload := handshakeStep(h.t, h.srv.baseURL, "look up own name", "GET", path,
		"Bearer "+h.setupToken, "", "", http.StatusOK)
	var listing struct {
		Count int     `json:"count"`
		Value []Agent `json:"value"`
	}
	if err := json.Unmarshal(payload, &listing); err != nil {
		h.t.Fatalf("decode agent listing: %v", err)
	}
	if listing.Count != len(listing.Value) {
		h.t.Fatalf("agent listing count = %d but carried %d agents", listing.Count, len(listing.Value))
	}
	return listing.Value
}

func (h *handshakeRunner) agentBody() string {
	return fmt.Sprintf(
		`{"name":%q,"version":"2.330.0","osDescription":"Linux","ephemeral":false,`+
			`"labels":[{"name":"self-hosted","type":"custom"}],"authorization":{"publicKey":%s}}`,
		h.name, h.publicKeyJSON())
}

// addAgent is AddAgentAsync.
func (h *handshakeRunner) addAgent() {
	h.t.Helper()
	const path = "/_apis/v1/Agent/1"
	body := h.agentBody()
	refuseAnonymous(h.t, h.srv.baseURL, "add agent", "POST", path, "application/json", body)
	payload := handshakeStep(h.t, h.srv.baseURL, "add agent", "POST", path,
		"Bearer "+h.setupToken, "application/json", body, http.StatusOK)
	h.readAgent("add agent", payload)
}

// replaceAgent is ReplaceAgentAsync — what `config.sh --replace` does when the
// pool already carries the runner's name. The runner presents a freshly
// generated key pair here, and signs its next client_assertion with it.
func (h *handshakeRunner) replaceAgent(agentID int) {
	h.t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		h.t.Fatalf("generate replacement key: %v", err)
	}
	h.key = key
	path := fmt.Sprintf("/_apis/v1/Agent/1/%d", agentID)
	body := h.agentBody()
	refuseAnonymous(h.t, h.srv.baseURL, "replace agent", "PUT", path, "application/json", body)
	payload := handshakeStep(h.t, h.srv.baseURL, "replace agent", "PUT", path,
		"Bearer "+h.setupToken, "application/json", body, http.StatusOK)
	h.readAgent("replace agent", payload)
}

func (h *handshakeRunner) readAgent(step string, payload []byte) {
	h.t.Helper()
	var agent Agent
	if err := json.Unmarshal(payload, &agent); err != nil {
		h.t.Fatalf("%s: decode agent: %v", step, err)
	}
	if agent.ID == 0 {
		h.t.Fatalf("%s: agent record carries no id", step)
	}
	if agent.Authorization == nil || agent.Authorization.ClientID == "" {
		h.t.Fatalf("%s: agent record carries no clientId", step)
	}
	if agent.Authorization.AuthorizationURL == "" {
		h.t.Fatalf("%s: agent record carries no authorizationUrl", step)
	}
	h.agentID = agent.ID
	h.clientID = agent.Authorization.ClientID
}

// exchangeSessionToken is the token exchange the runner performs against the
// authorizationUrl on its agent record: the client credentials grant, with the
// client authenticated by an assertion signed with the key it registered.
func (h *handshakeRunner) exchangeSessionToken() {
	h.t.Helper()
	form := runnerTokenExchangeForm(signTestAssertion(h.t, h.key, h.clientID))
	payload := handshakeStep(h.t, h.srv.baseURL, "session token", "POST", "/_apis/v1/auth/", "",
		"application/x-www-form-urlencoded", form.Encode(), http.StatusOK)
	var issued struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(payload, &issued); err != nil {
		h.t.Fatalf("decode session token: %v", err)
	}
	if issued.AccessToken == "" {
		h.t.Fatal("token exchange returned no access_token")
	}
	h.session = issued.AccessToken
}

func (h *handshakeRunner) createSession() {
	h.t.Helper()
	const path = "/_apis/v1/AgentSession/1"
	body := fmt.Sprintf(`{"ownerName":%q,"agent":{"id":%d}}`, h.name, h.agentID)
	refuseAnonymous(h.t, h.srv.baseURL, "create session", "POST", path, "application/json", body)
	payload := handshakeStep(h.t, h.srv.baseURL, "create session", "POST", path,
		"Bearer "+h.session, "application/json", body, http.StatusOK)
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		h.t.Fatalf("decode session: %v", err)
	}
	if created.SessionID == "" {
		h.t.Fatal("session response carried no sessionId")
	}
	h.sessionID = created.SessionID
}

func (h *handshakeRunner) messagePath() string {
	return "/_apis/v1/Message/1?sessionId=" + url.QueryEscape(h.sessionID) + "&lastMessageId=0"
}

// pollForMessage takes the job waiting for this runner. It is only called
// where one has been queued: an empty queue makes the listener's long poll
// block for its full timeout, which is correct behaviour and a useless wait.
func (h *handshakeRunner) pollForMessage(messageType, bodyMarker string) []byte {
	h.t.Helper()
	payload := handshakeStep(h.t, h.srv.baseURL, "poll for messages", "GET", h.messagePath(),
		"Bearer "+h.session, "", "", http.StatusOK)
	var delivered TaskAgentMessage
	if err := json.Unmarshal(payload, &delivered); err != nil {
		h.t.Fatalf("decode polled message: %v; body=%s", err, payload)
	}
	if delivered.MessageType != messageType || !strings.Contains(delivered.Body, bodyMarker) {
		h.t.Fatalf("polled foreign message type=%q body=%q, want type=%q body containing %q",
			delivered.MessageType, delivered.Body, messageType, bodyMarker)
	}
	return payload
}

func (h *handshakeRunner) deleteSession() {
	h.t.Helper()
	path := fmt.Sprintf("/_apis/v1/AgentSession/1/%s", url.PathEscape(h.sessionID))
	refuseAnonymous(h.t, h.srv.baseURL, "delete session", "DELETE", path, "", "")
	handshakeStep(h.t, h.srv.baseURL, "delete session", "DELETE", path, "Bearer "+h.session, "", "", http.StatusOK)
}

// configure runs the whole `config.sh` sequence and leaves the runner holding
// a session bearer, exactly as the runner does before `run.sh` starts.
func (h *handshakeRunner) configure(registrationTokenPath string) {
	h.t.Helper()
	h.exchangeTenantCredential(h.mintRegistrationToken(registrationTokenPath), "register")
	h.connectionData("")
	h.listPools()
	if existing := h.lookUpOwnName(); len(existing) > 0 {
		h.replaceAgent(existing[0].ID)
	} else {
		h.addAgent()
	}
	h.testConnection()
}

// TestRunnerConfigurationHandshakeSucceedsForARepositoryRunner is the
// regression this file exists for: a runner registering against a repository
// must complete configuration and open a listening session.
func TestRunnerConfigurationHandshakeSucceedsForARepositoryRunner(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	testRepo(t, s.Server, "admin", "handshake-repo", false)

	runner := s.newHandshakeRunner(t, "handshake-repo-runner", "/admin/handshake-repo")
	runner.configure("/api/v3/repos/admin/handshake-repo/actions/runners/registration-token")
	runner.createSession()
	refuseAnonymous(t, s.baseURL, "poll for messages", "GET", runner.messagePath(), "", "")
	runner.deleteSession()
}

// TestRunnerConfigurationHandshakeSucceedsForAnOrganizationRunner is the same
// sequence at the other scope the harness exercises. The credential carries an
// organization rather than a repository, and every gate has to read it.
func TestRunnerConfigurationHandshakeSucceedsForAnOrganizationRunner(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	store := s.store
	admin := store.LookupUserByLogin("admin")
	if admin == nil {
		t.Fatal("the seeded admin user is missing")
	}
	const orgLogin = "handshake-org"
	if store.GetOrg(orgLogin) == nil && store.CreateOrg(admin, orgLogin, "Handshake Org", "") == nil {
		t.Fatalf("could not create the fixture organization")
	}

	runner := s.newHandshakeRunner(t, "handshake-org-runner", "/"+orgLogin)
	runner.configure("/api/v3/orgs/" + orgLogin + "/actions/runners/registration-token")
	runner.createSession()
	refuseAnonymous(t, s.baseURL, "poll for messages", "GET", runner.messagePath(), "", "")
	runner.deleteSession()

	// The clientId the runner stores is a bare GUID — it deserializes the
	// field into one and refuses anything else — so the scope is read from the
	// agent record it names.
	if _, err := uuid.Parse(runner.clientID); err != nil {
		t.Fatalf("clientId %q is not a GUID, which the runner will refuse: %v", runner.clientID, err)
	}
	agent := s.store.LookupAgentByClientID(runner.clientID)
	if agent == nil {
		t.Fatalf("no agent registered with clientId %q", runner.clientID)
	}
	if agent.Scope.Org != orgLogin {
		t.Fatalf("agent scope = %+v, want org %s", agent.Scope, orgLogin)
	}
}

// TestRunnerReconfigurationReplacesItsRegistration covers `config.sh
// --replace` against a pool that already carries the runner's name: the
// lookup finds the old registration, the replacement carries a new key pair,
// and the session exchange has to accept that new key.
func TestRunnerReconfigurationReplacesItsRegistration(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	testRepo(t, s.Server, "admin", "handshake-replace", false)
	const tokenPath = "/api/v3/repos/admin/handshake-replace/actions/runners/registration-token"

	first := s.newHandshakeRunner(t, "handshake-replace-runner", "/admin/handshake-replace")
	first.configure(tokenPath)

	second := s.newHandshakeRunner(t, "handshake-replace-runner", "/admin/handshake-replace")
	second.exchangeTenantCredential(second.mintRegistrationToken(tokenPath), "register")
	existing := second.lookUpOwnName()
	if len(existing) != 1 {
		t.Fatalf("pool lookup found %d registrations for %q, want the one just configured", len(existing), second.name)
	}
	if existing[0].ID != first.agentID {
		t.Fatalf("pool lookup found agent %d, want %d", existing[0].ID, first.agentID)
	}
	second.replaceAgent(existing[0].ID)
	if second.agentID != first.agentID {
		t.Fatalf("replacement created agent %d instead of rewriting %d", second.agentID, first.agentID)
	}
	// The session exchange is the assertion that matters: it verifies the
	// client_assertion against the key the replacement carried.
	second.exchangeSessionToken()
	second.createSession()
	second.deleteSession()
}

// TestRunnerRemovalHandshakeSucceeds covers `config.sh remove --token`: a
// removal token is exchanged for a tenant credential, which looks the runner
// up and deletes its registration.
func TestRunnerRemovalHandshakeSucceeds(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	testRepo(t, s.Server, "admin", "handshake-remove", false)

	runner := s.newHandshakeRunner(t, "handshake-remove-runner", "/admin/handshake-remove")
	runner.configure("/api/v3/repos/admin/handshake-remove/actions/runners/registration-token")

	removalToken := runner.mintRegistrationToken("/api/v3/repos/admin/handshake-remove/actions/runners/remove-token")
	runner.exchangeTenantCredential(removalToken, "remove")

	found := runner.lookUpOwnName()
	if len(found) != 1 || found[0].ID != runner.agentID {
		t.Fatalf("removal lookup found %d registrations, want agent %d", len(found), runner.agentID)
	}

	path := fmt.Sprintf("/_apis/v1/Agent/1/%d", runner.agentID)
	refuseAnonymous(t, s.baseURL, "delete agent", "DELETE", path, "", "")
	handshakeStep(t, s.baseURL, "delete agent", "DELETE", path, "Bearer "+runner.setupToken, "", "", http.StatusOK)

	if remaining := runner.lookUpOwnName(); len(remaining) != 0 {
		t.Fatalf("registration survived removal: %d agents still named %q", len(remaining), runner.name)
	}
}

// TestRunnerSetupCredentialsDoNotCrossPurposes pins the separation the config
// routes depend on. A registration token registers; a removal token removes;
// neither does the other's job, and neither is a substitute for the session
// bearer a configured runner holds.
func TestRunnerSetupCredentialsDoNotCrossPurposes(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	testRepo(t, s.Server, "admin", "handshake-purposes", false)
	base := "/api/v3/repos/admin/handshake-purposes/actions/runners/"

	runner := s.newHandshakeRunner(t, "handshake-purposes-runner", "/admin/handshake-purposes")
	registration := runner.mintRegistrationToken(base + "registration-token")
	removal := runner.mintRegistrationToken(base + "remove-token")

	// A removal token cannot be exchanged for a registration tenant credential,
	// and a registration token cannot be exchanged for a removal one.
	for _, tc := range []struct{ event, token, name string }{
		{"register", removal, "removal token registering"},
		{"remove", registration, "registration token removing"},
	} {
		body := fmt.Sprintf(`{"url":%q,"runner_event":%q}`, s.baseURL+runner.scopePath, tc.event)
		status, _, payload := handshakeCall(t, s.baseURL, "POST", "/api/v3/actions/runner-registration",
			"RemoteAuth "+tc.token, "application/json", body)
		if status != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401; body=%s", tc.name, status, payload)
		}
	}

	// An unrecognized runner_event names no operation, so it cannot select one.
	body := fmt.Sprintf(`{"url":%q,"runner_event":"sideways"}`, s.baseURL+runner.scopePath)
	if status, _, payload := handshakeCall(t, s.baseURL, "POST", "/api/v3/actions/runner-registration",
		"RemoteAuth "+registration, "application/json", body); status != http.StatusBadRequest {
		t.Errorf("unknown runner_event: status = %d, want 400; body=%s", status, payload)
	}

	// Configure for real, then check the config-time routes against the wrong
	// purpose. The tenant token here is a registration credential.
	runner.exchangeTenantCredential(registration, "register")
	runner.listPools()
	runner.addAgent()

	removalTenant := s.newHandshakeRunner(t, runner.name, runner.scopePath)
	removalTenant.exchangeTenantCredential(removal, "remove")

	// A removal credential may look a runner up — `config.sh remove` does —
	// but it may not mint a registration or rewrite one, and a registration
	// credential may not delete one. A setup token offered for the wrong
	// purpose is not a credential the route knows, so it is refused exactly
	// like none at all.
	if status, _, payload := handshakeCall(t, s.baseURL, "POST", "/_apis/v1/Agent/1",
		"Bearer "+removalTenant.setupToken, "application/json", runner.agentBody()); status != http.StatusUnauthorized {
		t.Errorf("removal token adding an agent: status = %d, want 401; body=%s", status, payload)
	}
	replacePath := fmt.Sprintf("/_apis/v1/Agent/1/%d", runner.agentID)
	if status, _, payload := handshakeCall(t, s.baseURL, "PUT", replacePath,
		"Bearer "+removalTenant.setupToken, "application/json", runner.agentBody()); status != http.StatusUnauthorized {
		t.Errorf("removal token replacing an agent: status = %d, want 401; body=%s", status, payload)
	}
	if status, _, payload := handshakeCall(t, s.baseURL, "DELETE", replacePath,
		"Bearer "+runner.setupToken, "", ""); status != http.StatusUnauthorized {
		t.Errorf("registration token deleting an agent: status = %d, want 401; body=%s", status, payload)
	}
	// The pool listing is not a fleet-wide view: a credential for another
	// repository sees nothing of this one's runners.
	outsider := s.newHandshakeRunner(t, runner.name, "/admin/handshake-purposes-other")
	testRepo(t, s.Server, "admin", "handshake-purposes-other", false)
	outsider.exchangeTenantCredential(
		outsider.mintRegistrationToken("/api/v3/repos/admin/handshake-purposes-other/actions/runners/registration-token"),
		"register")
	if seen := outsider.lookUpOwnName(); len(seen) != 0 {
		t.Errorf("a credential for another repository saw %d of this repository's runners", len(seen))
	}
}

// TestRunnerJobTokenIsNotAConfigurationCredential keeps the widening honest in
// the other direction: the per-job runtime token a worker holds is a verified
// runner credential, but it is not a runner registering itself.
func TestRunnerJobTokenIsNotAConfigurationCredential(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	testRepo(t, s.Server, "admin", "handshake-jobtoken", false)
	jobToken, _ := testJobToken(t, s.Server, "admin/handshake-jobtoken")

	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/_apis/v1/AgentPools", ""},
		{"GET", "/_apis/v1/Agent/1", ""},
		{"PUT", "/_apis/v1/Agent/1/1", "{}"},
		{"DELETE", "/_apis/v1/Agent/1/1", ""},
	} {
		contentType := ""
		if tc.body != "" {
			contentType = "application/json"
		}
		status, _, payload := handshakeCall(t, s.baseURL, tc.method, tc.path, "Bearer "+jobToken, contentType, tc.body)
		if status != http.StatusForbidden {
			t.Errorf("job token on %s %s: status = %d, want 403; body=%s", tc.method, tc.path, status, payload)
		}
	}
}

// TestEphemeralRunnerTeardownStaysAuthenticated walks an ephemeral runner
// through the whole of its life: configure, take one job, report it finished,
// close the job request and delete the session. Deregistration happens on the
// last of those, so every call before it must still authenticate — a
// registration removed any earlier takes the runner's credential with it.
func TestEphemeralRunnerTeardownStaysAuthenticated(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	testRepo(t, s.Server, "admin", "handshake-ephemeral", false)

	runner := s.newHandshakeRunner(t, "handshake-ephemeral-runner", "/admin/handshake-ephemeral")
	runner.exchangeTenantCredential(
		runner.mintRegistrationToken("/api/v3/repos/admin/handshake-ephemeral/actions/runners/registration-token"),
		"register")
	runner.listPools()
	if existing := runner.lookUpOwnName(); len(existing) > 0 {
		t.Fatalf("the pool already carries %q", runner.name)
	}

	// The ephemeral flag has to survive registration: config.sh --ephemeral
	// aborts when the response drops it.
	body := fmt.Sprintf(
		`{"name":%q,"version":"2.330.0","ephemeral":true,`+
			`"labels":[{"name":"self-hosted","type":"custom"},{"name":%q,"type":"custom"}],`+
			`"authorization":{"publicKey":%s}}`,
		runner.name, runner.name, runner.publicKeyJSON())
	payload := handshakeStep(t, s.baseURL, "add ephemeral agent", "POST", "/_apis/v1/Agent/1",
		"Bearer "+runner.setupToken, "application/json", body, http.StatusOK)
	var registered Agent
	if err := json.Unmarshal(payload, &registered); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	if !registered.Ephemeral {
		t.Fatal("registration response dropped the ephemeral flag")
	}
	runner.readAgent("add ephemeral agent", payload)
	runner.exchangeSessionToken()
	runner.createSession()

	// Queue one job for it, the way the workflow engine does.
	planID := fmt.Sprintf("handshake-ephemeral-plan-%d", runner.agentID)
	jobID := "handshake-ephemeral-job-" + planID
	requestID := s.nextRequestID()
	message := fmt.Sprintf(
		`{"plan":{"scopeIdentifier":%q,"planId":%q},"requestId":%d,`+
			`"contextData":{"github":{"t":2,"d":[{"k":"repository","v":"admin/handshake-ephemeral"}]}}}`,
		planID, planID, requestID)
	s.store.Mu.Lock()
	s.store.Jobs[jobID] = &Job{
		ID: jobID, RequestID: requestID, PlanID: planID, Status: "queued", Message: message,
		LockedUntil: fixedTestTime.Add(time.Hour),
	}
	s.store.Mu.Unlock()
	queued := &TaskAgentMessage{
		MessageID:   s.nextMessageID(),
		MessageType: "PipelineAgentJobRequest",
		Body:        message,
		JobID:       jobID,
		Labels:      []string{"self-hosted", runner.name},
	}
	// This suite intentionally shares one server. Put this test's explicitly
	// targeted message first so an unrelated test's generic self-hosted job
	// cannot be claimed and mistaken for this one after shuffled execution.
	s.store.Mu.Lock()
	s.store.PendingMessages = append([]*TaskAgentMessage{queued}, s.store.PendingMessages...)
	s.store.Mu.Unlock()

	runner.pollForMessage("PipelineAgentJobRequest", planID)

	// The listener renews the job request while the worker runs.
	requestPath := fmt.Sprintf("/_apis/v1/AgentRequest/1/%d", requestID)
	refuseAnonymous(t, s.baseURL, "renew job request", "PATCH", requestPath, "application/json", "{}")
	handshakeStep(t, s.baseURL, "renew job request", "PATCH", requestPath,
		"Bearer "+runner.session, "application/json", "{}", http.StatusOK)

	// The worker reports completion on the job's runtime token.
	jobRuntimeToken := makeJWT(planID, runnerAudJob)
	finishPath := fmt.Sprintf("/_apis/v1/FinishJob/%s/build/%s", planID, planID)
	finishBody := fmt.Sprintf(`{"name":"JobCompleted","jobId":%q,"result":0}`, jobID)
	refuseAnonymous(t, s.baseURL, "finish job", "POST", finishPath, "application/json", finishBody)
	handshakeStep(t, s.baseURL, "finish job", "POST", finishPath,
		"Bearer "+jobRuntimeToken, "application/json", finishBody, http.StatusOK)

	// Then the listener closes the request and the session — both on the agent
	// session bearer, so the registration must still exist for both.
	handshakeStep(t, s.baseURL, "complete job request", "DELETE", requestPath+"?result=succeeded",
		"Bearer "+runner.session, "", "", http.StatusOK)
	runner.deleteSession()

	s.store.Mu.RLock()
	_, stillRegistered := s.store.Agents[runner.agentID]
	s.store.Mu.RUnlock()
	if stillRegistered {
		t.Fatalf("ephemeral agent %d survived its teardown", runner.agentID)
	}

	// And with the registration gone, its session bearer is dead.
	if status, _, payload := handshakeCall(t, s.baseURL, "GET", "/_apis/v1/AgentPools",
		"Bearer "+runner.session, "", ""); status != http.StatusUnauthorized {
		t.Errorf("deregistered runner's session token: status = %d, want 401; body=%s", status, payload)
	}
}

// actionArchiveAuthorization is the header the runner's action downloader
// builds out of the token an ActionDownloadInfo response named. It is spelled
// out here rather than taken from the server so the test pins the wire shape
// the runner actually sends: basic auth, the token as the password, under a
// fixed user name.
func actionArchiveAuthorization(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// TestRunnerActionDownloadCarriesTheCredentialItIsGiven walks the half of a job
// the worker performs: resolve an action reference to an archive, then fetch
// that archive.
//
// The fetch is the one runner protocol call not made by the runner's service
// stack. A plain HTTP client makes it, holding no credential of its own and no
// ability to negotiate one — it sends the token the download info named, as
// basic auth, or no header at all when the response named none. So the
// contract has two halves and neither is optional: the response has to carry a
// usable token, and the route has to accept it in that shape.
func TestRunnerActionDownloadCarriesTheCredentialItIsGiven(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	const actionRepo = "handshake-actions/hello-action"
	commitFilesToStorage(t, s.Server, actionRepo, map[string]string{
		"action.yml": "name: hello\nruns:\n  using: composite\n  steps:\n    - run: echo hi\n      shell: bash\n",
	})
	jobToken, scopeID := testJobToken(t, s.Server, actionRepo)

	infoPath := fmt.Sprintf("/_apis/v1/ActionDownloadInfo/%s/build/plan-%s", scopeID, scopeID)
	infoBody := fmt.Sprintf(`{"actions":[{"nameWithOwner":%q,"ref":"main"}]}`, actionRepo)
	refuseAnonymous(t, s.baseURL, "resolve action download", "POST", infoPath, "application/json", infoBody)
	payload := handshakeStep(t, s.baseURL, "resolve action download", "POST", infoPath,
		"Bearer "+jobToken, "application/json", infoBody, http.StatusOK)

	var resolved struct {
		Actions map[string]struct {
			TarballURL     string `json:"tarballUrl"`
			ResolvedSha    string `json:"resolvedSha"`
			Authentication struct {
				Token     string `json:"token"`
				ExpiresAt string `json:"expiresAt"`
			} `json:"authentication"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(payload, &resolved); err != nil {
		t.Fatalf("decode action download info: %v", err)
	}
	info, ok := resolved.Actions[actionRepo+"@main"]
	if !ok {
		t.Fatalf("action download info carried no entry for %s@main: %s", actionRepo, payload)
	}
	if info.Authentication.Token == "" {
		t.Fatalf("action download info named no token; the runner would fetch the archive with no credential at all: %s", payload)
	}
	expiry, err := time.Parse(time.RFC3339, info.Authentication.ExpiresAt)
	if err != nil {
		t.Fatalf("action download expiresAt %q is not a timestamp: %v", info.Authentication.ExpiresAt, err)
	}
	if !expiry.After(fixedTestTime) || expiry.After(fixedTestTime.Add(48*time.Hour)) {
		t.Errorf("action download expiresAt = %s, want the real near-term expiry of the token it describes", expiry)
	}

	archivePath, found := strings.CutPrefix(info.TarballURL, s.baseURL)
	if !found {
		t.Fatalf("tarball url %q does not address this server", info.TarballURL)
	}

	// Presented the way the runner presents it, the archive is served.
	refuseAnonymous(t, s.baseURL, "download action archive", "GET", archivePath, "", "")
	handshakeStep(t, s.baseURL, "download action archive", "GET", archivePath,
		actionArchiveAuthorization(info.Authentication.Token), "", "", http.StatusOK)

	// And the token in it is verified, not merely present: a basic credential
	// carrying anything else is refused exactly like none at all.
	for name, authorization := range map[string]string{
		"a forged token":       actionArchiveAuthorization("not-a-runner-token"),
		"an agent session":     actionArchiveAuthorization(makeJWT(uuid.New().String(), runnerAudSession)),
		"another user's token": "Basic " + base64.StdEncoding.EncodeToString([]byte("someone-else:"+info.Authentication.Token)),
	} {
		status, _, body := handshakeCall(t, s.baseURL, "GET", archivePath, authorization, "", "")
		if status == http.StatusOK {
			t.Errorf("action archive served to %s: status = %d; body=%d bytes", name, status, len(body))
		}
	}
}

// TestRunnerHandshakeRoutesAreRegistered keeps the sequence above honest
// against the route table rather than against a remembered list of paths: a
// step exercising a path that no longer exists would otherwise pass by
// reaching the catch-all.
func TestRunnerHandshakeRoutesAreRegistered(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	registered := make(map[string]bool, len(s.routePatterns))
	for _, pattern := range s.routePatterns {
		registered[pattern] = true
	}
	for _, pattern := range []string{
		"POST /api/v3/repos/{owner}/{repo}/actions/runners/registration-token",
		"POST /api/v3/repos/{owner}/{repo}/actions/runners/remove-token",
		"POST /api/v3/orgs/{org}/actions/runners/registration-token",
		"POST /api/v3/actions/runner-registration",
		"GET /_apis/connectionData",
		"GET /_apis/v1/AgentPools",
		"GET /_apis/v1/Agent/{poolId}",
		"POST /_apis/v1/Agent/{poolId}",
		"PUT /_apis/v1/Agent/{poolId}/{agentId}",
		"DELETE /_apis/v1/Agent/{poolId}/{agentId}",
		"POST /_apis/v1/auth/",
		"POST /_apis/v1/AgentSession/{poolId}",
		"DELETE /_apis/v1/AgentSession/{poolId}/{sessionId}",
		"GET /_apis/v1/Message/{poolId}",
		"PATCH /_apis/v1/AgentRequest/{poolId}/{requestId}",
		"DELETE /_apis/v1/AgentRequest/{poolId}/{requestId}",
		"POST /_apis/v1/FinishJob/{scopeId}/{hubName}/{planId}",
		"POST /_apis/v1/ActionDownloadInfo/{scopeId}/{hubName}/{planId}",
		"GET /_apis/v1/actions/tarball/{owner}/{repo}/{ref...}",
	} {
		if !registered[pattern] {
			t.Errorf("the runner handshake exercises %q, which is not a registered route", pattern)
		}
	}
}

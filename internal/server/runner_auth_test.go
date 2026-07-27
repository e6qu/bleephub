package bleephub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- helpers ---

func testUser(t *testing.T, s *Server, login string) *User {
	t.Helper()
	if u := s.store.LookupUserByLogin(login); u != nil {
		return u
	}
	s.store.mu.Lock()
	user := &User{ID: s.store.NextUser, Login: login, Type: "User", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	s.store.NextUser++
	s.store.Users[user.ID] = user
	s.store.UsersByLogin[user.Login] = user
	s.store.mu.Unlock()
	return user
}

func testRepo(t *testing.T, s *Server, owner, name string, private bool) *Repo {
	t.Helper()
	user := testUser(t, s, owner)
	if repo := s.store.GetRepo(owner, name); repo != nil {
		return repo
	}
	repo := s.store.CreateRepo(user, name, "", private)
	if repo == nil {
		t.Fatalf("create repo %s/%s", owner, name)
	}
	return repo
}

// testJobToken seeds a dispatched job for repoFullName and returns the
// runtime token its message carries, along with the plan scope identifier.
func testJobToken(t *testing.T, s *Server, repoFullName string) (token, scopeID string) {
	t.Helper()
	scopeID = fmt.Sprintf("plan-scope-%s", strings.ReplaceAll(repoFullName, "/", "-"))
	message := fmt.Sprintf(
		`{"plan":{"scopeIdentifier":%q},"contextData":{"github":{"t":2,"d":[{"k":"repository","v":%q}]}}}`,
		scopeID, repoFullName)
	jobID := "job-" + scopeID
	s.store.mu.Lock()
	s.store.Jobs[jobID] = &Job{ID: jobID, PlanID: "plan-" + scopeID, Status: "queued", Message: message}
	s.store.mu.Unlock()
	return makeJWT(scopeID, runnerAudJob), scopeID
}

// seedRunJobToken registers a workflow run for repoFullName under backendID
// and returns the runtime token a job of that run carries. Artifact calls are
// scoped to their run, so a test that exercises one needs both.
func seedRunJobToken(t *testing.T, s *Server, repoFullName, backendID string) string {
	t.Helper()
	owner, name, ok := strings.Cut(repoFullName, "/")
	if !ok {
		t.Fatalf("expected owner/repo, got %q", repoFullName)
	}
	testRepo(t, s, owner, name, false)
	s.store.mu.Lock()
	s.store.Workflows[backendID] = &Workflow{ID: backendID, RepoFullName: repoFullName}
	s.store.mu.Unlock()
	token, _ := testJobToken(t, s, repoFullName)
	return token
}

// seedPlanScopeToken registers a dispatched job whose plan scope is planID
// and returns the runtime token a runner holds while writing that plan's
// timeline, logs and console output.
func seedPlanScopeToken(t *testing.T, s *Server, planID, repoFullName string) string {
	t.Helper()
	message := fmt.Sprintf(
		`{"plan":{"scopeIdentifier":%q,"planId":%q},"contextData":{"github":{"t":2,"d":[{"k":"repository","v":%q}]}}}`,
		planID, planID, repoFullName)
	s.store.mu.Lock()
	// The plan's job may already be installed (linkJobToPlan does it). The
	// token is minted against that job, not a second one claiming the same
	// plan — the routes resolve a plan to exactly one job.
	var job *Job
	for _, existing := range s.store.Jobs {
		if existing.PlanID == planID {
			job = existing
			break
		}
	}
	if job == nil {
		job = &Job{ID: "plan-scope-job-" + planID, PlanID: planID, Status: "running"}
		s.store.Jobs[job.ID] = job
	}
	job.Message = message
	s.store.mu.Unlock()
	return makeJWT(planID, runnerAudJob)
}

// testAgentSession registers an agent scoped to scope and returns its session
// bearer token and the stored agent.
func testAgentSession(t *testing.T, s *Server, scope runnerScope, labels ...string) (string, *Agent) {
	t.Helper()
	clientID, err := newAgentClientID(scope)
	if err != nil {
		t.Fatalf("newAgentClientID: %v", err)
	}
	agent := &Agent{Enabled: true, Status: "online", CreatedOn: time.Now(),
		Scope:         scope,
		Authorization: &AgentAuthorization{ClientID: clientID}}
	for _, l := range labels {
		agent.Labels = append(agent.Labels, Label{Name: l, Type: "custom"})
	}
	s.store.mu.Lock()
	agent.ID = s.store.NextAgent
	s.store.NextAgent++
	s.store.Agents[agent.ID] = agent
	s.store.mu.Unlock()
	return makeJWT(clientID, runnerAudSession), agent
}

// mustRunnerRegistrationToken mints the credential config.sh presents.
func mustRunnerRegistrationToken(t *testing.T, scope runnerScope) string {
	t.Helper()
	token, err := newRunnerRegistrationToken(scope, runnerPurposeRegistration)
	if err != nil {
		t.Fatalf("newRunnerRegistrationToken: %v", err)
	}
	return token
}

// runnerDo issues a runner protocol request against a live test server with
// the given bearer credential.
func runnerDo(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func runnerRequest(s *Server, method, path, token string, body string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	return w
}

// --- forged and tampered runner tokens ---

func TestRunnerTokenRejectsForgedAlgNone(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	testRepo(t, s, "octo", "private-repo", true)
	_, scopeID := testJobToken(t, s, "octo/private-repo")

	header := base64url([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64url([]byte(fmt.Sprintf(`{"sub":%q,"aud":"actions","exp":%d}`, scopeID, time.Now().Add(time.Hour).Unix())))
	forged := header + "." + payload + "."

	if _, err := parseRunnerToken(forged); err == nil {
		t.Fatal("parseRunnerToken accepted an alg:none token")
	}

	// The production path: the credential resolver every runner route goes
	// through must refuse to name a repository for a forged token.
	req := httptest.NewRequest("GET", "/_apis/artifactcache/cache?keys=k&version=v", nil)
	req.Header.Set("Authorization", "Bearer "+forged)
	if principal, err := s.authenticateRunner(req); err == nil {
		t.Fatalf("authenticateRunner resolved scope %s from a forged alg:none token", principal.Scope)
	}
	if w := runnerRequest(s, "GET", "/_apis/artifactcache/cache?keys=k&version=v", forged, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("cache lookup with a forged token = %d, want 401; body=%s", w.Code, w.Body.String())
	}

	// The same claims signed by the server do resolve, so the rejection is
	// the signature check and not a broken lookup.
	genuine := makeJWT(scopeID, runnerAudJob)
	req.Header.Set("Authorization", "Bearer "+genuine)
	principal, err := s.authenticateRunner(req)
	if err != nil {
		t.Fatalf("authenticateRunner on a genuine token: %v", err)
	}
	if principal.Scope.Repo != "octo/private-repo" {
		t.Fatalf("scope = %s, want repo octo/private-repo", principal.Scope)
	}
	if w := runnerRequest(s, "GET", "/_apis/artifactcache/cache?keys=k&version=v", genuine, ""); w.Code != http.StatusNoContent {
		t.Fatalf("cache lookup with a genuine token = %d, want 204 (miss); body=%s", w.Code, w.Body.String())
	}
}

func TestRunnerTokenRejectsTamperedPayload(t *testing.T) {
	token := makeJWT("scope-a", runnerAudJob)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	tampered := parts[0] + "." + base64url([]byte(fmt.Sprintf(`{"sub":"scope-b","aud":"actions","exp":%d}`, time.Now().Add(time.Hour).Unix()))) + "." + parts[2]
	if _, err := parseRunnerToken(tampered); err == nil {
		t.Fatal("parseRunnerToken accepted a token whose payload was swapped")
	}
}

func TestRunnerTokenLifetimeIsBoundedToAJob(t *testing.T) {
	claims, err := parseRunnerToken(makeJWT("scope-a", runnerAudJob))
	if err != nil {
		t.Fatalf("parseRunnerToken: %v", err)
	}
	lifetime := time.Until(time.Unix(claims.Exp, 0))
	if lifetime > 24*time.Hour {
		t.Fatalf("runtime token lifetime = %s, want no more than a day", lifetime)
	}
}

// --- the runner protocol is authenticated ---

func TestRunnerProtocolRejectsUnauthenticatedCalls(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/_apis/v1/AgentPools", ""},
		{"GET", "/_apis/v1/Agent/1", ""},
		{"GET", "/_apis/v1/Agent/1/1", ""},
		{"PUT", "/_apis/v1/Agent/1/1", "{}"},
		{"DELETE", "/_apis/v1/Agent/1/1", ""},
		{"POST", "/_apis/v1/Agent/1", "{}"},
		{"POST", "/_apis/v1/AgentSession/1", "{}"},
		{"GET", "/_apis/v1/Message/1?sessionId=x", ""},
		{"DELETE", "/_apis/v1/Message/1/1", ""},
		{"PATCH", "/_apis/v1/Timeline/scope/build/plan/timeline", "{}"},
		{"POST", "/_apis/v1/Logfiles/scope/build/plan", "{}"},
		{"POST", "/_apis/v1/Logfiles/scope/build/plan/1", "log line"},
		{"POST", "/_apis/v1/TimeLineWebConsoleLog/scope/build/plan/timeline/record", "[]"},
		{"PUT", "/_apis/v1/artifacts/1/upload", "data"},
		{"POST", "/_apis/artifactcache/caches", `{"key":"k","version":"v"}`},
		{"GET", "/_apis/artifactcache/cache?keys=k&version=v", ""},
		{"PATCH", "/_apis/artifactcache/caches/1", "x"},
		{"POST", "/_apis/artifactcache/caches/1", "{}"},
		{"GET", "/_apis/v1/actions/tarball/octo/action/v1", ""},
		{"POST", "/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact", "{}"},
		{"POST", "/twirp/github.actions.results.api.v1.ArtifactService/ListArtifacts", "{}"},
		{"POST", "/twirp/github.actions.results.api.v1.ArtifactService/FinalizeArtifact", "{}"},
		{"POST", "/twirp/github.actions.results.api.v1.ArtifactService/GetSignedArtifactURL", "{}"},
	} {
		w := runnerRequest(s, tc.method, tc.path, "", tc.body)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401; body=%s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}

func TestRunnerProtocolAllowlistStaysOpen(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	// Service discovery carries no tenant state and is read before the runner
	// holds any credential.
	if w := runnerRequest(s, "GET", "/_apis/connectionData", "", ""); w.Code != http.StatusOK {
		t.Fatalf("connectionData status = %d, want 200", w.Code)
	}
	// The token exchange authenticates on the RSA client_assertion it carries;
	// a request without one is rejected on its own terms, not with a 401 for a
	// bearer token it has yet to be issued.
	w := runnerRequest(s, "POST", "/_apis/v1/auth/", "", "grant_type=bogus")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("auth exchange status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestAgentRegistrationRequiresRegistrationToken(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	testRepo(t, s, "octo", "repo", true)

	body := `{"name":"runner-1","version":"2.300.0","labels":[{"name":"self-hosted","type":"custom"}]}`

	if w := runnerRequest(s, "POST", "/_apis/v1/Agent/1", "", body); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated registration status = %d, want 401", w.Code)
	}
	if w := runnerRequest(s, "POST", "/_apis/v1/Agent/1", "Anot-a-real-token.AAAA", body); w.Code != http.StatusUnauthorized {
		t.Fatalf("forged registration status = %d, want 401", w.Code)
	}

	// A removal token is not a registration token.
	removal, err := newRunnerRegistrationToken(runnerScope{Repo: "octo/repo"}, runnerPurposeRemoval)
	if err != nil {
		t.Fatalf("newRunnerRegistrationToken: %v", err)
	}
	if w := runnerRequest(s, "POST", "/_apis/v1/Agent/1", removal, body); w.Code != http.StatusUnauthorized {
		t.Fatalf("removal-token registration status = %d, want 401", w.Code)
	}

	registration, err := newRunnerRegistrationToken(runnerScope{Repo: "octo/repo"}, runnerPurposeRegistration)
	if err != nil {
		t.Fatalf("newRunnerRegistrationToken: %v", err)
	}
	req := httptest.NewRequest("POST", "/_apis/v1/Agent/1", strings.NewReader(body))
	req.SetPathValue("poolId", "1")
	req.Header.Set("Authorization", "Bearer "+registration)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("registration status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var agent Agent
	if err := json.Unmarshal(w.Body.Bytes(), &agent); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	if agent.Authorization == nil || agent.Authorization.ClientID == "" {
		t.Fatal("registered agent has no clientId")
	}
	// The clientId is a bare GUID: the runner deserializes it into one and
	// rejects anything else at configuration time. The scope it used to carry
	// lives on the agent record instead.
	if _, err := uuid.Parse(agent.Authorization.ClientID); err != nil {
		t.Fatalf("clientId %q is not a GUID, which the runner will refuse: %v",
			agent.Authorization.ClientID, err)
	}
	stored := s.store.LookupAgentByClientID(agent.Authorization.ClientID)
	if stored == nil {
		t.Fatal("the registered agent cannot be found by its clientId")
	}
	if stored.Scope.Repo != "octo/repo" {
		t.Fatalf("agent scope = %+v, want repo octo/repo", stored.Scope)
	}
}

func TestAgentSessionTokenCannotReachAnotherRunnersSession(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	tokenA, agentA := testAgentSession(t, s, runnerScope{Repo: "octo/a"})
	tokenB, _ := testAgentSession(t, s, runnerScope{Repo: "octo/b"})

	session := &Session{SessionID: "session-a", Agent: agentA, MsgCh: make(chan *TaskAgentMessage, 1)}
	s.store.mu.Lock()
	s.store.Sessions[session.SessionID] = session
	s.store.mu.Unlock()

	if w := runnerRequest(s, "GET", "/_apis/v1/Message/1?sessionId=session-a", tokenB, ""); w.Code != http.StatusForbidden {
		t.Fatalf("cross-runner poll status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if w := runnerRequest(s, "DELETE", "/_apis/v1/AgentSession/1/session-a", tokenB, ""); w.Code == http.StatusOK {
		s.store.mu.RLock()
		_, still := s.store.Sessions["session-a"]
		s.store.mu.RUnlock()
		if !still {
			t.Fatal("another runner's session token deleted the session")
		}
	}
	// The owner still reaches its own session.
	if w := runnerRequest(s, "GET", "/_apis/v1/Message/1?sessionId=session-a", tokenA, ""); w.Code != http.StatusOK {
		t.Fatalf("owner poll status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// --- job secrets only reach an entitled runner ---

func TestBrokerWithholdsJobMessagesOutsideTheRunnerScope(t *testing.T) {
	s := newTestServer()
	message := `{"plan":{"scopeIdentifier":"scope-1"},"contextData":{"github":{"t":2,"d":[{"k":"repository","v":"octo/secret-repo"}]}}}`
	msg := &TaskAgentMessage{MessageID: 1, MessageType: "PipelineAgentJobRequest", Body: message, JobID: "job-1"}
	s.store.mu.Lock()
	s.store.Jobs["job-1"] = &Job{ID: "job-1", Status: "queued", Message: message}
	s.store.PendingMessages = append(s.store.PendingMessages, msg)
	s.store.mu.Unlock()

	_, outsider := testAgentSession(t, s, runnerScope{Repo: "octo/other-repo"})
	outsiderSession := &Session{SessionID: "s-out", Agent: outsider, MsgCh: make(chan *TaskAgentMessage, 1)}
	if got := s.pullPendingMessage(outsiderSession, runnerScope{Repo: "octo/other-repo"}); got != nil {
		t.Fatal("a runner registered for another repository received the job message")
	}

	_, owner := testAgentSession(t, s, runnerScope{Org: "octo"})
	ownerSession := &Session{SessionID: "s-own", Agent: owner, MsgCh: make(chan *TaskAgentMessage, 1)}
	if got := s.pullPendingMessage(ownerSession, runnerScope{Org: "octo"}); got == nil {
		t.Fatal("an org-scoped runner did not receive its organization's job message")
	}
}

func TestJobSecretsEntitlement(t *testing.T) {
	for _, tc := range []struct {
		scope runnerScope
		repo  string
		want  bool
	}{
		{runnerScope{Repo: "octo/a"}, "octo/a", true},
		{runnerScope{Repo: "octo/a"}, "octo/b", false},
		{runnerScope{Org: "octo"}, "octo/b", true},
		{runnerScope{Org: "octo"}, "other/b", false},
		{runnerScope{}, "octo/a", false},
		{runnerScope{Repo: "octo/a"}, "", true},
	} {
		if got := jobSecretsEntitled(tc.scope, tc.repo); got != tc.want {
			t.Errorf("jobSecretsEntitled(%+v, %q) = %v, want %v", tc.scope, tc.repo, got, tc.want)
		}
	}
}

// --- artifacts are repository-scoped ---

func TestArtifactDownloadRejectsCrossRepository(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	testRepo(t, s, "octo", "owner-repo", true)
	testRepo(t, s, "octo", "other-repo", true)

	s.artifactStore.mu.Lock()
	s.artifactStore.artifacts[1] = &Artifact{
		ID: 1, Name: "build", Data: []byte("secret bytes"), Size: 12,
		Finalized: true, RepoFullName: "octo/owner-repo", CreatedAt: time.Now(),
	}
	s.artifactStore.mu.Unlock()

	if w := runnerRequest(s, "GET", "/_apis/v1/artifacts/1/download", "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("anonymous download of a private repo artifact = %d, want 404", w.Code)
	}

	otherToken, _ := testJobToken(t, s, "octo/other-repo")
	if w := runnerRequest(s, "GET", "/_apis/v1/artifacts/1/download", otherToken, ""); w.Code != http.StatusNotFound {
		t.Fatalf("cross-repository download = %d, want 404; body=%s", w.Code, w.Body.String())
	}

	ownToken, _ := testJobToken(t, s, "octo/owner-repo")
	w := runnerRequest(s, "GET", "/_apis/v1/artifacts/1/download", ownToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("owning-repository download = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != "secret bytes" {
		t.Fatalf("body = %q", w.Body.String())
	}
}

func TestArtifactUploadRejectsCrossRepository(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	testRepo(t, s, "octo", "owner-repo", true)
	testRepo(t, s, "octo", "other-repo", true)

	s.artifactStore.mu.Lock()
	s.artifactStore.artifacts[1] = &Artifact{ID: 1, Name: "build", RepoFullName: "octo/owner-repo", CreatedAt: time.Now()}
	s.artifactStore.mu.Unlock()

	otherToken, _ := testJobToken(t, s, "octo/other-repo")
	if w := runnerRequest(s, "PUT", "/_apis/v1/artifacts/1/upload", otherToken, "overwrite"); w.Code != http.StatusNotFound {
		t.Fatalf("cross-repository upload = %d, want 404; body=%s", w.Code, w.Body.String())
	}

	s.artifactStore.mu.RLock()
	stored := string(s.artifactStore.artifacts[1].Data)
	s.artifactStore.mu.RUnlock()
	if stored != "" {
		t.Fatalf("artifact data = %q, want untouched", stored)
	}
}

func TestListArtifactsRequiresARunID(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	testRepo(t, s, "octo", "repo", false)
	token, _ := testJobToken(t, s, "octo/repo")

	s.artifactStore.mu.Lock()
	s.artifactStore.artifacts[1] = &Artifact{ID: 1, Name: "a", Finalized: true, RepoFullName: "octo/repo", WorkflowRunBackendID: "run-1"}
	s.artifactStore.artifacts[2] = &Artifact{ID: 2, Name: "b", Finalized: true, RepoFullName: "other/repo", WorkflowRunBackendID: "run-2"}
	s.artifactStore.mu.Unlock()

	w := runnerRequest(s, "POST", "/twirp/github.actions.results.api.v1.ArtifactService/ListArtifacts", token, "{}")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("list without a run id = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"name"`) {
		t.Fatalf("list without a run id returned artifacts: %s", w.Body.String())
	}
}

// --- action tarballs are not a private-repository export ---

func TestActionTarballRejectsPrivateRepositoryOutsideScope(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	testRepo(t, s, "octo", "private-action", true)
	testRepo(t, s, "octo", "job-repo", false)

	if w := runnerRequest(s, "GET", "/_apis/v1/actions/tarball/octo/private-action/main", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated tarball = %d, want 401", w.Code)
	}

	token, _ := testJobToken(t, s, "other/unrelated")
	if w := runnerRequest(s, "GET", "/_apis/v1/actions/tarball/octo/private-action/main", token, ""); w.Code != http.StatusNotFound {
		t.Fatalf("out-of-scope private action tarball = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// --- a small body cannot make the server allocate a terabyte ---

func TestCacheUploadContentRangeDoesNotAllocateBeyondTheBody(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	testRepo(t, s, "octo", "repo", false)
	token, _ := testJobToken(t, s, "octo/repo")

	reserve := runnerRequest(s, "POST", "/_apis/artifactcache/caches", token, `{"key":"k","version":"v"}`)
	if reserve.Code != http.StatusOK {
		t.Fatalf("reserve = %d, body=%s", reserve.Code, reserve.Body.String())
	}
	var reserved struct {
		CacheID int64 `json:"cacheId"`
	}
	if err := json.Unmarshal(reserve.Body.Bytes(), &reserved); err != nil {
		t.Fatalf("decode reservation: %v", err)
	}

	const farEnd = int64(1) << 40 // 1 TiB
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	req := httptest.NewRequest("PATCH", fmt.Sprintf("/_apis/artifactcache/caches/%d", reserved.CacheID), bytes.NewBufferString("x"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", farEnd, farEnd))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	runtime.ReadMemStats(&after)
	if w.Code == http.StatusOK {
		t.Fatalf("a 1-byte body at offset %d was accepted", farEnd)
	}
	if grew := int64(after.TotalAlloc - before.TotalAlloc); grew > 64<<20 {
		t.Fatalf("handling the request allocated %d bytes", grew)
	}
}

func TestCacheUploadAssemblesChunksAndRejectsGaps(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	testRepo(t, s, "octo", "repo", false)
	token, _ := testJobToken(t, s, "octo/repo")

	reserve := runnerRequest(s, "POST", "/_apis/artifactcache/caches", token, `{"key":"k","version":"v"}`)
	var reserved struct {
		CacheID int64 `json:"cacheId"`
	}
	if err := json.Unmarshal(reserve.Body.Bytes(), &reserved); err != nil {
		t.Fatalf("decode reservation: %v", err)
	}
	path := fmt.Sprintf("/_apis/artifactcache/caches/%d", reserved.CacheID)

	// Chunks arriving out of order still assemble.
	upload := func(body string, start, end int64) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PATCH", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", start, end))
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		return w
	}
	if w := upload("world", 5, 9); w.Code != http.StatusOK {
		t.Fatalf("second chunk first = %d, body=%s", w.Code, w.Body.String())
	}
	if w := upload("hello", 0, 4); w.Code != http.StatusOK {
		t.Fatalf("first chunk second = %d, body=%s", w.Code, w.Body.String())
	}
	if w := runnerRequest(s, "POST", path, token, `{"size":10}`); w.Code != http.StatusOK {
		t.Fatalf("finalize = %d, body=%s", w.Code, w.Body.String())
	}

	s.artifactStore.mu.RLock()
	got := string(s.artifactStore.caches[reserved.CacheID].Data)
	s.artifactStore.mu.RUnlock()
	if got != "helloworld" {
		t.Fatalf("assembled cache = %q, want helloworld", got)
	}
}

// --- the filter-pattern cache under concurrency ---

func TestFilterPatternCacheConcurrentCompilation(t *testing.T) {
	patterns := make([]string, 64)
	for i := range patterns {
		patterns[i] = fmt.Sprintf("src/**/pkg-%d/*.go", i)
	}

	var wg sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for round := 0; round < len(patterns); round++ {
				i := (worker + round) % len(patterns)
				value := fmt.Sprintf("src/x/pkg-%d/main.go", i)
				if !filterPatternMatch(patterns[i], value) {
					t.Errorf("pattern %q did not match %q", patterns[i], value)
				}
			}
		}(worker)
	}
	wg.Wait()
}

// --- a dropped connection must not lose a queued job ---

// failingResponseWriter fails the body write, standing in for a runner that
// hung up between the status line and the payload.
type failingResponseWriter struct {
	header http.Header
	code   int
}

func (f *failingResponseWriter) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}
func (f *failingResponseWriter) WriteHeader(code int)      { f.code = code }
func (f *failingResponseWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("connection reset") }

func TestPendingMessageSurvivesAFailedDelivery(t *testing.T) {
	s := newTestServer()
	token, agent := testAgentSession(t, s, runnerScope{Repo: "octo/repo"})
	claims, err := parseRunnerToken(token)
	if err != nil {
		t.Fatalf("parseRunnerToken: %v", err)
	}
	principal := &runnerPrincipal{Claims: claims, Agent: agent, Scope: runnerScope{Repo: "octo/repo"}}

	session := &Session{SessionID: "s1", Agent: agent, MsgCh: make(chan *TaskAgentMessage, 1)}
	message := `{"plan":{"scopeIdentifier":"scope-1"},"contextData":{"github":{"t":2,"d":[{"k":"repository","v":"octo/repo"}]}}}`
	msg := &TaskAgentMessage{MessageID: 9, MessageType: "PipelineAgentJobRequest", Body: message, JobID: "job-9"}
	s.store.mu.Lock()
	s.store.Sessions["s1"] = session
	s.store.Jobs["job-9"] = &Job{ID: "job-9", Status: "queued", Message: message}
	s.store.PendingMessages = append(s.store.PendingMessages, msg)
	s.store.mu.Unlock()

	req := httptest.NewRequest("GET", "/_apis/v1/Message/1?sessionId=s1", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxRunner, principal))
	s.handleGetMessage(&failingResponseWriter{}, req)

	s.store.mu.RLock()
	queued := len(s.store.PendingMessages)
	assigned := s.store.Jobs["job-9"].AgentID
	s.store.mu.RUnlock()
	if queued != 1 {
		t.Fatalf("pending messages = %d, want the undelivered job still queued", queued)
	}
	if assigned != 0 {
		t.Fatalf("job agent = %d, want released after a failed delivery", assigned)
	}

	// A successful poll then takes it exactly once.
	w := httptest.NewRecorder()
	s.handleGetMessage(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "PipelineAgentJobRequest") {
		t.Fatalf("redelivery status = %d, body = %s", w.Code, w.Body.String())
	}
	s.store.mu.RLock()
	queued = len(s.store.PendingMessages)
	s.store.mu.RUnlock()
	if queued != 0 {
		t.Fatalf("pending messages after delivery = %d, want 0", queued)
	}
}

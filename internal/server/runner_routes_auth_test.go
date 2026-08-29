package bleephub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// seedDispatchedJob registers a job the broker has already handed to agentID,
// exactly as pullPendingMessage leaves it, and returns it with the plan scope
// its runtime token is minted for. An empty repoFullName is the
// operator-submitted shape (/internal/exec/submit): a plan naming no repository.
func seedDispatchedJob(t *testing.T, s *Server, repoFullName string, agentID int, requestID int64) (*store.Job, string) {
	t.Helper()
	if repoFullName != "" {
		owner, name, ok := strings.Cut(repoFullName, "/")
		if !ok {
			t.Fatalf("expected owner/repo, got %q", repoFullName)
		}
		testRepo(t, s, owner, name, false)
	}

	scopeID := fmt.Sprintf("scope-%s-%d", strings.ReplaceAll(repoFullName, "/", "-"), requestID)
	planID := "plan-" + scopeID
	message := fmt.Sprintf(
		`{"plan":{"scopeIdentifier":%q,"planId":%q},"contextData":{"github":{"t":2,"d":[{"k":"repository","v":%q}]}}}`,
		scopeID, planID, repoFullName)
	job := &store.Job{
		ID:        "job-" + scopeID,
		RequestID: requestID,
		PlanID:    planID,
		Status:    "queued",
		Message:   message,
		AgentID:   agentID,
	}
	s.store.Mu.Lock()
	s.store.Jobs[job.ID] = job
	s.store.Mu.Unlock()
	return job, scopeID
}

func brokerJobStatus(s *Server, jobID string) string {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	if job := s.store.Jobs[jobID]; job != nil {
		return job.Status
	}
	return ""
}

// nothing on the job control plane answers without a credential

func TestJobControlRoutesRejectUnauthenticatedCalls(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	_, scopeID := seedDispatchedJob(t, s, "octo/repo", 1, 41)

	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/_apis/v1/AgentRequest/1/41", ""},
		{"PATCH", "/_apis/v1/AgentRequest/1/41", ""},
		{"PUT", "/_apis/v1/AgentRequest/1/41", ""},
		{"DELETE", "/_apis/v1/AgentRequest/1/41", ""},
		{"POST", "/_apis/v1/FinishJob/" + scopeID + "/free/plan-" + scopeID, `{"name":"JobCompleted","result":"succeeded"}`},
		{"PUT", "/_apis/v1/plans/plan-" + scopeID + "/events", `{"name":"JobCompleted","result":"succeeded"}`},
		{"POST", "/_apis/v1/ActionDownloadInfo/" + scopeID + "/free/plan-" + scopeID, `{"actions":[]}`},
		{"GET", "/_apis/v1/tasks/00000000-0000-0000-0000-000000000000/1.0.0", ""},
		{"POST", "/_apis/v1/tasks", "{}"},
	} {
		w := runnerRequest(s, tc.method, tc.path, "", tc.body)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401; body=%s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	if got := brokerJobStatus(s, "job-"+scopeID); got != "queued" {
		t.Fatalf("an unauthenticated sweep moved the job to %q", got)
	}
}

// a job request belongs to the runner it was dispatched to

func TestAgentRequestRefusesAnotherRunnersJob(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	tokenA, _ := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	_, agentB := testAgentSession(t, s, store.RunnerScope{Repo: "octo/b"})
	job, _ := seedDispatchedJob(t, s, "octo/b", agentB.ID, 77)

	for _, method := range []string{"GET", "PATCH", "PUT", "DELETE"} {
		w := runnerRequest(s, method, "/_apis/v1/AgentRequest/1/77", tokenA, "")
		if w.Code != http.StatusForbidden {
			t.Errorf("%s AgentRequest as another repository's runner = %d, want 403; body=%s", method, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "scopeIdentifier") {
			t.Errorf("%s AgentRequest leaked the job message to another runner: %s", method, w.Body.String())
		}
	}
	if got := brokerJobStatus(s, job.ID); got != "queued" {
		t.Fatalf("another runner moved the job to %q", got)
	}
}

func TestAgentRequestRefusesAnUnassignedRunnerAndAJobToken(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	// Same repository scope, but the broker handed the job to a different
	// agent: scope alone is not ownership.
	_, assigned := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	sameScope, _ := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	job, scopeID := seedDispatchedJob(t, s, "octo/a", assigned.ID, 78)

	if w := runnerRequest(s, "DELETE", "/_apis/v1/AgentRequest/1/78", sameScope, ""); w.Code != http.StatusForbidden {
		t.Fatalf("unassigned same-scope runner = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	// The job's own runtime token is not an agent session: the listener owns
	// the request, the worker owns the plan.
	if w := runnerRequest(s, "DELETE", "/_apis/v1/AgentRequest/1/78", makeJWT(scopeID, runnerAudJob), ""); w.Code != http.StatusForbidden {
		t.Fatalf("job runtime token on AgentRequest = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if got := brokerJobStatus(s, job.ID); got != "queued" {
		t.Fatalf("a refused caller moved the job to %q", got)
	}
}

// TestAgentRequestServesTheAssignedRunner is the over-blocking control: the
// runner the broker dispatched to must still acquire, renew and complete.
func TestAgentRequestServesTheAssignedRunner(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	token, agent := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	job, scopeID := seedDispatchedJob(t, s, "octo/a", agent.ID, 79)

	w := runnerRequest(s, "GET", "/_apis/v1/AgentRequest/1/79", token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("assigned runner acquire = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), scopeID) {
		t.Fatalf("acquire did not return the job message: %s", w.Body.String())
	}
	for _, method := range []string{"PATCH", "PUT"} {
		if w := runnerRequest(s, method, "/_apis/v1/AgentRequest/1/79", token, ""); w.Code != http.StatusOK {
			t.Fatalf("assigned runner %s = %d, want 200; body=%s", method, w.Code, w.Body.String())
		}
	}
	if got := brokerJobStatus(s, job.ID); got != "running" {
		t.Fatalf("renew left the job %q, want running", got)
	}
	if w := runnerRequest(s, "DELETE", "/_apis/v1/AgentRequest/1/79?result=succeeded", token, ""); w.Code != http.StatusOK {
		t.Fatalf("assigned runner complete = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := brokerJobStatus(s, job.ID); got != "completed" {
		t.Fatalf("complete left the job %q, want completed", got)
	}
}

// plan-addressed routes belong to the job whose plan they name

func TestPlanRoutesRefuseAnotherJobsRuntimeToken(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	_, agentA := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	_, agentB := testAgentSession(t, s, store.RunnerScope{Repo: "octo/b"})
	_, scopeA := seedDispatchedJob(t, s, "octo/a", agentA.ID, 81)
	victim, scopeB := seedDispatchedJob(t, s, "octo/b", agentB.ID, 82)
	tokenA := makeJWT(scopeA, runnerAudJob)
	completed := `{"name":"JobCompleted","result":"succeeded"}`

	// A runner holding repository A's job token, naming its own scope in the
	// path but repository B's plan.
	for _, tc := range []struct{ method, path string }{
		{"POST", "/_apis/v1/FinishJob/" + scopeA + "/free/" + victim.PlanID},
		{"PUT", "/_apis/v1/plans/" + victim.PlanID + "/events"},
	} {
		if w := runnerRequest(s, tc.method, tc.path, tokenA, completed); w.Code != http.StatusForbidden {
			t.Errorf("%s %s with another job's token = %d, want 403; body=%s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	if got := brokerJobStatus(s, victim.ID); got != "queued" {
		t.Fatalf("another job's runtime token moved the victim to %q", got)
	}

	// Naming its own scope AND its own plan works, so the refusal above is the
	// plan binding and not a broken lookup.
	tokenB := makeJWT(scopeB, runnerAudJob)
	if w := runnerRequest(s, "POST", "/_apis/v1/FinishJob/"+scopeB+"/free/"+victim.PlanID, tokenB, completed); w.Code != http.StatusOK {
		t.Fatalf("owning job token on FinishJob = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := brokerJobStatus(s, victim.ID); got != "completed" {
		t.Fatalf("FinishJob by the owning job left it %q, want completed", got)
	}
}

func TestPlanEventsServeTheOwningJob(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	_, agent := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	job, scopeID := seedDispatchedJob(t, s, "octo/a", agent.ID, 83)

	w := runnerRequest(s, "PUT", "/_apis/v1/plans/"+job.PlanID+"/events",
		makeJWT(scopeID, runnerAudJob), `{"name":"JobCompleted","result":"succeeded"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("owning job token on plan events = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := brokerJobStatus(s, job.ID); got != "completed" {
		t.Fatalf("plan event left the job %q, want completed", got)
	}
}

// TestFinishJobRefusesABodySuppliedJobID guards the completion path itself:
// the plan names the job, and a jobId in the body may only confirm it.
func TestFinishJobRefusesABodySuppliedJobID(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	_, agentA := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	_, agentB := testAgentSession(t, s, store.RunnerScope{Repo: "octo/b"})
	own, scopeA := seedDispatchedJob(t, s, "octo/a", agentA.ID, 84)
	victim, _ := seedDispatchedJob(t, s, "octo/b", agentB.ID, 85)

	body := fmt.Sprintf(`{"name":"JobCompleted","jobId":%q,"result":"succeeded"}`, victim.ID)
	w := runnerRequest(s, "POST", "/_apis/v1/FinishJob/"+scopeA+"/free/"+own.PlanID, makeJWT(scopeA, runnerAudJob), body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("FinishJob naming another job in the body = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if got := brokerJobStatus(s, victim.ID); got != "queued" {
		t.Fatalf("a body-supplied jobId moved another job to %q", got)
	}
}

// action metadata is plan-scoped

func TestActionDownloadInfoRequiresThePlansJobToken(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	_, agentA := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	_, agentB := testAgentSession(t, s, store.RunnerScope{Repo: "octo/b"})
	own, scopeA := seedDispatchedJob(t, s, "octo/a", agentA.ID, 86)
	other, scopeB := seedDispatchedJob(t, s, "octo/b", agentB.ID, 87)

	if w := runnerRequest(s, "POST", "/_apis/v1/ActionDownloadInfo/"+scopeB+"/free/"+other.PlanID,
		makeJWT(scopeA, runnerAudJob), `{"actions":[]}`); w.Code != http.StatusForbidden {
		t.Fatalf("ActionDownloadInfo under another plan's scope = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	// An agent session token is not a job runtime token here either.
	sessionToken, _ := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	if w := runnerRequest(s, "POST", "/_apis/v1/ActionDownloadInfo/"+scopeA+"/free/"+own.PlanID,
		sessionToken, `{"actions":[]}`); w.Code != http.StatusForbidden {
		t.Fatalf("ActionDownloadInfo with an agent session token = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	w := runnerRequest(s, "POST", "/_apis/v1/ActionDownloadInfo/"+scopeA+"/free/"+own.PlanID,
		makeJWT(scopeA, runnerAudJob), `{"actions":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("ActionDownloadInfo under its own plan = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// task metadata needs a runner credential of either audience

func TestTaskRoutesAcceptEitherRunnerAudience(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	sessionToken, agent := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	_, scopeID := seedDispatchedJob(t, s, "octo/a", agent.ID, 88)
	jobToken := makeJWT(scopeID, runnerAudJob)

	for _, token := range []string{sessionToken, jobToken} {
		if w := runnerRequest(s, "GET", "/_apis/v1/tasks/00000000-0000-0000-0000-000000000000/1.0.0", token, ""); w.Code != http.StatusNotFound {
			t.Errorf("task definition = %d, want the handler's own 404; body=%s", w.Code, w.Body.String())
		}
		if w := runnerRequest(s, "POST", "/_apis/v1/tasks", token, `{"area":"runner"}`); w.Code != http.StatusOK {
			t.Errorf("telemetry = %d, want 200; body=%s", w.Code, w.Body.String())
		}
	}
}

// the timeline is written by the job whose plan it is

func TestTimelineRoutesRefuseAnotherJobsPlan(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	_, agentA := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	_, agentB := testAgentSession(t, s, store.RunnerScope{Repo: "octo/b"})
	own, scopeA := seedDispatchedJob(t, s, "octo/a", agentA.ID, 91)
	victim, _ := seedDispatchedJob(t, s, "octo/b", agentB.ID, 92)
	tokenA := makeJWT(scopeA, runnerAudJob)

	records := `{"count":1,"value":[{"id":"rec-1","name":"forged step","state":"completed","result":"succeeded"}]}`
	console := `{"count":1,"value":["forged console output"]}`

	// Repository A's job token, naming its own scope but repository B's plan.
	for _, tc := range []struct{ method, path, body string }{
		{"PATCH", "/_apis/v1/Timeline/" + scopeA + "/build/" + victim.PlanID + "/timeline-1", records},
		{"POST", "/_apis/v1/TimeLineWebConsoleLog/" + scopeA + "/build/" + victim.PlanID + "/timeline-1/rec-1", console},
		{"POST", "/_apis/v1/Logfiles/" + scopeA + "/build/" + victim.PlanID, ""},
	} {
		if w := runnerRequest(s, tc.method, tc.path, tokenA, tc.body); w.Code != http.StatusForbidden {
			t.Errorf("%s %s with another job's token = %d, want 403; body=%s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}

	s.store.Mu.RLock()
	forgedRecords := len(s.store.TimelineRecords[victim.PlanID])
	forgedConsole := len(s.store.LogLines[victim.ID])
	s.store.Mu.RUnlock()
	if forgedRecords != 0 {
		t.Fatalf("another job's token wrote %d timeline records into the victim's plan", forgedRecords)
	}
	if forgedConsole != 0 {
		t.Fatalf("another job's token wrote %d console lines into the victim's job", forgedConsole)
	}

	// The owning job still writes its own plan.
	if w := runnerRequest(s, "PATCH", "/_apis/v1/Timeline/"+scopeA+"/build/"+own.PlanID+"/timeline-1", tokenA, records); w.Code != http.StatusOK {
		t.Fatalf("owning job PATCH timeline = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w := runnerRequest(s, "POST", "/_apis/v1/TimeLineWebConsoleLog/"+scopeA+"/build/"+own.PlanID+"/timeline-1/rec-1", tokenA, console); w.Code != http.StatusOK {
		t.Fatalf("owning job console log = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	s.store.Mu.RLock()
	ownRecords := len(s.store.TimelineRecords[own.PlanID])
	ownConsole := len(s.store.LogLines[own.ID])
	s.store.Mu.RUnlock()
	if ownRecords != 1 || ownConsole != 1 {
		t.Fatalf("owning job wrote %d records / %d console lines, want 1 / 1", ownRecords, ownConsole)
	}
}

// TestLogUploadRefusesAnotherPlansContainer covers the same class one path
// parameter over: the plan gate passes because the caller names its own plan,
// but the {logId} it uploads to was reserved by a different one.
func TestLogUploadRefusesAnotherPlansContainer(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	_, agentA := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	_, agentB := testAgentSession(t, s, store.RunnerScope{Repo: "octo/b"})
	own, scopeA := seedDispatchedJob(t, s, "octo/a", agentA.ID, 93)
	victim, scopeB := seedDispatchedJob(t, s, "octo/b", agentB.ID, 94)
	tokenA, tokenB := makeJWT(scopeA, runnerAudJob), makeJWT(scopeB, runnerAudJob)

	newLog := func(token, scopeID, planID string) int {
		t.Helper()
		w := runnerRequest(s, "POST", "/_apis/v1/Logfiles/"+scopeID+"/build/"+planID, token, "")
		if w.Code != http.StatusOK {
			t.Fatalf("create log = %d; body=%s", w.Code, w.Body.String())
		}
		var created struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode created log: %v", err)
		}
		return created.ID
	}

	victimLog := newLog(tokenB, scopeB, victim.PlanID)
	ownLog := newLog(tokenA, scopeA, own.PlanID)

	w := runnerRequest(s, "POST", fmt.Sprintf("/_apis/v1/Logfiles/%s/build/%s/%d", scopeA, own.PlanID, victimLog), tokenA, "forged log content")
	if w.Code != http.StatusNotFound {
		t.Fatalf("upload into another plan's log container = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	s.store.Mu.RLock()
	forged := len(s.store.LogFiles[victimLog])
	s.store.Mu.RUnlock()
	if forged != 0 {
		t.Fatalf("another plan's job wrote %d bytes into log %d", forged, victimLog)
	}

	if w := runnerRequest(s, "POST", fmt.Sprintf("/_apis/v1/Logfiles/%s/build/%s/%d", scopeA, own.PlanID, ownLog), tokenA, "real log content"); w.Code != http.StatusOK {
		t.Fatalf("upload into its own log container = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	s.store.Mu.RLock()
	stored := string(s.store.LogFiles[ownLog])
	s.store.Mu.RUnlock()
	if stored != "real log content" {
		t.Fatalf("own log content = %q", stored)
	}
}

// an operator-submitted job reaches its own plan and nothing else

func TestOperatorSubmittedJobTokenReportsItsOwnPlanOnly(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	_, agent := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	operatorJob, operatorScope := seedDispatchedJob(t, s, "", agent.ID, 95)
	repoJob, _ := seedDispatchedJob(t, s, "octo/a", agent.ID, 96)
	token := makeJWT(operatorScope, runnerAudJob)

	// It reports its own plan: timeline, console and completion.
	if w := runnerRequest(s, "PATCH", "/_apis/v1/Timeline/"+operatorScope+"/build/"+operatorJob.PlanID+"/timeline-1",
		token, `{"count":1,"value":[{"id":"rec-1","name":"step","state":"completed"}]}`); w.Code != http.StatusOK {
		t.Fatalf("operator job PATCH timeline = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w := runnerRequest(s, "POST", "/_apis/v1/FinishJob/"+operatorScope+"/free/"+operatorJob.PlanID,
		token, `{"name":"JobCompleted","result":"succeeded"}`); w.Code != http.StatusOK {
		t.Fatalf("operator job FinishJob = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := brokerJobStatus(s, operatorJob.ID); got != "completed" {
		t.Fatalf("operator job status = %q, want completed", got)
	}

	// It is not a wildcard. Another plan is refused...
	if w := runnerRequest(s, "POST", "/_apis/v1/FinishJob/"+operatorScope+"/free/"+repoJob.PlanID,
		token, `{"name":"JobCompleted","result":"succeeded"}`); w.Code != http.StatusForbidden {
		t.Fatalf("operator job token on another plan = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if got := brokerJobStatus(s, repoJob.ID); got != "queued" {
		t.Fatalf("operator job token moved a repository job to %q", got)
	}
	// ...and so is every repository-scoped surface, because the empty scope
	// covers no repository.
	if w := runnerRequest(s, "POST", "/_apis/artifactcache/caches", token, `{"key":"k","version":"v"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("operator job token reserving a cache = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	s.artifactStore.Mu.Lock()
	s.artifactStore.Artifacts[1] = &store.Artifact{ID: 1, Name: "build", Data: []byte("secret"), Size: 6, Finalized: true, RepoFullName: "octo/a"}
	s.artifactStore.Mu.Unlock()
	if w := runnerRequest(s, "GET", "/_apis/v1/artifacts/1/download", token, ""); w.Code != http.StatusNotFound {
		t.Fatalf("operator job token downloading a repository artifact = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// an ephemeral runner survives its own teardown

func TestEphemeralRunnerFinishesItsOwnTeardown(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	sessionToken, agent := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	s.store.Mu.Lock()
	agent.Ephemeral = true
	s.store.Mu.Unlock()
	job, scopeID := seedDispatchedJob(t, s, "octo/a", agent.ID, 97)

	session := &store.Session{SessionID: "eph-session", Agent: agent, MsgCh: make(chan *store.TaskAgentMessage, 1)}
	s.store.Mu.Lock()
	s.store.Sessions[session.SessionID] = session
	s.store.Mu.Unlock()

	// The worker reports completion first, on the job runtime token.
	if w := runnerRequest(s, "POST", "/_apis/v1/FinishJob/"+scopeID+"/free/"+job.PlanID,
		makeJWT(scopeID, runnerAudJob), `{"name":"JobCompleted","result":"succeeded"}`); w.Code != http.StatusOK {
		t.Fatalf("worker FinishJob = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// The listener then closes the job request and its session, both on the
	// agent session token. Deregistering the runner at FinishJob made these
	// unauthenticatable.
	if w := runnerRequest(s, "DELETE", "/_apis/v1/AgentRequest/1/97?result=succeeded", sessionToken, ""); w.Code != http.StatusOK {
		t.Fatalf("listener complete after FinishJob = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w := runnerRequest(s, "DELETE", "/_apis/v1/AgentSession/1/eph-session", sessionToken, ""); w.Code != http.StatusOK {
		t.Fatalf("listener session delete = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	s.store.Mu.RLock()
	_, stillRegistered := s.store.Agents[agent.ID]
	s.store.Mu.RUnlock()
	if stillRegistered {
		t.Fatal("ephemeral runner is still registered after its session went away")
	}
}

func TestEphemeralRunnerTakesExactlyOneJob(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	queue := func(jobID, repo string) {
		message := fmt.Sprintf(`{"plan":{"scopeIdentifier":%q},"contextData":{"github":{"t":2,"d":[{"k":"repository","v":%q}]}}}`, "scope-"+jobID, repo)
		s.store.Mu.Lock()
		s.store.Jobs[jobID] = &store.Job{ID: jobID, Status: "queued", Message: message}
		s.store.PendingMessages = append(s.store.PendingMessages,
			&store.TaskAgentMessage{MessageID: 1, MessageType: "PipelineAgentJobRequest", Body: message, JobID: jobID})
		s.store.Mu.Unlock()
	}

	// An ephemeral runner that has already finished one job is done. Mirror
	// the post-delivery bookkeeping recordJobAgentLocked writes: EverAssigned
	// is what disqualifies a used ephemeral runner, and it must keep doing so
	// even after the completed job's stub is garbage-collected.
	_, ephemeral := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	s.store.Mu.Lock()
	ephemeral.Ephemeral = true
	s.store.Jobs["done-eph"] = &store.Job{ID: "done-eph", Status: "completed", AgentID: ephemeral.ID}
	ephemeral.AssignedJobID = "done-eph"
	ephemeral.EverAssigned = true
	s.store.Mu.Unlock()
	queue("next-eph", "octo/a")
	ephSession := &store.Session{SessionID: "s-eph", Agent: ephemeral, MsgCh: make(chan *store.TaskAgentMessage, 1)}
	if msg := s.actions.PullPendingMessage(ephSession, store.RunnerScope{Repo: "octo/a"}); msg != nil {
		t.Fatal("an ephemeral runner was handed a second job")
	}

	// A resident runner that finished one takes the next.
	_, resident := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	s.store.Mu.Lock()
	s.store.Jobs["done-res"] = &store.Job{ID: "done-res", Status: "completed", AgentID: resident.ID}
	s.store.Mu.Unlock()
	resSession := &store.Session{SessionID: "s-res", Agent: resident, MsgCh: make(chan *store.TaskAgentMessage, 1)}
	if msg := s.actions.PullPendingMessage(resSession, store.RunnerScope{Repo: "octo/a"}); msg == nil {
		t.Fatal("a resident runner that finished a job was not handed the next one")
	}
}

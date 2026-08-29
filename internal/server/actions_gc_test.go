package bleephub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/actions"
	"github.com/e6qu/bleephub/internal/store"
)

// ACT-044: job-state GC + hot-path indexes

func gcQueueTestJob(t *testing.T, s *Server, jobID, repo string) *store.Job {
	t.Helper()
	scopeID := "scope-" + jobID
	message := fmt.Sprintf(
		`{"plan":{"scopeIdentifier":%q},"contextData":{"github":{"t":2,"d":[{"k":"repository","v":%q}]}}}`,
		scopeID, repo)
	job := &store.Job{ID: jobID, PlanID: "plan-" + jobID, Status: "queued", Message: message}
	var msg map[string]interface{}
	if err := json.Unmarshal([]byte(message), &msg); err != nil {
		t.Fatalf("unmarshal seeded job message: %v", err)
	}
	s.store.Mu.Lock()
	s.store.Jobs[jobID] = job
	s.store.RegisterDispatchedJobLocked(job, msg, repo)
	s.store.Mu.Unlock()
	s.actions.QueueJobMessage(&store.TaskAgentMessage{MessageID: 1, MessageType: "PipelineAgentJobRequest", Body: message, JobID: jobID})
	return job
}

// TestUsedEphemeralAgentStaysDisqualifiedAfterJobGC pins the security property
// the broker comment documents: a used EPHEMERAL agent used to be disqualified
// from a second job by its completed job lingering in store.Jobs forever. Now
// that the janitor deletes completed job stubs, the disqualification must
// survive on the agent itself (EverAssigned) — and a resident runner must NOT
// be over-penalized by the same sweep.
func TestUsedEphemeralAgentStaysDisqualifiedAfterJobGC(t *testing.T) {
	s := newTestServer()

	_, ephemeral := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	s.store.Mu.Lock()
	ephemeral.Ephemeral = true
	s.store.Mu.Unlock()
	session := &store.Session{SessionID: "gc-eph", Agent: ephemeral, MsgCh: make(chan *store.TaskAgentMessage, 1)}

	job := gcQueueTestJob(t, s, "gc-eph-job", "octo/a")
	if msg := s.actions.PullPendingMessage(session, store.RunnerScope{Repo: "octo/a"}); msg == nil {
		t.Fatal("fresh ephemeral runner was not handed its first job")
	}
	if !ephemeral.EverAssigned {
		t.Fatal("delivery did not mark the agent as ever-assigned")
	}

	// The job completes and, runnerTokenTTL later, the janitor sweeps its stub.
	s.store.Mu.Lock()
	s.store.MarkJobCompletedLocked(job)
	s.store.Mu.Unlock()
	if swept := s.actions.SweepRetiredActionsJobs(fixedTestTime.Add(runnerTokenTTL + time.Minute)); swept != 1 {
		t.Fatalf("sweep removed %d jobs, want 1", swept)
	}
	s.store.Mu.RLock()
	_, stubRemains := s.store.Jobs[job.ID]
	s.store.Mu.RUnlock()
	if stubRemains {
		t.Fatal("swept job stub is still in store.Jobs")
	}

	// With the stub gone, the flag alone must keep the used ephemeral runner
	// away from the next job.
	gcQueueTestJob(t, s, "gc-eph-second", "octo/a")
	if msg := s.actions.PullPendingMessage(session, store.RunnerScope{Repo: "octo/a"}); msg != nil {
		t.Fatal("used ephemeral runner was handed a second job after its completed job was garbage-collected")
	}

	// Control: a resident runner completes a job, the stub is swept, and the
	// runner still takes the next one.
	_, resident := testAgentSession(t, s, store.RunnerScope{Repo: "octo/a"})
	resSession := &store.Session{SessionID: "gc-res", Agent: resident, MsgCh: make(chan *store.TaskAgentMessage, 1)}
	if msg := s.actions.PullPendingMessage(resSession, store.RunnerScope{Repo: "octo/a"}); msg == nil {
		t.Fatal("resident runner did not take the queued job")
	}
	s.store.Mu.Lock()
	s.store.MarkJobCompletedLocked(s.store.Jobs["gc-eph-second"])
	s.store.Mu.Unlock()
	if swept := s.actions.SweepRetiredActionsJobs(fixedTestTime.Add(runnerTokenTTL + time.Minute)); swept != 1 {
		t.Fatalf("second sweep removed %d jobs, want 1", swept)
	}
	gcQueueTestJob(t, s, "gc-res-second", "octo/a")
	if msg := s.actions.PullPendingMessage(resSession, store.RunnerScope{Repo: "octo/a"}); msg == nil {
		t.Fatal("resident runner was refused its second job after GC of the first")
	}
}

// TestLateFinishJobAfterMessageGCAuthenticates proves the plan-scope record
// keeps job-token authentication working after run finalization has cleared
// the secret-bearing job message: the official runner's listener can report
// FinishJob (and flush logs) minutes after the run completed.
func TestLateFinishJobAfterMessageGCAuthenticates(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	testRepo(t, s, "octo", "gc-late", false)

	def, err := store.ParseWorkflow([]byte("name: late\njobs:\n  build:\n    runs-on: self-hosted\n    steps:\n      - run: echo hi\n"))
	if err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	wf, err := s.actions.SubmitWorkflow(context.Background(), "http://localhost", def, "",
		&actions.WorkflowEventMeta{EventName: "push", Repo: "octo/gc-late", Ref: "refs/heads/main"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	wfJob := wf.Jobs["build"]
	if wfJob == nil || wfJob.PlanID == "" {
		t.Fatalf("job was not dispatched: %+v", wfJob)
	}

	s.store.Mu.RLock()
	job := s.store.Jobs[wfJob.JobID]
	scopeID, _ := store.JobMessageScopeAndRepo(job.Message)
	s.store.Mu.RUnlock()
	if scopeID == "" {
		t.Fatal("dispatched job message carries no plan scope")
	}

	// The run finalizes (first completion report); the finalization GC must
	// clear the secret-bearing message.
	s.actions.OnJobCompleted(context.Background(), wfJob.JobID, "Succeeded")
	s.store.Mu.RLock()
	message := job.Message
	completedAt := job.CompletedAt
	s.store.Mu.RUnlock()
	if message != "" {
		t.Fatal("run finalization did not clear the job message")
	}
	if completedAt.IsZero() {
		t.Fatal("run finalization did not stamp the job's retirement time")
	}

	// The late duplicate FinishJob (the official runner reports completion
	// twice) must still authenticate — via planScopes, since the message that
	// used to carry the scope is gone.
	token := makeJWT(scopeID, runnerAudJob)
	w := runnerRequest(s, "POST", "/_apis/v1/FinishJob/"+scopeID+"/free/"+wfJob.PlanID,
		token, `{"name":"JobCompleted","result":"succeeded"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("late FinishJob after message GC = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// A wrong-plan token must still be refused.
	wrong := makeJWT("some-other-scope", runnerAudJob)
	if w := runnerRequest(s, "POST", "/_apis/v1/FinishJob/"+scopeID+"/free/"+wfJob.PlanID,
		wrong, `{"name":"JobCompleted","result":"succeeded"}`); w.Code == http.StatusOK {
		t.Fatal("a foreign job token was accepted for the plan after message GC")
	}
}

// TestActionsJanitorSweepsRetiredJobState verifies the sweep removes every
// piece of a retired job's replica-local state — stub, indexes, plan scope,
// log masks, console lines and in-memory log bytes — while leaving live jobs
// untouched.
func TestActionsJanitorSweepsRetiredJobState(t *testing.T) {
	s := newTestServer()

	retired := gcQueueTestJob(t, s, "sweep-retired", "octo/a")
	live := gcQueueTestJob(t, s, "sweep-live", "octo/a")

	s.store.Mu.Lock()
	s.store.MarkJobCompletedLocked(retired)
	s.store.LogMasks[retired.PlanID] = []string{"hunter2"}
	s.store.LogLines[retired.ID] = []string{"line"}
	s.store.LogFiles[77] = []byte("log bytes")
	s.store.Mu.Unlock()
	s.artifactStore.ClaimLog(77, retired.PlanID)

	if swept := s.actions.SweepRetiredActionsJobs(fixedTestTime.Add(runnerTokenTTL + time.Minute)); swept != 1 {
		t.Fatalf("sweep removed %d jobs, want 1 (live job must be kept)", swept)
	}

	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	if _, ok := s.store.Jobs[retired.ID]; ok {
		t.Error("retired job stub survived the sweep")
	}
	if _, ok := s.store.PlanScopes[retired.PlanID]; ok {
		t.Error("retired job's plan scope survived the sweep")
	}
	if _, ok := s.store.PlanIDByScope["scope-sweep-retired"]; ok {
		t.Error("retired job's scope index entry survived the sweep")
	}
	if _, ok := s.store.LogMasks[retired.PlanID]; ok {
		t.Error("retired job's log masks survived the sweep")
	}
	if _, ok := s.store.LogLines[retired.ID]; ok {
		t.Error("retired job's console lines survived the sweep")
	}
	if _, ok := s.store.LogFiles[77]; ok {
		t.Error("retired job's in-memory log bytes survived the sweep")
	}
	if s.store.JobsByPlanID[retired.PlanID] != nil {
		t.Error("retired job's plan-id index entry survived the sweep")
	}
	// The live job survives intact and still indexed.
	if s.store.Jobs[live.ID] == nil || s.store.JobsByPlanID[live.PlanID] != live {
		t.Error("live job state was damaged by the sweep")
	}
	if _, ok := s.store.PlanScopes[live.PlanID]; !ok {
		t.Error("live job's plan scope was removed by the sweep")
	}
}

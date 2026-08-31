package store

import "testing"

// TestConcurrencyGroupsAreRepoScoped pins that concurrency-group indexes are
// namespaced per repository: two repositories that evaluate the same group name
// (e.g. `ci-refs/heads/main`) never share a scheduling namespace, so one repo's
// run neither blocks nor cancels an unrelated repo's run. Before the fix the
// indexes were keyed by the bare group string, making groups a global namespace
// (a cross-tenant DoS via cancel-in-progress).
func TestConcurrencyGroupsAreRepoScoped(t *testing.T) {
	st := NewStore()

	group := "ci-refs/heads/main"
	wfA := &Workflow{ID: "a", RepoFullName: "octo/alpha", ConcurrencyGroup: group, Status: WorkflowStatusRunning}
	wfB := &Workflow{ID: "b", RepoFullName: "octo/beta", ConcurrencyGroup: group, Status: WorkflowStatusRunning}
	st.Mu.Lock()
	st.Workflows[wfA.ID] = wfA
	st.Workflows[wfB.ID] = wfB
	st.SyncWorkflowIndexesLocked(wfA)
	st.SyncWorkflowIndexesLocked(wfB)

	peersA := st.WorkflowConcurrencyPeersLocked("octo/alpha", group)
	peersB := st.WorkflowConcurrencyPeersLocked("octo/beta", group)
	st.Mu.Unlock()

	if len(peersA) != 1 || peersA[0].ID != "a" {
		t.Fatalf("repo alpha peers = %v, want only workflow a (group must be repo-scoped)", peersA)
	}
	if len(peersB) != 1 || peersB[0].ID != "b" {
		t.Fatalf("repo beta peers = %v, want only workflow b", peersB)
	}
}

// TestJobConcurrencyGroupsAreRepoScoped is the job-level twin: two jobs in
// different repositories sharing a job concurrency group must not contend.
func TestJobConcurrencyGroupsAreRepoScoped(t *testing.T) {
	st := NewStore()

	group := "deploy"
	jobA := &WorkflowJob{Key: "deploy", JobID: "ja", Status: JobStatusPending, ConcurrencyGroup: group}
	jobB := &WorkflowJob{Key: "deploy", JobID: "jb", Status: JobStatusPending, ConcurrencyGroup: group}
	wfA := &Workflow{ID: "a", RepoFullName: "octo/alpha", Status: WorkflowStatusRunning, Jobs: map[string]*WorkflowJob{"deploy": jobA}}
	wfB := &Workflow{ID: "b", RepoFullName: "octo/beta", Status: WorkflowStatusRunning, Jobs: map[string]*WorkflowJob{"deploy": jobB}}
	st.Mu.Lock()
	st.Workflows[wfA.ID] = wfA
	st.Workflows[wfB.ID] = wfB
	st.SyncWorkflowIndexesLocked(wfA)
	st.SyncWorkflowIndexesLocked(wfB)

	peersA := st.JobConcurrencyPeersLocked("octo/alpha", group)
	peersB := st.JobConcurrencyPeersLocked("octo/beta", group)
	st.Mu.Unlock()

	if len(peersA) != 1 || peersA[0].Job.JobID != "ja" {
		t.Fatalf("repo alpha job peers = %v, want only job ja", peersA)
	}
	if len(peersB) != 1 || peersB[0].Job.JobID != "jb" {
		t.Fatalf("repo beta job peers = %v, want only job jb", peersB)
	}
}

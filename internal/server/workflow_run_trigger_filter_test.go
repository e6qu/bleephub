package bleephub

import (
	"testing"
)

// TestWorkflowRunTriggerHonoursWorkflowsFilter drives the real event path: a
// listener declaring `on.workflow_run.workflows: [CI]` must start for a
// completed run of "CI" and stay out of every other workflow's completion.
func TestWorkflowRunTriggerHonoursWorkflowsFilter(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "wfrunfilter/listener-repo"
	s.cancelRepoRunsCleanup(t, repoKey)

	commitFilesToStorage(t, s.Server, repoKey, map[string]string{
		".github/workflows/listener.yml": `name: listener
on:
  workflow_run:
    workflows: ["CI"]
    types: [completed]
jobs:
  react:
    runs-on: ubuntu-latest
    steps:
      - run: echo react
`,
	})

	completed := func(sourceName string) {
		s.triggerWorkflowsForEvent(repoKey, "workflow_run", "completed", "refs/heads/main", map[string]interface{}{
			"workflow_run": map[string]interface{}{"name": sourceName, "conclusion": "success"},
		})
	}
	listenerRuns := func() int {
		s.store.Mu.RLock()
		defer s.store.Mu.RUnlock()
		count := 0
		for _, wf := range s.store.Workflows {
			if wf.RepoFullName == repoKey && wf.Name == "listener" {
				count++
			}
		}
		return count
	}

	completed("Release")
	if got := listenerRuns(); got != 0 {
		t.Fatalf("listener started %d run(s) for an unnamed source workflow, want 0", got)
	}

	completed("CI")
	if got := listenerRuns(); got != 1 {
		t.Fatalf("listener runs after a CI completion = %d, want 1", got)
	}

	// Its own completion must not restart it either — that is what the filter
	// prevents turning into an unbounded loop.
	completed("listener")
	if got := listenerRuns(); got != 1 {
		t.Fatalf("listener runs after its own completion = %d, want 1", got)
	}
}

package bleephub

import "testing"

// These are the STORE-044 regressions: store setters must clone caller-owned
// slices/maps rather than adopt them by reference, so a handler that reuses or
// mutates the argument after the call cannot corrupt stored state.

func TestSetCopilotSelectedReposClonesCallerSlice(t *testing.T) {
	s := newTestServer()

	repoIDs := []int{1, 2, 3}
	s.store.SetCopilotCodingAgentSelectedRepos("acme", repoIDs)

	// Caller mutates its slice after the call.
	repoIDs[0] = 999

	got := s.store.GetCopilotCodingAgentPermissions("acme")
	if len(got.SelectedRepositoryIDs) != 3 || got.SelectedRepositoryIDs[0] != 1 {
		t.Fatalf("store adopted the caller's slice by reference: %v", got.SelectedRepositoryIDs)
	}
}

func TestSetCopilotContentExclusionClonesCallerMap(t *testing.T) {
	s := newTestServer()

	rules := map[string][]interface{}{"*": {"path/one"}}
	s.store.SetCopilotContentExclusion("acme", rules)

	// Caller mutates both a nested slice and the map itself after the call.
	rules["*"][0] = "path/hacked"
	rules["injected"] = []interface{}{"leak"}

	got := s.store.GetCopilotContentExclusion("acme")
	if v := got["*"]; len(v) != 1 || v[0] != "path/one" {
		t.Fatalf("store adopted the caller's nested slice by reference: %v", got["*"])
	}
	if _, ok := got["injected"]; ok {
		t.Fatal("store adopted the caller's map by reference: injected key leaked in")
	}
}

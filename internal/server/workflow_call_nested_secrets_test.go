package bleephub

import (
	"sort"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// secretsContextNames reads the secret names a built job message exposes in its
// `secrets` expression context (contextData.secrets is a DictContextData).
func secretsContextNames(t *testing.T, msg map[string]interface{}) map[string]string {
	t.Helper()
	ctxData, ok := msg["contextData"].(map[string]interface{})
	if !ok {
		t.Fatalf("message has no contextData: %#v", msg)
	}
	dict, ok := ctxData["secrets"].(map[string]interface{})
	if !ok {
		t.Fatalf("secrets context = %#v, want dictionary", ctxData["secrets"])
	}
	entries, ok := dict["d"].([]map[string]interface{})
	if !ok {
		t.Fatalf("secrets dictionary payload = %#v", dict["d"])
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, _ := entry["k"].(string)
		value, _ := entry["v"].(string)
		out[key] = value
	}
	return out
}

// TestNestedWorkflowCallSecretsAreNotRewidened pins the reusable-workflow
// secret contract at the second level of nesting: a workflow reached through a
// caller that passed an explicit `secrets:` map may only ever see what that map
// granted. `secrets: inherit` inherits the CALLING workflow's secrets, and the
// calling workflow here holds exactly one.
func TestNestedWorkflowCallSecretsAreNotRewidened(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "nestsec/nest-repo"
	s.cancelRepoRunsCleanup(t, repoKey)

	commitFilesToStorage(t, s.Server, repoKey, map[string]string{
		".github/workflows/caller.yml": `name: caller
on: [push]
jobs:
  mid:
    uses: ./.github/workflows/mid.yml
    secrets:
      PASSED: ${{ secrets.ALPHA }}
`,
		".github/workflows/mid.yml": `name: mid
on:
  workflow_call:
    secrets:
      PASSED:
        required: true
jobs:
  leaf:
    uses: ./.github/workflows/leaf.yml
    secrets: inherit
`,
		".github/workflows/leaf.yml": `name: leaf
on:
  workflow_call:
jobs:
  run:
    runs-on: ubuntu-latest
    steps:
      - run: echo leaf
`,
	})

	s.store.Mu.Lock()
	s.store.RepoSecrets[repoKey] = map[string]*store.Secret{
		"ALPHA": {Name: "ALPHA", Value: "alpha-value"},
		"BETA":  {Name: "BETA", Value: "beta-value-must-not-leak"},
	}
	s.store.Mu.Unlock()

	s.triggerWorkflowsForEvent(repoKey, "push", "", "refs/heads/main", nil)

	s.store.Mu.RLock()
	var wf *store.Workflow
	for _, w := range s.store.Workflows {
		if w.RepoFullName == repoKey && w.Name == "caller" {
			wf = w
		}
	}
	s.store.Mu.RUnlock()
	if wf == nil {
		t.Fatal("caller workflow not triggered")
	}

	const leafKey = "mid/leaf/run"
	s.store.Mu.RLock()
	leaf := wf.Jobs[leafKey]
	s.store.Mu.RUnlock()
	if leaf == nil {
		t.Fatalf("missing job %q; have %v", leafKey, jobKeys(wf))
	}

	msg, err := s.actions.BuildJobMessageFromDef("http://localhost", wf, leaf, "p", "t", 1, "alpine:latest")
	if err != nil {
		t.Fatal(err)
	}
	got := secretsContextNames(t, msg)
	if _, leaked := got["BETA"]; leaked {
		t.Errorf("leaf job received BETA, which the caller never passed to mid: %v", secretNames(got))
	}
	if _, leaked := got["ALPHA"]; leaked {
		t.Errorf("leaf job received ALPHA under its original name; mid only holds it as PASSED: %v", secretNames(got))
	}
	if got["PASSED"] != "alpha-value" {
		t.Errorf("leaf PASSED = %q, want the value inherited from mid", got["PASSED"])
	}
}

func secretNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

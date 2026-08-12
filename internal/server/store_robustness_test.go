package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// An alert with no instances must not panic the store under the write lock.
func TestCreateCodeScanningAutofixWithoutInstances(t *testing.T) {
	st := store.NewStore()
	fix, created := st.CreateCodeScanningAutofix(&store.CodeScanningAlert{RepoKey: "a/b", Number: 1, RuleID: "rule"})
	if !created || fix == nil {
		t.Fatalf("expected an autofix, got created=%v fix=%v", created, fix)
	}
}

// A pure read of the default Copilot policy must not materialize a phantom
// (never-persisted) entry, and returns GitHub's default posture.
func TestGetCopilotCodingAgentPermsDoesNotMaterializeDefault(t *testing.T) {
	st := store.NewStore()
	p := st.GetCopilotCodingAgentPermissions("acme")
	if p == nil || p.EnabledRepositories != "all" {
		t.Fatalf("default perms = %+v, want EnabledRepositories=all", p)
	}
	st.Mu.RLock()
	_, exists := st.CopilotCodingAgentPerms["acme"]
	st.Mu.RUnlock()
	if exists {
		t.Fatal("a pure read materialized a phantom perms entry")
	}
}

package bleephub

import (
	"strconv"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestInterruptedCodespaceProvisioningReconcilesOnLoad pins STORE-041's
// remaining half: reserveCodespace commits the durable record in "Provisioning"
// before the container is started, so a crash in that window leaves an orphan
// whose container never survives a restart. On load the store must reconcile it
// to a resumable "Shutdown" state rather than strand it in "Provisioning"
// forever, and that heal must itself be durable.
func TestInterruptedCodespaceProvisioningReconcilesOnLoad(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)

	p1, err := store.NewPersistence()
	if err != nil {
		t.Fatalf("open persistence: %v", err)
	}
	st1 := store.NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatalf("attach persistence: %v", err)
	}
	// Simulate the crash: the durable record exists in "Provisioning" (as
	// reserveCodespace leaves it) but no container/process survives. A settled
	// codespace is written alongside to prove only the interrupted one is touched.
	st1.Persist.MustPut("codespaces", "1", &store.Codespace{ID: 1, Name: "orphan-cs", State: "Provisioning"})
	st1.Persist.MustPut("codespaces", "2", &store.Codespace{ID: 2, Name: "healthy-cs", State: "Available"})
	if err := p1.Close(); err != nil {
		t.Fatalf("close persistence: %v", err)
	}

	reload := func(t *testing.T) *store.Store {
		t.Helper()
		p, err := store.NewPersistence()
		if err != nil {
			t.Fatalf("reopen persistence: %v", err)
		}
		t.Cleanup(func() { _ = p.Close() })
		st := store.NewStore()
		if err := st.SetPersistence(p); err != nil {
			t.Fatalf("reload persistence: %v", err)
		}
		return st
	}

	st2 := reload(t)
	orphan := st2.Codespaces[1]
	if orphan == nil {
		t.Fatal("interrupted codespace did not reload")
	}
	if orphan.State != "Shutdown" {
		t.Fatalf("interrupted Provisioning codespace reconciled to %q, want Shutdown", orphan.State)
	}
	if healthy := st2.Codespaces[2]; healthy == nil || healthy.State != "Available" {
		t.Fatalf("a settled codespace was disturbed by reconciliation: %#v", healthy)
	}

	// The heal is durable: reopening the same data dir still sees "Shutdown",
	// proving the reconciled state was committed, not just fixed in memory.
	raw, err := st2.Persist.Get("codespaces", strconv.Itoa(1))
	if err != nil || raw == nil {
		t.Fatalf("reconciled codespace row unreadable: %v", err)
	}
	var persisted store.Codespace
	if err := store.LoadJSON(raw, &persisted); err != nil {
		t.Fatalf("decode reconciled row: %v", err)
	}
	if persisted.State != "Shutdown" {
		t.Fatalf("reconciled state was not persisted: durable state is %q, want Shutdown", persisted.State)
	}
}

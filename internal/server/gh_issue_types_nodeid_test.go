package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestFindIssueTypeByNodeIDUsesIndex pins GQL-024's fix: the issue-type node-ID
// finder resolves through the O(1) IssueTypesByID index (populated on create,
// cleared on delete) rather than walking every org's map, while the NodeID guard
// and prefix decode reject foreign or wrong-shaped node IDs.
func TestFindIssueTypeByNodeIDUsesIndex(t *testing.T) {
	t.Parallel()
	st := newIsolatedServer(t).store

	it := st.CreateIssueType("some-org", "Epic", nil, nil, true)
	if it == nil {
		t.Fatal("create issue type")
	}
	if st.IssueTypesByID[it.ID] != it {
		t.Fatalf("IssueTypesByID missing id %d after create", it.ID)
	}
	if got := store.FindIssueTypeByNodeID(st, it.NodeID); got == nil || got.ID != it.ID {
		t.Fatalf("finder = %#v, want id %d", got, it.ID)
	}
	// Decode succeeds but no such id → guard/lookup returns nil, not a wrong type.
	if got := store.FindIssueTypeByNodeID(st, "IT_kwDO99999999"); got != nil {
		t.Fatalf("unknown node id resolved to %#v", got)
	}
	// Wrong prefix decodes to nothing and the scan finds no match.
	if got := store.FindIssueTypeByNodeID(st, "R_kgDO00000001"); got != nil {
		t.Fatalf("wrong-prefix node id resolved to %#v", got)
	}

	if !st.DeleteIssueType("some-org", it.ID) {
		t.Fatal("delete issue type")
	}
	if _, ok := st.IssueTypesByID[it.ID]; ok {
		t.Fatal("IssueTypesByID retained a deleted id")
	}
	if got := store.FindIssueTypeByNodeID(st, it.NodeID); got != nil {
		t.Fatalf("deleted issue type still found: %#v", got)
	}
}

// TestIssueTypeNodeIDIndexRebuiltOnReload pins that the O(1) index is rebuilt
// from persistence on startup, so the fast-path finder works after a restart.
func TestIssueTypeNodeIDIndexRebuiltOnReload(t *testing.T) {
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
	it := st1.CreateIssueType("org-x", "Bug", nil, nil, true)
	if it == nil {
		t.Fatal("create issue type")
	}
	nodeID, id := it.NodeID, it.ID
	if err := p1.Close(); err != nil {
		t.Fatalf("close persistence: %v", err)
	}

	p2, err := store.NewPersistence()
	if err != nil {
		t.Fatalf("reopen persistence: %v", err)
	}
	defer p2.Close()
	st2 := store.NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatalf("reload persistence: %v", err)
	}
	if st2.IssueTypesByID[id] == nil {
		t.Fatal("IssueTypesByID was not rebuilt on reload")
	}
	if got := store.FindIssueTypeByNodeID(st2, nodeID); got == nil || got.ID != id {
		t.Fatalf("finder after reload = %#v, want id %d", got, id)
	}
}

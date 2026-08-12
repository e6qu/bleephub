package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestFindByNodeIDDecodesDBID covers GQL-024: GraphQL node finders decode the
// database id embedded in the node ID and do an O(1) map lookup instead of
// scanning the whole map under the global lock, while a foreign- or legacy-
// shaped id still falls back to the scan (so behavior is unchanged).
func TestFindByNodeIDDecodesDBID(t *testing.T) {
	if id, ok := store.DecodeNodeDBID("R_kgDO00000123", "R_kgDO"); !ok || id != 123 {
		t.Fatalf("decode R_kgDO00000123 = %d,%v; want 123,true", id, ok)
	}
	if _, ok := store.DecodeNodeDBID("U_bleephub_ghost", "U_kgDO"); ok {
		t.Fatal("decode accepted a foreign-shaped node id")
	}
	if _, ok := store.DecodeNodeDBID("R_kgDOxyz", "R_kgDO"); ok {
		t.Fatal("decode accepted a non-numeric id")
	}

	s := newTestServer()
	st := s.store
	admin := st.UsersByLogin["admin"]
	repo := st.CreateRepo(admin, "node-id-repo", "", false)

	// Fast path resolves the same records a scan would.
	if got := store.FindRepoByNodeID(st, repo.NodeID); got == nil || got.ID != repo.ID {
		t.Fatalf("findRepoByNodeID(%q) = %v, want repo %d", repo.NodeID, got, repo.ID)
	}
	if got := store.FindUserByNodeID(st, admin.NodeID); got == nil || got.ID != admin.ID {
		t.Fatalf("findUserByNodeID(%q) = %v, want user %d", admin.NodeID, got, admin.ID)
	}

	// An unknown / foreign-shaped id resolves to nil — the decode fast path must
	// never mis-hit a real record.
	if got := store.FindRepoByNodeID(st, "R_kgDO99999999"); got != nil {
		t.Fatalf("findRepoByNodeID(unknown) = %v, want nil", got)
	}
	if got := store.FindUserByNodeID(st, "U_bleephub_ghost"); got != nil {
		t.Fatalf("findUserByNodeID(foreign-shaped) = %v, want nil", got)
	}
}

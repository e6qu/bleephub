package bleephub

import (
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// STORE-025: a per-user logout resolves the user's sessions through a durable
// secondary index (a bounded prefix scan) instead of listing and decoding the
// whole session bucket, and it works across replicas.

func newPersistedStore(t *testing.T) (*store.Store, *store.Persistence) {
	t.Helper()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	p, err := store.NewPersistence()
	if err != nil {
		t.Fatalf("NewPersistence: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	st := store.NewStore()
	if err := st.SetPersistence(p); err != nil {
		t.Fatalf("SetPersistence: %v", err)
	}
	return st, p
}

func TestLoginSessionUserIndexPerUserPurge(t *testing.T) {
	st, p := newPersistedStore(t)
	future := fixedTestTime.Add(time.Hour)
	put := func(id string, uid int) {
		if err := st.PutLoginSession(id, &store.LoginSession{UserID: uid, ExpiresAt: future}); err != nil {
			t.Fatalf("PutLoginSession %s: %v", id, err)
		}
	}
	put("u1-a", 1)
	put("u1-b", 1)
	put("u12-a", 12) // shares the "1" prefix digit — must not be caught by user 1's scan

	if err := st.DeleteLoginSessionsForUser(1); err != nil {
		t.Fatalf("DeleteLoginSessionsForUser: %v", err)
	}

	for _, id := range []string{"u1-a", "u1-b"} {
		if s, _ := st.GetLoginSession(id); s != nil {
			t.Fatalf("session %s survived a user-1 purge", id)
		}
	}
	if s, _ := st.GetLoginSession("u12-a"); s == nil {
		t.Fatal("user 12's session was wrongly purged with user 1 (prefix disambiguation failed)")
	}
	if idx, _ := p.ListPrefix(store.LoginSessionsByUserBucket, store.LoginSessionUserIndexPrefix(1)); len(idx) != 0 {
		t.Fatalf("stale index rows for user 1 after purge: %d", len(idx))
	}
	if idx, _ := p.ListPrefix(store.LoginSessionsByUserBucket, store.LoginSessionUserIndexPrefix(12)); len(idx) != 1 {
		t.Fatalf("user 12 index rows = %d, want 1", len(idx))
	}
}

func TestLoginSessionUserIndexRevokesCrossReplica(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	p1, err := store.NewPersistence()
	if err != nil {
		t.Fatalf("NewPersistence p1: %v", err)
	}
	defer p1.Close()
	st1 := store.NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatalf("SetPersistence st1: %v", err)
	}
	p2, err := store.NewPersistence()
	if err != nil {
		t.Fatalf("NewPersistence p2: %v", err)
	}
	defer p2.Close()
	st2 := store.NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatalf("SetPersistence st2: %v", err)
	}

	future := fixedTestTime.Add(time.Hour)
	if err := st1.PutLoginSession("only-on-1", &store.LoginSession{UserID: 7, ExpiresAt: future}); err != nil {
		t.Fatalf("PutLoginSession: %v", err)
	}

	// st2 never cached this session; the durable index must still let it revoke.
	if err := st2.DeleteLoginSessionsForUser(7); err != nil {
		t.Fatalf("DeleteLoginSessionsForUser on peer: %v", err)
	}
	if raw, err := p1.Get(store.LoginSessionsBucket, "only-on-1"); err != nil || raw != nil {
		t.Fatalf("cross-replica session not revoked via durable index (raw=%v err=%v)", raw, err)
	}
}

func TestLoginSessionUserIndexBackfillHealsPreexisting(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	future := fixedTestTime.Add(time.Hour)

	// Phase 1: a session row written before the index existed (no index row).
	p1, err := store.NewPersistence()
	if err != nil {
		t.Fatalf("NewPersistence p1: %v", err)
	}
	st1 := store.NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatalf("SetPersistence st1: %v", err)
	}
	st1.SeedDefaultUser()
	uid := st1.LookupUserByLogin("admin").ID
	if err := p1.Put(store.LoginSessionsBucket, "legacy", &store.LoginSession{UserID: uid, ExpiresAt: future}); err != nil {
		t.Fatalf("seed legacy session: %v", err)
	}
	if idx, _ := p1.ListPrefix(store.LoginSessionsByUserBucket, store.LoginSessionUserIndexPrefix(uid)); len(idx) != 0 {
		t.Fatalf("unexpected index rows before backfill: %d", len(idx))
	}
	_ = p1.Close()

	// Phase 2: a fresh process loads the DB; SetPersistence backfills the index.
	p2, err := store.NewPersistence()
	if err != nil {
		t.Fatalf("NewPersistence p2: %v", err)
	}
	defer p2.Close()
	st2 := store.NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatalf("SetPersistence st2: %v", err)
	}
	if idx, _ := p2.ListPrefix(store.LoginSessionsByUserBucket, store.LoginSessionUserIndexPrefix(uid)); len(idx) != 1 {
		t.Fatalf("backfill did not index the pre-existing session (index rows=%d)", len(idx))
	}
	if err := st2.DeleteLoginSessionsForUser(uid); err != nil {
		t.Fatalf("DeleteLoginSessionsForUser after backfill: %v", err)
	}
	if raw, _ := p2.Get(store.LoginSessionsBucket, "legacy"); raw != nil {
		t.Fatal("pre-existing session not revoked after backfill")
	}
}

func TestLoginSessionUserIndexEphemeral(t *testing.T) {
	st := store.NewStore() // no persistence: the in-memory map is the complete set
	future := fixedTestTime.Add(time.Hour)
	if err := st.PutLoginSession("e1", &store.LoginSession{UserID: 3, ExpiresAt: future}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutLoginSession("e2", &store.LoginSession{UserID: 4, ExpiresAt: future}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteLoginSessionsForUser(3); err != nil {
		t.Fatalf("DeleteLoginSessionsForUser: %v", err)
	}
	if s, _ := st.GetLoginSession("e1"); s != nil {
		t.Fatal("ephemeral user-3 session survived the purge")
	}
	if s, _ := st.GetLoginSession("e2"); s == nil {
		t.Fatal("ephemeral user-4 session was wrongly purged")
	}
}

package bleephub

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// TestPutLoginSessionConcurrentSameIDStaysConsistent is a regression guard:
// concurrent writes to the same session id must not leave the
// durable row and the in-memory index disagreeing. Run under -race, it also
// asserts the map write is serialized. With the durable Put and the map write
// in one critical section the last writer sets both, so they always agree.
func TestPutLoginSessionConcurrentSameIDStaysConsistent(t *testing.T) {
	dataDir := t.TempDir()
	persistence := openTestPersistence(t, dataDir)
	defer persistence.Close()
	st := store.NewStore()
	replaceStoreClockNow(st, func() time.Time { return fixedTestTime })
	if err := st.SetPersistence(persistence); err != nil {
		t.Fatalf("set persistence: %v", err)
	}
	st.SeedDefaultUser()
	userID := st.UsersByLogin["admin"].ID

	const writers = 16
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := range writers {
		go func(i int) {
			defer wg.Done()
			// Distinct expiries let us tell which write landed.
			session := &store.LoginSession{UserID: userID, ExpiresAt: fixedTestTime.Add(time.Duration(i+1) * time.Hour)}
			if err := st.PutLoginSession("shared", session); err != nil {
				t.Errorf("put login session: %v", err)
			}
		}(i)
	}
	wg.Wait()

	mapKey := store.LoginSessionMapKey(st.Persist, "shared")
	st.Mu.RLock()
	mem := st.LoginSessions[mapKey]
	st.Mu.RUnlock()
	if mem == nil {
		t.Fatal("in-memory session missing after concurrent writes")
	}
	raw, err := st.Persist.Get(store.LoginSessionsBucket, mapKey)
	if err != nil {
		t.Fatalf("read durable session: %v", err)
	}
	if raw == nil {
		t.Fatal("durable session missing after concurrent writes")
	}
	var durable store.LoginSession
	if err := json.Unmarshal(raw, &durable); err != nil {
		t.Fatalf("decode durable session: %v", err)
	}
	if !mem.ExpiresAt.Equal(durable.ExpiresAt) {
		t.Fatalf("durable and in-memory sessions diverged: memory=%s durable=%s", mem.ExpiresAt, durable.ExpiresAt)
	}
}

// TestReapDropsLoginSessionsOfDeletedUsers is a regression guard: a login
// session whose owner no longer resolves must be reaped from both memory and the
// durable bucket, even though it has not yet expired. A session for a live user
// must be left untouched.
func TestReapDropsLoginSessionsOfDeletedUsers(t *testing.T) {
	dataDir := t.TempDir()
	persistence := openTestPersistence(t, dataDir)
	defer persistence.Close()
	st := store.NewStore()
	replaceStoreClockNow(st, func() time.Time { return fixedTestTime })
	if err := st.SetPersistence(persistence); err != nil {
		t.Fatalf("set persistence: %v", err)
	}
	st.SeedDefaultUser()
	adminID := st.UsersByLogin["admin"].ID

	// "live" belongs to the seeded admin; "orphan" references a user id that does
	// not resolve, standing in for a user deleted after the session was minted.
	const deletedUserID = 999999
	live := &store.LoginSession{UserID: adminID, ExpiresAt: fixedTestTime.Add(time.Hour)}
	orphan := &store.LoginSession{UserID: deletedUserID, ExpiresAt: fixedTestTime.Add(time.Hour)}
	if err := st.PutLoginSession("live", live); err != nil {
		t.Fatalf("put live session: %v", err)
	}
	if err := st.PutLoginSession("orphan", orphan); err != nil {
		t.Fatalf("put orphan session: %v", err)
	}

	if err := st.ReapExpiredLoginSessions(fixedTestTime); err != nil {
		t.Fatalf("reap: %v", err)
	}

	if session, err := st.GetLoginSession("orphan"); err != nil || session != nil {
		t.Fatalf("orphaned session survived reap: %#v, %v", session, err)
	}
	if session, err := st.GetLoginSession("live"); err != nil || session == nil {
		t.Fatalf("live session was wrongly reaped: %#v, %v", session, err)
	}

	// The orphan must also be gone from durable storage.
	orphanKey := store.LoginSessionMapKey(st.Persist, "orphan")
	if raw, err := st.Persist.Get(store.LoginSessionsBucket, orphanKey); err != nil {
		t.Fatalf("read durable orphan session: %v", err)
	} else if raw != nil {
		t.Fatal("orphaned session survived in durable storage")
	}
}

package bleephub

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// TestPutLoginSessionConcurrentSameIDStaysConsistent is the STORE-072
// regression: concurrent writes to the same session id must not leave the
// durable row and the in-memory index disagreeing. Run under -race, it also
// asserts the map write is serialized. With the durable Put and the map write
// in one critical section the last writer sets both, so they always agree.
func TestPutLoginSessionConcurrentSameIDStaysConsistent(t *testing.T) {
	dataDir := t.TempDir()
	persistence := openTestPersistence(t, dataDir)
	defer persistence.Close()
	store := NewStore()
	store.replaceClockNow(func() time.Time { return fixedTestTime })
	if err := store.SetPersistence(persistence); err != nil {
		t.Fatalf("set persistence: %v", err)
	}
	store.SeedDefaultUser()
	userID := store.UsersByLogin["admin"].ID

	const writers = 16
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := range writers {
		go func(i int) {
			defer wg.Done()
			// Distinct expiries let us tell which write landed.
			session := &LoginSession{UserID: userID, ExpiresAt: fixedTestTime.Add(time.Duration(i+1) * time.Hour)}
			if err := store.PutLoginSession("shared", session); err != nil {
				t.Errorf("put login session: %v", err)
			}
		}(i)
	}
	wg.Wait()

	mapKey := loginSessionMapKey(store.persist, "shared")
	store.mu.RLock()
	mem := store.LoginSessions[mapKey]
	store.mu.RUnlock()
	if mem == nil {
		t.Fatal("in-memory session missing after concurrent writes")
	}
	raw, err := store.persist.Get(loginSessionsBucket, mapKey)
	if err != nil {
		t.Fatalf("read durable session: %v", err)
	}
	if raw == nil {
		t.Fatal("durable session missing after concurrent writes")
	}
	var durable LoginSession
	if err := json.Unmarshal(raw, &durable); err != nil {
		t.Fatalf("decode durable session: %v", err)
	}
	if !mem.ExpiresAt.Equal(durable.ExpiresAt) {
		t.Fatalf("durable and in-memory sessions diverged: memory=%s durable=%s", mem.ExpiresAt, durable.ExpiresAt)
	}
}

// TestReapDropsLoginSessionsOfDeletedUsers is the STORE-073 regression: a login
// session whose owner no longer resolves must be reaped from both memory and the
// durable bucket, even though it has not yet expired. A session for a live user
// must be left untouched.
func TestReapDropsLoginSessionsOfDeletedUsers(t *testing.T) {
	dataDir := t.TempDir()
	persistence := openTestPersistence(t, dataDir)
	defer persistence.Close()
	store := NewStore()
	store.replaceClockNow(func() time.Time { return fixedTestTime })
	if err := store.SetPersistence(persistence); err != nil {
		t.Fatalf("set persistence: %v", err)
	}
	store.SeedDefaultUser()
	adminID := store.UsersByLogin["admin"].ID

	// "live" belongs to the seeded admin; "orphan" references a user id that does
	// not resolve, standing in for a user deleted after the session was minted.
	const deletedUserID = 999999
	live := &LoginSession{UserID: adminID, ExpiresAt: fixedTestTime.Add(time.Hour)}
	orphan := &LoginSession{UserID: deletedUserID, ExpiresAt: fixedTestTime.Add(time.Hour)}
	if err := store.PutLoginSession("live", live); err != nil {
		t.Fatalf("put live session: %v", err)
	}
	if err := store.PutLoginSession("orphan", orphan); err != nil {
		t.Fatalf("put orphan session: %v", err)
	}

	if err := store.ReapExpiredLoginSessions(fixedTestTime); err != nil {
		t.Fatalf("reap: %v", err)
	}

	if session, err := store.GetLoginSession("orphan"); err != nil || session != nil {
		t.Fatalf("orphaned session survived reap: %#v, %v", session, err)
	}
	if session, err := store.GetLoginSession("live"); err != nil || session == nil {
		t.Fatalf("live session was wrongly reaped: %#v, %v", session, err)
	}

	// The orphan must also be gone from durable storage.
	orphanKey := loginSessionMapKey(store.persist, "orphan")
	if raw, err := store.persist.Get(loginSessionsBucket, orphanKey); err != nil {
		t.Fatalf("read durable orphan session: %v", err)
	} else if raw != nil {
		t.Fatal("orphaned session survived in durable storage")
	}
}

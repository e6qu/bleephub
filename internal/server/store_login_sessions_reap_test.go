package bleephub

import (
	"testing"
	"time"
)

func TestExpiredLoginSessionReapIsDurableAndDeterministic(t *testing.T) {
	dataDir := t.TempDir()
	persistence := openTestPersistence(t, dataDir)
	store := NewStore()
	replaceStoreClockNow(store, func() time.Time { return fixedTestTime })
	if err := store.SetPersistence(persistence); err != nil {
		t.Fatalf("set persistence: %v", err)
	}
	store.SeedDefaultUser()

	expired := &LoginSession{UserID: store.UsersByLogin["admin"].ID, ExpiresAt: fixedTestTime.Add(-time.Minute)}
	live := &LoginSession{UserID: store.UsersByLogin["admin"].ID, ExpiresAt: fixedTestTime.Add(time.Hour)}
	if err := store.PutLoginSession("expired", expired); err != nil {
		t.Fatalf("put expired session: %v", err)
	}
	if err := store.PutLoginSession("live", live); err != nil {
		t.Fatalf("put live session: %v", err)
	}
	if err := store.ReapExpiredLoginSessions(fixedTestTime); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if session, err := store.GetLoginSession("expired"); err != nil || session != nil {
		t.Fatalf("expired session = %#v, %v", session, err)
	}
	if session, err := store.GetLoginSession("live"); err != nil || session == nil {
		t.Fatalf("live session = %#v, %v", session, err)
	}

	if err := persistence.Close(); err != nil {
		t.Fatalf("close persistence: %v", err)
	}
	reopened := openTestPersistence(t, dataDir)
	defer func() { _ = reopened.Close() }()
	reloaded := NewStore()
	replaceStoreClockNow(reloaded, func() time.Time { return fixedTestTime })
	if err := reloaded.SetPersistence(reopened); err != nil {
		t.Fatalf("reload persistence: %v", err)
	}
	if session, err := reloaded.GetLoginSession("expired"); err != nil || session != nil {
		t.Fatalf("expired session survived reload: %#v, %v", session, err)
	}
	if session, err := reloaded.GetLoginSession("live"); err != nil || session == nil {
		t.Fatalf("live session missing after reload: %#v, %v", session, err)
	}
}

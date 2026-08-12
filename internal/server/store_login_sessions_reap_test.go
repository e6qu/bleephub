package bleephub

import (
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func TestExpiredLoginSessionReapIsDurableAndDeterministic(t *testing.T) {
	dataDir := t.TempDir()
	persistence := openTestPersistence(t, dataDir)
	st := store.NewStore()
	replaceStoreClockNow(st, func() time.Time { return fixedTestTime })
	if err := st.SetPersistence(persistence); err != nil {
		t.Fatalf("set persistence: %v", err)
	}
	st.SeedDefaultUser()

	expired := &store.LoginSession{UserID: st.UsersByLogin["admin"].ID, ExpiresAt: fixedTestTime.Add(-time.Minute)}
	live := &store.LoginSession{UserID: st.UsersByLogin["admin"].ID, ExpiresAt: fixedTestTime.Add(time.Hour)}
	if err := st.PutLoginSession("expired", expired); err != nil {
		t.Fatalf("put expired session: %v", err)
	}
	if err := st.PutLoginSession("live", live); err != nil {
		t.Fatalf("put live session: %v", err)
	}
	if err := st.ReapExpiredLoginSessions(fixedTestTime); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if session, err := st.GetLoginSession("expired"); err != nil || session != nil {
		t.Fatalf("expired session = %#v, %v", session, err)
	}
	if session, err := st.GetLoginSession("live"); err != nil || session == nil {
		t.Fatalf("live session = %#v, %v", session, err)
	}

	if err := persistence.Close(); err != nil {
		t.Fatalf("close persistence: %v", err)
	}
	reopened := openTestPersistence(t, dataDir)
	defer func() { _ = reopened.Close() }()
	reloaded := store.NewStore()
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

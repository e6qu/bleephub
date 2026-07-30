package bleephub

import (
	"sync"
	"testing"
)

// TestTokenMatchesClientUsesTheStoreLock covers the final REST-014 map access:
// application-token inspection runs concurrently with App management. The
// assertion makes the test meaningful without -race; the detector proves the
// lookup cannot collide with the write.
func TestTokenMatchesClientUsesTheStoreLock(t *testing.T) {
	st := newTestServer().store
	const clientID = "Iv1.concurrent-client"
	token := &UserToServerToken{AppID: 73}
	app := &App{ID: token.AppID, ClientID: clientID}

	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; i < 1_000; i++ {
			st.mu.Lock()
			st.AppsByClientID[clientID] = app
			st.mu.Unlock()
			st.mu.Lock()
			delete(st.AppsByClientID, clientID)
			st.mu.Unlock()
		}
	}()
	for i := 0; i < 1_000; i++ {
		_ = tokenMatchesClient(token, clientID, st)
	}
	writers.Wait()

	st.mu.Lock()
	st.AppsByClientID[clientID] = app
	st.mu.Unlock()
	if !tokenMatchesClient(token, clientID, st) {
		t.Fatal("matching App client was not recognized after concurrent access")
	}
}

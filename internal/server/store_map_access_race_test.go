package bleephub

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
	"golang.org/x/crypto/ssh"
)

// TestTokenMatchesClientUsesTheStoreLock covers the final unguarded map access:
// application-token inspection runs concurrently with App management. The
// assertion makes the test meaningful without -race; the detector proves the
// lookup cannot collide with the write.
func TestTokenMatchesClientUsesTheStoreLock(t *testing.T) {
	st := newTestServer().store
	const clientID = "Iv1.concurrent-client"
	token := &store.UserToServerToken{AppID: 73}
	app := &store.App{ID: token.AppID, ClientID: clientID}

	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; i < 1_000; i++ {
			st.Mu.Lock()
			st.AppsByClientID[clientID] = app
			st.Mu.Unlock()
			st.Mu.Lock()
			delete(st.AppsByClientID, clientID)
			st.Mu.Unlock()
		}
	}()
	for i := 0; i < 1_000; i++ {
		_ = tokenMatchesClient(token, clientID, st)
	}
	writers.Wait()

	st.Mu.Lock()
	st.AppsByClientID[clientID] = app
	st.Mu.Unlock()
	if !tokenMatchesClient(token, clientID, st) {
		t.Fatal("matching App client was not recognized after concurrent access")
	}
}

// TestUserGraphReadersUseStoreLock covers the STORE map-access race (P5) and
// lock-order inversion (P9) fixes. ListUserBlocks, IsUserFollowing and
// LookupUserBySSHKey are Misc.mu-guarded read paths that must resolve
// st.Users through the st.mu-guarded accessor rather than dereferencing it
// under Misc.mu (P5) or holding Misc.mu across a GetUserByID call (P9). The
// -race detector proves these readers cannot collide with concurrent user
// writes; the post-run assertions keep the test meaningful without it.
func TestUserGraphReadersUseStoreLock(t *testing.T) {
	st := newTestServer().store

	blocker := &store.User{ID: 9001, Login: "blocker"}
	blocked := &store.User{ID: 9002, Login: "blocked"}
	follower := &store.User{ID: 9003, Login: "follower"}
	followee := &store.User{ID: 9004, Login: "followee"}
	st.Mu.Lock()
	for _, u := range []*store.User{blocker, blocked, follower, followee} {
		st.Users[u.ID] = u
		st.UsersByLogin[u.Login] = u
	}
	st.Mu.Unlock()

	st.BlockUser(blocker.ID, blocked.ID)

	st.Misc.Mu.Lock()
	st.Misc.Follows[follower.Login] = map[string]bool{followee.Login: true}
	st.Misc.Mu.Unlock()

	pub, err := ssh.NewPublicKey(testSSHKey.Public())
	if err != nil {
		t.Fatalf("derive ssh public key: %v", err)
	}
	uk := &store.UserKey{ID: 1, Key: string(ssh.MarshalAuthorizedKey(pub)), UserID: followee.ID}
	if err := store.CacheParsedKey(uk); err != nil {
		t.Fatalf("cache parsed key: %v", err)
	}
	st.Misc.Mu.Lock()
	st.Misc.KeysByUser[followee.ID] = []*store.UserKey{uk}
	st.Misc.Mu.Unlock()

	// Writer churns st.Users under st.mu.Lock for as long as the readers run.
	var stop atomic.Bool
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		churn := &store.User{ID: 9999, Login: "churn"}
		for !stop.Load() {
			st.Mu.Lock()
			st.Users[churn.ID] = churn
			st.UsersByLogin[churn.Login] = churn
			st.Mu.Unlock()
			st.Mu.Lock()
			delete(st.Users, churn.ID)
			delete(st.UsersByLogin, churn.Login)
			st.Mu.Unlock()
		}
	}()

	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for n := 0; n < 2_000; n++ {
				_ = st.ListUserBlocks(blocker.ID)
				_ = st.IsUserFollowing(follower.ID, followee.ID)
				_ = st.LookupUserBySSHKey(pub)
			}
		}()
	}
	readers.Wait()
	stop.Store(true)
	writers.Wait()

	if blocks := st.ListUserBlocks(blocker.ID); len(blocks) != 1 || blocks[0].ID != blocked.ID {
		t.Fatalf("ListUserBlocks = %v, want [blocked]", blocks)
	}
	if !st.IsUserFollowing(follower.ID, followee.ID) {
		t.Fatal("IsUserFollowing = false, want true")
	}
	if got := st.LookupUserBySSHKey(pub); got == nil || got.ID != followee.ID {
		t.Fatalf("LookupUserBySSHKey = %v, want followee", got)
	}
}

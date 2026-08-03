package bleephub

import (
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestTokenMatchesClientUsesTheStoreLock covers the final unguarded map access:
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

// TestUserGraphReadersUseStoreLock covers the STORE map-access race (P5) and
// lock-order inversion (P9) fixes. ListUserBlocks, IsUserFollowing and
// LookupUserBySSHKey are Misc.mu-guarded read paths that must resolve
// st.Users through the st.mu-guarded accessor rather than dereferencing it
// under Misc.mu (P5) or holding Misc.mu across a GetUserByID call (P9). The
// -race detector proves these readers cannot collide with concurrent user
// writes; the post-run assertions keep the test meaningful without it.
func TestUserGraphReadersUseStoreLock(t *testing.T) {
	st := newTestServer().store

	blocker := &User{ID: 9001, Login: "blocker"}
	blocked := &User{ID: 9002, Login: "blocked"}
	follower := &User{ID: 9003, Login: "follower"}
	followee := &User{ID: 9004, Login: "followee"}
	st.mu.Lock()
	for _, u := range []*User{blocker, blocked, follower, followee} {
		st.Users[u.ID] = u
		st.UsersByLogin[u.Login] = u
	}
	st.mu.Unlock()

	st.BlockUser(blocker.ID, blocked.ID)

	st.Misc.mu.Lock()
	st.Misc.follows[follower.Login] = map[string]bool{followee.Login: true}
	st.Misc.mu.Unlock()

	pub, err := ssh.NewPublicKey(testSSHKey.Public())
	if err != nil {
		t.Fatalf("derive ssh public key: %v", err)
	}
	uk := &UserKey{ID: 1, Key: string(ssh.MarshalAuthorizedKey(pub)), UserID: followee.ID}
	if err := cacheParsedKey(uk); err != nil {
		t.Fatalf("cache parsed key: %v", err)
	}
	st.Misc.mu.Lock()
	st.Misc.keysByUser[followee.ID] = []*UserKey{uk}
	st.Misc.mu.Unlock()

	// Writer churns st.Users under st.mu.Lock for as long as the readers run.
	var stop atomic.Bool
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		churn := &User{ID: 9999, Login: "churn"}
		for !stop.Load() {
			st.mu.Lock()
			st.Users[churn.ID] = churn
			st.UsersByLogin[churn.Login] = churn
			st.mu.Unlock()
			st.mu.Lock()
			delete(st.Users, churn.ID)
			delete(st.UsersByLogin, churn.Login)
			st.mu.Unlock()
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

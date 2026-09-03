package bleephub

import (
	"net/http"
	"testing"
)

// TestRepoTransferRekeysStarredAndPinned pins that transferring (or renaming) a
// repository re-keys the full name every user record stores for it: the starred-
// repo map and the ordered pinned-repo list. Previously only the repo's own
// stargazer set moved, so a starrer's starred list and a pinner's profile kept
// naming the vacated address.
func TestRepoTransferRekeysStarredAndPinned(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.seedRepo(t, "widget", false) // admin/widget
	s.newUser(t, "recipient")
	fan, _ := s.newUser(t, "starfan")

	if !s.store.StarRepo(fan.ID, "admin", "widget") {
		t.Fatal("star failed")
	}
	s.store.Mu.Lock()
	s.store.Users[fan.ID].PinnedRepos = []string{"admin/widget"}
	s.store.Mu.Unlock()

	if !s.store.TransferRepo("admin", "widget", "recipient") {
		t.Fatal("transfer failed")
	}

	starred := s.store.ListStarredRepos(fan.ID)
	if len(starred) != 1 || starred[0] != "recipient/widget" {
		t.Fatalf("starred repos after transfer = %v, want [recipient/widget]", starred)
	}
	s.store.Mu.RLock()
	pinned := append([]string(nil), s.store.Users[fan.ID].PinnedRepos...)
	s.store.Mu.RUnlock()
	if len(pinned) != 1 || pinned[0] != "recipient/widget" {
		t.Fatalf("pinned repos after transfer = %v, want [recipient/widget]", pinned)
	}
}

// TestAdminUserRenameCascadesReposAndFollows pins that renaming an account
// re-keys the login-keyed state the user row alone does not carry: every
// repository it owns is transferred to the new login (reachable there, gone from
// the old address), and its follow edges move in both directions. Previously the
// rename touched only UsersByLogin, so the account's repos 404'd at the new login
// and its followers/following counts silently pointed at a login that no longer
// resolved.
func TestAdminUserRenameCascadesReposAndFollows(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	renamer, _ := s.newUser(t, "renamer")
	s.newUser(t, "fan")

	if s.store.CreateRepo(renamer, "gadget", "", false) == nil {
		t.Fatal("seed repo failed")
	}
	s.store.SetFollow("renamer", "admin", true) // renamer follows admin (outgoing)
	s.store.SetFollow("fan", "renamer", true)   // fan follows renamer (incoming)

	resp := s.patch(t, "/api/v3/admin/users/renamer", defaultToken, map[string]any{"login": "gizmo"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("rename = %d, want 202", resp.StatusCode)
	}

	// The owned repo followed the rename.
	r := s.get(t, "/api/v3/repos/gizmo/gadget", defaultToken)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("repo at new login = %d, want 200", r.StatusCode)
	}

	// The old account address no longer resolves.
	old := s.get(t, "/api/v3/users/renamer", defaultToken)
	old.Body.Close()
	if old.StatusCode != http.StatusNotFound {
		t.Fatalf("old login = %d, want 404", old.StatusCode)
	}

	// Follow edges moved in both directions and nothing lingers under the old login.
	if !s.store.LoginFollows("gizmo", "admin") {
		t.Fatal("outgoing follow was not re-keyed to the new login")
	}
	if s.store.LoginFollows("renamer", "admin") {
		t.Fatal("outgoing follow still stranded under the old login")
	}
	if !s.store.LoginFollows("fan", "gizmo") {
		t.Fatal("incoming follow was not re-keyed to the new login")
	}
	if s.store.LoginFollows("fan", "renamer") {
		t.Fatal("incoming follow still stranded under the old login")
	}
	if got := s.store.CountFollowers("gizmo"); got != 1 {
		t.Fatalf("followers of renamed account = %d, want 1", got)
	}
	if got := s.store.CountFollowing("gizmo"); got != 1 {
		t.Fatalf("following of renamed account = %d, want 1", got)
	}
}

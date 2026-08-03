package bleephub

import "testing"

// These tests exercise the STORE-001 conversions of multi-bucket mutations to a
// single persistBatch commit. Each asserts that every bucket the mutation
// touches survives a reload together — a batch that dropped one of its writes
// (the failure the atomic commit prevents) would leave the reloaded store
// inconsistent.

func TestAddRepoCollaboratorPersistsCollaboratorAndRepoAcrossReload(t *testing.T) {
	st := reloadedStore(t, func(p *Persistence, st *Store) {
		st.SeedDefaultUser()
		admin := st.UsersByLogin["admin"]
		addTestUser(p, st, "alice")
		if st.CreateRepo(admin, "collab", "", false) == nil {
			t.Fatal("create repo")
		}
		if !st.AddRepoCollaborator("admin", "collab", "alice", "push") {
			t.Fatal("add collaborator")
		}
	})

	if got := st.GetRepoCollaboratorPermission("admin", "collab", "alice"); got != "push" {
		t.Fatalf("collaborator permission after reload = %q, want %q", got, "push")
	}
	if st.GetRepo("admin", "collab") == nil {
		t.Fatal("repo row missing after reload")
	}
}

func TestRemoveRepoCollaboratorPersistsAcrossReload(t *testing.T) {
	st := reloadedStore(t, func(p *Persistence, st *Store) {
		st.SeedDefaultUser()
		admin := st.UsersByLogin["admin"]
		addTestUser(p, st, "alice")
		if st.CreateRepo(admin, "collab", "", false) == nil {
			t.Fatal("create repo")
		}
		if !st.AddRepoCollaborator("admin", "collab", "alice", "push") {
			t.Fatal("add collaborator")
		}
		if !st.RemoveRepoCollaborator("admin", "collab", "alice") {
			t.Fatal("remove collaborator")
		}
	})

	if got := st.GetRepoCollaboratorPermission("admin", "collab", "alice"); got != "" {
		t.Fatalf("collaborator still present after reload: %q", got)
	}
}

func TestCreateRepoDeployKeyPersistsAcrossReload(t *testing.T) {
	var repoID int
	st := reloadedStore(t, func(p *Persistence, st *Store) {
		st.SeedDefaultUser()
		admin := st.UsersByLogin["admin"]
		repo := st.CreateRepo(admin, "keys", "", false)
		if repo == nil {
			t.Fatal("create repo")
		}
		repoID = repo.ID
		if st.CreateRepoDeployKey(repo.ID, "ci", "ssh-ed25519 AAAA", true) == nil {
			t.Fatal("create deploy key")
		}
	})

	if keys := st.ListRepoDeployKeys(repoID); len(keys) != 1 || keys[0].Title != "ci" {
		t.Fatalf("deploy key not durable after reload: %#v", keys)
	}
}

func TestDeleteTeamReparentsChildrenAcrossReload(t *testing.T) {
	var grandparentID int
	st := reloadedStore(t, func(p *Persistence, st *Store) {
		st.SeedDefaultUser()
		admin := st.UsersByLogin["admin"]
		if st.CreateOrg(admin, "acme", "Acme", "") == nil {
			t.Fatal("create org")
		}
		grandparent := st.CreateTeam("acme", "grandparent", TeamOptions{})
		if grandparent == nil {
			t.Fatal("create grandparent team")
		}
		parent := st.CreateTeam("acme", "parent", TeamOptions{ParentID: grandparent.ID})
		if parent == nil {
			t.Fatal("create parent team")
		}
		if st.CreateTeam("acme", "child", TeamOptions{ParentID: parent.ID}) == nil {
			t.Fatal("create child team")
		}
		grandparentID = grandparent.ID
		if !st.DeleteTeam("acme", parent.Slug) {
			t.Fatal("delete parent team")
		}
	})

	child := st.GetTeam("acme", "child")
	if child == nil {
		t.Fatal("child team missing after reload")
	}
	if child.ParentID != grandparentID {
		t.Fatalf("child reparented to %d after reload, want grandparent %d", child.ParentID, grandparentID)
	}
	if st.GetTeam("acme", "parent") != nil {
		t.Fatal("deleted parent team survived reload")
	}
}

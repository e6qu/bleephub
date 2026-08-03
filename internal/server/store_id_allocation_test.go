package bleephub

import (
	"testing"
	"time"
)

// newReplicaStore opens a store backed by a persistence over dataDir that
// behaves like one dqlite replica: several of these share the same durable
// database, so ID allocation must coordinate through the counters table rather
// than each replica's private in-memory NextX.
func newReplicaStore(t *testing.T, dataDir string) (*Store, *Persistence) {
	t.Helper()
	persistence := openTestPersistence(t, dataDir)
	persistence.dialect.name = "dqlite"
	store := NewStore()
	store.replaceClockNow(func() time.Time { return fixedTestTime })
	if err := store.SetPersistence(persistence); err != nil {
		t.Fatalf("set persistence: %v", err)
	}
	return store, persistence
}

// TestCoreEntityIDsDoNotCollideAcrossReplicas is a regression guard: two
// replicas that mint an org/team from the same in-memory NextX must not pick the
// same global ID, or the second durable write silently overwrites the first.
// Routing allocation through AllocateCounterValue makes the shared counters
// table the arbiter.
func TestCoreEntityIDsDoNotCollideAcrossReplicas(t *testing.T) {
	dataDir := t.TempDir()

	first, firstPersistence := newReplicaStore(t, dataDir)
	defer firstPersistence.Close()
	first.SeedDefaultUser()

	// The second replica opens after the admin row is durable, so it loads the
	// same seed state — including the same NextOrg/NextTeam — as the first.
	second, secondPersistence := newReplicaStore(t, dataDir)
	defer secondPersistence.Close()

	adminA := first.LookupUserByLogin("admin")
	adminB := second.LookupUserByLogin("admin")
	if adminA == nil || adminB == nil {
		t.Fatalf("admin not seeded on both replicas: %v %v", adminA, adminB)
	}

	orgA := first.CreateOrg(adminA, "org-a", "Org A", "")
	orgB := second.CreateOrg(adminB, "org-b", "Org B", "")
	if orgA == nil || orgB == nil {
		t.Fatalf("org creation failed: %v %v", orgA, orgB)
	}
	if orgA.ID == orgB.ID {
		t.Fatalf("cross-replica org ID collision: both replicas minted %d", orgA.ID)
	}

	// The counter row must reflect the allocation; a zero value means the create
	// path never consulted the durable counter.
	if v, err := firstPersistence.GetCounter("next_org"); err != nil {
		t.Fatalf("read next_org counter: %v", err)
	} else if v < int64(orgA.ID) || v < int64(orgB.ID) {
		t.Fatalf("next_org counter = %d, want >= max minted id (%d, %d)", v, orgA.ID, orgB.ID)
	}

	teamA := first.CreateTeam("org-a", "team-a", TeamOptions{})
	teamB := second.CreateTeam("org-b", "team-b", TeamOptions{})
	if teamA == nil || teamB == nil {
		t.Fatalf("team creation failed: %v %v", teamA, teamB)
	}
	if teamA.ID == teamB.ID {
		t.Fatalf("cross-replica team ID collision: both replicas minted %d", teamA.ID)
	}

	// Both orgs must survive durably: a collision would have left only one row.
	verify, verifyPersistence := newReplicaStore(t, dataDir)
	defer verifyPersistence.Close()
	if verify.Orgs[orgA.ID] == nil || verify.Orgs[orgB.ID] == nil {
		t.Fatalf("an org was lost to a durable overwrite: A=%v B=%v", verify.Orgs[orgA.ID], verify.Orgs[orgB.ID])
	}
}

// TestCoreEntityIDsAreDurableAcrossReload is the durability half: once
// a store hands out an org/user/repo/team/issue ID, a fresh store over the same
// data dir must never re-issue it — the durable counter carries the high-water
// mark across the reload.
func TestCoreEntityIDsAreDurableAcrossReload(t *testing.T) {
	dataDir := t.TempDir()

	first, firstPersistence := newReplicaStore(t, dataDir)
	first.SeedDefaultUser()
	admin := first.LookupUserByLogin("admin")

	org := first.CreateOrg(admin, "acme", "Acme", "")
	team := first.CreateTeam("acme", "core", TeamOptions{})
	repo := first.CreateRepo(admin, "widgets", "", false)
	if org == nil || team == nil || repo == nil {
		t.Fatalf("fixture creation failed: org=%v team=%v repo=%v", org, team, repo)
	}
	issue := first.CreateIssue(repo.ID, admin.ID, "first issue", "", nil, nil, 0)
	if issue == nil {
		t.Fatalf("issue creation failed")
	}
	userID, orgID, teamID, repoID, issueID := admin.ID, org.ID, team.ID, repo.ID, issue.ID
	if err := firstPersistence.Close(); err != nil {
		t.Fatalf("close persistence: %v", err)
	}

	// Reopen over the same data dir and mint one more of each entity.
	second, secondPersistence := newReplicaStore(t, dataDir)
	defer secondPersistence.Close()
	second.SeedDefaultUser() // a second admin row exercises the user counter
	admin2 := second.LookupUserByLogin("admin")

	org2 := second.CreateOrg(admin2, "beta", "Beta", "")
	team2 := second.CreateTeam("beta", "core", TeamOptions{})
	repo2 := second.CreateRepo(admin2, "gadgets", "", false)
	issue2 := second.CreateIssue(repo2.ID, admin2.ID, "second issue", "", nil, nil, 0)
	if org2 == nil || team2 == nil || repo2 == nil || issue2 == nil {
		t.Fatalf("post-reload creation failed: org=%v team=%v repo=%v issue=%v", org2, team2, repo2, issue2)
	}

	if admin2.ID <= userID {
		t.Errorf("user ID re-issued across reload: got %d, want > %d", admin2.ID, userID)
	}
	if org2.ID <= orgID {
		t.Errorf("org ID re-issued across reload: got %d, want > %d", org2.ID, orgID)
	}
	if team2.ID <= teamID {
		t.Errorf("team ID re-issued across reload: got %d, want > %d", team2.ID, teamID)
	}
	if repo2.ID <= repoID {
		t.Errorf("repo ID re-issued across reload: got %d, want > %d", repo2.ID, repoID)
	}
	if issue2.ID <= issueID {
		t.Errorf("issue ID re-issued across reload: got %d, want > %d", issue2.ID, issueID)
	}
}

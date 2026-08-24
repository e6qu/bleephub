package store

// The GEI state machine's invariants, at the layer that enforces them: the
// claim that stops two workers running one migration, the terminal state that
// makes an abort final, and the source lock that is held for exactly as long
// as the migration is.

import "testing"

func newGEITestOrg(st *Store, login string) *Org {
	owner := newEnterpriseTestUser(st, login+"-owner")
	return st.CreateOrg(owner, login, login, "")
}

func TestClaimRepositoryMigrationAdmitsOneWorker(t *testing.T) {
	st := NewStore()
	org := newGEITestOrg(st, "claimorg")
	m := st.CreateRepositoryMigration(NewRepositoryMigration{
		OwnerOrgID: org.ID, RepositoryName: "one", SourceURL: "https://example.test/one.git",
	})
	if m.State != GEIMigrationStateQueued {
		t.Fatalf("a new migration is %s, want QUEUED", m.State)
	}
	if !st.ClaimRepositoryMigration(m.ID) {
		t.Fatal("the first worker could not claim a queued migration")
	}
	if st.ClaimRepositoryMigration(m.ID) {
		t.Fatal("a second worker claimed a migration already in progress")
	}
	// Only an in-progress migration is requeueable, which is what makes the
	// boot-time resume return exactly the work a dead process abandoned.
	if !st.RequeueRepositoryMigration(m.ID) {
		t.Fatal("an in-progress migration could not be requeued")
	}
	if st.RequeueRepositoryMigration(m.ID) {
		t.Fatal("a queued migration was requeued a second time")
	}
}

func TestRepositoryMigrationTerminalStatesAreFinal(t *testing.T) {
	st := NewStore()
	org := newGEITestOrg(st, "finalorg")
	m := st.CreateRepositoryMigration(NewRepositoryMigration{
		OwnerOrgID: org.ID, RepositoryName: "aborted", SourceURL: "https://example.test/a.git",
	})
	if st.SetRepositoryMigrationState(m.ID, GEIMigrationStateFailed, "aborted") == nil {
		t.Fatal("a queued migration could not be aborted")
	}
	// A worker finishing after the abort must not overwrite it.
	if st.SetRepositoryMigrationState(m.ID, GEIMigrationStateSucceeded, "") != nil {
		t.Fatal("a late worker rewrote an aborted migration")
	}
	if got := st.GetRepositoryMigration(m.ID); got.State != GEIMigrationStateFailed ||
		got.FailureReason != "aborted" {
		t.Fatalf("state after a late write = %+v", got)
	}
	if st.ClaimRepositoryMigration(m.ID) {
		t.Fatal("a terminal migration was claimable")
	}
	if st.RequeueRepositoryMigration(m.ID) {
		t.Fatal("a terminal migration was requeueable")
	}
}

func TestRepositoryMigrationSourceLockBlocksAndReleases(t *testing.T) {
	st := NewStore()
	org := newGEITestOrg(st, "lockorg")
	owner := newEnterpriseTestUser(st, "locksource")
	source := st.CreateRepo(owner, "frozen", "", false)
	if source == nil {
		t.Fatal("could not seed the source repository")
	}
	if st.RepoLockedForMigration(source.FullName) {
		t.Fatal("a repository no migration names is locked")
	}
	m := st.CreateRepositoryMigration(NewRepositoryMigration{
		OwnerOrgID: org.ID, RepositoryName: "frozen-copy",
		SourceURL: "https://example.test/frozen.git", LockSource: true,
	})
	if !st.SetRepositoryMigrationSourceLock(m.ID, source.FullName) {
		t.Fatal("the migration could not take the source lock")
	}
	if !st.RepoLockedForMigration(source.FullName) {
		t.Fatal("the source is not locked while the migration runs")
	}
	// The migration's own state releases the lock; nothing else has to
	// remember to, which is why "unlocked" cannot depend on a cleanup step.
	st.SetRepositoryMigrationState(m.ID, GEIMigrationStateSucceeded, "")
	if st.RepoLockedForMigration(source.FullName) {
		t.Fatal("the lock outlived the migration that took it")
	}
	// And a terminal migration cannot re-freeze anything.
	if st.SetRepositoryMigrationSourceLock(m.ID, source.FullName) {
		t.Fatal("a terminal migration took a source lock")
	}
}

func TestAbortQueuedRepositoryMigrationsLeavesRunningWorkAlone(t *testing.T) {
	st := NewStore()
	org := newGEITestOrg(st, "abortorg")
	other := newGEITestOrg(st, "otherorg")
	queued := st.CreateRepositoryMigration(NewRepositoryMigration{
		OwnerOrgID: org.ID, RepositoryName: "queued", SourceURL: "https://example.test/q.git",
	})
	running := st.CreateRepositoryMigration(NewRepositoryMigration{
		OwnerOrgID: org.ID, RepositoryName: "running", SourceURL: "https://example.test/r.git",
	})
	elsewhere := st.CreateRepositoryMigration(NewRepositoryMigration{
		OwnerOrgID: other.ID, RepositoryName: "elsewhere", SourceURL: "https://example.test/e.git",
	})
	st.ClaimRepositoryMigration(running.ID)

	if aborted := st.AbortQueuedRepositoryMigrations(org.ID, "operator aborted the queue"); aborted != 1 {
		t.Fatalf("aborted %d migrations, want the one that was still queued", aborted)
	}
	if got := st.GetRepositoryMigration(queued.ID); got.State != GEIMigrationStateFailed {
		t.Errorf("the queued migration is %s, want FAILED", got.State)
	}
	if got := st.GetRepositoryMigration(running.ID); got.State != GEIMigrationStateInProgress {
		t.Errorf("the running migration is %s, want it left alone", got.State)
	}
	// Another organization's queue is untouched: the abort names an owner.
	if got := st.GetRepositoryMigration(elsewhere.ID); got.State != GEIMigrationStateQueued {
		t.Errorf("another organization's queue is %s, want QUEUED", got.State)
	}
}

func TestUserHoldsOrgMigratorRoleIsPerOrganization(t *testing.T) {
	st := NewStore()
	org := newGEITestOrg(st, "granting")
	other := newGEITestOrg(st, "unrelated")
	user := newEnterpriseTestUser(st, "migrator")

	if st.UserHoldsOrgMigratorRole(org.ID, user) {
		t.Fatal("an ungranted account holds the migrator role")
	}
	if !st.SetOrgMigratorRole(org.ID, "USER", user.Login, 0, true) {
		t.Fatal("the grant did not change the grant set")
	}
	if st.SetOrgMigratorRole(org.ID, "USER", user.Login, 0, true) {
		t.Fatal("re-granting an existing grant reported a change")
	}
	if !st.UserHoldsOrgMigratorRole(org.ID, user) {
		t.Fatal("the granted account does not hold the migrator role")
	}
	// A grant on one organization is nothing on another — the property the
	// whole authorization surface rests on.
	if st.UserHoldsOrgMigratorRole(other.ID, user) {
		t.Fatal("a migrator on one organization is a migrator on another")
	}
	if !st.SetOrgMigratorRole(org.ID, "USER", user.Login, 0, false) {
		t.Fatal("the revoke did not change the grant set")
	}
	if st.UserHoldsOrgMigratorRole(org.ID, user) {
		t.Fatal("the revoked account still holds the migrator role")
	}
}

func TestListUnfinishedGEIMigrationsReturnsOnlyResumableWork(t *testing.T) {
	st := NewStore()
	org := newGEITestOrg(st, "resumeorg")
	pending := st.CreateRepositoryMigration(NewRepositoryMigration{
		OwnerOrgID: org.ID, RepositoryName: "pending", SourceURL: "https://example.test/p.git",
	})
	finished := st.CreateRepositoryMigration(NewRepositoryMigration{
		OwnerOrgID: org.ID, RepositoryName: "finished", SourceURL: "https://example.test/f.git",
	})
	st.ClaimRepositoryMigration(finished.ID)
	st.SetRepositoryMigrationState(finished.ID, GEIMigrationStateSucceeded, "")

	unfinished := st.ListUnfinishedRepositoryMigrations()
	if len(unfinished) != 1 || unfinished[0] != pending.ID {
		t.Fatalf("unfinished repository migrations = %v, want only %d", unfinished, pending.ID)
	}

	orgMigration := st.CreateOrganizationMigration(1, "https://example.test/acme", "acme", "acme-here", "token", 0)
	if got := st.ListUnfinishedOrganizationMigrations(); len(got) != 1 || got[0] != orgMigration.ID {
		t.Fatalf("unfinished organization migrations = %v, want only %d", got, orgMigration.ID)
	}
	st.ClaimOrganizationMigration(orgMigration.ID)
	st.UpdateOrganizationMigration(orgMigration.ID, func(m *OrganizationMigration) {
		m.State = GEIMigrationStateSucceeded
	})
	if got := st.ListUnfinishedOrganizationMigrations(); len(got) != 0 {
		t.Fatalf("a finished organization migration is still resumable: %v", got)
	}
}

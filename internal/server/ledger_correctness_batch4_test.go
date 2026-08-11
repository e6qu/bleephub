package bleephub

import (
	"testing"
)

// TestCopilotSeatReadsAreNonMutating covers the copilot half of STORE-034: the
// seat GET/List endpoints used to take the exclusive lock and perform a durable
// delete of expired seats, so a read was non-idempotent and a persist failure
// during a GET faulted the request. Reads now filter expired seats in memory
// (under a read lock) and leave the durable prune to the write paths.
func TestCopilotSeatReadsAreNonMutating(t *testing.T) {
	s := newTestServer()
	st := s.store
	admin := st.UsersByLogin["admin"]
	org := st.CreateOrg(admin, "copilot-org", "Copilot Org", "")

	st.AddCopilotSeats(org.Login, []int{admin.ID}, "")

	// Force the seat past its cancellation date.
	st.Mu.Lock()
	st.CopilotSeats[org.Login][admin.ID].PendingCancellationDate = "2000-01-01"
	st.Mu.Unlock()

	// A read reports it absent but must NOT delete it (idempotent read).
	if seat := st.GetCopilotSeat(org.Login, admin.ID); seat != nil {
		t.Fatal("expired seat should read as absent")
	}
	if seats := st.ListCopilotSeats(org.Login); len(seats) != 0 {
		t.Fatalf("expired seat listed: %d seats", len(seats))
	}
	st.Mu.RLock()
	_, stillPresent := st.CopilotSeats[org.Login][admin.ID]
	st.Mu.RUnlock()
	if !stillPresent {
		t.Fatal("STORE-034: a copilot seat read physically deleted the seat")
	}

	// A write path prunes the expired seat durably.
	st.AddCopilotSeats(org.Login, nil, "")
	st.Mu.RLock()
	_, prunedAway := st.CopilotSeats[org.Login][admin.ID]
	st.Mu.RUnlock()
	if prunedAway {
		t.Fatal("the seat write path did not prune the expired seat")
	}
}

// TestRepoDeletePurgesSoftDeletedPackages covers the package half of STORE-028:
// the repo-delete cascade iterated the PackagesByOwnerKey secondary index, which
// a soft-delete prunes while keeping the row in st.Packages — so a soft-deleted
// package's rows (and file bytes) were orphaned when its repo was deleted. The
// cascade now iterates the authoritative st.Packages filtered by owner.
func TestRepoDeletePurgesSoftDeletedPackages(t *testing.T) {
	s := newTestServer()
	st := s.store
	admin := st.UsersByLogin["admin"]
	repo := st.CreateRepo(admin, "pkg-repo", "", false)

	pkg, created := st.CreatePackage("Repository", repo.FullName, "container", "app", "private")
	if !created || pkg == nil {
		t.Fatal("package not created")
	}

	// Soft-delete: removed from the secondary index, retained in st.Packages.
	if !st.DeletePackage(repo.FullName, "container", "app") {
		t.Fatal("soft-delete failed")
	}
	st.Mu.RLock()
	_, inPrimary := st.Packages[pkg.ID]
	_, inIndex := st.PackagesByOwnerKey[repo.FullName][packageKey("container", "app")]
	st.Mu.RUnlock()
	if !inPrimary || inIndex {
		t.Fatalf("soft-delete precondition wrong: primary=%v index=%v", inPrimary, inIndex)
	}

	if ok, err := st.DeleteRepo(admin.Login, "pkg-repo"); !ok || err != nil {
		t.Fatalf("delete repo: ok=%v err=%v", ok, err)
	}

	st.Mu.RLock()
	_, stillOrphaned := st.Packages[pkg.ID]
	st.Mu.RUnlock()
	if stillOrphaned {
		t.Fatal("STORE-028: a soft-deleted package survived repo deletion as an owner-less row")
	}
}

package bleephub

import "testing"

// TestListDeploymentsDeterministicOrder covers the STORE pagination fix (P7).
// The reload path repopulates DeploymentStore.byRepo in arbitrary
// map-iteration order, so ListDeployments must impose its own deterministic,
// GitHub-faithful order (most-recent first, highest ID) rather than returning
// the raw slice. Here byRepo is deliberately scrambled to mimic a post-reload
// ordering; the returned slice must still be strictly descending by ID.
func TestListDeploymentsDeterministicOrder(t *testing.T) {
	ds := newTestServer().store.Deployments
	const repoID = 4242

	ids := make([]int, 0, 6)
	for i := 0; i < 6; i++ {
		d := ds.CreateDeployment(repoID, 1, "main", "sha", "deploy", "production", "", nil, false, false)
		ids = append(ids, d.ID)
	}

	// Simulate a reload: byRepo comes back in arbitrary order. Apply a fixed
	// non-sorted permutation so the test does not depend on map iteration.
	ds.Mu.Lock()
	src := ds.ByRepo[repoID]
	perm := []int{2, 5, 0, 3, 1, 4}
	scrambled := make([]*Deployment, len(src))
	for i, p := range perm {
		scrambled[i] = src[p]
	}
	ds.ByRepo[repoID] = scrambled
	ds.Mu.Unlock()

	got := ds.ListDeployments(repoID)
	if len(got) != len(ids) {
		t.Fatalf("ListDeployments len = %d, want %d", len(got), len(ids))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].ID <= got[i].ID {
			t.Fatalf("deployments not in descending ID order: index %d has %d then %d", i, got[i-1].ID, got[i].ID)
		}
	}
	// Most-recent (highest ID) first.
	if got[0].ID != ids[len(ids)-1] {
		t.Fatalf("first deployment ID = %d, want most-recent %d", got[0].ID, ids[len(ids)-1])
	}
}

package bleephub

import (
	"fmt"
	"sync"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// These tests hammer the store's write-serialized invariants from many
// goroutines. They are correctness tests (run under -race in CI's full-race
// suite), complementing the throughput benchmarks: the point is that the single
// global lock keeps allocation and indexing consistent no matter the contention.

// TestConcurrentIssueAndPRNumbersAreUnique pins that the shared per-repo number
// sequence (issues and PRs draw from repo.NextIssueNumber) never hands two
// concurrent creators the same number and never skips one.
func TestConcurrentIssueAndPRNumbersAreUnique(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repoKey := s.createTestRepo(t)
	repo := s.store.GetRepoByFullName(repoKey.fullName())
	seedPullRequestBranches(t, s.Server, repo, "a", "b", "c", "d", "e", "f", "g", "h")

	const issueWorkers, prWorkers, per = 8, 8, 25
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[int]string{}
	record := func(kind string, number int) {
		mu.Lock()
		defer mu.Unlock()
		if prev, dup := seen[number]; dup {
			t.Errorf("number %d handed to both %s and %s", number, prev, kind)
		}
		seen[number] = kind
	}

	for w := 0; w < issueWorkers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				iss := s.store.CreateIssue(repo.ID, admin.ID, fmt.Sprintf("i-%d-%d", w, i), "", nil, nil, 0)
				if iss != nil {
					record("issue", iss.Number)
				}
			}
		}(w)
	}
	branches := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for w := 0; w < prWorkers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// One PR per branch per worker would collide on the open-PR invariant;
			// instead each worker creates issues via the PR path is not possible, so
			// create a handful of PRs on distinct head branches sequentially.
			pr := s.store.CreatePullRequest(repo.ID, admin.ID, fmt.Sprintf("pr-%d", w), "", branches[w%len(branches)], "main", false, nil, nil, 0)
			if pr != nil {
				record("pull_request", pr.Number)
			}
		}(w)
	}
	wg.Wait()

	// Numbers must be a gap-free 1..N over everything the shared counter minted.
	if len(seen) == 0 {
		t.Fatal("no issues or PRs were created")
	}
	for n := 1; n <= len(seen); n++ {
		if _, ok := seen[n]; !ok {
			t.Fatalf("number sequence has a gap at %d (allocated %d total)", n, len(seen))
		}
	}
	assertStoreIndexInvariants(t, s.store)
}

// TestConcurrentMergeQueueEnqueuePositions pins that concurrent enqueues onto one
// base branch produce a dense, unique 1..N position ordering with no two entries
// sharing a slot.
func TestConcurrentMergeQueueEnqueuePositions(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repoKey := s.createTestRepo(t)
	repo := s.store.GetRepoByFullName(repoKey.fullName())

	const n = 24
	branches := make([]string, n)
	for i := range branches {
		branches[i] = fmt.Sprintf("mq-%d", i)
	}
	seedPullRequestBranches(t, s.Server, repo, branches...)
	prs := make([]*store.PullRequest, 0, n)
	for i := 0; i < n; i++ {
		pr := s.store.CreatePullRequest(repo.ID, admin.ID, fmt.Sprintf("mq pr %d", i), "", branches[i], "main", false, nil, nil, 0)
		if pr == nil {
			t.Fatalf("create PR %d failed", i)
		}
		prs = append(prs, pr)
	}

	var wg sync.WaitGroup
	for _, pr := range prs {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			s.store.EnqueuePullRequest(id, false)
		}(pr.ID)
	}
	wg.Wait()

	entries := s.store.MergeQueuePullRequests(repo.ID, "main")
	positions := map[int]bool{}
	for _, e := range entries {
		if e.MergeQueuePosition < 1 || positions[e.MergeQueuePosition] {
			t.Fatalf("duplicate or invalid queue position %d", e.MergeQueuePosition)
		}
		positions[e.MergeQueuePosition] = true
	}
	if len(entries) != n {
		t.Fatalf("queued %d entries, want %d", len(entries), n)
	}
	for p := 1; p <= n; p++ {
		if !positions[p] {
			t.Fatalf("queue positions are not dense: missing %d", p)
		}
	}
}

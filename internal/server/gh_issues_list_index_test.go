package bleephub

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// listIssuesViaHandler drives the real repo issues listing and returns the
// issue/PR numbers in response order.
func listIssuesViaHandler(t *testing.T, s *Server, owner, repo, query string) []int {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v3/repos/"+owner+"/"+repo+"/issues?"+query, nil)
	req.SetPathValue("owner", owner)
	req.SetPathValue("repo", repo)
	rec := httptest.NewRecorder()
	s.handleListIssues(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET issues?%s: status = %d, body = %s", query, rec.Code, rec.Body.String())
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("GET issues?%s: decode body: %v", query, err)
	}
	numbers := make([]int, 0, len(rows))
	for _, row := range rows {
		numbers = append(numbers, int(row["number"].(float64)))
	}
	return numbers
}

// TestIssueOrderIndexStaysConsistentAcrossLifecycle exercises the per-repo
// creation-order index through the issue lifecycle: creation (including one
// created "in the past", which must binary-insert mid-slice), close, reopen,
// and the repo-delete cascade. The order index must always agree with the
// per-repo number index, hand out detached snapshots (STORE-021), and vanish
// with the repo.
func TestIssueOrderIndexStaysConsistentAcrossLifecycle(t *testing.T) {
	s := newTestServer()
	current := fixedTestTime
	var clockMu sync.Mutex
	s.replaceClockNow(func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return current
	})
	advance := func(d time.Duration) {
		clockMu.Lock()
		current = current.Add(d)
		clockMu.Unlock()
	}
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "order-index", "", false)

	// Issues 1-3 share one creation instant (the number is the tie-break),
	// 4 and 5 are later, and 6 is created with the clock wound back between
	// 4 and 5 so it must insert mid-slice.
	for i := 0; i < 3; i++ {
		s.store.CreateIssue(repo.ID, admin.ID, fmt.Sprintf("issue %d", i+1), "", nil, nil, 0)
	}
	advance(40 * time.Second)
	s.store.CreateIssue(repo.ID, admin.ID, "issue 4", "", nil, nil, 0)
	advance(10 * time.Second)
	s.store.CreateIssue(repo.ID, admin.ID, "issue 5", "", nil, nil, 0)
	advance(-5 * time.Second)
	s.store.CreateIssue(repo.ID, admin.ID, "issue 6", "", nil, nil, 0)

	numbersOf := func(issues []*store.Issue) []int {
		numbers := make([]int, 0, len(issues))
		for _, issue := range issues {
			numbers = append(numbers, issue.Number)
		}
		return numbers
	}
	assertOrder := func(state string, desc bool, want ...int) {
		t.Helper()
		got := numbersOf(s.store.ListIssuesOrderedByCreation(repo.ID, state, desc))
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("ListIssuesOrderedByCreation(%q, desc=%v) = %v, want %v", state, desc, got, want)
		}
	}

	assertOrder("all", false, 1, 2, 3, 4, 6, 5)
	assertOrder("all", true, 5, 6, 4, 3, 2, 1)

	// Close 2 and 4, reopen 2: state transitions must not disturb the order.
	closeIssue := func(number int, state string) {
		issue := s.store.GetIssueByNumber(repo.ID, number)
		s.store.UpdateIssue(issue.ID, func(mut *store.Issue) { mut.State = state })
	}
	closeIssue(2, "CLOSED")
	closeIssue(4, "CLOSED")
	closeIssue(2, "OPEN")
	assertOrder("all", false, 1, 2, 3, 4, 6, 5)
	assertOrder("OPEN", false, 1, 2, 3, 6, 5)
	assertOrder("CLOSED", true, 4)

	// The order index and the number index must hold exactly the same rows.
	s.store.Mu.RLock()
	byNumber := s.store.IssuesByRepo[repo.ID]
	ordered := s.store.IssueOrderByRepo[repo.ID]
	if len(ordered) != len(byNumber) {
		s.store.Mu.RUnlock()
		t.Fatalf("order index holds %d issues, number index holds %d", len(ordered), len(byNumber))
	}
	for _, issue := range ordered {
		if byNumber[issue.Number] != issue {
			s.store.Mu.RUnlock()
			t.Fatalf("order index row #%d is not the row the number index holds", issue.Number)
		}
	}
	s.store.Mu.RUnlock()

	// Snapshots are detached: scribbling on a result must not reach the store.
	listed := s.store.ListIssuesOrderedByCreation(repo.ID, "all", false)
	listed[0].Title = "scribbled"
	listed[0].LabelIDs = append(listed[0].LabelIDs, 999)
	if got := s.store.GetIssueByNumber(repo.ID, listed[0].Number); got.Title == "scribbled" || len(got.LabelIDs) != 0 {
		t.Fatalf("mutating a listed snapshot leaked into the store: %+v", got)
	}

	// Deleting the repo unindexes every issue and drops the per-repo slice.
	if ok, err := s.store.DeleteRepo("admin", "order-index"); !ok || err != nil {
		t.Fatalf("DeleteRepo = %v, %v", ok, err)
	}
	s.store.Mu.RLock()
	_, stillIndexed := s.store.IssueOrderByRepo[repo.ID]
	s.store.Mu.RUnlock()
	if stillIndexed {
		t.Fatal("order index still holds the deleted repo")
	}
	if got := s.store.ListIssuesOrderedByCreation(repo.ID, "all", false); len(got) != 0 {
		t.Fatalf("deleted repo still lists %d issues", len(got))
	}
}

// TestListIssuesIndexPathMatchesNaiveScan is the A/B correctness check for
// the indexed listing: for every supported sort, direction, and state filter
// the handler's order must equal an independently computed naive
// scan-and-sort over the same seeded data — issues and pull requests mixed,
// with duplicate timestamps and comment counts to exercise the number
// tie-break.
func TestListIssuesIndexPathMatchesNaiveScan(t *testing.T) {
	s := newTestServer()
	current := fixedTestTime
	var clockMu sync.Mutex
	s.replaceClockNow(func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return current
	})
	advance := func(d time.Duration) {
		clockMu.Lock()
		current = current.Add(d)
		clockMu.Unlock()
	}
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "ab-scan", "", false)

	// 12 issues: clustered creation times, one wound-back creation, a spread
	// of comment counts, some closed, and staggered updates.
	var issueIDs []int
	for i := 0; i < 12; i++ {
		if i%4 == 0 {
			advance(30 * time.Second)
		}
		if i == 9 {
			advance(-45 * time.Second)
		}
		issue := s.store.CreateIssue(repo.ID, admin.ID, fmt.Sprintf("issue %d", i+1), "", nil, nil, 0)
		issueIDs = append(issueIDs, issue.ID)
		for c := 0; c < i%3; c++ {
			s.store.CreateComment(issue.ID, admin.ID, "hm")
		}
	}
	for i, id := range issueIDs {
		if i%3 == 1 {
			advance(7 * time.Second)
			s.store.UpdateIssue(id, func(mut *store.Issue) { mut.State = "CLOSED" })
		}
	}

	// 3 pull requests (open, closed, open) with comments — the listing merges
	// them into the same rows. CreatePullRequest resolves real git branches,
	// so seed the rows directly under the store lock, the way
	// store_map_access_race_test.go seeds its fixtures.
	for i := 0; i < 3; i++ {
		advance(11 * time.Second)
		now := s.store.CurrentTime()
		state := "OPEN"
		if i == 1 {
			state = "CLOSED"
		}
		s.store.Mu.Lock()
		repoRow := s.store.Repos[repo.ID]
		pr := &store.PullRequest{
			ID:          s.store.NextPR,
			NodeID:      fmt.Sprintf("PR_kgDO%08d", s.store.NextPR),
			Number:      repoRow.NextIssueNumber, // shared issue/PR sequence
			RepoID:      repo.ID,
			Title:       fmt.Sprintf("pr %d", i+1),
			State:       state,
			AuthorID:    admin.ID,
			AssigneeIDs: []int{},
			LabelIDs:    []int{},
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		repoRow.NextIssueNumber++
		s.store.NextPR++
		s.store.PullRequests[pr.ID] = pr
		s.store.IndexPullLocked(pr)
		s.store.Mu.Unlock()
		for c := 0; c <= i; c++ {
			s.store.CreateCommentFor("pull_request", pr.ID, admin.ID, "hm")
		}
	}

	// The naive reference: a flat scan of both stores into rows, filtered and
	// sorted in the test with the documented semantics.
	type refRow struct {
		number   int
		state    string
		created  time.Time
		updated  time.Time
		comments int
	}
	var reference []refRow
	for _, issue := range s.store.ListIssues(repo.ID, "all") {
		reference = append(reference, refRow{issue.Number, issue.State, issue.CreatedAt, issue.UpdatedAt,
			s.store.CountCommentsFor("issue", issue.ID)})
	}
	for _, pr := range s.store.ListPullRequests(repo.ID, "all") {
		reference = append(reference, refRow{pr.Number, pr.State, pr.CreatedAt, pr.UpdatedAt,
			s.store.CountCommentsFor("pull_request", pr.ID)})
	}

	naive := func(state, sortField, direction string) []int {
		rows := make([]refRow, 0, len(reference))
		for _, row := range reference {
			switch state {
			case "open":
				if row.state != "OPEN" {
					continue
				}
			case "closed":
				if row.state != "CLOSED" && row.state != "MERGED" {
					continue
				}
			}
			rows = append(rows, row)
		}
		sort.Slice(rows, func(i, j int) bool {
			var less bool
			switch sortField {
			case "updated":
				less = rows[i].updated.Before(rows[j].updated)
				if rows[i].updated.Equal(rows[j].updated) {
					less = rows[i].number < rows[j].number
				}
			case "comments":
				less = rows[i].comments < rows[j].comments
				if rows[i].comments == rows[j].comments {
					less = rows[i].number < rows[j].number
				}
			default:
				less = rows[i].created.Before(rows[j].created)
				if rows[i].created.Equal(rows[j].created) {
					less = rows[i].number < rows[j].number
				}
			}
			if direction == "desc" {
				return !less && rows[i].number != rows[j].number
			}
			return less
		})
		numbers := make([]int, 0, len(rows))
		for _, row := range rows {
			numbers = append(numbers, row.number)
		}
		return numbers
	}

	for _, state := range []string{"open", "closed", "all"} {
		for _, sortField := range []string{"created", "updated", "comments"} {
			for _, direction := range []string{"asc", "desc"} {
				query := fmt.Sprintf("state=%s&sort=%s&direction=%s&per_page=100", state, sortField, direction)
				got := listIssuesViaHandler(t, s, "admin", "ab-scan", query)
				want := naive(state, sortField, direction)
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("%s: handler = %v, naive scan = %v", query, got, want)
				}
			}
		}
	}

	// Pagination semantics ride on the same rows: page 2 of 5 must be rows
	// 5-9 of the naive order.
	got := listIssuesViaHandler(t, s, "admin", "ab-scan", "state=all&per_page=5&page=2")
	want := naive("all", "created", "desc")[5:10]
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("page 2 = %v, want %v", got, want)
	}
}

// TestIssueOrderIndexAccessIsRaceFree drives concurrent issue creation and
// state updates against the ordered listing, in the bounded pattern of
// store_map_access_race_test.go: the -race detector proves the index reads
// take the store lock; the final assertions keep the test meaningful
// without it.
func TestIssueOrderIndexAccessIsRaceFree(t *testing.T) {
	s := newTestServer()
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "order-race", "", false)

	const writes = 500
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; i < writes; i++ {
			issue := s.store.CreateIssue(repo.ID, admin.ID, fmt.Sprintf("race %d", i), "", nil, nil, 0)
			if i%2 == 0 {
				s.store.UpdateIssue(issue.ID, func(mut *store.Issue) { mut.State = "CLOSED" })
			}
		}
	}()
	for i := 0; i < writes; i++ {
		_ = s.store.ListIssuesOrderedByCreation(repo.ID, "all", true)
		_ = s.store.ListIssuesOrderedByCreation(repo.ID, "OPEN", false)
	}
	writers.Wait()

	issues := s.store.ListIssuesOrderedByCreation(repo.ID, "all", false)
	if len(issues) != writes {
		t.Fatalf("final listing holds %d issues, want %d", len(issues), writes)
	}
	for i := 1; i < len(issues); i++ {
		previous, next := issues[i-1], issues[i]
		if next.CreatedAt.Before(previous.CreatedAt) ||
			(next.CreatedAt.Equal(previous.CreatedAt) && next.Number < previous.Number) {
			t.Fatalf("listing out of order at %d: #%d then #%d", i, previous.Number, next.Number)
		}
	}
}

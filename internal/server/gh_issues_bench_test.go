package bleephub

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// seedIssueListBenchServer builds an in-memory server whose one repository
// carries issueCount issues (every third closed) so the repo issues listing
// can be measured at a realistic scale.
func seedIssueListBenchServer(b *testing.B, issueCount int) *Server {
	b.Helper()
	s := newTestServer()
	admin := s.store.LookupUserByLogin("admin")
	if admin == nil {
		b.Fatal("seeded admin user missing")
	}
	repo := s.store.CreateRepo(admin, "bench-issues", "", false)
	if repo == nil {
		b.Fatal("bench repo was not created")
	}
	for i := 0; i < issueCount; i++ {
		issue := s.store.CreateIssue(repo.ID, admin.ID, fmt.Sprintf("issue %d", i), "body", nil, nil, 0)
		if issue == nil {
			b.Fatalf("issue %d was not created", i)
		}
		if i%3 == 0 {
			s.store.UpdateIssue(issue.ID, func(mut *store.Issue) {
				mut.State = "CLOSED"
			})
		}
	}
	return s
}

// benchListIssues drives the real handler with the default page size, the
// exact request shape the SPA and API clients issue.
func benchListIssues(b *testing.B, s *Server, query string) {
	b.Helper()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("GET", "/api/v3/repos/admin/bench-issues/issues?"+query, nil)
		req.SetPathValue("owner", "admin")
		req.SetPathValue("repo", "bench-issues")
		rec := httptest.NewRecorder()
		s.handleListIssues(rec, req)
		if rec.Code != 200 {
			b.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}
}

func BenchmarkListRepoIssuesDefaultSort10k(b *testing.B) {
	s := seedIssueListBenchServer(b, 10_000)
	b.ResetTimer()
	benchListIssues(b, s, "state=all")
}

func BenchmarkListRepoIssuesUpdatedSort10k(b *testing.B) {
	s := seedIssueListBenchServer(b, 10_000)
	b.ResetTimer()
	benchListIssues(b, s, "state=all&sort=updated&direction=asc")
}

func BenchmarkListRepoIssuesCommentsSort10k(b *testing.B) {
	s := seedIssueListBenchServer(b, 10_000)
	b.ResetTimer()
	benchListIssues(b, s, "state=all&sort=comments")
}

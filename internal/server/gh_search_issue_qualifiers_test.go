package bleephub

import (
	"testing"
	"time"
)

// TestSearchIssuesTemporalAndDraftQualifiers covers the issue-search qualifiers
// GitHub documents but bleephub used to 422 on: created:/updated:/closed: date
// filters and draft:. A routine query like `is:issue created:>2023-01-01` must
// filter rather than be rejected.
func TestSearchIssuesTemporalAndDraftQualifiers(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "sq", "", false)
	if repo == nil {
		t.Fatal("create repo failed")
	}

	old := s.store.CreateIssue(repo.ID, admin.ID, "old bug", "", nil, nil, 0)
	recent := s.store.CreateIssue(repo.ID, admin.ID, "new bug", "", nil, nil, 0)
	s.store.Mu.Lock()
	old.CreatedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	old.UpdatedAt = old.CreatedAt
	recent.CreatedAt = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	recent.UpdatedAt = recent.CreatedAt
	s.store.Mu.Unlock()

	// created:>2023-01-01 selects only the recent issue.
	if got := searchIssueTitles(t, s, "repo:admin/sq created:>2023-01-01"); len(got) != 1 || got[0] != "new bug" {
		t.Errorf("created:>2023 titles = %v, want [new bug]", got)
	}
	// created:<2023-01-01 selects only the old one.
	if got := searchIssueTitles(t, s, "repo:admin/sq created:<2023-01-01"); len(got) != 1 || got[0] != "old bug" {
		t.Errorf("created:<2023 titles = %v, want [old bug]", got)
	}
	// draft:true matches nothing — plain issues are never drafts.
	if got := searchIssueTitles(t, s, "repo:admin/sq draft:true"); len(got) != 0 {
		t.Errorf("draft:true titles = %v, want none", got)
	}
	// archived:false matches both (the repo is not archived).
	if got := searchIssueTitles(t, s, "repo:admin/sq archived:false"); len(got) != 2 {
		t.Errorf("archived:false count = %d, want 2", len(got))
	}
}

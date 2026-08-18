package bleephub

import (
	"encoding/json"
	"net/url"
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

// TestSearchIssuesReactionsRollupAndSort covers the reaction-rollup on issue/PR
// search items (issue-search-result-item.reactions) and the sort=reactions and
// sort=interactions ordering GitHub documents but bleephub silently ignored.
func TestSearchIssuesReactionsRollupAndSort(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "rx", "", false)
	low := s.store.CreateIssue(repo.ID, admin.ID, "low reactions", "marker-rx", nil, nil, 0)
	high := s.store.CreateIssue(repo.ID, admin.ID, "high reactions", "marker-rx", nil, nil, 0)
	_ = low
	// 'high' gets two reactions; 'low' none.
	if _, _, err := s.store.Reactions.AddReaction("issue", high.ID, admin.ID, "heart"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.store.Reactions.AddReaction("issue", high.ID, admin.ID, "+1"); err != nil {
		t.Fatal(err)
	}

	type item struct {
		Title     string `json:"title"`
		Reactions struct {
			TotalCount int `json:"total_count"`
		} `json:"reactions"`
	}
	fetch := func(sort string) []item {
		resp := s.authedGet(t, "/api/v3/search/issues?"+url.Values{
			"q": {"marker-rx repo:admin/rx"}, "sort": {sort}, "order": {"desc"},
		}.Encode())
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("search sort=%s status %d", sort, resp.StatusCode)
		}
		var env struct {
			Items []item `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatal(err)
		}
		return env.Items
	}

	// sort=reactions desc puts the two-reaction issue first, and the rollup is
	// present on the items.
	items := fetch("reactions")
	if len(items) != 2 {
		t.Fatalf("reactions sort items = %d, want 2", len(items))
	}
	if items[0].Title != "high reactions" || items[0].Reactions.TotalCount != 2 {
		t.Errorf("reactions sort head = %q (total %d), want high reactions (2)", items[0].Title, items[0].Reactions.TotalCount)
	}
	if items[1].Reactions.TotalCount != 0 {
		t.Errorf("second item reaction total = %d, want 0", items[1].Reactions.TotalCount)
	}
	// sort=interactions (reactions + comments) likewise ranks 'high' first.
	if inter := fetch("interactions"); inter[0].Title != "high reactions" {
		t.Errorf("interactions sort head = %q, want high reactions", inter[0].Title)
	}
}

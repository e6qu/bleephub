package bleephub

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func achievementsTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer()
	s.registerGHAchievementsRoutes()
	return s
}

func TestUserAchievements_PullSharkYoloQuickdraw(t *testing.T) {
	s := achievementsTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	reviewer := seedTestUser(s, "ach-reviewer")
	repo := s.store.CreateRepo(admin, "ach", "", false)
	if repo == nil {
		t.Fatal("failed to create repo")
	}

	seedPullRequestBranches(t, s, repo, "feat-1", "feat-2")
	// A Co-authored-by trailer on the PR-1 branch → pair-extraordinaire.
	stor := s.store.GetGitStorage("admin", "ach")
	sig := repoSignature("admin", "admin@bleephub.local")
	if _, err := createFileCommit(stor, "feat-1", "pair.txt", "together\n",
		"Add pair work\n\nCo-authored-by: Reviewer <reviewer@example.com>\n", sig); err != nil {
		t.Fatalf("co-authored commit: %v", err)
	}

	mergedAt := fixedTestTime
	openedAt := fixedTestTime.Add(-time.Hour) // an hour before close: no quickdraw from the PRs

	// PR 1: merged with zero APPROVED reviews → counts for pull-shark and YOLO.
	pr1 := s.store.CreatePullRequest(repo.ID, admin.ID, "one", "", "feat-1", "main", false, nil, nil, 0)
	if pr1 == nil {
		t.Fatal("failed to create pr1")
	}
	s.store.UpdatePullRequest(pr1.ID, func(p *store.PullRequest) {
		p.State = "MERGED"
		p.CreatedAt = openedAt
		p.MergedAt = &mergedAt
		p.ClosedAt = &mergedAt
	})

	// PR 2: merged with an APPROVED review submitted at merge time → pull-shark
	// only, not YOLO.
	pr2 := s.store.CreatePullRequest(repo.ID, admin.ID, "two", "", "feat-2", "main", false, nil, nil, 0)
	if pr2 == nil {
		t.Fatal("failed to create pr2")
	}
	if review := s.store.CreatePullRequestReview("admin/ach", pr2.Number, reviewer.ID, "lgtm", "APPROVED"); review == nil {
		t.Fatal("failed to create review")
	}
	s.store.UpdatePullRequest(pr2.ID, func(p *store.PullRequest) {
		p.State = "MERGED"
		p.CreatedAt = openedAt
		p.MergedAt = &mergedAt
		p.ClosedAt = &mergedAt
	})

	// An issue the user opened and that closed 3 minutes later → quickdraw.
	issue := s.store.CreateIssue(repo.ID, admin.ID, "fast", "", nil, nil, 0)
	if issue == nil {
		t.Fatal("failed to create issue")
	}
	s.store.UpdateIssue(issue.ID, func(i *store.Issue) {
		i.State = "CLOSED"
		closedAt := i.CreatedAt.Add(3 * time.Minute)
		i.ClosedAt = &closedAt
	})

	w := serveTestRequest(s, bearerHeader(adminPAT), "GET", "/ui-data/users/admin/achievements", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got []profileAchievement
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body = %s)", err, w.Body.String())
	}
	want := []profileAchievement{
		{Slug: "pull-shark", Name: "Pull Shark", Tier: 1, Count: 2},
		{Slug: "pair-extraordinaire", Name: "Pair Extraordinaire", Tier: 1, Count: 1},
		{Slug: "yolo", Name: "YOLO", Tier: 1, Count: 1},
		{Slug: "quickdraw", Name: "Quickdraw", Tier: 1, Count: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("achievements = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("achievements[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestUserAchievements_GalaxyBrainAndStarstruckTiers(t *testing.T) {
	s := achievementsTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	asker := seedTestUser(s, "ach-asker")
	repo := s.store.CreateRepo(admin, "answers", "", false)
	if repo == nil {
		t.Fatal("failed to create repo")
	}

	// 8 accepted answers → galaxy-brain tier 2 (thresholds 2/8/16/32).
	category := s.store.CreateDiscussionCategory(repo.ID, "Q&A2", ":question:", "", true)
	for i := 0; i < 8; i++ {
		d := s.store.CreateDiscussion(repo.ID, category.ID, asker.ID, "q", "?")
		if d == nil {
			t.Fatal("failed to create discussion")
		}
		c := s.store.CreateDiscussionComment(d.ID, admin.ID, "answer", 0)
		if c == nil || !s.store.MarkDiscussionCommentAsAnswer(c.ID) {
			t.Fatal("failed to create/mark answer")
		}
	}

	// A 128-star owned repo → starstruck tier 2 (thresholds 16/128/512/4096).
	s.store.Mu.Lock()
	s.store.Repos[repo.ID].StargazersCount = 128
	s.store.Mu.Unlock()

	w := serveTestRequest(s, bearerHeader(adminPAT), "GET", "/ui-data/users/admin/achievements", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got []profileAchievement
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []profileAchievement{
		{Slug: "galaxy-brain", Name: "Galaxy Brain", Tier: 2, Count: 8},
		{Slug: "starstruck", Name: "Starstruck", Tier: 2, Count: 128},
	}
	if len(got) != len(want) {
		t.Fatalf("achievements = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("achievements[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestUserAchievements_EmptyUserIs200EmptyList(t *testing.T) {
	s := achievementsTestServer(t)
	seedTestUser(s, "ach-nobody")

	w := serveTestRequest(s, bearerHeader(adminPAT), "GET", "/ui-data/users/ach-nobody/achievements", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (an empty achievements list must not be an error); body = %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); body != "[]\n" && body != "[]" {
		t.Fatalf("body = %q, want an empty JSON array", body)
	}
}

func TestUserAchievements_UnknownUserIs404(t *testing.T) {
	s := achievementsTestServer(t)
	w := serveTestRequest(s, bearerHeader(adminPAT), "GET", "/ui-data/users/ghost/achievements", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

package bleephub

import (
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// firstDiscussionCategory returns one of the repo's seeded default categories.
func firstDiscussionCategory(t *testing.T, s *isolatedServer, repo repoRef) *store.DiscussionCategory {
	t.Helper()
	r := s.store.GetRepo(repo.owner, repo.name)
	if r == nil {
		t.Fatalf("repo %s not found", repo.fullName())
	}
	cats := s.store.ListDiscussionCategories(r.ID)
	if len(cats) == 0 {
		t.Fatalf("repo %s has no discussion categories", repo.fullName())
	}
	return cats[0]
}

func TestUIConvertIssueToDiscussion(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)
	_, number := s.createIssueForTest(t, repo, "convert me")
	issueBody := "the issue body"
	decodeJSONWithStatus(t, s.patch(t, repo.path()+"/issues/"+itoa(number), defaultToken,
		map[string]interface{}{"body": issueBody}), http.StatusOK)

	// Two conversation comments, the second by another user, so the conversion
	// must carry both authors and their original timestamps.
	mustPost(t, s.post(t, repo.path()+"/issues/"+itoa(number)+"/comments", defaultToken,
		map[string]interface{}{"body": "first comment"}))
	_, otherTok := s.newUser(t, "disc-commenter")
	mustPost(t, s.post(t, repo.path()+"/issues/"+itoa(number)+"/comments", otherTok,
		map[string]interface{}{"body": "second comment"}))
	storeRepo := s.store.GetRepo(repo.owner, repo.name)
	issue := s.store.GetIssueByNumber(storeRepo.ID, number)
	originalComments := s.store.ListCommentsFor("issue", issue.ID)
	if len(originalComments) != 2 {
		t.Fatalf("seeded %d comments, want 2", len(originalComments))
	}

	cat := firstDiscussionCategory(t, s, repo)
	uiPath := "/ui-data/repos/" + repo.fullName() + "/issues/" + itoa(number) + "/convert-to-discussion"

	// Discussions are off by default on a fresh repo: the conversion is
	// refused until the feature is enabled.
	requireStatus(t, s.post(t, uiPath, defaultToken,
		map[string]interface{}{"category_id": cat.ID}), http.StatusUnprocessableEntity)
	decodeJSONWithStatus(t, s.patch(t, repo.path(), defaultToken,
		map[string]interface{}{"has_discussions": true}), http.StatusOK)

	created := decodeJSONWithStatus(t, s.post(t, uiPath, defaultToken,
		map[string]interface{}{"category_id": cat.ID}), http.StatusCreated)

	if created["title"] != "convert me" || created["body"] != issueBody {
		t.Errorf("discussion carries title=%v body=%v, want the issue's", created["title"], created["body"])
	}
	if user, _ := created["user"].(map[string]interface{}); user == nil || user["login"] != "admin" {
		t.Errorf("discussion user = %v, want the issue author admin", created["user"])
	}
	if category, _ := created["category"].(map[string]interface{}); category == nil || int(category["id"].(float64)) != cat.ID {
		t.Errorf("discussion category = %v, want id %d", created["category"], cat.ID)
	}
	if int(created["comments"].(float64)) != 2 {
		t.Errorf("discussion comments = %v, want 2", created["comments"])
	}

	// The carried comments keep their original authors and timestamps.
	discussionID := int(created["id"].(float64))
	carried := s.store.ListDiscussionComments(discussionID, 0)
	if len(carried) != 2 {
		t.Fatalf("discussion has %d comments, want 2", len(carried))
	}
	for i, dc := range carried {
		src := originalComments[i]
		if dc.AuthorID != src.AuthorID || dc.Body != src.Body {
			t.Errorf("comment %d = author %d body %q, want author %d body %q", i, dc.AuthorID, dc.Body, src.AuthorID, src.Body)
		}
		if !dc.CreatedAt.Equal(src.CreatedAt) {
			t.Errorf("comment %d created_at = %v, want the original %v", i, dc.CreatedAt, src.CreatedAt)
		}
	}

	// The issue is closed as not_planned with a converted_to_discussion
	// timeline event.
	issueJSON := decodeJSONWithStatus(t, s.get(t, repo.path()+"/issues/"+itoa(number), defaultToken), http.StatusOK)
	if issueJSON["state"] != "closed" || issueJSON["state_reason"] != "not_planned" {
		t.Errorf("issue state=%v state_reason=%v, want closed/not_planned", issueJSON["state"], issueJSON["state_reason"])
	}
	timeline := decodeJSONArray(t, s.get(t, repo.path()+"/issues/"+itoa(number)+"/timeline", defaultToken))
	foundEvent := false
	for _, item := range timeline {
		if item["event"] == "converted_to_discussion" {
			foundEvent = true
		}
	}
	if !foundEvent {
		t.Errorf("timeline lacks the converted_to_discussion event: %v", timeline)
	}
}

func TestUIConvertIssueToDiscussionRefusals(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.sweepRepo(t, "convert-refusals")
	decodeJSONWithStatus(t, s.patch(t, repo.path(), defaultToken,
		map[string]interface{}{"has_discussions": true}), http.StatusOK)
	_, number := s.createIssueForTest(t, repo, "stays an issue")
	prNumber, _ := s.sweepPR(t, repo, "prs cannot convert")
	cat := firstDiscussionCategory(t, s, repo)
	uiPath := func(n int) string {
		return "/ui-data/repos/" + repo.fullName() + "/issues/" + itoa(n) + "/convert-to-discussion"
	}

	// An unknown category — and a category belonging to another repo — is 422.
	requireStatus(t, s.post(t, uiPath(number), defaultToken,
		map[string]interface{}{"category_id": 999999}), http.StatusUnprocessableEntity)
	other := s.sweepRepo(t, "convert-refusals-other")
	otherCat := firstDiscussionCategory(t, s, other)
	requireStatus(t, s.post(t, uiPath(number), defaultToken,
		map[string]interface{}{"category_id": otherCat.ID}), http.StatusUnprocessableEntity)

	// A pull request number and an unknown number are 404.
	requireStatus(t, s.post(t, uiPath(prNumber), defaultToken,
		map[string]interface{}{"category_id": cat.ID}), http.StatusNotFound)
	requireStatus(t, s.post(t, uiPath(999999), defaultToken,
		map[string]interface{}{"category_id": cat.ID}), http.StatusNotFound)

	// A viewer without issue-write on the repo is refused like the resource
	// gate refuses elsewhere (the conversion closes the issue).
	_, readerTok := s.newUser(t, "convert-reader")
	requireStatus(t, s.post(t, uiPath(number), readerTok,
		map[string]interface{}{"category_id": cat.ID}), http.StatusNotFound)

	// Nothing above converted it: the issue is still open.
	issueJSON := decodeJSONWithStatus(t, s.get(t, repo.path()+"/issues/"+itoa(number), defaultToken), http.StatusOK)
	if issueJSON["state"] != "open" {
		t.Errorf("issue state = %v after refused conversions, want open", issueJSON["state"])
	}
}

func TestUIPinnedDiscussions(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t)
	storeRepo := s.store.GetRepo(repo.owner, repo.name)
	cat := firstDiscussionCategory(t, s, repo)
	admin := s.store.LookupUserByLogin("admin")

	numbers := make([]int, 0, 5)
	for i := 0; i < 5; i++ {
		d := s.store.CreateDiscussion(storeRepo.ID, cat.ID, admin.ID, "pinnable", "body")
		numbers = append(numbers, d.Number)
	}
	pinnedPath := "/ui-data/repos/" + repo.fullName() + "/discussions/pinned"

	// Empty until someone pins.
	if got := decodeJSONArrayWithStatus(t, s.get(t, pinnedPath, defaultToken), http.StatusOK); len(got) != 0 {
		t.Fatalf("pinned list starts with %d entries, want 0", len(got))
	}

	// PUT stores order; GET returns it in that order.
	want := []int{numbers[2], numbers[0], numbers[4]}
	put := decodeJSONArrayWithStatus(t, s.put(t, pinnedPath, defaultToken,
		map[string]interface{}{"numbers": want}), http.StatusOK)
	got := decodeJSONArrayWithStatus(t, s.get(t, pinnedPath, defaultToken), http.StatusOK)
	for _, listed := range [][]map[string]interface{}{put, got} {
		if len(listed) != len(want) {
			t.Fatalf("pinned list has %d entries, want %d", len(listed), len(want))
		}
		for i, row := range listed {
			if int(row["number"].(float64)) != want[i] {
				t.Errorf("pinned[%d].number = %v, want %d (order must be preserved)", i, row["number"], want[i])
			}
		}
	}

	// The cap is GitHub's four.
	requireStatus(t, s.put(t, pinnedPath, defaultToken,
		map[string]interface{}{"numbers": numbers}), http.StatusUnprocessableEntity)
	// A number that is not a discussion in this repo is 422.
	requireStatus(t, s.put(t, pinnedPath, defaultToken,
		map[string]interface{}{"numbers": []int{999999}}), http.StatusUnprocessableEntity)
	// A viewer without discussion-write cannot edit the pins (but can read).
	_, readerTok := s.newUser(t, "pin-reader")
	requireStatus(t, s.put(t, pinnedPath, readerTok,
		map[string]interface{}{"numbers": []int{numbers[0]}}), http.StatusNotFound)
	if got := decodeJSONArrayWithStatus(t, s.get(t, pinnedPath, readerTok), http.StatusOK); len(got) != len(want) {
		t.Errorf("reader sees %d pinned discussions, want %d", len(got), len(want))
	}

	// STORE-021: mutating the slice a caller handed in must not change the
	// stored pins.
	handed := []int{s.store.GetDiscussionByNumber(storeRepo.ID, numbers[1]).ID}
	s.store.SetPinnedDiscussions(storeRepo.ID, handed)
	handed[0] = 424242
	if ids := s.store.ListPinnedDiscussions(storeRepo.ID); len(ids) != 1 || ids[0] == 424242 {
		t.Errorf("stored pins follow the caller's slice mutation: %v", ids)
	}
}

// decodeJSONArrayWithStatus decodes a JSON array response after asserting its
// status code.
func decodeJSONArrayWithStatus(t *testing.T, resp *http.Response, want int) []map[string]interface{} {
	t.Helper()
	if resp.StatusCode != want {
		resp.Body.Close()
		t.Fatalf("status = %d, want %d", resp.StatusCode, want)
	}
	return decodeJSONArray(t, resp)
}

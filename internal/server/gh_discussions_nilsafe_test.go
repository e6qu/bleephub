package bleephub

import "testing"

// TestDiscussionToGQLNilDiscussionReturnsNil is the GQL-030 regression: a comment
// can outlive its soft-deleted discussion (GetDiscussion filters out Deleted
// rows and returns nil), and several callers pass a re-fetched GetDiscussion
// result straight into discussionToGQL. It must tolerate a nil discussion rather
// than panic dereferencing d.RepoID.
// TestDiscussionGetReturnsDetachedSnapshot pins STORE-021 for the discussion
// family: GetDiscussion/GetDiscussionByNumber must return copies.
func TestDiscussionGetReturnsDetachedSnapshot(t *testing.T) {
	s := newTestServer()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "disc-detach", "", false)
	cat := s.store.CreateDiscussionCategory(repo.ID, "General", "💬", "d", false)
	d := s.store.CreateDiscussion(repo.ID, cat.ID, admin.ID, "hi", "body")

	got := s.store.GetDiscussion(d.ID)
	got.Title = "hacked"
	if fresh := s.store.GetDiscussion(d.ID); fresh.Title != "hi" {
		t.Fatalf("discussion mutated through GetDiscussion: %q", fresh.Title)
	}
	byNum := s.store.GetDiscussionByNumber(repo.ID, d.Number)
	byNum.Body = "hacked"
	if fresh := s.store.GetDiscussionByNumber(repo.ID, d.Number); fresh.Body != "body" {
		t.Fatalf("discussion mutated through GetDiscussionByNumber: %q", fresh.Body)
	}
}

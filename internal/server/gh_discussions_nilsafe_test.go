package bleephub

import "testing"

// TestDiscussionToGQLNilDiscussionReturnsNil is the GQL-030 regression: a comment
// can outlive its soft-deleted discussion (GetDiscussion filters out Deleted
// rows and returns nil), and several callers pass a re-fetched GetDiscussion
// result straight into discussionToGQL. It must tolerate a nil discussion rather
// than panic dereferencing d.RepoID.
func TestDiscussionToGQLNilDiscussionReturnsNil(t *testing.T) {
	s := newTestServer()
	if got := discussionToGQL(nil, s.store); got != nil {
		t.Fatalf("discussionToGQL(nil) = %#v, want nil", got)
	}
}

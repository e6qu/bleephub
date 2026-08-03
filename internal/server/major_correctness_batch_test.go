package bleephub

import "testing"

// TestPullRequestEventPersistsWithCorrectParentAcrossReload is the STORE-039
// regression: a pull-request event must be persisted exactly once, already
// carrying ParentType "pull_request", rather than first written as an "issue"
// event and rewritten (a crash in that window filed it against an unrelated
// issue). Verified via reload, which reflects only durably-committed rows.
func TestPullRequestEventPersistsWithCorrectParentAcrossReload(t *testing.T) {
	st := reloadedStore(t, func(_ *Persistence, st *Store) {
		st.RecordPullRequestEvent(1, 42, 1, "review_requested", "abc123", 7)
		st.RecordIssueEvent(1, 99, 1, "labeled", map[string]interface{}{"label_id": 5})
	})

	var prEvent, issueEvent *IssueEvent
	for _, e := range st.IssueEvents {
		switch e.Event {
		case "review_requested":
			prEvent = e
		case "labeled":
			issueEvent = e
		}
	}

	if prEvent == nil {
		t.Fatal("pull-request event missing after reload")
	}
	if prEvent.ParentType != "pull_request" || prEvent.IssueID != 42 || prEvent.CommitID != "abc123" || prEvent.RequestedReviewerID != 7 {
		t.Fatalf("pull-request event persisted wrong: %#v", prEvent)
	}
	if issueEvent == nil || issueEvent.ParentType != "issue" || issueEvent.IssueID != 99 || issueEvent.LabelID != 5 {
		t.Fatalf("issue event persisted wrong: %#v", issueEvent)
	}
}

// TestRequestedReviewerTypeName is the GQL-037 regression: the RequestedReviewer
// union must resolve its member from the __typename discriminator rather than
// unconditionally reporting User.
func TestRequestedReviewerTypeName(t *testing.T) {
	cases := []struct {
		name string
		src  interface{}
		want string
	}{
		{"user", map[string]interface{}{"__typename": "User", "login": "octocat"}, "User"},
		{"team", map[string]interface{}{"__typename": "Team", "slug": "reviewers"}, "Team"},
		{"bot", map[string]interface{}{"__typename": "Bot"}, "Bot"},
		{"untagged defaults to user", map[string]interface{}{"login": "octocat"}, "User"},
		{"non-map defaults to user", "not a map", "User"},
	}
	for _, c := range cases {
		if got := requestedReviewerTypeName(c.src); got != c.want {
			t.Errorf("%s: requestedReviewerTypeName = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestProjectV2SourceNoLongerCarriesStore is the GQL-029 regression: the project
// source map must not embed a live *Store, and the id is still extractable
// without one.
func TestProjectV2SourceNoLongerCarriesStore(t *testing.T) {
	m := projectV2ToGQL(&ProjectV2{ID: 7, NodeID: "PVT_x", Number: 3, Title: "Roadmap"})
	if _, ok := m["store"]; ok {
		t.Fatal("projectV2ToGQL still embeds a *Store in the source map")
	}
	id, err := projectV2SourceID(m)
	if err != nil || id != 7 {
		t.Fatalf("projectV2SourceID = %d, %v; want 7, nil", id, err)
	}
	if _, err := projectV2SourceID("not a map"); err == nil {
		t.Fatal("projectV2SourceID accepted a non-map source")
	}
}

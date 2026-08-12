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

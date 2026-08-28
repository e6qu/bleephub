package bleephub

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// Issue.timelineItems / PullRequest.timelineItems.
//
// Every assertion here drives the same REST write paths a client would, then
// reads the timeline back over GraphQL: the point of the surface is that the
// two report the same history, so a test that seeded the store directly would
// not prove the recording half.

func timelineBody(t *testing.T, resp *http.Response, want int) []byte {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, want, raw)
	}
	return raw
}

func timelineTypenames(t *testing.T, nodes []interface{}) []string {
	t.Helper()
	out := make([]string, 0, len(nodes))
	for _, raw := range nodes {
		node, _ := raw.(map[string]interface{})
		name, _ := node["__typename"].(string)
		out = append(out, name)
	}
	return out
}

func timelineNodes(t *testing.T, env map[string]interface{}) (map[string]interface{}, []interface{}) {
	t.Helper()
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("timelineItems query returned errors: %v", errs)
	}
	data, _ := env["data"].(map[string]interface{})
	repository, _ := data["repository"].(map[string]interface{})
	subject, _ := repository["issueOrPullRequest"].(map[string]interface{})
	if subject == nil {
		t.Fatalf("no issueOrPullRequest in %v", data)
	}
	connection, _ := subject["timelineItems"].(map[string]interface{})
	if connection == nil {
		t.Fatalf("no timelineItems in %v", subject)
	}
	nodes, _ := connection["nodes"].([]interface{})
	return connection, nodes
}

const timelineIssueQuery = `query($owner:String!,$name:String!,$number:Int!,$first:Int,$after:String,$itemTypes:[IssueTimelineItemsItemType!],$since:DateTime,$skip:Int){
  repository(owner:$owner,name:$name){
    issueOrPullRequest(number:$number){
      ... on Issue {
        timelineItems(first:$first,after:$after,itemTypes:$itemTypes,since:$since,skip:$skip){
          totalCount
          filteredCount
          pageCount
          updatedAt
          pageInfo{hasNextPage hasPreviousPage startCursor endCursor}
          edges{cursor}
          nodes{
            __typename
            ... on IssueComment{body author{login}}
            ... on ClosedEvent{createdAt actor{login} stateReason closable{... on Issue{number}}}
            ... on ReopenedEvent{closable{... on Issue{number}}}
            ... on LabeledEvent{label{name} labelable{... on Issue{number}}}
            ... on UnlabeledEvent{label{name}}
            ... on AssignedEvent{assignee{... on User{login}} assignable{... on Issue{number}}}
            ... on UnassignedEvent{assignee{... on User{login}}}
            ... on MilestonedEvent{milestoneTitle subject{... on Issue{number}}}
            ... on DemilestonedEvent{milestoneTitle}
            ... on RenamedTitleEvent{previousTitle currentTitle subject{... on Issue{title}}}
            ... on LockedEvent{lockReason lockable{... on Issue{locked}}}
            ... on UnlockedEvent{lockable{... on Issue{locked}}}
            ... on PinnedEvent{issue{number}}
            ... on UnpinnedEvent{issue{number}}
            ... on CrossReferencedEvent{isCrossRepository willCloseTarget source{... on Issue{number title} ... on PullRequest{number title}}}
          }
        }
      }
    }
  }
}`

const timelinePullRequestQuery = `query($owner:String!,$name:String!,$number:Int!,$first:Int,$itemTypes:[PullRequestTimelineItemsItemType!]){
  repository(owner:$owner,name:$name){
    issueOrPullRequest(number:$number){
      ... on PullRequest {
        timelineItems(first:$first,itemTypes:$itemTypes){
          totalCount
          filteredCount
          pageCount
          nodes{
            __typename
            ... on IssueComment{body}
            ... on PullRequestCommit{commit{oid message}}
            ... on PullRequestReview{state body}
            ... on PullRequestReviewThread{path}
            ... on ReviewRequestedEvent{requestedReviewer{... on User{login}} pullRequest{number}}
            ... on ReviewRequestRemovedEvent{requestedReviewer{... on User{login}}}
            ... on ReviewDismissedEvent{previousReviewState dismissalMessage review{state}}
            ... on ReadyForReviewEvent{pullRequest{number} url}
            ... on ConvertToDraftEvent{pullRequest{number}}
            ... on MergedEvent{mergeRefName mergeRef{name} pullRequest{number} url}
            ... on ClosedEvent{closable{... on PullRequest{number}}}
            ... on LabeledEvent{label{name}}
            ... on BaseRefChangedEvent{previousRefName currentRefName}
            ... on RenamedTitleEvent{previousTitle currentTitle}
          }
        }
      }
    }
  }
}`

// TestGraphQLIssueTimelineItemsRendersRecordedHistory drives an issue through
// every write that records a timeline event and asserts the GraphQL union
// renders each of them, with the payload each member carries.
func TestGraphQLIssueTimelineItemsRendersRecordedHistory(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)

	timelineBody(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "tlgql"}), http.StatusCreated)
	timelineBody(t, s.post(t, "/api/v3/repos/admin/tlgql/milestones", defaultToken,
		map[string]interface{}{"title": "v1"}), http.StatusCreated)
	timelineBody(t, s.post(t, "/api/v3/repos/admin/tlgql/issues", defaultToken,
		map[string]interface{}{"title": "first title"}), http.StatusCreated)

	timelineBody(t, s.post(t, "/api/v3/repos/admin/tlgql/issues/1/comments", defaultToken,
		map[string]interface{}{"body": "hello"}), http.StatusCreated)
	timelineBody(t, s.post(t, "/api/v3/repos/admin/tlgql/issues/1/labels", defaultToken,
		map[string]interface{}{"labels": []string{"bug"}}), http.StatusOK)
	timelineBody(t, s.delete(t, "/api/v3/repos/admin/tlgql/issues/1/labels/bug", defaultToken), http.StatusNoContent)
	timelineBody(t, s.post(t, "/api/v3/repos/admin/tlgql/issues/1/assignees", defaultToken,
		map[string]interface{}{"assignees": []string{"admin"}}), http.StatusCreated)
	timelineBody(t, s.do(t, http.MethodDelete, "/api/v3/repos/admin/tlgql/issues/1/assignees", defaultToken,
		map[string]interface{}{"assignees": []string{"admin"}}), http.StatusOK)
	timelineBody(t, s.patch(t, "/api/v3/repos/admin/tlgql/issues/1", defaultToken,
		map[string]interface{}{"milestone": 1}), http.StatusOK)
	timelineBody(t, s.patch(t, "/api/v3/repos/admin/tlgql/issues/1", defaultToken,
		map[string]interface{}{"milestone": nil}), http.StatusOK)
	timelineBody(t, s.patch(t, "/api/v3/repos/admin/tlgql/issues/1", defaultToken,
		map[string]interface{}{"title": "second title"}), http.StatusOK)
	timelineBody(t, s.put(t, "/api/v3/repos/admin/tlgql/issues/1/lock", defaultToken,
		map[string]interface{}{"lock_reason": "off-topic"}), http.StatusNoContent)
	timelineBody(t, s.delete(t, "/api/v3/repos/admin/tlgql/issues/1/lock", defaultToken), http.StatusNoContent)
	timelineBody(t, s.patch(t, "/api/v3/repos/admin/tlgql/issues/1", defaultToken,
		map[string]interface{}{"state": "closed", "state_reason": "not_planned"}), http.StatusOK)
	timelineBody(t, s.patch(t, "/api/v3/repos/admin/tlgql/issues/1", defaultToken,
		map[string]interface{}{"state": "open"}), http.StatusOK)

	env := s.gqlAuthzPost(t, defaultToken, timelineIssueQuery, map[string]interface{}{
		"owner": "admin", "name": "tlgql", "number": 1, "first": 100,
	})
	connection, nodes := timelineNodes(t, env)
	seen := map[string]int{}
	for _, name := range timelineTypenames(t, nodes) {
		seen[name]++
	}
	for _, want := range []string{
		"IssueComment", "LabeledEvent", "UnlabeledEvent", "AssignedEvent", "UnassignedEvent",
		"MilestonedEvent", "DemilestonedEvent", "RenamedTitleEvent", "LockedEvent", "UnlockedEvent",
		"ClosedEvent", "ReopenedEvent",
	} {
		if seen[want] == 0 {
			t.Errorf("timelineItems is missing a %s; saw %v", want, seen)
		}
	}
	if total, _ := connection["totalCount"].(float64); int(total) != len(nodes) {
		t.Errorf("totalCount = %v, want %d", connection["totalCount"], len(nodes))
	}
	if page, _ := connection["pageCount"].(float64); int(page) != len(nodes) {
		t.Errorf("pageCount = %v, want %d", connection["pageCount"], len(nodes))
	}
	if filtered, _ := connection["filteredCount"].(float64); int(filtered) != len(nodes) {
		t.Errorf("filteredCount = %v, want %d", connection["filteredCount"], len(nodes))
	}
	if updated, _ := connection["updatedAt"].(string); updated == "" {
		t.Error("timelineItems.updatedAt is empty")
	}

	// The payload each member carries, not just its presence.
	for _, raw := range nodes {
		node, _ := raw.(map[string]interface{})
		switch node["__typename"] {
		case "LabeledEvent":
			label, _ := node["label"].(map[string]interface{})
			if label["name"] != "bug" {
				t.Errorf("LabeledEvent.label.name = %v, want bug", label["name"])
			}
			subject, _ := node["labelable"].(map[string]interface{})
			if got, _ := subject["number"].(float64); int(got) != 1 {
				t.Errorf("LabeledEvent.labelable.number = %v, want 1", subject["number"])
			}
		case "AssignedEvent":
			assignee, _ := node["assignee"].(map[string]interface{})
			if assignee["login"] != "admin" {
				t.Errorf("AssignedEvent.assignee.login = %v, want admin", assignee["login"])
			}
		case "MilestonedEvent":
			if node["milestoneTitle"] != "v1" {
				t.Errorf("MilestonedEvent.milestoneTitle = %v, want v1", node["milestoneTitle"])
			}
		case "RenamedTitleEvent":
			if node["previousTitle"] != "first title" || node["currentTitle"] != "second title" {
				t.Errorf("RenamedTitleEvent = %v/%v, want first title/second title",
					node["previousTitle"], node["currentTitle"])
			}
		case "LockedEvent":
			if node["lockReason"] != "OFF_TOPIC" {
				t.Errorf("LockedEvent.lockReason = %v, want OFF_TOPIC", node["lockReason"])
			}
		case "IssueComment":
			if node["body"] != "hello" {
				t.Errorf("IssueComment.body = %v, want hello", node["body"])
			}
		}
	}
	// The issue was reopened, so no close is the one that produced its current
	// state: stateReason must be null rather than the stale not_planned.
	for _, raw := range nodes {
		node, _ := raw.(map[string]interface{})
		if node["__typename"] == "ClosedEvent" && node["stateReason"] != nil {
			t.Errorf("ClosedEvent.stateReason = %v on a reopened issue, want null", node["stateReason"])
		}
	}
}

// TestGraphQLIssueTimelineItemsPaginatesAndFilters exercises the connection
// arguments GitHub gives the field: first/after, itemTypes, since and skip,
// and the filteredCount/pageCount members only these two connections carry.
func TestGraphQLIssueTimelineItemsPaginatesAndFilters(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)

	timelineBody(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "tlpage"}), http.StatusCreated)
	timelineBody(t, s.post(t, "/api/v3/repos/admin/tlpage/issues", defaultToken,
		map[string]interface{}{"title": "t"}), http.StatusCreated)
	for _, body := range []string{"one", "two", "three"} {
		timelineBody(t, s.post(t, "/api/v3/repos/admin/tlpage/issues/1/comments", defaultToken,
			map[string]interface{}{"body": body}), http.StatusCreated)
	}
	timelineBody(t, s.post(t, "/api/v3/repos/admin/tlpage/issues/1/labels", defaultToken,
		map[string]interface{}{"labels": []string{"bug"}}), http.StatusOK)

	query := func(vars map[string]interface{}) (map[string]interface{}, []interface{}) {
		vars["owner"], vars["name"], vars["number"] = "admin", "tlpage", 1
		return timelineNodes(t, s.gqlAuthzPost(t, defaultToken, timelineIssueQuery, vars))
	}

	all, allNodes := query(map[string]interface{}{"first": 100})
	if len(allNodes) != 4 {
		t.Fatalf("timeline has %d items, want 4 (three comments + one label)", len(allNodes))
	}
	if total, _ := all["totalCount"].(float64); int(total) != 4 {
		t.Fatalf("totalCount = %v, want 4", all["totalCount"])
	}

	// first: 2 returns a page and reports the rest as still pending.
	page, pageNodes := query(map[string]interface{}{"first": 2})
	if len(pageNodes) != 2 {
		t.Fatalf("first:2 returned %d nodes, want 2", len(pageNodes))
	}
	if count, _ := page["pageCount"].(float64); int(count) != 2 {
		t.Errorf("pageCount = %v, want 2", page["pageCount"])
	}
	if count, _ := page["filteredCount"].(float64); int(count) != 4 {
		t.Errorf("filteredCount = %v with no cursor filter, want the full 4", page["filteredCount"])
	}
	info, _ := page["pageInfo"].(map[string]interface{})
	if info["hasNextPage"] != true {
		t.Errorf("pageInfo.hasNextPage = %v after first:2 of 4, want true", info["hasNextPage"])
	}
	cursor, _ := info["endCursor"].(string)
	if cursor == "" {
		t.Fatal("pageInfo.endCursor is empty")
	}

	// The cursor resumes exactly where the first page stopped.
	_, restNodes := query(map[string]interface{}{"first": 100, "after": cursor})
	if len(restNodes) != 2 {
		t.Fatalf("after the first page, %d nodes remain, want 2", len(restNodes))
	}
	firstBodies := timelineTypenames(t, pageNodes)
	restBodies := timelineTypenames(t, restNodes)
	if len(firstBodies)+len(restBodies) != 4 {
		t.Fatalf("the two pages cover %d items, want 4", len(firstBodies)+len(restBodies))
	}

	// itemTypes narrows the union to one member.
	filtered, filteredNodes := query(map[string]interface{}{"first": 100, "itemTypes": []string{"LABELED_EVENT"}})
	if len(filteredNodes) != 1 || timelineTypenames(t, filteredNodes)[0] != "LabeledEvent" {
		t.Fatalf("itemTypes:[LABELED_EVENT] returned %v", timelineTypenames(t, filteredNodes))
	}
	if total, _ := filtered["totalCount"].(float64); int(total) != 1 {
		t.Errorf("totalCount under itemTypes = %v, want 1", filtered["totalCount"])
	}

	// skip advances the window without changing what the window contains.
	skipped, skippedNodes := query(map[string]interface{}{"first": 100, "skip": 3})
	if len(skippedNodes) != 1 {
		t.Fatalf("skip:3 returned %d nodes, want 1", len(skippedNodes))
	}
	if count, _ := skipped["filteredCount"].(float64); int(count) != 4 {
		t.Errorf("filteredCount under skip = %v, want the pre-skip 4", skipped["filteredCount"])
	}

	// since in the future filters everything out; the connection is empty
	// rather than absent.
	_, futureNodes := query(map[string]interface{}{"first": 100, "since": "2999-01-01T00:00:00Z"})
	if len(futureNodes) != 0 {
		t.Fatalf("since:2999 returned %d nodes, want 0", len(futureNodes))
	}
}

// TestGraphQLPullRequestTimelineItemsRendersPullRequestMembers covers the
// members only a pull request's timeline has — its git commits, its reviews,
// its review threads and the review-request/dismissal events.
func TestGraphQLPullRequestTimelineItemsRendersPullRequestMembers(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "tlpr")

	timelineBody(t, s.post(t, "/api/v3/repos/admin/tlpr/pulls", defaultToken, map[string]interface{}{
		"title": "pr title", "head": "feature", "base": "main", "body": "body",
	}), http.StatusCreated)
	timelineBody(t, s.post(t, "/api/v3/repos/admin/tlpr/issues/1/comments", defaultToken,
		map[string]interface{}{"body": "conversation"}), http.StatusCreated)
	timelineBody(t, s.patch(t, "/api/v3/repos/admin/tlpr/pulls/1", defaultToken,
		map[string]interface{}{"title": "renamed pr"}), http.StatusOK)

	raw := timelineBody(t, s.post(t, "/api/v3/repos/admin/tlpr/pulls/1/reviews", defaultToken,
		map[string]interface{}{"body": "LGTM", "event": "APPROVE"}), http.StatusOK)
	var review struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(raw, &review); err != nil {
		t.Fatal(err)
	}
	timelineBody(t, s.put(t, "/api/v3/repos/admin/tlpr/pulls/1/reviews/"+itoa(review.ID)+"/dismissals", defaultToken,
		map[string]interface{}{"message": "stale"}), http.StatusOK)

	env := s.gqlAuthzPost(t, defaultToken, timelinePullRequestQuery, map[string]interface{}{
		"owner": "admin", "name": "tlpr", "number": 1, "first": 100,
	})
	connection, nodes := timelineNodes(t, env)
	seen := map[string]int{}
	for _, name := range timelineTypenames(t, nodes) {
		seen[name]++
	}
	for _, want := range []string{"IssueComment", "PullRequestCommit", "PullRequestReview", "ReviewDismissedEvent", "RenamedTitleEvent"} {
		if seen[want] == 0 {
			t.Errorf("pull-request timelineItems is missing a %s; saw %v", want, seen)
		}
	}
	if total, _ := connection["totalCount"].(float64); int(total) != len(nodes) {
		t.Errorf("totalCount = %v, want %d", connection["totalCount"], len(nodes))
	}

	for _, item := range nodes {
		node, _ := item.(map[string]interface{})
		switch node["__typename"] {
		case "PullRequestCommit":
			commit, _ := node["commit"].(map[string]interface{})
			if oid, _ := commit["oid"].(string); len(oid) != 40 {
				t.Errorf("PullRequestCommit.commit.oid = %v, want a real 40-character sha", commit["oid"])
			}
		case "ReviewDismissedEvent":
			if node["dismissalMessage"] != "stale" {
				t.Errorf("ReviewDismissedEvent.dismissalMessage = %v, want stale", node["dismissalMessage"])
			}
			if node["previousReviewState"] != "APPROVED" {
				t.Errorf("ReviewDismissedEvent.previousReviewState = %v, want APPROVED", node["previousReviewState"])
			}
			dismissed, _ := node["review"].(map[string]interface{})
			if dismissed["state"] != "DISMISSED" {
				t.Errorf("ReviewDismissedEvent.review.state = %v, want DISMISSED", dismissed["state"])
			}
		}
	}

	// The pull-request-only item types are selectable through itemTypes.
	_, commitsOnly := timelineNodes(t, s.gqlAuthzPost(t, defaultToken, timelinePullRequestQuery, map[string]interface{}{
		"owner": "admin", "name": "tlpr", "number": 1, "first": 100,
		"itemTypes": []string{"PULL_REQUEST_COMMIT"},
	}))
	if len(commitsOnly) == 0 {
		t.Fatal("itemTypes:[PULL_REQUEST_COMMIT] returned nothing; the pull request has commits")
	}
	for _, name := range timelineTypenames(t, commitsOnly) {
		if name != "PullRequestCommit" {
			t.Fatalf("itemTypes:[PULL_REQUEST_COMMIT] returned a %s", name)
		}
	}
}

// TestGraphQLPullRequestTimelineItemsRecordsMergeAndBaseChange covers the two
// pull-request members whose payload is a ref: MergedEvent (the base the merge
// landed on) and BaseRefChangedEvent (the retarget that preceded it).
func TestGraphQLPullRequestTimelineItemsRecordsMergeAndBaseChange(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "tlmerge")

	timelineBody(t, s.post(t, "/api/v3/repos/admin/tlmerge/pulls", defaultToken, map[string]interface{}{
		"title": "merge me", "head": "feature", "base": "main",
	}), http.StatusCreated)
	timelineBody(t, s.patch(t, "/api/v3/repos/admin/tlmerge/pulls/1", defaultToken,
		map[string]interface{}{"base": "spare"}), http.StatusOK)
	timelineBody(t, s.patch(t, "/api/v3/repos/admin/tlmerge/pulls/1", defaultToken,
		map[string]interface{}{"base": "main"}), http.StatusOK)
	timelineBody(t, s.put(t, "/api/v3/repos/admin/tlmerge/pulls/1/merge", defaultToken,
		map[string]interface{}{}), http.StatusOK)

	_, nodes := timelineNodes(t, s.gqlAuthzPost(t, defaultToken, timelinePullRequestQuery, map[string]interface{}{
		"owner": "admin", "name": "tlmerge", "number": 1, "first": 100,
	}))
	var merged, retargeted map[string]interface{}
	for _, item := range nodes {
		node, _ := item.(map[string]interface{})
		switch node["__typename"] {
		case "MergedEvent":
			merged = node
		case "BaseRefChangedEvent":
			if retargeted == nil {
				retargeted = node
			}
		}
	}
	if merged == nil {
		t.Fatalf("no MergedEvent in %v", timelineTypenames(t, nodes))
	}
	if merged["mergeRefName"] != "main" {
		t.Errorf("MergedEvent.mergeRefName = %v, want main", merged["mergeRefName"])
	}
	ref, _ := merged["mergeRef"].(map[string]interface{})
	if ref == nil || ref["name"] != "main" {
		t.Errorf("MergedEvent.mergeRef = %v, want the main ref", merged["mergeRef"])
	}
	if retargeted == nil {
		t.Fatalf("no BaseRefChangedEvent in %v", timelineTypenames(t, nodes))
	}
	if retargeted["previousRefName"] != "main" || retargeted["currentRefName"] != "spare" {
		t.Errorf("BaseRefChangedEvent = %v -> %v, want main -> spare",
			retargeted["previousRefName"], retargeted["currentRefName"])
	}
}

// TestGraphQLTimelineCrossReferencesRedactUnreadableSources checks the one
// item that is derived from another repository's content: a cross-reference
// from a private repository the viewer cannot read must not disclose the
// referencing issue's title, number or author — and, because the drop happens
// before pagination, must not show up in totalCount either.
func TestGraphQLTimelineCrossReferencesRedactUnreadableSources(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	outsider := s.store.CreateToken(s.createTestUser(t, "tl-outsider").ID, "repo").Value

	timelineBody(t, s.post(t, "/api/v3/user/repos", defaultToken,
		map[string]interface{}{"name": "tlpublic", "private": false}), http.StatusCreated)
	timelineBody(t, s.post(t, "/api/v3/user/repos", defaultToken,
		map[string]interface{}{"name": "tlsecret", "private": true}), http.StatusCreated)
	timelineBody(t, s.post(t, "/api/v3/repos/admin/tlpublic/issues", defaultToken,
		map[string]interface{}{"title": "target"}), http.StatusCreated)
	timelineBody(t, s.post(t, "/api/v3/repos/admin/tlpublic/issues", defaultToken,
		map[string]interface{}{"title": "public mention", "body": "see #1"}), http.StatusCreated)
	timelineBody(t, s.post(t, "/api/v3/repos/admin/tlsecret/issues", defaultToken,
		map[string]interface{}{"title": "CONFIDENTIAL ACQUISITION", "body": "fixes admin/tlpublic#1"}), http.StatusCreated)

	crossReferences := func(token string) []map[string]interface{} {
		t.Helper()
		_, nodes := timelineNodes(t, s.gqlAuthzPost(t, token, timelineIssueQuery, map[string]interface{}{
			"owner": "admin", "name": "tlpublic", "number": 1, "first": 100,
		}))
		out := []map[string]interface{}{}
		for _, item := range nodes {
			node, _ := item.(map[string]interface{})
			if node["__typename"] == "CrossReferencedEvent" {
				out = append(out, node)
			}
		}
		return out
	}

	owner := crossReferences(defaultToken)
	if len(owner) != 2 {
		t.Fatalf("the owner sees %d cross-references, want 2 (one public, one private)", len(owner))
	}
	sawClosing := false
	for _, event := range owner {
		if event["willCloseTarget"] == true {
			sawClosing = true
		}
	}
	if !sawClosing {
		t.Error("no cross-reference reports willCloseTarget; the private issue uses a closing keyword")
	}

	stranger := crossReferences(outsider)
	if len(stranger) != 1 {
		t.Fatalf("an outsider sees %d cross-references, want only the public one", len(stranger))
	}
	source, _ := stranger[0]["source"].(map[string]interface{})
	if title, _ := source["title"].(string); title != "public mention" {
		t.Fatalf("the outsider's cross-reference source is %q, want the public issue", title)
	}
	raw, err := json.Marshal(stranger)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); strings.Contains(got, "CONFIDENTIAL ACQUISITION") {
		t.Fatalf("the private repository's issue title leaked into an outsider's timeline: %s", got)
	}
}

// TestGraphQLTimelineRecordsPreviouslyUnrecordedEvents covers the four events
// bleephub always produced but never wrote down, so the timeline could not
// report them: a deleted comment, a commit that names an issue, and the
// deletion and restoration of a pull request's head branch.
func TestGraphQLTimelineRecordsPreviouslyUnrecordedEvents(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "tlrec")

	timelineBody(t, s.post(t, "/api/v3/repos/admin/tlrec/issues", defaultToken,
		map[string]interface{}{"title": "referenced target"}), http.StatusCreated)
	raw := timelineBody(t, s.post(t, "/api/v3/repos/admin/tlrec/issues/1/comments", defaultToken,
		map[string]interface{}{"body": "doomed"}), http.StatusCreated)
	var comment struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(raw, &comment); err != nil {
		t.Fatal(err)
	}
	timelineBody(t, s.delete(t, "/api/v3/repos/admin/tlrec/issues/comments/"+itoa(comment.ID), defaultToken),
		http.StatusNoContent)

	// A commit whose message names the issue is github's `referenced` event.
	timelineBody(t, s.put(t, "/api/v3/repos/admin/tlrec/contents/notes.txt", defaultToken, map[string]interface{}{
		"message": "tidy up, refs #1",
		"content": base64.StdEncoding.EncodeToString([]byte("notes\n")),
	}), http.StatusCreated)

	issueQuery := `query($owner:String!,$name:String!,$number:Int!){
  repository(owner:$owner,name:$name){
    issueOrPullRequest(number:$number){
      ... on Issue {
        timelineItems(first:100){
          nodes{
            __typename
            ... on CommentDeletedEvent{databaseId deletedCommentAuthor{login}}
            ... on ReferencedEvent{isDirectReference commitRepository{nameWithOwner} commit{oid message} subject{... on Issue{number}}}
          }
        }
      }
    }
  }
}`
	_, nodes := timelineNodes(t, s.gqlAuthzPost(t, defaultToken, issueQuery, map[string]interface{}{
		"owner": "admin", "name": "tlrec", "number": 1,
	}))
	var deleted, referenced map[string]interface{}
	for _, item := range nodes {
		node, _ := item.(map[string]interface{})
		switch node["__typename"] {
		case "CommentDeletedEvent":
			deleted = node
		case "ReferencedEvent":
			referenced = node
		}
	}
	if deleted == nil {
		t.Fatalf("no CommentDeletedEvent in %v", timelineTypenames(t, nodes))
	}
	if got, _ := deleted["databaseId"].(float64); int(got) != comment.ID {
		t.Errorf("CommentDeletedEvent.databaseId = %v, want the deleted comment id %d", deleted["databaseId"], comment.ID)
	}
	author, _ := deleted["deletedCommentAuthor"].(map[string]interface{})
	if author["login"] != "admin" {
		t.Errorf("CommentDeletedEvent.deletedCommentAuthor.login = %v, want admin", author["login"])
	}
	if referenced == nil {
		t.Fatalf("no ReferencedEvent in %v", timelineTypenames(t, nodes))
	}
	commit, _ := referenced["commit"].(map[string]interface{})
	if oid, _ := commit["oid"].(string); len(oid) != 40 {
		t.Errorf("ReferencedEvent.commit.oid = %v, want a real sha", commit["oid"])
	}
	repository, _ := referenced["commitRepository"].(map[string]interface{})
	if repository["nameWithOwner"] != "admin/tlrec" {
		t.Errorf("ReferencedEvent.commitRepository = %v, want admin/tlrec", referenced["commitRepository"])
	}

	// Deleting and restoring a pull request's head branch.
	timelineBody(t, s.post(t, "/api/v3/repos/admin/tlrec/pulls", defaultToken, map[string]interface{}{
		"title": "head ref", "head": "feature", "base": "main",
	}), http.StatusCreated)
	headSHA := store.ResolveBranchSha(s.store.GetGitStorage("admin", "tlrec"), "feature")
	if headSHA == "" {
		t.Fatal("the feature branch has no head commit")
	}
	timelineBody(t, s.delete(t, "/api/v3/repos/admin/tlrec/git/refs/heads/feature", defaultToken), http.StatusNoContent)
	timelineBody(t, s.post(t, "/api/v3/repos/admin/tlrec/git/refs", defaultToken, map[string]interface{}{
		"ref": "refs/heads/feature", "sha": headSHA,
	}), http.StatusCreated)

	prQuery := `query($owner:String!,$name:String!,$number:Int!){
  repository(owner:$owner,name:$name){
    issueOrPullRequest(number:$number){
      ... on PullRequest {
        timelineItems(first:100,itemTypes:[HEAD_REF_DELETED_EVENT,HEAD_REF_RESTORED_EVENT]){
          totalCount
          nodes{
            __typename
            ... on HeadRefDeletedEvent{headRefName headRef{name} pullRequest{number}}
            ... on HeadRefRestoredEvent{pullRequest{number}}
          }
        }
      }
    }
  }
}`
	_, refNodes := timelineNodes(t, s.gqlAuthzPost(t, defaultToken, prQuery, map[string]interface{}{
		"owner": "admin", "name": "tlrec", "number": 2,
	}))
	got := timelineTypenames(t, refNodes)
	if len(got) != 2 || got[0] != "HeadRefDeletedEvent" || got[1] != "HeadRefRestoredEvent" {
		t.Fatalf("head-ref timeline = %v, want [HeadRefDeletedEvent HeadRefRestoredEvent]", got)
	}
	head, _ := refNodes[0].(map[string]interface{})
	if head["headRefName"] != "feature" {
		t.Errorf("HeadRefDeletedEvent.headRefName = %v, want feature", head["headRefName"])
	}
	// The branch is back, so the deletion event now resolves a live ref.
	ref, _ := head["headRef"].(map[string]interface{})
	if ref == nil || ref["name"] != "feature" {
		t.Errorf("HeadRefDeletedEvent.headRef = %v, want the restored feature ref", head["headRef"])
	}
}

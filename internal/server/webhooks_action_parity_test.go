package bleephub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/server/testutil"
)

// Receiver-driven proof that a subscriber to `*` sees GitHub's full action set
// for issues, pull requests, and releases — not just the create/close subset
// bleephub used to fan out. Each case drives real API mutations and asserts the
// exact delivered action sequence plus the members GitHub attaches to the
// action-specific payloads (label / assignee / milestone / changes /
// requested_reviewer).

type webhookDelivery struct {
	event   string
	action  string
	payload map[string]interface{}
}

// webhookActionSink records every delivery a `*` subscriber receives.
type webhookActionSink struct {
	mu         sync.Mutex
	deliveries []webhookDelivery
}

// handler counts only deliveries whose body parsed: under race-load a delivery
// write can be cut short by the sender's timeout, and asserting on a truncated
// attempt would make the test flaky rather than catch a real regression (the
// same rule TestWebhookPing follows).
func (sink *webhookActionSink) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		raw := body
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			vals, err := url.ParseQuery(string(body))
			if err != nil {
				w.WriteHeader(http.StatusOK)
				return
			}
			raw = []byte(vals.Get("payload"))
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		action, _ := parsed["action"].(string)
		sink.mu.Lock()
		sink.deliveries = append(sink.deliveries, webhookDelivery{
			event:   r.Header.Get("X-GitHub-Event"),
			action:  action,
			payload: parsed,
		})
		sink.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}

// actionsFor returns the actions delivered for one event type, in the order
// they arrived. Deliveries for a single hook are drained by one queue, so the
// order is the emission order.
func (sink *webhookActionSink) actionsFor(event string) []string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var out []string
	for _, d := range sink.deliveries {
		if d.event == event {
			out = append(out, d.action)
		}
	}
	return out
}

// payloadFor returns the payload of the first delivery of event/action.
func (sink *webhookActionSink) payloadFor(t *testing.T, event, action string) map[string]interface{} {
	t.Helper()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, d := range sink.deliveries {
		if d.event == event && d.action == action {
			return d.payload
		}
	}
	t.Fatalf("no %s.%s delivery was received", event, action)
	return nil
}

// requireActions waits for the expected number of deliveries of one event type
// and then asserts the whole sequence; an extra or reordered action fails.
func (sink *webhookActionSink) requireActions(t *testing.T, event string, want []string) {
	t.Helper()
	testutil.TestEventually(10*time.Second, 20*time.Millisecond, func() bool {
		return len(sink.actionsFor(event)) >= len(want)
	})
	got := sink.actionsFor(event)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s actions =\n  %v\nwant\n  %v", event, got, want)
	}
}

// subscribeAllEvents points a `*` hook at the sink. It is created after the
// fixture is built so only the mutations under test are recorded.
func (s *isolatedServer) subscribeAllEvents(t *testing.T, repoFullName, receiverURL string) {
	t.Helper()
	resp := s.post(t, "/api/v3/repos/"+repoFullName+"/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": receiverURL},
		"events": []string{"*"},
		"active": true,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create `*` hook: status = %d", resp.StatusCode)
	}
}

// nestedString walks a payload by key path and returns the string it ends on.
func nestedString(t *testing.T, payload map[string]interface{}, path ...string) string {
	t.Helper()
	cursor := payload
	for i, key := range path {
		if i == len(path)-1 {
			out, ok := cursor[key].(string)
			if !ok {
				t.Fatalf("payload %v is not a string: %#v", path, cursor[key])
			}
			return out
		}
		next, ok := cursor[key].(map[string]interface{})
		if !ok {
			t.Fatalf("payload %v: %q is not an object: %#v", path, key, cursor[key])
		}
		cursor = next
	}
	return ""
}

func TestWebhookIssuesActionSetIsComplete(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	sink := &webhookActionSink{}
	receiver, cleanup := startWebhookReceiver(t, sink.handler())
	defer cleanup()

	const repo = "admin/wh-issue-actions"
	expectStatus(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "wh-issue-actions", "auto_init": true,
	}), http.StatusCreated, "create repo")
	expectStatus(t, s.post(t, "/api/v3/repos/"+repo+"/labels", defaultToken, map[string]interface{}{
		"name": "regression", "color": "d73a4a",
	}), http.StatusCreated, "create label")
	expectStatus(t, s.post(t, "/api/v3/repos/"+repo+"/milestones", defaultToken, map[string]interface{}{
		"title": "v1",
	}), http.StatusCreated, "create milestone")

	s.subscribeAllEvents(t, repo, receiver)

	expectStatus(t, s.post(t, "/api/v3/repos/"+repo+"/issues", defaultToken, map[string]interface{}{
		"title": "original title", "body": "original body",
	}), http.StatusCreated, "open issue")
	issue := "/api/v3/repos/" + repo + "/issues/1"
	expectStatus(t, s.patch(t, issue, defaultToken, map[string]interface{}{
		"title": "edited title",
	}), http.StatusOK, "edit title")
	expectStatus(t, s.post(t, issue+"/labels", defaultToken, map[string]interface{}{
		"labels": []string{"regression"},
	}), http.StatusOK, "add label")
	expectStatus(t, s.delete(t, issue+"/labels/regression", defaultToken), http.StatusNoContent, "remove label")
	expectStatus(t, s.post(t, issue+"/assignees", defaultToken, map[string]interface{}{
		"assignees": []string{"admin"},
	}), http.StatusCreated, "add assignee")
	expectStatus(t, s.do(t, http.MethodDelete, issue+"/assignees", defaultToken, map[string]interface{}{
		"assignees": []string{"admin"},
	}), http.StatusOK, "remove assignee")
	expectStatus(t, s.patch(t, issue, defaultToken, map[string]interface{}{
		"milestone": 1,
	}), http.StatusOK, "set milestone")
	expectStatus(t, s.patch(t, issue, defaultToken, map[string]interface{}{
		"milestone": nil,
	}), http.StatusOK, "clear milestone")
	expectStatus(t, s.put(t, issue+"/lock", defaultToken, map[string]interface{}{
		"lock_reason": "resolved",
	}), http.StatusNoContent, "lock")
	expectStatus(t, s.delete(t, issue+"/lock", defaultToken), http.StatusNoContent, "unlock")
	expectStatus(t, s.patch(t, issue, defaultToken, map[string]interface{}{
		"state": "closed",
	}), http.StatusOK, "close")
	expectStatus(t, s.patch(t, issue, defaultToken, map[string]interface{}{
		"state": "open",
	}), http.StatusOK, "reopen")

	sink.requireActions(t, "issues", []string{
		"opened", "edited", "labeled", "unlabeled", "assigned", "unassigned",
		"milestoned", "demilestoned", "locked", "unlocked", "closed", "reopened",
	})

	// `edited` carries changes.title.from; `labeled`/`unlabeled` carry `label`;
	// `assigned`/`unassigned` carry `assignee`; the milestone pair carries
	// `milestone`. A consumer branches on exactly these members.
	if from := nestedString(t, sink.payloadFor(t, "issues", "edited"), "changes", "title", "from"); from != "original title" {
		t.Errorf("issues.edited changes.title.from = %q, want %q", from, "original title")
	}
	for _, action := range []string{"labeled", "unlabeled"} {
		if name := nestedString(t, sink.payloadFor(t, "issues", action), "label", "name"); name != "regression" {
			t.Errorf("issues.%s label.name = %q, want regression", action, name)
		}
	}
	for _, action := range []string{"assigned", "unassigned"} {
		if login := nestedString(t, sink.payloadFor(t, "issues", action), "assignee", "login"); login != "admin" {
			t.Errorf("issues.%s assignee.login = %q, want admin", action, login)
		}
	}
	for _, action := range []string{"milestoned", "demilestoned"} {
		if title := nestedString(t, sink.payloadFor(t, "issues", action), "milestone", "title"); title != "v1" {
			t.Errorf("issues.%s milestone.title = %q, want v1", action, title)
		}
	}
	// Every action still carries the shared members, so a `*` consumer can
	// treat any of them uniformly.
	locked := sink.payloadFor(t, "issues", "locked")
	if got := nestedString(t, locked, "repository", "full_name"); got != repo {
		t.Errorf("issues.locked repository.full_name = %q, want %q", got, repo)
	}
	if got := nestedString(t, locked, "sender", "login"); got != "admin" {
		t.Errorf("issues.locked sender.login = %q, want admin", got)
	}
}

func TestWebhookPullRequestActionSetIsComplete(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	sink := &webhookActionSink{}
	receiver, cleanup := startWebhookReceiver(t, sink.handler())
	defer cleanup()

	const name = "wh-pr-actions"
	const repo = "admin/" + name
	s.createTestPRRepo(t, name)
	reviewer, _ := s.newUser(t, "pr-reviewer")
	admin := s.store.LookupUserByLogin("admin")
	if admin == nil {
		t.Fatal("default admin user missing")
	}
	expectStatus(t, s.patch(t, "/api/v3/repos/"+repo, defaultToken, map[string]interface{}{
		"allow_auto_merge": true,
	}), http.StatusOK, "allow auto-merge")
	// A required check that never turns green is the blocking condition
	// auto-merge arms against; without one, enabling is refused as "clean".
	expectStatus(t, s.put(t, "/api/v3/repos/"+repo+"/branches/main/protection", defaultToken, map[string]interface{}{
		"required_status_checks": map[string]interface{}{"strict": false, "contexts": []string{"ci"}},
		"enforce_admins":         true,
	}), http.StatusOK, "protect main")
	expectStatus(t, s.post(t, "/api/v3/repos/"+repo+"/labels", defaultToken, map[string]interface{}{
		"name": "regression", "color": "d73a4a",
	}), http.StatusCreated, "create label")
	expectStatus(t, s.post(t, "/api/v3/repos/"+repo+"/milestones", defaultToken, map[string]interface{}{
		"title": "v1",
	}), http.StatusCreated, "create milestone")
	milestone := s.store.GetMilestoneByNumber(s.store.GetRepo("admin", name).ID, 1)
	if milestone == nil {
		t.Fatal("milestone 1 not created")
	}

	s.subscribeAllEvents(t, repo, receiver)

	expectStatus(t, s.post(t, "/api/v3/repos/"+repo+"/pulls", defaultToken, map[string]interface{}{
		"title": "original title", "head": "feat", "base": "main",
	}), http.StatusCreated, "open PR")
	pr := s.store.GetPullRequestByNumber(s.store.GetRepo("admin", name).ID, 1)
	if pr == nil {
		t.Fatal("pull request 1 not created")
	}

	// Labels reach a pull request through the shared /issues surface.
	expectStatus(t, s.post(t, "/api/v3/repos/"+repo+"/issues/1/labels", defaultToken, map[string]interface{}{
		"labels": []string{"regression"},
	}), http.StatusOK, "add label")
	expectStatus(t, s.delete(t, "/api/v3/repos/"+repo+"/issues/1/labels/regression", defaultToken),
		http.StatusNoContent, "remove label")

	// Assignees and the milestone are GraphQL-only on a pull request, so
	// updatePullRequest is the only site that can produce these actions.
	const updatePRDoc = `mutation($input:UpdatePullRequestInput!){updatePullRequest(input:$input){pullRequest{number}}}`
	requireNoGQLErrors(t, s.gqlAuthzPost(t, defaultToken, updatePRDoc, map[string]interface{}{
		"input": map[string]interface{}{"pullRequestId": pr.NodeID, "assigneeIds": []string{admin.NodeID}},
	}))
	requireNoGQLErrors(t, s.gqlAuthzPost(t, defaultToken, updatePRDoc, map[string]interface{}{
		"input": map[string]interface{}{"pullRequestId": pr.NodeID, "assigneeIds": []string{}},
	}))
	requireNoGQLErrors(t, s.gqlAuthzPost(t, defaultToken, updatePRDoc, map[string]interface{}{
		"input": map[string]interface{}{"pullRequestId": pr.NodeID, "milestoneId": milestone.NodeID},
	}))
	// An empty milestoneId is how updatePullRequest clears the milestone.
	requireNoGQLErrors(t, s.gqlAuthzPost(t, defaultToken, updatePRDoc, map[string]interface{}{
		"input": map[string]interface{}{"pullRequestId": pr.NodeID, "milestoneId": ""},
	}))

	expectStatus(t, s.post(t, "/api/v3/repos/"+repo+"/pulls/1/requested_reviewers", defaultToken, map[string]interface{}{
		"reviewers": []string{reviewer.Login},
	}), http.StatusCreated, "request review")
	expectStatus(t, s.do(t, http.MethodDelete, "/api/v3/repos/"+repo+"/pulls/1/requested_reviewers", defaultToken,
		map[string]interface{}{"reviewers": []string{reviewer.Login}}), http.StatusOK, "remove review request")

	const convertDoc = `mutation($input:ConvertPullRequestToDraftInput!){convertPullRequestToDraft(input:$input){pullRequest{isDraft}}}`
	const readyDoc = `mutation($input:MarkPullRequestReadyForReviewInput!){markPullRequestReadyForReview(input:$input){pullRequest{isDraft}}}`
	requireNoGQLErrors(t, s.gqlAuthzPost(t, defaultToken, convertDoc, map[string]interface{}{
		"input": map[string]interface{}{"pullRequestId": pr.NodeID},
	}))
	requireNoGQLErrors(t, s.gqlAuthzPost(t, defaultToken, readyDoc, map[string]interface{}{
		"input": map[string]interface{}{"pullRequestId": pr.NodeID},
	}))

	requireNoGQLErrors(t, s.gqlAuthzPost(t, defaultToken, enableAutoMergeDoc, map[string]interface{}{
		"input": map[string]interface{}{"pullRequestId": pr.NodeID, "mergeMethod": "SQUASH"},
	}))
	requireNoGQLErrors(t, s.gqlAuthzPost(t, defaultToken, disableAutoMergeDoc, map[string]interface{}{
		"input": map[string]interface{}{"pullRequestId": pr.NodeID},
	}))

	expectStatus(t, s.patch(t, "/api/v3/repos/"+repo+"/pulls/1", defaultToken, map[string]interface{}{
		"title": "edited title",
	}), http.StatusOK, "edit title")
	expectStatus(t, s.put(t, "/api/v3/repos/"+repo+"/issues/1/lock", defaultToken, nil),
		http.StatusNoContent, "lock")
	expectStatus(t, s.delete(t, "/api/v3/repos/"+repo+"/issues/1/lock", defaultToken),
		http.StatusNoContent, "unlock")
	expectStatus(t, s.patch(t, "/api/v3/repos/"+repo+"/pulls/1", defaultToken, map[string]interface{}{
		"state": "closed",
	}), http.StatusOK, "close")
	expectStatus(t, s.patch(t, "/api/v3/repos/"+repo+"/pulls/1", defaultToken, map[string]interface{}{
		"state": "open",
	}), http.StatusOK, "reopen")

	sink.requireActions(t, "pull_request", []string{
		"opened", "labeled", "unlabeled", "assigned", "unassigned",
		"milestoned", "demilestoned",
		"review_requested", "review_request_removed",
		"converted_to_draft", "ready_for_review",
		"auto_merge_enabled", "auto_merge_disabled",
		"edited", "locked", "unlocked", "closed", "reopened",
	})

	if label := nestedString(t, sink.payloadFor(t, "pull_request", "labeled"), "label", "name"); label != "regression" {
		t.Errorf("pull_request.labeled label.name = %q, want regression", label)
	}
	for _, action := range []string{"assigned", "unassigned"} {
		if login := nestedString(t, sink.payloadFor(t, "pull_request", action), "assignee", "login"); login != "admin" {
			t.Errorf("pull_request.%s assignee.login = %q, want admin", action, login)
		}
	}
	for _, action := range []string{"milestoned", "demilestoned"} {
		if title := nestedString(t, sink.payloadFor(t, "pull_request", action), "milestone", "title"); title != "v1" {
			t.Errorf("pull_request.%s milestone.title = %q, want v1", action, title)
		}
	}
	for _, action := range []string{"review_requested", "review_request_removed"} {
		got := nestedString(t, sink.payloadFor(t, "pull_request", action), "requested_reviewer", "login")
		if got != reviewer.Login {
			t.Errorf("pull_request.%s requested_reviewer.login = %q, want %q", action, got, reviewer.Login)
		}
	}
	if from := nestedString(t, sink.payloadFor(t, "pull_request", "edited"), "changes", "title", "from"); from != "original title" {
		t.Errorf("pull_request.edited changes.title.from = %q, want %q", from, "original title")
	}
	// Every pull_request payload keeps the `number` + `pull_request` members a
	// consumer keys on, whichever action it carries.
	draft := sink.payloadFor(t, "pull_request", "converted_to_draft")
	if number, _ := draft["number"].(float64); int(number) != 1 {
		t.Errorf("pull_request.converted_to_draft number = %v, want 1", draft["number"])
	}
	if head := nestedString(t, draft, "pull_request", "head", "ref"); head != "feat" {
		t.Errorf("pull_request.converted_to_draft head.ref = %q, want feat", head)
	}
}

func TestWebhookReleaseActionSetIsComplete(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	sink := &webhookActionSink{}
	receiver, cleanup := startWebhookReceiver(t, sink.handler())
	defer cleanup()

	const repo = "admin/wh-release-actions"
	expectStatus(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "wh-release-actions", "auto_init": true,
	}), http.StatusCreated, "create repo")

	s.subscribeAllEvents(t, repo, receiver)

	// A release published straight away is `created` (it was never a draft),
	// then `published`, then the flavour of publish it was.
	created := decodeJSONWithStatus(t, s.post(t, "/api/v3/repos/"+repo+"/releases", defaultToken, map[string]interface{}{
		"tag_name": "v1.0.0", "name": "one",
	}), http.StatusCreated)
	releaseID := int(created["id"].(float64))

	expectStatus(t, s.post(t, "/api/v3/repos/"+repo+"/releases", defaultToken, map[string]interface{}{
		"tag_name": "v2.0.0-rc1", "name": "two", "prerelease": true,
	}), http.StatusCreated, "create prerelease")

	expectStatus(t, s.patch(t, fmt.Sprintf("/api/v3/repos/%s/releases/%d", repo, releaseID), defaultToken,
		map[string]interface{}{"name": "one (renamed)"}), http.StatusOK, "edit release")
	expectStatus(t, s.delete(t, fmt.Sprintf("/api/v3/repos/%s/releases/%d", repo, releaseID), defaultToken),
		http.StatusNoContent, "delete release")

	sink.requireActions(t, "release", []string{
		"created", "published", "released",
		"created", "published", "prereleased",
		"edited", "deleted",
	})

	if tag := nestedString(t, sink.payloadFor(t, "release", "released"), "release", "tag_name"); tag != "v1.0.0" {
		t.Errorf("release.released tag_name = %q, want v1.0.0", tag)
	}
	if tag := nestedString(t, sink.payloadFor(t, "release", "prereleased"), "release", "tag_name"); tag != "v2.0.0-rc1" {
		t.Errorf("release.prereleased tag_name = %q, want v2.0.0-rc1", tag)
	}
}

// requireNoGQLErrors fails when a GraphQL response carries an errors array —
// a refused mutation would silently emit nothing and the action assertion
// would report a confusing missing-event instead of the real cause.
func requireNoGQLErrors(t *testing.T, env map[string]interface{}) {
	t.Helper()
	if errs, _ := env["errors"].([]interface{}); len(errs) > 0 {
		t.Fatalf("graphql mutation failed: %v", errs)
	}
}

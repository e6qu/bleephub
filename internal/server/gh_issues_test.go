package bleephub

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"
)

// mustPost asserts a setup/precondition request returned a 2xx status, then
// closes its body. Setup helpers that discarded the status let a broken
// precondition surface later as a misleading assertion failure in the code
// under test; routing them through mustPost fails loudly at the setup point.
func mustPost(t *testing.T, resp *http.Response) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("setup request failed: status %d, body: %s", resp.StatusCode, body)
	}
}

// helper: create a repo for issue tests, returns owner/name.
func createTestIssueRepo(t *testing.T, name string) {
	t.Helper()
	// A failed repo create (e.g. a name collision) must fail here, not later as
	// a confusing 404 in the test that assumed the repo exists.
	mustPost(t, ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": name,
	}))
}

// --- Label tests ---

func TestCreateLabel(t *testing.T) {
	createTestIssueRepo(t, "label-test")

	resp := ghPost(t, "/api/v3/repos/admin/label-test/labels", defaultToken, map[string]interface{}{
		"name":        "bug",
		"color":       "d73a4a",
		"description": "Something is broken",
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)

	if data["name"] != "bug" {
		t.Fatalf("expected name=bug, got %v", data["name"])
	}
	if data["color"] != "d73a4a" {
		t.Fatalf("expected color=d73a4a, got %v", data["color"])
	}
	if data["description"] != "Something is broken" {
		t.Fatalf("expected description='Something is broken', got %v", data["description"])
	}
}

func TestListLabels(t *testing.T) {
	createTestIssueRepo(t, "label-list")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/label-list/labels", defaultToken, map[string]interface{}{
		"name": "enhancement", "color": "a2eeef",
	}))

	resp := ghGet(t, "/api/v3/repos/admin/label-list/labels", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	labels := decodeJSONArray(t, resp)
	if len(labels) == 0 {
		t.Fatal("expected at least 1 label")
	}
}

func TestGetLabel(t *testing.T) {
	createTestIssueRepo(t, "label-get")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/label-get/labels", defaultToken, map[string]interface{}{
		"name": "docs", "color": "0075ca",
	}))

	resp := ghGet(t, "/api/v3/repos/admin/label-get/labels/docs", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)
	if data["name"] != "docs" {
		t.Fatalf("expected name=docs, got %v", data["name"])
	}
}

func TestUpdateLabel(t *testing.T) {
	createTestIssueRepo(t, "label-update")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/label-update/labels", defaultToken, map[string]interface{}{
		"name": "wontfix", "color": "ffffff",
	}))

	resp := ghPatch(t, "/api/v3/repos/admin/label-update/labels/wontfix", defaultToken, map[string]interface{}{
		"color": "000000",
	})
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)
	if data["color"] != "000000" {
		t.Fatalf("expected color=000000, got %v", data["color"])
	}
}

func TestDeleteLabel(t *testing.T) {
	createTestIssueRepo(t, "label-delete")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/label-delete/labels", defaultToken, map[string]interface{}{
		"name": "temp", "color": "aaaaaa",
	}))

	resp := ghDelete(t, "/api/v3/repos/admin/label-delete/labels/temp", defaultToken)
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}

	resp2 := ghGet(t, "/api/v3/repos/admin/label-delete/labels/temp", "")
	defer resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("expected 404 after delete, got %d", resp2.StatusCode)
	}
}

// --- Milestone tests ---

func TestCreateMilestone(t *testing.T) {
	createTestIssueRepo(t, "ms-test")

	resp := ghPost(t, "/api/v3/repos/admin/ms-test/milestones", defaultToken, map[string]interface{}{
		"title":       "v1.0",
		"description": "First release",
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)

	if data["title"] != "v1.0" {
		t.Fatalf("expected title=v1.0, got %v", data["title"])
	}
	if data["number"] != 1.0 {
		t.Fatalf("expected number=1, got %v", data["number"])
	}
	if data["state"] != "open" {
		t.Fatalf("expected state=open, got %v", data["state"])
	}
}

func TestListMilestones(t *testing.T) {
	createTestIssueRepo(t, "ms-list")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/ms-list/milestones", defaultToken, map[string]interface{}{
		"title": "v2.0",
	}))

	resp := ghGet(t, "/api/v3/repos/admin/ms-list/milestones", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	milestones := decodeJSONArray(t, resp)
	if len(milestones) == 0 {
		t.Fatal("expected at least 1 milestone")
	}
}

func TestGetMilestone(t *testing.T) {
	createTestIssueRepo(t, "ms-get")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/ms-get/milestones", defaultToken, map[string]interface{}{
		"title": "v3.0",
	}))

	resp := ghGet(t, "/api/v3/repos/admin/ms-get/milestones/1", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)
	if data["title"] != "v3.0" {
		t.Fatalf("expected title=v3.0, got %v", data["title"])
	}
}

func TestUpdateMilestone(t *testing.T) {
	createTestIssueRepo(t, "ms-update")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/ms-update/milestones", defaultToken, map[string]interface{}{
		"title": "v4.0",
	}))

	resp := ghPatch(t, "/api/v3/repos/admin/ms-update/milestones/1", defaultToken, map[string]interface{}{
		"state": "closed",
	})
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)
	if data["state"] != "closed" {
		t.Fatalf("expected state=closed, got %v", data["state"])
	}
}

func TestDeleteMilestone(t *testing.T) {
	createTestIssueRepo(t, "ms-delete")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/ms-delete/milestones", defaultToken, map[string]interface{}{
		"title": "temp-ms",
	}))

	resp := ghDelete(t, "/api/v3/repos/admin/ms-delete/milestones/1", defaultToken)
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}

	resp2 := ghGet(t, "/api/v3/repos/admin/ms-delete/milestones/1", "")
	defer resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("expected 404 after delete, got %d", resp2.StatusCode)
	}
}

// --- Issue REST tests ---

func TestCreateIssueREST(t *testing.T) {
	createTestIssueRepo(t, "issue-create")

	resp := ghPost(t, "/api/v3/repos/admin/issue-create/issues", defaultToken, map[string]interface{}{
		"title": "First issue",
		"body":  "This is a test",
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)

	if data["title"] != "First issue" {
		t.Fatalf("expected title='First issue', got %v", data["title"])
	}
	if data["number"] != 1.0 {
		t.Fatalf("expected number=1, got %v", data["number"])
	}
	if data["state"] != "open" {
		t.Fatalf("expected state=open, got %v", data["state"])
	}
	if data["user"] == nil {
		t.Fatal("missing user")
	}
}

func TestListIssuesREST(t *testing.T) {
	createTestIssueRepo(t, "issue-list")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-list/issues", defaultToken, map[string]interface{}{
		"title": "List test issue",
	}))

	resp := ghGet(t, "/api/v3/repos/admin/issue-list/issues", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	issues := decodeJSONArray(t, resp)
	if len(issues) == 0 {
		t.Fatal("expected at least 1 issue")
	}
}

func TestListIssuesRESTHonorsDocumentedFiltersAndOrdering(t *testing.T) {
	const repo = "issue-list-fidelity"
	createTestIssueRepo(t, repo)
	mustPost(t, ghPost(t, "/api/v3/repos/admin/"+repo+"/milestones", defaultToken, map[string]interface{}{
		"title": "v1",
	}))
	mustPost(t, ghPost(t, "/api/v3/repos/admin/"+repo+"/issues", defaultToken, map[string]interface{}{
		"title": "target", "body": "Please ask @alice", "milestone": 1,
		"assignees": []string{"admin"},
	}))
	mustPost(t, ghPost(t, "/api/v3/repos/admin/"+repo+"/issues", defaultToken, map[string]interface{}{
		"title": "most discussed",
	}))
	for _, body := range []string{"one", "two"} {
		mustPost(t, ghPost(t, "/api/v3/repos/admin/"+repo+"/issues/2/comments", defaultToken, map[string]interface{}{
			"body": body,
		}))
	}

	assertNumbers := func(query string, want ...int) {
		t.Helper()
		resp := ghGet(t, "/api/v3/repos/admin/"+repo+"/issues?"+query, defaultToken)
		if resp.StatusCode != 200 {
			resp.Body.Close()
			t.Fatalf("%s: status = %d, want 200", query, resp.StatusCode)
		}
		rows := decodeJSONArray(t, resp)
		if len(rows) != len(want) {
			t.Fatalf("%s: got %d rows, want %d: %v", query, len(rows), len(want), rows)
		}
		for index, number := range want {
			if got := int(rows[index]["number"].(float64)); got != number {
				t.Fatalf("%s: row %d number = %d, want %d", query, index, got, number)
			}
		}
	}

	assertNumbers("milestone=1", 1)
	assertNumbers("mentioned=alice", 1)
	assertNumbers("assignee=does-not-exist")
	assertNumbers("creator=does-not-exist")
	assertNumbers("since=2999-01-01T00:00:00Z")
	assertNumbers("sort=comments&direction=desc", 2, 1)
	assertNumbers("sort=comments&direction=asc", 1, 2)

	for _, query := range []string{"state=wat", "sort=wat", "direction=wat", "since=yesterday"} {
		resp := ghGet(t, "/api/v3/repos/admin/"+repo+"/issues?"+query, defaultToken)
		resp.Body.Close()
		if resp.StatusCode != 422 {
			t.Fatalf("%s: status = %d, want 422", query, resp.StatusCode)
		}
	}
}

func TestGetIssueREST(t *testing.T) {
	createTestIssueRepo(t, "issue-get")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-get/issues", defaultToken, map[string]interface{}{
		"title": "Get test",
	}))

	resp := ghGet(t, "/api/v3/repos/admin/issue-get/issues/1", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)
	if data["title"] != "Get test" {
		t.Fatalf("expected title='Get test', got %v", data["title"])
	}
}

func TestUpdateIssueREST(t *testing.T) {
	createTestIssueRepo(t, "issue-update")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-update/issues", defaultToken, map[string]interface{}{
		"title": "Update test",
	}))

	resp := ghPatch(t, "/api/v3/repos/admin/issue-update/issues/1", defaultToken, map[string]interface{}{
		"state": "closed",
	})
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)
	if data["state"] != "closed" {
		t.Fatalf("expected state=closed, got %v", data["state"])
	}
}

func TestUpdateIssueMilestoneLabelsAssigneesREST(t *testing.T) {
	createTestIssueRepo(t, "issue-update-fields")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-update-fields/labels", defaultToken, map[string]interface{}{
		"name": "bug", "color": "d73a4a",
	}))
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-update-fields/milestones", defaultToken, map[string]interface{}{
		"title": "v1.0",
	}))
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-update-fields/issues", defaultToken, map[string]interface{}{
		"title": "Field update test",
	}))

	// PATCH sets milestone (by number), labels, and assignees.
	resp := ghPatch(t, "/api/v3/repos/admin/issue-update-fields/issues/1", defaultToken, map[string]interface{}{
		"milestone": 1,
		"labels":    []string{"bug"},
		"assignees": []string{"admin"},
	})
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)
	ms, _ := data["milestone"].(map[string]interface{})
	if ms == nil || ms["title"] != "v1.0" {
		t.Fatalf("expected milestone v1.0, got %v", data["milestone"])
	}
	labels, _ := data["labels"].([]interface{})
	if len(labels) != 1 {
		t.Fatalf("expected 1 label, got %v", data["labels"])
	}
	assignees, _ := data["assignees"].([]interface{})
	if len(assignees) != 1 {
		t.Fatalf("expected 1 assignee, got %v", data["assignees"])
	}

	// An explicit null milestone clears it; empty arrays clear labels/assignees.
	resp = ghPatch(t, "/api/v3/repos/admin/issue-update-fields/issues/1", defaultToken, map[string]interface{}{
		"milestone": nil,
		"labels":    []string{},
		"assignees": []string{},
	})
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data = decodeJSON(t, resp)
	if data["milestone"] != nil {
		t.Fatalf("expected milestone cleared, got %v", data["milestone"])
	}
	if cleared, _ := data["labels"].([]interface{}); len(cleared) != 0 {
		t.Fatalf("expected labels cleared, got %v", data["labels"])
	}

	// An unknown milestone number 422s without mutating the issue.
	resp = ghPatch(t, "/api/v3/repos/admin/issue-update-fields/issues/1", defaultToken, map[string]interface{}{
		"milestone": 99,
	})
	resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("expected 422 for unknown milestone, got %d", resp.StatusCode)
	}
}

// --- Comment REST tests ---

func TestCreateCommentREST(t *testing.T) {
	createTestIssueRepo(t, "comment-create")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/comment-create/issues", defaultToken, map[string]interface{}{
		"title": "Comment test",
	}))

	resp := ghPost(t, "/api/v3/repos/admin/comment-create/issues/1/comments", defaultToken, map[string]interface{}{
		"body": "A comment",
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)
	if data["body"] != "A comment" {
		t.Fatalf("expected body='A comment', got %v", data["body"])
	}
	if data["user"] == nil {
		t.Fatal("missing user in comment")
	}
}

func TestListCommentsREST(t *testing.T) {
	createTestIssueRepo(t, "comment-list")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/comment-list/issues", defaultToken, map[string]interface{}{
		"title": "Comment list test",
	}))
	mustPost(t, ghPost(t, "/api/v3/repos/admin/comment-list/issues/1/comments", defaultToken, map[string]interface{}{
		"body": "Comment 1",
	}))

	resp := ghGet(t, "/api/v3/repos/admin/comment-list/issues/1/comments", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	comments := decodeJSONArray(t, resp)
	if len(comments) == 0 {
		t.Fatal("expected at least 1 comment")
	}
}

// --- Issue label management tests ---

func TestAddIssueLabelsREST(t *testing.T) {
	createTestIssueRepo(t, "issue-addlabels")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-addlabels/labels", defaultToken, map[string]interface{}{
		"name": "bug", "color": "d73a4a",
	}))
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-addlabels/issues", defaultToken, map[string]interface{}{
		"title": "Label test",
	}))

	resp := ghPost(t, "/api/v3/repos/admin/issue-addlabels/issues/1/labels", defaultToken, map[string]interface{}{
		"labels": []string{"bug"},
	})
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	labels := decodeJSONArray(t, resp)
	if len(labels) == 0 {
		t.Fatal("expected at least 1 label")
	}
	if labels[0]["name"] != "bug" {
		t.Fatalf("expected label name=bug, got %v", labels[0]["name"])
	}
}

func TestRemoveIssueLabelREST(t *testing.T) {
	createTestIssueRepo(t, "issue-rmlabel")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-rmlabel/labels", defaultToken, map[string]interface{}{
		"name": "remove-me", "color": "ffffff",
	}))
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-rmlabel/issues", defaultToken, map[string]interface{}{
		"title": "Remove label test",
	}))
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-rmlabel/issues/1/labels", defaultToken, map[string]interface{}{
		"labels": []string{"remove-me"},
	}))

	resp := ghDelete(t, "/api/v3/repos/admin/issue-rmlabel/issues/1/labels/remove-me", defaultToken)
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
}

// --- Issue label set/clear tests ---

func TestSetIssueLabelsREST(t *testing.T) {
	createTestIssueRepo(t, "issue-setlabels")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-setlabels/labels", defaultToken, map[string]interface{}{
		"name": "bug", "color": "d73a4a",
	}))
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-setlabels/labels", defaultToken, map[string]interface{}{
		"name": "feature", "color": "a2eeef",
	}))
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-setlabels/issues", defaultToken, map[string]interface{}{
		"title":  "Set labels test",
		"labels": []string{"bug"},
	}))

	resp := ghPut(t, "/api/v3/repos/admin/issue-setlabels/issues/1/labels", defaultToken, map[string]interface{}{
		"labels": []string{"feature"},
	})
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	labels := decodeJSONArray(t, resp)
	if len(labels) != 1 {
		t.Fatalf("expected 1 label, got %d", len(labels))
	}
	if labels[0]["name"] != "feature" {
		t.Fatalf("expected feature, got %v", labels[0]["name"])
	}
}

func TestClearIssueLabelsREST(t *testing.T) {
	createTestIssueRepo(t, "issue-clearlabels")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-clearlabels/labels", defaultToken, map[string]interface{}{
		"name": "bug", "color": "d73a4a",
	}))
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-clearlabels/issues", defaultToken, map[string]interface{}{
		"title":  "Clear labels test",
		"labels": []string{"bug"},
	}))

	resp := ghDelete(t, "/api/v3/repos/admin/issue-clearlabels/issues/1/labels", defaultToken)
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}

	resp2 := ghGet(t, "/api/v3/repos/admin/issue-clearlabels/issues/1", "")
	data := decodeJSON(t, resp2)
	if labels, _ := data["labels"].([]interface{}); len(labels) != 0 {
		t.Fatalf("expected 0 labels, got %d", len(labels))
	}
}

// --- Issue assignee tests ---

func TestAddIssueAssigneesREST(t *testing.T) {
	createTestIssueRepo(t, "issue-addassignees")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-addassignees/issues", defaultToken, map[string]interface{}{
		"title": "Assignee test",
	}))

	resp := ghPost(t, "/api/v3/repos/admin/issue-addassignees/issues/1/assignees", defaultToken, map[string]interface{}{
		"assignees": []string{"admin"},
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)
	assignees, _ := data["assignees"].([]interface{})
	if len(assignees) != 1 {
		t.Fatalf("expected 1 assignee, got %d", len(assignees))
	}
}

func TestRemoveIssueAssigneesREST(t *testing.T) {
	createTestIssueRepo(t, "issue-rmassignees")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-rmassignees/issues", defaultToken, map[string]interface{}{
		"title":     "Remove assignee test",
		"assignees": []string{"admin"},
	}))

	resp := ghDeleteWithBody(t, "/api/v3/repos/admin/issue-rmassignees/issues/1/assignees", defaultToken, map[string]interface{}{
		"assignees": []string{"admin"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// --- Repo-level comment tests ---

func TestListRepoIssueCommentsREST(t *testing.T) {
	createTestIssueRepo(t, "issue-repo-comments")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-repo-comments/issues", defaultToken, map[string]interface{}{
		"title": "Repo comments test",
	}))
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-repo-comments/issues/1/comments", defaultToken, map[string]interface{}{
		"body": "Comment 1",
	}))

	resp := ghGet(t, "/api/v3/repos/admin/issue-repo-comments/issues/comments", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	comments := decodeJSONArray(t, resp)
	if len(comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(comments))
	}
}

func TestGetIssueCommentREST(t *testing.T) {
	createTestIssueRepo(t, "issue-get-comment")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-get-comment/issues", defaultToken, map[string]interface{}{
		"title": "Get comment test",
	}))
	resp := ghPost(t, "/api/v3/repos/admin/issue-get-comment/issues/1/comments", defaultToken, map[string]interface{}{
		"body": "A comment",
	})
	data := decodeJSON(t, resp)
	commentID := int(data["id"].(float64))

	resp2 := ghGet(t, "/api/v3/repos/admin/issue-get-comment/issues/comments/"+strconv.Itoa(commentID), "")
	if resp2.StatusCode != 200 {
		resp2.Body.Close()
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}
	got := decodeJSON(t, resp2)
	if got["body"] != "A comment" {
		t.Fatalf("expected body='A comment', got %v", got["body"])
	}
}

func TestPinIssueCommentREST(t *testing.T) {
	createTestIssueRepo(t, "issue-pin-comment")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-pin-comment/issues", defaultToken, map[string]interface{}{
		"title": "Pin comment test",
	}))
	resp := ghPost(t, "/api/v3/repos/admin/issue-pin-comment/issues/1/comments", defaultToken, map[string]interface{}{
		"body": "Pin me",
	})
	data := decodeJSON(t, resp)
	commentID := int(data["id"].(float64))

	resp2 := ghPut(t, "/api/v3/repos/admin/issue-pin-comment/issues/comments/"+strconv.Itoa(commentID)+"/pin", defaultToken, nil)
	if resp2.StatusCode != 200 {
		resp2.Body.Close()
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}
}

// --- Issue timeline + events tests ---

func TestListIssueTimelineREST(t *testing.T) {
	createTestIssueRepo(t, "issue-timeline")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-timeline/issues", defaultToken, map[string]interface{}{
		"title": "Timeline test",
	}))
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-timeline/issues/1/comments", defaultToken, map[string]interface{}{
		"body": "Timeline comment",
	}))

	resp := ghGet(t, "/api/v3/repos/admin/issue-timeline/issues/1/timeline", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	timeline := decodeJSONArray(t, resp)
	if len(timeline) == 0 {
		t.Fatal("expected non-empty timeline")
	}
}

func TestListIssueEventsREST(t *testing.T) {
	createTestIssueRepo(t, "issue-events")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-events/issues", defaultToken, map[string]interface{}{
		"title": "Events test",
	}))

	resp := ghGet(t, "/api/v3/repos/admin/issue-events/issues/1/events", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	events := decodeJSONArray(t, resp)
	if len(events) == 0 {
		t.Fatal("expected non-empty events")
	}
}

func TestListRepoIssueEventsREST(t *testing.T) {
	createTestIssueRepo(t, "issue-repo-events")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-repo-events/issues", defaultToken, map[string]interface{}{
		"title": "Repo events test",
	}))

	resp := ghGet(t, "/api/v3/repos/admin/issue-repo-events/issues/events", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	events := decodeJSONArray(t, resp)
	if len(events) == 0 {
		t.Fatal("expected non-empty events")
	}
}

func TestGetIssueEventREST(t *testing.T) {
	createTestIssueRepo(t, "issue-event-get")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-event-get/issues", defaultToken, map[string]interface{}{
		"title": "Get event test",
	}))

	listResp := ghGet(t, "/api/v3/repos/admin/issue-event-get/issues/events", "")
	if listResp.StatusCode != 200 {
		listResp.Body.Close()
		t.Fatalf("expected 200 listing events, got %d", listResp.StatusCode)
	}
	var events []map[string]interface{}
	if err := json.NewDecoder(listResp.Body).Decode(&events); err != nil {
		listResp.Body.Close()
		t.Fatal(err)
	}
	listResp.Body.Close()
	if len(events) == 0 {
		t.Fatal("expected non-empty events")
	}
	eventID := strconv.Itoa(int(events[0]["id"].(float64)))

	resp := ghGet(t, "/api/v3/repos/admin/issue-event-get/issues/events/"+eventID, "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)
	if data["event"] != "opened" {
		t.Fatalf("expected event=opened, got %v", data["event"])
	}
}

// --- Sub-issues / dependencies ---

func TestSubIssuesREST_EmptyList(t *testing.T) {
	createTestIssueRepo(t, "issue-subissues")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/issue-subissues/issues", defaultToken, map[string]interface{}{
		"title": "Sub issues test",
	}))

	resp := ghGet(t, "/api/v3/repos/admin/issue-subissues/issues/1/sub_issues", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	items := decodeJSONArray(t, resp)
	if len(items) != 0 {
		t.Fatalf("expected empty sub_issues, got %d", len(items))
	}
}

// --- GraphQL tests ---

func TestGraphQLCreateIssue(t *testing.T) {
	// Create repo and get its node ID
	resp := ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "gql-issue-create",
	})
	repoData := decodeJSON(t, resp)
	repoNodeID := repoData["node_id"].(string)

	resp2 := ghPost(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `mutation($input: CreateIssueInput!) { createIssue(input: $input) { issue { number title state url } } }`,
		"variables": map[string]interface{}{
			"input": map[string]interface{}{
				"repositoryId": repoNodeID,
				"title":        "GQL Issue",
				"body":         "Created via GraphQL",
			},
		},
	})
	if resp2.StatusCode != 200 {
		resp2.Body.Close()
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}
	data := decodeJSON(t, resp2)

	d, _ := data["data"].(map[string]interface{})
	if d == nil {
		t.Fatalf("expected data, got errors: %v", data)
	}
	payload, _ := d["createIssue"].(map[string]interface{})
	issue, _ := payload["issue"].(map[string]interface{})
	if issue == nil {
		t.Fatalf("expected issue in payload: %v", data)
	}
	if issue["title"] != "GQL Issue" {
		t.Fatalf("expected title='GQL Issue', got %v", issue["title"])
	}
	if issue["number"] != 1.0 {
		t.Fatalf("expected number=1, got %v", issue["number"])
	}
	if issue["state"] != "OPEN" {
		t.Fatalf("expected state=OPEN, got %v", issue["state"])
	}
}

func TestGraphQLCloseIssue(t *testing.T) {
	resp := ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "gql-issue-close",
	})
	repoData := decodeJSON(t, resp)
	repoNodeID := repoData["node_id"].(string)

	// Create issue
	resp2 := ghPost(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `mutation($input: CreateIssueInput!) { createIssue(input: $input) { issue { id } } }`,
		"variables": map[string]interface{}{
			"input": map[string]interface{}{
				"repositoryId": repoNodeID,
				"title":        "To close",
			},
		},
	})
	data2 := decodeJSON(t, resp2)
	d2, _ := data2["data"].(map[string]interface{})
	ci, _ := d2["createIssue"].(map[string]interface{})
	iss, _ := ci["issue"].(map[string]interface{})
	issueID := iss["id"].(string)

	// Close
	resp3 := ghPost(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `mutation($input: CloseIssueInput!) { closeIssue(input: $input) { issue { state stateReason } } }`,
		"variables": map[string]interface{}{
			"input": map[string]interface{}{
				"issueId": issueID,
			},
		},
	})
	if resp3.StatusCode != 200 {
		resp3.Body.Close()
		t.Fatalf("expected 200, got %d", resp3.StatusCode)
	}
	data := decodeJSON(t, resp3)

	d, _ := data["data"].(map[string]interface{})
	if d == nil {
		t.Fatalf("expected data, got errors: %v", data)
	}
	cl, _ := d["closeIssue"].(map[string]interface{})
	issue, _ := cl["issue"].(map[string]interface{})
	if issue["state"] != "CLOSED" {
		t.Fatalf("expected state=CLOSED, got %v", issue["state"])
	}
	if issue["stateReason"] != "COMPLETED" {
		t.Fatalf("expected stateReason=COMPLETED, got %v", issue["stateReason"])
	}
}

func TestGraphQLReopenIssue(t *testing.T) {
	resp := ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "gql-issue-reopen",
	})
	repoData := decodeJSON(t, resp)
	repoNodeID := repoData["node_id"].(string)

	// Create and close
	resp2 := ghPost(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `mutation($input: CreateIssueInput!) { createIssue(input: $input) { issue { id } } }`,
		"variables": map[string]interface{}{
			"input": map[string]interface{}{
				"repositoryId": repoNodeID,
				"title":        "To reopen",
			},
		},
	})
	data2 := decodeJSON(t, resp2)
	d2, _ := data2["data"].(map[string]interface{})
	ci, _ := d2["createIssue"].(map[string]interface{})
	iss, _ := ci["issue"].(map[string]interface{})
	issueID := iss["id"].(string)

	mustPost(t, ghPost(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `mutation($input: CloseIssueInput!) { closeIssue(input: $input) { issue { state } } }`,
		"variables": map[string]interface{}{
			"input": map[string]interface{}{"issueId": issueID},
		},
	}))

	// Reopen
	resp3 := ghPost(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `mutation($input: ReopenIssueInput!) { reopenIssue(input: $input) { issue { state } } }`,
		"variables": map[string]interface{}{
			"input": map[string]interface{}{"issueId": issueID},
		},
	})
	data := decodeJSON(t, resp3)
	d, _ := data["data"].(map[string]interface{})
	ro, _ := d["reopenIssue"].(map[string]interface{})
	issue, _ := ro["issue"].(map[string]interface{})
	if issue["state"] != "OPEN" {
		t.Fatalf("expected state=OPEN, got %v", issue["state"])
	}
}

func TestGraphQLAddComment(t *testing.T) {
	resp := ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "gql-comment",
	})
	repoData := decodeJSON(t, resp)
	repoNodeID := repoData["node_id"].(string)

	resp2 := ghPost(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `mutation($input: CreateIssueInput!) { createIssue(input: $input) { issue { id } } }`,
		"variables": map[string]interface{}{
			"input": map[string]interface{}{
				"repositoryId": repoNodeID,
				"title":        "Comment target",
			},
		},
	})
	data2 := decodeJSON(t, resp2)
	d2, _ := data2["data"].(map[string]interface{})
	ci, _ := d2["createIssue"].(map[string]interface{})
	iss, _ := ci["issue"].(map[string]interface{})
	issueID := iss["id"].(string)

	resp3 := ghPost(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `mutation($input: AddCommentInput!) { addComment(input: $input) { commentEdge { node { body } } } }`,
		"variables": map[string]interface{}{
			"input": map[string]interface{}{
				"subjectId": issueID,
				"body":      "GQL comment",
			},
		},
	})
	if resp3.StatusCode != 200 {
		resp3.Body.Close()
		t.Fatalf("expected 200, got %d", resp3.StatusCode)
	}
	data := decodeJSON(t, resp3)
	d, _ := data["data"].(map[string]interface{})
	if d == nil {
		t.Fatalf("expected data, got errors: %v", data)
	}
	ac, _ := d["addComment"].(map[string]interface{})
	edge, _ := ac["commentEdge"].(map[string]interface{})
	node, _ := edge["node"].(map[string]interface{})
	if node["body"] != "GQL comment" {
		t.Fatalf("expected body='GQL comment', got %v", node["body"])
	}
}

func TestGraphQLListIssues(t *testing.T) {
	createTestIssueRepo(t, "gql-issue-list")

	mustPost(t, ghPost(t, "/api/v3/repos/admin/gql-issue-list/issues", defaultToken, map[string]interface{}{
		"title": "GQL list issue 1",
	}))
	mustPost(t, ghPost(t, "/api/v3/repos/admin/gql-issue-list/issues", defaultToken, map[string]interface{}{
		"title": "GQL list issue 2",
	}))

	resp := ghPost(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `{repository(owner:"admin",name:"gql-issue-list"){issues(first:10,states:[OPEN]){totalCount,nodes{number,title,state}}}}`,
	})
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)

	d, _ := data["data"].(map[string]interface{})
	repo, _ := d["repository"].(map[string]interface{})
	issues, _ := repo["issues"].(map[string]interface{})
	if issues == nil {
		t.Fatalf("expected issues in repository: %v", data)
	}
	tc, _ := issues["totalCount"].(float64)
	if tc < 2 {
		t.Fatalf("expected totalCount >= 2, got %v", tc)
	}
	nodes, _ := issues["nodes"].([]interface{})
	if len(nodes) < 2 {
		t.Fatalf("expected >= 2 nodes, got %d", len(nodes))
	}
}

func TestGraphQLGetIssue(t *testing.T) {
	createTestIssueRepo(t, "gql-issue-get")

	mustPost(t, ghPost(t, "/api/v3/repos/admin/gql-issue-get/issues", defaultToken, map[string]interface{}{
		"title": "GQL get issue",
		"body":  "Get by number",
	}))

	resp := ghPost(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `{repository(owner:"admin",name:"gql-issue-get"){issue(number:1){title,body,state,author{login},labels(first:10){nodes{name}},comments(first:10){nodes{body}}}}}`,
	})
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)

	d, _ := data["data"].(map[string]interface{})
	repo, _ := d["repository"].(map[string]interface{})
	issue, _ := repo["issue"].(map[string]interface{})
	if issue == nil {
		t.Fatalf("expected issue: %v", data)
	}
	if issue["title"] != "GQL get issue" {
		t.Fatalf("expected title='GQL get issue', got %v", issue["title"])
	}
	if issue["body"] != "Get by number" {
		t.Fatalf("expected body='Get by number', got %v", issue["body"])
	}
	author, _ := issue["author"].(map[string]interface{})
	if author == nil || author["login"] != "admin" {
		t.Fatalf("expected author.login=admin, got %v", author)
	}
}

func TestGraphQLRepoLabels(t *testing.T) {
	createTestIssueRepo(t, "gql-labels")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/gql-labels/labels", defaultToken, map[string]interface{}{
		"name": "gql-bug", "color": "d73a4a",
	}))

	resp := ghPost(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `{repository(owner:"admin",name:"gql-labels"){labels(first:10){totalCount,nodes{name,color}}}}`,
	})
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)

	d, _ := data["data"].(map[string]interface{})
	repo, _ := d["repository"].(map[string]interface{})
	labels, _ := repo["labels"].(map[string]interface{})
	if labels == nil {
		t.Fatalf("expected labels: %v", data)
	}
	tc, _ := labels["totalCount"].(float64)
	if tc < 1 {
		t.Fatalf("expected totalCount >= 1, got %v", tc)
	}
}

func TestGraphQLRepoMilestones(t *testing.T) {
	createTestIssueRepo(t, "gql-milestones")
	mustPost(t, ghPost(t, "/api/v3/repos/admin/gql-milestones/milestones", defaultToken, map[string]interface{}{
		"title": "gql-v1",
	}))

	resp := ghPost(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `{repository(owner:"admin",name:"gql-milestones"){milestones(first:10){totalCount,nodes{title,number,state}}}}`,
	})
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)

	d, _ := data["data"].(map[string]interface{})
	repo, _ := d["repository"].(map[string]interface{})
	milestones, _ := repo["milestones"].(map[string]interface{})
	if milestones == nil {
		t.Fatalf("expected milestones: %v", data)
	}
	tc, _ := milestones["totalCount"].(float64)
	if tc < 1 {
		t.Fatalf("expected totalCount >= 1, got %v", tc)
	}
}

func TestGraphQLUpdateIssue(t *testing.T) {
	resp := ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "gql-issue-update",
	})
	repoData := decodeJSON(t, resp)
	repoNodeID := repoData["node_id"].(string)

	resp2 := ghPost(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `mutation($input: CreateIssueInput!) { createIssue(input: $input) { issue { id } } }`,
		"variables": map[string]interface{}{
			"input": map[string]interface{}{
				"repositoryId": repoNodeID,
				"title":        "Before update",
			},
		},
	})
	data2 := decodeJSON(t, resp2)
	d2, _ := data2["data"].(map[string]interface{})
	ci, _ := d2["createIssue"].(map[string]interface{})
	iss, _ := ci["issue"].(map[string]interface{})
	issueID := iss["id"].(string)

	resp3 := ghPost(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `mutation($input: UpdateIssueInput!) { updateIssue(input: $input) { issue { title } } }`,
		"variables": map[string]interface{}{
			"input": map[string]interface{}{
				"id":    issueID,
				"title": "After update",
			},
		},
	})
	if resp3.StatusCode != 200 {
		resp3.Body.Close()
		t.Fatalf("expected 200, got %d", resp3.StatusCode)
	}
	data := decodeJSON(t, resp3)
	d, _ := data["data"].(map[string]interface{})
	if d == nil {
		t.Fatalf("expected data, got errors: %v", data)
	}
	ui, _ := d["updateIssue"].(map[string]interface{})
	issue, _ := ui["issue"].(map[string]interface{})
	if issue["title"] != "After update" {
		t.Fatalf("expected title='After update', got %v", issue["title"])
	}
}

func TestGraphQLHasIssuesEnabled(t *testing.T) {
	createTestIssueRepo(t, "gql-has-issues")

	resp := ghPost(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `{repository(owner:"admin",name:"gql-has-issues"){hasIssuesEnabled,viewerPermission}}`,
	})
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)
	d, _ := data["data"].(map[string]interface{})
	repo, _ := d["repository"].(map[string]interface{})
	if repo["hasIssuesEnabled"] != true {
		t.Fatalf("expected hasIssuesEnabled=true, got %v", repo["hasIssuesEnabled"])
	}
	if repo["viewerPermission"] != "ADMIN" {
		t.Fatalf("expected viewerPermission=ADMIN, got %v", repo["viewerPermission"])
	}
}

// TestGraphQLViewerPermissionNotHardcoded proves viewerPermission is computed
// from the viewer's real access, not a hardcoded ADMIN: an anonymous viewer of
// a public repo gets READ (not ADMIN).
func TestGraphQLViewerPermissionNotHardcoded(t *testing.T) {
	createTestIssueRepo(t, "gql-viewer-perm")

	// A signed-in user who is not the owner. The endpoint requires
	// authentication, so the earlier anonymous form of this test could not
	// distinguish "viewerPermission is computed" from "the request was
	// refused" — an authenticated non-owner tests the same property and is a
	// request GitHub would actually serve.
	store := testServer.store
	store.mu.Lock()
	outsider := &User{ID: store.NextUser, Login: "gql-viewer-perm-outsider", Type: "User"}
	store.Users[outsider.ID] = outsider
	store.UsersByLogin[outsider.Login] = outsider
	store.NextUser++
	store.mu.Unlock()
	outsiderToken := store.CreateToken(outsider.ID, "repo")

	resp := ghPost(t, "/api/graphql", outsiderToken.Value, map[string]interface{}{
		"query": `{repository(owner:"admin",name:"gql-viewer-perm"){viewerPermission}}`,
	})
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	data := decodeJSON(t, resp)
	d, _ := data["data"].(map[string]interface{})
	repo, _ := d["repository"].(map[string]interface{})
	if got := repo["viewerPermission"]; got == "ADMIN" {
		t.Fatalf("anonymous viewer must not get ADMIN (hardcoded), got %v", got)
	}
}

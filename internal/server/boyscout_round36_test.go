package bleephub

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestCursorPaginateKeepsPositionOrderedPages pins that cursor pagination
// returns every element exactly once even when the slice is NOT ordered
// ascending by the cursor identity — the case for Projects V2 items ordered by
// manual position after a reorder. Previously the `after` scan assumed
// ascending-by-id and looked for the first id greater than the cursor, so a
// reordered list silently dropped every item whose id was below the cursor's.
func TestCursorPaginateKeepsPositionOrderedPages(t *testing.T) {
	t.Parallel()
	// Position order [3,1,2]: ids no longer ascending (a drag reordered them).
	items := []int{3, 1, 2}
	idOf := func(x int) int { return x }

	seen := map[int]int{}
	var order []int
	after := ""
	for i := 0; i < 10; i++ { // bounded to catch an accidental infinite loop
		target := "/x?per_page=1"
		if after != "" {
			target += "&after=" + url.QueryEscape(after)
		}
		r := httptest.NewRequest("GET", target, nil)
		page, pi := cursorPaginate(r, items, idOf)
		for _, it := range page {
			seen[it]++
			order = append(order, it)
		}
		if !pi.HasNext {
			break
		}
		after = pi.Next
	}

	if len(seen) != 3 || seen[1] != 1 || seen[2] != 1 || seen[3] != 1 {
		t.Fatalf("paginated view = %v (counts %v), want each of 1,2,3 exactly once", order, seen)
	}
	// Page order must follow the given (position) order, not id order.
	if got := [3]int{order[0], order[1], order[2]}; got != [3]int{3, 1, 2} {
		t.Fatalf("page order = %v, want position order [3 1 2]", got)
	}
}

// TestGistCommentAuthorAssociationForNonOwner pins that a gist comment reports
// author_association OWNER only for the gist's own author; a third party who
// comments on a public gist is NONE. It was hardcoded OWNER for everyone.
func TestGistCommentAuthorAssociationForNonOwner(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	_, otherToken := s.userSurfaceUser(t, "gistcommenter")

	// admin (defaultToken) owns a public gist.
	resp := s.post(t, "/api/v3/gists", defaultToken, map[string]interface{}{
		"public": true,
		"files":  map[string]interface{}{"a.txt": map[string]interface{}{"content": "hi"}},
	})
	requireStatusNoClose(t, resp, 201)
	gist := decodeJSON(t, resp)
	gistID, _ := gist["id"].(string)
	if gistID == "" {
		t.Fatal("gist id missing")
	}

	// A non-owner comment is NONE.
	resp = s.post(t, "/api/v3/gists/"+gistID+"/comments", otherToken, map[string]interface{}{"body": "nice"})
	requireStatusNoClose(t, resp, 201)
	c := decodeJSON(t, resp)
	if got := c["author_association"]; got != "NONE" {
		t.Fatalf("non-owner comment author_association = %v, want NONE", got)
	}

	// The owner's own comment is OWNER.
	resp = s.post(t, "/api/v3/gists/"+gistID+"/comments", defaultToken, map[string]interface{}{"body": "thanks"})
	requireStatusNoClose(t, resp, 201)
	oc := decodeJSON(t, resp)
	if got := oc["author_association"]; got != "OWNER" {
		t.Fatalf("owner comment author_association = %v, want OWNER", got)
	}
}

// TestPagesCreateConflictWhenAlreadyEnabled pins that POST .../pages returns 409
// once a site exists (PUT modifies it), rather than silently re-creating it and
// dropping https_enforced / the built status.
func TestPagesCreateConflictWhenAlreadyEnabled(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createRepoWriteRepo(t, true)

	resp := s.post(t, "/api/v3/repos/admin/"+repo+"/pages", defaultToken, map[string]interface{}{
		"source": map[string]interface{}{"branch": "main"},
	})
	requireStatus(t, resp, 201)

	resp = s.post(t, "/api/v3/repos/admin/"+repo+"/pages", defaultToken, map[string]interface{}{
		"source": map[string]interface{}{"branch": "main"},
	})
	requireStatus(t, resp, http.StatusConflict)
}

// gqlObj walks a chain of single-object fields (not connections) and returns the
// map at the end of the path.
func gqlObj(data map[string]interface{}, path ...string) map[string]interface{} {
	cur := data
	for _, key := range path {
		cur, _ = cur[key].(map[string]interface{})
	}
	return cur
}

// newDiscussionGQL creates a repo + a discussion in its first category and
// returns (owner, repoName, discussionNumber, discussionNodeID).
func newDiscussionGQL(t *testing.T, repoName string) (string, string, int, string) {
	t.Helper()
	resp := ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": repoName})
	repoData := decodeJSON(t, resp)
	owner, _ := repoData["owner"].(map[string]interface{})
	login, _ := owner["login"].(string)
	name, _ := repoData["name"].(string)
	repoNodeID, _ := repoData["node_id"].(string)

	catQuery := `query($owner:String!,$name:String!){repository(owner:$owner,name:$name){discussionCategories(first:10){nodes{id,name}}}}`
	cats := runDiscussionGQL(t, catQuery, map[string]interface{}{"owner": login, "name": name})
	catID, _ := firstNode(cats, "repository", "discussionCategories")["id"].(string)

	create := `mutation($repo:ID!,$cat:ID!){createDiscussion(input:{repositoryId:$repo,categoryId:$cat,title:"T",body:"B"}){discussion{number,id}}}`
	createRes := runDiscussionGQL(t, create, map[string]interface{}{"repo": repoNodeID, "cat": catID})
	disc, _ := createRes["createDiscussion"].(map[string]interface{})["discussion"].(map[string]interface{})
	num, _ := disc["number"].(float64)
	nodeID, _ := disc["id"].(string)
	return login, name, int(num), nodeID
}

// TestDiscussionLastEditedAtTracksContentEditsOnly pins that lastEditedAt is set
// only by a title/body edit — closing (and a category-only change) must leave it
// unset. UpdateDiscussion previously stamped lastEditedAt on every write.
func TestDiscussionLastEditedAtTracksContentEditsOnly(t *testing.T) {
	login, name, num, nodeID := newDiscussionGQL(t, "disc-lastedited")

	lastEdited := func() interface{} {
		q := `query($owner:String!,$name:String!,$num:Int!){repository(owner:$owner,name:$name){discussion(number:$num){lastEditedAt}}}`
		res := runDiscussionGQL(t, q, map[string]interface{}{"owner": login, "name": name, "num": num})
		return gqlObj(res, "repository", "discussion")["lastEditedAt"]
	}

	if v := lastEdited(); v != nil {
		t.Fatalf("new discussion lastEditedAt = %v, want null", v)
	}

	// Closing must not stamp lastEditedAt.
	runDiscussionGQL(t, `mutation($id:ID!){closeDiscussion(input:{discussionId:$id}){discussion{number}}}`,
		map[string]interface{}{"id": nodeID})
	if v := lastEdited(); v != nil {
		t.Fatalf("lastEditedAt after close = %v, want still null (close is not a content edit)", v)
	}

	// Editing the body must stamp it.
	runDiscussionGQL(t, `mutation($id:ID!){updateDiscussion(input:{discussionId:$id,body:"edited"}){discussion{number}}}`,
		map[string]interface{}{"id": nodeID})
	if v := lastEdited(); v == nil {
		t.Fatalf("lastEditedAt after body edit = null, want a timestamp")
	}
}

// TestDiscussionCommentViewerCanUpvoteRespectsLock pins that a comment's
// viewerCanUpvote goes false once its discussion is locked — mirroring the
// addUpvote mutation, which refuses to upvote a comment on a locked discussion.
func TestDiscussionCommentViewerCanUpvoteRespectsLock(t *testing.T) {
	login, name, num, nodeID := newDiscussionGQL(t, "disc-comment-upvote-lock")

	runDiscussionGQL(t, `mutation($did:ID!){addDiscussionComment(input:{discussionId:$did,body:"c"}){comment{id}}}`,
		map[string]interface{}{"did": nodeID})

	commentCanUpvote := func() bool {
		q := `query($owner:String!,$name:String!,$num:Int!){repository(owner:$owner,name:$name){discussion(number:$num){comments(first:10){nodes{viewerCanUpvote}}}}}`
		res := runDiscussionGQL(t, q, map[string]interface{}{"owner": login, "name": name, "num": num})
		comments := gqlObj(res, "repository", "discussion", "comments")
		nodes, _ := comments["nodes"].([]interface{})
		if len(nodes) == 0 {
			t.Fatalf("no discussion comments returned: %v", res)
		}
		first, _ := nodes[0].(map[string]interface{})
		v, _ := first["viewerCanUpvote"].(bool)
		return v
	}

	if !commentCanUpvote() {
		t.Fatal("comment viewerCanUpvote before lock = false, want true")
	}

	runDiscussionGQL(t, `mutation($id:ID!){lockLockable(input:{lockableId:$id,lockReason:SPAM}){lockedRecord{locked}}}`,
		map[string]interface{}{"id": nodeID})

	if commentCanUpvote() {
		t.Fatal("comment viewerCanUpvote on a locked discussion = true, want false")
	}
}

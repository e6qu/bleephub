package bleephub

import (
	"testing"
)

// TestNodeResolvesDiscussionCommentAndGistGlobalIDs pins that GraphQL
// node(id:) resolves Discussion (D_), DiscussionComment (DC_) and Gist (G_)
// global IDs — GitHub exposes all three as Node; they returned null before.
func TestNodeResolvesDiscussionCommentAndGistGlobalIDs(t *testing.T) {
	login, name, num, discNodeID := newDiscussionGQL(t, "node-globalid")
	_ = login
	_ = name
	_ = num

	addRes := runDiscussionGQL(t, `mutation($d:ID!){addDiscussionComment(input:{discussionId:$d,body:"c"}){comment{id}}}`,
		map[string]interface{}{"d": discNodeID})
	commentID, _ := addRes["addDiscussionComment"].(map[string]interface{})["comment"].(map[string]interface{})["id"].(string)
	if commentID == "" {
		t.Fatal("no comment id")
	}

	// A public gist.
	resp := ghPost(t, "/api/v3/gists", defaultToken, map[string]interface{}{
		"public": true,
		"files":  map[string]interface{}{"a.txt": map[string]interface{}{"content": "hi"}},
	})
	gist := decodeJSON(t, resp)
	gistNodeID, _ := gist["node_id"].(string)
	if gistNodeID == "" {
		t.Fatal("no gist node_id")
	}

	typename := func(id string) string {
		q := `query($id:ID!){node(id:$id){__typename}}`
		r := runDiscussionGQL(t, q, map[string]interface{}{"id": id})
		node, _ := r["node"].(map[string]interface{})
		tn, _ := node["__typename"].(string)
		return tn
	}

	if got := typename(discNodeID); got != "Discussion" {
		t.Fatalf("node(discussion) __typename = %q, want Discussion", got)
	}
	if got := typename(commentID); got != "DiscussionComment" {
		t.Fatalf("node(comment) __typename = %q, want DiscussionComment", got)
	}
	if got := typename(gistNodeID); got != "Gist" {
		t.Fatalf("node(gist) __typename = %q, want Gist", got)
	}

	// A field selection through the concrete type resolves too.
	r := runDiscussionGQL(t, `query($id:ID!){node(id:$id){... on Discussion{title}}}`,
		map[string]interface{}{"id": discNodeID})
	node, _ := r["node"].(map[string]interface{})
	if node["title"] != "T" {
		t.Fatalf("node(discussion).title = %v, want T", node["title"])
	}
}

// TestNodeGistVisibilityRespectsSecret pins that node(id:) will not expose a
// secret gist to a non-owner.
func TestNodeGistVisibilityRespectsSecret(t *testing.T) {
	// Owner creates a SECRET gist.
	resp := ghPost(t, "/api/v3/gists", defaultToken, map[string]interface{}{
		"public": false,
		"files":  map[string]interface{}{"s.txt": map[string]interface{}{"content": "secret"}},
	})
	gist := decodeJSON(t, resp)
	gistNodeID, _ := gist["node_id"].(string)
	if gistNodeID == "" {
		t.Fatal("no gist node_id")
	}

	// A different user must not resolve it through node(id:).
	other := seedTestUser(testServer, "gist-node-stranger")
	otherTok := testServer.store.CreateToken(other.ID, "gist")
	resp = ghPost(t, "/api/graphql", otherTok.Value, map[string]interface{}{
		"query":     `query($id:ID!){node(id:$id){__typename}}`,
		"variables": map[string]interface{}{"id": gistNodeID},
	})
	body := decodeJSON(t, resp)
	data, _ := body["data"].(map[string]interface{})
	if data == nil || data["node"] != nil {
		t.Fatalf("secret gist leaked to a non-owner via node(id:): %v", body)
	}
}

package bleephub

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
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
	t.Parallel()
	s := newIsolatedServer(t)
	// admin (defaultToken) creates a SECRET gist.
	resp := s.post(t, "/api/v3/gists", defaultToken, map[string]interface{}{
		"public": false,
		"files":  map[string]interface{}{"s.txt": map[string]interface{}{"content": "secret"}},
	})
	requireStatusNoClose(t, resp, 201)
	gist := decodeJSON(t, resp)
	gistNodeID, _ := gist["node_id"].(string)
	if gistNodeID == "" {
		t.Fatal("no gist node_id")
	}

	// A different user must not resolve it through node(id:).
	_, otherTok := s.userSurfaceUser(t, "gist-node-stranger")
	resp = s.post(t, "/api/graphql", otherTok, map[string]interface{}{
		"query":     `query($id:ID!){node(id:$id){__typename}}`,
		"variables": map[string]interface{}{"id": gistNodeID},
	})
	body := decodeJSON(t, resp)
	data, _ := body["data"].(map[string]interface{})
	if data == nil || data["node"] != nil {
		t.Fatalf("secret gist leaked to a non-owner via node(id:): %v", body)
	}
}

// TestRepoCodespaceListScopedToCaller pins that GET /repos/{o}/{r}/codespaces
// lists only the authenticated user's codespaces, not every user's on the repo.
func TestRepoCodespaceListScopedToCaller(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	st := s.store
	name := s.createRepoWriteRepo(t, true)
	other, _ := s.userSurfaceUser(t, "cs-leak-other")
	if _, err := st.CreateCodespace("admin", "admin/"+name, "main", "EastUs", store.CodespaceCreateOptions{}); err != nil {
		t.Fatalf("seed admin codespace: %v", err)
	}
	if _, err := st.CreateCodespace(other.Login, "admin/"+name, "main", "EastUs", store.CodespaceCreateOptions{}); err != nil {
		t.Fatalf("seed other codespace: %v", err)
	}

	resp := s.get(t, "/api/v3/repos/admin/"+name+"/codespaces", defaultToken)
	requireStatusNoClose(t, resp, 200)
	body := decodeJSON(t, resp)
	list, _ := body["codespaces"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("admin saw %d codespaces, want only their own 1 (no cross-user leak)", len(list))
	}
	owner, _ := list[0].(map[string]interface{})["owner"].(map[string]interface{})
	if owner["login"] != "admin" {
		t.Fatalf("listed a codespace owned by %v, want admin", owner["login"])
	}
}

// TestOrgCodespaceSecretReposValidated pins that setting an org codespace
// secret's selected repos rejects a repo not owned by the org (422).
func TestOrgCodespaceSecretReposValidated(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	st := s.store
	admin := st.LookupUserByLogin("admin")
	org := st.CreateOrg(admin, "cs-secret-org", "", "")
	st.CreateCodespaceSecret(store.CodespaceSecretScopeKey("org", org.Login), "TOK", "v", "selected", nil)
	// A repo owned by someone else must not be accepted into the org secret.
	foreign := st.CreateRepo(admin, "cs-foreign", "", false) // user-owned, not the org

	resp := s.put(t, "/api/v3/orgs/"+org.Login+"/codespaces/secrets/TOK/repositories", defaultToken,
		map[string]interface{}{"selected_repository_ids": []int{foreign.ID}})
	requireStatus(t, resp, http.StatusUnprocessableEntity)
}

// TestSecretScanningResolvedByPopulated pins that resolving an alert records and
// returns the resolving user in resolved_by (was always null).
func TestSecretScanningResolvedByPopulated(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	st := s.store
	name := s.createRepoWriteRepo(t, false)
	alert := st.CreateSecretScanningAlert("admin/"+name, "github_personal_access_token", nil)
	if alert == nil {
		t.Fatal("seed alert failed")
	}

	resp := s.patch(t, "/api/v3/repos/admin/"+name+"/secret-scanning/alerts/"+itoa(alert.Number), defaultToken,
		map[string]interface{}{"state": "resolved", "resolution": "used_in_tests"})
	requireStatusNoClose(t, resp, 200)
	body := decodeJSON(t, resp)
	rb, _ := body["resolved_by"].(map[string]interface{})
	if rb == nil || rb["login"] != "admin" {
		t.Fatalf("resolved_by = %v, want the admin user object", body["resolved_by"])
	}
}

// TestPagesUpdateSourcePathOnlyKeepsBranch pins that a legacy Pages PATCH sending
// only source.path (branch omitted) is accepted, keeping the stored branch.
func TestPagesUpdateSourcePathOnlyKeepsBranch(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := s.createRepoWriteRepo(t, true)
	resp := s.post(t, "/api/v3/repos/admin/"+name+"/pages", defaultToken,
		map[string]interface{}{"source": map[string]interface{}{"branch": "main"}})
	requireStatus(t, resp, 201)

	// PATCH the path alone; must not fail "branch required".
	resp = s.put(t, "/api/v3/repos/admin/"+name+"/pages", defaultToken,
		map[string]interface{}{"source": map[string]interface{}{"path": "/docs"}})
	requireStatus(t, resp, http.StatusNoContent)
}

// TestSAMLAssertionReplayRejected pins one-time-use of SAML assertion IDs:
// a fresh ID is accepted, a replay within its window is rejected, an ID-less
// assertion is refused, and distinct IDs are independent. This closes the
// IdP-initiated replay window (SP-initiated already binds a single-use state).
func TestSAMLAssertionReplayRejected(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	exp := s.currentTime().Add(5 * time.Minute)

	if !s.consumeSAMLAssertionID("_assert-1", exp) {
		t.Fatal("first use of an assertion ID must be accepted")
	}
	if s.consumeSAMLAssertionID("_assert-1", exp) {
		t.Fatal("replay of the same assertion ID must be rejected")
	}
	if s.consumeSAMLAssertionID("", exp) {
		t.Fatal("an assertion with no ID must be rejected (untrackable)")
	}
	if !s.consumeSAMLAssertionID("_assert-2", exp) {
		t.Fatal("a distinct assertion ID must be accepted")
	}
}

// TestReceivePackRejectsRefCreateToAbsentObject pins that the receive-pack
// transport refuses creating a ref at an object the push never sent (git's
// "missing necessary objects" connectivity check) — it left a dangling ref.
func TestReceivePackRejectsRefCreateToAbsentObject(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := s.createRepoWriteRepo(t, true)
	fake := "0123456789abcdef0123456789abcdef01234567"

	var body bytes.Buffer
	line := strings.Repeat("0", 40) + " " + fake + " refs/heads/evil\x00report-status\n"
	fmt.Fprintf(&body, "%04x%s", len(line)+4, line)
	body.WriteString("0000") // flush-pkt
	// An empty but well-formed packfile: PACK, version 2, zero objects, SHA-1 trailer.
	var pack bytes.Buffer
	pack.WriteString("PACK")
	_ = binary.Write(&pack, binary.BigEndian, uint32(2))
	_ = binary.Write(&pack, binary.BigEndian, uint32(0))
	sum := sha1.Sum(pack.Bytes())
	pack.Write(sum[:])
	body.Write(pack.Bytes())

	req, _ := http.NewRequest("POST", s.baseURL+"/admin/"+name+".git/git-receive-pack", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	req.SetBasicAuth("x-token", defaultToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("receive-pack POST: %v", err)
	}
	resp.Body.Close()

	// The ref must not exist: the create-to-absent-object was refused.
	got := s.get(t, "/api/v3/repos/admin/"+name+"/git/ref/heads/evil", defaultToken)
	requireStatus(t, got, http.StatusNotFound)
}

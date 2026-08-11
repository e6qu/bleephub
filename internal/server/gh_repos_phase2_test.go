package bleephub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

// TestRepoTopicsREST verifies GET and PUT /repos/{owner}/{repo}/topics.
func TestRepoTopicsREST(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "topics-rest",
	})

	// GET returns empty topics.
	getResp := s.get(t, "/api/v3/repos/admin/topics-rest/topics", defaultToken)
	defer getResp.Body.Close()
	if getResp.StatusCode != 200 {
		t.Fatalf("expected 200 for get topics, got %d", getResp.StatusCode)
	}
	var got map[string]interface{}
	if err := json.NewDecoder(getResp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	names, _ := got["names"].([]interface{})
	if len(names) != 0 {
		t.Fatalf("expected empty topics, got %v", names)
	}

	// PUT topics.
	putResp := s.put(t, "/api/v3/repos/admin/topics-rest/topics", defaultToken, map[string]interface{}{
		"names": []string{"go", "ci", "bleephub"},
	})
	if putResp.StatusCode != 200 {
		putResp.Body.Close()
		t.Fatalf("expected 200 for put topics, got %d", putResp.StatusCode)
	}
	var putOut map[string]interface{}
	if err := json.NewDecoder(putResp.Body).Decode(&putOut); err != nil {
		t.Fatal(err)
	}
	putResp.Body.Close()
	putNames, _ := putOut["names"].([]interface{})
	if len(putNames) != 3 {
		t.Fatalf("expected 3 topics, got %v", putNames)
	}

	// GET reflects the update.
	getResp2 := s.get(t, "/api/v3/repos/admin/topics-rest/topics", defaultToken)
	defer getResp2.Body.Close()
	var got2 map[string]interface{}
	if err := json.NewDecoder(getResp2.Body).Decode(&got2); err != nil {
		t.Fatal(err)
	}
	names2, _ := got2["names"].([]interface{})
	if len(names2) != 3 {
		t.Fatalf("expected 3 topics after put, got %v", names2)
	}

	// Repo JSON also exposes topics.
	repoResp := s.get(t, "/api/v3/repos/admin/topics-rest", defaultToken)
	defer repoResp.Body.Close()
	var repo map[string]interface{}
	if err := json.NewDecoder(repoResp.Body).Decode(&repo); err != nil {
		t.Fatal(err)
	}
	repoTopics, _ := repo["topics"].([]interface{})
	if len(repoTopics) != 3 {
		t.Fatalf("expected 3 topics in repo json, got %v", repoTopics)
	}
}

// TestRepoTopicsREST_Validation verifies topic name validation.
func TestRepoTopicsREST_Validation(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "topics-validation",
	})

	cases := []struct {
		name   string
		topics []string
	}{
		{"empty topic", []string{"go", ""}},
		{"too long", []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}, // 51 chars
		{"invalid char space", []string{"go lang"}},
		{"invalid char slash", []string{"go/lang"}},
		{"too many", make([]string, 21)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := s.put(t, "/api/v3/repos/admin/topics-validation/topics", defaultToken, map[string]interface{}{
				"names": tc.topics,
			})
			defer resp.Body.Close()
			if resp.StatusCode != 422 {
				t.Fatalf("expected 422, got %d", resp.StatusCode)
			}
		})
	}
}

// TestRepoTopicsREST_PrivateRequiresRead verifies private repo access control.
func TestRepoTopicsREST_PrivateRequiresRead(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name":    "topics-private",
		"private": true,
	})

	s.store.Mu.Lock()
	other := &User{ID: s.store.NextUser, Login: "other", Type: "User"}
	s.store.NextUser++
	s.store.Users[other.ID] = other
	s.store.UsersByLogin[other.Login] = other
	otherTok := &Token{Value: "ghp_otherusertoken000000000000000000000000", UserID: other.ID, Scopes: "repo"}
	s.store.Tokens[otherTok.Value] = otherTok
	s.store.Mu.Unlock()

	resp := s.get(t, "/api/v3/repos/admin/topics-private/topics", otherTok.Value)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 for unreadable private repo, got %d", resp.StatusCode)
	}
}

// TestDeleteContentsFile verifies DELETE /repos/{owner}/{repo}/contents/{path}.
func TestDeleteContentsFile(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name":      "delete-contents",
		"auto_init": true,
	})

	// Create a file to delete.
	encoded := base64.StdEncoding.EncodeToString([]byte("to be deleted"))
	putResp := s.put(t, "/api/v3/repos/admin/delete-contents/contents/remove-me.txt", defaultToken, map[string]interface{}{
		"message": "add file",
		"content": encoded,
	})
	if putResp.StatusCode != 201 {
		putResp.Body.Close()
		t.Fatalf("expected 201, got %d", putResp.StatusCode)
	}
	putData := decodeJSON(t, putResp)
	content, _ := putData["content"].(map[string]interface{})
	sha := content["sha"].(string)

	// Delete the file.
	delResp := s.do(t, "DELETE", "/api/v3/repos/admin/delete-contents/contents/remove-me.txt", defaultToken, map[string]interface{}{
		"message": "remove file",
		"sha":     sha,
	})
	if delResp.StatusCode != 200 {
		delResp.Body.Close()
		t.Fatalf("expected 200, got %d", delResp.StatusCode)
	}
	delData := decodeJSON(t, delResp)
	if delData["content"] != nil {
		t.Fatalf("expected nil content, got %v", delData["content"])
	}
	commit, _ := delData["commit"].(map[string]interface{})
	if commit["message"] != "remove file" {
		t.Fatalf("expected commit message 'remove file', got %v", commit["message"])
	}

	// File is gone.
	getResp := s.get(t, "/api/v3/repos/admin/delete-contents/contents/remove-me.txt", defaultToken)
	defer getResp.Body.Close()
	if getResp.StatusCode != 404 {
		t.Fatalf("expected 404 after delete, got %d", getResp.StatusCode)
	}
}

// TestDeleteContentsFile_ShaMismatch verifies deletion is rejected when SHA does not match.
func TestDeleteContentsFile_ShaMismatch(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name":      "delete-sha-mismatch",
		"auto_init": true,
	})

	encoded := base64.StdEncoding.EncodeToString([]byte("content"))
	putResp := s.put(t, "/api/v3/repos/admin/delete-sha-mismatch/contents/x.txt", defaultToken, map[string]interface{}{
		"message": "add file",
		"content": encoded,
	})
	if putResp.StatusCode != 201 {
		putResp.Body.Close()
		t.Fatalf("expected 201, got %d", putResp.StatusCode)
	}
	putResp.Body.Close()

	delResp := s.do(t, "DELETE", "/api/v3/repos/admin/delete-sha-mismatch/contents/x.txt", defaultToken, map[string]interface{}{
		"message": "remove file",
		"sha":     "0000000000000000000000000000000000000000",
	})
	defer delResp.Body.Close()
	if delResp.StatusCode != 409 {
		t.Fatalf("expected 409 for sha mismatch, got %d", delResp.StatusCode)
	}
}

// TestDeleteContentsFile_NonExistentPath verifies deletion of a missing path returns 422.
func TestDeleteContentsFile_NonExistentPath(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name":      "delete-missing",
		"auto_init": true,
	})

	delResp := s.do(t, "DELETE", "/api/v3/repos/admin/delete-missing/contents/nope.txt", defaultToken, map[string]interface{}{
		"message": "remove file",
		"sha":     "0000000000000000000000000000000000000000",
	})
	defer delResp.Body.Close()
	if delResp.StatusCode != 422 {
		t.Fatalf("expected 422 for missing path, got %d", delResp.StatusCode)
	}
}

// TestDeleteContentsFile_RequiresPush verifies write access is enforced.
func TestDeleteContentsFile_RequiresPush(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "delete-perms",
	})

	s.store.Mu.Lock()
	other := &User{ID: s.store.NextUser, Login: "other", Type: "User"}
	s.store.NextUser++
	s.store.Users[other.ID] = other
	s.store.UsersByLogin[other.Login] = other
	otherTok := &Token{Value: "ghp_otherusertoken000000000000000000000000", UserID: other.ID, Scopes: "repo"}
	s.store.Tokens[otherTok.Value] = otherTok
	s.store.Mu.Unlock()

	delResp := s.do(t, "DELETE", "/api/v3/repos/admin/delete-perms/contents/x.txt", otherTok.Value, map[string]interface{}{
		"message": "remove file",
		"sha":     "0000000000000000000000000000000000000000",
	})
	defer delResp.Body.Close()
	if delResp.StatusCode != 403 {
		t.Fatalf("expected 403, got %d", delResp.StatusCode)
	}
}

// helper for DELETE with JSON body.
func ghDeleteWithBody(t *testing.T, path, token string, body map[string]interface{}) *http.Response {
	t.Helper()
	if body == nil {
		return ghDo(t, "DELETE", path, token, nil)
	}
	return ghDo(t, "DELETE", path, token, body)
}

// TestRepoStargazersREST verifies star/unstar and listing endpoints.
func TestRepoStargazersREST(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "stargazers-rest",
	})

	// Initially no stargazers.
	listResp := s.get(t, "/api/v3/repos/admin/stargazers-rest/stargazers", defaultToken)
	defer listResp.Body.Close()
	if listResp.StatusCode != 200 {
		t.Fatalf("expected 200 for list stargazers, got %d", listResp.StatusCode)
	}
	var stargazers []map[string]interface{}
	if err := json.NewDecoder(listResp.Body).Decode(&stargazers); err != nil {
		t.Fatal(err)
	}
	if len(stargazers) != 0 {
		t.Fatalf("expected 0 stargazers, got %d", len(stargazers))
	}

	// Star the repo.
	starResp := s.put(t, "/api/v3/user/starred/admin/stargazers-rest", defaultToken, nil)
	if starResp.StatusCode != 204 {
		starResp.Body.Close()
		t.Fatalf("expected 204 for star, got %d", starResp.StatusCode)
	}
	starResp.Body.Close()

	// Repo JSON reflects count.
	repoResp := s.get(t, "/api/v3/repos/admin/stargazers-rest", defaultToken)
	defer repoResp.Body.Close()
	var repo map[string]interface{}
	if err := json.NewDecoder(repoResp.Body).Decode(&repo); err != nil {
		t.Fatal(err)
	}
	if count, _ := repo["stargazers_count"].(float64); count != 1 {
		t.Fatalf("expected stargazers_count=1, got %v", repo["stargazers_count"])
	}

	// List shows the current user.
	listResp2 := s.get(t, "/api/v3/repos/admin/stargazers-rest/stargazers", defaultToken)
	defer listResp2.Body.Close()
	var stargazers2 []map[string]interface{}
	if err := json.NewDecoder(listResp2.Body).Decode(&stargazers2); err != nil {
		t.Fatal(err)
	}
	if len(stargazers2) != 1 {
		t.Fatalf("expected 1 stargazer, got %d", len(stargazers2))
	}
	if stargazers2[0]["login"] != "admin" {
		t.Fatalf("expected stargazer admin, got %v", stargazers2[0]["login"])
	}

	// List user's starred repos (may include repos starred by earlier tests).
	starredResp := s.get(t, "/api/v3/user/starred", defaultToken)
	defer starredResp.Body.Close()
	if starredResp.StatusCode != 200 {
		t.Fatalf("expected 200 for list starred, got %d", starredResp.StatusCode)
	}
	var starred []map[string]interface{}
	if err := json.NewDecoder(starredResp.Body).Decode(&starred); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range starred {
		if full, _ := r["full_name"].(string); full == "admin/stargazers-rest" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected admin/stargazers-rest in starred repos, got %+v", starred)
	}

	// Unstar.
	unstarResp := s.do(t, "DELETE", "/api/v3/user/starred/admin/stargazers-rest", defaultToken, nil)
	defer unstarResp.Body.Close()
	if unstarResp.StatusCode != 204 {
		t.Fatalf("expected 204 for unstar, got %d", unstarResp.StatusCode)
	}

	listResp3 := s.get(t, "/api/v3/repos/admin/stargazers-rest/stargazers", defaultToken)
	defer listResp3.Body.Close()
	var stargazers3 []map[string]interface{}
	if err := json.NewDecoder(listResp3.Body).Decode(&stargazers3); err != nil {
		t.Fatal(err)
	}
	if len(stargazers3) != 0 {
		t.Fatalf("expected 0 stargazers after unstar, got %d", len(stargazers3))
	}
}

// TestRepoStargazersPagination verifies per_page/page slicing and Link headers.
func TestRepoStargazersPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	pagedJSONRequest(t, s, http.MethodPost, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "stargazers-pg",
	})

	st := s.store
	st.Mu.Lock()
	other := &User{ID: st.NextUser, Login: "stargazer-user", Type: "User", StarredRepos: map[string]bool{}}
	st.NextUser++
	st.Users[other.ID] = other
	st.UsersByLogin[other.Login] = other
	otherTok := &Token{Value: "ghp_stargazerusertoken00000000000000000", UserID: other.ID, Scopes: "repo"}
	st.Tokens[otherTok.Value] = otherTok
	st.Mu.Unlock()

	for _, tok := range []string{defaultToken, otherTok.Value} {
		w := pagedJSONRequest(t, s, http.MethodPut, "/api/v3/user/starred/admin/stargazers-pg", tok, nil)
		if w.Code != 204 {
			t.Fatalf("expected 204 for star, got %d", w.Code)
		}
	}

	resp := tokenRequest(s, http.MethodGet, "/api/v3/repos/admin/stargazers-pg/stargazers?per_page=1", defaultToken)
	if resp.Code != 200 {
		t.Fatalf("expected 200, got %d", resp.Code)
	}
	var page1 []map[string]interface{}
	if err := json.Unmarshal(resp.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decode page 1: %v", err)
	}
	if len(page1) != 1 {
		t.Fatalf("expected 1 stargazer on page 1, got %d", len(page1))
	}
	if link := resp.Header().Get("Link"); !strings.Contains(link, `rel="next"`) {
		t.Fatalf("expected rel=next in Link, got %s", link)
	}

	resp2 := tokenRequest(s, http.MethodGet, "/api/v3/repos/admin/stargazers-pg/stargazers?per_page=1&page=2", defaultToken)
	if resp2.Code != 200 {
		t.Fatalf("expected 200, got %d", resp2.Code)
	}
	var page2 []map[string]interface{}
	if err := json.Unmarshal(resp2.Body.Bytes(), &page2); err != nil {
		t.Fatalf("decode page 2: %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("expected 1 stargazer on page 2, got %d", len(page2))
	}
	if page2[0]["login"] == page1[0]["login"] {
		t.Fatalf("expected distinct stargazers across pages, got %v twice", page1[0]["login"])
	}
}

// TestRepoCollaboratorsREST verifies collaborator add/list/remove and permission check.
func TestRepoCollaboratorsREST(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "collab-rest",
	})

	// Create another user.
	s.store.Mu.Lock()
	other := &User{ID: s.store.NextUser, Login: "collab-user", Type: "User", StarredRepos: map[string]bool{}}
	s.store.NextUser++
	s.store.Users[other.ID] = other
	s.store.UsersByLogin[other.Login] = other
	otherTok := &Token{Value: "ghp_collabusertoken000000000000000000000", UserID: other.ID, Scopes: "repo"}
	s.store.Tokens[otherTok.Value] = otherTok
	s.store.Mu.Unlock()

	// Inviting a new collaborator answers 201 with a pending repository
	// invitation carrying the invitee, inviter, and requested role.
	addResp := s.put(t, "/api/v3/repos/admin/collab-rest/collaborators/collab-user", defaultToken, map[string]interface{}{
		"permission": "push",
	})
	if addResp.StatusCode != 201 {
		addResp.Body.Close()
		t.Fatalf("expected 201 for add collaborator, got %d", addResp.StatusCode)
	}
	invitation := decodeJSON(t, addResp)
	invID, _ := invitation["id"].(float64)
	if invID <= 0 {
		t.Fatalf("expected real invitation id, got %v", invitation["id"])
	}
	if invitee, _ := invitation["invitee"].(map[string]interface{}); invitee == nil || invitee["login"] != "collab-user" {
		t.Fatalf("expected invitee collab-user, got %v", invitation["invitee"])
	}
	if invitation["permissions"] != "write" {
		t.Fatalf("expected write role on invitation, got %v", invitation["permissions"])
	}

	// The pending invitation is listed on the repository until accepted.
	pendingResp := s.get(t, "/api/v3/repos/admin/collab-rest/invitations", defaultToken)
	if pendingResp.StatusCode != 200 {
		pendingResp.Body.Close()
		t.Fatalf("expected 200 for list invitations, got %d", pendingResp.StatusCode)
	}
	if pending := decodeJSONArray(t, pendingResp); len(pending) != 1 {
		t.Fatalf("expected 1 pending invitation, got %d", len(pending))
	}

	// The invitee accepts, becoming a collaborator.
	acceptResp := s.patch(t, fmt.Sprintf("/api/v3/user/repository_invitations/%d", int(invID)), otherTok.Value, nil)
	acceptResp.Body.Close()
	if acceptResp.StatusCode != 204 {
		t.Fatalf("expected 204 for accept invitation, got %d", acceptResp.StatusCode)
	}

	// Re-PUT on an existing collaborator updates the permission in place (204).
	updateResp := s.put(t, "/api/v3/repos/admin/collab-rest/collaborators/collab-user", defaultToken, map[string]interface{}{
		"permission": "push",
	})
	updateResp.Body.Close()
	if updateResp.StatusCode != 204 {
		t.Fatalf("expected 204 for permission update on existing collaborator, got %d", updateResp.StatusCode)
	}

	// List collaborators includes the owner and the new collaborator.
	listResp := s.get(t, "/api/v3/repos/admin/collab-rest/collaborators", defaultToken)
	defer listResp.Body.Close()
	if listResp.StatusCode != 200 {
		t.Fatalf("expected 200 for list collaborators, got %d", listResp.StatusCode)
	}
	var collabs []map[string]interface{}
	if err := json.NewDecoder(listResp.Body).Decode(&collabs); err != nil {
		t.Fatal(err)
	}
	if len(collabs) != 2 {
		t.Fatalf("expected 2 collaborators, got %d", len(collabs))
	}

	// Permission check.
	permResp := s.get(t, "/api/v3/repos/admin/collab-rest/collaborators/collab-user/permission", defaultToken)
	defer permResp.Body.Close()
	if permResp.StatusCode != 200 {
		t.Fatalf("expected 200 for permission check, got %d", permResp.StatusCode)
	}
	var perm map[string]interface{}
	if err := json.NewDecoder(permResp.Body).Decode(&perm); err != nil {
		t.Fatal(err)
	}
	if perm["permission"] != "push" {
		t.Fatalf("expected push permission, got %v", perm["permission"])
	}

	// Collaborator can read the private repo's contents metadata.
	metaResp := s.get(t, "/api/v3/repos/admin/collab-rest", otherTok.Value)
	defer metaResp.Body.Close()
	if metaResp.StatusCode != 200 {
		t.Fatalf("expected 200 for collaborator repo read, got %d", metaResp.StatusCode)
	}

	// Remove collaborator.
	delResp := s.do(t, "DELETE", "/api/v3/repos/admin/collab-rest/collaborators/collab-user", defaultToken, nil)
	defer delResp.Body.Close()
	if delResp.StatusCode != 204 {
		t.Fatalf("expected 204 for remove collaborator, got %d", delResp.StatusCode)
	}

	// After removal, list shows only owner.
	listResp2 := s.get(t, "/api/v3/repos/admin/collab-rest/collaborators", defaultToken)
	defer listResp2.Body.Close()
	var collabs2 []map[string]interface{}
	if err := json.NewDecoder(listResp2.Body).Decode(&collabs2); err != nil {
		t.Fatal(err)
	}
	if len(collabs2) != 1 {
		t.Fatalf("expected 1 collaborator after removal, got %d", len(collabs2))
	}
}

// TestRepoCollaboratorsPagination verifies per_page/page slicing and Link headers.
func TestRepoCollaboratorsPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	pagedJSONRequest(t, s, http.MethodPost, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "collab-pg",
	})

	st := s.store
	st.Mu.Lock()
	other := &User{ID: st.NextUser, Login: "collab-pg-user", Type: "User", StarredRepos: map[string]bool{}}
	st.NextUser++
	st.Users[other.ID] = other
	st.UsersByLogin[other.Login] = other
	otherTok := &Token{Value: "ghp_collabpgusertoken000000000000000000", UserID: other.ID, Scopes: "repo"}
	st.Tokens[otherTok.Value] = otherTok
	st.Mu.Unlock()

	addResp := pagedJSONRequest(t, s, http.MethodPut, "/api/v3/repos/admin/collab-pg/collaborators/collab-pg-user", defaultToken, map[string]interface{}{
		"permission": "push",
	})
	if addResp.Code != 201 && addResp.Code != 204 {
		t.Fatalf("expected 201 or 204 for add collaborator, got %d", addResp.Code)
	}
	if addResp.Code == 201 {
		var invitation map[string]interface{}
		if err := json.Unmarshal(addResp.Body.Bytes(), &invitation); err != nil {
			t.Fatalf("decode invitation: %v", err)
		}
		invID, _ := invitation["id"].(float64)
		acceptResp := pagedJSONRequest(t, s, http.MethodPatch, fmt.Sprintf("/api/v3/user/repository_invitations/%d", int(invID)), otherTok.Value, nil)
		if acceptResp.Code != 204 {
			t.Fatalf("expected 204 for accept invitation, got %d", acceptResp.Code)
		}
	}

	resp := tokenRequest(s, http.MethodGet, "/api/v3/repos/admin/collab-pg/collaborators?per_page=1", defaultToken)
	if resp.Code != 200 {
		t.Fatalf("expected 200, got %d", resp.Code)
	}
	var page1 []map[string]interface{}
	if err := json.Unmarshal(resp.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decode page 1: %v", err)
	}
	if len(page1) != 1 {
		t.Fatalf("expected 1 collaborator on page 1, got %d", len(page1))
	}
	if page1[0]["login"] != "admin" {
		t.Fatalf("expected owner first on page 1, got %v", page1[0]["login"])
	}
	if link := resp.Header().Get("Link"); !strings.Contains(link, `rel="next"`) {
		t.Fatalf("expected rel=next in Link, got %s", link)
	}

	resp2 := tokenRequest(s, http.MethodGet, "/api/v3/repos/admin/collab-pg/collaborators?per_page=1&page=2", defaultToken)
	if resp2.Code != 200 {
		t.Fatalf("expected 200, got %d", resp2.Code)
	}
	var page2 []map[string]interface{}
	if err := json.Unmarshal(resp2.Body.Bytes(), &page2); err != nil {
		t.Fatalf("decode page 2: %v", err)
	}
	if len(page2) != 1 || page2[0]["login"] != "collab-pg-user" {
		t.Fatalf("expected collab-pg-user on page 2, got %+v", page2)
	}
}

// TestRepoLanguagesREST verifies GET /repos/{owner}/{repo}/languages returns
// byte totals by language for the default branch.
func TestRepoLanguagesREST(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "languages-rest",
	})
	repo := s.store.GetRepo("admin", "languages-rest")
	if repo == nil {
		t.Fatalf("repo not created")
	}
	stor := s.store.GetGitStorage("admin", "languages-rest")
	if stor == nil {
		t.Fatalf("no git storage")
	}
	_, err := initRepoWithFiles(stor, "main", "init", map[string]string{
		"main.go":     "package main\n",
		"lib.go":      "package lib\n",
		"app.js":      "console.log('hi')\n",
		"readme.md":   "# readme\n",
		"vendor/x.go": "package x\n", // ignored
	}, repoSignature("t", "t@t"))
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}

	resp := s.get(t, "/api/v3/repos/admin/languages-rest/languages", defaultToken)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 for languages, got %d", resp.StatusCode)
	}
	var got map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["Go"] == nil || got["Go"].(float64) <= 0 {
		t.Fatalf("expected Go byte count, got %v", got)
	}
	if got["JavaScript"] == nil || got["JavaScript"].(float64) <= 0 {
		t.Fatalf("expected JavaScript byte count, got %v", got)
	}
	if got["Markdown"] == nil || got["Markdown"].(float64) <= 0 {
		t.Fatalf("expected Markdown byte count, got %v", got)
	}
	if _, ok := got["Text"]; ok {
		// vendor/x.go is vendored and should not count.
		t.Fatalf("did not expect Text from vendor, got %v", got)
	}

	if got["Go"].(float64) <= got["Markdown"].(float64) {
		t.Fatalf("expected Go bytes > Markdown bytes, got %v", got)
	}
	if got["Go"].(float64) <= got["JavaScript"].(float64) {
		t.Fatalf("expected Go bytes > JavaScript bytes, got %v", got)
	}
}

// TestRepoMergeREST verifies POST /repos/{owner}/{repo}/merges performs a
// three-way merge of head into base.
func TestRepoMergeREST(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "merge-rest",
	})
	repo := s.store.GetRepo("admin", "merge-rest")
	if repo == nil {
		t.Fatalf("repo not created")
	}
	stor := s.store.GetGitStorage("admin", "merge-rest")
	if stor == nil {
		t.Fatalf("no git storage")
	}
	_, err := initRepoWithFiles(stor, "main", "init", map[string]string{
		"README.md": "# hello",
	}, repoSignature("t", "t@t"))
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	_, err = createFileCommit(stor, "main", "main.go", "package main\n", "add main", repoSignature("t", "t@t"))
	if err != nil {
		t.Fatalf("commit on main: %v", err)
	}
	mainRef, err := stor.Reference(plumbing.NewBranchReferenceName("main"))
	if err != nil {
		t.Fatalf("resolve main: %v", err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), mainRef.Hash())); err != nil {
		t.Fatalf("set feature ref: %v", err)
	}
	_, err = createFileCommit(stor, "feature", "feature.go", "package feature\n", "add feature", repoSignature("t", "t@t"))
	if err != nil {
		t.Fatalf("commit on feature: %v", err)
	}

	resp := s.post(t, "/api/v3/repos/admin/merge-rest/merges", defaultToken, map[string]interface{}{
		"base":           "main",
		"head":           "feature",
		"commit_message": "merge feature",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201 for merge, got %d", resp.StatusCode)
	}
	var got map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["sha"] == nil {
		t.Fatalf("expected merge commit sha, got %v", got)
	}

	// main should now point at the merge commit (two parents).
	mainRef2, err := stor.Reference(plumbing.NewBranchReferenceName("main"))
	if err != nil {
		t.Fatalf("resolve main after merge: %v", err)
	}
	if mainRef2.Hash().String() == mainRef.Hash().String() {
		t.Fatalf("main did not move after merge")
	}
}

// TestRepoForksREST verifies POST and GET /repos/{owner}/{repo}/forks.
func TestRepoForksREST(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "fork-source",
	})
	source := s.store.GetRepo("admin", "fork-source")
	if source == nil {
		t.Fatalf("source repo not created")
	}
	stor := s.store.GetGitStorage("admin", "fork-source")
	if stor == nil {
		t.Fatalf("no git storage")
	}
	_, err := initRepoWithFiles(stor, "main", "init", map[string]string{
		"README.md": "# source",
	}, repoSignature("t", "t@t"))
	if err != nil {
		t.Fatalf("init source repo: %v", err)
	}

	// Create a second user to fork into.
	s.store.Mu.Lock()
	forker := &User{ID: s.store.NextUser, Login: "forker", Type: "User", CreatedAt: fixedTestTime, UpdatedAt: fixedTestTime}
	s.store.NextUser++
	s.store.Users[forker.ID] = forker
	s.store.UsersByLogin[forker.Login] = forker
	tok := &Token{Value: "forker-token", UserID: forker.ID, Scopes: "repo", CreatedAt: fixedTestTime}
	s.store.Tokens[tok.Value] = tok
	s.store.Mu.Unlock()

	resp := s.post(t, "/api/v3/repos/admin/fork-source/forks", "forker-token", map[string]interface{}{})
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("expected 202 for fork, got %d", resp.StatusCode)
	}
	var got map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["fork"] != true {
		t.Fatalf("expected fork=true, got %v", got["fork"])
	}
	if got["parent"] == nil {
		t.Fatalf("expected parent field, got none")
	}

	// List forks as source owner.
	listResp := s.get(t, "/api/v3/repos/admin/fork-source/forks", defaultToken)
	defer listResp.Body.Close()
	if listResp.StatusCode != 200 {
		t.Fatalf("expected 200 for list forks, got %d", listResp.StatusCode)
	}
	var forks []map[string]interface{}
	if err := json.NewDecoder(listResp.Body).Decode(&forks); err != nil {
		t.Fatal(err)
	}
	if len(forks) != 1 {
		t.Fatalf("expected 1 fork, got %d", len(forks))
	}
}

// TestRepoRenameREST verifies PATCH /repos/{owner}/{repo} can rename a repo
// and that the new name is reachable afterwards.
func TestRepoRenameREST(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "rename-me",
	})
	stor := s.store.GetGitStorage("admin", "rename-me")
	if stor == nil {
		t.Fatalf("no git storage")
	}
	_, err := initRepoWithFiles(stor, "main", "init", map[string]string{
		"README.md": "# hello",
	}, repoSignature("t", "t@t"))
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}

	patchResp := s.patch(t, "/api/v3/repos/admin/rename-me", defaultToken, map[string]interface{}{
		"name": "renamed-repo",
	})
	defer patchResp.Body.Close()
	if patchResp.StatusCode != 200 {
		t.Fatalf("expected 200 for rename, got %d", patchResp.StatusCode)
	}
	var got map[string]interface{}
	if err := json.NewDecoder(patchResp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["name"] != "renamed-repo" || got["full_name"] != "admin/renamed-repo" {
		t.Fatalf("unexpected rename response: %v", got)
	}

	// Old name is gone.
	oldResp := s.get(t, "/api/v3/repos/admin/rename-me", defaultToken)
	defer oldResp.Body.Close()
	if oldResp.StatusCode != 404 {
		t.Fatalf("expected 404 for old name, got %d", oldResp.StatusCode)
	}

	// New name resolves and git storage still works.
	newResp := s.get(t, "/api/v3/repos/admin/renamed-repo", defaultToken)
	defer newResp.Body.Close()
	if newResp.StatusCode != 200 {
		t.Fatalf("expected 200 for new name, got %d", newResp.StatusCode)
	}

	commitsResp := s.get(t, "/api/v3/repos/admin/renamed-repo/commits", defaultToken)
	defer commitsResp.Body.Close()
	if commitsResp.StatusCode != 200 {
		t.Fatalf("expected 200 for commits after rename, got %d", commitsResp.StatusCode)
	}
}

// TestRepoCompareREST verifies GET /repos/{owner}/{repo}/compare/{base}...{head}.
func TestRepoCompareREST(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "compare-rest",
	})
	repo := s.store.GetRepo("admin", "compare-rest")
	if repo == nil {
		t.Fatalf("repo not created")
	}
	stor := s.store.GetGitStorage("admin", "compare-rest")
	if stor == nil {
		t.Fatalf("no git storage")
	}
	_, err := initRepoWithFiles(stor, "main", "init", map[string]string{
		"README.md": "# hello",
	}, repoSignature("t", "t@t"))
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	// Add a commit on main.
	_, err = createFileCommit(stor, "main", "main.go", "package main\n", "add main", repoSignature("t", "t@t"))
	if err != nil {
		t.Fatalf("commit on main: %v", err)
	}
	// Create feature branch from current main HEAD.
	mainRef, err := stor.Reference(plumbing.NewBranchReferenceName("main"))
	if err != nil {
		t.Fatalf("resolve main: %v", err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), mainRef.Hash())); err != nil {
		t.Fatalf("set feature ref: %v", err)
	}
	// Add a commit on feature branch.
	_, err = createFileCommit(stor, "feature", "feature.go", "package feature\n", "add feature", repoSignature("t", "t@t"))
	if err != nil {
		t.Fatalf("commit on feature: %v", err)
	}

	resp := s.get(t, "/api/v3/repos/admin/compare-rest/compare/main...feature", defaultToken)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 for compare, got %d", resp.StatusCode)
	}
	var got map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "ahead" {
		t.Fatalf("expected status ahead, got %v", got["status"])
	}
	if got["ahead_by"].(float64) != 1 {
		t.Fatalf("expected ahead_by 1, got %v", got["ahead_by"])
	}
	commits, _ := got["commits"].([]interface{})
	if len(commits) != 1 {
		t.Fatalf("expected 1 commit, got %d", len(commits))
	}
	files, _ := got["files"].([]interface{})
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	f := files[0].(map[string]interface{})
	if f["filename"] != "feature.go" {
		t.Fatalf("expected feature.go, got %v", f["filename"])
	}
}

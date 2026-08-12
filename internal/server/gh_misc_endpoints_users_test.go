package bleephub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func createTestUser(t *testing.T, login string) *store.User {
	t.Helper()
	resp, err := authedPost("/internal/users", "application/json", bytes.NewReader(mustJSON(map[string]interface{}{
		"login": login,
		"email": login + "@example.com",
	})))
	if err != nil {
		t.Fatalf("create user %s: %v", login, err)
	}
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create user %s: %d %s", login, resp.StatusCode, b)
	}
	resp.Body.Close()
	return testServer.store.UsersByLogin[login]
}

func TestUserExtras_ListUsers(t *testing.T) {
	resp := ghGet(t, "/api/v3/users", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	users := decodeJSONArray(t, resp)
	if len(users) == 0 {
		t.Fatal("expected users")
	}
}

func TestEnterpriseAdminUsersCRUDSiteAdminAndSuspension(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	login := "admin-api-user"
	create := s.post(t, "/api/v3/admin/users", defaultToken, map[string]interface{}{
		"login": login,
		"email": "admin-api-user@example.com",
	})
	if create.StatusCode != 201 {
		body, _ := io.ReadAll(create.Body)
		create.Body.Close()
		t.Fatalf("create admin user: got %d, want 201: %s", create.StatusCode, body)
	}
	created := decodeJSON(t, create)
	if created["login"] != login {
		t.Fatalf("create login = %v, want %s", created["login"], login)
	}

	promote := s.put(t, "/api/v3/users/"+login+"/site_admin", defaultToken, nil)
	promote.Body.Close()
	if promote.StatusCode != 204 {
		t.Fatalf("promote status = %d, want 204", promote.StatusCode)
	}
	got := decodeJSONWithStatus(t, s.get(t, "/api/v3/users/"+login, defaultToken), 200)
	if got["site_admin"] != true {
		t.Fatalf("site_admin after promote = %v, want true", got["site_admin"])
	}

	demote := s.delete(t, "/api/v3/users/"+login+"/site_admin", defaultToken)
	demote.Body.Close()
	if demote.StatusCode != 204 {
		t.Fatalf("demote status = %d, want 204", demote.StatusCode)
	}
	got = decodeJSONWithStatus(t, s.get(t, "/api/v3/users/"+login, defaultToken), 200)
	if got["site_admin"] != false {
		t.Fatalf("site_admin after demote = %v, want false", got["site_admin"])
	}

	suspend := s.put(t, "/api/v3/users/"+login+"/suspended", defaultToken, map[string]string{"reason": "test"})
	suspend.Body.Close()
	if suspend.StatusCode != 204 {
		t.Fatalf("suspend status = %d, want 204", suspend.StatusCode)
	}

	u := s.store.LookupUserByLogin(login)
	if u == nil {
		t.Fatalf("created user %q missing from store", login)
	}
	userToken := "tok-" + login
	s.store.Mu.Lock()
	s.store.Tokens[userToken] = &store.Token{Value: userToken, UserID: u.ID, Scopes: "repo", CreatedAt: fixedTestTime.UTC()}
	s.store.Mu.Unlock()
	asSuspended := s.get(t, "/api/v3/user", userToken)
	asSuspended.Body.Close()
	if asSuspended.StatusCode != 403 {
		t.Fatalf("suspended user token status = %d, want 403", asSuspended.StatusCode)
	}

	unsuspend := s.delete(t, "/api/v3/users/"+login+"/suspended", defaultToken)
	unsuspend.Body.Close()
	if unsuspend.StatusCode != 204 {
		t.Fatalf("unsuspend status = %d, want 204", unsuspend.StatusCode)
	}
	asActive := s.get(t, "/api/v3/user", userToken)
	asActive.Body.Close()
	if asActive.StatusCode != 200 {
		t.Fatalf("unsuspended user token status = %d, want 200", asActive.StatusCode)
	}

	del := s.delete(t, "/api/v3/admin/users/"+login, defaultToken)
	del.Body.Close()
	if del.StatusCode != 204 {
		t.Fatalf("delete admin user status = %d, want 204", del.StatusCode)
	}
	getDeleted := s.get(t, "/api/v3/users/"+login, defaultToken)
	getDeleted.Body.Close()
	if getDeleted.StatusCode != 404 {
		t.Fatalf("get deleted user status = %d, want 404", getDeleted.StatusCode)
	}
}

func TestUserExtras_Blocks(t *testing.T) {
	u := createTestUser(t, "blocktarget")
	_ = u

	putResp := ghPut(t, "/api/v3/user/blocks/blocktarget", defaultToken, nil)
	putResp.Body.Close()
	if putResp.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", putResp.StatusCode)
	}

	checkResp := ghGet(t, "/api/v3/user/blocks/blocktarget", defaultToken)
	checkResp.Body.Close()
	if checkResp.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", checkResp.StatusCode)
	}

	listResp := ghGet(t, "/api/v3/user/blocks", defaultToken)
	if listResp.StatusCode != 200 {
		listResp.Body.Close()
		t.Fatalf("expected 200, got %d", listResp.StatusCode)
	}
	blocks := decodeJSONArray(t, listResp)
	if len(blocks) == 0 {
		t.Fatal("expected blocked users")
	}

	delResp := ghDelete(t, "/api/v3/user/blocks/blocktarget", defaultToken)
	delResp.Body.Close()
	if delResp.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", delResp.StatusCode)
	}
}

func TestUserExtras_SocialAccounts(t *testing.T) {
	resp := ghPost(t, "/api/v3/user/social_accounts", defaultToken, []map[string]interface{}{
		{"url": "https://example.com/me"},
	})
	// GitHub answers this 201 Created, not 200.
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	accounts := decodeJSONArray(t, resp)
	if len(accounts) == 0 {
		t.Fatal("expected social accounts")
	}

	listResp := ghGet(t, "/api/v3/users/admin/social_accounts", defaultToken)
	if listResp.StatusCode != 200 {
		listResp.Body.Close()
		t.Fatalf("expected 200, got %d", listResp.StatusCode)
	}
	got := decodeJSONArray(t, listResp)
	if len(got) == 0 {
		t.Fatal("expected public social accounts")
	}

	delResp := ghDelete(t, "/api/v3/user/social_accounts", defaultToken)
	delResp.Body.Close()
	if delResp.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", delResp.StatusCode)
	}
}

func TestUserExtras_SSHSigningKeys(t *testing.T) {
	resp := ghPost(t, "/api/v3/user/ssh_signing_keys", defaultToken, map[string]interface{}{
		"key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDIhz2GK/XCUj4i6Q5yQJNL1MXMY0RxzPV2QrBqfHrDq",
	})
	if resp.StatusCode != 201 {
		resp.Body.Close()
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	key := decodeJSON(t, resp)
	keyID := int(key["id"].(float64))

	listResp := ghGet(t, "/api/v3/user/ssh_signing_keys", defaultToken)
	if listResp.StatusCode != 200 {
		listResp.Body.Close()
		t.Fatalf("expected 200, got %d", listResp.StatusCode)
	}
	keys := decodeJSONArray(t, listResp)
	if len(keys) == 0 {
		t.Fatal("expected keys")
	}

	delResp := ghDelete(t, "/api/v3/user/ssh_signing_keys/"+itoa(keyID), defaultToken)
	delResp.Body.Close()
	if delResp.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", delResp.StatusCode)
	}
}

func TestUserExtras_Following(t *testing.T) {
	createTestUser(t, "followtarget")

	putResp := ghPut(t, "/api/v3/user/following/followtarget", defaultToken, nil)
	putResp.Body.Close()
	if putResp.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", putResp.StatusCode)
	}

	checkResp := ghGet(t, "/api/v3/user/following/followtarget", defaultToken)
	checkResp.Body.Close()
	if checkResp.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", checkResp.StatusCode)
	}

	publicCheck := ghGet(t, "/api/v3/users/admin/following/followtarget", defaultToken)
	publicCheck.Body.Close()
	if publicCheck.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", publicCheck.StatusCode)
	}
}

func assertPaginatedList(t *testing.T, s *Server, path, token string) {
	t.Helper()
	w := tokenRequest(s, http.MethodGet, path+"?per_page=1", token)
	if w.Code != 200 {
		t.Fatalf("%s page 1 status = %d, want 200", path, w.Code)
	}
	var first []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatalf("%s page 1 decode: %v", path, err)
	}
	if len(first) != 1 {
		t.Fatalf("%s page 1 items = %d, want 1", path, len(first))
	}
	if link := w.Header().Get("Link"); !strings.Contains(link, `rel="next"`) {
		t.Fatalf("%s page 1 Link = %q, want rel=\"next\"", path, link)
	}

	w = tokenRequest(s, http.MethodGet, path+"?per_page=1&page=2", token)
	if w.Code != 200 {
		t.Fatalf("%s page 2 status = %d, want 200", path, w.Code)
	}
	var second []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatalf("%s page 2 decode: %v", path, err)
	}
	if len(second) != 1 {
		t.Fatalf("%s page 2 items = %d, want 1", path, len(second))
	}
	if reflect.DeepEqual(second[0], first[0]) {
		t.Fatalf("%s page 2 returned the same item as page 1: %v", path, second[0])
	}
}

func TestUserExtras_FollowersFollowingPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	userA := seedTestUser(s, "pg-follow-a")
	userB := seedTestUser(s, "pg-follow-b")
	tokenA := s.store.CreateToken(userA.ID, "user repo")
	tokenB := s.store.CreateToken(userB.ID, "user repo")

	follows := []struct{ token, target string }{
		{defaultToken, "pg-follow-a"},
		{defaultToken, "pg-follow-b"},
		{tokenA.Value, "admin"},
		{tokenB.Value, "admin"},
		{tokenB.Value, "pg-follow-a"},
	}
	for _, f := range follows {
		w := pagedJSONRequest(t, s, http.MethodPut, "/api/v3/user/following/"+f.target, f.token, nil)
		if w.Code != 204 {
			t.Fatalf("follow %s status = %d, want 204", f.target, w.Code)
		}
	}

	for _, path := range []string{
		"/api/v3/users/admin/following",
		"/api/v3/users/pg-follow-a/followers",
		"/api/v3/user/following",
		"/api/v3/user/followers",
	} {
		w := tokenRequest(s, http.MethodGet, path+"?per_page=1", defaultToken)
		if w.Code != 200 {
			t.Fatalf("%s page 1 status = %d, want 200", path, w.Code)
		}
		var first []map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
			t.Fatalf("%s page 1 decode: %v", path, err)
		}
		if len(first) != 1 {
			t.Fatalf("%s page 1 items = %d, want 1", path, len(first))
		}
		if link := w.Header().Get("Link"); !strings.Contains(link, `rel="next"`) {
			t.Fatalf("%s page 1 Link = %q, want rel=\"next\"", path, link)
		}
		w = tokenRequest(s, http.MethodGet, path+"?per_page=1&page=2", defaultToken)
		if w.Code != 200 {
			t.Fatalf("%s page 2 status = %d, want 200", path, w.Code)
		}
		var second []map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
			t.Fatalf("%s page 2 decode: %v", path, err)
		}
		if len(second) != 1 {
			t.Fatalf("%s page 2 items = %d, want 1", path, len(second))
		}
		if link := w.Header().Get("Link"); !strings.Contains(link, `rel="prev"`) {
			t.Fatalf("%s page 2 Link = %q, want rel=\"prev\"", path, link)
		}
	}
}

func TestUserExtras_BlocksPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	seedTestUser(s, "pg-block-a")
	seedTestUser(s, "pg-block-b")
	for _, login := range []string{"pg-block-a", "pg-block-b"} {
		w := pagedJSONRequest(t, s, http.MethodPut, "/api/v3/user/blocks/"+login, defaultToken, nil)
		if w.Code != 204 {
			t.Fatalf("block %s status = %d, want 204", login, w.Code)
		}
	}

	assertPaginatedList(t, s, "/api/v3/user/blocks", defaultToken)

	for _, login := range []string{"pg-block-a", "pg-block-b"} {
		w := pagedJSONRequest(t, s, http.MethodDelete, "/api/v3/user/blocks/"+login, defaultToken, nil)
		if w.Code != 204 {
			t.Fatalf("unblock %s status = %d, want 204", login, w.Code)
		}
	}
}

func TestUserExtras_SocialAccountsPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	w := pagedJSONRequest(t, s, http.MethodPost, "/api/v3/user/social_accounts", defaultToken, []map[string]interface{}{
		{"url": "https://example.com/pg-one"},
		{"url": "https://example.com/pg-two"},
	})
	if w.Code != 201 {
		t.Fatalf("create social accounts status = %d, want 201", w.Code)
	}

	assertPaginatedList(t, s, "/api/v3/user/social_accounts", defaultToken)
	assertPaginatedList(t, s, "/api/v3/users/admin/social_accounts", defaultToken)

	del := pagedJSONRequest(t, s, http.MethodDelete, "/api/v3/user/social_accounts", defaultToken, nil)
	if del.Code != 204 {
		t.Fatalf("clear social accounts status = %d, want 204", del.Code)
	}
}

func TestUserExtras_SSHSigningKeysPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	var ids []int
	for _, key := range []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDIhz2GK/XCUj4i6Q5yQJNL1MXMY0RxzPV2QrBqfHrDp",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDIhz2GK/XCUj4i6Q5yQJNL1MXMY0RxzPV2QrBqfHrDr",
	} {
		w := pagedJSONRequest(t, s, http.MethodPost, "/api/v3/user/ssh_signing_keys", defaultToken, map[string]interface{}{"key": key})
		if w.Code != 201 {
			t.Fatalf("create ssh signing key status = %d, want 201", w.Code)
		}
		var created map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode ssh signing key: %v", err)
		}
		ids = append(ids, int(created["id"].(float64)))
	}

	assertPaginatedList(t, s, "/api/v3/user/ssh_signing_keys", defaultToken)
	assertPaginatedList(t, s, "/api/v3/users/admin/ssh_signing_keys", defaultToken)

	for _, id := range ids {
		w := pagedJSONRequest(t, s, http.MethodDelete, "/api/v3/user/ssh_signing_keys/"+itoa(id), defaultToken, nil)
		if w.Code != 204 {
			t.Fatalf("delete ssh signing key %d status = %d, want 204", id, w.Code)
		}
	}
}

func TestUserExtras_Events(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "event-repo", "", false)
	if repo == nil {
		t.Fatal("create repo failed")
	}
	issue := s.store.CreateIssue(repo.ID, admin.ID, "Issue title", "body", nil, nil, 0)

	fetchIssueEvent := func() map[string]interface{} {
		t.Helper()
		resp := s.get(t, "/api/v3/users/admin/events?per_page=100", defaultToken)
		if resp.StatusCode != 200 {
			resp.Body.Close()
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		for _, ev := range decodeJSONArray(t, resp) {
			if ev["type"] != "IssuesEvent" {
				continue
			}
			payload, _ := ev["payload"].(map[string]interface{})
			embedded, _ := payload["issue"].(map[string]interface{})
			if embedded != nil && embedded["title"] == "Issue title" && payload["action"] == "opened" {
				return ev
			}
		}
		t.Fatal("expected an IssuesEvent for the created issue")
		return nil
	}

	first := fetchIssueEvent()
	second := fetchIssueEvent()

	// The event derives from the stored issue: its ID and timestamp are
	// stable across requests, and created_at is the issue's recorded
	// creation time — not the render time.
	if id, _ := first["id"].(string); id == "" {
		t.Fatalf("event id missing: %v", first)
	}
	if second["id"] != first["id"] {
		t.Fatalf("event id not stable across requests: %v vs %v", first["id"], second["id"])
	}
	if second["created_at"] != first["created_at"] {
		t.Fatalf("event created_at not stable across requests: %v vs %v", first["created_at"], second["created_at"])
	}
	if want := issue.CreatedAt.UTC().Format(time.RFC3339); first["created_at"] != want {
		t.Fatalf("event created_at = %v, want the issue's recorded creation time %s", first["created_at"], want)
	}
	actor, _ := first["actor"].(map[string]interface{})
	if actor == nil || actor["login"] != "admin" {
		t.Fatalf("event actor = %v", first["actor"])
	}
	repoJSON, _ := first["repo"].(map[string]interface{})
	if repoJSON == nil || repoJSON["name"] != "admin/event-repo" {
		t.Fatalf("event repo = %v", first["repo"])
	}
}

func TestUserExtras_UserGists(t *testing.T) {
	created := createTestGist(t, defaultToken, true)
	id := created["id"].(string)

	resp := ghGet(t, "/api/v3/users/admin/gists", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	gists := decodeJSONArray(t, resp)
	found := false
	for _, g := range gists {
		if g["id"] == id {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected user gist")
	}
}

func TestUserExtras_StarredRepo(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "star-repo", "", false)
	if repo == nil {
		t.Fatal("create repo failed")
	}
	s.store.StarRepo(admin.ID, "admin", "star-repo")

	resp := s.get(t, "/api/v3/user/starred/admin/star-repo", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
}

func TestUserExtras_Subscriptions(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "sub-repo", "", false)
	if repo == nil {
		t.Fatal("create repo failed")
	}
	s.store.SetRepoSubscription(admin.ID, repo.ID, true)

	resp := s.get(t, "/api/v3/user/subscriptions", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	subs := decodeJSONArray(t, resp)
	if len(subs) == 0 {
		t.Fatal("expected subscriptions")
	}
}

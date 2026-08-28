package bleephub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func invitationsTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer()
	s.registerGHRepoRoutes()
	s.registerGHRepoInvitationRoutes()
	return s
}

func doInvitationReq(s *Server, token, method, path string, body []byte) *httptest.ResponseRecorder {
	return serveTestRequest(s, bearerHeader(token), method, path, body)
}

func makeOtherUser(s *Server, login string) (*store.User, string) {
	s.store.Mu.Lock()
	u := &store.User{ID: s.store.NextUser, Login: login, Type: "User", Email: login + "@bleephub.local"}
	s.store.NextUser++
	s.store.Users[u.ID] = u
	s.store.UsersByLogin[u.Login] = u
	s.store.Mu.Unlock()
	tok := s.store.CreateToken(u.ID, "repo")
	return u, tok.Value
}

func TestInvitations_RepoListUpdateCancel(t *testing.T) {
	s := invitationsTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "invite-repo", "", false)
	other, _ := makeOtherUser(s, "colleague")
	inv := s.store.CreateRepoInvitation(repo.FullName, other.Login, "", admin.ID, "pull")

	w := doInvitationReq(s, adminPAT, "GET", "/api/v3/repos/"+repo.FullName+"/invitations", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", w.Code, w.Body.String())
	}
	var list []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}
	if list[0]["permissions"] != "read" {
		t.Fatalf("permissions = %v, want read", list[0]["permissions"])
	}

	w = doInvitationReq(s, adminPAT, "PATCH", fmt.Sprintf("/api/v3/repos/%s/invitations/%d", repo.FullName, inv.ID), []byte(`{"permissions":"push"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", w.Code, w.Body.String())
	}
	var updated map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &updated)
	if updated["permissions"] != "write" {
		t.Fatalf("permissions after update = %v, want write", updated["permissions"])
	}

	w = doInvitationReq(s, adminPAT, "DELETE", fmt.Sprintf("/api/v3/repos/%s/invitations/%d", repo.FullName, inv.ID), nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("cancel status = %d, body = %s", w.Code, w.Body.String())
	}
	if s.store.GetRepoInvitation(repo.FullName, inv.ID) != nil {
		t.Fatal("invitation still exists after cancel")
	}
}

func TestInvitations_UserAcceptAndDecline(t *testing.T) {
	s := invitationsTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "invite-user-repo", "", false)
	other, otherToken := makeOtherUser(s, "invited")
	inv := s.store.CreateRepoInvitation(repo.FullName, other.Login, "", admin.ID, "push")

	w := doInvitationReq(s, otherToken, "GET", "/api/v3/user/repository_invitations", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("user list status = %d, body = %s", w.Code, w.Body.String())
	}
	var list []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("user list len = %d, want 1", len(list))
	}

	w = doInvitationReq(s, otherToken, "PATCH", fmt.Sprintf("/api/v3/user/repository_invitations/%d", inv.ID), nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("accept status = %d, body = %s", w.Code, w.Body.String())
	}
	perm := s.store.GetRepoCollaboratorPermission(repo.Owner.Login, repo.Name, other.Login)
	if perm != "push" {
		t.Fatalf("collaborator permission = %v, want push", perm)
	}
	if s.store.GetRepoInvitation(repo.FullName, inv.ID) != nil {
		t.Fatal("invitation still pending after accept")
	}

	inv2 := s.store.CreateRepoInvitation(repo.FullName, other.Login, "", admin.ID, "pull")
	w = doInvitationReq(s, otherToken, "DELETE", fmt.Sprintf("/api/v3/user/repository_invitations/%d", inv2.ID), nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("decline status = %d, body = %s", w.Code, w.Body.String())
	}
	if s.store.GetRepoInvitation(repo.FullName, inv2.ID) != nil {
		t.Fatal("invitation still exists after decline")
	}
}

func TestInvitations_NonAdminCannotManage(t *testing.T) {
	s := invitationsTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "invite-priv", "", true)
	other, otherToken := makeOtherUser(s, "outsider")
	inv := s.store.CreateRepoInvitation(repo.FullName, other.Login, "", admin.ID, "pull")
	s.store.AddRepoCollaborator(repo.Owner.Login, repo.Name, other.Login, "push")

	w := doInvitationReq(s, otherToken, "GET", "/api/v3/repos/"+repo.FullName+"/invitations", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin list status = %d, want 403", w.Code)
	}

	w = doInvitationReq(s, otherToken, "PATCH", fmt.Sprintf("/api/v3/repos/%s/invitations/%d", repo.FullName, inv.ID), []byte(`{"permissions":"admin"}`))
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin update status = %d, want 403", w.Code)
	}

	w = doInvitationReq(s, otherToken, "DELETE", fmt.Sprintf("/api/v3/repos/%s/invitations/%d", repo.FullName, inv.ID), nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin cancel status = %d, want 403", w.Code)
	}
}

func TestInvitations_CannotAcceptOtherUsersInvitation(t *testing.T) {
	s := invitationsTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "invite-mismatch", "", false)
	_, aliceToken := makeOtherUser(s, "alice")
	bob, _ := makeOtherUser(s, "bob")
	inv := s.store.CreateRepoInvitation(repo.FullName, bob.Login, "", admin.ID, "pull")

	w := doInvitationReq(s, aliceToken, "PATCH", fmt.Sprintf("/api/v3/user/repository_invitations/%d", inv.ID), nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("mismatched accept status = %d, want 404", w.Code)
	}

	w = doInvitationReq(s, aliceToken, "DELETE", fmt.Sprintf("/api/v3/user/repository_invitations/%d", inv.ID), nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("mismatched decline status = %d, want 404", w.Code)
	}
}

func TestInvitations_UserEndpointsRequireAuth(t *testing.T) {
	s := invitationsTestServer(t)
	w := doInvitationReq(s, "", "GET", "/api/v3/user/repository_invitations", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthed user list status = %d, want 401", w.Code)
	}
}

func TestInvitations_RepoListPagination(t *testing.T) {
	s := invitationsTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "invite-page-repo", "", false)
	u1, _ := makeOtherUser(s, "invitee-one")
	u2, _ := makeOtherUser(s, "invitee-two")
	s.store.CreateRepoInvitation(repo.FullName, u1.Login, "", admin.ID, "pull")
	s.store.CreateRepoInvitation(repo.FullName, u2.Login, "", admin.ID, "pull")

	w := doInvitationReq(s, adminPAT, "GET", "/api/v3/repos/"+repo.FullName+"/invitations?per_page=1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("page 1 status = %d, body = %s", w.Code, w.Body.String())
	}
	var page1 []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &page1)
	if len(page1) != 1 {
		t.Fatalf("page 1 len = %d, want 1", len(page1))
	}
	if link := w.Header().Get("Link"); !strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 1 Link = %q, want rel=next", link)
	}

	w = doInvitationReq(s, adminPAT, "GET", "/api/v3/repos/"+repo.FullName+"/invitations?per_page=1&page=2", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("page 2 status = %d, body = %s", w.Code, w.Body.String())
	}
	var page2 []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &page2)
	if len(page2) != 1 {
		t.Fatalf("page 2 len = %d, want 1", len(page2))
	}
	if page1[0]["id"] == page2[0]["id"] {
		t.Fatalf("page 1 and page 2 returned the same invitation: %v", page1[0]["id"])
	}
}

func TestInvitations_UserListPagination(t *testing.T) {
	s := invitationsTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	r1 := s.store.CreateRepo(admin, "invite-user-page-1", "", false)
	r2 := s.store.CreateRepo(admin, "invite-user-page-2", "", false)
	other, otherToken := makeOtherUser(s, "paged-invitee")
	s.store.CreateRepoInvitation(r1.FullName, other.Login, "", admin.ID, "pull")
	s.store.CreateRepoInvitation(r2.FullName, other.Login, "", admin.ID, "pull")

	w := doInvitationReq(s, otherToken, "GET", "/api/v3/user/repository_invitations?per_page=1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("page 1 status = %d, body = %s", w.Code, w.Body.String())
	}
	var page1 []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &page1)
	if len(page1) != 1 {
		t.Fatalf("page 1 len = %d, want 1", len(page1))
	}
	if link := w.Header().Get("Link"); !strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 1 Link = %q, want rel=next", link)
	}

	w = doInvitationReq(s, otherToken, "GET", "/api/v3/user/repository_invitations?per_page=1&page=2", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("page 2 status = %d, body = %s", w.Code, w.Body.String())
	}
	var page2 []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &page2)
	if len(page2) != 1 {
		t.Fatalf("page 2 len = %d, want 1", len(page2))
	}
	if page1[0]["id"] == page2[0]["id"] {
		t.Fatalf("page 1 and page 2 returned the same invitation: %v", page1[0]["id"])
	}
}

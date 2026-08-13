package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func pinnedTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer()
	s.registerGHRepoRoutes()
	s.registerGHPinnedRoutes()
	return s
}

func doPinnedReq(s *Server, token, method, path string, body []byte) *httptest.ResponseRecorder {
	return serveTestRequest(s, bearerHeader(token), method, path, body)
}

func TestPinned_SetAndList(t *testing.T) {
	s := pinnedTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	s.store.CreateRepo(admin, "alpha", "", false)
	s.store.CreateRepo(admin, "beta", "", false)

	w := doPinnedReq(s, adminPAT, "PUT", "/ui-data/users/admin/pinned", []byte(`{"repos":["admin/beta","admin/alpha"]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("set status = %d, body = %s", w.Code, w.Body.String())
	}
	var set []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &set)
	if len(set) != 2 || set[0]["full_name"] != "admin/beta" {
		t.Fatalf("set result = %v (want beta first)", set)
	}

	w = doPinnedReq(s, adminPAT, "GET", "/ui-data/users/admin/pinned", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	var got []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got) != 2 {
		t.Fatalf("list len = %d, want 2", len(got))
	}
}

func TestPinned_TooManyIs422(t *testing.T) {
	s := pinnedTestServer(t)
	w := doPinnedReq(s, adminPAT, "PUT", "/ui-data/users/admin/pinned",
		[]byte(`{"repos":["a/1","a/2","a/3","a/4","a/5","a/6","a/7"]}`))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
}

func TestPinned_DedupAndNonexistentDropped(t *testing.T) {
	s := pinnedTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	s.store.CreateRepo(admin, "alpha", "", false)

	w := doPinnedReq(s, adminPAT, "PUT", "/ui-data/users/admin/pinned",
		[]byte(`{"repos":["admin/alpha","admin/alpha","admin/ghost"]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got) != 1 || got[0]["full_name"] != "admin/alpha" {
		t.Fatalf("result = %v, want single admin/alpha", got)
	}
}

func TestPinned_NonOwnerForbidden(t *testing.T) {
	s := pinnedTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	s.store.CreateRepo(admin, "alpha", "", false)

	s.store.Mu.Lock()
	other := &store.User{ID: s.store.NextUser, Login: "intruder", Type: "User", Email: "intruder@bleephub.local"}
	s.store.NextUser++
	s.store.Users[other.ID] = other
	s.store.UsersByLogin[other.Login] = other
	s.store.Mu.Unlock()
	otherToken := s.store.CreateToken(other.ID, "repo").Value

	w := doPinnedReq(s, otherToken, "PUT", "/ui-data/users/admin/pinned", []byte(`{"repos":["admin/alpha"]}`))
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-owner set status = %d, want 403", w.Code)
	}
}

package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func notificationsViewsServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer()
	s.registerGHNotificationsRoutes()
	return s
}

func doNotifReq(s *Server, token, method, path string, body []byte) *httptest.ResponseRecorder {
	return serveTestRequest(s, bearerHeader(token), method, path, body)
}

func decodeThreadList(t *testing.T, w *httptest.ResponseRecorder) []map[string]interface{} {
	t.Helper()
	var threads []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &threads); err != nil {
		t.Fatalf("unmarshal threads: %v (body=%s)", err, w.Body.String())
	}
	return threads
}

func TestNotificationsUI_SavedFlagAndSavedView(t *testing.T) {
	s := notificationsViewsServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "saved-repo", "", false)
	issue := s.store.CreateIssue(repo.ID, admin.ID, "bookmark me", "body", nil, nil, 0)
	threadID := store.NotificationThreadID("Issue", issue.ID)

	// Nothing saved yet.
	w := doNotifReq(s, adminPAT, "GET", "/ui-data/notifications?view=saved", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("saved view status = %d, body = %s", w.Code, w.Body.String())
	}
	if threads := decodeThreadList(t, w); len(threads) != 0 {
		t.Fatalf("saved view before saving = %v, want empty", threads)
	}

	// Save (bookmark) the thread.
	w = doNotifReq(s, adminPAT, "PUT", "/ui-data/notifications/threads/"+threadID+"/saved", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("save status = %d, body = %s", w.Code, w.Body.String())
	}

	w = doNotifReq(s, adminPAT, "GET", "/ui-data/notifications?view=saved", nil)
	threads := decodeThreadList(t, w)
	if len(threads) != 1 || threads[0]["id"] != threadID {
		t.Fatalf("saved view = %v, want the bookmarked thread", threads)
	}
	if threads[0]["saved"] != true {
		t.Fatalf("saved flag = %v, want true", threads[0]["saved"])
	}

	// Unsave: the view empties again.
	w = doNotifReq(s, adminPAT, "DELETE", "/ui-data/notifications/threads/"+threadID+"/saved", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("unsave status = %d", w.Code)
	}
	w = doNotifReq(s, adminPAT, "GET", "/ui-data/notifications?view=saved", nil)
	if threads := decodeThreadList(t, w); len(threads) != 0 {
		t.Fatalf("saved view after unsave = %v, want empty", threads)
	}
}

func TestNotificationsUI_DoneViewListsThreadsMarkedDone(t *testing.T) {
	s := notificationsViewsServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "done-repo", "", false)
	kept := s.store.CreateIssue(repo.ID, admin.ID, "kept", "body", nil, nil, 0)
	done := s.store.CreateIssue(repo.ID, admin.ID, "done", "body", nil, nil, 0)
	doneID := store.NotificationThreadID("Issue", done.ID)
	keptID := store.NotificationThreadID("Issue", kept.ID)

	// Mark one thread done through the public REST endpoint (unchanged
	// semantics: it disappears from the inbox).
	w := doNotifReq(s, adminPAT, "DELETE", "/api/v3/notifications/threads/"+doneID, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("mark done status = %d, body = %s", w.Code, w.Body.String())
	}
	w = doNotifReq(s, adminPAT, "GET", "/api/v3/notifications", nil)
	for _, thread := range decodeThreadList(t, w) {
		if thread["id"] == doneID {
			t.Fatalf("done thread still in the REST inbox: %v", thread)
		}
	}

	// The done view lists exactly the dismissed thread.
	w = doNotifReq(s, adminPAT, "GET", "/ui-data/notifications?view=done", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("done view status = %d, body = %s", w.Code, w.Body.String())
	}
	threads := decodeThreadList(t, w)
	if len(threads) != 1 || threads[0]["id"] != doneID {
		t.Fatalf("done view = %v, want only %s (not %s)", threads, doneID, keptID)
	}
}

func TestNotificationsUI_InvalidViewIs422(t *testing.T) {
	s := notificationsViewsServer(t)
	for _, path := range []string{
		"/ui-data/notifications",
		"/ui-data/notifications?view=inbox",
	} {
		w := doNotifReq(s, adminPAT, "GET", path, nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s status = %d, want 422", path, w.Code)
		}
	}
}

func TestNotificationsUI_ForeignThreadIs404(t *testing.T) {
	s := notificationsViewsServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "saved-secret", "", true)
	issue := s.store.CreateIssue(repo.ID, admin.ID, "private", "body", nil, nil, 0)
	threadID := store.NotificationThreadID("Issue", issue.ID)

	s.store.Mu.Lock()
	stranger := &store.User{ID: s.store.NextUser, Login: "saved-stranger", Type: "User"}
	s.store.NextUser++
	s.store.Users[stranger.ID] = stranger
	s.store.UsersByLogin[stranger.Login] = stranger
	s.store.Mu.Unlock()
	strangerToken := s.store.CreateToken(stranger.ID, "repo").Value

	w := doNotifReq(s, strangerToken, "PUT", "/ui-data/notifications/threads/"+threadID+"/saved", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("foreign save status = %d, want 404", w.Code)
	}
	w = doNotifReq(s, strangerToken, "DELETE", "/ui-data/notifications/threads/"+threadID+"/saved", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("foreign unsave status = %d, want 404", w.Code)
	}

	// And the owner's bookmark stays untouched by the stranger's attempts.
	w = doNotifReq(s, adminPAT, "PUT", "/ui-data/notifications/threads/"+threadID+"/saved", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("owner save status = %d", w.Code)
	}
	w = doNotifReq(s, strangerToken, "GET", "/ui-data/notifications?view=saved", nil)
	if threads := decodeThreadList(t, w); len(threads) != 0 {
		t.Fatalf("stranger sees foreign saved threads: %v", threads)
	}
}

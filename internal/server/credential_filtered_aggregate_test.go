package bleephub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAggregateIssueAndNotificationReadsHonorAppRepositorySelection(t *testing.T) {
	s := newTestServer()
	s.registerGHAppsRoutes()
	s.registerGHNotificationsRoutes()
	s.registerGHUserIssuesRoutes()

	user := s.store.UsersByLogin["admin"]
	selectedRepo := s.store.CreateRepo(user, "aggregate-selected", "", true)
	excludedRepo := s.store.CreateRepo(user, "aggregate-excluded", "", true)
	selectedIssue := s.store.CreateIssue(selectedRepo.ID, user.ID, "selected issue", "", nil, nil, 0)
	excludedIssue := s.store.CreateIssue(excludedRepo.ID, user.ID, "excluded issue", "", nil, nil, 0)
	if selectedIssue == nil || excludedIssue == nil {
		t.Fatal("failed to create issue fixtures")
	}

	app := s.store.CreateApp(user.ID, "Aggregate Selection App", "", nil, nil)
	inst := s.store.CreateInstallation(app.ID, "User", user.ID, user.Login, nil, nil)
	s.store.SetInstallationRepositorySelection(inst.ID, "selected", []int{selectedRepo.ID})
	token, _ := s.store.CreateUserToServerToken(user.ID, app.ID, "", "", time.Hour, false)
	s.store.SetUserToServerTokenInstallations(token.Token, []int{inst.ID})

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token.Token)
		w := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(w, req)
		return w
	}

	issuesResponse := get("/api/v3/issues?filter=created")
	if issuesResponse.Code != http.StatusOK {
		t.Fatalf("global issues status = %d, body = %s", issuesResponse.Code, issuesResponse.Body.String())
	}
	var issues []struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(issuesResponse.Body.Bytes(), &issues); err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].ID != selectedIssue.ID {
		t.Fatalf("global issues = %#v, want only issue %d", issues, selectedIssue.ID)
	}

	notificationsResponse := get("/api/v3/notifications")
	if notificationsResponse.Code != http.StatusOK {
		t.Fatalf("notifications status = %d, body = %s", notificationsResponse.Code, notificationsResponse.Body.String())
	}
	var notifications []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(notificationsResponse.Body.Bytes(), &notifications); err != nil {
		t.Fatal(err)
	}
	wantThreadID := notificationThreadID("Issue", selectedIssue.ID)
	if len(notifications) != 1 || notifications[0].ID != wantThreadID {
		t.Fatalf("notifications = %#v, want only %q", notifications, wantThreadID)
	}

	excludedThread := get(fmt.Sprintf("/api/v3/notifications/threads/%s", notificationThreadID("Issue", excludedIssue.ID)))
	if excludedThread.Code != http.StatusNotFound {
		t.Fatalf("excluded thread status = %d, want 404", excludedThread.Code)
	}
}

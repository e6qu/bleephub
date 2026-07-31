package bleephub

import (
	"net/http"
	"testing"
)

func TestGHESAdminStatsAndLicenseReflectStore(t *testing.T) {
	s := newTestServer()
	s.registerGHESAdminStatsRoutes()
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "stats-org", "Stats Org", "")
	root := s.store.CreateRepo(admin, "stats-root", "", false)
	orgRepo := s.store.CreateOrgRepo(org, admin, "stats-service", "", false)
	if root == nil || orgRepo == nil {
		t.Fatal("seed stats repositories")
	}

	rec := enterpriseActionsRequest(t, s, http.MethodGet, "/api/v3/enterprise/stats/repos", nil)
	repos := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || repos["total_repos"] != float64(2) ||
		repos["root_repos"] != float64(2) || repos["org_repos"] != float64(1) {
		t.Fatalf("repository stats = %d %#v", rec.Code, repos)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, "/api/v3/enterprise/stats/all", nil)
	all := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || all["repos"] == nil || all["users"] == nil ||
		all["security-products"] != nil {
		t.Fatalf("all stats = %d %#v", rec.Code, all)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, "/api/v3/enterprise/settings/license", nil)
	license := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || license["seats_used"] != float64(1) ||
		license["seats_available"].(float64) < 0 {
		t.Fatalf("license = %d %#v", rec.Code, license)
	}
}

func TestGHESGlobalAnnouncementAliasAndHiddenAuthorization(t *testing.T) {
	s := newTestServer()
	s.registerGHESAdminStatsRoutes()
	path := "/api/v3/enterprise/announcement"
	rec := enterpriseActionsRequest(t, s, http.MethodPatch, path,
		map[string]interface{}{"announcement": "GHES maintenance", "user_dismissible": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("set announcement = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet, path, nil)
	if got := decodeRecorderObject(t, rec); got["announcement"] != "GHES maintenance" ||
		got["user_dismissible"] != true {
		t.Fatalf("get announcement = %d %#v", rec.Code, got)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete, path, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete announcement = %d", rec.Code)
	}

	member := seedTestUser(s, "stats-member")
	s.store.mu.Lock()
	s.store.Tokens["stats-member-token"] = &Token{Value: "stats-member-token", UserID: member.ID}
	s.store.mu.Unlock()
	rec = enterpriseBearerRequest(t, s, http.MethodGet, "/api/v3/enterprise/stats/users", nil, "stats-member-token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-admin stats = %d, want hidden 404", rec.Code)
	}
}

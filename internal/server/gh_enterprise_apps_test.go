package bleephub

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestEnterpriseOrganizationInstallationJourney(t *testing.T) {
	s := newTestServer()
	s.registerGHEnterpriseAppRoutes()
	s.replaceClockNow(func() time.Time { return fixedTestTime })
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "enterprise-app-org", "Enterprise Apps", "")
	repoA := s.store.CreateOrgRepo(org, admin, "alpha", "", false)
	repoB := s.store.CreateOrgRepo(org, admin, "beta", "", false)
	app, err := s.store.CreateAppE(admin.ID, "Enterprise Provisioner", "", map[string]string{
		"contents": "read", "metadata": "read",
	}, []string{"push"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	base := "/api/v3/enterprises/bleephub/apps"

	rec := enterpriseActionsRequest(t, s, http.MethodGet, base+"/installable_organizations", nil)
	var orgs []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &orgs); err != nil || len(orgs) != 1 ||
		orgs[0]["login"] != org.Login {
		t.Fatalf("installable organizations = %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodGet,
		base+"/installable_organizations/"+org.Login+"/accessible_repositories", nil)
	var available []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &available); err != nil || len(available) != 2 {
		t.Fatalf("accessible repositories = %d %q: %v", rec.Code, rec.Body.String(), err)
	}

	installations := base + "/organizations/" + org.Login + "/installations"
	rec = enterpriseActionsRequest(t, s, http.MethodPost, installations, map[string]interface{}{
		"client_id": app.ClientID, "repository_selection": "selected",
		"repositories": []string{repoA.Name},
	})
	created := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusCreated || created["app_slug"] != app.Slug ||
		created["repository_selection"] != "selected" ||
		created["created_at"] != fixedTestTime.Format(time.RFC3339) {
		t.Fatalf("create enterprise installation = %d %#v", rec.Code, created)
	}
	id := int(created["id"].(float64))
	repositories := installations + "/" + strconv.Itoa(id) + "/repositories"

	rec = enterpriseActionsRequest(t, s, http.MethodPatch, repositories+"/add", map[string]interface{}{
		"repositories": []string{repoB.Name},
	})
	var selected []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &selected); err != nil || len(selected) != 2 {
		t.Fatalf("add installation repository = %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPatch, repositories+"/remove", map[string]interface{}{
		"repositories": []string{repoA.Name},
	})
	if err := json.Unmarshal(rec.Body.Bytes(), &selected); err != nil || len(selected) != 1 {
		t.Fatalf("remove installation repository = %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPatch, repositories+"/remove", map[string]interface{}{
		"repositories": []string{repoB.Name},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("remove last installation repository = %d %q, want 422", rec.Code, rec.Body.String())
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPatch, repositories, map[string]interface{}{
		"repository_selection": "all",
	})
	updated := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || updated["repository_selection"] != "all" {
		t.Fatalf("toggle installation repositories = %d %#v", rec.Code, updated)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPost, installations, map[string]interface{}{
		"client_id": app.ClientID, "repository_selection": "all",
	})
	reused := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusOK || int(reused["id"].(float64)) != id {
		t.Fatalf("reinstall existing app = %d %#v", rec.Code, reused)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete, installations+"/"+strconv.Itoa(id), nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete enterprise installation = %d %q", rec.Code, rec.Body.String())
	}
}

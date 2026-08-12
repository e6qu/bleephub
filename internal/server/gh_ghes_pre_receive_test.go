package bleephub

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestGHESPreReceivePolicyJourney(t *testing.T) {
	s := newTestServer()
	s.registerGHESAdminStatsRoutes()
	fixed := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	s.replaceClockNow(func() time.Time { return fixed })
	replaceStoreClockNow(s.store, func() time.Time { return fixed })
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "pre-receive-org", "Pre Receive", "")
	repo := s.store.CreateOrgRepo(org, admin, "hooks", "", false)
	if repo == nil {
		t.Fatal("create hook script repository")
	}

	rec := enterpriseActionsRequest(t, s, http.MethodPost, "/api/v3/admin/pre-receive-environments",
		map[string]interface{}{"name": "Ruby", "image_url": "https://images.example.test/ruby.tar.gz"})
	env := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusCreated || env["name"] != "Ruby" ||
		env["created_at"] != fixed.Format(time.RFC3339) {
		t.Fatalf("create environment = %d %#v", rec.Code, env)
	}
	envID := int(env["id"].(float64))
	envPath := "/api/v3/admin/pre-receive-environments/" + strconv.Itoa(envID)
	rec = enterpriseActionsRequest(t, s, http.MethodPost, envPath+"/downloads", nil)
	if got := decodeRecorderObject(t, rec); rec.Code != http.StatusAccepted ||
		got["state"] != "success" || got["downloaded_at"] != fixed.Format(time.RFC3339) {
		t.Fatalf("start environment download = %d %#v", rec.Code, got)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodPost, "/api/v3/admin/pre-receive-hooks",
		map[string]interface{}{
			"name": "Check commits", "script": "scripts/commit-check.sh",
			"script_repository": map[string]interface{}{"full_name": repo.FullName},
			"environment":       map[string]interface{}{"id": envID},
			"enforcement":       "testing", "allow_downstream_configuration": true,
		})
	hook := decodeRecorderObject(t, rec)
	if rec.Code != http.StatusCreated || hook["enforcement"] != "testing" ||
		hook["script_repository"].(map[string]interface{})["full_name"] != repo.FullName {
		t.Fatalf("create pre-receive hook = %d %#v", rec.Code, hook)
	}
	hookID := int(hook["id"].(float64))
	hookSuffix := strconv.Itoa(hookID)

	rec = enterpriseActionsRequest(t, s, http.MethodPatch,
		"/api/v3/orgs/"+org.Login+"/pre-receive-hooks/"+hookSuffix,
		map[string]interface{}{"enforcement": "enabled", "allow_downstream_configuration": true})
	if got := decodeRecorderObject(t, rec); rec.Code != http.StatusOK ||
		got["enforcement"] != "enabled" || got["allow_downstream_configuration"] != true {
		t.Fatalf("organization pre-receive override = %d %#v", rec.Code, got)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodPatch,
		"/api/v3/repos/"+repo.FullName+"/pre-receive-hooks/"+hookSuffix,
		map[string]interface{}{"enforcement": "disabled"})
	if got := decodeRecorderObject(t, rec); rec.Code != http.StatusOK || got["enforcement"] != "disabled" {
		t.Fatalf("repository pre-receive override = %d %#v", rec.Code, got)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete,
		"/api/v3/repos/"+repo.FullName+"/pre-receive-hooks/"+hookSuffix, nil)
	if got := decodeRecorderObject(t, rec); rec.Code != http.StatusOK || got["enforcement"] != "enabled" {
		t.Fatalf("repository inherited enforcement = %d %#v", rec.Code, got)
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete,
		"/api/v3/orgs/"+org.Login+"/pre-receive-hooks/"+hookSuffix, nil)
	if got := decodeRecorderObject(t, rec); rec.Code != http.StatusOK || got["enforcement"] != "testing" {
		t.Fatalf("organization inherited enforcement = %d %#v", rec.Code, got)
	}

	rec = enterpriseActionsRequest(t, s, http.MethodDelete, envPath, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("delete referenced environment = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete,
		"/api/v3/admin/pre-receive-hooks/"+hookSuffix, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete pre-receive hook = %d %q", rec.Code, rec.Body.String())
	}
	rec = enterpriseActionsRequest(t, s, http.MethodDelete, envPath, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete unreferenced environment = %d %q", rec.Code, rec.Body.String())
	}
}

func TestGHESPreReceivePolicyPersists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)
	fixed := time.Date(2026, time.July, 30, 13, 0, 0, 0, time.UTC)
	p1, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	st1 := NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatal(err)
	}
	st1.Mu.Lock()
	st1.EnterpriseSettings.GHESPreReceiveEnvironments[4] = &GHESPreReceiveEnvironment{
		ID: 4, Name: "Node", ImageURL: "https://images.example.test/node.tar.gz", CreatedAt: fixed,
	}
	st1.EnterpriseSettings.GHESPreReceiveHooks[6] = &GHESPreReceiveHook{
		ID: 6, Name: "policy", Script: "policy.sh", EnvironmentID: 4,
		ScriptRepositoryID: 12, Enforcement: "enabled", AllowDownstreamConfiguration: true,
	}
	st1.EnterpriseSettings.GHESOrgPreReceiveOverrides["acme"] = map[int]*GHESPreReceiveOverride{
		6: {Enforcement: "testing", AllowDownstreamConfiguration: true},
	}
	st1.EnterpriseSettings.GHESRepoPreReceiveOverrides["acme/service"] = map[int]*GHESPreReceiveOverride{
		6: {Enforcement: "disabled"},
	}
	st1.EnterpriseSettings.NextGHESPreReceiveEnvironmentID = 5
	st1.EnterpriseSettings.NextGHESPreReceiveHookID = 7
	st1.PersistEnterpriseSettings()
	st1.Mu.Unlock()
	if err := p1.Close(); err != nil {
		t.Fatal(err)
	}

	p2, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	st2 := NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatal(err)
	}
	if st2.EnterpriseSettings.GHESPreReceiveHooks[6] == nil ||
		st2.EnterpriseSettings.GHESOrgPreReceiveOverrides["acme"][6].Enforcement != "testing" ||
		st2.EnterpriseSettings.GHESRepoPreReceiveOverrides["acme/service"][6].Enforcement != "disabled" ||
		st2.EnterpriseSettings.NextGHESPreReceiveHookID != 7 {
		t.Fatalf("reloaded pre-receive settings = %#v", st2.EnterpriseSettings)
	}
}

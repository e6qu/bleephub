package bleephub

import (
	"fmt"
	"net/http"
	"testing"
)

// An organization's external custom properties belong to the GitHub App
// installation that writes them: it is registered once under a unique display
// name, its values are its own, an unregistered installation is refused, and
// uninstalling the app removes the registration and its properties.
func TestExternalCustomPropertiesForRepositories(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	st := s.store
	admin := st.UsersByLogin["admin"]
	org := st.CreateOrg(admin, "ext-props-org", "External properties", "")
	repo := st.CreateOrgRepo(org, admin, "service", "", false)
	if repo == nil {
		t.Fatal("could not create the repository")
	}
	// Registering and listing need admin; the installation registered must
	// hold at least write.
	granted := map[string]string{"metadata": "read", "organization_external_properties_for_repos": "admin"}
	writeOnly := map[string]string{"metadata": "read", "organization_external_properties_for_repos": "write"}
	app := st.CreateApp(admin.ID, "External Props App", "", granted, nil)
	inst := st.CreateInstallation(app.ID, "Organization", org.ID, org.Login, granted, nil)
	other := st.CreateInstallation(st.CreateApp(admin.ID, "Other Props App", "", writeOnly, nil).ID, "Organization", org.ID, org.Login, writeOnly, nil)
	readOnly := map[string]string{"metadata": "read", "organization_external_properties_for_repos": "read"}
	unentitled := st.CreateInstallation(st.CreateApp(admin.ID, "Read Props App", "", readOnly, nil).ID, "Organization", org.ID, org.Login, readOnly, nil)
	if app == nil || inst == nil || other == nil || unentitled == nil {
		t.Fatal("could not install the apps")
	}
	token := st.CreateInstallationToken(inst.ID, app.ID, granted, nil).Token
	base := "/api/v3/orgs/" + org.Login + "/properties/installations"

	resp := s.get(t, base+"/schema", token)
	requireHTTPStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()

	for _, refused := range []map[string]interface{}{
		{"installation_id": inst.ID, "display_name": "not valid!"},
		{"installation_id": inst.ID, "display_name": "SixteenCharsLong"},
		{"installation_id": 999999, "display_name": "Ghost"},
		{"display_name": "NoInstallation"},
		{"installation_id": unentitled.ID, "display_name": "ReadOnly"},
	} {
		resp = s.post(t, base, defaultToken, refused)
		requireHTTPStatus(t, resp, http.StatusUnprocessableEntity)
		resp.Body.Close()
	}

	resp = s.post(t, base, token, map[string]interface{}{"display_name": "Acme"})
	requireHTTPStatus(t, resp, http.StatusCreated)
	created := decodeJSON(t, resp)
	if created["display_name"] != "Acme" || created["installation"].(map[string]interface{})["id"] != float64(inst.ID) {
		t.Fatalf("registration = %v", created)
	}
	resp = s.post(t, base, defaultToken, map[string]interface{}{"installation_id": inst.ID, "display_name": "Again"})
	requireHTTPStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()
	resp = s.post(t, base, defaultToken, map[string]interface{}{"installation_id": other.ID, "display_name": "acme"})
	requireHTTPStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()
	resp = s.post(t, base, defaultToken, map[string]interface{}{"installation_id": other.ID, "display_name": "Other"})
	requireHTTPStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	resp = s.get(t, base, defaultToken)
	requireHTTPStatus(t, resp, http.StatusOK)
	if all := decodeJSONArray(t, resp); len(all) != 2 {
		t.Fatalf("an organization owner sees %d registrations, want 2", len(all))
	}
	resp = s.get(t, base, token)
	requireHTTPStatus(t, resp, http.StatusOK)
	if own := decodeJSONArray(t, resp); len(own) != 1 {
		t.Fatalf("an installation sees %d registrations, want only its own", len(own))
	}

	resp = s.patch(t, base+"/values", token, map[string]interface{}{
		"repository_names": []string{repo.Name},
		"properties":       []map[string]interface{}{{"property_name": "environment", "value": "production"}, {"property_name": "owners", "value": []string{"a", "b"}}},
	})
	requireHTTPStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()
	resp = s.patch(t, base+"/values/business_unit", token, map[string]interface{}{
		"repository_values": []map[string]interface{}{{"repository_name": repo.Name, "value": "payments"}},
	})
	requireHTTPStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()
	resp = s.patch(t, base+"/values", token, map[string]interface{}{
		"repository_names": []string{"missing"},
		"properties":       []map[string]interface{}{{"property_name": "environment", "value": "x"}},
	})
	requireHTTPStatus(t, resp, http.StatusUnprocessableEntity)
	resp.Body.Close()

	resp = s.get(t, base+"/schema", token)
	requireHTTPStatus(t, resp, http.StatusOK)
	names := []string{}
	for _, p := range decodeJSONArray(t, resp) {
		names = append(names, p["property_name"].(string))
	}
	if fmt.Sprint(names) != "[business_unit environment owners]" {
		t.Fatalf("properties = %v", names)
	}

	resp = s.delete(t, base+"/values/environment", token)
	requireHTTPStatus(t, resp, http.StatusNoContent)
	resp.Body.Close()
	resp = s.delete(t, base+"/values/environment", token)
	requireHTTPStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()

	if !st.DeleteInstallation(inst.ID) {
		t.Fatal("could not uninstall the app")
	}
	if st.GetExternalPropertyInstallation(org.Login, inst.ID) != nil {
		t.Fatal("uninstalling the app left its registration")
	}
	if st.GetExternalPropertyInstallation(org.Login, other.ID) == nil {
		t.Fatal("uninstalling one app removed another's registration")
	}
}

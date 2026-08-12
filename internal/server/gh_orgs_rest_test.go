package bleephub

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestOrgUpdateDeleteAllowSiteAdmin guards the operator path on the org
// edit/delete mutations. The /ui Operations → Organizations page lists every
// org on the instance for a site admin and offers Edit/Delete on each row, but
// the seeded site admin holds no personal org membership. handleUpdateOrg and
// handleDeleteOrg must honor SiteAdmin (a GHES site admin administers any org
// from stafftools) or those buttons 403 for every org the operator didn't
// personally create.
func TestOrgUpdateDeleteAllowSiteAdmin(t *testing.T) {
	s := newTestServer()
	s.registerGHOrgRoutes()

	admin := s.store.LookupUserByLogin("admin")
	if admin == nil || !admin.SiteAdmin {
		t.Fatal("seeded admin should be a site admin")
	}

	// An org owned by a non-site-admin user; the site admin is a non-owner.
	owner := seedTestUser(s, "org-owner")
	org := s.store.CreateOrg(owner, "acme-corp", "Acme Corp", "")
	if org == nil {
		t.Fatal("CreateOrg nil")
	}

	// A non-owner, non-site-admin caller is rejected (requirePerm middleware).
	outsider := seedTestUser(s, "org-outsider")
	outTok := s.store.CreateToken(outsider.ID, "admin:org")
	if w := serveTestRequest(s, bearerHeader(outTok.Value), "PATCH", "/api/v3/orgs/acme-corp",
		[]byte(`{"description":"nope"}`)); w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Errorf("outsider PATCH org = %d, want 403/404", w.Code)
	}

	// Site admin (non-owner) can edit the org.
	w := serveTestRequest(s, bearerHeader(store.AdminToken()), "PATCH", "/api/v3/orgs/acme-corp",
		[]byte(`{"description":"edited by operator"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("site-admin PATCH org = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	json.Unmarshal(w.Body.Bytes(), &got)
	if got["description"] != "edited by operator" {
		t.Errorf("description = %v, want %q", got["description"], "edited by operator")
	}

	// Site admin (non-owner) can delete the org.
	if w := serveTestRequest(s, bearerHeader(store.AdminToken()), "DELETE", "/api/v3/orgs/acme-corp", nil); w.Code != http.StatusNoContent {
		t.Fatalf("site-admin DELETE org = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if s.store.GetOrg("acme-corp") != nil {
		t.Error("org should be deleted")
	}
}

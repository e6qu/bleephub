package bleephub

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func TestOrgPinned_SetAndListRequiresOrgOwnedRepos(t *testing.T) {
	s := pinnedTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "pin-org", "Pin Org", "")
	if org == nil {
		t.Fatal("create org")
	}
	if s.store.CreateOrgRepo(org, admin, "alpha", "", false) == nil ||
		s.store.CreateOrgRepo(org, admin, "beta", "", false) == nil {
		t.Fatal("create org repos")
	}
	// A repo the admin owns personally: pinnable on a user profile, but never
	// on the org's.
	s.store.CreateRepo(admin, "personal", "", false)

	w := doPinnedReq(s, adminPAT, "PUT", "/ui-data/orgs/pin-org/pinned",
		[]byte(`{"repos":["pin-org/beta","admin/personal","pin-org/ghost","pin-org/alpha"]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("set status = %d, body = %s", w.Code, w.Body.String())
	}
	var set []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &set)
	if len(set) != 2 || set[0]["full_name"] != "pin-org/beta" || set[1]["full_name"] != "pin-org/alpha" {
		t.Fatalf("set result = %v (want beta then alpha; foreign and ghost dropped)", set)
	}

	w = doPinnedReq(s, adminPAT, "GET", "/ui-data/orgs/pin-org/pinned", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	var got []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got) != 2 {
		t.Fatalf("list len = %d, want 2", len(got))
	}
}

func TestOrgPinned_NonAdminForbiddenAndViewerCanRead(t *testing.T) {
	s := pinnedTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "pin-org-authz", "", "")
	if s.store.CreateOrgRepo(org, admin, "alpha", "", false) == nil {
		t.Fatal("create org repo")
	}

	s.store.Mu.Lock()
	outsider := &store.User{ID: s.store.NextUser, Login: "pin-outsider", Type: "User"}
	s.store.NextUser++
	s.store.Users[outsider.ID] = outsider
	s.store.UsersByLogin[outsider.Login] = outsider
	s.store.Mu.Unlock()
	outsiderToken := s.store.CreateToken(outsider.ID, "repo, read:org, admin:org").Value

	// PUT is owner-only.
	w := doPinnedReq(s, outsiderToken, "PUT", "/ui-data/orgs/pin-org-authz/pinned",
		[]byte(`{"repos":["pin-org-authz/alpha"]}`))
	if w.Code != http.StatusForbidden {
		t.Fatalf("outsider set status = %d, want 403", w.Code)
	}

	// GET is open to any viewer (public repos render).
	if w := doPinnedReq(s, adminPAT, "PUT", "/ui-data/orgs/pin-org-authz/pinned",
		[]byte(`{"repos":["pin-org-authz/alpha"]}`)); w.Code != http.StatusOK {
		t.Fatalf("owner set status = %d, body = %s", w.Code, w.Body.String())
	}
	w = doPinnedReq(s, outsiderToken, "GET", "/ui-data/orgs/pin-org-authz/pinned", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("outsider list status = %d", w.Code)
	}
	var got []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got) != 1 || got[0]["full_name"] != "pin-org-authz/alpha" {
		t.Fatalf("outsider list = %v", got)
	}
}

func TestOrgPinned_TooManyIs422AndUnknownOrgIs404(t *testing.T) {
	s := pinnedTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	s.store.CreateOrg(admin, "pin-org-limits", "", "")

	w := doPinnedReq(s, adminPAT, "PUT", "/ui-data/orgs/pin-org-limits/pinned",
		[]byte(`{"repos":["o/1","o/2","o/3","o/4","o/5","o/6","o/7"]}`))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("too-many status = %d, want 422", w.Code)
	}

	if w := doPinnedReq(s, adminPAT, "GET", "/ui-data/orgs/no-such-org/pinned", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown org list status = %d, want 404", w.Code)
	}
	if w := doPinnedReq(s, adminPAT, "PUT", "/ui-data/orgs/no-such-org/pinned", []byte(`{"repos":[]}`)); w.Code != http.StatusNotFound {
		t.Fatalf("unknown org set status = %d, want 404", w.Code)
	}
}

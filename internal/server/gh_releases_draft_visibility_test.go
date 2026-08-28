package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDraftReleaseHiddenFromReaders pins that an unpublished release is readable only with push access — fetching a draft by id or tag once leaked notes the list endpoint already hid.
func TestDraftReleaseHiddenFromReaders(t *testing.T) {
	s := newTestServer()
	s.store.SeedDefaultUser()
	s.registerGHReleasesRoutes()

	owner := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(owner, "draft-visibility", "", false)
	initializeReleaseTestRepo(t, s, repo, owner)

	release := s.store.Releases.Create(repo.ID, owner.ID, "v9.9.9", repo.DefaultBranch,
		"Unannounced", "embargoed notes", true, false, false)

	_, strangerToken := authflowStranger(t, s, "release-reader")

	do := func(path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.requestHandler().ServeHTTP(w, req)
		return w
	}

	base := "/api/v3/repos/admin/draft-visibility/releases/"
	for _, path := range []string{base + itoa(release.ID), base + "tags/v9.9.9"} {
		if w := do(path, strangerToken); w.Code != http.StatusNotFound {
			t.Errorf("GET %s as a non-pusher = %d, want 404; body %s", path, w.Code, w.Body.String())
		}
		w := do(path, "bleephub-admin-token-00000000000000000000")
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s as the owner = %d, want 200; body %s", path, w.Code, w.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["draft"] != true {
			t.Errorf("owner view of %s: draft = %v, want true", path, got["draft"])
		}
	}
}

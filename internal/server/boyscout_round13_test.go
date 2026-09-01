package bleephub

import (
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestDeleteCodespaceSurvivesConcurrentStateFlip pins the fix for the
// codespace-delete race that flaked TestCodespaces_OrgList under load: the
// create's async provisioning/lifecycle goroutine could land a
// SetCodespaceState("Available") write in the window where DeleteCodespace has
// set "Deleting" and is running the (unlocked) runtime delete, flipping the
// state out from under it and failing the delete with a 500. SetCodespaceState
// now ignores a codespace that is already "Deleting".
func TestDeleteCodespaceSurvivesConcurrentStateFlip(t *testing.T) {
	t.Parallel()
	st := store.NewStore()
	st.SeedDefaultUser()
	cs := &store.Codespace{
		ID:         st.NextCodespaceID,
		Name:       "flip-race",
		OwnerLogin: "admin",
		State:      "Available",
	}
	st.NextCodespaceID++
	st.Codespaces[cs.ID] = cs
	st.CodespacesByName[cs.Name] = cs

	// The runtime delete runs while State == "Deleting" (store lock released);
	// simulate the lifecycle goroutine's state write landing here.
	st.CodespaceRuntimeDelete = func(*store.Codespace) error {
		st.SetCodespaceState(cs.ID, "Available", true)
		return nil
	}

	ok, err := st.DeleteCodespace(cs.ID)
	if err != nil {
		t.Fatalf("delete failed on a concurrent state flip: %v", err)
	}
	if !ok {
		t.Fatal("delete returned not-ok")
	}
	if st.GetCodespaceByName("flip-race") != nil {
		t.Fatal("codespace was not removed after delete")
	}
}

// TestArchivedRepoIsReadOnly pins that an archived repository rejects content
// writes with 403 (GitHub: "Repository was archived so is read-only."), still
// serves reads, and becomes writable again when unarchived.
func TestArchivedRepoIsReadOnly(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "arch-repo", "auto_init": true,
	}).Body.Close()
	// Seed an issue while writable (so the comment path has a target).
	s.post(t, "/api/v3/repos/admin/arch-repo/issues", defaultToken,
		map[string]interface{}{"title": "before archive"}).Body.Close()

	// Archive it.
	s.patch(t, "/api/v3/repos/admin/arch-repo", defaultToken,
		map[string]interface{}{"archived": true}).Body.Close()

	// Content writes are refused with 403.
	writes := []struct {
		name, method, path string
		body               map[string]interface{}
	}{
		{"create issue", http.MethodPost, "/api/v3/repos/admin/arch-repo/issues", map[string]interface{}{"title": "nope"}},
		{"comment", http.MethodPost, "/api/v3/repos/admin/arch-repo/issues/1/comments", map[string]interface{}{"body": "nope"}},
		{"create PR", http.MethodPost, "/api/v3/repos/admin/arch-repo/pulls", map[string]interface{}{"title": "x", "head": "main", "base": "main"}},
	}
	for _, wr := range writes {
		resp := s.do(t, wr.method, wr.path, defaultToken, wr.body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s on archived repo = %d, want 403", wr.name, resp.StatusCode)
		}
	}

	// Reads still work.
	get := s.get(t, "/api/v3/repos/admin/arch-repo", defaultToken)
	get.Body.Close()
	if get.StatusCode != http.StatusOK {
		t.Fatalf("read of archived repo = %d, want 200", get.StatusCode)
	}

	// Unarchiving restores writability.
	s.patch(t, "/api/v3/repos/admin/arch-repo", defaultToken,
		map[string]interface{}{"archived": false}).Body.Close()
	iss := s.post(t, "/api/v3/repos/admin/arch-repo/issues", defaultToken,
		map[string]interface{}{"title": "after unarchive"})
	iss.Body.Close()
	if iss.StatusCode != http.StatusCreated {
		t.Fatalf("create issue after unarchive = %d, want 201", iss.StatusCode)
	}
}

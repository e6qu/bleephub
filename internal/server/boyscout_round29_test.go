package bleephub

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestReleaseWritesRejectedOnArchivedRepo pins that creating a release on an
// archived (read-only) repository is refused with GitHub's 403, like every other
// content write. The release handlers previously skipped the archived check that
// issues/pulls/git writes all apply.
func TestReleaseWritesRejectedOnArchivedRepo(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "arch-rel", false)
	s.store.Mu.Lock()
	s.store.Repos[repo.ID].Archived = true
	s.store.Mu.Unlock()

	resp := s.post(t, "/api/v3/repos/admin/arch-rel/releases", defaultToken, map[string]interface{}{"tag_name": "v1.0.0"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("create release on archived repo = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(string(body), "archived") {
		t.Fatalf("archived release body = %s, want the read-only message", body)
	}
}

// TestGenerateNotesDerivesPreviousTag pins that POST /releases/generate-notes
// with no previous_tag_name bounds the changelog at the newest existing release,
// as GitHub (and the create path) do. It previously spanned the whole history,
// emitting an empty base in the Full Changelog line.
func TestGenerateNotesDerivesPreviousTag(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "notes-rel", false)
	admin := s.store.LookupUserByLogin("admin")
	if s.store.Releases.Create(repo.ID, admin.ID, "v1.0.0", "", "V1", "", false, false, false) == nil {
		t.Fatal("seed release failed")
	}

	resp := s.post(t, "/api/v3/repos/admin/notes-rel/releases/generate-notes", defaultToken, map[string]interface{}{
		"tag_name": "v2.0.0",
	})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("generate-notes = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "v1.0.0...v2.0.0") {
		t.Fatalf("changelog did not derive the previous tag; body = %s", body)
	}
}

package bleephub

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/server/testutil"
)

// waitForMigrationExport polls a migration until its export worker has landed
// it in a terminal state, and fails loudly on "failed" with the reason the
// worker recorded rather than timing out on a state that will never change.
func (s *isolatedServer) waitForMigrationExport(t *testing.T, path string) {
	t.Helper()
	var last map[string]any
	if !testutil.TestEventually(20*time.Second, 25*time.Millisecond, func() bool {
		resp := s.get(t, path, defaultToken)
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return false
		}
		last = decodeJSON(t, resp)
		state, _ := last["state"].(string)
		return state == "exported" || state == "failed"
	}) {
		t.Fatalf("%s never left the pending/exporting states: %v", path, last)
	}
	if state, _ := last["state"].(string); state != "exported" {
		t.Fatalf("%s ended %s: %v", path, state, last)
	}
}

func TestMigrations_UserCRUD(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	r1 := s.store.CreateRepo(admin, "migration-repo-1", "first migration repo", false)
	r2 := s.store.CreateRepo(admin, "migration-repo-2", "second migration repo", false)

	// Start migration
	resp := s.post(t, "/api/v3/user/migrations", defaultToken, map[string]any{
		"repositories":      []string{r1.FullName, r2.FullName},
		"lock_repositories": true,
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create user migration: %d %s", resp.StatusCode, b)
	}
	created := decodeJSON(t, resp)
	// A migration is created pending: the export is real work an owned worker
	// does, so the 201 reports that it was queued rather than that it is done.
	if created["state"] != "pending" {
		t.Fatalf("expected state pending on creation, got %v", created["state"])
	}
	if len(created["repositories"].([]any)) != 2 {
		t.Fatalf("expected 2 repositories, got %v", created["repositories"])
	}
	migrationID := int(created["id"].(float64))
	s.waitForMigrationExport(t, "/api/v3/user/migrations/"+itoa(migrationID))

	// List
	resp = s.get(t, "/api/v3/user/migrations", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list user migrations: %d %s", resp.StatusCode, b)
	}
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	resp.Body.Close()
	found := false
	for _, m := range list {
		if int(m["id"].(float64)) == migrationID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("created migration %d not in list: %v", migrationID, list)
	}

	// Get
	resp = s.get(t, "/api/v3/user/migrations/"+itoa(migrationID), defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("get user migration: %d %s", resp.StatusCode, b)
	}
	got := decodeJSON(t, resp)
	if got["lock_repositories"] != true {
		t.Fatalf("expected lock_repositories true")
	}

	// Download archive
	resp = s.get(t, "/api/v3/user/migrations/"+itoa(migrationID)+"/archive", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("download archive: %d %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/gzip" {
		t.Fatalf("expected Content-Type application/gzip, got %s", ct)
	}
	if disp := resp.Header.Get("Content-Disposition"); !strings.Contains(disp, "migration-"+itoa(migrationID)+".tar.gz") {
		t.Fatalf("unexpected Content-Disposition: %s", disp)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	tr := tar.NewReader(gz)
	entries := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		entries[h.Name] = true
	}
	resp.Body.Close()
	// The archive is the export the worker actually produced: the schema stamp
	// that says which layout it is, the migration's own record, and the roll of
	// repositories it carries.
	for _, want := range []string{"schema.json", "migration.json", "repositories_000001.json"} {
		if !entries[want] {
			t.Fatalf("archive is missing %s; it holds %v", want, entries)
		}
	}
	if !entries["repositories/"+r1.FullName+"/issues_000001.json"] {
		t.Fatalf("archive is missing %s's records; it holds %v", r1.FullName, entries)
	}

	// Delete archive
	resp = s.delete(t, "/api/v3/user/migrations/"+itoa(migrationID)+"/archive", defaultToken)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("delete archive: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	// Download after delete returns 404
	resp = s.get(t, "/api/v3/user/migrations/"+itoa(migrationID)+"/archive", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after archive delete, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Unlock repo
	resp = s.delete(t, "/api/v3/user/migrations/"+itoa(migrationID)+"/repos/"+r1.Name+"/lock", defaultToken)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("unlock repo: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	// Unlock non-locked repo returns 404
	resp = s.delete(t, "/api/v3/user/migrations/"+itoa(migrationID)+"/repos/"+r1.Name+"/lock", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 re-unlock, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestMigrations_OrgCRUD(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "migration-org", "Migration Org", "")
	r1 := s.store.CreateOrgRepo(org, admin, "org-repo", "org migration repo", false)

	// Start org migration
	resp := s.post(t, "/api/v3/orgs/"+org.Login+"/migrations", defaultToken, map[string]any{
		"repositories":      []string{r1.FullName},
		"lock_repositories": true,
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create org migration: %d %s", resp.StatusCode, b)
	}
	created := decodeJSON(t, resp)
	migrationID := int(created["id"].(float64))
	s.waitForMigrationExport(t, "/api/v3/orgs/"+org.Login+"/migrations/"+itoa(migrationID))

	// List
	resp = s.get(t, "/api/v3/orgs/"+org.Login+"/migrations", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list org migrations: %d %s", resp.StatusCode, b)
	}
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	resp.Body.Close()
	found := false
	for _, m := range list {
		if int(m["id"].(float64)) == migrationID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("created org migration %d not in list: %v", migrationID, list)
	}

	// Get
	resp = s.get(t, "/api/v3/orgs/"+org.Login+"/migrations/"+itoa(migrationID), defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("get org migration: %d %s", resp.StatusCode, b)
	}
	got := decodeJSON(t, resp)
	if got["state"] != "exported" {
		t.Fatalf("expected exported, got %v", got["state"])
	}

	// Unlock
	resp = s.delete(t, "/api/v3/orgs/"+org.Login+"/migrations/"+itoa(migrationID)+"/repos/"+r1.Name+"/lock", defaultToken)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("unlock org repo: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	// Repeating the documented unlock after it has already been unlocked
	// returns 404; there is no GitHub GET lock-status operation.
	resp = s.delete(t, "/api/v3/orgs/"+org.Login+"/migrations/"+itoa(migrationID)+"/repos/"+r1.Name+"/lock", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected second unlock to return 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp.Body.Close()
}

func TestMigrations_UserListPagination(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestRepo(t).fullName()
	for i := 0; i < 2; i++ {
		resp := s.post(t, "/api/v3/user/migrations", defaultToken, map[string]any{
			"repositories": []string{repo},
		})
		if resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("create migration %d: %d %s", i, resp.StatusCode, b)
		}
		resp.Body.Close()
	}

	resp := s.get(t, "/api/v3/user/migrations?per_page=1", defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("page 1: %d", resp.StatusCode)
	}
	link := resp.Header.Get("Link")
	var page1 []map[string]any
	json.NewDecoder(resp.Body).Decode(&page1)
	resp.Body.Close()
	if len(page1) != 1 {
		t.Fatalf("page 1 len = %d, want 1", len(page1))
	}
	if !strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 1 Link = %q, want rel=next", link)
	}

	resp = s.get(t, "/api/v3/user/migrations?per_page=1&page=2", defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("page 2: %d", resp.StatusCode)
	}
	var page2 []map[string]any
	json.NewDecoder(resp.Body).Decode(&page2)
	resp.Body.Close()
	if len(page2) != 1 {
		t.Fatalf("page 2 len = %d, want 1", len(page2))
	}
	if page1[0]["id"] == page2[0]["id"] {
		t.Fatalf("page 1 and page 2 returned the same migration: %v", page1[0]["id"])
	}
}

func TestMigrations_OrgListPagination(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.seedTestOrg(t, "migration-page-org")
	repo := s.seedOrgRepo(t, org, "migration-page-org-repo", false)
	for i := 0; i < 2; i++ {
		resp := s.post(t, "/api/v3/orgs/"+org.Login+"/migrations", defaultToken, map[string]any{
			"repositories": []string{repo.FullName},
		})
		if resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("create migration %d: %d %s", i, resp.StatusCode, b)
		}
		resp.Body.Close()
	}

	resp := s.get(t, "/api/v3/orgs/"+org.Login+"/migrations?per_page=1", defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("page 1: %d", resp.StatusCode)
	}
	link := resp.Header.Get("Link")
	var page1 []map[string]any
	json.NewDecoder(resp.Body).Decode(&page1)
	resp.Body.Close()
	if len(page1) != 1 {
		t.Fatalf("page 1 len = %d, want 1", len(page1))
	}
	if !strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 1 Link = %q, want rel=next", link)
	}

	resp = s.get(t, "/api/v3/orgs/"+org.Login+"/migrations?per_page=1&page=2", defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("page 2: %d", resp.StatusCode)
	}
	var page2 []map[string]any
	json.NewDecoder(resp.Body).Decode(&page2)
	resp.Body.Close()
	if len(page2) != 1 {
		t.Fatalf("page 2 len = %d, want 1", len(page2))
	}
	if page1[0]["id"] == page2[0]["id"] {
		t.Fatalf("page 1 and page 2 returned the same migration: %v", page1[0]["id"])
	}
}

func TestMigrations_404s(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	// Missing user migration
	resp := s.get(t, "/api/v3/user/migrations/999999", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing user migration, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Missing org
	resp = s.get(t, "/api/v3/orgs/nonexistent/migrations", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing org, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Missing org migration
	resp = s.get(t, "/api/v3/orgs/nonexistent/migrations/999999", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing org migration, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Unlock on missing migration
	resp = s.delete(t, "/api/v3/user/migrations/999999/repos/foo/lock", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 unlock missing migration, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestMigrations_StartRequiresAuth(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp := s.post(t, "/api/v3/user/migrations", "", map[string]any{"repositories": []string{"a/b"}})
	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 401 without token, got %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

func TestMigrations_StartValidation(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	// Missing repositories
	resp := s.post(t, "/api/v3/user/migrations", defaultToken, map[string]any{})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 422 missing repos, got %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	// Invalid repository
	resp = s.post(t, "/api/v3/user/migrations", defaultToken, map[string]any{
		"repositories": []string{"does/not-exist"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 422 invalid repo, got %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

func TestMigrations_OrgMigrationRepositories(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "migration-repos-org", "Migration Repos Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}
	r1 := s.store.CreateOrgRepo(org, admin, "migration-repos-1", "", false)
	r2 := s.store.CreateOrgRepo(org, admin, "migration-repos-2", "", false)
	if r1 == nil || r2 == nil {
		t.Fatal("create org repos failed")
	}

	resp := s.post(t, "/api/v3/orgs/migration-repos-org/migrations", defaultToken, map[string]any{
		"repositories": []string{r1.FullName, r2.FullName},
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("start org migration: %d %s", resp.StatusCode, b)
	}
	created := decodeJSON(t, resp)
	migrationID := int(created["id"].(float64))

	resp = s.get(t, fmt.Sprintf("/api/v3/orgs/migration-repos-org/migrations/%d/repositories", migrationID), defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("list migration repositories: %d", resp.StatusCode)
	}
	repos := decodeJSONArray(t, resp)
	if len(repos) != 2 {
		t.Fatalf("expected 2 migration repositories, got %v", repos)
	}
	names := map[string]bool{}
	for _, repo := range repos {
		fullName, _ := repo["full_name"].(string)
		names[fullName] = true
	}
	if !names[r1.FullName] || !names[r2.FullName] {
		t.Fatalf("migration repositories wrong: %v", names)
	}

	// Unknown migration.
	resp = s.get(t, "/api/v3/orgs/migration-repos-org/migrations/999999/repositories", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown migration repositories: %d, want 404", resp.StatusCode)
	}
}

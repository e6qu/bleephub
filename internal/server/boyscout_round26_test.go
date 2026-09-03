package bleephub

import (
	"net/http"
	"testing"
)

// TestPrivatePackageDoesNotLeakExistence pins that a private package the caller
// cannot view returns 404 — the same as a package that does not exist — rather
// than a 200 with an empty body. The status difference previously let any
// authenticated user enumerate another account's private package names.
func TestPrivatePackageDoesNotLeakExistence(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	if _, ok := s.store.CreatePackage("User", "admin", "npm", "secretpkg", "private"); !ok {
		t.Fatal("seed private package failed")
	}
	_, outsiderTok := s.newUser(t, "pkg-outsider")

	existing := s.get(t, "/api/v3/users/admin/packages/npm/secretpkg", outsiderTok)
	existing.Body.Close()
	missing := s.get(t, "/api/v3/users/admin/packages/npm/no-such-pkg", outsiderTok)
	missing.Body.Close()

	if existing.StatusCode != http.StatusNotFound {
		t.Fatalf("private package to a non-viewer = %d, want 404 (leaks existence)", existing.StatusCode)
	}
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing package = %d, want 404", missing.StatusCode)
	}
}

// TestPublicUserPackageVersionsPaginate pins that the public user-package
// version list honors per_page/page and emits a Link header, matching its
// authenticated-user and org siblings. It previously returned the whole list
// and no Link header.
func TestPublicUserPackageVersionsPaginate(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	if _, ok := s.store.CreatePackage("User", "admin", "npm", "paged-pub", "public"); !ok {
		t.Fatal("seed package failed")
	}
	for _, v := range []string{"1.0.0", "2.0.0", "3.0.0"} {
		if _, err := s.store.CreatePackageVersion("User", "admin", "npm", "paged-pub", v, "", nil, nil); err != nil {
			t.Fatalf("seed version %s: %v", v, err)
		}
	}

	resp := s.get(t, "/api/v3/users/admin/packages/npm/paged-pub/versions?per_page=1", defaultToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list versions = %d, want 200", resp.StatusCode)
	}
	versions := decodeJSONArray(t, resp)
	if len(versions) != 1 {
		t.Fatalf("per_page=1 returned %d versions, want 1 (pagination ignored)", len(versions))
	}
	if resp.Header.Get("Link") == "" {
		t.Fatal("paginated version list emitted no Link header")
	}
}

// TestRepoMarkReadClearsGlobalInbox pins that marking a repository's
// notifications read (PUT /repos/{o}/{r}/notifications) clears its threads from
// the whole inbox, not just the repo-scoped listing. The global list previously
// consulted only the global last-read marker and ignored the per-repo one.
func TestRepoMarkReadClearsGlobalInbox(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "notif26", "", false)
	s.store.CreateIssue(repo.ID, admin.ID, "bug", "body", nil, nil, 0)

	// The authored thread starts unread in the global inbox.
	before := decodeJSONArray(t, s.get(t, "/api/v3/notifications", defaultToken))
	if len(before) != 1 || before[0]["unread"] != true {
		t.Fatalf("expected one unread thread before marking read, got %v", before)
	}

	resp := s.put(t, "/api/v3/repos/admin/notif26/notifications", defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("mark repo read = %d, want 202", resp.StatusCode)
	}

	// The global inbox must now show the thread as read.
	after := decodeJSONArray(t, s.get(t, "/api/v3/notifications?all=true", defaultToken))
	if len(after) != 1 {
		t.Fatalf("expected the thread to remain listed with all=true, got %v", after)
	}
	if after[0]["unread"] != false {
		t.Fatal("marking the repository read did not clear the thread from the global inbox")
	}
}

package bleephub

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/server/testutil"
)

// GitHub resolves owner logins, organization logins and repository names
// case-insensitively while payloads keep the canonical casing (no redirect is
// involved). These tests pin that contract across distant surfaces that all
// ride on the store's folded lookups: REST, GraphQL, the git smart-HTTP
// transport and /ui-data — plus the rename flows that must re-point the
// folded index.

func caseResName(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, strconv.FormatInt(int64(testutil.NextTestID()), 36))
}

// TestRESTRepoResolvesCaseInsensitively pins GET /repos/{owner}/{repo} with a
// case-variant owner and name: 200, payload carries the canonical casing.
func TestRESTRepoResolvesCaseInsensitively(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := caseResName("Case-Repo")
	mustPost(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": name}))

	resp := s.get(t, "/api/v3/repos/ADMIN/"+strings.ToUpper(name), defaultToken)
	repo := decodeJSONWithStatus(t, resp, http.StatusOK)
	if got := repo["full_name"]; got != "admin/"+name {
		t.Errorf("full_name = %v, want %q (canonical casing)", got, "admin/"+name)
	}
	owner, _ := repo["owner"].(map[string]interface{})
	if owner == nil || owner["login"] != "admin" {
		t.Errorf("owner.login = %v, want canonical \"admin\"", owner["login"])
	}
}

// TestRESTUserResolvesCaseInsensitively pins GET /users/{username} with a
// case-variant login: 200 with the canonical login in the payload.
func TestRESTUserResolvesCaseInsensitively(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp := s.get(t, "/api/v3/users/Admin", defaultToken)
	user := decodeJSONWithStatus(t, resp, http.StatusOK)
	if user["login"] != "admin" {
		t.Errorf("login = %v, want canonical \"admin\"", user["login"])
	}
}

// TestRESTOrgResolvesCaseInsensitively pins GET /orgs/{org} and an org
// sub-resource (members) under a case-variant login.
func TestRESTOrgResolvesCaseInsensitively(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	login := caseResName("case-org")
	s.createOrg(t, login)

	variant := strings.ToUpper(login)
	org := decodeJSONWithStatus(t, s.get(t, "/api/v3/orgs/"+variant, defaultToken), http.StatusOK)
	if org["login"] != login {
		t.Errorf("org login = %v, want canonical %q", org["login"], login)
	}
	// A derived-key surface (memberships) must also resolve the variant.
	requireStatus(t, s.get(t, "/api/v3/orgs/"+variant+"/members", defaultToken), http.StatusOK)
}

// TestGraphQLRepositoryResolvesCaseInsensitively pins repository(owner:,
// name:) — a distant surface that rides on the same store accessors.
func TestGraphQLRepositoryResolvesCaseInsensitively(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := caseResName("gql-case-repo")
	mustPost(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": name}))

	data := s.gqlData(t, `query($o:String!,$n:String!){repository(owner:$o,name:$n){nameWithOwner}}`,
		map[string]interface{}{"o": "ADMIN", "n": strings.ToUpper(name)})
	repo, _ := data["repository"].(map[string]interface{})
	if repo == nil || repo["nameWithOwner"] != "admin/"+name {
		t.Errorf("nameWithOwner = %v, want %q", data["repository"], "admin/"+name)
	}
}

// TestGitInfoRefsResolvesCaseInsensitively pins the git smart-HTTP transport:
// a case-variant clone URL advertises refs like real GitHub.
func TestGitInfoRefsResolvesCaseInsensitively(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := caseResName("git-case-repo")
	mustPost(t, s.post(t, "/api/v3/user/repos", defaultToken,
		map[string]interface{}{"name": name, "auto_init": true}))

	resp := s.get(t, "/ADMIN/"+strings.ToUpper(name)+".git/info/refs?service=git-upload-pack", defaultToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("case-variant info/refs = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-git-upload-pack-advertisement" {
		t.Errorf("Content-Type = %q, want upload-pack advertisement", ct)
	}
}

// TestUIDataViewerResolvesCaseInsensitively pins the /ui-data bootstrap
// surface under a case-variant repo path.
func TestUIDataViewerResolvesCaseInsensitively(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := caseResName("uidata-case-repo")
	mustPost(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": name}))

	requireStatus(t, s.get(t, "/ui-data/repos/ADMIN/"+strings.ToUpper(name)+"/viewer", defaultToken), http.StatusOK)
}

// TestRenameRepointsFoldedRepoResolution pins that a rename moves the folded
// key with the canonical one: every casing of the old name stops resolving
// (bleephub keeps no rename redirects) and every casing of the new name
// resolves to the canonical new full name.
func TestRenameRepointsFoldedRepoResolution(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	oldName := caseResName("fold-rename-old")
	newName := caseResName("Fold-Rename-New")
	mustPost(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": oldName}))

	renamed := decodeJSONWithStatus(t,
		s.patch(t, "/api/v3/repos/admin/"+oldName, defaultToken, map[string]interface{}{"name": newName}),
		http.StatusOK)
	if renamed["full_name"] != "admin/"+newName {
		t.Fatalf("rename full_name = %v, want %q", renamed["full_name"], "admin/"+newName)
	}

	requireStatus(t, s.get(t, "/api/v3/repos/ADMIN/"+strings.ToUpper(oldName), defaultToken), http.StatusNotFound)
	requireStatus(t, s.get(t, "/api/v3/repos/admin/"+oldName, defaultToken), http.StatusNotFound)
	got := decodeJSONWithStatus(t, s.get(t, "/api/v3/repos/ADMIN/"+strings.ToUpper(newName), defaultToken), http.StatusOK)
	if got["full_name"] != "admin/"+newName {
		t.Errorf("post-rename full_name = %v, want %q", got["full_name"], "admin/"+newName)
	}
}

// TestCaseOnlyRepoRenameAllowed pins that renaming a repository to a
// different casing of its own name succeeds (GitHub allows it) instead of
// tripping the folded-collision check.
func TestCaseOnlyRepoRenameAllowed(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := caseResName("case-only-rename")
	mustPost(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": name}))

	upper := strings.ToUpper(name)
	renamed := decodeJSONWithStatus(t,
		s.patch(t, "/api/v3/repos/admin/"+name, defaultToken, map[string]interface{}{"name": upper}),
		http.StatusOK)
	if renamed["full_name"] != "admin/"+upper {
		t.Fatalf("case-only rename full_name = %v, want %q", renamed["full_name"], "admin/"+upper)
	}
	// Both casings still resolve to the one repository, canonical in payload.
	got := decodeJSONWithStatus(t, s.get(t, "/api/v3/repos/admin/"+name, defaultToken), http.StatusOK)
	if got["full_name"] != "admin/"+upper {
		t.Errorf("lower-cased lookup full_name = %v, want %q", got["full_name"], "admin/"+upper)
	}
}

// TestCaseVariantDuplicateRepoRejected pins the collision guard the folded
// index relies on: a create whose name differs from a live repository only by
// case is refused, as on GitHub.
func TestCaseVariantDuplicateRepoRejected(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := caseResName("dup-case-repo")
	mustPost(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": name}))

	resp := s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": strings.ToUpper(name)})
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		t.Fatalf("case-variant duplicate create = 201; want a conflict refusal")
	}
}

// TestAdminUserRenameRepointsFoldedLogin pins that the admin rename flow
// (a primary-map mutation living in the server package) keeps the folded
// login index in step: the old login stops resolving under any casing and
// the new one resolves case-insensitively.
func TestAdminUserRenameRepointsFoldedLogin(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	oldLogin := caseResName("fold-user-old")
	newLogin := caseResName("fold-user-new")
	mustPost(t, s.post(t, "/api/v3/admin/users", defaultToken, map[string]interface{}{"login": oldLogin}))

	requireStatus(t,
		s.patch(t, "/api/v3/admin/users/"+strings.ToUpper(oldLogin), defaultToken,
			map[string]interface{}{"login": newLogin}),
		http.StatusAccepted)

	requireStatus(t, s.get(t, "/api/v3/users/"+strings.ToUpper(oldLogin), defaultToken), http.StatusNotFound)
	requireStatus(t, s.get(t, "/api/v3/users/"+oldLogin, defaultToken), http.StatusNotFound)
	user := decodeJSONWithStatus(t, s.get(t, "/api/v3/users/"+strings.ToUpper(newLogin), defaultToken), http.StatusOK)
	if user["login"] != newLogin {
		t.Errorf("renamed login = %v, want canonical %q", user["login"], newLogin)
	}
}

// TestGHESOrgRenameRepointsFoldedLogin pins that the GHES org rename (a
// primary-map mutation living in the server package, which also re-keys every
// owned repository) keeps the folded org and repo indexes in step.
func TestGHESOrgRenameRepointsFoldedLogin(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	oldLogin := caseResName("fold-org-old")
	newLogin := caseResName("fold-org-new")
	s.createOrg(t, oldLogin)
	repo, _ := s.createOrgRepoForGovernance(t, oldLogin)

	requireStatus(t,
		s.patch(t, "/api/v3/admin/organizations/"+strings.ToUpper(oldLogin), defaultToken,
			map[string]interface{}{"login": newLogin}),
		http.StatusAccepted)

	requireStatus(t, s.get(t, "/api/v3/orgs/"+strings.ToUpper(oldLogin), defaultToken), http.StatusNotFound)
	org := decodeJSONWithStatus(t, s.get(t, "/api/v3/orgs/"+strings.ToUpper(newLogin), defaultToken), http.StatusOK)
	if org["login"] != newLogin {
		t.Errorf("renamed org login = %v, want canonical %q", org["login"], newLogin)
	}
	// The owned repository moved namespaces; its folded key must follow.
	requireStatus(t, s.get(t, "/api/v3/repos/"+strings.ToUpper(oldLogin)+"/"+repo.name, defaultToken), http.StatusNotFound)
	moved := decodeJSONWithStatus(t,
		s.get(t, "/api/v3/repos/"+strings.ToUpper(newLogin)+"/"+strings.ToUpper(repo.name), defaultToken),
		http.StatusOK)
	if moved["full_name"] != newLogin+"/"+repo.name {
		t.Errorf("moved repo full_name = %v, want %q", moved["full_name"], newLogin+"/"+repo.name)
	}
}

// TestTransferRepoTreatsCaseVariantOwnerAsSameOwner pins the security-shaped
// comparison inside TransferRepo: a transfer naming the current owner in a
// different casing is the identity transfer, not a move to a second owner.
func TestTransferRepoTreatsCaseVariantOwnerAsSameOwner(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := caseResName("transfer-case-repo")
	mustPost(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": name}))

	if !s.store.TransferRepo("ADMIN", strings.ToUpper(name), "Admin") {
		t.Fatal("case-variant identity transfer failed")
	}
	repo := s.store.GetRepoByFullName("admin/" + name)
	if repo == nil || repo.FullName != "admin/"+name {
		t.Fatalf("repository lost its canonical key after identity transfer: %+v", repo)
	}
}

package bleephub

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestPackageVersionGetReturnsDetachedSnapshot pins STORE-021 for the package
// family: GetPackageVersion must return a copy, including its Metadata map.
func TestPackageVersionGetReturnsDetachedSnapshot(t *testing.T) {
	s := newTestServer()
	pkg, _ := s.store.CreatePackage("User", "admin", "npm", "detach-pkg", "private")
	v, err := s.store.CreatePackageVersion("User", "admin", "npm", pkg.Name, "1.0.0", "desc",
		map[string]interface{}{"tag": "latest"}, nil)
	if err != nil {
		t.Fatalf("create version: %v", err)
	}

	got := s.store.GetPackageVersion(v.ID)
	got.Description = "hacked"
	got.Metadata["tag"] = "hacked"

	again := s.store.GetPackageVersion(v.ID)
	if again.Description == "hacked" || again.Metadata["tag"] != "latest" {
		t.Fatalf("package version mutated through the getter: desc=%q metadata=%v", again.Description, again.Metadata)
	}
}

func TestPackageFixturesDoNotSeedContainerPackagesThroughInternalRoute(t *testing.T) {
	for _, path := range []string{"gh_packages_test.go", "gh_packages_live_test.go", "gh_packages_user_surface_test.go"} {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, needle := range []string{
			strings.Join([]string{`seedPackageVersion`, `(t, "user", admin.Login, "container"`}, ""),
			strings.Join([]string{`seedPackageVersion`, `(t, "org", org.Login, "container"`}, ""),
			strings.Join([]string{`seed`, `("user", admin.Login, "container"`}, ""),
		} {
			if strings.Contains(string(source), needle) {
				t.Fatalf("%s still seeds a container package through internal package setup; publish it through /v2/", path)
			}
		}
	}
}

func TestPackages_UserCRUD(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	pkgID, versionID := s.publishContainerPackageVersion(t, admin.Login, "user-pkg", "1.0.0")

	// package_type is a required query parameter.
	resp := s.get(t, "/api/v3/users/"+admin.Login+"/packages?package_type=container", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list user packages: %d %s", resp.StatusCode, b)
	}
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	resp.Body.Close()
	found := false
	for _, p := range list {
		if p["name"] == "user-pkg" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("seeded user-pkg not in list: %v", list)
	}

	resp = s.get(t, "/api/v3/users/"+admin.Login+"/packages/container/user-pkg", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("get user package: %d %s", resp.StatusCode, b)
	}
	pkg := decodeJSON(t, resp)
	if pkg["id"] == nil {
		t.Fatal("missing package id")
	}
	if pkg["owner"].(map[string]any)["login"] != admin.Login {
		t.Fatalf("expected owner login %s, got %v", admin.Login, pkg["owner"])
	}

	resp = s.get(t, "/api/v3/users/"+admin.Login+"/packages/container/user-pkg/versions", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list versions: %d %s", resp.StatusCode, b)
	}
	var versions []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&versions); err != nil {
		t.Fatalf("decode versions: %v", err)
	}
	resp.Body.Close()
	if len(versions) != 1 {
		t.Fatalf("expected 1 version, got %d", len(versions))
	}
	if versions[0]["name"] != "1.0.0" {
		t.Fatalf("expected version 1.0.0, got %v", versions[0]["name"])
	}

	resp = s.get(t, "/api/v3/users/"+admin.Login+"/packages/container/user-pkg/versions/"+strconv.Itoa(versionID), defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("get version: %d %s", resp.StatusCode, b)
	}
	version := decodeJSON(t, resp)
	if version["name"] != "1.0.0" {
		t.Fatalf("expected version name 1.0.0, got %v", version["name"])
	}

	resp = s.get(t, "/ui-data/users/"+admin.Login+"/packages/container/user-pkg/versions/"+strconv.Itoa(versionID)+"/files", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list files: %d %s", resp.StatusCode, b)
	}
	var files []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&files); err != nil {
		t.Fatalf("decode files: %v", err)
	}
	resp.Body.Close()
	if len(files) < 2 {
		t.Fatalf("expected registry manifest and layer files, got %d", len(files))
	}
	fileID := int(files[0]["id"].(float64))
	hasManifest := false
	hasLayer := false
	for _, file := range files {
		name, _ := file["name"].(string)
		switch {
		case name == "manifest.json":
			hasManifest = true
		case strings.HasPrefix(name, "blobs/sha256/"):
			hasLayer = true
		}
	}
	if !hasManifest || !hasLayer {
		t.Fatalf("expected registry manifest and layer files, got %v", files)
	}

	resp = s.get(t, "/ui-data/users/"+admin.Login+"/packages/container/user-pkg/versions/"+strconv.Itoa(versionID)+"/files/"+strconv.Itoa(fileID), defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("download file: %d %s", resp.StatusCode, b)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(data) == 0 {
		t.Fatal("expected package file bytes, got empty body")
	}

	resp = s.delete(t, "/api/v3/users/"+admin.Login+"/packages/container/user-pkg/versions/"+strconv.Itoa(versionID), defaultToken)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("delete version: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	resp = s.get(t, "/api/v3/users/"+admin.Login+"/packages/container/user-pkg/versions", defaultToken)
	versions = nil
	if err := json.NewDecoder(resp.Body).Decode(&versions); err != nil {
		t.Fatalf("decode versions after delete: %v", err)
	}
	resp.Body.Close()
	if len(versions) != 0 {
		t.Fatalf("expected 0 versions after delete, got %d", len(versions))
	}

	resp = s.post(t, "/api/v3/users/"+admin.Login+"/packages/container/user-pkg/versions/"+strconv.Itoa(versionID)+"/restore", defaultToken, nil)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("restore version: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	resp = s.get(t, "/api/v3/users/"+admin.Login+"/packages/container/user-pkg/versions", defaultToken)
	versions = nil
	if err := json.NewDecoder(resp.Body).Decode(&versions); err != nil {
		t.Fatalf("decode versions after restore: %v", err)
	}
	resp.Body.Close()
	if len(versions) != 1 {
		t.Fatalf("expected 1 version after restore, got %d", len(versions))
	}

	resp = s.delete(t, "/api/v3/users/"+admin.Login+"/packages/container/user-pkg", defaultToken)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("delete package: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	resp = s.get(t, "/api/v3/users/"+admin.Login+"/packages/container/user-pkg", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after package delete, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	_ = pkgID
}

func TestPackages_UserVersionsPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	_, byteStore := newObjectByteStoreForTest(t)
	s.store.ObjectByteStore = byteStore
	seedIsolatedPackageVersion(t, s, "user", "admin", "npm", "paged-user-pkg", "1.0.0")
	seedIsolatedPackageVersion(t, s, "user", "admin", "npm", "paged-user-pkg", "2.0.0")

	resp := tokenRequest(s, http.MethodGet, "/api/v3/user/packages/npm/paged-user-pkg/versions?per_page=1", defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 1: %d %s", resp.Code, resp.Body.String())
	}
	link := resp.Header().Get("Link")
	var page1 []map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &page1); err != nil {
		t.Fatal(err)
	}
	if len(page1) != 1 {
		t.Fatalf("page 1 len = %d, want 1", len(page1))
	}
	if !strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 1 Link = %q, want rel=next", link)
	}

	resp = tokenRequest(s, http.MethodGet, "/api/v3/user/packages/npm/paged-user-pkg/versions?per_page=1&page=2", defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 2: %d", resp.Code)
	}
	var page2 []map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &page2); err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 {
		t.Fatalf("page 2 len = %d, want 1", len(page2))
	}
	if page1[0]["id"] == page2[0]["id"] {
		t.Fatalf("page 1 and page 2 returned the same version: %v", page1[0]["id"])
	}
}

func TestPackages_OrgVersionsPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	_, byteStore := newObjectByteStoreForTest(t)
	s.store.ObjectByteStore = byteStore
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "pkg-page-org", "pkg-page-org", "")
	seedIsolatedPackageVersion(t, s, "org", org.Login, "npm", "paged-org-pkg", "1.0.0")
	seedIsolatedPackageVersion(t, s, "org", org.Login, "npm", "paged-org-pkg", "2.0.0")

	resp := tokenRequest(s, http.MethodGet, "/api/v3/orgs/"+org.Login+"/packages/npm/paged-org-pkg/versions?per_page=1", defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 1: %d %s", resp.Code, resp.Body.String())
	}
	link := resp.Header().Get("Link")
	var page1 []map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &page1); err != nil {
		t.Fatal(err)
	}
	if len(page1) != 1 {
		t.Fatalf("page 1 len = %d, want 1", len(page1))
	}
	if !strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 1 Link = %q, want rel=next", link)
	}

	resp = tokenRequest(s, http.MethodGet, "/api/v3/orgs/"+org.Login+"/packages/npm/paged-org-pkg/versions?per_page=1&page=2", defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 2: %d", resp.Code)
	}
	var page2 []map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &page2); err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 {
		t.Fatalf("page 2 len = %d, want 1", len(page2))
	}
	if page1[0]["id"] == page2[0]["id"] {
		t.Fatalf("page 1 and page 2 returned the same version: %v", page1[0]["id"])
	}
}

func seedIsolatedPackageVersion(t *testing.T, s *Server, ownerType, owner, pkgType, pkgName, version string) {
	t.Helper()
	resp := pagedJSONRequest(t, s, http.MethodPost, "/internal/packages/"+ownerType+"/"+owner+"/"+pkgType+"/"+pkgName+"/versions", defaultToken, map[string]any{
		"version":     version,
		"description": "test version",
		"metadata": map[string]any{
			"package_type": pkgType,
			"container":    map[string]any{"tags": []string{"latest"}},
		},
		"files": []map[string]any{
			{
				"name":           "package.tgz",
				"content_type":   "application/gzip",
				"content_base64": base64.StdEncoding.EncodeToString([]byte("hello package")),
			},
		},
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("seed package version: %d %s", resp.Code, resp.Body.String())
	}
}

func TestPackages_OrgCRUD(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "pkg-org", "Pkg Org", "")
	pkgID, versionID := s.seedPackageVersion(t, "org", org.Login, "npm", "org-pkg", "2.0.0")

	resp := s.get(t, "/api/v3/orgs/"+org.Login+"/packages?package_type=npm", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list org packages: %d %s", resp.StatusCode, b)
	}
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode org packages: %v", err)
	}
	resp.Body.Close()
	found := false
	for _, p := range list {
		if p["name"] == "org-pkg" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("seeded org-pkg not in list: %v", list)
	}

	resp = s.get(t, "/api/v3/orgs/"+org.Login+"/packages/npm/org-pkg", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("get org package: %d %s", resp.StatusCode, b)
	}
	pkg := decodeJSON(t, resp)
	if pkg["owner"].(map[string]any)["login"] != org.Login {
		t.Fatalf("expected org owner, got %v", pkg["owner"])
	}

	resp = s.delete(t, "/api/v3/orgs/"+org.Login+"/packages/npm/org-pkg/versions/"+strconv.Itoa(versionID), defaultToken)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("delete org version: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	resp = s.post(t, "/api/v3/orgs/"+org.Login+"/packages/npm/org-pkg/versions/"+strconv.Itoa(versionID)+"/restore", defaultToken, nil)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("restore org version: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	resp = s.delete(t, "/api/v3/orgs/"+org.Login+"/packages/npm/org-pkg", defaultToken)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("delete org package: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	_ = pkgID
}

func TestPackages_RepoCRUD(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "pkg-repo", "pkg repo", false)
	pkgID, versionID := s.seedPackageVersion(t, "repository", repo.FullName, "docker", "repo-pkg", "3.0.0")

	resp := s.get(t, "/ui-data/repos/"+repo.FullName+"/packages", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list repo packages: %d %s", resp.StatusCode, b)
	}
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode repo packages: %v", err)
	}
	resp.Body.Close()
	found := false
	for _, p := range list {
		if p["name"] == "repo-pkg" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("seeded repo-pkg not in list: %v", list)
	}

	resp = s.get(t, "/ui-data/repos/"+repo.FullName+"/packages/docker/repo-pkg", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("get repo package: %d %s", resp.StatusCode, b)
	}
	pkg := decodeJSON(t, resp)
	if pkg["repository"] == nil {
		t.Fatal("expected repository block")
	}

	resp = s.delete(t, "/ui-data/repos/"+repo.FullName+"/packages/docker/repo-pkg/versions/"+strconv.Itoa(versionID), defaultToken)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("delete repo version: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	resp = s.delete(t, "/ui-data/repos/"+repo.FullName+"/packages/docker/repo-pkg", defaultToken)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("delete repo package: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	_ = pkgID
}

func TestPackages_404s(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	_ = admin

	resp := s.get(t, "/api/v3/users/nonexistent-user-xyz/packages", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing user, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = s.get(t, "/api/v3/users/admin/packages/container/does-not-exist", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing package, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = s.get(t, "/api/v3/users/admin/packages/container/does-not-exist/versions/999999", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing version, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = s.get(t, "/api/v3/orgs/nonexistent-org-xyz/packages", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing org, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = s.get(t, "/ui-data/repos/nonexistent/repo/packages", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing repo, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = s.get(t, "/api/v3/users/admin/packages/invalid/foo", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for invalid package type, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPackages_RequiresAuth(t *testing.T) {
	resp := ghGet(t, "/api/v3/users/admin/packages", "")
	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 401 without token, got %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

func TestPackages_InternalUploadValidation(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]

	resp, _ := s.authedPost("/internal/packages/user/"+admin.Login+"/npm/bad-pkg/versions", "application/json", bytes.NewReader([]byte(`{}`)))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 422 missing version, got %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	// Container packages publish through the GitHub Container Registry-compatible
	// registry data plane, not the internal metadata upload route.
	resp, _ = s.authedPost("/internal/packages/user/"+admin.Login+"/container/bad-pkg/versions", "application/json", bytes.NewReader([]byte(`{"version":"1.0.0"}`)))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 422 container package internal upload, got %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	resp, _ = s.authedPost("/internal/packages/user/"+admin.Login+"/invalid/bad-pkg/versions", "application/json", bytes.NewReader([]byte(`{"version":"1.0.0"}`)))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 422 invalid package type, got %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	resp, _ = s.authedPost("/internal/packages/user/no-such-user/npm/bad-pkg/versions", "application/json", bytes.NewReader([]byte(`{"version":"1.0.0"}`)))
	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 404 missing owner, got %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

func TestPackages_OrgPackageRestore(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "pkg-restore-org", "Pkg Restore Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}
	_, versionID := s.seedPackageVersion(t, "org", org.Login, "npm", "restorable-pkg", "1.0.0")

	// Delete the whole package: gone from every read surface.
	resp := s.delete(t, "/api/v3/orgs/pkg-restore-org/packages/npm/restorable-pkg", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete package: %d, want 204", resp.StatusCode)
	}
	resp = s.get(t, "/api/v3/orgs/pkg-restore-org/packages/npm/restorable-pkg", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get deleted package: %d, want 404", resp.StatusCode)
	}

	// Restore brings it back with its versions intact.
	resp = s.post(t, "/api/v3/orgs/pkg-restore-org/packages/npm/restorable-pkg/restore", defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("restore package: %d, want 204", resp.StatusCode)
	}
	resp = s.get(t, "/api/v3/orgs/pkg-restore-org/packages/npm/restorable-pkg", defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("get restored package: %d", resp.StatusCode)
	}
	restored := decodeJSON(t, resp)
	if restored["version_count"] != float64(1) {
		t.Fatalf("restored package version_count = %v, want 1", restored["version_count"])
	}
	resp = s.get(t, "/api/v3/orgs/pkg-restore-org/packages/npm/restorable-pkg/versions/"+strconv.Itoa(versionID), defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get restored package version: %d", resp.StatusCode)
	}

	// A live package cannot be restored again; unknown names are not found.
	resp = s.post(t, "/api/v3/orgs/pkg-restore-org/packages/npm/restorable-pkg/restore", defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("restore live package: %d, want 404", resp.StatusCode)
	}
	resp = s.post(t, "/api/v3/orgs/pkg-restore-org/packages/npm/never-existed/restore", defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("restore unknown package: %d, want 404", resp.StatusCode)
	}

	// Reusing the namespace forfeits restore: deleting and republishing the
	// same name purges the old package for good.
	resp = s.delete(t, "/api/v3/orgs/pkg-restore-org/packages/npm/restorable-pkg", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("re-delete package: %d, want 204", resp.StatusCode)
	}
	s.seedPackageVersion(t, "org", org.Login, "npm", "restorable-pkg", "2.0.0")
	resp = s.get(t, "/api/v3/orgs/pkg-restore-org/packages/npm/restorable-pkg", defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("get republished package: %d", resp.StatusCode)
	}
	republished := decodeJSON(t, resp)
	if republished["version_count"] != float64(1) {
		t.Fatalf("republished package version_count = %v, want 1 (old versions purged)", republished["version_count"])
	}
}

func TestPackages_OrgDockerConflicts(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "docker-conflicts-org", "Docker Conflicts Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}

	// No packages → honestly empty.
	resp := s.get(t, "/api/v3/orgs/docker-conflicts-org/docker/conflicts", defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("docker conflicts (empty): %d", resp.StatusCode)
	}
	if conflicts := decodeJSONArray(t, resp); len(conflicts) != 0 {
		t.Fatalf("expected no conflicts, got %v", conflicts)
	}

	// A docker package alone does not conflict.
	s.seedPackageVersion(t, "org", org.Login, "docker", "shared-image", "1.0.0")
	resp = s.get(t, "/api/v3/orgs/docker-conflicts-org/docker/conflicts", defaultToken)
	if conflicts := decodeJSONArray(t, resp); len(conflicts) != 0 {
		t.Fatalf("expected no conflicts with docker package alone, got %v", conflicts)
	}

	// A container package with the same name makes the docker package a
	// migration conflict.
	s.publishContainerPackageVersion(t, org.Login, "shared-image", "1.0.0")
	resp = s.get(t, "/api/v3/orgs/docker-conflicts-org/docker/conflicts", defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("docker conflicts: %d", resp.StatusCode)
	}
	conflicts := decodeJSONArray(t, resp)
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %v", conflicts)
	}
	if conflicts[0]["name"] != "shared-image" || conflicts[0]["package_type"] != "docker" {
		t.Fatalf("conflict row wrong: %v", conflicts[0])
	}

	// Requires authentication.
	resp = s.get(t, "/api/v3/orgs/docker-conflicts-org/docker/conflicts", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated docker conflicts: %d, want 401", resp.StatusCode)
	}
}

// TestPackageVersionFilesServedOverGraphQL pins GQL-095: PackageVersion.files
// returns the version's stored files (name/size/updatedAt) instead of an empty
// connection.
func TestPackageVersionFilesServedOverGraphQL(t *testing.T) {
	s := newIsolatedServer(t)
	if _, ok := s.store.CreatePackage("User", "admin", "npm", "filed-pkg", "private"); !ok {
		t.Fatal("create package failed")
	}
	if _, err := s.store.CreatePackageVersion("User", "admin", "npm", "filed-pkg", "1.0.0", "desc", nil,
		[]store.PackageFileInput{{
			Name:          "filed-pkg-1.0.0.tgz",
			ContentType:   "application/gzip",
			ContentBase64: base64.StdEncoding.EncodeToString([]byte("hello")),
		}}); err != nil {
		t.Fatalf("create version: %v", err)
	}

	query := `{user(login:"admin"){packages(first:1){nodes{versions(first:1){nodes{files{nodes{name size updatedAt}}}}}}}}`
	resp := s.post(t, "/api/graphql", defaultToken, map[string]string{"query": query})
	data := decodeJSON(t, resp)
	if data["errors"] != nil {
		t.Fatalf("graphql errors: %v", data["errors"])
	}
	files := nestedNodes(t, data,
		"data", "user", "packages", "nodes", 0, "versions", "nodes", 0, "files", "nodes")
	if len(files) != 1 {
		t.Fatalf("files nodes = %d, want 1 (%v)", len(files), files)
	}
	file, _ := files[0].(map[string]interface{})
	if file["name"] != "filed-pkg-1.0.0.tgz" {
		t.Errorf("file.name = %#v, want filed-pkg-1.0.0.tgz", file["name"])
	}
	if sz, _ := file["size"].(float64); int(sz) != 5 {
		t.Errorf("file.size = %#v, want 5", file["size"])
	}
	if file["updatedAt"] == nil || file["updatedAt"] == "" {
		t.Errorf("file.updatedAt must be a non-null DateTime, got %#v", file["updatedAt"])
	}
}

// nestedNodes walks a decoded GraphQL response by a path of string keys and int
// slice indices, returning the []interface{} the final key names.
func nestedNodes(t *testing.T, m map[string]interface{}, path ...interface{}) []interface{} {
	t.Helper()
	var cur interface{} = m
	for _, step := range path {
		switch key := step.(type) {
		case string:
			obj, ok := cur.(map[string]interface{})
			if !ok {
				t.Fatalf("path %v: expected object at %q, got %T", path, key, cur)
			}
			cur = obj[key]
		case int:
			arr, ok := cur.([]interface{})
			if !ok {
				t.Fatalf("path %v: expected array at index %d, got %T", path, key, cur)
			}
			if key >= len(arr) {
				t.Fatalf("path %v: index %d out of range (len %d)", path, key, len(arr))
			}
			cur = arr[key]
		}
	}
	out, ok := cur.([]interface{})
	if !ok {
		t.Fatalf("path %v: expected final array, got %T", path, cur)
	}
	return out
}

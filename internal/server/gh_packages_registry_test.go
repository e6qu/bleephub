package bleephub

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func TestContainerRegistryPublishCreatesPackageVersion(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	requireStatus(t, s.registryRequest(t, http.MethodGet, "/v2/", nil), http.StatusOK)

	configDigest := s.uploadRegistryBlob(t, "admin/registry-image", []byte(`{"architecture":"amd64","os":"linux"}`))
	layerBytes := []byte("layer bytes")
	layerDigest := s.uploadRegistryBlob(t, "admin/registry-image", layerBytes)

	manifest := map[string]interface{}{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]interface{}{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    configDigest,
			"size":      len(`{"architecture":"amd64","os":"linux"}`),
		},
		"layers": []map[string]interface{}{
			{
				"mediaType": "application/vnd.oci.image.layer.v1.tar",
				"digest":    layerDigest,
				"size":      len(layerBytes),
			},
		},
	}
	manifestBytes := mustRegistryJSON(manifest)
	putManifest := s.registryRequest(t, http.MethodPut, "/v2/admin/registry-image/manifests/1.0.0", bytes.NewReader(manifestBytes))
	requireStatus(t, putManifest, http.StatusCreated)
	if got, want := putManifest.Header.Get("Docker-Content-Digest"), digestSHA256(manifestBytes); got != want {
		t.Fatalf("manifest digest = %q, want %q", got, want)
	}

	list := decodeJSONArray(t, s.get(t, "/api/v3/users/admin/packages?package_type=container", defaultToken))
	foundPackage := false
	for _, pkg := range list {
		if pkg["name"] == "registry-image" && pkg["package_type"] == "container" {
			foundPackage = true
			if pkg["version_count"].(float64) != 1 {
				t.Fatalf("version_count = %v, want 1", pkg["version_count"])
			}
		}
	}
	if !foundPackage {
		t.Fatalf("published package not listed: %#v", list)
	}

	versions := decodeJSONArray(t, s.get(t, "/api/v3/users/admin/packages/container/registry-image/versions", defaultToken))
	if len(versions) != 1 {
		t.Fatalf("versions len = %d, want 1: %#v", len(versions), versions)
	}
	if versions[0]["name"] != "1.0.0" {
		t.Fatalf("version name = %v, want 1.0.0", versions[0]["name"])
	}
	versionID := int(versions[0]["id"].(float64))
	files := decodeJSONArray(t, s.get(t, "/ui-data/users/admin/packages/container/registry-image/versions/"+itoa(versionID)+"/files", defaultToken))
	if len(files) != 3 {
		t.Fatalf("files len = %d, want manifest + config + layer: %#v", len(files), files)
	}

	getManifest := s.registryRequest(t, http.MethodGet, "/v2/admin/registry-image/manifests/1.0.0", nil)
	requireStatusNoClose(t, getManifest, http.StatusOK)
	gotManifest, _ := io.ReadAll(getManifest.Body)
	getManifest.Body.Close()
	if !bytes.Equal(gotManifest, manifestBytes) {
		t.Fatalf("registry manifest bytes = %q, want %q", string(gotManifest), string(manifestBytes))
	}

	getBlob := s.registryRequest(t, http.MethodGet, "/v2/admin/registry-image/blobs/"+layerDigest, nil)
	requireStatusNoClose(t, getBlob, http.StatusOK)
	gotLayer, _ := io.ReadAll(getBlob.Body)
	getBlob.Body.Close()
	if !bytes.Equal(gotLayer, layerBytes) {
		t.Fatalf("registry blob bytes = %q, want %q", string(gotLayer), string(layerBytes))
	}
}

func TestPackageAndRegistryBytesUseObjectStore(t *testing.T) {
	fs := newS3FSForTest(t)
	objectFS := deriveS3FSForTest(t, fs.Bucket(), "objects")
	s := newTestServer()
	s.store.ObjectByteStore = &store.S3ActionsByteStore{Fs: objectFS}
	admin := s.store.UsersByLogin["admin"]
	pkg, _ := s.store.CreatePackage("User", admin.Login, "container", "object-package", "public")
	version, err := s.store.CreatePackageVersion("User", admin.Login, "container", pkg.Name, "1.0.0", "", nil, []store.PackageFileInput{{
		Name:          "manifest.json",
		ContentType:   "application/vnd.oci.image.manifest.v1+json",
		ContentBase64: "cGFja2FnZSBvYmplY3QgYnl0ZXM=",
	}})
	if err != nil {
		t.Fatalf("create package version: %v", err)
	}
	files := s.store.ListPackageFiles(version.ID)
	if len(files) != 1 {
		t.Fatalf("package files len = %d, want 1", len(files))
	}
	got := readS3TestFile(t, objectFS, store.PackageFileDataKey(files[0].ID))
	if string(got) != "package object bytes" {
		t.Fatalf("package object bytes = %q", string(got))
	}
	data, contentType, ok := s.packageVersionFileData(version.ID, "manifest.json")
	if !ok || string(data) != "package object bytes" || contentType != "application/vnd.oci.image.manifest.v1+json" {
		t.Fatalf("package file data ok=%v contentType=%q data=%q", ok, contentType, string(data))
	}
	s.registerGHPackagesRoutes()
	s.registerUIAPIRoutes()
	req := httptest.NewRequest("GET", "/ui-data/users/admin/packages/container/object-package/versions/"+itoa(version.ID)+"/files/"+itoa(files[0].ID), nil)
	req.Header.Set("Authorization", "Bearer "+defaultToken)
	rec := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("download package object file status = %d, body = %s", rec.Code, rec.Body.String())
	}
	downloaded := rec.Body.Bytes()
	if string(downloaded) != "package object bytes" {
		t.Fatalf("downloaded package object bytes = %q", string(downloaded))
	}

	raw := []byte("registry object bytes")
	digest := digestSHA256(raw)
	sum := sha256.Sum256(raw)
	if err := s.writeRegistryBlobStream(digest, bytes.NewReader(raw), int64(len(raw)), sum[:]); err != nil {
		t.Fatalf("write registry blob: %v", err)
	}
	registryGot := readS3TestFile(t, objectFS, store.PackageRegistryBlobDataKey(digest))
	if string(registryGot) != "registry object bytes" {
		t.Fatalf("registry object bytes = %q", string(registryGot))
	}
	readBack, err := s.readRegistryBlob(digest)
	if err != nil {
		t.Fatalf("read registry blob: %v", err)
	}
	if string(readBack) != "registry object bytes" {
		t.Fatalf("registry read back = %q", string(readBack))
	}
}

func TestDeleteRepoPurgesRepositoryPackageObjectBytes(t *testing.T) {
	fs := newS3FSForTest(t)
	objectFS := deriveS3FSForTest(t, fs.Bucket(), "objects")
	s := newTestServer()
	s.store.ObjectByteStore = &store.S3ActionsByteStore{Fs: objectFS}
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "repo-package-objects", "", false)
	pkg, _ := s.store.CreatePackage("Repository", repo.FullName, "container", "image", "private")
	version, err := s.store.CreatePackageVersion("Repository", repo.FullName, "container", pkg.Name, "1.0.0", "", nil, []store.PackageFileInput{{
		Name:          "manifest.json",
		ContentType:   "application/vnd.oci.image.manifest.v1+json",
		ContentBase64: "cmVwbyBwYWNrYWdlIGJ5dGVz",
	}})
	if err != nil {
		t.Fatalf("create package version: %v", err)
	}
	files := s.store.ListPackageFiles(version.ID)
	if len(files) != 1 {
		t.Fatalf("package files len = %d, want 1", len(files))
	}
	if got := string(readS3TestFile(t, objectFS, files[0].StoragePath)); got != "repo package bytes" {
		t.Fatalf("package object bytes = %q", got)
	}

	deleted, err := s.store.DeleteRepo("admin", repo.Name)
	if err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	if !deleted {
		t.Fatal("delete repo returned false")
	}
	if s.store.GetPackage(repo.FullName, "container", "image") != nil {
		t.Fatal("repository-owned package metadata survived repository deletion")
	}
	f, err := objectFS.Open(files[0].StoragePath)
	if err == nil {
		_ = f.Close()
		t.Fatalf("repository package object %s survived repository deletion", files[0].StoragePath)
	}
}

func TestContainerRegistryRejectsDuplicateVersionName(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := "admin/registry-duplicate"
	layerDigest := s.uploadRegistryBlob(t, name, []byte("layer"))
	manifest := mustRegistryJSON(map[string]interface{}{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"layers": []map[string]interface{}{
			{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": layerDigest, "size": 5},
		},
	})
	requireStatus(t, s.registryRequest(t, http.MethodPut, "/v2/"+name+"/manifests/latest", bytes.NewReader(manifest)), http.StatusCreated)
	requireStatus(t, s.registryRequest(t, http.MethodPut, "/v2/"+name+"/manifests/latest", bytes.NewReader(manifest)), http.StatusConflict)
	versions := decodeJSONArray(t, s.get(t, "/api/v3/users/admin/packages/container/registry-duplicate/versions", defaultToken))
	if len(versions) != 1 {
		t.Fatalf("versions len = %d, want 1 after duplicate push", len(versions))
	}
}

func TestContainerRegistryRequiresAuthentication(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	req, err := http.NewRequest(http.MethodPut, s.baseURL+"/v2/admin/no-auth/manifests/latest", strings.NewReader(`{"schemaVersion":2}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, resp, http.StatusUnauthorized)
}

func requireStatusNoClose(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, want, body)
	}
}

func mustRegistryJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

package bleephub

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestCreatePackageVersionRequiresObjectStoreWhenPersistent covers STORE-042:
// with persistence enabled, package metadata replicates cluster-wide but a local
// version directory lives on one node only, so bytes must go to the shared
// object store. CreatePackageVersion was missing the guard its attestation and
// code-scanning siblings enforce, and would write bytes to local disk while the
// metadata replicated. A non-empty PackageDataDir makes this exercise the new
// guard rather than the pre-existing "storage not configured" one.
func TestCreatePackageVersionRequiresObjectStoreWhenPersistent(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	p, err := NewPersistence()
	if err != nil {
		t.Fatalf("persistence: %v", err)
	}
	if p == nil {
		t.Fatal("persistence not enabled")
	}
	t.Cleanup(func() { _ = p.Close() })
	st := NewStore()
	if err := st.SetPersistence(p); err != nil {
		t.Fatalf("set persistence: %v", err)
	}
	st.PackageDataDir = t.TempDir() // vdir != "" — the "not configured" guard cannot fire
	// st.ObjectByteStore stays nil.

	if _, created := st.CreatePackage("User", "octocat", "container", "app", "private"); !created {
		t.Fatal("package not created")
	}

	_, err = st.CreatePackageVersion("User", "octocat", "container", "app", "1.0.0", "", nil,
		[]PackageFileInput{{Name: "layer", ContentBase64: base64.StdEncoding.EncodeToString([]byte("bytes"))}})
	if err == nil {
		t.Fatal("STORE-042: CreatePackageVersion accepted package bytes with persistence enabled but no object store")
	}
	if !strings.Contains(err.Error(), "object byte store") {
		t.Fatalf("STORE-042: expected the object-byte-store guard, got a different error: %v", err)
	}
}

// TestRepoRenameRekeysWorkflowFiles covers STORE-029: a workflow file's ID (its
// map key and persistence key) is derived from the mutable repo full name, so a
// rename must re-key it. The rename cascade previously only rewrote the field,
// leaving the row keyed by the old-name hash; the next registration under the
// new name derived a different hash and inserted a duplicate.
func TestRepoRenameRekeysWorkflowFiles(t *testing.T) {
	s := newTestServer()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "wf-rename", "", false)
	oldFull := repo.FullName
	const path = ".github/workflows/ci.yml"

	wf := s.store.RegisterWorkflowFile(oldFull, path, "CI", "on: push", "test")
	if wf == nil {
		t.Fatal("workflow file not registered")
	}
	oldID := wf.ID

	if !s.store.RenameRepo(admin.Login, "wf-rename", "wf-renamed") {
		t.Fatal("rename failed")
	}
	newFull := admin.Login + "/wf-renamed"
	newID := stableWorkflowFileID(newFull, path)

	s.store.mu.RLock()
	_, oldExists := s.store.WorkflowFiles[oldID]
	moved, newExists := s.store.WorkflowFiles[newID]
	s.store.mu.RUnlock()

	if oldExists {
		t.Fatalf("STORE-029: stale workflow-file row still keyed by the old-name hash %d", oldID)
	}
	if !newExists || moved.RepoFullName != newFull {
		t.Fatalf("STORE-029: workflow file not re-keyed to the new name: %#v", moved)
	}

	// Re-registering under the new name must update in place, not duplicate.
	s.store.RegisterWorkflowFile(newFull, path, "CI", "on: push", "test")
	if files := s.store.ListWorkflowFiles(newFull); len(files) != 1 {
		t.Fatalf("STORE-029: re-registration duplicated the row: got %d files", len(files))
	}
}

package bleephub

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/e6qu/bleephub/internal/store"
)

// TestAnUpdateRaisesAnAlertForASecretItsCommitsAddedAndRemoved pins that the
// alerts a branch update raises cover every commit it brings, not only the tip.
// A secret one commit adds and a later commit in the same update deletes is in
// the repository's history for good, and scanning only the tip tree never saw
// it. The alert names the commit that introduced it.
func TestAnUpdateRaisesAnAlertForASecretItsCommitsAddedAndRemoved(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	if s.store.CreateRepo(admin, "ss-range-alerts", "", false) == nil {
		t.Fatal("create repo failed")
	}
	full := "admin/ss-range-alerts"

	base := s.gitDataCommitSHA(t, full, "start", s.gitDataTreeSHA(t, full, "README.md", "hello\n"))
	resp := s.post(t, "/api/v3/repos/"+full+"/git/refs", defaultToken, map[string]any{"ref": "refs/heads/main", "sha": base})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create main: %d", resp.StatusCode)
	}
	resp.Body.Close()
	alertsPath := "/api/v3/repos/" + full + "/secret-scanning/alerts?secret_type=aws_access_key_id"
	if alerts := decodeJSONArray(t, s.get(t, alertsPath, defaultToken)); len(alerts) != 0 {
		t.Fatalf("premise: a clean branch has %d alerts", len(alerts))
	}

	secret := "token=" + secretScanningSeedValue("aws_access_key_id") + "\n"
	added := s.gitDataCommitSHA(t, full, "add credentials", s.gitDataTreeSHA(t, full, "credentials.txt", secret), base)
	removed := s.gitDataCommitSHA(t, full, "remove credentials", s.gitDataTreeSHA(t, full, "README.md", "clean again\n"), added)
	resp = s.patch(t, "/api/v3/repos/"+full+"/git/refs/heads/main", defaultToken, map[string]any{"sha": removed})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update main: %d", resp.StatusCode)
	}
	resp.Body.Close()

	alerts := decodeJSONArray(t, s.get(t, alertsPath, defaultToken))
	if len(alerts) != 1 {
		t.Fatalf("an update whose middle commit added a secret raised %d alerts, want 1", len(alerts))
	}
	number := fmt.Sprint(alerts[0]["number"])
	locations := decodeJSONArray(t, s.get(t, "/api/v3/repos/"+full+"/secret-scanning/alerts/"+number+"/locations", defaultToken))
	if len(locations) != 1 {
		t.Fatalf("the alert has %d locations, want 1", len(locations))
	}
	details, _ := locations[0]["details"].(map[string]any)
	if details["commit_sha"] != added || details["path"] != "credentials.txt" {
		t.Fatalf("the alert is located at %v in %v, want credentials.txt in the commit that added it, %s", details["path"], details["commit_sha"], added)
	}
}

// blobCountingStorer counts the blobs read through it.
type blobCountingStorer struct {
	storer.Storer
	blobs int
}

func (c *blobCountingStorer) EncodedObject(kind plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) { //nolint:ireturn
	object, err := c.Storer.EncodedObject(kind, hash)
	if err == nil && object.Type() == plumbing.BlobObject {
		c.blobs++
	}
	return object, err
}

// TestAnUpdateReadsOnlyTheFilesItChanges pins what a branch update costs secret
// scanning. It used to read every blob in the new tip's tree, so a one-file push
// to a repository of twenty thousand files read twenty thousand blobs, and a
// push took most of a second; the cost must follow the change instead. A new
// branch still has its whole tip read, which is the premise.
func TestAnUpdateReadsOnlyTheFilesItChanges(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	stor := memory.NewStorage()
	files := map[string]string{}
	for i := range 200 {
		files[fmt.Sprintf("src/file-%03d.go", i)] = fmt.Sprintf("package src // %d\n", i)
	}
	first, err := initRepoWithFiles(stor, "main", "two hundred files", files, packReuseSignature(0))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	second, err := createFileCommit(stor, "main", "src/file-007.go", "package src // changed\n", "change one file", packReuseSignature(1))
	if err != nil {
		t.Fatalf("change one file: %v", err)
	}

	repo := &store.Repo{FullName: "admin/scanned"}
	counting := &blobCountingStorer{Storer: stor}
	if err := s.scanRefForSecretScanning(repo, counting, "refs/heads/main", plumbing.ZeroHash, first, "http://bleephub.invalid"); err != nil {
		t.Fatalf("scan a new branch: %v", err)
	}
	if counting.blobs < 200 {
		t.Fatalf("premise: scanning a new branch read %d blobs, want all 200", counting.blobs)
	}

	counting.blobs = 0
	if err := s.scanRefForSecretScanning(repo, counting, "refs/heads/main", first, second, "http://bleephub.invalid"); err != nil {
		t.Fatalf("scan an update: %v", err)
	}
	if counting.blobs != 1 {
		t.Fatalf("an update changing one file read %d blobs, want 1", counting.blobs)
	}
}

// TestPushProtectionReadsNothingWhenNothingCouldBlock pins that push protection
// costs nothing where it is not turned on, which is the default. A match blocks
// only through a pattern push protection is enabled for, and the scan used to
// read every blob a push brought — a repository's whole history on its first
// push — and then discard what it found.
func TestPushProtectionReadsNothingWhenNothingCouldBlock(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	stor := memory.NewStorage()
	files := map[string]string{"credentials.txt": "token=" + secretScanningSeedValue("aws_access_key_id") + "\n"}
	for i := range 50 {
		files[fmt.Sprintf("src/file-%03d.go", i)] = fmt.Sprintf("package src // %d\n", i)
	}
	tip, err := initRepoWithFiles(stor, "main", "history with a secret", files, packReuseSignature(0))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	repo := &store.Repo{FullName: "admin/unprotected", SecretScanningPushProtectionEnabled: false}
	counting := &blobCountingStorer{Storer: stor}
	placeholder, err := s.secretScanningPushProtectionPlaceholderForRef(repo, counting, "refs/heads/main", plumbing.ZeroHash, tip)
	if err != nil || placeholder != nil {
		t.Fatalf("a repository without push protection was blocked (%v, %v)", placeholder, err)
	}
	if counting.blobs != 0 {
		t.Fatalf("push protection read %d blobs where nothing could block", counting.blobs)
	}

	repo.SecretScanningPushProtectionEnabled = true
	placeholder, err = s.secretScanningPushProtectionPlaceholderForRef(repo, counting, "refs/heads/main", plumbing.ZeroHash, tip)
	if err != nil || placeholder == nil {
		t.Fatalf("premise: with push protection on, the secret did not block (%v, %v)", placeholder, err)
	}
	if counting.blobs == 0 {
		t.Fatal("premise: with push protection on, nothing was read")
	}
}

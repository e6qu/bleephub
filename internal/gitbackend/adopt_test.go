package gitbackend

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/gitstore"
	"github.com/e6qu/bleephub/gitstore/objstore"
	"github.com/e6qu/bleephub/internal/server/testutil"
	"github.com/go-git/go-git/v5/plumbing"
)

// TestARepositoryWithoutAManifestIsRefusedUntilItIsAdopted protects an upgrade.
// A deployment whose bucket was written before repositories had manifests holds
// references as objects, which the engine no longer reads. A server that took
// such a repository for an empty one would initialize over it, and its branches
// would be gone from every clone; so a restart refuses it, by name, until the
// operator has run `bleephub adopt` — once, on every driver the same.
func TestARepositoryWithoutAManifestIsRefusedUntilItIsAdopted(t *testing.T) {
	for _, driver := range testutil.ObjectStoreDrivers {
		t.Run(driver, func(t *testing.T) {
			clearTunables(t)
			testutil.ConfigureFakeObjectStore(t, driver, "bleephub-test")
			t.Setenv("BLEEPHUB_GIT_BUCKET", "bleephub-test")
			t.Setenv("BLEEPHUB_GIT_PREFIX", "git")
			t.Setenv("BLEEPHUB_GITSTORE_CACHE_DIR", t.TempDir())
			forgetOpenedStore(t)
			ctx := context.Background()
			store, err := GetStore(ctx)
			if err != nil {
				t.Fatalf("open: %v", err)
			}

			// The old layout, by hand: a branch and HEAD as objects of their own.
			tip := plumbing.NewHash("1111111111111111111111111111111111111111")
			for key, content := range map[string]string{
				"git/owner/old/HEAD":            "ref: refs/heads/main\n",
				"git/owner/old/refs/heads/main": tip.String() + "\n",
			} {
				if _, err := store.Bucket().Put(ctx, key, strings.NewReader(content), int64(len(content)), objstore.Always, nil); err != nil {
					t.Fatalf("write %s: %v", key, err)
				}
			}

			if _, err := OpenExistingGitStorage(ctx, "owner/old"); !errors.Is(err, gitstore.ErrNoManifest) {
				t.Fatalf("a repository in the old layout opened with %v, want ErrNoManifest", err)
			}

			var out bytes.Buffer
			if err := Adopt(ctx, AdoptRequest{}, &out); err != nil {
				t.Fatalf("adopt: %v", err)
			}
			if !strings.Contains(out.String(), "owner/old: manifest written with 0 live packs, 0 retired packs and 2 references") {
				t.Fatalf("adopt reported %q", out.String())
			}
			forgetOpenedStore(t)
			adopted, err := OpenExistingGitStorage(ctx, "owner/old")
			if err != nil {
				t.Fatalf("after adoption: %v", err)
			}
			if ref, err := adopted.Reference("refs/heads/main"); err != nil || ref.Hash() != tip {
				t.Fatalf("the adopted branch is %v (%v), want %s", ref, err, tip)
			}

			out.Reset()
			if err := Adopt(ctx, AdoptRequest{Repository: "owner/old"}, &out); err != nil || !strings.Contains(out.String(), "refused") {
				t.Fatalf("adopting twice: %v, %q; want the second refused and reported", err, out.String())
			}
			out.Reset()
			if err := Adopt(ctx, AdoptRequest{RemoveOldLayout: true}, &out); err != nil || !strings.Contains(out.String(), "owner/old: removed 2 keys") {
				t.Fatalf("removing the old layout: %v, %q", err, out.String())
			}
			if _, err := store.Bucket().Head(ctx, "git/owner/old/refs/heads/main"); !errors.Is(err, objstore.ErrNotFound) {
				t.Fatalf("the old branch object is still there: %v", err)
			}
		})
	}
}

// TestAdoptNeedsAnObjectStore protects the operator from a command that does
// nothing and says nothing: without a git bucket there is no store to adopt.
func TestAdoptNeedsAnObjectStore(t *testing.T) {
	clearTunables(t)
	t.Setenv("BLEEPHUB_GIT_BUCKET", "")
	t.Setenv("BLEEPHUB_GIT_DIR", t.TempDir())
	forgetOpenedStore(t)
	var out bytes.Buffer
	if err := Adopt(context.Background(), AdoptRequest{}, &out); err == nil || !strings.Contains(err.Error(), "BLEEPHUB_GIT_BUCKET") {
		t.Fatalf("adopt without a bucket answered %v", err)
	}
	if stor, err := OpenExistingGitStorage(context.Background(), "owner/repo"); err != nil || stor == nil {
		t.Fatalf("a directory repository did not open: %v", err)
	}
}

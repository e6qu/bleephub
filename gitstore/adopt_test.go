package gitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/storage/memory"
)

// oldLayoutPack puts a pack in the fake as the engine before the manifest left
// one: a .pack, a .idx and a .bfilter under objects/pack/, and nothing else.
func oldLayoutPack(t *testing.T, fake *fakeS3, repo string, objects int, seed string) (string, []plumbing.Hash) {
	t.Helper()
	client := memory.NewStorage()
	var hashes []plumbing.Hash
	for i := range objects {
		hashes = append(hashes, storeBlob(t, client, fmt.Sprintf("%s object %d", seed, i)))
	}
	var pack bytes.Buffer
	if _, err := packfile.NewEncoder(&pack, client, false).Encode(hashes, gitPackWindow); err != nil {
		t.Fatalf("encode: %v", err)
	}
	// A scratch repository of the engine's own builds the index and the filter;
	// its keys are then moved to where the old layout kept them.
	scratch, err := fake.store("scratch").Repository("scratch/" + seed)
	if err != nil {
		t.Fatalf("scratch: %v", err)
	}
	if err := packfile.UpdateObjectStorage(scratch, &pack); err != nil {
		t.Fatalf("scratch push: %v", err)
	}
	var name string
	for _, key := range fake.KeysWithPrefix("scratch/scratch/" + seed + "/objects/pack/") {
		data, _ := fake.Get(key)
		base := key[strings.LastIndex(key, "/")+1:]
		fake.Put("prefix/"+repo+"/objects/pack/"+base, data)
		if trimmed, isPack := strings.CutSuffix(base, ".pack"); isPack {
			name = trimmed
		}
	}
	return name, hashes
}

// TestAdoptWritesTheManifestTheOldLayoutImplied is the upgrade. A bucket written
// before the manifest says which packs are live by which keys lie in it — a
// .pack beside its .idx and no .superseded marker — and holds every reference as
// an object, with git's precedence over packed-refs. Adopt must read exactly
// that, once, into a manifest: a pack wrongly taken for live would serve objects
// twice, one wrongly dropped would lose them, and a reference resolved with the
// wrong precedence would move a branch. It must refuse a repository that has a
// manifest, and leave the old keys alone until told to remove them.
func TestAdoptWritesTheManifestTheOldLayoutImplied(t *testing.T) {
	fake := newFakeS3(t)
	fake.clock = newTestClock()
	const repo = "octocat/upgraded"
	prefix := "prefix/" + repo + "/"
	store := fake.store("prefix")

	live, liveObjects := oldLayoutPack(t, fake, repo, 12, "live")
	superseded, _ := oldLayoutPack(t, fake, repo, 3, "superseded")
	unfinished, unfinishedObjects := oldLayoutPack(t, fake, repo, 2, "unfinished")
	fake.Remove(prefix + "objects/pack/" + unfinished + ".idx")
	markerTime := fake.clock.Now()
	fake.Put(prefix+"objects/pack/"+superseded+".superseded", []byte(live))
	fake.clock.Advance(10 * time.Minute)

	tip, packedOnly, shadowed := liveObjects[0], liveObjects[1], liveObjects[2]
	for key, content := range map[string]string{
		"HEAD":                    "ref: refs/heads/main\n",
		"refs/heads/main":         tip.String() + "\n",
		"refs/heads/team/topic":   tip.String() + "\n",
		"refs/tags/v1":            tip.String() + "\n",
		"refs/heads/../../escape": tip.String() + "\n",
		"packed-refs": "# pack-refs with: peeled fully-peeled sorted \n" +
			packedOnly.String() + " refs/heads/packed-only\n" +
			shadowed.String() + " refs/heads/main\n" +
			packedOnly.String() + " refs/tags/annotated\n^" + tip.String() + "\n",
		"config":                         "[core]\n\tbare = true\n",
		"modules/lib/HEAD":               "ref: refs/heads/trunk\n",
		"modules/lib/refs/heads/trunk":   packedOnly.String() + "\n",
		"objects/bundle/bundle-1.bundle": "an auxiliary object",
		"objects/" + looseKeySuffix(tip): "a loose object is left where it is",
	} {
		fake.Put(prefix+key, []byte(content))
	}
	before := fake.KeysWithPrefix(prefix)

	if _, err := store.ExistingRepository(repo); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("premise: the old layout opened with %v, want ErrNoManifest", err)
	}
	reports, err := store.Adopt(context.Background(), repo)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if len(reports) != 2 || reports[0].Repository != repo || reports[1].Repository != repo+"/modules/lib" {
		t.Fatalf("adopt reported %+v, want the repository and its submodule", reports)
	}
	if got := reports[0]; got.Packs != 1 || got.Retired != 1 || got.References != 6 || got.Sequence != 1 {
		t.Fatalf("adopt reported %+v, want 1 live pack, 1 retired, 6 references at sequence 1", got)
	}

	data, _ := fake.Get(prefix + manifestName)
	adopted, err := decodeManifest(data)
	if err != nil {
		t.Fatalf("the adopted manifest: %v", err)
	}
	if len(adopted.Packs) != 1 || adopted.Packs[0].Name != live || adopted.Packs[0].Objects != len(liveObjects) || adopted.Packs[0].Source != packSourceAdoption || adopted.Packs[0].FilterBytes == 0 {
		t.Fatalf("the adopted live packs are %+v, want %s with %d objects", adopted.Packs, live, len(liveObjects))
	}
	if len(adopted.Retired) != 1 || adopted.Retired[0].Name != superseded || !adopted.Retired[0].Retired.Equal(markerTime) || adopted.Retired[0].Orphan {
		t.Fatalf("the adopted retired packs are %+v, want %s retired at its marker's time %s", adopted.Retired, superseded, markerTime)
	}

	stor, err := store.ExistingRepository(repo)
	if err != nil {
		t.Fatalf("open after adoption: %v", err)
	}
	sameReferences(t, "after adoption", advertise(t, stor), map[string]string{
		"refs/heads/main": tip.String(), "refs/heads/team/topic": tip.String(), "refs/tags/v1": tip.String(),
		"refs/heads/packed-only": packedOnly.String(), "refs/tags/annotated": packedOnly.String(),
	})
	if head, err := stor.Reference(plumbing.HEAD); err != nil || head.Target() != testBranch {
		t.Fatalf("the adopted HEAD is %v (%v)", head, err)
	}
	for _, hash := range liveObjects {
		if err := stor.HasEncodedObject(hash); err != nil {
			t.Fatalf("an object of the live pack is missing after adoption: %v", err)
		}
	}
	if err := stor.HasEncodedObject(unfinishedObjects[0]); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("a pack without an index was adopted: %v", err)
	}
	module, err := stor.Module("lib")
	if err != nil {
		t.Fatalf("module: %v", err)
	}
	resolvesTo(t, "the adopted submodule", module, "refs/heads/trunk", packedOnly)

	// Nothing of the old layout was touched, and a second adoption is refused.
	after := fake.KeysWithPrefix(prefix)
	if len(after) != len(before)+2 {
		t.Fatalf("adoption left %d keys where there were %d, want the two manifests added and nothing else", len(after), len(before))
	}
	if _, err := store.Adopt(context.Background(), repo); !errors.Is(err, ErrAlreadyAdopted) {
		t.Fatalf("adopting twice answered %v, want ErrAlreadyAdopted", err)
	}
	if again, _ := fake.Get(prefix + manifestName); !bytes.Equal(again, data) {
		t.Fatal("the refused adoption rewrote the manifest")
	}

	// The second, explicit step removes what only the old layout used.
	if _, err := store.RemoveAdoptedLayout(context.Background(), "octocat/never-adopted"); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("removing the old layout of a repository without a manifest answered %v, want ErrNoManifest", err)
	}
	removed, err := store.RemoveAdoptedLayout(context.Background(), repo)
	if err != nil || removed != 9 {
		t.Fatalf("removed %d keys (%v), want HEAD, four references, packed-refs, the marker and the submodule's two", removed, err)
	}
	for _, key := range fake.KeysWithPrefix(prefix) {
		relative := strings.TrimPrefix(key, prefix)
		if relative == "HEAD" || relative == "packed-refs" || strings.HasPrefix(relative, "refs/") || strings.HasSuffix(relative, ".superseded") || strings.HasPrefix(relative, "modules/lib/refs/") {
			t.Fatalf("the old layout's %s is still there", relative)
		}
	}
	resolvesTo(t, "after the old layout was removed", testPackedStorageNamed(t, fake, repo), testBranch, tip)
}

// looseKeySuffix is a loose object's key below objects/.
func looseKeySuffix(hash plumbing.Hash) string {
	return hash.String()[:2] + "/" + hash.String()[2:]
}

func testPackedStorageNamed(t *testing.T, fake *fakeS3, repo string) *repository {
	t.Helper()
	stor, err := packedStorage(fake, repo)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	return stor
}

// TestAdoptFoldsManyReferencesAndFindsEveryRepository covers a store of several
// repositories, one with more references than a manifest carries inline: the
// listing of repositories must find each, and the adopted references must land
// in a snapshot written before the manifest that names it.
func TestAdoptFoldsManyReferencesAndFindsEveryRepository(t *testing.T) {
	fake := newFakeS3(t)
	store := fake.store("prefix")
	const many = refChangeBound + 100
	for i := range many {
		fake.Put(fmt.Sprintf("prefix/octocat/many/refs/tags/v%04d", i), []byte(hashOf(i).String()+"\n"))
	}
	fake.Put("prefix/octocat/many/HEAD", []byte("ref: refs/heads/main\n"))
	fake.Put("prefix/hubot/few/HEAD", []byte("ref: refs/heads/main\n"))
	fake.Put("prefix/.conformance/conformance-1", []byte("not a repository"))

	names, err := store.Repositories(context.Background())
	if err != nil || strings.Join(names, ",") != "hubot/few,octocat/many" {
		t.Fatalf("the store's repositories are %v (%v)", names, err)
	}
	reports, err := store.Adopt(context.Background(), "octocat/many")
	if err != nil || len(reports) != 1 || reports[0].References != many+1 || reports[0].Snapshot == "" {
		t.Fatalf("adopt reported %+v (%v), want %d references folded into a snapshot", reports, err, many+1)
	}
	if _, ok := fake.Get("prefix/octocat/many/" + reports[0].Snapshot); !ok {
		t.Fatal("the manifest names a snapshot that is not in the store")
	}
	stor := testPackedStorageNamed(t, fake, "octocat/many")
	if refs := advertise(t, stor); len(refs) != many+1 {
		t.Fatalf("the adopted repository advertises %d references, want %d", len(refs), many+1)
	}
}

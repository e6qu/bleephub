package gitstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/storer"
	gitStorage "github.com/go-git/go-git/v5/storage"
	gitFilesystem "github.com/go-git/go-git/v5/storage/filesystem"

	"github.com/e6qu/bleephub/gitstore/objstore"
)

// uploadDirectory puts a directory's files in the bucket under prefix, keyed by
// their paths: what copying a repository from a disk into a bucket does.
func uploadDirectory(t *testing.T, fake *fakeS3, dir, prefix string) {
	t.Helper()
	if err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(name) // #nosec G304,G122 -- a test's own temporary directory
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(dir, name)
		if err != nil {
			return err
		}
		fake.Put(prefix+filepath.ToSlash(relative), body)
		return nil
	}); err != nil {
		t.Fatalf("upload %s: %v", dir, err)
	}
}

// downloadPrefix writes every key under prefix to dir, keyed paths as file paths.
func downloadPrefix(t *testing.T, fake *fakeS3, prefix, dir string) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	defer func() { _ = root.Close() }()
	for _, key := range fake.KeysWithPrefix(prefix) {
		body, _ := fake.Get(key)
		relative := filepath.FromSlash(strings.TrimPrefix(key, prefix))
		if err := root.MkdirAll(filepath.Dir(relative), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", key, err)
		}
		if err := root.WriteFile(relative, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", key, err)
		}
	}
}

// TestTheBucketHoldsGitsOwnObjects pins what keeps a bucket's bulk portable: a
// pack is git's own file under git's own name, and the head of its sidecar is
// git's own index of it, byte for byte. A repository written by go-git's own
// storage onto a disk — loose objects, a pack, loose and packed references, a
// symbolic HEAD — reads correctly once its files are keys and Adopt has given
// it a manifest, its loose objects packed; and the objects of a repository
// written through the engine read correctly through go-git's own storage once
// its packs are files and each sidecar's index is cut out as the pack's .idx.
// The references are the manifest's and travel in no other form.
func TestTheBucketHoldsGitsOwnObjects(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1

	// From a disk into the bucket.
	dir := t.TempDir()
	onDisk := gitFilesystem.NewStorage(osfs.New(dir), cache.NewObjectLRUDefault())
	pack, packed := pushPack(t, 30)
	if err := packfile.UpdateObjectStorage(onDisk, bytes.NewReader(pack)); err != nil {
		t.Fatalf("pack onto disk: %v", err)
	}
	loose := storeBlob(t, onDisk, "a loose object written by git")
	tip := packed[len(packed)-1]
	for name, hash := range map[plumbing.ReferenceName]plumbing.Hash{"refs/heads/packed": tip, "refs/tags/v1": tip} {
		if err := onDisk.SetReference(plumbing.NewHashReference(name, hash)); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	if err := onDisk.PackRefs(); err != nil {
		t.Fatalf("pack refs: %v", err)
	}
	if err := onDisk.SetReference(plumbing.NewHashReference(testBranch, tip)); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := onDisk.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, testBranch)); err != nil {
		t.Fatalf("set HEAD: %v", err)
	}
	cfg := config.NewConfig()
	cfg.Core.IsBare = true
	if err := onDisk.SetConfig(cfg); err != nil {
		t.Fatalf("set config: %v", err)
	}
	uploadDirectory(t, fake, dir, "prefix/"+testRepo+"/")
	if len(packKeys(fake, ".pack")) != 1 || len(looseLayoutKeys(fake)) != 1 {
		t.Fatalf("premise: the disk repository holds %d packs and %d loose objects, want one of each", len(packKeys(fake, ".pack")), len(looseLayoutKeys(fake)))
	}
	if reports, err := fake.store("prefix").Adopt(context.Background(), testRepo); err != nil || len(reports) != 1 || reports[0].Loose != 1 {
		t.Fatalf("adopt: %+v, %v; want the one loose object packed", reports, err)
	}

	fromDisk := testPackedStorage(t, fake)
	want := map[string]string{"refs/heads/main": tip.String(), "refs/heads/packed": tip.String(), "refs/tags/v1": tip.String()}
	sameReferences(t, "a repository written by git", advertise(t, fromDisk), want)
	if head, err := storer.ResolveReference(fromDisk, plumbing.HEAD); err != nil || head.Hash() != tip {
		t.Fatalf("HEAD resolves to %v (%v), want %s", head, err, tip)
	}
	if got := readObjects(t, fromDisk, []plumbing.Hash{loose})[loose]; got != "a loose object written by git\x00blob" {
		t.Fatalf("the loose object read back %q", got)
	}
	readObjects(t, fromDisk, packed)
	if read, err := fromDisk.Config(); err != nil || !read.Core.IsBare {
		t.Fatalf("config: %+v, %v", read, err)
	}

	// From the bucket onto a disk.
	const written = "octocat/written-here"
	stor, err := fake.store("prefix").Repository(written)
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	if err := Init(stor); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(pack)); err != nil {
		t.Fatalf("push: %v", err)
	}
	mine := storeBlob(t, stor, "an object written by the engine")
	if err := FlushObjects(stor); err != nil {
		t.Fatalf("flush: %v", err)
	}
	out := t.TempDir()
	downloadPrefix(t, fake, "prefix/"+written+"/", out)
	// git reads a pack by its .idx: the head of the sidecar, as long as the
	// sidecar's footer says.
	sidecars := fake.KeysWithPrefix("prefix/" + written + "/objects/pack/")
	cut := 0
	for _, key := range sidecars {
		name, isSidecar := strings.CutSuffix(key, sidecarSuffix)
		if !isSidecar {
			continue
		}
		sidecar, _ := fake.Get(key)
		footer, err := decodeSidecarFooter(sidecar[len(sidecar)-sidecarFooterSize:])
		if err != nil {
			t.Fatalf("sidecar %s: %v", key, err)
		}
		index := filepath.Join(out, filepath.FromSlash(strings.TrimPrefix(name, "prefix/"+written+"/")+".idx"))
		if err := os.WriteFile(index, sidecar[:footer.indexBytes], 0o600); err != nil {
			t.Fatalf("write %s: %v", index, err)
		}
		cut++
	}
	if cut != 2 {
		t.Fatalf("premise: the engine wrote %d sidecars, want the push's and the flush's", cut)
	}
	reopened := gitFilesystem.NewStorage(osfs.New(out), cache.NewObjectLRUDefault())
	for _, hash := range append([]plumbing.Hash{mine}, packed...) {
		if _, err := reopened.EncodedObject(plumbing.AnyObject, hash); err != nil {
			t.Fatalf("git cannot read %s out of what the engine wrote: %v", hash, err)
		}
	}
}

// TestObjectsAreFoundInEitherTierByTypeAndSize covers the object reads a server
// makes beside a plain fetch: by type, where the wrong type is "not found"; by
// size, without the content; and walking every object once, however many
// places hold it — what is pending and a pack on the handle that wrote it, two
// packs once it is flushed.
func TestObjectsAreFoundInEitherTierByTypeAndSize(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	stor := testPackedStorage(t, fake)
	pack, packed := pushPack(t, 40)
	if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(pack)); err != nil {
		t.Fatalf("push: %v", err)
	}
	// One of the packed objects is written again, so two places hold it.
	pendingBody := string(blobBody(0))
	if got := storeBlob(t, stor, pendingBody); got != packed[0] {
		t.Fatalf("premise: the written copy hashed to %s, the packed one to %s", got, packed[0])
	}
	onlyWritten := storeBlob(t, stor, "only written, never pushed")
	all := append([]plumbing.Hash{onlyWritten}, packed...)

	check := func(who string, handle *repository) {
		t.Helper()
		if _, err := handle.EncodedObject(plumbing.BlobObject, packed[0]); err != nil {
			t.Fatalf("%s: a blob asked for as a blob: %v", who, err)
		}
		if _, err := handle.EncodedObject(plumbing.CommitObject, packed[0]); !errors.Is(err, plumbing.ErrObjectNotFound) {
			t.Fatalf("%s: a blob asked for as a commit: %v", who, err)
		}
		if _, err := handle.EncodedObject(plumbing.CommitObject, packed[len(packed)-1]); err != nil {
			t.Fatalf("%s: the commit asked for as a commit: %v", who, err)
		}
		if _, err := handle.EncodedObject(plumbing.CommitObject, onlyWritten); !errors.Is(err, plumbing.ErrObjectNotFound) {
			t.Fatalf("%s: a written blob asked for as a commit: %v", who, err)
		}
		for hash, wantSize := range map[plumbing.Hash]int{packed[0]: len(pendingBody), onlyWritten: len("only written, never pushed")} {
			if size, err := handle.EncodedObjectSize(hash); err != nil || size != int64(wantSize) {
				t.Fatalf("%s: size of %s: %d, %v; want %d", who, hash, size, err, wantSize)
			}
		}
		if _, err := handle.EncodedObjectSize(absentHash(5)); !errors.Is(err, plumbing.ErrObjectNotFound) {
			t.Fatalf("%s: size of an absent object: %v", who, err)
		}

		iter, err := handle.IterEncodedObjects(plumbing.AnyObject)
		if err != nil {
			t.Fatalf("%s: walk: %v", who, err)
		}
		seen := map[plumbing.Hash]int{}
		if err := iter.ForEach(func(object plumbing.EncodedObject) error {
			seen[object.Hash()]++
			return nil
		}); err != nil {
			t.Fatalf("%s: walk: %v", who, err)
		}
		for _, hash := range all {
			if seen[hash] != 1 {
				t.Fatalf("%s: the walk met %s %d times, want once", who, hash, seen[hash])
			}
		}
		if len(seen) != len(all) {
			t.Fatalf("%s: the walk met %d objects, want %d", who, len(seen), len(all))
		}

		blobs, err := handle.IterEncodedObjects(plumbing.CommitObject)
		if err != nil {
			t.Fatalf("%s: walk commits: %v", who, err)
		}
		commits := 0
		for {
			object, err := blobs.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%s: walk commits: %v", who, err)
			}
			if object.Type() != plumbing.CommitObject {
				t.Fatalf("%s: a walk of commits met a %s", who, object.Type())
			}
			commits++
		}
		blobs.Close()
		if commits != 1 {
			t.Fatalf("%s: the walk met %d commits, want 1", who, commits)
		}
	}

	check("the writing handle, with two objects pending", stor)
	if err := stor.FlushObjects(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if live := len(storedManifest(t, fake).Packs); live != 2 {
		t.Fatalf("premise: the push and the flush left %d packs, want 2", live)
	}
	check("another replica, after the flush", testPackedStorage(t, fake))

	delta := &plumbing.MemoryObject{}
	delta.SetType(plumbing.OFSDeltaObject)
	if _, err := stor.SetEncodedObject(delta); !errors.Is(err, plumbing.ErrInvalidType) {
		t.Fatalf("storing a delta as an object: %v", err)
	}
	if err := stor.AddAlternate("/somewhere/else"); err == nil {
		t.Fatal("an object-store repository accepted an alternate")
	}
}

// TestTheSmallFilesRoundTrip covers what go-git keeps beside objects and
// references — the config, the index, the shallow list — and that each reads as
// empty, not as an error, in a repository that has never had one.
func TestTheSmallFilesRoundTrip(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)

	if cfg, err := stor.Config(); err != nil || len(cfg.Remotes) != 0 {
		t.Fatalf("the config of a new repository: %+v, %v", cfg, err)
	}
	if idx, err := stor.Index(); err != nil || idx.Version != 2 || len(idx.Entries) != 0 {
		t.Fatalf("the index of a new repository: %+v, %v", idx, err)
	}
	if shallow, err := stor.Shallow(); err != nil || shallow != nil {
		t.Fatalf("the shallow list of a new repository: %v, %v", shallow, err)
	}

	cfg := config.NewConfig()
	cfg.Remotes["origin"] = &config.RemoteConfig{Name: "origin", URLs: []string{"https://example.invalid/repo.git"}}
	if err := stor.SetConfig(cfg); err != nil {
		t.Fatalf("set config: %v", err)
	}
	if err := stor.SetConfig(&config.Config{Remotes: map[string]*config.RemoteConfig{"bad": {Name: ""}}}); err == nil {
		t.Fatal("an invalid config was stored")
	}
	if err := stor.SetIndex(&index.Index{Version: 2, Entries: []*index.Entry{{Name: "file.txt", Hash: hashOf(1)}}}); err != nil {
		t.Fatalf("set index: %v", err)
	}
	if err := stor.SetShallow([]plumbing.Hash{hashOf(1), hashOf(2)}); err != nil {
		t.Fatalf("set shallow: %v", err)
	}

	reader := testPackedStorage(t, fake)
	if got, err := reader.Config(); err != nil || got.Remotes["origin"] == nil || got.Remotes["origin"].URLs[0] != "https://example.invalid/repo.git" {
		t.Fatalf("config read back: %+v, %v", got, err)
	}
	if got, err := reader.Index(); err != nil || len(got.Entries) != 1 || got.Entries[0].Name != "file.txt" {
		t.Fatalf("index read back: %+v, %v", got, err)
	}
	if got, err := reader.Shallow(); err != nil || len(got) != 2 || got[0] != hashOf(1) || got[1] != hashOf(2) {
		t.Fatalf("shallow list read back: %v, %v", got, err)
	}
}

// TestASubmoduleIsARepositoryOfItsOwn covers Module: the submodule's data lives
// under modules/<name>/ as git keeps it, apart from its parent's, behind a
// handle that is the same one each time it is asked for.
func TestASubmoduleIsARepositoryOfItsOwn(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	module, err := stor.Module("vendor/lib")
	if err != nil {
		t.Fatalf("module: %v", err)
	}
	if again, _ := stor.Module("vendor/lib"); again != module {
		t.Fatal("a second handle was made for one submodule")
	}
	hash := storeBlob(t, module, "kept in the submodule")
	if err := module.SetReference(plumbing.NewHashReference(testBranch, hash)); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := stor.HasEncodedObject(hash); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("the parent holds the submodule's object: %v", err)
	}
	for _, key := range fake.KeysWithPrefix("") {
		if !strings.HasPrefix(key, "prefix/"+testRepo+"/modules/vendor/lib/") {
			t.Fatalf("the submodule wrote %s, outside its own prefix", key)
		}
	}
	for _, name := range []string{"", "..", "../sibling", "/absolute", "a/../../b"} {
		if _, err := stor.Module(name); err == nil {
			t.Errorf("a submodule was opened under the unsafe name %q", name)
		}
	}
}

// TestAReaderSurvivesThePacksItHoldsBeingRetired covers a replica whose
// manifest names packs that a merge elsewhere retired and, a grace period
// later, deleted. It never missed an object, so it never had cause to read the
// manifest again — and then a pack it reads from is gone. That too is proof the
// manifest held is out of date: it reads it again and reads the object from the
// pack that replaced it. The reader's first probe has already brought some of
// the packs' sidecars into its cache — a filter and its index share an extent —
// so for those it is the pack's own bytes it finds gone, not the index.
func TestAReaderSurvivesThePacksItHoldsBeingRetired(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	fake.opts.IndexFreshness = time.Hour
	writer := testPackedStorage(t, fake)
	var small []plumbing.Hash
	for push := range 4 {
		small = append(small, smallPush(t, writer, "small push "+strings.Repeat("p", push)))
	}

	// The reader has its own disk, so nothing of the old packs is cached on it.
	apart := newFakeS3(t)
	apart.Server = fake.Server
	apart.opts.IndexFreshness = time.Hour
	apart.clock = newTestClock()
	reader := testPackedStorage(t, apart)
	if err := reader.HasEncodedObject(small[0]); err != nil {
		t.Fatalf("premise: the reader takes its snapshot before the merge: %v", err)
	}
	if held := len(reader.manifests.current.Load().packs); held != len(small) {
		t.Fatalf("premise: the reader holds %d packs, want %d", held, len(small))
	}

	result, err := CompactRepository(context.Background(), writer)
	if err != nil || result.Merged != len(small) {
		t.Fatalf("premise: the merge rewrote %d objects (err %v), want %d", result.Merged, err, len(small))
	}
	// The grace period passes and a later compaction deletes the retired packs:
	// their keys are gone.
	retired := writer.manifests.current.Load().manifest.Retired
	if len(retired) != len(small) {
		t.Fatalf("premise: the merge retired %d packs, want %d", len(retired), len(small))
	}
	for _, pack := range retired {
		for _, extension := range packKeySuffixes {
			fake.Remove("prefix/" + testRepo + "/objects/pack/" + pack.Name + extension)
		}
	}
	if len(packKeys(fake, ".pack")) != 1 {
		t.Fatalf("premise: %d packs are left, want the merged one", len(packKeys(fake, ".pack")))
	}

	for _, hash := range small {
		if _, err := reader.EncodedObject(plumbing.AnyObject, hash); err != nil {
			t.Fatalf("an object whose pack was retired under the reader: %v", err)
		}
	}
	if held := len(reader.manifests.current.Load().packs); held != 1 {
		t.Fatalf("the reader still holds %d packs", held)
	}
}

// slowRevalidator is a bucket whose conditional reads take time: whatever it is
// told to do happens after the store has answered a conditional read and before
// the caller hears.
type slowRevalidator struct {
	objstore.Bucket
	mu        sync.Mutex
	meanwhile func()
}

func (b *slowRevalidator) GetIfChanged(ctx context.Context, key string, held objstore.Version) (io.ReadCloser, objstore.Info, error) {
	body, info, err := b.Bucket.GetIfChanged(ctx, key, held)
	b.mu.Lock()
	meanwhile := b.meanwhile
	b.meanwhile = nil
	b.mu.Unlock()
	if meanwhile != nil {
		meanwhile()
	}
	return body, info, err
}

// TestWhatIsWrittenDuringARevalidationIsNotLost covers the race between the two
// ways a handle's state changes. A revalidation takes time; a pack this replica
// commits while one is in flight — a push, a flush of written objects — was not
// in the manifest the store answered with, and the state the revalidation
// would publish must not replace the newer one the commit published — or the
// push's own reference update, a moment later, finds its objects missing.
func TestWhatIsWrittenDuringARevalidationIsNotLost(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	fake.opts.IndexFreshness = time.Hour
	clock := newTestClock()
	bucket := &slowRevalidator{Bucket: objstore.NewS3WithClient(fake.Client().Client, "bucket", 0)}
	store := Open(bucket, "prefix", fake.opts)
	store.shared.now = clock.Now
	stor, err := store.Repository(testRepo)
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	warmed := smallPush(t, stor, "the push that gives the handle a manifest")
	handle, ok := stor.(*repository)
	if !ok {
		t.Fatalf("unexpected storer type %T", stor)
	}

	var pushed, written plumbing.Hash
	bucket.mu.Lock()
	bucket.meanwhile = func() {
		pushed = smallPush(t, stor, "pushed while the revalidation was in flight")
		written = storeBlob(t, stor, "written while the revalidation was in flight")
		if err := handle.FlushObjects(); err != nil {
			t.Errorf("flush: %v", err)
		}
	}
	bucket.mu.Unlock()
	clock.Advance(2 * time.Hour)
	if err := stor.HasEncodedObject(absentHash(1)); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("the probe that starts the revalidation: %v", err)
	}
	if pushed.IsZero() || written.IsZero() {
		t.Fatal("premise: nothing was written while the revalidation was in flight")
	}
	if _, pending := handle.pending.get(written); pending {
		t.Fatal("premise: the written object is still pending, so no pack of it was committed")
	}

	// The state held is new by its own clock and an hour from stale, so nothing
	// will read the manifest again: only the commits' own states can hold what
	// the revalidation's answer did not.
	before := fake.Snapshot()
	for _, hash := range []plumbing.Hash{warmed, pushed, written} {
		if err := stor.HasEncodedObject(hash); err != nil {
			t.Fatalf("what was committed while a revalidation was in flight was lost from the state held: %v", err)
		}
	}
	if spent := fake.Snapshot().Sub(before); spent.Get != 0 {
		t.Fatalf("premise: the objects were found by reading the manifest again: %s", spent)
	}
}

// TestStoredPacksAreOfferedForReuse covers PackSource on both backends that have
// packs: the live packs are named with their sizes and object counts, their
// indexes answer, and their bytes read back at any offset exactly as stored —
// which is all a server needs to answer a clone by copying a pack out.
func TestStoredPacksAreOfferedForReuse(t *testing.T) {
	pack, hashes := pushPack(t, 400)
	if len(pack) < 8000 {
		t.Fatalf("premise: the pack is %d bytes, too small to span the extents read below", len(pack))
	}
	fake := newFakeS3(t)
	fake.opts.ChunkBytes = 4096
	gitDir := t.TempDir()
	onDisk, err := OpenDir(gitDir, testRepo)
	if err != nil {
		t.Fatalf("directory storage: %v", err)
	}
	repoDir, err := RepoGitDirPath(gitDir, testRepo)
	if err != nil {
		t.Fatalf("repository directory: %v", err)
	}
	// A directory gains a pack the way git gives it one — a gc, a fetch by the
	// git binary — which here is go-git's own storage keeping the pack it is sent.
	packers := map[string]gitStorage.Storer{
		"object store": testPackedStorage(t, fake),
		"directory":    gitFilesystem.NewStorage(osfs.New(repoDir), cache.NewObjectLRUDefault()),
	}
	ctx := context.Background()

	for name, stor := range map[string]gitStorage.Storer{"object store": packers["object store"], "directory": onDisk} {
		source, ok := stor.(PackSource)
		if !ok {
			t.Fatalf("%s: a %T offers no stored packs", name, stor)
		}
		if packs, err := source.StoredPacks(ctx); err != nil || len(packs) != 0 {
			t.Fatalf("%s: a repository with no packs lists %v, %v", name, packs, err)
		}
		if err := packfile.UpdateObjectStorage(packers[name], bytes.NewReader(pack)); err != nil {
			t.Fatalf("%s: push: %v", name, err)
		}
		packs, err := source.StoredPacks(ctx)
		if err != nil || len(packs) != 1 {
			t.Fatalf("%s: stored packs: %v, %v", name, packs, err)
		}
		if packs[0].Objects != len(hashes) || packs[0].Size <= 0 || !strings.HasPrefix(packs[0].Name, "pack-") {
			t.Fatalf("%s: the pack is described as %+v, want %d objects", name, packs[0], len(hashes))
		}
		index, err := source.PackIndex(ctx, packs[0].Name)
		if err != nil {
			t.Fatalf("%s: index: %v", name, err)
		}
		for _, hash := range hashes {
			if held, err := index.Contains(hash); err != nil || !held {
				t.Fatalf("%s: the index does not list %s (%v)", name, hash, err)
			}
		}

		reader, err := source.OpenPack(ctx, packs[0].Name)
		if err != nil {
			t.Fatalf("%s: open: %v", name, err)
		}
		whole := make([]byte, packs[0].Size)
		if _, err := reader.ReadAt(whole, 0); err != nil {
			t.Fatalf("%s: read: %v", name, err)
		}
		if !bytes.HasPrefix(whole, []byte("PACK")) {
			t.Fatalf("%s: the stored pack begins %q", name, whole[:4])
		}
		// A read that starts mid-extent and crosses into the next.
		middle := make([]byte, 5000)
		if _, err := reader.ReadAt(middle, 3000); err != nil || !bytes.Equal(middle, whole[3000:8000]) {
			t.Fatalf("%s: a read across extents returned other bytes than the pack holds (%v)", name, err)
		}
		if n, err := reader.ReadAt(make([]byte, 64), packs[0].Size-10); n != 10 || !errors.Is(err, io.EOF) {
			t.Fatalf("%s: a read past the end returned %d bytes and %v", name, n, err)
		}
		if err := reader.Close(); err != nil {
			t.Fatalf("%s: close: %v", name, err)
		}

		for _, unknown := range []string{"pack-" + absentHash(1).String(), "../../etc/passwd", "pack-1"} {
			if _, err := source.OpenPack(ctx, unknown); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s: opening %q: %v, want os.ErrNotExist", name, unknown, err)
			}
			if _, err := source.PackIndex(ctx, unknown); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s: the index of %q: %v, want os.ErrNotExist", name, unknown, err)
			}
		}
	}

	memory, err := OpenMemory(testRepo)
	if err != nil {
		t.Fatalf("memory storage: %v", err)
	}
	if _, ok := memory.(PackSource); ok {
		t.Fatal("the memory backend, which stores no packs, offers them")
	}
}

// TestAPackFileIsReadSequentiallyToo covers the other way the decoder, and a
// caller copying a pack out, reads: from a position, to the end.
func TestAPackFileIsReadSequentiallyToo(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.ChunkBytes = 1024
	stor := testPackedStorage(t, fake)
	pack, _ := pushPack(t, 60)
	if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(pack)); err != nil {
		t.Fatalf("push: %v", err)
	}
	state, err := stor.manifests.held()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	file := newPackFile(state.packs[0].pack, "pack")
	whole, err := io.ReadAll(file)
	if err != nil || !bytes.Equal(whole, pack) {
		t.Fatalf("a sequential read returned %d bytes (err %v), want the %d pushed", len(whole), err, len(pack))
	}
	if at, err := file.Seek(-20, io.SeekEnd); err != nil || at != int64(len(pack))-20 {
		t.Fatalf("seek from the end: %d, %v", at, err)
	}
	if at, err := file.Seek(-4, io.SeekCurrent); err != nil || at != int64(len(pack))-24 {
		t.Fatalf("seek from the position: %d, %v", at, err)
	}
	tail, _ := io.ReadAll(file)
	if !bytes.Equal(tail, pack[len(pack)-24:]) {
		t.Fatal("a read after a seek returned other bytes than the pack holds")
	}
	if _, err := file.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("a seek before the start succeeded")
	}
	if _, err := file.Seek(0, 99); err == nil {
		t.Fatal("a seek from nowhere succeeded")
	}
	if _, err := file.Write([]byte("x")); err == nil {
		t.Fatal("a stored pack accepted a write")
	}
	if err := file.Truncate(0); err == nil {
		t.Fatal("a stored pack accepted a truncation")
	}
	if file.Lock() != nil || file.Unlock() != nil || file.Name() != "pack" {
		t.Fatal("the pack adapter misreports itself")
	}
}

// sortedKeys is a small aid for failure messages.
func sortedKeys(set map[string]string) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// TestInitIsIdempotent covers the call an application makes on every open: the
// first makes the repository — a HEAD, a bare config — and the second finds it
// made and changes nothing.
func TestInitIsIdempotent(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	if err := Init(stor); err != nil {
		t.Fatalf("init: %v", err)
	}
	refs := advertise(t, stor)
	if _, ok := refs["HEAD"]; !ok {
		t.Fatalf("init left no HEAD: %v", sortedKeys(refs))
	}
	if err := stor.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, testBranch)); err != nil {
		t.Fatalf("set HEAD: %v", err)
	}
	before := fake.Snapshot()
	if err := Init(testPackedStorage(t, fake)); err != nil {
		t.Fatalf("second init: %v", err)
	}
	if spent := fake.Snapshot().Sub(before); spent.Put != 0 {
		t.Fatalf("initializing a repository that exists wrote to it: %s", spent)
	}
	if head, err := stor.Reference(plumbing.HEAD); err != nil || head.Target() != testBranch {
		t.Fatalf("a second init moved HEAD to %v (%v)", head, err)
	}
}

// TestARepositoryIsNeverSeenWithABranchItsHeadDoesNotName pins what a reader
// may find while a repository is given its first branch. A branch in a
// repository whose HEAD still names some other, unborn branch is a state no
// repository should be in: a clone at that moment checks out nothing and warns
// that HEAD refers to a nonexistent ref. When references were objects the two
// were two writes, ordered with care; they are now one commit of the manifest,
// so there is no moment between them.
func TestARepositoryIsNeverSeenWithABranchItsHeadDoesNotName(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	branch := plumbing.NewHashReference(plumbing.NewBranchReferenceName("trunk"), plumbing.NewHash("1111111111111111111111111111111111111111"))

	writes := countManifestWrites(fake)
	t.Cleanup(func() { fake.SetOnRequest(nil) })
	if err := stor.InitializeRepositoryReferences(branch, true); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if got := writes(); got != 1 {
		t.Fatalf("the first branch and HEAD took %d writes of the manifest, want one", got)
	}
	first := storedManifest(t, fake)
	if first.Sequence != 1 || len(first.Refs.Changes) != 2 {
		t.Fatalf("the first manifest is sequence %d with %+v, want HEAD and the branch together", first.Sequence, first.Refs.Changes)
	}
	head, err := testPackedStorage(t, fake).Reference(plumbing.HEAD)
	if err != nil || head.Target() != branch.Name() {
		t.Fatalf("HEAD is %v (%v), want a symbolic reference to %s", head, err, branch.Name())
	}
	if err := stor.InitializeRepositoryReferences(branch, true); !errors.Is(err, ErrReferenceAlreadyExists) {
		t.Fatalf("initializing twice: %v, want ErrReferenceAlreadyExists", err)
	}
}

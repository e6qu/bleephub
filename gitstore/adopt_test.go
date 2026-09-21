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
	"github.com/go-git/go-git/v5/plumbing/format/objfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/storage/memory"
)

// oldLayoutPack puts a pack in the fake as the earlier layouts left one: a
// .pack, a .idx and a .bfilter under objects/pack/, and nothing else.
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
	// A scratch repository of the engine's own builds the index and the filter,
	// in a sidecar; they are cut out of it and put where the old layouts kept
	// them, as objects of their own.
	scratch, err := fake.store("scratch").Repository("scratch/" + seed)
	if err != nil {
		t.Fatalf("scratch: %v", err)
	}
	if err := packfile.UpdateObjectStorage(scratch, &pack); err != nil {
		t.Fatalf("scratch push: %v", err)
	}
	var name string
	directory := "prefix/" + repo + "/objects/pack/"
	for _, key := range fake.KeysWithPrefix("scratch/scratch/" + seed + "/objects/pack/") {
		data, _ := fake.Get(key)
		base := key[strings.LastIndex(key, "/")+1:]
		if trimmed, isPack := strings.CutSuffix(base, ".pack"); isPack {
			name = trimmed
			fake.Put(directory+base, data)
			continue
		}
		trimmed, isSidecar := strings.CutSuffix(base, sidecarSuffix)
		if !isSidecar {
			t.Fatalf("the scratch repository wrote %s", key)
		}
		footer, err := decodeSidecarFooter(data[len(data)-sidecarFooterSize:])
		if err != nil {
			t.Fatalf("scratch sidecar: %v", err)
		}
		fake.Put(directory+trimmed+".idx", data[:footer.indexBytes])
		fake.Put(directory+trimmed+".bfilter", data[footer.indexBytes:footer.indexBytes+footer.filterBytes])
	}
	return name, hashes
}

// oldLooseObject puts an object in the fake as the earlier layouts kept one
// written through the API: git's loose format — the type, the size and the
// content, zlib-compressed — under objects/XX/ and the other 38 hex digits.
func oldLooseObject(t *testing.T, fake *fakeS3, repo string, kind plumbing.ObjectType, content string) plumbing.Hash {
	t.Helper()
	var encoded bytes.Buffer
	writer := objfile.NewWriter(&encoded)
	if err := writer.WriteHeader(kind, int64(len(content))); err != nil {
		t.Fatalf("loose header: %v", err)
	}
	if _, err := writer.Write([]byte(content)); err != nil {
		t.Fatalf("loose write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("loose close: %v", err)
	}
	hash := writer.Hash()
	fake.Put("prefix/"+repo+"/objects/"+looseKeySuffix(hash), encoded.Bytes())
	return hash
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
	} {
		fake.Put(prefix+key, []byte(content))
	}
	// Objects written through the API, kept loose; one of them is also in the
	// live pack, as an object written twice was.
	loose := []plumbing.Hash{
		oldLooseObject(t, fake, repo, plumbing.BlobObject, "written through the API"),
		oldLooseObject(t, fake, repo, plumbing.BlobObject, "written through the API, and again"),
		oldLooseObject(t, fake, repo, plumbing.BlobObject, "live object 3"),
	}
	if loose[2] != liveObjects[3] {
		t.Fatalf("premise: the loose copy hashed to %s, the packed one to %s", loose[2], liveObjects[3])
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
	if got := reports[0]; got.Packs != 2 || got.Retired != 1 || got.Loose != len(loose) || got.References != 6 || got.Sequence != 1 {
		t.Fatalf("adopt reported %+v, want 2 live packs (the old one and the loose objects'), 1 retired, %d loose, 6 references at sequence 1", got, len(loose))
	}

	data, _ := fake.Get(prefix + manifestName)
	adopted, err := decodeManifest(data)
	if err != nil {
		t.Fatalf("the adopted manifest: %v", err)
	}
	if adopted.Format != manifestFormat || len(adopted.Packs) != 2 {
		t.Fatalf("the adopted manifest is format %d with packs %+v, want format %d and two", adopted.Format, adopted.Packs, manifestFormat)
	}
	for _, pack := range adopted.Packs {
		wantObjects := len(loose)
		if pack.Name == live {
			wantObjects = len(liveObjects)
		}
		if pack.Objects != wantObjects || pack.Source != packSourceAdoption || pack.FilterBytes == 0 || pack.SidecarBytes != sidecarBytes(pack.IndexBytes, pack.FilterBytes) {
			t.Fatalf("the adopted pack %+v, want %d objects and a sidecar", pack, wantObjects)
		}
		if sidecar, ok := fake.Get(prefix + "objects/pack/" + pack.Name + sidecarSuffix); !ok || int64(len(sidecar)) != pack.SidecarBytes {
			t.Fatalf("the sidecar of %s is %d bytes (stored: %v), want %d", pack.Name, len(sidecar), ok, pack.SidecarBytes)
		}
	}
	if adopted.Packs[0].Name != live && adopted.Packs[1].Name != live {
		t.Fatalf("the adopted live packs are %+v, want %s among them", adopted.Packs, live)
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
	for _, hash := range append(append([]plumbing.Hash{}, liveObjects...), loose...) {
		if err := stor.HasEncodedObject(hash); err != nil {
			t.Fatalf("an object of the live pack, or a loose one, is missing after adoption: %v", err)
		}
	}
	if got := readObjects(t, testPackedStorageNamed(t, fake, repo), loose[:1])[loose[0]]; got != "written through the API\x00blob" {
		t.Fatalf("a loose object read back %q after adoption", got)
	}
	if err := stor.HasEncodedObject(unfinishedObjects[0]); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("a pack without an index was adopted: %v", err)
	}
	module, err := stor.Module("lib")
	if err != nil {
		t.Fatalf("module: %v", err)
	}
	resolvesTo(t, "the adopted submodule", module, "refs/heads/trunk", packedOnly)

	// Nothing of the old layout was touched — what was added is the two
	// manifests, the live pack's sidecar, and the pack of the loose objects with
	// its sidecar — and a second adoption is refused.
	after := fake.KeysWithPrefix(prefix)
	if len(after) != len(before)+5 {
		t.Fatalf("adoption left %d keys where there were %d, want five added and nothing else", len(after), len(before))
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
	// HEAD, four references and packed-refs; the marker; the separate indexes
	// and filters — the live pack's two, the superseded one's two and the
	// unfinished one's filter; the three loose objects; the submodule's HEAD
	// and branch.
	if err != nil || removed != 6+1+5+3+2 {
		t.Fatalf("removed %d keys (%v), want %d", removed, err, 6+1+5+3+2)
	}
	for _, key := range fake.KeysWithPrefix(prefix) {
		relative := strings.TrimPrefix(key, prefix)
		if relative == "HEAD" || relative == "packed-refs" || strings.HasPrefix(relative, "refs/") || strings.HasSuffix(relative, ".superseded") ||
			strings.HasSuffix(relative, ".idx") || strings.HasSuffix(relative, ".bfilter") || isLooseObjectKey(relative) || strings.HasPrefix(relative, "modules/lib/refs/") {
			t.Fatalf("the old layout's %s is still there", relative)
		}
	}
	// A replica with a disk of its own reads everything from what is left.
	apart := newFakeS3(t)
	apart.Server = fake.Server
	remaining := testPackedStorageNamed(t, apart, repo)
	resolvesTo(t, "after the old layout was removed", remaining, testBranch, tip)
	readObjects(t, remaining, append(append([]plumbing.Hash{}, liveObjects...), loose...))
}

// TestAdoptConvertsAFormatOneManifest is the upgrade from the first manifest
// format, which named the packs and held the references but kept each pack's
// index and filter as objects of their own and the objects written through the
// API loose. A normal handle refuses such a repository as outdated rather than
// read part of it; Adopt writes each live pack's sidecar, packs the loose
// objects, and swaps in a format-2 manifest that keeps everything the format-1
// one said. Adopting again is refused, and removing the old layout afterwards
// loses nothing.
func TestAdoptConvertsAFormatOneManifest(t *testing.T) {
	fake := newFakeS3(t)
	const repo = "octocat/format-one"
	prefix := "prefix/" + repo + "/"
	store := fake.store("prefix")

	live, liveObjects := oldLayoutPack(t, fake, repo, 10, "format-one")
	loose := make([]plumbing.Hash, 0, 4)
	for i := range 4 {
		loose = append(loose, oldLooseObject(t, fake, repo, plumbing.BlobObject, fmt.Sprintf("written through the API %d", i)))
	}
	sizeOf := func(suffix string) int64 {
		data, ok := fake.Get(prefix + "objects/pack/" + live + suffix)
		if !ok {
			t.Fatalf("premise: no %s%s", live, suffix)
		}
		return int64(len(data))
	}
	tip := liveObjects[0]
	formatOne := fmt.Sprintf(`{"format":1,"sequence":7,`+
		`"packs":[{"name":%q,"bytes":%d,"index_bytes":%d,"filter_bytes":%d,"objects":%d,"source":"push","added":"2020-01-01T00:00:00Z"}],`+
		`"retired":[],"refs":{"changes":[{"name":"HEAD","value":"ref: refs/heads/main"},{"name":"refs/heads/main","value":%q}]}}`,
		live, sizeOf(".pack"), sizeOf(".idx"), sizeOf(".bfilter"), len(liveObjects), tip.String())
	fake.Put(prefix+manifestName, []byte(formatOne))

	// A normal handle refuses the repository, reads and writes alike.
	if _, err := store.ExistingRepository(repo); !errors.Is(err, ErrManifestOutdated) {
		t.Fatalf("opening a format-1 repository answered %v, want ErrManifestOutdated", err)
	}
	outdated := testPackedStorageNamed(t, fake, repo)
	if _, err := outdated.Reference(testBranch); !errors.Is(err, ErrManifestOutdated) {
		t.Fatalf("reading a reference of a format-1 repository answered %v, want ErrManifestOutdated", err)
	}
	if err := outdated.HasEncodedObject(tip); !errors.Is(err, ErrManifestOutdated) {
		t.Fatalf("probing a format-1 repository answered %v, want ErrManifestOutdated", err)
	}

	reports, err := store.Adopt(context.Background(), repo)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if len(reports) != 1 || reports[0].Packs != 2 || reports[0].Loose != len(loose) || reports[0].References != 2 || reports[0].Sequence != 8 {
		t.Fatalf("adopt reported %+v, want 2 live packs, %d loose objects packed, 2 references, at sequence 8", reports, len(loose))
	}
	adopted := func() *manifest {
		t.Helper()
		data, _ := fake.Get(prefix + manifestName)
		decoded, err := decodeManifest(data)
		if err != nil {
			t.Fatalf("the adopted manifest: %v", err)
		}
		return decoded
	}()
	if adopted.Format != manifestFormat || len(adopted.Packs) != 2 {
		t.Fatalf("the adopted manifest is format %d with packs %+v", adopted.Format, adopted.Packs)
	}
	for _, pack := range adopted.Packs {
		if pack.SidecarBytes != sidecarBytes(pack.IndexBytes, pack.FilterBytes) || pack.FilterBytes == 0 {
			t.Fatalf("the adopted pack %+v has no whole sidecar", pack)
		}
		if pack.Name == live && (pack.Source != "push" || pack.IndexBytes != sizeOf(".idx") || pack.FilterBytes != sizeOf(".bfilter")) {
			t.Fatalf("the format-1 pack was adopted as %+v, want what the format-1 manifest said of it", pack)
		}
		if pack.Name != live && (pack.Source != packSourceAdoption || pack.Objects != len(loose)) {
			t.Fatalf("the loose objects were adopted as %+v", pack)
		}
	}

	all := append(append([]plumbing.Hash{}, liveObjects...), loose...)
	check := func(when string) {
		t.Helper()
		// A replica with a disk of its own, so every byte is read from the store.
		apart := newFakeS3(t)
		apart.Server = fake.Server
		fresh := testPackedStorageNamed(t, apart, repo)
		resolvesTo(t, when, fresh, testBranch, tip)
		if head, err := fresh.Reference(plumbing.HEAD); err != nil || head.Target() != testBranch {
			t.Fatalf("%s: HEAD is %v (%v)", when, head, err)
		}
		got := readObjects(t, fresh, all)
		for i, hash := range loose {
			if want := fmt.Sprintf("written through the API %d\x00blob", i); got[hash] != want {
				t.Fatalf("%s: a loose object read back %q, want %q", when, got[hash], want)
			}
		}
	}
	check("after adoption")

	before, _ := fake.Get(prefix + manifestName)
	if _, err := store.Adopt(context.Background(), repo); !errors.Is(err, ErrAlreadyAdopted) {
		t.Fatalf("adopting twice answered %v, want ErrAlreadyAdopted", err)
	}
	if again, _ := fake.Get(prefix + manifestName); !bytes.Equal(again, before) {
		t.Fatal("the refused adoption rewrote the manifest")
	}

	removed, err := store.RemoveAdoptedLayout(context.Background(), repo)
	if err != nil || removed != 2+len(loose) {
		t.Fatalf("removed %d keys (%v), want the pack's index and filter and the %d loose objects", removed, err, len(loose))
	}
	for _, key := range fake.KeysWithPrefix(prefix) {
		relative := strings.TrimPrefix(key, prefix)
		if strings.HasSuffix(relative, ".idx") || strings.HasSuffix(relative, ".bfilter") || isLooseObjectKey(relative) {
			t.Fatalf("the old layout's %s is still there", relative)
		}
	}
	check("after the old layout was removed")
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

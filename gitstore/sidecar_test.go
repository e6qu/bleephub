package gitstore

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

// TestASidecarFooterRoundTrips pins the sidecar's layout: the index, then the
// filter, then a footer of the magic, the two lengths and the pack's checksum,
// which a reader decodes to find the parts and to know which pack they belong
// to. Anything that is not a footer is refused as errSidecar rather than read.
func TestASidecarFooterRoundTrips(t *testing.T) {
	index, filter := bytes.Repeat([]byte{0xab}, 1100), bytes.Repeat([]byte{0xcd}, 37)
	pack := hashOf(42)
	encoded := encodeSidecar(index, filter, pack)
	if int64(len(encoded)) != sidecarBytes(int64(len(index)), int64(len(filter))) {
		t.Fatalf("a sidecar of an index of %d bytes and a filter of %d is %d bytes, sidecarBytes says %d",
			len(index), len(filter), len(encoded), sidecarBytes(int64(len(index)), int64(len(filter))))
	}
	if !bytes.Equal(encoded[:len(index)], index) || !bytes.Equal(encoded[len(index):len(index)+len(filter)], filter) {
		t.Fatal("the sidecar does not begin with the index and then the filter")
	}
	footer, err := decodeSidecarFooter(encoded[len(encoded)-sidecarFooterSize:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if footer.indexBytes != int64(len(index)) || footer.filterBytes != int64(len(filter)) || footer.pack != pack {
		t.Fatalf("the footer decoded as %+v, want %d, %d and %s", footer, len(index), len(filter), pack)
	}
	if empty, err := decodeSidecarFooter(encodeSidecar(index, nil, pack)[len(index):]); err != nil || empty.filterBytes != 0 {
		t.Fatalf("the footer of a sidecar without a filter decoded as %+v, %v", empty, err)
	}

	tail := encoded[len(encoded)-sidecarFooterSize:]
	wrongMagic := bytes.Clone(tail)
	wrongMagic[0] ^= 0xff
	huge := bytes.Clone(tail)
	binary.BigEndian.PutUint64(huge[len(sidecarMagic):], 1<<63)
	for name, malformed := range map[string][]byte{
		"a footer with another magic":            wrongMagic,
		"a footer cut short":                     tail[1:],
		"a footer with junk after it":            append(bytes.Clone(tail), 0),
		"a footer giving a length no object has": huge,
		"nothing":                                nil,
	} {
		if _, err := decodeSidecarFooter(malformed); !errors.Is(err, errSidecar) {
			t.Errorf("%s decoded with %v, want errSidecar", name, err)
		}
	}
}

// TestAManifestWhoseSidecarSizeDisagreesIsRefused pins that the manifest's
// record of a sidecar is checked against what it must be: the index, the filter
// and the footer. A reader addresses the parts by those sizes without asking
// the store, so a manifest that disagrees with itself would send it to read the
// wrong bytes; the manifest is refused whole instead.
func TestAManifestWhoseSidecarSizeDisagreesIsRefused(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 5)
	var raw map[string]any
	data, _ := fake.Get(manifestKey)
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	packs, _ := raw["packs"].([]any)
	if len(packs) != 1 {
		t.Fatalf("premise: the manifest names %d packs", len(packs))
	}
	entry, _ := packs[0].(map[string]any)
	if entry["sidecar_bytes"] == nil {
		t.Fatalf("premise: the manifest's pack entry has no sidecar_bytes: %v", entry)
	}
	entry["sidecar_bytes"] = entry["sidecar_bytes"].(float64) + 1
	tampered, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	fake.Put(manifestKey, tampered)

	reader := testPackedStorage(t, fake)
	if err := reader.HasEncodedObject(hashes[0]); !errors.Is(err, ErrManifestFormat) {
		t.Fatalf("a manifest whose sidecar size is not its index, filter and footer was read: %v", err)
	}
	if _, err := reader.Reference(testBranch); !errors.Is(err, ErrManifestFormat) {
		t.Fatalf("a manifest whose sidecar size is not its index, filter and footer was read: %v", err)
	}
}

// TestASidecarThatIsNotThePacksIsRefused pins the check a reader makes when it
// loads an index: the sidecar's footer must describe the pack the manifest
// names, with the lengths the manifest records, and the index in it must index
// that pack. A sidecar left from something else — a torn copy, a key rewritten
// by hand — is refused as errSidecar, never read as though it were the pack's:
// an index of another pack would send reads to offsets that hold other objects.
func TestASidecarThatIsNotThePacksIsRefused(t *testing.T) {
	fake := newFakeS3(t)
	fake.opts.CompactAfterPacks = -1
	stor := testPackedStorage(t, fake)
	// Two single-object packs: their indexes are the same size, so one can be
	// put in the other's sidecar without the lengths giving it away.
	first := smallPush(t, stor, "the first pack's object")
	second := smallPush(t, stor, "the second pack's object")
	stored := storedManifest(t, fake).Packs
	if len(stored) != 2 || stored[0].IndexBytes != stored[1].IndexBytes || stored[0].FilterBytes != stored[1].FilterBytes {
		t.Fatalf("premise: the packs are %+v, want two with sidecars of one shape", stored)
	}
	directory := "prefix/" + testRepo + "/objects/pack/"
	sidecarOf := func(name string) []byte {
		t.Helper()
		data, ok := fake.Get(directory + name + sidecarSuffix)
		if !ok {
			t.Fatalf("no sidecar for %s", name)
		}
		return data
	}
	original := map[string][]byte{stored[0].Name: sidecarOf(stored[0].Name), stored[1].Name: sidecarOf(stored[1].Name)}
	target, other := stored[0], stored[1]

	for name, tampered := range map[string]func() []byte{
		"a footer naming another pack": func() []byte {
			data := bytes.Clone(original[target.Name])
			copy(data[len(data)-hashSize:], original[other.Name][len(data)-hashSize:])
			return data
		},
		"a footer giving other lengths": func() []byte {
			data := bytes.Clone(original[target.Name])
			at := len(data) - sidecarFooterSize + len(sidecarMagic)
			binary.BigEndian.PutUint64(data[at:], uint64(target.IndexBytes-1))
			binary.BigEndian.PutUint64(data[at+8:], uint64(target.FilterBytes+1))
			return data
		},
		"another pack's index under this pack's footer": func() []byte {
			data := bytes.Clone(original[target.Name])
			copy(data[:target.IndexBytes], original[other.Name][:other.IndexBytes])
			return data
		},
		"no footer at all": func() []byte {
			data := bytes.Clone(original[target.Name])
			copy(data[len(data)-sidecarFooterSize:], bytes.Repeat([]byte{0}, sidecarFooterSize))
			return data
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake.Put(directory+target.Name+sidecarSuffix, tampered())
			t.Cleanup(func() { fake.Put(directory+target.Name+sidecarSuffix, original[target.Name]) })

			// A replica with a disk of its own, so the tampered bytes are the
			// ones read.
			apart := newFakeS3(t)
			apart.Server = fake.Server
			reader := testPackedStorage(t, apart)
			state, err := reader.manifests.held()
			if err != nil {
				t.Fatalf("manifest: %v", err)
			}
			pack := state.pack(target.Name)
			if pack == nil || pack.parsed.Load() != nil {
				t.Fatalf("premise: the reader holds %v for %s, want it unread", pack, target.Name)
			}
			if _, err := pack.loadIndex(); !errors.Is(err, errSidecar) {
				t.Fatalf("loading the index from %s answered %v, want errSidecar", name, err)
			}
			if pack.parsed.Load() != nil {
				t.Fatal("a refused index was kept")
			}
			// Through the reads: the object of the tampered pack is not read, and
			// the refusal is not taken for an absence.
			hash := first
			if target.Name == stored[1].Name {
				hash = second
			}
			if _, err := reader.EncodedObject(plumbing.AnyObject, hash); !errors.Is(err, errSidecar) || errors.Is(err, plumbing.ErrObjectNotFound) {
				t.Fatalf("reading through %s answered %v, want errSidecar", name, err)
			}
		})
	}

	// Put back, the sidecars read as they did.
	readObjects(t, testPackedStorage(t, fake), []plumbing.Hash{first, second})
}

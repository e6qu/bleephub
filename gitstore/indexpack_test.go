package gitstore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/storage/memory"
)

// spoolPack writes pack to a file of its own.
func spoolPack(t *testing.T, pack []byte) *os.File {
	t.Helper()
	file, err := os.Create(filepath.Join(t.TempDir(), "spooled.pack"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err := file.Write(pack); err != nil {
		t.Fatal(err)
	}
	return file
}

// indexEntries lists an index as id → offset and checksum.
func indexEntries(t *testing.T, index idxfile.Index) map[plumbing.Hash][2]uint64 {
	t.Helper()
	entries, err := index.Entries()
	if err != nil {
		t.Fatal(err)
	}
	listed := map[plumbing.Hash][2]uint64{}
	for {
		entry, err := entries.Next()
		if errors.Is(err, io.EOF) {
			return listed
		}
		if err != nil {
			t.Fatal(err)
		}
		listed[entry.Hash] = [2]uint64{entry.Offset, uint64(entry.CRC32)}
	}
}

// goGitIndex indexes a self-contained pack with go-git's parser, the reference.
func goGitIndex(t *testing.T, pack []byte) (map[plumbing.Hash][2]uint64, plumbing.Hash) {
	t.Helper()
	writer := new(idxfile.Writer)
	parser, err := packfile.NewParser(packfile.NewScanner(bytes.NewReader(pack)), writer)
	if err != nil {
		t.Fatal(err)
	}
	checksum, err := parser.Parse()
	if err != nil {
		t.Fatalf("go-git cannot parse the pack: %v", err)
	}
	index, err := writer.Index()
	if err != nil {
		t.Fatal(err)
	}
	return indexEntries(t, index), checksum
}

// historyPack makes, with stock git, a pack of revisions of one file, each a
// delta of another, and names every base by id: git pack-objects does that
// without --delta-base-offset.
func historyPack(t *testing.T, client *gitClient, revisions int) ([]byte, []plumbing.Hash) {
	t.Helper()
	var tips []plumbing.Hash
	for revision := range revisions {
		tips = append(tips, client.commit("main.go", sourceFile(fmt.Sprint(revision)), fmt.Sprint(revision)))
	}
	pack := client.run([]byte(tips[len(tips)-1].String()+"\n"), "pack-objects", "--stdout", "--revs", "--depth=50", "-q")
	return pack, tips
}

// TestAPackIsIndexedAsGoGitIndexesIt holds the indexer to go-git's parser on
// a pack stock git made whose deltas all name their bases by id, deltas of
// deltas among them: the same objects at the same offsets with the same
// checksums, and the same pack checksum.
func TestAPackIsIndexedAsGoGitIndexesIt(t *testing.T) {
	pack, _ := historyPack(t, newGitClient(t), 8)
	if len(refDeltaBases(t, pack)) < 2 {
		t.Fatal("premise broken: the pack holds fewer than two deltas named by id")
	}
	want, wantChecksum := goGitIndex(t, pack)

	file := spoolPack(t, pack)
	index, checksum, err := indexPack(file, layoutOf(file), nil)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	got := indexEntries(t, index)
	if checksum != wantChecksum || len(got) != len(want) {
		t.Fatalf("indexed %d objects under %s, go-git %d under %s", len(got), checksum, len(want), wantChecksum)
	}
	for id, at := range want {
		if got[id] != at {
			t.Fatalf("object %s: indexed at %v, go-git at %v", id, got[id], at)
		}
	}
}

// TestACompletedThinPackIsAPackGoGitReads completes a thin pack and hands the
// result to go-git's parser, which knows nothing of how it was made: it must
// read it on its own and find exactly what the indexer recorded.
func TestACompletedThinPackIsAPackGoGitReads(t *testing.T) {
	client := newGitClient(t)
	_, tips := historyPack(t, client, 3)
	base := client.run([]byte(tips[1].String()+"\n"), "pack-objects", "--stdout", "--revs", "-q")
	thin := client.run([]byte(tips[2].String()+"\n^"+tips[1].String()+"\n"), "pack-objects", "--stdout", "--revs", "--thin", "-q")

	repository := memory.NewStorage()
	if err := packfile.UpdateObjectStorage(repository, bytes.NewReader(base)); err != nil {
		t.Fatal(err)
	}
	file := spoolPack(t, thin)
	if _, _, err := indexPack(file, layoutOf(file), nil); err == nil {
		t.Fatal("premise broken: the pack is not thin")
	}
	index, checksum, err := indexPack(file, layoutOf(file), repository)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	completed, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	want, wantChecksum := goGitIndex(t, completed)
	got := indexEntries(t, index)
	if checksum != wantChecksum || len(got) != len(want) {
		t.Fatalf("recorded %d objects under %s, go-git reads %d under %s", len(got), checksum, len(want), wantChecksum)
	}
	for id, at := range want {
		if got[id] != at {
			t.Fatalf("object %s: recorded at %v, go-git reads it at %v", id, got[id], at)
		}
	}
	if !bytes.Equal(completed[12:len(thin)-hashSize], thin[12:len(thin)-hashSize]) {
		t.Fatal("completing the pack changed the objects pushed")
	}
}

// TestAPackThatIsNotWhatItClaimsIsRefused pins the checksum: a pack is named
// by it, so a pack whose contents do not match, or that goes on after it, is
// refused rather than stored under a name that is not its own.
func TestAPackThatIsNotWhatItClaimsIsRefused(t *testing.T) {
	pack, _ := historyPack(t, newGitClient(t), 2)
	if layout := readPackLayout(bytes.NewReader(pack)); layout.err != nil {
		t.Fatalf("premise broken: the pack as made is refused: %v", layout.err)
	}
	altered := bytes.Clone(pack)
	altered[len(altered)-hashSize-1] ^= 0xff
	followed := append(bytes.Clone(pack), 0)
	for name, bad := range map[string][]byte{"altered": altered, "followed by more": followed} {
		if layout := readPackLayout(bytes.NewReader(bad)); layout.err == nil {
			t.Errorf("a pack %s was accepted", name)
		}
	}
}

func TestApplyDelta(t *testing.T) {
	base := []byte("0123456789abcdef")
	cases := []struct {
		name  string
		base  []byte
		delta []byte
		want  string
	}{
		// sizes 16 → 7; copy 4 bytes from offset 2; insert "xyz".
		{"copy then insert", base, []byte{16, 7, 0x80 | 0x01 | 0x10, 2, 4, 3, 'x', 'y', 'z'}, "2345xyz"},
		// A copy with no size bytes copies 0x10000 bytes; with no offset
		// bytes it starts at 0.
		{"empty base, insert only", nil, []byte{0, 2, 2, 'h', 'i'}, "hi"},
	}
	for _, tc := range cases {
		got, err := applyDelta(tc.base, tc.delta)
		if err != nil || string(got) != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}

	big := bytes.Repeat([]byte{7}, 0x10000)
	got, err := applyDelta(big, []byte{0x80, 0x80, 0x04, 0x80, 0x80, 0x04, 0x80})
	if err != nil || !bytes.Equal(got, big) {
		t.Errorf("a copy with no size bytes: got %d bytes, %v; want all %d", len(got), err, len(big))
	}

	bad := map[string][]byte{
		"source size is not the base's": {15, 1, 1, 'x'},
		"copy past the base":            {16, 4, 0x80 | 0x01 | 0x10, 14, 4},
		"insert past the delta":         {16, 3, 3, 'x'},
		"result past its size":          {16, 1, 2, 'x', 'y'},
		"result short of its size":      {16, 3, 1, 'x'},
		"reserved instruction":          {16, 1, 0},
		"copy missing its offset":       {16, 1, 0x80 | 0x01},
		"size never ends":               {0x80, 0x80},
	}
	for name, delta := range bad {
		if _, err := applyDelta(base, delta); err == nil {
			t.Errorf("%s: applied", name)
		}
	}
}

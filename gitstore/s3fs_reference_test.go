package gitstore

import (
	"io"
	"os"
	"testing"
)

func writeReferenceFile(t *testing.T, fs *S3FS, name, body string) {
	t.Helper()
	file, err := fs.Create(name)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if _, err := file.Write([]byte(body)); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
}

func readAll(t *testing.T, fs *S3FS, name string) string {
	t.Helper()
	file, err := fs.Open(name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

// TestResolvingAReferenceCostsOneRequest pins what go-git's stat-then-open costs
// against an object store: one GET, where a HEAD and a GET were paid before. A
// server resolves a branch many times in the course of one push or clone.
func TestResolvingAReferenceCostsOneRequest(t *testing.T) {
	fake := newFakeS3(t)
	stor := testPackedStorage(t, fake)
	hashes := seedObjects(t, stor, 3)
	tip := hashes[len(hashes)-1]

	fresh := testPackedStorage(t, fake)
	// A handle builds its lazy state once, on first use; a server keeps one per
	// repository for its lifetime, so the steady state is what is priced here.
	if _, err := fresh.Reference("refs/heads/main"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	before := fake.Snapshot()
	ref, err := fresh.Reference("refs/heads/main")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ref.Hash() != tip {
		t.Fatalf("resolved %s, want %s", ref.Hash(), tip)
	}
	spent := fake.Snapshot().Sub(before)
	if spent.Head != 0 || spent.Get != 1 || spent.Total() != 1 {
		t.Fatalf("resolving a reference should cost one GET and no HEAD: %s", spent)
	}
}

// TestAReferenceHandoffNeverServesStaleBytes covers the guards that keep the
// saving from weakening a compare-and-set: a read made in order to write always
// reaches the store, a local write discards what was fetched before it, and
// bytes nobody collected expire.
func TestAReferenceHandoffNeverServesStaleBytes(t *testing.T) {
	fake := newFakeS3(t)
	fs := fake.fs("bucket", "prefix")
	const name = "refs/heads/main"
	writeReferenceFile(t, fs, name, "first\n")

	// Another replica moves the reference after this one fetched it. An Open
	// that means to write must see the move.
	if _, err := fs.Stat(name); err != nil {
		t.Fatalf("stat: %v", err)
	}
	fake.Put("prefix/"+name, []byte("moved elsewhere\n"))
	before := fake.Snapshot()
	file, err := fs.OpenFile(name, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open for writing: %v", err)
	}
	body, _ := io.ReadAll(file)
	_ = file.Close()
	if string(body) != "moved elsewhere\n" {
		t.Fatalf("a read made in order to write saw %q, not the store's current value", body)
	}
	if spent := fake.Snapshot().Sub(before); spent.Get != 1 {
		t.Fatalf("a read made in order to write did not reach the store: %s", spent)
	}

	// A local write discards bytes fetched before it.
	if _, err := fs.Stat(name); err != nil {
		t.Fatalf("stat: %v", err)
	}
	writeReferenceFile(t, fs, name, "second\n")
	if got := readAll(t, fs, name); got != "second\n" {
		t.Fatalf("a read after a local write saw %q", got)
	}

	// Uncollected bytes expire rather than wait for some later, unrelated read.
	if _, err := fs.Stat(name); err != nil {
		t.Fatalf("stat: %v", err)
	}
	shared := fs.shared()
	shared.mu.Lock()
	for _, handoff := range shared.handoffs {
		handoff.at = handoff.at.Add(-2 * referenceHandoffTTL)
	}
	shared.mu.Unlock()
	fake.Put("prefix/"+name, []byte("third\n"))
	if got := readAll(t, fs, name); got != "third\n" {
		t.Fatalf("an expired handoff was served: read %q", got)
	}

	// And a handoff is collected once: the second read is a fresh one.
	if _, err := fs.Stat(name); err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := readAll(t, fs, name); got != "third\n" {
		t.Fatalf("first read after stat = %q", got)
	}
	fake.Put("prefix/"+name, []byte("fourth\n"))
	if got := readAll(t, fs, name); got != "fourth\n" {
		t.Fatalf("a handoff was served twice: read %q", got)
	}
}

// TestOnlyReferenceFilesAreFetchedByAStat pins the scope: a Stat of anything
// else stays a HEAD, since downloading an object to learn that it exists would
// be the opposite of a saving.
func TestOnlyReferenceFilesAreFetchedByAStat(t *testing.T) {
	for name, want := range map[string]bool{
		"HEAD": true, "packed-refs": true, "refs/heads/main": true, "refs/tags/v1": true,
		"config": false, "objects/ab/cdef": false, "objects/pack/pack-1.pack": false, "refsx/heads/main": false,
	} {
		if got := isReferenceFile(name); got != want {
			t.Errorf("isReferenceFile(%q) = %v, want %v", name, got, want)
		}
	}
}

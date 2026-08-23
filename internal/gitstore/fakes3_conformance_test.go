package gitstore

import (
	"errors"
	"io"
	"os"
	"testing"
)

// TestFakeS3SatisfiesS3FS pins the measuring instrument against the code it
// measures. A counter is only worth reporting if the filesystem under it
// actually works, so every S3FS operation the git storage relies on is
// exercised here before any benchmark quotes a number from this fake.
func TestFakeS3SatisfiesS3FS(t *testing.T) {
	fake := newFakeS3(t)
	fs := fake.fs("bucket", "prefix")

	file, err := fs.Create("dir/one.txt")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := file.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	opened, err := fs.Open("dir/one.txt")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	body, err := io.ReadAll(opened)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("read back %q, want hello", body)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close read handle: %v", err)
	}

	info, err := fs.Stat("dir/one.txt")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 5 {
		t.Fatalf("stat size = %d, want 5", info.Size())
	}

	if _, err := fs.Stat("dir/missing.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat of a missing key = %v, want os.ErrNotExist", err)
	}
	if _, err := fs.Open("dir/missing.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open of a missing key = %v, want os.ErrNotExist", err)
	}

	entries, err := fs.ReadDir("dir")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "one.txt" {
		t.Fatalf("readdir = %v, want one.txt", entries)
	}

	if err := fs.Rename("dir/one.txt", "dir/two.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := fs.Stat("dir/one.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat of the renamed-away key = %v, want os.ErrNotExist", err)
	}
	if _, err := fs.Stat("dir/two.txt"); err != nil {
		t.Fatalf("stat of the rename destination: %v", err)
	}

	if err := fs.Remove("dir/two.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := fs.Stat("dir/two.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat after remove = %v, want os.ErrNotExist", err)
	}
}

// TestFakeS3PaginatesListings proves the fake's thousand-key page size, which
// is what makes a listing round-trip count comparable to the real service.
func TestFakeS3PaginatesListings(t *testing.T) {
	fake := newFakeS3(t)
	fs := fake.fs("bucket", "prefix")

	const objects = 2500
	for i := range objects {
		fake.put("prefix/many/"+padHex(i), []byte("x"))
	}

	fake.reset()
	entries, err := fs.ReadDir("many")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != objects {
		t.Fatalf("readdir returned %d entries, want %d", len(entries), objects)
	}
	counts := fake.snapshot()
	if counts.list != 3 {
		t.Fatalf("listing %d keys took %d requests, want 3 pages of 1000", objects, counts.list)
	}
}

func padHex(i int) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 8)
	for pos := 7; pos >= 0; pos-- {
		out[pos] = digits[i&0xf]
		i >>= 4
	}
	return string(out)
}

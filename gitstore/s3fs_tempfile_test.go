package gitstore

import (
	"strings"
	"testing"
	"time"
)

// TestATempFileReachesTheBucketOnlyUnderItsFinalName pins what git's
// write-then-rename costs. The temporary name exists to make the final one
// appear atomically on a disk; a PUT already is atomic, so the temporary key
// must never be written, copied or deleted.
func TestATempFileReachesTheBucketOnlyUnderItsFinalName(t *testing.T) {
	fake := newFakeS3(t)
	fs := fake.fs("bucket", "prefix")

	temp, err := fs.TempFile("objects/pack", "tmp_obj_")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if _, err := temp.Write([]byte("object bytes")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := temp.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if spent := fake.Snapshot(); spent.Total() != 0 {
		t.Fatalf("closing a temp file cost requests before it had a name: %s", spent)
	}

	// Closed but not yet renamed, it must still read back: git's pack writer
	// re-opens its temp file.
	if info, err := fs.Stat(temp.Name()); err != nil || info.Size() != int64(len("object bytes")) {
		t.Fatalf("stat parked temp: %v", err)
	}

	if err := fs.Rename(temp.Name(), "objects/ab/cdef"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	spent := fake.Snapshot()
	if spent.Put != 1 || spent.Copy != 0 || spent.Delete != 0 || spent.Total() != 1 {
		t.Fatalf("landing a temp file should cost exactly one PUT: %s", spent)
	}
	if body, ok := fake.Get("prefix/objects/ab/cdef"); !ok || string(body) != "object bytes" {
		t.Fatalf("final key holds %q", body)
	}
	for _, key := range fake.KeysWithPrefix("prefix/") {
		if strings.Contains(key, "tmp_obj_") {
			t.Fatalf("the temporary name reached the bucket: %s", key)
		}
	}
	if fs.activeFile(temp.Name()) != nil {
		t.Fatal("the staging entry outlived the rename")
	}
}

// TestAFailedLandingCanBeRetried pins that a rename which could not upload
// leaves the source in place, as a failed rename on a disk does.
func TestAFailedLandingCanBeRetried(t *testing.T) {
	fake := newFakeS3(t)
	fs := fake.fs("bucket", "prefix")
	temp, err := fs.TempFile("objects/pack", "tmp_obj_")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if _, err := temp.Write([]byte("object bytes")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := temp.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	fake.SetFailOn(func(method, _ string) bool { return method == "PUT" })
	if err := fs.Rename(temp.Name(), "objects/ab/cdef"); err == nil {
		t.Fatal("a rename whose upload failed reported success")
	}
	fake.SetFailOn(nil)
	if _, ok := fake.Get("prefix/objects/ab/cdef"); ok {
		t.Fatal("a failed rename left the destination behind")
	}
	if err := fs.Rename(temp.Name(), "objects/ab/cdef"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if body, ok := fake.Get("prefix/objects/ab/cdef"); !ok || string(body) != "object bytes" {
		t.Fatalf("final key holds %q after the retry", body)
	}
}

// TestDiscardingATempFileCostsNothing covers the path git takes when the object
// it was writing turns out to exist already.
func TestDiscardingATempFileCostsNothing(t *testing.T) {
	fake := newFakeS3(t)
	fs := fake.fs("bucket", "prefix")
	temp, err := fs.TempFile("objects/pack", "tmp_obj_")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if err := temp.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := fs.Remove(temp.Name()); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if spent := fake.Snapshot(); spent.Total() != 0 {
		t.Fatalf("discarding a temp file that never reached the bucket cost requests: %s", spent)
	}
	if fs.activeFile(temp.Name()) != nil {
		t.Fatal("the staging entry outlived the removal")
	}
}

// TestAnAbandonedTempFileIsSwept pins that a temp file whose writer failed
// before renaming it does not hold its bytes for the life of the process.
func TestAnAbandonedTempFileIsSwept(t *testing.T) {
	fake := newFakeS3(t)
	fs := fake.fs("bucket", "prefix")
	abandoned, err := fs.TempFile("objects/pack", "tmp_obj_")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if err := abandoned.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	state := fs.activeFile(abandoned.Name())
	if state == nil {
		t.Fatal("a closed temp file was not parked")
	}
	state.mu.Lock()
	state.parkedAt = state.parkedAt.Add(-2 * parkedTempRetention)
	state.mu.Unlock()

	next, err := fs.TempFile("objects/pack", "tmp_obj_")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if err := next.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if fs.activeFile(abandoned.Name()) != nil {
		t.Fatal("an abandoned temp file was not swept")
	}
	if got := fs.parkedTemp(next.Name()); got == nil || time.Since(got.parkedAt) > time.Minute {
		t.Fatal("the sweep dropped a temp file that was only just parked")
	}
}

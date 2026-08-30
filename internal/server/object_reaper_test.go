package bleephub

import (
	"context"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// TestObjectReaperReclaimsOrphansSafely pins the reaper's safety contract: it
// deletes an orphan under a reapable prefix, never deletes a live object, never
// touches a prefix whose live-set it can't enumerate (logs), and never sweeps an
// object younger than the grace period.
func TestObjectReaperReclaimsOrphansSafely(t *testing.T) {
	fs := newS3FSForTest(t)
	objectFS := deriveS3FSForTest(t, fs.Bucket(), "objects")
	byteStore := &store.S3ActionsByteStore{Fs: objectFS}
	s := newTestServer()
	s.setArtifactStore(store.NewArtifactStoreWithByteStore("", byteStore))
	s.store.ObjectByteStore = byteStore
	ctx := context.Background()

	// A live artifact: metadata in the store, bytes under its key.
	s.artifactStore.Mu.Lock()
	s.artifactStore.Artifacts[1] = &store.Artifact{ID: 1, Name: "live", Size: 4, Finalized: true, RepoFullName: "octo/repo"}
	s.artifactStore.Mu.Unlock()
	mustPut(t, byteStore, store.ArtifactDataKey(1), "live")
	// An orphan under a reapable prefix (no metadata references it).
	mustPut(t, byteStore, store.ArtifactDataKey(999), "orphan")
	// An orphan under a NON-reapable prefix (logs — live-set not enumerable).
	mustPut(t, byteStore, store.LogDataKey(5), "logbytes")

	// Freeze "now" 48h ahead so every object is comfortably past a 24h grace.
	future := time.Now().Add(48 * time.Hour).UTC()
	s.store.ClockNow = func() time.Time { return future }

	// Grace protects everything when nothing is old enough yet.
	young, err := s.store.ReapOrphanObjects(ctx, store.ReapOptions{Delete: true, GracePeriod: 72 * time.Hour})
	if err != nil {
		t.Fatalf("reap (young): %v", err)
	}
	if young.DeletedCount != 0 {
		t.Fatalf("grace should protect all objects, deleted %d", young.DeletedCount)
	}

	// With a 24h grace and now 48h ahead, the reapable orphan is swept.
	report, err := s.store.ReapOrphanObjects(ctx, store.ReapOptions{Delete: true, GracePeriod: 24 * time.Hour})
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if report.DeletedCount != 1 {
		t.Fatalf("deleted %d orphans, want exactly 1 (the reapable one); report=%+v", report.DeletedCount, report)
	}

	// The live object survived.
	if raw, err := byteStore.Get(ctx, store.ArtifactDataKey(1)); err != nil || string(raw) != "live" {
		t.Fatalf("live artifact was destroyed: raw=%q err=%v", raw, err)
	}
	// The logs orphan (non-enumerable prefix) was NOT touched.
	if raw, err := byteStore.Get(ctx, store.LogDataKey(5)); err != nil || string(raw) != "logbytes" {
		t.Fatalf("reaper deleted a log object it cannot reason about: raw=%q err=%v", raw, err)
	}
	// The reapable orphan is gone.
	if _, err := byteStore.Get(ctx, store.ArtifactDataKey(999)); err == nil {
		t.Fatalf("reapable orphan should have been deleted")
	}
}

func mustPut(t *testing.T, bs store.ActionsByteStore, key, data string) {
	t.Helper()
	if err := bs.Put(context.Background(), key, []byte(data)); err != nil {
		t.Fatalf("seed object %s: %v", key, err)
	}
}

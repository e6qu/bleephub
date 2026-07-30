package bleephub

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type blockingDeleteByteStore struct {
	started chan string
	release chan struct{}
}

type recordingDeleteByteStore struct {
	deleted map[string]bool
}

func (s *recordingDeleteByteStore) Put(context.Context, string, []byte) error { return nil }
func (s *recordingDeleteByteStore) Get(context.Context, string) ([]byte, error) {
	return nil, os.ErrNotExist
}
func (s *recordingDeleteByteStore) Delete(_ context.Context, key string) error {
	s.deleted[key] = true
	return nil
}

func (s *blockingDeleteByteStore) Put(context.Context, string, []byte) error { return nil }
func (s *blockingDeleteByteStore) Get(context.Context, string) ([]byte, error) {
	return nil, os.ErrNotExist
}
func (s *blockingDeleteByteStore) Delete(_ context.Context, key string) error {
	s.started <- key
	<-s.release
	return nil
}

func TestDeleteRepoExternalCleanupDoesNotHoldStoreLock(t *testing.T) {
	st := NewStore()
	st.SeedDefaultUser()
	admin := st.UsersByLogin["admin"]
	repo := st.CreateRepo(admin, "cleanup-lock", "", false)
	if repo == nil {
		t.Fatal("create repository")
	}
	bytes := &blockingDeleteByteStore{started: make(chan string, 1), release: make(chan struct{})}
	st.ObjectByteStore = bytes
	st.mu.Lock()
	st.Attestations[1] = &Attestation{ID: 1, RepoID: repo.ID, StoragePath: "attestations/cleanup-lock"}
	st.mu.Unlock()

	deleted := make(chan error, 1)
	go func() {
		_, err := st.DeleteRepo("admin", repo.Name)
		deleted <- err
	}()

	select {
	case key := <-bytes.started:
		if key != "attestations/cleanup-lock" {
			t.Fatalf("cleanup object = %q", key)
		}
	case <-time.After(time.Second):
		t.Fatal("repository deletion never reached external cleanup")
	}

	readFinished := make(chan struct{})
	go func() {
		_ = st.LookupUserByLogin("admin")
		close(readFinished)
	}()
	select {
	case <-readFinished:
	case <-time.After(time.Second):
		t.Fatal("external cleanup retained the global store lock")
	}

	close(bytes.release)
	if err := <-deleted; err != nil {
		t.Fatalf("delete repository: %v", err)
	}
}

func TestInterruptedRepoDeleteReplaysRecordedExternalCleanup(t *testing.T) {
	dataDir := t.TempDir()
	orphan := filepath.Join(t.TempDir(), "package-object")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatalf("seed external file: %v", err)
	}

	p1 := openTestPersistence(t, dataDir)
	if err := p1.Put(pendingDeletionsBucket, pendingRepoDeletionKey("admin/interrupted"), pendingDeletion{
		Kind:       "repo",
		Name:       "admin/interrupted",
		StartedAt:  fixedTestTime,
		LocalFiles: []string{orphan},
	}); err != nil {
		t.Fatalf("record deletion intent: %v", err)
	}
	if err := p1.Close(); err != nil {
		t.Fatalf("close first persistence: %v", err)
	}

	p2 := openTestPersistence(t, dataDir)
	defer func() { _ = p2.Close() }()
	st := NewStore()
	if err := st.SetPersistence(p2); err != nil {
		t.Fatalf("resume deletion: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("recorded external file survived replay: %v", err)
	}
	rows, err := p2.List(pendingDeletionsBucket)
	if err != nil {
		t.Fatalf("list deletion intents: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("completed deletion intent survived: %#v", rows)
	}
}

func TestDeleteRepoAtomicallyIncludesActionsArtifactsAndCaches(t *testing.T) {
	s := newTestServer()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "actions-cleanup", "", false)
	if repo == nil {
		t.Fatal("create repository")
	}
	bytes := &recordingDeleteByteStore{deleted: map[string]bool{}}
	s.artifactStore.byteStore = bytes
	s.artifactStore.mu.Lock()
	s.artifactStore.artifacts[41] = &Artifact{
		ID: 41, Name: "build", Finalized: true, RepoFullName: repo.FullName,
	}
	s.artifactStore.caches[42] = &CacheEntry{
		ID: 42, Repo: repo.FullName, Key: "deps", Version: "v1", Finalized: true,
	}
	s.artifactStore.cacheIndex[cacheLookupKey(repo.FullName, "deps", "v1")] = 42
	s.artifactStore.mu.Unlock()

	deleted, err := s.store.DeleteRepo("admin", repo.Name)
	if err != nil || !deleted {
		t.Fatalf("delete repository = %v, %v", deleted, err)
	}
	if !bytes.deleted[artifactDataKey(41)] || !bytes.deleted[cacheDataKey(42)] {
		t.Fatalf("Actions bytes not deleted: %#v", bytes.deleted)
	}
	s.artifactStore.mu.RLock()
	artifact := s.artifactStore.artifacts[41]
	cache := s.artifactStore.caches[42]
	_, indexed := s.artifactStore.cacheIndex[cacheLookupKey(repo.FullName, "deps", "v1")]
	s.artifactStore.mu.RUnlock()
	if artifact != nil || cache != nil || indexed {
		t.Fatalf("Actions metadata survived: artifact=%#v cache=%#v indexed=%v", artifact, cache, indexed)
	}
}

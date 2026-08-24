package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// The byte store a deployment without object storage falls back to.
//
// BLEEPHUB_OBJECT_S3_BUCKET is how a persistent, multi-replica deployment
// holds service bytes, and validatePersistentServerStorage still refuses to run
// one without it. But a single-process deployment has no such requirement, and
// the features that need somewhere to put bytes must still work: real GitHub
// always has Git LFS, so `git lfs push` against a freshly started server has to
// succeed rather than answer "object storage is not configured".
//
// This mirrors the precedent PackageDataDir set for package files: when a data
// directory is configured the bytes live under it, and when nothing is
// configured — the wholly in-memory development server — they live in the
// process, exactly like every other piece of that server's state.

// NewLocalByteStore returns the fallback byte store for a deployment with no
// object storage: filesystem-backed beneath dataDir when one is configured,
// process-memory-backed when it is not.
func NewLocalByteStore(dataDir string) ActionsByteStore {
	if dataDir == "" {
		return &MemoryByteStore{objects: map[string][]byte{}}
	}
	return &FilesystemByteStore{Root: filepath.Join(dataDir, "objects")}
}

// FilesystemByteStore keeps object bytes in a directory tree, one file per key.
// Like the S3 store it streams: an upload is written straight to a temporary
// file and renamed into place, and a download hands back the open file, so an
// object larger than the process's memory never lands on the heap (STORE-019).
type FilesystemByteStore struct {
	Root string `json:"-"`
}

// resolve maps a key to a path beneath Root, refusing any key that could climb
// out of it. Keys are server-generated (LFSObjectDataKey and friends build them
// from validated digests), so a rejected key is a programming error rather than
// a caller's input — but the check is what makes the join safe to read back.
func (s *FilesystemByteStore) resolve(key string) (string, error) {
	clean := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(key)), "/")
	if clean == "" {
		return "", fmt.Errorf("empty object key")
	}
	for _, segment := range strings.Split(clean, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("invalid object key %q", key)
		}
	}
	return filepath.Join(s.Root, filepath.FromSlash(clean)), nil
}

func (s *FilesystemByteStore) Put(ctx context.Context, key string, data []byte) error {
	return s.PutStream(ctx, key, bytes.NewReader(data))
}

func (s *FilesystemByteStore) PutStream(_ context.Context, key string, r io.Reader) error {
	path, err := s.resolve(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("object put %s: %w", key, err)
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return fmt.Errorf("object put %s: %w", key, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		// The copy already failed; the close can only add noise, and the
		// deferred Remove discards the partial file either way.
		_ = tmp.Close()
		return fmt.Errorf("object put %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("object put %s: %w", key, err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("object put %s: %w", key, err)
	}
	// Rename publishes the object atomically: a reader either sees the whole
	// object or no object, never a half-written one.
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("object put %s: %w", key, err)
	}
	return nil
}

func (s *FilesystemByteStore) Get(_ context.Context, key string) ([]byte, error) {
	path, err := s.resolve(key)
	if err != nil {
		return nil, err
	}
	// #nosec G304 -- resolve rejects every key that is not a plain relative
	// path, so the read stays beneath Root.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("object get %s: %w", key, err)
	}
	return data, nil
}

func (s *FilesystemByteStore) GetStream(_ context.Context, key string) (io.ReadCloser, error) {
	path, err := s.resolve(key)
	if err != nil {
		return nil, err
	}
	// #nosec G304 -- see Get: resolve constrains the path to Root.
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("object get %s: %w", key, err)
	}
	return file, nil
}

func (s *FilesystemByteStore) Delete(_ context.Context, key string) error {
	path, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("object delete %s: %w", key, err)
	}
	return nil
}

// MemoryByteStore keeps object bytes in the process. It is the fallback for a
// server with no data directory — the development default, whose git objects,
// repositories and metadata are already in memory and equally lost on restart.
type MemoryByteStore struct {
	mu      sync.RWMutex
	objects map[string][]byte
}

func (s *MemoryByteStore) Put(_ context.Context, key string, data []byte) error {
	stored := append([]byte(nil), data...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects == nil {
		s.objects = map[string][]byte{}
	}
	s.objects[key] = stored
	return nil
}

func (s *MemoryByteStore) PutStream(ctx context.Context, key string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("object put %s: %w", key, err)
	}
	return s.Put(ctx, key, data)
}

func (s *MemoryByteStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("object get %s: not found", key)
	}
	return append([]byte(nil), data...), nil
}

func (s *MemoryByteStore) GetStream(ctx context.Context, key string) (io.ReadCloser, error) {
	data, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *MemoryByteStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

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

// The byte store a deployment without object storage falls back to, so
// features that need somewhere to put bytes (Git LFS) still work on a
// single-process server. Filesystem-backed under a data directory, else
// process-memory-backed like the rest of the in-memory dev server's state.

// NewLocalByteStore returns the fallback byte store: filesystem-backed beneath
// dataDir when one is configured, process-memory-backed when it is not.
func NewLocalByteStore(dataDir string) ActionsByteStore {
	if dataDir == "" {
		return &MemoryByteStore{objects: map[string][]byte{}}
	}
	return &FilesystemByteStore{Root: filepath.Join(dataDir, "objects")}
}

// FilesystemByteStore keeps object bytes in a directory tree, one file per key.
// It streams uploads through a temp file renamed into place and hands back the
// open file on download, so an oversized object never lands on the heap
// (STORE-019).
type FilesystemByteStore struct {
	Root string `json:"-"`
}

// resolve maps a key to a path beneath Root, refusing any key that could climb
// out of it. Keys are server-generated, so a rejection is a programming error —
// but the check is what makes the join safe.
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
	joined := filepath.Join(s.Root, filepath.FromSlash(clean))
	// Containment guard: the joined path must stay strictly beneath Root. The
	// segment check above already rejects traversal; restating it here makes the
	// join provably safe and satisfies path-injection analysis.
	root := filepath.Clean(s.Root)
	if joined != root && !strings.HasPrefix(joined, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid object key %q", key)
	}
	return joined, nil
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
		// Copy already failed; the deferred Remove discards the partial file.
		_ = tmp.Close()
		return fmt.Errorf("object put %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("object put %s: %w", key, err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("object put %s: %w", key, err)
	}
	// Rename publishes atomically: a reader sees the whole object or none.
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("object put %s: %w", key, err)
	}
	return nil
}

// PutStreamHashed streams r to disk like PutStream; the filesystem store keeps
// no per-object checksum (Get does not verify), so size and sha256Sum are unused.
func (s *FilesystemByteStore) PutStreamHashed(ctx context.Context, key string, r io.Reader, _ int64, _ []byte) error {
	return s.PutStream(ctx, key, r)
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
// server with no data directory, whose other state is already in memory and
// equally lost on restart.
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

func (s *MemoryByteStore) PutStreamHashed(ctx context.Context, key string, r io.Reader, _ int64, _ []byte) error {
	return s.PutStream(ctx, key, r)
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

package bleephub

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"testing"
)

// flakyByteStore is an actionsByteStore that fails the failOn-th Put and
// records the blobs that survive, so a test can assert cleanup behaviour.
type flakyByteStore struct {
	blobs  map[string][]byte
	failOn int
	puts   int
}

func (m *flakyByteStore) Put(_ context.Context, key string, data []byte) error {
	m.puts++
	if m.puts == m.failOn {
		return errors.New("simulated object-store write failure")
	}
	if m.blobs == nil {
		m.blobs = map[string][]byte{}
	}
	m.blobs[key] = append([]byte(nil), data...)
	return nil
}

func (m *flakyByteStore) Get(_ context.Context, key string) ([]byte, error) { return m.blobs[key], nil }
func (m *flakyByteStore) PutStream(ctx context.Context, key string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return m.Put(ctx, key, data)
}
func (m *flakyByteStore) GetStream(_ context.Context, key string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m.blobs[key])), nil
}
func (m *flakyByteStore) Delete(_ context.Context, key string) error {
	delete(m.blobs, key)
	return nil
}

// TestCreatePackageVersionCleansUpBlobsOnPartialWriteFailure pins STORE-026: a
// multi-file version writes each file's bytes before any metadata is committed,
// so a mid-upload write failure must delete the bytes already written — leaving
// no orphaned blob that nothing references and nothing ever reclaims.
func TestCreatePackageVersionCleansUpBlobsOnPartialWriteFailure(t *testing.T) {
	t.Parallel()
	st := NewStore()
	st.SeedDefaultUser()
	if _, ok := st.CreatePackage("User", "admin", "npm", "widget", "private"); !ok {
		t.Fatal("create package")
	}
	bs := &flakyByteStore{failOn: 2} // the second file's byte write fails
	st.ObjectByteStore = bs

	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	_, err := st.CreatePackageVersion("User", "admin", "npm", "widget", "1.0.0", "", nil, []PackageFileInput{
		{Name: "a.tgz", ContentType: "application/octet-stream", ContentBase64: b64("first")},
		{Name: "b.tgz", ContentType: "application/octet-stream", ContentBase64: b64("second")},
	})
	if err == nil {
		t.Fatal("expected the second file's write to fail")
	}

	// The first file's bytes must have been reclaimed, not orphaned.
	if len(bs.blobs) != 0 {
		t.Fatalf("partial-write failure left %d orphaned blob(s), want 0", len(bs.blobs))
	}
	// No version or file metadata may have been committed either.
	if len(st.PackageVersions) != 0 || len(st.PackageFiles) != 0 {
		t.Fatalf("committed metadata despite failed upload: %d versions, %d files",
			len(st.PackageVersions), len(st.PackageFiles))
	}
}

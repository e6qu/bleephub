package store

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"
)

// TestUpdateReleaseAssetRejectsDuplicateName pins that renaming a release asset
// to a name another asset on the same release already has is refused, the same
// invariant the upload path enforces. A blind rename previously produced two
// assets with one name (and one browser_download_url).
func TestUpdateReleaseAssetRejectsDuplicateName(t *testing.T) {
	st := NewStore()
	rel := st.Releases.Create(1, 1, "v1", "", "V1", "", false, false, false)
	if rel == nil {
		t.Fatal("create release failed")
	}
	data := []byte("payload")
	sum := sha256.Sum256(data)
	mk := func(name string) *ReleaseAsset {
		a, err := st.Releases.CreateReleaseAssetStream(rel.ID, 1, name, "", "application/zip", bytes.NewReader(data), int64(len(data)), sum[:])
		if err != nil {
			t.Fatalf("create asset %s: %v", name, err)
		}
		return a
	}
	mk("a.zip")
	b := mk("b.zip")

	if _, err := st.Releases.UpdateReleaseAsset(b.ID, "a.zip", ""); !errors.Is(err, ErrReleaseAssetNameExists) {
		t.Fatalf("rename to a taken name: err = %v, want ErrReleaseAssetNameExists", err)
	}
	// A rename to a free name still works, and a no-op name keeps the asset.
	updated, err := st.Releases.UpdateReleaseAsset(b.ID, "c.zip", "new label")
	if err != nil || updated == nil || updated.Name != "c.zip" || updated.Label != "new label" {
		t.Fatalf("rename to a free name: got (%+v, %v)", updated, err)
	}
}

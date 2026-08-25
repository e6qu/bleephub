package bleephub

import (
	"testing"
)

// TestReleaseGetsAreDetached pins STORE-021 for the release store: every
// single-object getter and every List* must hand back a snapshot, or a reader
// rewrites persisted state by touching the value it was given.
func TestReleaseGetsAreDetached(t *testing.T) {
	s := newTestServer()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "release-detach", "", false)

	rel := s.store.Releases.Create(repo.ID, admin.ID, "v1.0.0", "main", "One", "notes", false, false, false)
	asset, err := s.store.Releases.CreateReleaseAsset(rel.ID, admin.ID, "bin.tar.gz", "label", "application/gzip", []byte("bytes"))
	if err != nil {
		t.Fatal(err)
	}

	got := s.store.Releases.Get(rel.ID)
	got.TagName = "v9.9.9"
	got.Draft = true
	got.Assets[0].Name = "rewritten"

	fresh := s.store.Releases.Get(rel.ID)
	if fresh.TagName != "v1.0.0" || fresh.Draft {
		t.Fatalf("release mutated through Get: tag=%q draft=%v", fresh.TagName, fresh.Draft)
	}
	if fresh.Assets[0].Name != "bin.tar.gz" {
		t.Fatalf("release asset mutated through Get: %q", fresh.Assets[0].Name)
	}

	s.store.Releases.GetByTag(repo.ID, "v1.0.0").TagName = "by-tag"
	s.store.Releases.Latest(repo.ID).TagName = "latest"
	s.store.Releases.List(repo.ID)[0].TagName = "listed"
	if fresh := s.store.Releases.Get(rel.ID); fresh.TagName != "v1.0.0" {
		t.Fatalf("release mutated through GetByTag/Latest/List: %q", fresh.TagName)
	}

	s.store.Releases.GetReleaseAsset(asset.ID).Name = "by-id"
	s.store.Releases.ListReleaseAssets(rel.ID)[0].Name = "listed"
	if fresh := s.store.Releases.GetReleaseAsset(asset.ID); fresh.Name != "bin.tar.gz" {
		t.Fatalf("asset mutated through GetReleaseAsset/ListReleaseAssets: %q", fresh.Name)
	}
}

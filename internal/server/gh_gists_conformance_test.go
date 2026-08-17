package bleephub

import (
	"testing"
)

// TestGistRenamePreservesContentAndRemovesOldFile covers REST gist rename: a
// PATCH that only sets a new `filename` (no content) renames the file, preserves
// its body, and does not leave the old file behind.
func TestGistRenamePreservesContentAndRemovesOldFile(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	created := s.createTestGist(t, defaultToken, true)
	id := created["id"].(string)

	patched := decodeJSONWithStatus(t, s.patch(t, "/api/v3/gists/"+id, defaultToken, map[string]interface{}{
		"files": map[string]interface{}{
			"hello.go": map[string]interface{}{"filename": "renamed.go"},
		},
	}), 200)
	files, _ := patched["files"].(map[string]interface{})
	if _, stillThere := files["hello.go"]; stillThere {
		t.Errorf("old filename hello.go survived the rename: %v", files)
	}
	renamed, ok := files["renamed.go"].(map[string]interface{})
	if !ok {
		t.Fatalf("renamed file missing: %v", files)
	}
	if renamed["content"] != "package main\n\nfunc main() {}" {
		t.Errorf("rename dropped the file body: %v", renamed["content"])
	}
}

// TestUserGistsExcludesSecret covers that GET /users/{username}/gists returns
// only public gists — a user's secret gists must not leak there.
func TestUserGistsExcludesSecret(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	pub := s.createTestGist(t, defaultToken, true)
	secret := s.createTestGist(t, defaultToken, false)

	list := decodeJSONArray(t, s.get(t, "/api/v3/users/admin/gists", defaultToken))
	ids := map[string]bool{}
	for _, g := range list {
		ids[g["id"].(string)] = true
	}
	if !ids[pub["id"].(string)] {
		t.Errorf("public gist missing from /users/admin/gists")
	}
	if ids[secret["id"].(string)] {
		t.Errorf("secret gist leaked in /users/admin/gists")
	}
}

// TestGistRevisionReturnsHistoricalContent covers GET /gists/{id}/{sha}: it
// returns the gist as it existed at that revision, not today's files.
func TestGistRevisionReturnsHistoricalContent(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	created := s.createTestGist(t, defaultToken, true)
	id := created["id"].(string)
	firstFiles, _ := created["files"].(map[string]interface{})
	origHello, _ := firstFiles["hello.go"].(map[string]interface{})
	origContent := origHello["content"].(string)

	created2 := decodeJSONWithStatus(t, s.patch(t, "/api/v3/gists/"+id, defaultToken, map[string]interface{}{
		"files": map[string]interface{}{"hello.go": map[string]interface{}{"content": "package main // v2"}},
	}), 200)
	history, _ := created2["history"].([]interface{})
	if len(history) < 2 {
		t.Fatalf("expected >=2 history entries, got %d", len(history))
	}
	firstRev := history[0].(map[string]interface{})["version"].(string) // oldest first (the create revision)

	resp := s.get(t, "/api/v3/gists/"+id+"/"+firstRev, defaultToken)
	rev := decodeJSONWithStatus(t, resp, 200)
	revFiles, _ := rev["files"].(map[string]interface{})
	revHello, _ := revFiles["hello.go"].(map[string]interface{})
	if revHello["content"] != origContent {
		t.Errorf("revision content = %v, want the historical %q", revHello["content"], origContent)
	}
}

// TestGistForksAndForkOf covers that a fork carries a `fork_of` parent object and
// the parent's `forks` array lists the fork.
func TestGistForksAndForkOf(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	parent := s.createTestGist(t, defaultToken, true)
	parentID := parent["id"].(string)

	fork := decodeJSONWithStatus(t, s.post(t, "/api/v3/gists/"+parentID+"/forks", defaultToken, map[string]interface{}{}), 201)
	forkID := fork["id"].(string)

	// The fork's single-gist GET carries fork_of pointing at the parent.
	forkGet := decodeJSONWithStatus(t, s.get(t, "/api/v3/gists/"+forkID, defaultToken), 200)
	forkOf, ok := forkGet["fork_of"].(map[string]interface{})
	if !ok || forkOf["id"] != parentID {
		t.Errorf("fork fork_of = %v, want parent %s", forkGet["fork_of"], parentID)
	}

	// The parent lists the fork in its forks array.
	parentGet := decodeJSONWithStatus(t, s.get(t, "/api/v3/gists/"+parentID, defaultToken), 200)
	forks, _ := parentGet["forks"].([]interface{})
	found := false
	for _, f := range forks {
		if f.(map[string]interface{})["id"] == forkID {
			found = true
		}
	}
	if !found {
		t.Errorf("parent forks array %v does not list fork %s", forks, forkID)
	}
}

// TestGistFileTruncatedOnSingleGet covers that a single-gist GET emits the
// per-file `truncated` member (gist-simple), and the list omits it (base-gist).
func TestGistFileTruncatedOnSingleGet(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	created := s.createTestGist(t, defaultToken, true)
	id := created["id"].(string)

	single := decodeJSONWithStatus(t, s.get(t, "/api/v3/gists/"+id, defaultToken), 200)
	sf := single["files"].(map[string]interface{})["hello.go"].(map[string]interface{})
	if _, ok := sf["truncated"]; !ok {
		t.Errorf("single-gist file missing truncated: %v", sf)
	}

	list := decodeJSONArray(t, s.get(t, "/api/v3/gists", defaultToken))
	if len(list) == 0 {
		t.Fatal("expected at least one gist in the list")
	}
	lf := list[0]["files"].(map[string]interface{})["hello.go"].(map[string]interface{})
	if _, ok := lf["truncated"]; ok {
		t.Errorf("list gist file must not carry truncated (base-gist): %v", lf)
	}
}

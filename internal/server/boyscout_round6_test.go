package bleephub

import (
	"encoding/json"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/e6qu/bleephub/internal/store"
)

// TestThreeWayMergeDropsDeletedFiles pins the fix for a three-way merge that
// resurrected deleted files: a path absent from a side means that side deleted
// it, but the merge kept the base entry. It backs PUT .../merge, POST .../merges
// and the PR test-merge sha, so a resurrected file is a silently wrong merge.
func TestThreeWayMergeDropsDeletedFiles(t *testing.T) {
	entry := func(hex string) object.TreeEntry {
		return object.TreeEntry{Mode: filemode.Regular, Hash: plumbing.NewHash(hex)}
	}
	hA := "1111111111111111111111111111111111111111"
	hB := "2222222222222222222222222222222222222222"

	base := map[string]object.TreeEntry{
		"keep.txt":       entry(hA),
		"ours-del.txt":   entry(hA),
		"theirs-del.txt": entry(hA),
		"both-del.txt":   entry(hA),
	}
	ours := map[string]object.TreeEntry{
		"keep.txt":       entry(hA),
		"theirs-del.txt": entry(hA), // ours keeps it unchanged
	}
	theirs := map[string]object.TreeEntry{
		"keep.txt":     entry(hA),
		"ours-del.txt": entry(hA), // theirs keeps it unchanged
	}

	result, err := threeWayMergePaths(base, ours, theirs)
	if err != nil {
		t.Fatalf("merge returned conflict unexpectedly: %v", err)
	}
	if _, ok := result["keep.txt"]; !ok {
		t.Error("keep.txt (unchanged both sides) was dropped")
	}
	for _, deleted := range []string{"ours-del.txt", "theirs-del.txt", "both-del.txt"} {
		if _, ok := result[deleted]; ok {
			t.Errorf("%s was deleted on a side but resurrected in the merge", deleted)
		}
	}

	// A delete on one side with a modify on the other stays a conflict.
	if _, err := threeWayMergePaths(
		map[string]object.TreeEntry{"f.txt": entry(hA)},
		map[string]object.TreeEntry{},                   // ours deleted f.txt
		map[string]object.TreeEntry{"f.txt": entry(hB)}, // theirs modified it
	); err == nil {
		t.Error("delete/modify on the same path must be a conflict")
	}
}

// TestListCollaboratorsRoleNameUsesGitHubVocabulary pins that the collaborators
// list reports role_name in GitHub's read/write/admin vocabulary, not the
// internal pull/push/admin store perm.
func TestListCollaboratorsRoleNameUsesGitHubVocabulary(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "collab-roles"})
	writer, _ := s.newUser(t, "collab-writer")
	if !s.store.AddRepoCollaborator("admin", "collab-roles", writer.Login, "push") {
		t.Fatal("AddRepoCollaborator failed")
	}

	resp := s.get(t, "/api/v3/repos/admin/collab-roles/collaborators", defaultToken)
	defer resp.Body.Close()
	var collabs []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&collabs); err != nil {
		t.Fatal(err)
	}
	var got string
	for _, c := range collabs {
		if c["login"] == "collab-writer" {
			got, _ = c["role_name"].(string)
		}
	}
	if got != "write" {
		t.Fatalf("push collaborator role_name = %q, want \"write\"", got)
	}
}

// TestCompareCommitsAreOldestFirst pins that the compare API lists commits
// oldest-first (like GitHub and the .patch builder), not the newest-first order
// CommitsBetween returns.
func TestCompareCommitsAreOldestFirst(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "compare-order"})
	stor := s.store.GetGitStorage("admin", "compare-order")
	if stor == nil {
		t.Fatal("no git storage")
	}
	sig := repoSignature("t", "t@t")
	base, err := initRepoWithFiles(stor, "main", "init", map[string]string{"README.md": "# hi"}, sig)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), base)); err != nil {
		t.Fatalf("feature ref: %v", err)
	}
	h1, err := createFileCommit(stor, "feature", "a.txt", "1\n", "first", sig)
	if err != nil {
		t.Fatalf("commit 1: %v", err)
	}
	if _, err := createFileCommit(stor, "feature", "b.txt", "2\n", "second", sig); err != nil {
		t.Fatalf("commit 2: %v", err)
	}
	h3, err := createFileCommit(stor, "feature", "c.txt", "3\n", "third", sig)
	if err != nil {
		t.Fatalf("commit 3: %v", err)
	}

	resp := s.get(t, "/api/v3/repos/admin/compare-order/compare/main...feature", defaultToken)
	defer resp.Body.Close()
	var got map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	commits, _ := got["commits"].([]interface{})
	if len(commits) != 3 {
		t.Fatalf("commits = %d, want 3", len(commits))
	}
	first := commits[0].(map[string]interface{})
	last := commits[2].(map[string]interface{})
	if first["sha"] != h1.String() {
		t.Fatalf("commits[0].sha = %v, want oldest %s", first["sha"], h1.String())
	}
	if last["sha"] != h3.String() {
		t.Fatalf("commits[2].sha = %v, want newest %s", last["sha"], h3.String())
	}
}

// TestForkPullRequestTokenIsReadOnly pins that a fork-authored pull_request run
// receives a read-only GITHUB_TOKEN by default, even when the workflow declares
// write permissions — outside-contributor code must not get a write token.
func TestForkPullRequestTokenIsReadOnly(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)

	forkPayload := map[string]interface{}{
		"pull_request": map[string]interface{}{
			"head": map[string]interface{}{
				"repo": map[string]interface{}{"full_name": "attacker/fork"},
			},
		},
	}
	writePerms := store.PermissionDef{"contents": "write", "pull-requests": "write"}

	forkWf := &store.Workflow{
		RepoFullName: "octo/base",
		EventName:    "pull_request",
		EventPayload: forkPayload,
		Permissions:  writePerms,
	}
	perms := s.resolveJobTokenPermissions(forkWf, nil)
	if perms["contents"] != "read" || perms["pull_requests"] != "read" {
		t.Fatalf("fork PR token perms = %v, want contents/pull_requests clamped to read", perms)
	}

	// A same-repo (non-fork) pull_request keeps its declared write level.
	samePayload := map[string]interface{}{
		"pull_request": map[string]interface{}{
			"head": map[string]interface{}{
				"repo": map[string]interface{}{"full_name": "octo/base"},
			},
		},
	}
	sameWf := &store.Workflow{
		RepoFullName: "octo/base",
		EventName:    "pull_request",
		EventPayload: samePayload,
		Permissions:  writePerms,
	}
	if perms := s.resolveJobTokenPermissions(sameWf, nil); perms["contents"] != "write" {
		t.Fatalf("same-repo PR token contents = %q, want write (must not clamp)", perms["contents"])
	}
}

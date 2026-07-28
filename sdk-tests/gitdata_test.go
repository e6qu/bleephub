package sdktests

import (
	"testing"

	github "github.com/google/go-github/v88/github"
)

func createSDKCommit(t *testing.T, repoName, message, path, content string, parents []*github.Commit) *github.Commit {
	t.Helper()
	tree, _, err := client.Git.CreateTree(ctx(), "admin", repoName, "", []*github.TreeEntry{{
		Path:    github.Ptr(path),
		Mode:    github.Ptr("100644"),
		Type:    github.Ptr("blob"),
		Content: github.Ptr(content),
	}})
	if err != nil {
		t.Fatalf("Git.CreateTree(%s): %v", path, err)
	}
	commit, _, err := client.Git.CreateCommit(ctx(), "admin", repoName, github.Commit{
		Message: github.Ptr(message),
		Tree:    tree,
		Parents: parents,
		Author: &github.CommitAuthor{
			Name:  github.Ptr("SDK Test"),
			Email: github.Ptr("sdk@example.test"),
		},
		Committer: &github.CommitAuthor{
			Name:  github.Ptr("SDK Test"),
			Email: github.Ptr("sdk@example.test"),
		},
	}, nil)
	if err != nil {
		t.Fatalf("Git.CreateCommit(%s): %v", message, err)
	}
	if commit.GetSHA() == "" {
		t.Fatalf("Git.CreateCommit(%s) returned empty SHA", message)
	}
	return commit
}

func createSDKDefaultBranch(t *testing.T, repoName string) *github.Commit {
	t.Helper()
	commit := createSDKCommit(t, repoName, "initial commit", "README.md", "# "+repoName+"\n", nil)
	if _, _, err := client.Git.CreateRef(ctx(), "admin", repoName, github.CreateRef{
		Ref: "refs/heads/main",
		SHA: commit.GetSHA(),
	}); err != nil {
		t.Fatalf("Git.CreateRef(main): %v", err)
	}
	return commit
}

func createPullRequestBranches(t *testing.T, repoName string) (base, head *github.Commit) {
	t.Helper()
	base = createSDKDefaultBranch(t, repoName)

	head = createSDKCommit(t, repoName, "feature commit", "feature.txt", "feature\n", []*github.Commit{base})
	if _, _, err := client.Git.CreateRef(ctx(), "admin", repoName, github.CreateRef{
		Ref: "refs/heads/feature",
		SHA: head.GetSHA(),
	}); err != nil {
		t.Fatalf("Git.CreateRef(feature): %v", err)
	}
	return base, head
}

func TestGitData(t *testing.T) {
	name := uniqueName("git-data")
	createRepo(t, name)

	blob, _, err := client.Git.CreateBlob(ctx(), "admin", name, github.Blob{
		Content:  github.Ptr("hello from the SDK\n"),
		Encoding: github.Ptr("utf-8"),
	})
	if err != nil {
		t.Fatalf("Git.CreateBlob: %v", err)
	}
	if blob.GetSHA() == "" {
		t.Fatal("Git.CreateBlob returned empty SHA")
	}
	readBlob, _, err := client.Git.GetBlob(ctx(), "admin", name, blob.GetSHA())
	if err != nil {
		t.Fatalf("Git.GetBlob: %v", err)
	}
	if readBlob.GetSHA() != blob.GetSHA() || readBlob.GetNodeID() == "" || readBlob.GetURL() == "" {
		t.Fatalf("Git.GetBlob shape = %+v, want sha, node_id, and url", readBlob)
	}

	tree, _, err := client.Git.CreateTree(ctx(), "admin", name, "", []*github.TreeEntry{{
		Path: github.Ptr("hello.txt"),
		Mode: github.Ptr("100644"),
		Type: github.Ptr("blob"),
		SHA:  blob.SHA,
	}})
	if err != nil {
		t.Fatalf("Git.CreateTree: %v", err)
	}
	if tree.GetSHA() == "" {
		t.Fatal("Git.CreateTree returned empty SHA")
	}

	// Entries supplied with base_tree are request-order independent. The new
	// name sorts before hello.txt, which used to make go-git's encoder reject
	// Bleephub's unsorted merged tree with HTTP 500.
	merged, _, err := client.Git.CreateTree(ctx(), "admin", name, tree.GetSHA(), []*github.TreeEntry{{
		Path:    github.Ptr("a-first.txt"),
		Mode:    github.Ptr("100644"),
		Type:    github.Ptr("blob"),
		Content: github.Ptr("first\n"),
	}})
	if err != nil {
		t.Fatalf("Git.CreateTree(base_tree, unsorted input): %v", err)
	}
	if len(merged.Entries) != 2 ||
		merged.Entries[0].GetPath() != "a-first.txt" ||
		merged.Entries[1].GetPath() != "hello.txt" {
		t.Fatalf("merged tree entries = %+v, want [a-first.txt hello.txt]", merged.Entries)
	}

	commit, _, err := client.Git.CreateCommit(ctx(), "admin", name, github.Commit{
		Message: github.Ptr("add hello"),
		Tree:    tree,
		Author: &github.CommitAuthor{
			Name:  github.Ptr("SDK Test"),
			Email: github.Ptr("sdk@example.test"),
		},
		Committer: &github.CommitAuthor{
			Name:  github.Ptr("SDK Test"),
			Email: github.Ptr("sdk@example.test"),
		},
	}, nil)
	if err != nil {
		t.Fatalf("Git.CreateCommit: %v", err)
	}

	ref, _, err := client.Git.CreateRef(ctx(), "admin", name, github.CreateRef{
		Ref: "refs/heads/main",
		SHA: commit.GetSHA(),
	})
	if err != nil {
		t.Fatalf("Git.CreateRef: %v", err)
	}
	if ref.GetRef() != "refs/heads/main" || ref.GetObject().GetSHA() != commit.GetSHA() {
		t.Fatalf("created ref = %q %q, want refs/heads/main %s", ref.GetRef(), ref.GetObject().GetSHA(), commit.GetSHA())
	}
	if ref.GetURL() == "" || ref.GetObject().GetType() != "commit" || ref.GetObject().GetURL() == "" {
		t.Fatalf("created ref shape = %+v, want canonical ref and commit URLs", ref)
	}

	got, _, err := client.Git.GetRef(ctx(), "admin", name, "heads/main")
	if err != nil {
		t.Fatalf("Git.GetRef: %v", err)
	}
	if got.GetObject().GetSHA() != commit.GetSHA() {
		t.Fatalf("Git.GetRef sha = %q, want %s", got.GetObject().GetSHA(), commit.GetSHA())
	}

	for _, treeish := range []string{tree.GetSHA(), commit.GetSHA(), "main", "heads/main", "refs/heads/main"} {
		gotTree, _, err := client.Git.GetTree(ctx(), "admin", name, treeish, true)
		if err != nil {
			t.Fatalf("Git.GetTree(%q): %v", treeish, err)
		}
		wantSHA := commit.GetSHA()
		if treeish == tree.GetSHA() {
			wantSHA = tree.GetSHA()
		}
		if gotTree.GetSHA() != wantSHA || gotTree.GetTruncated() {
			t.Fatalf("Git.GetTree(%q) = %+v, want sha %s and not truncated", treeish, gotTree, wantSHA)
		}
		if len(gotTree.Entries) != 1 || gotTree.Entries[0].GetSize() == 0 || gotTree.Entries[0].GetURL() == "" {
			t.Fatalf("Git.GetTree(%q) entries = %+v, want blob size and URL", treeish, gotTree.Entries)
		}
	}

	if _, _, err := client.Git.CreateRef(ctx(), "admin", name, github.CreateRef{
		Ref: "refs/heads/feature/slash",
		SHA: commit.GetSHA(),
	}); err != nil {
		t.Fatalf("Git.CreateRef(feature/slash): %v", err)
	}
	if _, _, err := client.Git.GetTree(ctx(), "admin", name, "feature/slash", false); err != nil {
		t.Fatalf("Git.GetTree(feature/slash): %v", err)
	}

	tag, _, err := client.Git.CreateTag(ctx(), "admin", name, github.CreateTag{
		Tag:     "v-sdk",
		Message: "SDK annotated tag",
		Object:  commit.GetSHA(),
		Type:    "commit",
	})
	if err != nil {
		t.Fatalf("Git.CreateTag: %v", err)
	}
	if tag.GetVerification() == nil || tag.GetNodeID() == "" || tag.GetURL() == "" {
		t.Fatalf("Git.CreateTag shape = %+v, want verification, node_id, and url", tag)
	}
	if _, _, err := client.Git.CreateRef(ctx(), "admin", name, github.CreateRef{
		Ref: "refs/tags/v-sdk",
		SHA: tag.GetSHA(),
	}); err != nil {
		t.Fatalf("Git.CreateRef(v-sdk): %v", err)
	}
	tagRef, _, err := client.Git.GetRef(ctx(), "admin", name, "tags/v-sdk")
	if err != nil {
		t.Fatalf("Git.GetRef(tags/v-sdk): %v", err)
	}
	if tagRef.GetObject().GetType() != "tag" {
		t.Fatalf("annotated tag ref object type = %q, want tag", tagRef.GetObject().GetType())
	}
	tagTree, _, err := client.Git.GetTree(ctx(), "admin", name, "v-sdk", false)
	if err != nil {
		t.Fatalf("Git.GetTree(v-sdk): %v", err)
	}
	if tagTree.GetSHA() != commit.GetSHA() {
		t.Fatalf("Git.GetTree(v-sdk) sha = %q, want peeled commit %s", tagTree.GetSHA(), commit.GetSHA())
	}

	next := createSDKCommit(t, name, "advance main", "next.txt", "next\n", []*github.Commit{commit})
	updated, _, err := client.Git.UpdateRef(ctx(), "admin", name, "heads/main", github.UpdateRef{
		SHA: next.GetSHA(),
	})
	if err != nil {
		t.Fatalf("Git.UpdateRef: %v", err)
	}
	if updated.GetObject().GetSHA() != next.GetSHA() {
		t.Fatalf("Git.UpdateRef sha = %q, want %s", updated.GetObject().GetSHA(), next.GetSHA())
	}

	refs, _, err := client.Git.ListMatchingRefs(ctx(), "admin", name, "heads")
	if err != nil {
		t.Fatalf("Git.ListMatchingRefs: %v", err)
	}
	if len(refs) != 2 ||
		refs[0].GetRef() != "refs/heads/feature/slash" ||
		refs[1].GetRef() != "refs/heads/main" {
		t.Fatalf("matching refs = %+v, want feature/slash and main in order", refs)
	}
}

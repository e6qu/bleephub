package gitstore

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoGitDirPathRejectsNamespaceEscapes(t *testing.T) {
	root := t.TempDir()
	got, err := RepoGitDirPath(root, "octocat/hello-world")
	if err != nil {
		t.Fatalf("valid repository path rejected: %v", err)
	}
	want := filepath.Join(root, "octocat", "hello-world")
	if got != want {
		t.Fatalf("repository path = %q, want %q", got, want)
	}

	for _, fullName := range []string{
		"",
		"octocat",
		"octocat/repo/extra",
		"octocat//repo",
		"octocat/.",
		"octocat/..",
		"../octocat/repo",
		"/octocat/repo",
		`octocat\repo`,
	} {
		t.Run(strings.ReplaceAll(fullName, "/", "_"), func(t *testing.T) {
			if _, err := RepoGitDirPath(root, fullName); err == nil {
				t.Fatalf("unsafe repository storage name %q accepted", fullName)
			}
		})
	}
}

package bleephub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hermeticGitTestEnv is the environment every `git` subprocess in this package
// must run with.
//
// Without it a test git inherits whoever's machine it runs on. On macOS that
// specifically means credential.helper=osxkeychain, configured in BOTH
// /opt/homebrew/etc/gitconfig (system) and ~/.gitconfig (global), so the moment
// a test clone or push authenticates against the test server git shells out to
// git-credential-osxkeychain and reads the developer's login keychain — which
// can raise an interactive keychain prompt in the middle of an otherwise
// headless test run, and makes the suite's behaviour depend on personal config
// it has no business consulting. Setting HOME alone is not enough: it hides the
// global file but leaves the system one, and the system helper still runs.
//
//   - GIT_CONFIG_NOSYSTEM drops the system file.
//   - GIT_CONFIG_GLOBAL points the global file at the test's own temp dir
//     rather than ~/.gitconfig. It has to be a writable path and not /dev/null:
//     `git lfs install` writes the filter.lfs.* entries to the global file, and
//     against /dev/null it fails with "could not lock config file".
//   - HOME/XDG_CONFIG_HOME point anything else that looks for per-user state at
//     the test's own temp dir.
//   - GIT_TERMINAL_PROMPT/GIT_ASKPASS turn a credential the test forgot to
//     supply into an immediate failure instead of a hang or a prompt.
//
// No credential helper is configured by any of this, so a test that needs to
// authenticate passes its own with `-c credential.helper=...`; that inline
// helper becomes the only entry in the list.
func hermeticGitTestEnv(home string) []string {
	return []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
	}
}

// TestEveryGitSubprocessIsHermetic is a ratchet. Spawning `git` without
// hermeticGitTestEnv silently re-inherits the developer's ~/.gitconfig and the
// system gitconfig, which on macOS reintroduces credential.helper=osxkeychain
// and lets a test run reach into the login keychain. That failure is invisible
// on CI (no keychain, no personal config) and only ever bites the person
// running the suite locally, so it needs to be caught mechanically rather than
// in review.
func TestEveryGitSubprocessIsHermetic(t *testing.T) {
	t.Parallel()
	sources, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		if source == "git_testenv_test.go" {
			continue
		}
		raw, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		if !strings.Contains(body, `exec.Command("git"`) {
			continue
		}
		if !strings.Contains(body, "hermeticGitTestEnv") {
			t.Errorf("%s spawns git but never sets hermeticGitTestEnv: the subprocess "+
				"will inherit the developer's git configuration, including "+
				"credential.helper=osxkeychain on macOS", source)
		}
	}
}

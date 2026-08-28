package bleephub

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Every content-bearing REST response advertises a download_url/raw_url, and
// nothing used to serve that URL shape: each one was a dead 404. These tests
// fetch the link the API itself handed out rather than a hand-built path, so
// the two halves can never drift apart again.

func rawSeedFile(t *testing.T, s *isolatedServer, repo, path, content string) map[string]interface{} {
	t.Helper()
	resp := s.put(t, "/api/v3/repos/admin/"+repo+"/contents/"+path, defaultToken, map[string]interface{}{
		"message": "add " + path,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create %s = %d, want 201; body = %s", path, resp.StatusCode, body)
	}
	var out struct {
		Content map[string]interface{} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Content
}

func fetchAdvertisedRaw(t *testing.T, s *isolatedServer, rawURL, token string) *http.Response {
	t.Helper()
	// download_url is absolute against the server's own base URL; the isolated
	// listener answers on a different port, so keep only the path.
	idx := strings.Index(rawURL, "://")
	if idx < 0 {
		t.Fatalf("download_url is not absolute: %q", rawURL)
	}
	rest := rawURL[idx+3:]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		t.Fatalf("download_url has no path: %q", rawURL)
	}
	return s.get(t, rest[slash:], token)
}

func TestRawURLAdvertisedByContentsAPIServesTheFile(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp := s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "rawrepo"})
	resp.Body.Close()

	const body = "hello raw world\n"
	content := rawSeedFile(t, s, "rawrepo", "docs/README.md", body)
	downloadURL, _ := content["download_url"].(string)
	if downloadURL == "" {
		t.Fatal("contents response carried no download_url")
	}

	raw := fetchAdvertisedRaw(t, s, downloadURL, defaultToken)
	defer raw.Body.Close()
	got, _ := io.ReadAll(raw.Body)
	if raw.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body = %s", downloadURL, raw.StatusCode, got)
	}
	if string(got) != body {
		t.Fatalf("raw body = %q, want %q", got, body)
	}
	if ct := raw.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
	// A raw .html or .svg rendered in-origin would be stored XSS against any
	// viewer of an untrusted repository; github.com sends nosniff for exactly
	// this reason.
	if got := raw.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestRawServesBinaryAsOctetStream(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp := s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "rawbin"})
	resp.Body.Close()

	binary := string([]byte{0x00, 0x01, 0x02, 0xff})
	content := rawSeedFile(t, s, "rawbin", "blob.bin", binary)
	raw := fetchAdvertisedRaw(t, s, content["download_url"].(string), defaultToken)
	defer raw.Body.Close()
	got, _ := io.ReadAll(raw.Body)
	if raw.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", raw.StatusCode)
	}
	if string(got) != binary {
		t.Fatalf("raw body = %v, want %v", []byte(got), []byte(binary))
	}
	if ct := raw.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", ct)
	}
}

// The ref and the path are both slash-bearing and concatenated into one URL, so
// the split between them is genuinely ambiguous and has to be probed against
// the ref store.
func TestRawResolvesRefContainingSlashes(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp := s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "rawslash"})
	resp.Body.Close()
	content := rawSeedFile(t, s, "rawslash", "dir/file.txt", "on the branch\n")

	commitSHA := ""
	if c, ok := content["sha"].(string); ok {
		commitSHA = c
	}
	// The blob sha is not a commit; take the branch tip instead.
	head := s.get(t, "/api/v3/repos/admin/rawslash/commits/main", defaultToken)
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(head.Body).Decode(&commit); err != nil {
		t.Fatal(err)
	}
	head.Body.Close()
	if commit.SHA == "" {
		t.Fatalf("no head commit resolved (blob sha was %q)", commitSHA)
	}

	ref := s.post(t, "/api/v3/repos/admin/rawslash/git/refs", defaultToken, map[string]interface{}{
		"ref": "refs/heads/feature/deep/branch",
		"sha": commit.SHA,
	})
	if ref.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(ref.Body)
		ref.Body.Close()
		t.Fatalf("create ref = %d, want 201; body = %s", ref.StatusCode, body)
	}
	ref.Body.Close()

	raw := s.get(t, "/admin/rawslash/raw/feature/deep/branch/dir/file.txt", defaultToken)
	defer raw.Body.Close()
	got, _ := io.ReadAll(raw.Body)
	if raw.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", raw.StatusCode, got)
	}
	if string(got) != "on the branch\n" {
		t.Fatalf("raw body = %q", got)
	}
}

func TestRawRejectsUnreadableAndMissingTargets(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp := s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "rawpriv", "private": true,
	})
	resp.Body.Close()
	content := rawSeedFile(t, s, "rawpriv", "secret.txt", "classified\n")
	downloadURL := content["download_url"].(string)

	owner := fetchAdvertisedRaw(t, s, downloadURL, defaultToken)
	owner.Body.Close()
	if owner.StatusCode != http.StatusOK {
		t.Fatalf("owner status = %d, want 200", owner.StatusCode)
	}

	// An anonymous caller must not, and must not learn the repo exists.
	anon := fetchAdvertisedRaw(t, s, downloadURL, "")
	anon.Body.Close()
	if anon.StatusCode != http.StatusNotFound {
		t.Fatalf("anonymous status = %d, want 404", anon.StatusCode)
	}

	for _, path := range []string{
		"/admin/rawpriv/raw/main/no-such-file.txt",  // missing blob
		"/admin/rawpriv/raw/no-such-ref/secret.txt", // missing ref
		"/admin/nosuchrepo/raw/main/secret.txt",     // missing repo
	} {
		got := s.get(t, path, defaultToken)
		got.Body.Close()
		if got.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, got.StatusCode)
		}
	}
}

// A directory and a submodule gitlink have no raw byte representation; the
// gitlink in particular names a commit held in a different repository's object
// store, so serving it could only ever fail.
func TestRawRejectsDirectory(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp := s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "rawdir"})
	resp.Body.Close()
	rawSeedFile(t, s, "rawdir", "docs/README.md", "doc\n")

	got := s.get(t, "/admin/rawdir/raw/main/docs", defaultToken)
	got.Body.Close()
	if got.StatusCode != http.StatusNotFound {
		t.Fatalf("directory raw status = %d, want 404", got.StatusCode)
	}
}

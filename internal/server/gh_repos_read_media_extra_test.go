package bleephub

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// seedReadmeRepo creates a repo whose default branch holds a single root commit
// adding README.md, and returns the repo name and the head commit hash.
func seedReadmeRepo(t *testing.T, s *isolatedServer, name, message string) (string, plumbing.Hash) {
	t.Helper()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, name, "", false)
	stor := s.store.GetGitStorage("admin", repo.Name)

	readmeHash, err := storeBlob(stor, []byte("# Title\n\nhello world\n"))
	if err != nil {
		t.Fatal(err)
	}
	rootTree, err := storeTree(stor, []object.TreeEntry{
		{Name: "README.md", Mode: filemode.Regular, Hash: readmeHash},
	})
	if err != nil {
		t.Fatal(err)
	}
	head, err := storeCommit(stor, rootTree, plumbing.ZeroHash, message)
	if err != nil {
		t.Fatal(err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(repo.DefaultBranch), head)); err != nil {
		t.Fatal(err)
	}
	return repo.Name, head
}

// TestCommitPatchMediaTypeIsFormatPatch covers the residual where the .patch
// media type served the same tree diff as .diff. GitHub serves a real
// git-format-patch mbox: a "From <sha> Mon Sep 17…" line, From:/Date:/Subject:
// headers, then the diff and a "-- \n<version>" signature.
func TestCommitPatchMediaTypeIsFormatPatch(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name, head := seedReadmeRepo(t, s, "patch-media", "add readme")

	resp := s.semanticRequest(t, http.MethodGet, "/api/v3/repos/admin/"+name+"/commits/"+head.String(), "application/vnd.github.patch")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/vnd.github.patch") {
		t.Errorf("Content-Type = %q, want a vnd.github.patch type", ct)
	}
	patch := string(body)
	for _, want := range []string{
		"From " + head.String() + " Mon Sep 17 00:00:00 2001\n",
		"From: Test User <test@bleephub.local>\n",
		"Date: ",
		"Subject: [PATCH] add readme\n",
		"README.md",
		"\n---\n",
		"\n-- \n",
	} {
		if !strings.Contains(patch, want) {
			t.Errorf("format-patch body missing %q:\n%s", want, patch)
		}
	}
	if !strings.HasPrefix(patch, "From "+head.String()) {
		t.Errorf("format-patch must open with the mbox From line, got:\n%s", patch)
	}
}

// TestReadmeMediaTypesRawAndHtml covers the residual where /readme ignored the
// .raw and .html media types and only ever returned the base64 JSON object.
func TestReadmeMediaTypesRawAndHtml(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name, _ := seedReadmeRepo(t, s, "readme-media", "seed")

	raw := s.semanticRequest(t, http.MethodGet, "/api/v3/repos/admin/"+name+"/readme", "application/vnd.github.raw")
	rawBody, _ := io.ReadAll(raw.Body)
	raw.Body.Close()
	if raw.StatusCode != http.StatusOK || string(rawBody) != "# Title\n\nhello world\n" {
		t.Fatalf("raw readme = %d %q", raw.StatusCode, rawBody)
	}
	// A non-JSON body must not carry a +json content type, or the openapi-shape
	// ratchet would try to validate it as JSON.
	if ct := raw.Header.Get("Content-Type"); strings.Contains(ct, "+json") || ct == "application/json" {
		t.Errorf("raw readme Content-Type = %q, want a non-JSON type", ct)
	}

	html := s.semanticRequest(t, http.MethodGet, "/api/v3/repos/admin/"+name+"/readme", "application/vnd.github.html")
	htmlBody, _ := io.ReadAll(html.Body)
	html.Body.Close()
	if html.StatusCode != http.StatusOK {
		t.Fatalf("html readme status = %d", html.StatusCode)
	}
	if ct := html.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("html readme Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(string(htmlBody), "hello world") {
		t.Errorf("html readme body did not render the content: %q", htmlBody)
	}
}

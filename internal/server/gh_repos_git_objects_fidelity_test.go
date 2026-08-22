package bleephub

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// Fidelity fixes on the git object / ref REST surface, each pinned against
// behaviour observed on api.github.com (the probe is named in the comment).

// seedSubmoduleRepo builds a repository whose tree carries a real gitlink
// (mode 160000) at vendor/lib, pointing at a commit that lives in no
// repository this server holds — the exact shape that made archives 500.
func seedSubmoduleRepo(t *testing.T, s *isolatedServer, name string) (gitStorage.Storer, plumbing.Hash) {
	t.Helper()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, name, "", false)
	if repo == nil {
		t.Fatalf("CreateRepo %s failed", name)
	}
	stor := s.store.GetGitStorage("admin", repo.Name)

	topHash, err := storeBlob(stor, []byte("top\n"))
	if err != nil {
		t.Fatal(err)
	}
	// A commit id from "another repository": stored nowhere, so reading it as
	// a blob fails the way a genuine submodule pointer does.
	gitlink := plumbing.NewHash("1111111111111111111111111111111111111111")
	vendorTree, err := storeTree(stor, []object.TreeEntry{
		{Name: "lib", Mode: filemode.Submodule, Hash: gitlink},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := []object.TreeEntry{
		{Name: "top.txt", Mode: filemode.Regular, Hash: topHash},
		{Name: "vendor", Mode: filemode.Dir, Hash: vendorTree},
	}
	sort.Sort(object.TreeEntrySorter(entries))
	root, err := storeTree(stor, entries)
	if err != nil {
		t.Fatal(err)
	}
	head, err := storeCommit(stor, root, plumbing.ZeroHash, "with submodule")
	if err != nil {
		t.Fatal(err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(repo.DefaultBranch), head)); err != nil {
		t.Fatal(err)
	}
	return stor, head
}

// TestGitReadSurfacesResolveLiteralHEAD pins that the literal ref HEAD resolves
// through the repository's HEAD symref on every read surface, the way
// api.github.com answers 200 for /contents/README?ref=HEAD, /git/trees/HEAD,
// /commits/HEAD and /tarball/HEAD on octocat/Hello-World.
func TestGitReadSurfacesResolveLiteralHEAD(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := "head-ref-reads"
	requireStatus(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": name, "auto_init": true,
	}), http.StatusCreated)
	base := "/api/v3/repos/admin/" + name

	contents := decodeJSONWithStatus(t, s.get(t, base+"/contents/README.md?ref=HEAD", defaultToken), http.StatusOK)
	if contents["path"] != "README.md" {
		t.Fatalf("contents?ref=HEAD = %v", contents)
	}
	tree := decodeJSONWithStatus(t, s.get(t, base+"/git/trees/HEAD", defaultToken), http.StatusOK)
	if _, ok := tree["tree"].([]interface{}); !ok {
		t.Fatalf("git/trees/HEAD = %v", tree)
	}
	commit := decodeJSONWithStatus(t, s.get(t, base+"/commits/HEAD", defaultToken), http.StatusOK)
	headSHA, _ := commit["sha"].(string)
	if headSHA == "" {
		t.Fatalf("commits/HEAD = %v", commit)
	}
	if tree["sha"] != headSHA {
		t.Fatalf("git/trees/HEAD sha = %v, want the HEAD commit %s", tree["sha"], headSHA)
	}
	archive := s.noRedirectGet(t, base+"/tarball/HEAD")
	archive.Body.Close()
	if archive.StatusCode != http.StatusFound {
		t.Fatalf("tarball/HEAD = %d, want 302", archive.StatusCode)
	}

	// An empty repository has an unborn HEAD: the symref exists but names a
	// branch with no commit, and that must still read as not-found rather than
	// resolving to anything.
	requireStatus(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": "head-ref-empty",
	}), http.StatusCreated)
	empty := "/api/v3/repos/admin/head-ref-empty"
	requireStatus(t, s.get(t, empty+"/git/trees/HEAD", defaultToken), http.StatusNotFound)
	requireStatus(t, s.get(t, empty+"/commits/HEAD", defaultToken), http.StatusNotFound)
	requireStatus(t, s.get(t, empty+"/contents/README.md?ref=HEAD", defaultToken), http.StatusNotFound)
}

// TestLegacyRefNamespaceListing pins the legacy GET /git/refs/{ref} contract
// against api.github.com: an empty namespace is a 404 at any depth
// (/git/refs/HEAD, /git/refs/tags on a repo without tags, /git/refs/bogusns
// all answer 404 on octocat/Hello-World), and a populated listing paginates.
func TestLegacyRefNamespaceListing(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := "legacy-ref-listing"
	requireStatus(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": name, "auto_init": true,
	}), http.StatusCreated)
	base := "/api/v3/repos/admin/" + name
	head := s.headShaForTest(t, name)

	for _, path := range []string{"/git/refs/HEAD", "/git/refs/tags", "/git/refs/bogusns"} {
		requireStatus(t, s.get(t, base+path, defaultToken), http.StatusNotFound)
	}

	for i := 0; i < 3; i++ {
		requireStatus(t, s.post(t, base+"/git/refs", defaultToken, map[string]interface{}{
			"ref": fmt.Sprintf("refs/heads/topic-%d", i),
			"sha": head,
		}), http.StatusCreated)
	}

	resp := s.get(t, base+"/git/refs/heads?per_page=2", defaultToken)
	page := decodeJSONArrayWithStatus(t, resp, http.StatusOK)
	if len(page) != 2 {
		t.Fatalf("per_page=2 returned %d refs, want 2", len(page))
	}
	if !strings.Contains(resp.Header.Get("Link"), `rel="next"`) {
		t.Fatalf("legacy ref listing Link = %q, want a next link", resp.Header.Get("Link"))
	}

	// The whole-repository listing paginates the same way.
	all := s.get(t, base+"/git/refs?per_page=1", defaultToken)
	if got := decodeJSONArrayWithStatus(t, all, http.StatusOK); len(got) != 1 {
		t.Fatalf("/git/refs?per_page=1 returned %d refs, want 1", len(got))
	}
	if !strings.Contains(all.Header.Get("Link"), `rel="next"`) {
		t.Fatalf("/git/refs Link = %q, want a next link", all.Header.Get("Link"))
	}
}

// TestArchiveOfRepositoryWithSubmodule pins that a gitlink no longer 500s the
// archive: `git archive --format=tar` emits an empty directory at the
// submodule's path ("vendor/sub/"), and github.com's archives are git's.
func TestArchiveOfRepositoryWithSubmodule(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	seedSubmoduleRepo(t, s, "archive-submodule")

	resp := s.get(t, "/admin/archive-submodule/legacy.tar.gz/main", defaultToken)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("tar.gz archive = %d, want 200: %s", resp.StatusCode, body)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	names := map[string]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[hdr.Name] = hdr.Typeflag
	}
	resp.Body.Close()
	var submodulePath string
	for name := range names {
		if strings.HasSuffix(name, "/vendor/lib/") {
			submodulePath = name
		}
	}
	if submodulePath == "" {
		t.Fatalf("tar archive has no empty directory for the submodule: %v", names)
	}
	if names[submodulePath] != tar.TypeDir {
		t.Fatalf("submodule entry typeflag = %q, want a directory", names[submodulePath])
	}

	zipResp := s.get(t, "/admin/archive-submodule/legacy.zip/main", defaultToken)
	if zipResp.StatusCode != http.StatusOK {
		zipResp.Body.Close()
		t.Fatalf("zip archive = %d, want 200", zipResp.StatusCode)
	}
	raw, err := io.ReadAll(zipResp.Body)
	zipResp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	foundZipDir := false
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/vendor/lib/") {
			foundZipDir = true
			if !f.FileInfo().IsDir() {
				t.Fatalf("zip submodule entry %q is not a directory", f.Name)
			}
		}
	}
	if !foundZipDir {
		t.Fatalf("zip archive has no directory entry for the submodule: %v", zr.File)
	}
}

// TestGitTreeEntryForSubmoduleOmitsURLAndSize pins the tree-entry shape
// api.github.com serves for a gitlink: rust-lang/rust's src tree answers
// {path, mode, type, sha} for src/llvm-project, with no url and no size,
// because the commit lives in another repository.
func TestGitTreeEntryForSubmoduleOmitsURLAndSize(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	_, head := seedSubmoduleRepo(t, s, "tree-submodule")

	tree := decodeJSONWithStatus(t, s.get(t,
		"/api/v3/repos/admin/tree-submodule/git/trees/"+head.String()+"?recursive=1", defaultToken), http.StatusOK)
	entries, _ := tree["tree"].([]interface{})
	var gitlink map[string]interface{}
	for _, raw := range entries {
		entry, _ := raw.(map[string]interface{})
		if entry["path"] == "vendor/lib" {
			gitlink = entry
		}
	}
	if gitlink == nil {
		t.Fatalf("no vendor/lib entry in %v", entries)
	}
	if gitlink["type"] != "commit" || gitlink["mode"] != "160000" {
		t.Fatalf("gitlink entry = %v", gitlink)
	}
	if _, hasURL := gitlink["url"]; hasURL {
		t.Fatalf("gitlink entry advertises a url GitHub omits: %v", gitlink)
	}
	if _, hasSize := gitlink["size"]; hasSize {
		t.Fatalf("gitlink entry advertises a size GitHub omits: %v", gitlink)
	}
	// A blob entry still carries both.
	for _, raw := range entries {
		entry, _ := raw.(map[string]interface{})
		if entry["path"] == "top.txt" {
			if entry["url"] == nil || entry["size"] == nil {
				t.Fatalf("blob entry lost url/size: %v", entry)
			}
		}
	}
}

// TestContentsAndBlobSizeCeilings pins the documented blob-size contract:
// <= 1 MB inlines base64; 1-100 MB answers content:"" + encoding:"none" for a
// JSON media type while raw still streams the bytes; the Git Data blob
// endpoint still base64-encodes past 1 MB (verified against
// github/rest-api-description's 12.9 MB api.github.com.json, which the contents
// API answers with encoding "none" and /git/blobs answers base64).
func TestContentsAndBlobSizeCeilings(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "blob-size-ceiling", "", false)
	stor := s.store.GetGitStorage("admin", repo.Name)

	small := []byte("small\n")
	large := bytes.Repeat([]byte("x"), contentsAPIInlineFileBytes+1)
	smallHash, err := storeBlob(stor, small)
	if err != nil {
		t.Fatal(err)
	}
	largeHash, err := storeBlob(stor, large)
	if err != nil {
		t.Fatal(err)
	}
	entries := []object.TreeEntry{
		{Name: "small.txt", Mode: filemode.Regular, Hash: smallHash},
		{Name: "large.txt", Mode: filemode.Regular, Hash: largeHash},
	}
	sort.Sort(object.TreeEntrySorter(entries))
	root, err := storeTree(stor, entries)
	if err != nil {
		t.Fatal(err)
	}
	head, err := storeCommit(stor, root, plumbing.ZeroHash, "sizes")
	if err != nil {
		t.Fatal(err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(repo.DefaultBranch), head)); err != nil {
		t.Fatal(err)
	}
	base := "/api/v3/repos/admin/" + repo.Name

	inlined := decodeJSONWithStatus(t, s.get(t, base+"/contents/small.txt", defaultToken), http.StatusOK)
	if inlined["encoding"] != "base64" {
		t.Fatalf("small file encoding = %v, want base64", inlined["encoding"])
	}
	decoded, err := base64.StdEncoding.DecodeString(inlined["content"].(string))
	if err != nil || string(decoded) != string(small) {
		t.Fatalf("small file content = %q (%v)", decoded, err)
	}

	oversized := decodeJSONWithStatus(t, s.get(t, base+"/contents/large.txt", defaultToken), http.StatusOK)
	if oversized["encoding"] != "none" || oversized["content"] != "" {
		t.Fatalf("1-100 MB file = encoding %v content %q, want none/\"\"",
			oversized["encoding"], oversized["content"])
	}
	if size, _ := oversized["size"].(float64); int64(size) != int64(len(large)) {
		t.Fatalf("1-100 MB file size = %v, want %d", oversized["size"], len(large))
	}

	// raw keeps working for the same file — it is the fallback the "none"
	// encoding points a client at.
	rawResp := s.semanticRequest(t, http.MethodGet, base+"/contents/large.txt", "application/vnd.github.raw+json")
	rawBody, _ := io.ReadAll(rawResp.Body)
	rawResp.Body.Close()
	if rawResp.StatusCode != http.StatusOK || len(rawBody) != len(large) {
		t.Fatalf("raw large file = %d, %d bytes (want 200, %d)", rawResp.StatusCode, len(rawBody), len(large))
	}

	// ...and so does the Git Data blob endpoint, whose ceiling is 100 MB.
	blob := decodeJSONWithStatus(t, s.get(t, base+"/git/blobs/"+largeHash.String(), defaultToken), http.StatusOK)
	if blob["encoding"] != "base64" {
		t.Fatalf("git blob encoding = %v, want base64", blob["encoding"])
	}
	if size, _ := blob["size"].(float64); int64(size) != int64(len(large)) {
		t.Fatalf("git blob size = %v, want %d", blob["size"], len(large))
	}
}

// TestBlobTooLargeRefusalShape pins the refusal both size ceilings write: the
// 403 + Blob/data/too_large error item GitHub uses for a blob past an API's
// limit. The >100 MB case itself is not materialized in a test — allocating
// 100 MB of blob to assert a status is not worth the memory.
func TestBlobTooLargeRefusalShape(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	writeGHBlobTooLargeError(rec, "This API does not support blobs larger than 100 MB in size.")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var body struct {
		Message string `json:"message"`
		Errors  []struct {
			Resource string `json:"resource"`
			Field    string `json:"field"`
			Code     string `json:"code"`
		} `json:"errors"`
	}
	if err := decodeRecorderJSON(rec, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Errors) != 1 || body.Errors[0].Resource != "Blob" ||
		body.Errors[0].Field != "data" || body.Errors[0].Code != "too_large" {
		t.Fatalf("error item = %+v", body.Errors)
	}

	oversize := httptest.NewRecorder()
	refuseOversizedBlob(oversize)
	if oversize.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create-blob refusal = %d, want 422 (403/404/409/422 are the statuses the operation documents; 413 is not one)", oversize.Code)
	}
	var created struct {
		Errors []struct {
			Resource string `json:"resource"`
			Field    string `json:"field"`
			Code     string `json:"code"`
		} `json:"errors"`
	}
	if err := decodeRecorderJSON(oversize, &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Errors) != 1 || created.Errors[0].Resource != "Blob" ||
		created.Errors[0].Field != "content" || created.Errors[0].Code != "too_large" {
		t.Fatalf("create-blob error item = %+v", created.Errors)
	}
}

// TestCreateBlobAcceptsBodyPastTheGenericJSONCap pins that POST /git/blobs no
// longer inherits the 25 MiB JSON-document cap (~18.7 MB of binary after
// base64): api.github.com accepts a 40 MB body on this route without a 413.
func TestCreateBlobAcceptsBodyPastTheGenericJSONCap(t *testing.T) {
	t.Parallel()
	if maxBlobJSONBodyBytes <= maxJSONBodyBytes {
		t.Fatalf("blob cap (%d) must exceed the generic JSON cap (%d)", maxBlobJSONBodyBytes, maxJSONBodyBytes)
	}
	if maxBlobJSONBodyBytes > maxStructuredRequestBody {
		t.Fatalf("blob cap (%d) exceeds the shared structured cap (%d)", maxBlobJSONBodyBytes, maxStructuredRequestBody)
	}

	s := newIsolatedServer(t)
	name := "blob-body-cap"
	requireStatus(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": name, "auto_init": true,
	}), http.StatusCreated)

	// A body comfortably past the old cap and inside the new one.
	content := strings.Repeat("a", maxJSONBodyBytes+(1<<20))
	created := decodeJSONWithStatus(t, s.post(t, "/api/v3/repos/admin/"+name+"/git/blobs", defaultToken,
		map[string]interface{}{"content": content, "encoding": "utf-8"}), http.StatusCreated)
	if sha, _ := created["sha"].(string); len(sha) != 40 {
		t.Fatalf("create blob = %v", created)
	}
}

// TestListTagsOrderAndArchiveURLs pins github.com's tag list: newest version
// first (kubernetes/kubernetes answers v1.38.0-alpha.0 before v1.36.4, so it is
// version-aware, not a plain text sort; nodejs/node groups every v26.x before
// every v25.x, so it is not chronological), and API-shaped archive URLs
// (https://api.github.com/repos/{o}/{r}/zipball/refs/tags/{tag}).
func TestListTagsOrderAndArchiveURLs(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := "tag-order"
	requireStatus(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": name, "auto_init": true,
	}), http.StatusCreated)
	base := "/api/v3/repos/admin/" + name
	head := s.headShaForTest(t, name)

	for _, tag := range []string{"v1.0.0", "v1.10.0", "v2.0.0", "v2.0.0-rc.1", "v1.9.0"} {
		requireStatus(t, s.post(t, base+"/git/refs", defaultToken, map[string]interface{}{
			"ref": "refs/tags/" + tag,
			"sha": head,
		}), http.StatusCreated)
	}

	tags := decodeJSONArrayWithStatus(t, s.get(t, base+"/tags", defaultToken), http.StatusOK)
	var names []string
	for _, tag := range tags {
		names = append(names, tag["name"].(string))
	}
	want := []string{"v2.0.0", "v2.0.0-rc.1", "v1.10.0", "v1.9.0", "v1.0.0"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tag order = %v, want %v", names, want)
	}

	zipball, _ := tags[0]["zipball_url"].(string)
	tarball, _ := tags[0]["tarball_url"].(string)
	if !strings.HasSuffix(zipball, "/api/v3/repos/admin/"+name+"/zipball/refs/tags/v2.0.0") {
		t.Fatalf("zipball_url = %q, want the API-shaped archive endpoint", zipball)
	}
	if !strings.HasSuffix(tarball, "/api/v3/repos/admin/"+name+"/tarball/refs/tags/v2.0.0") {
		t.Fatalf("tarball_url = %q, want the API-shaped archive endpoint", tarball)
	}
	// The advertised URL is one that works: it 302s to the archive stream.
	redirect := s.noRedirectGet(t, base+"/zipball/refs/tags/v2.0.0")
	redirect.Body.Close()
	if redirect.StatusCode != http.StatusFound {
		t.Fatalf("advertised zipball_url = %d, want 302", redirect.StatusCode)
	}
}

func TestCompareTagNamesOrdersLikeGitHub(t *testing.T) {
	t.Parallel()
	// Each pair is {greater, lesser} as api.github.com orders them.
	for _, pair := range [][2]string{
		{"v1.38.0-alpha.0", "v1.9.0"},       // version-aware, not text order
		{"v26.0.0", "v25.9.0"},              // nodejs/node groups by major
		{"v1.36.0", "v1.36.0-rc.1"},         // a release outranks its prerelease
		{"v1.37.0-rc.1", "v1.37.0-alpha.3"}, // prereleases compare among themselves
		{"weekly.2012-03-27", "go1.24.0"},   // golang/go's non-semver tags
		{"v1.0.10", "v1.0.9"},               // numeric run, not lexicographic
	} {
		if got := compareTagNames(pair[0], pair[1]); got <= 0 {
			t.Errorf("compareTagNames(%q, %q) = %d, want > 0", pair[0], pair[1], got)
		}
		if got := compareTagNames(pair[1], pair[0]); got >= 0 {
			t.Errorf("compareTagNames(%q, %q) = %d, want < 0", pair[1], pair[0], got)
		}
	}
	if got := compareTagNames("v1.0.0", "v1.0.0"); got != 0 {
		t.Errorf("compareTagNames on equal names = %d, want 0", got)
	}
}

// TestContentsDirectoryListingCapped pins GitHub's documented "upper limit of
// 1,000 files for a directory": nodejs/node's test/parallel (~4,000 files)
// answers 200 with exactly 1,000 entries, in git's own tree order.
func TestContentsDirectoryListingCapped(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "big-directory", "", false)
	stor := s.store.GetGitStorage("admin", repo.Name)

	total := contentsAPIDirectoryEntryLimit + 5
	entries := make([]object.TreeEntry, 0, total)
	for i := 0; i < total; i++ {
		hash, err := storeBlob(stor, []byte(fmt.Sprintf("file %d\n", i)))
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, object.TreeEntry{
			Name: fmt.Sprintf("f%05d.txt", i),
			Mode: filemode.Regular,
			Hash: hash,
		})
	}
	sort.Sort(object.TreeEntrySorter(entries))
	root, err := storeTree(stor, entries)
	if err != nil {
		t.Fatal(err)
	}
	head, err := storeCommit(stor, root, plumbing.ZeroHash, "many files")
	if err != nil {
		t.Fatal(err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(repo.DefaultBranch), head)); err != nil {
		t.Fatal(err)
	}

	listing := decodeJSONArrayWithStatus(t, s.get(t, "/api/v3/repos/admin/"+repo.Name+"/contents/", defaultToken), http.StatusOK)
	if len(listing) != contentsAPIDirectoryEntryLimit {
		t.Fatalf("directory listing returned %d entries, want the %d-entry cap", len(listing), contentsAPIDirectoryEntryLimit)
	}
	if listing[0]["name"] != "f00000.txt" {
		t.Fatalf("listing starts at %v, want git tree order", listing[0]["name"])
	}
}

// TestGitCommitSignatureIsStoredAndReported pins both halves of the
// verification fix: POST /git/commits keeps the documented `signature` field
// (GitHub writes it into the commit's gpgsig header) and the read side reports
// a signed commit as unknown_key with the signature and payload echoed, the
// shape api.github.com serves (github/docs' HEAD commit answers
// {verified, reason, signature, payload, verified_at}).
func TestGitCommitSignatureIsStoredAndReported(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := "commit-signature"
	requireStatus(t, s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": name, "auto_init": true,
	}), http.StatusCreated)
	base := "/api/v3/repos/admin/" + name

	head := s.headShaForTest(t, name)
	parent := decodeJSONWithStatus(t, s.get(t, base+"/git/commits/"+head, defaultToken), http.StatusOK)
	unsigned, _ := parent["verification"].(map[string]interface{})
	if unsigned["reason"] != "unsigned" || unsigned["signature"] != nil {
		t.Fatalf("unsigned commit verification = %v", unsigned)
	}
	treeSHA := parent["tree"].(map[string]interface{})["sha"].(string)

	// github.com echoes the signature in git's canonical newline-terminated
	// form (github/docs' HEAD commit signature ends "-----END PGP SIGNATURE-----\n"),
	// which is also what the gpgsig header round-trips to.
	signature := "-----BEGIN PGP SIGNATURE-----\n\nZm9vYmFyc2lnbmF0dXJl\n-----END PGP SIGNATURE-----\n"
	created := decodeJSONWithStatus(t, s.post(t, base+"/git/commits", defaultToken, map[string]interface{}{
		"message":   "signed commit",
		"tree":      treeSHA,
		"parents":   []string{head},
		"signature": signature,
	}), http.StatusCreated)

	verification, _ := created["verification"].(map[string]interface{})
	if verification["verified"] != false {
		t.Fatalf("verified = %v, want false without a keyring", verification["verified"])
	}
	if verification["reason"] != "unknown_key" {
		t.Fatalf("reason = %v, want unknown_key for a signature no registered key matches", verification["reason"])
	}
	if verification["signature"] != signature {
		t.Fatalf("signature = %v, want the submitted signature echoed", verification["signature"])
	}
	payload, _ := verification["payload"].(string)
	if !strings.HasPrefix(payload, "tree "+treeSHA) || strings.Contains(payload, "gpgsig") {
		t.Fatalf("payload = %q, want the commit object text without the gpgsig header", payload)
	}
	if verification["verified_at"] != nil {
		t.Fatalf("verified_at = %v, want null for an unverified signature", verification["verified_at"])
	}

	// The signature survives the write: re-reading the stored object reports it
	// again, so the created commit really carries the gpgsig header.
	sha := created["sha"].(string)
	reread := decodeJSONWithStatus(t, s.get(t, base+"/git/commits/"+sha, defaultToken), http.StatusOK)
	rereadVerification, _ := reread["verification"].(map[string]interface{})
	if rereadVerification["signature"] != signature {
		t.Fatalf("stored commit lost its signature: %v", rereadVerification)
	}

	// The REST commit renderer reports the same thing.
	restCommit := decodeJSONWithStatus(t, s.get(t, base+"/commits/"+sha, defaultToken), http.StatusOK)
	inner, _ := restCommit["commit"].(map[string]interface{})
	restVerification, _ := inner["verification"].(map[string]interface{})
	if restVerification["reason"] != "unknown_key" || restVerification["signature"] != signature {
		t.Fatalf("REST commit verification = %v", restVerification)
	}
}

func decodeRecorderJSON(rec *httptest.ResponseRecorder, v interface{}) error {
	return json.Unmarshal(rec.Body.Bytes(), v)
}

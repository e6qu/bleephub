package bleephub

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/memory"
)

type competingReferenceStorer struct {
	gitStorage.Storer
	target    plumbing.ReferenceName
	competing *plumbing.Reference
	fired     bool
}

func (s *competingReferenceStorer) CheckAndSetReference(next, old *plumbing.Reference) error {
	if next.Name() == s.target && !s.fired {
		s.fired = true
		if err := s.Storer.SetReference(s.competing); err != nil {
			return err
		}
	}
	return s.Storer.CheckAndSetReference(next, old)
}

func gitDataTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer()
	s.registerGHRepoRoutes()
	s.registerGHRepoReadsRoutes()
	admin := s.store.UsersByLogin["admin"]
	s.store.Tokens[adminPAT] = &store.Token{Value: adminPAT, UserID: admin.ID, Scopes: "repo"}
	return s
}

func TestGitDataBlob(t *testing.T) {
	s := gitDataTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "gitdata-blob", "", false)

	// Create blob
	body, _ := json.Marshal(map[string]string{"content": "hello world", "encoding": "utf-8"})
	w := doMiscReq(s, "POST", "/api/v3/repos/"+repo.FullName+"/git/blobs", string(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create blob status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	var blob map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &blob); err != nil {
		t.Fatal(err)
	}
	sha, _ := blob["sha"].(string)
	if sha == "" {
		t.Fatalf("blob sha empty")
	}

	// Get blob
	w = doMiscReq(s, "GET", "/api/v3/repos/"+repo.FullName+"/git/blobs/"+sha, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get blob status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["sha"] != sha {
		t.Errorf("sha = %v, want %v", got["sha"], sha)
	}
	if got["encoding"] != "base64" {
		t.Errorf("encoding = %v, want base64", got["encoding"])
	}
}

func TestGitDataTreeAndCommit(t *testing.T) {
	s := gitDataTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "gitdata-tree", "", false)
	stor := s.store.GetGitStorage(admin.Login, repo.Name)

	// Create blob
	body, _ := json.Marshal(map[string]string{"content": "file body", "encoding": "utf-8"})
	w := doMiscReq(s, "POST", "/api/v3/repos/"+repo.FullName+"/git/blobs", string(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create blob status = %d, want 201", w.Code)
	}
	var blobResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &blobResp)
	blobSHA := blobResp["sha"].(string)

	// Create tree
	body, _ = json.Marshal(map[string]any{
		"tree": []map[string]any{{"path": "hello.txt", "mode": "100644", "type": "blob", "sha": blobSHA}},
	})
	w = doMiscReq(s, "POST", "/api/v3/repos/"+repo.FullName+"/git/trees", string(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create tree status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	var treeResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &treeResp)
	treeSHA := treeResp["sha"].(string)
	if treeSHA == "" {
		t.Fatalf("tree sha empty")
	}

	// Get tree
	w = doMiscReq(s, "GET", "/api/v3/repos/"+repo.FullName+"/git/trees/"+treeSHA, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get tree status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	// Create commit (no parents -> root commit)
	body, _ = json.Marshal(map[string]any{
		"message": "root",
		"tree":    treeSHA,
	})
	w = doMiscReq(s, "POST", "/api/v3/repos/"+repo.FullName+"/git/commits", string(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create commit status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	var commitResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &commitResp)
	commitSHA := commitResp["sha"].(string)
	if commitSHA == "" {
		t.Fatalf("commit sha empty")
	}

	// Get commit
	w = doMiscReq(s, "GET", "/api/v3/repos/"+repo.FullName+"/git/commits/"+commitSHA, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get commit status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	// Verify commit is in storage
	_, err := object.GetCommit(stor, plumbing.NewHash(commitSHA))
	if err != nil {
		t.Fatalf("commit not in storage: %v", err)
	}
}

func TestGitDataGetTreeResolvesEveryDocumentedTreeish(t *testing.T) {
	s := gitDataTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "gitdata-treeish", "", false)
	stor := s.store.GetGitStorage(admin.Login, repo.Name)
	sig := repoSignature(admin.Login, "bleephub@local")
	commitHash, err := initRepoWithFiles(stor, "main", "init", map[string]string{
		"root.txt":       "root\n",
		"nested/file.md": "nested\n",
	}, sig)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	commit, err := object.GetCommit(stor, commitHash)
	if err != nil {
		t.Fatalf("get commit: %v", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("get tree: %v", err)
	}
	rootEntry, err := tree.FindEntry("root.txt")
	if err != nil {
		t.Fatalf("find root blob: %v", err)
	}

	if err := stor.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName("feature/slash"), commitHash,
	)); err != nil {
		t.Fatalf("create slash branch: %v", err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(
		plumbing.NewTagReferenceName("v-light"), commitHash,
	)); err != nil {
		t.Fatalf("create lightweight tag: %v", err)
	}
	annotated := &object.Tag{
		Name:       "v-annotated",
		Tagger:     *sig,
		Message:    "release",
		Target:     commitHash,
		TargetType: plumbing.CommitObject,
	}
	annotatedHash, err := encodeTag(stor, annotated)
	if err != nil {
		t.Fatalf("encode annotated tag: %v", err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(
		plumbing.NewTagReferenceName("v-annotated"), annotatedHash,
	)); err != nil {
		t.Fatalf("create annotated tag ref: %v", err)
	}

	cases := []struct {
		name string
		ref  string
		sha  plumbing.Hash
	}{
		{"tree object SHA", tree.Hash.String(), tree.Hash},
		{"commit SHA", commitHash.String(), commitHash},
		{"branch shorthand", "main", commitHash},
		{"heads shorthand", "heads/main", commitHash},
		{"full branch ref", "refs/heads/main", commitHash},
		{"slash branch", "feature/slash", commitHash},
		{"lightweight tag", "v-light", commitHash},
		{"annotated tag", "v-annotated", commitHash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doMiscReq(s, http.MethodGet, "/api/v3/repos/"+repo.FullName+"/git/trees/"+tc.ref, "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET tree %q = %d: %s", tc.ref, w.Code, w.Body.String())
			}
			var out map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out["sha"] != tc.sha.String() {
				t.Errorf("sha = %v, want %s", out["sha"], tc.sha)
			}
			wantURL := "/git/trees/" + tc.sha.String()
			if url, _ := out["url"].(string); !strings.HasSuffix(url, wantURL) {
				t.Errorf("url = %q, want suffix %q", url, wantURL)
			}
		})
	}

	w := doMiscReq(s, http.MethodGet, "/api/v3/repos/"+repo.FullName+"/git/trees/main?recursive=false", "")
	if w.Code != http.StatusOK {
		t.Fatalf("recursive tree = %d: %s", w.Code, w.Body.String())
	}
	var recursive struct {
		Tree []map[string]any `json:"tree"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &recursive); err != nil {
		t.Fatal(err)
	}
	var nested, root map[string]any
	for _, entry := range recursive.Tree {
		switch entry["path"] {
		case "nested/file.md":
			nested = entry
		case "root.txt":
			root = entry
		}
	}
	for name, entry := range map[string]map[string]any{"root.txt": root, "nested/file.md": nested} {
		if entry == nil {
			t.Fatalf("recursive response omitted %s: %#v", name, recursive.Tree)
		}
		if entry["size"] == nil {
			t.Errorf("%s omitted blob size: %#v", name, entry)
		}
		if url, _ := entry["url"].(string); !strings.Contains(url, "/git/blobs/") {
			t.Errorf("%s url = %q, want blob URL", name, url)
		}
	}

	for _, tc := range []struct {
		name string
		ref  string
		code int
	}{
		{"raw annotated tag object", annotatedHash.String(), http.StatusUnprocessableEntity},
		{"raw blob object", rootEntry.Hash.String(), http.StatusUnprocessableEntity},
		{"unknown ref", "definitely-missing", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doMiscReq(s, http.MethodGet, "/api/v3/repos/"+repo.FullName+"/git/trees/"+tc.ref, "")
			if w.Code != tc.code {
				t.Fatalf("GET tree %q = %d, want %d: %s", tc.ref, w.Code, tc.code, w.Body.String())
			}
		})
	}
}

func TestGitDataReadShapesAndReferenceObjectTypes(t *testing.T) {
	s := gitDataTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "gitdata-read-shapes", "", false)
	stor := s.store.GetGitStorage(admin.Login, repo.Name)
	sig := repoSignature(admin.Login, "bleephub@local")
	commitHash, err := initRepoWithFiles(stor, "main", "init", map[string]string{"shape.txt": "shape\n"}, sig)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	commit, _ := object.GetCommit(stor, commitHash)
	tree, _ := commit.Tree()
	blobEntry, _ := tree.FindEntry("shape.txt")

	blobResponse := doMiscReq(s, http.MethodGet, "/api/v3/repos/"+repo.FullName+"/git/blobs/"+blobEntry.Hash.String(), "")
	if blobResponse.Code != http.StatusOK {
		t.Fatalf("get blob = %d: %s", blobResponse.Code, blobResponse.Body.String())
	}
	var blob map[string]any
	if err := json.Unmarshal(blobResponse.Body.Bytes(), &blob); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"sha", "node_id", "url", "size", "encoding", "content"} {
		if blob[field] == nil || blob[field] == "" {
			t.Errorf("blob response omitted %s: %#v", field, blob)
		}
	}

	annotated := &object.Tag{
		Name:       "annotated",
		Tagger:     *sig,
		Message:    "annotated",
		Target:     commitHash,
		TargetType: plumbing.CommitObject,
	}
	tagHash, err := encodeTag(stor, annotated)
	if err != nil {
		t.Fatal(err)
	}
	for name, hash := range map[string]plumbing.Hash{"light": commitHash, "annotated": tagHash} {
		if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName(name), hash)); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		ref, objectType, objectPath string
	}{
		{"tags/light", "commit", "/git/commits/"},
		{"tags/annotated", "tag", "/git/tags/"},
	} {
		w := doMiscReq(s, http.MethodGet, "/api/v3/repos/"+repo.FullName+"/git/ref/"+tc.ref, "")
		if w.Code != http.StatusOK {
			t.Fatalf("get ref %s = %d: %s", tc.ref, w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		refURL, _ := out["url"].(string)
		if strings.Contains(refURL, "/refs/refs/") || !strings.Contains(refURL, "/git/refs/"+tc.ref) {
			t.Errorf("%s ref url = %q", tc.ref, refURL)
		}
		object, _ := out["object"].(map[string]any)
		if object["type"] != tc.objectType {
			t.Errorf("%s object type = %v, want %s", tc.ref, object["type"], tc.objectType)
		}
		if objectURL, _ := object["url"].(string); !strings.Contains(objectURL, tc.objectPath) {
			t.Errorf("%s object url = %q, want %s", tc.ref, objectURL, tc.objectPath)
		}
	}
}

func TestGitDataRejectsInvalidCreateShapes(t *testing.T) {
	s := gitDataTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "gitdata-invalid-shapes", "", false)
	stor := s.store.GetGitStorage(admin.Login, repo.Name)
	sig := repoSignature(admin.Login, "bleephub@local")
	commitHash, err := initRepoWithFiles(stor, "main", "init", map[string]string{"a.txt": "a"}, sig)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, path, body string
	}{
		{"blob missing content", "blobs", `{"encoding":"utf-8"}`},
		{"blob encoding", "blobs", `{"content":"x","encoding":"rot13"}`},
		{"blob base64", "blobs", `{"content":"%%%","encoding":"base64"}`},
		{"commit author identity", "commits", `{"message":"m","tree":"` + treeHashFromCommit(t, stor, commitHash) + `","author":{"name":"only-name"}}`},
		{"commit author date", "commits", `{"message":"m","tree":"` + treeHashFromCommit(t, stor, commitHash) + `","author":{"name":"A","email":"a@example.test","date":"yesterday"}}`},
		{"tag missing message", "tags", `{"tag":"v1","object":"` + commitHash.String() + `","type":"commit"}`},
		{"tag invalid type", "tags", `{"tag":"v1","message":"m","object":"` + commitHash.String() + `","type":"tag"}`},
		{"tag missing object", "tags", `{"tag":"v1","message":"m","object":"` + strings.Repeat("f", 40) + `","type":"commit"}`},
		{"tagger date", "tags", `{"tag":"v1","message":"m","object":"` + commitHash.String() + `","type":"commit","tagger":{"name":"A","email":"a@example.test","date":"yesterday"}}`},
		{"ref not fully qualified", "refs", `{"ref":"heads/feature","sha":"` + commitHash.String() + `"}`},
		{"ref invalid name", "refs", `{"ref":"refs/heads/bad..name","sha":"` + commitHash.String() + `"}`},
		{"ref invalid sha", "refs", `{"ref":"refs/heads/feature","sha":"not-a-sha"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doMiscReq(s, http.MethodPost, "/api/v3/repos/"+repo.FullName+"/git/"+tc.path, tc.body)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%s = %d, want 422: %s", tc.name, w.Code, w.Body.String())
			}
		})
	}

	w := doMiscReq(s, http.MethodPatch, "/api/v3/repos/"+repo.FullName+"/git/refs/heads/main", `{}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH ref without sha = %d, want 422: %s", w.Code, w.Body.String())
	}
}

func TestGitDataRefsAndTag(t *testing.T) {
	s := gitDataTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "gitdata-refs", "", false)
	stor := s.store.GetGitStorage(admin.Login, repo.Name)

	// Seed a root commit directly via helper.
	sig := repoSignature(admin.Login, "bleephub@local")
	commitHash, err := initRepoWithFiles(stor, "main", "init", map[string]string{"a.txt": "a"}, sig)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}

	// Create a tag object pointing at the commit
	body, _ := json.Marshal(map[string]any{
		"tag":     "v1.0.0",
		"message": "release",
		"object":  commitHash.String(),
		"type":    "commit",
	})
	w := doMiscReq(s, "POST", "/api/v3/repos/"+repo.FullName+"/git/tags", string(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create tag status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	var tagResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &tagResp)
	tagSHA := tagResp["sha"].(string)

	// Get tag
	w = doMiscReq(s, "GET", "/api/v3/repos/"+repo.FullName+"/git/tags/"+tagSHA, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get tag status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	// Create refs/heads/feature
	body, _ = json.Marshal(map[string]any{"ref": "refs/heads/feature", "sha": commitHash.String()})
	w = doMiscReq(s, "POST", "/api/v3/repos/"+repo.FullName+"/git/refs", string(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create ref status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	var refResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &refResp)
	if refResp["ref"] != "refs/heads/feature" {
		t.Errorf("ref = %v, want refs/heads/feature", refResp["ref"])
	}

	// Create a second commit to fast-forward the branch
	body, _ = json.Marshal(map[string]any{
		"tree":    treeHashFromCommit(t, stor, commitHash),
		"parents": []string{commitHash.String()},
		"message": "second",
	})
	w = doMiscReq(s, "POST", "/api/v3/repos/"+repo.FullName+"/git/commits", string(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create second commit status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	var commit2Resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &commit2Resp)
	commit2SHA := commit2Resp["sha"].(string)

	// Add a grandchild and update straight from the root to it. A
	// fast-forward is ancestry, not only an immediate parent edge.
	body, _ = json.Marshal(map[string]any{
		"tree":    treeHashFromCommit(t, stor, commitHash),
		"parents": []string{commit2SHA},
		"message": "third",
	})
	w = doMiscReq(s, "POST", "/api/v3/repos/"+repo.FullName+"/git/commits", string(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create third commit status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	var commit3Resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &commit3Resp)

	// Update ref (multi-commit fast-forward)
	body, _ = json.Marshal(map[string]any{"sha": commit3Resp["sha"].(string)})
	w = doMiscReq(s, "PATCH", "/api/v3/repos/"+repo.FullName+"/git/refs/heads/feature", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("update ref status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	// List refs
	w = doMiscReq(s, "GET", "/api/v3/repos/"+repo.FullName+"/git/refs/heads", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list refs status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var refs []map[string]any
	json.Unmarshal(w.Body.Bytes(), &refs)
	if len(refs) != 2 { // main + feature
		t.Errorf("len(refs) = %d, want 2", len(refs))
	}

	// Delete ref
	w = doMiscReq(s, "DELETE", "/api/v3/repos/"+repo.FullName+"/git/refs/heads/feature", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete ref status = %d, want 204; body = %s", w.Code, w.Body.String())
	}
}

// TestUpdateRefNonCommitTargetRejected: a non-force branch update to a target
// that is not a commit (an annotated tag object) can never be a fast-forward,
// so it must be rejected with 422 and leave the ref untouched.
func TestUpdateRefNonCommitTargetRejected(t *testing.T) {
	s := gitDataTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "gitdata-nonff", "", false)
	stor := s.store.GetGitStorage(admin.Login, repo.Name)

	sig := repoSignature(admin.Login, "bleephub@local")
	commitHash, err := initRepoWithFiles(stor, "main", "init", map[string]string{"a.txt": "a"}, sig)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"tag":     "v1.0.0",
		"message": "release",
		"object":  commitHash.String(),
		"type":    "commit",
	})
	w := doMiscReq(s, "POST", "/api/v3/repos/"+repo.FullName+"/git/tags", string(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create tag status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	var tagResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &tagResp)
	tagSHA := tagResp["sha"].(string)

	body, _ = json.Marshal(map[string]any{"sha": tagSHA})
	w = doMiscReq(s, "PATCH", "/api/v3/repos/"+repo.FullName+"/git/refs/heads/main", string(body))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("update ref to tag object status = %d, want 422; body = %s", w.Code, w.Body.String())
	}

	ref, err := stor.Reference(plumbing.ReferenceName("refs/heads/main"))
	if err != nil {
		t.Fatalf("main ref lookup: %v", err)
	}
	if ref.Hash() != commitHash {
		t.Errorf("main = %s after rejected update, want %s", ref.Hash(), commitHash)
	}
}

func treeHashFromCommit(t *testing.T, stor gitStorage.Storer, h plumbing.Hash) string {
	t.Helper()
	c, err := object.GetCommit(stor, h)
	if err != nil {
		t.Fatal(err)
	}
	return c.TreeHash.String()
}

func TestGitDataWriteRequiresAuth(t *testing.T) {
	s := gitDataTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "gitdata-auth", "", false)

	reqs := []struct {
		method string
		path   string
		body   string
	}{
		{"POST", "/api/v3/repos/" + repo.FullName + "/git/blobs", `{"content":"x"}`},
		{"POST", "/api/v3/repos/" + repo.FullName + "/git/trees", `{"tree":[]}`},
		{"POST", "/api/v3/repos/" + repo.FullName + "/git/commits", `{"message":"m","tree":""}`},
		{"POST", "/api/v3/repos/" + repo.FullName + "/git/tags", `{"tag":"t","object":"","type":"commit"}`},
		{"POST", "/api/v3/repos/" + repo.FullName + "/git/refs", `{"ref":"refs/heads/x","sha":""}`},
	}
	for _, tc := range reqs {
		w := doMiscReq(s, tc.method, tc.path, tc.body)
		// doMiscReq sends the admin PAT by default, so write should succeed (status not 401).
		// We are only verifying the route exists and auth is wired; empty SHAs may cause 422.
		if w.Code == http.StatusUnauthorized {
			t.Errorf("%s %s returned 401 with valid PAT", tc.method, tc.path)
		}
	}
}

func TestGitDataReadRequiresContentsRead(t *testing.T) {
	s := gitDataTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "gitdata-read", "", false)
	stor := s.store.GetGitStorage(admin.Login, repo.Name)
	sig := repoSignature(admin.Login, "bleephub@local")
	commitHash, _ := initRepoWithFiles(stor, "main", "init", map[string]string{"a.txt": "a"}, sig)

	// Create an installation token with metadata:read only (no contents)
	s.store.Installations[1] = &store.Installation{ID: 1, AppID: 1, TargetID: admin.ID, Permissions: map[string]string{"metadata": "read"}}
	s.store.InstallationTokens["ghs_notallowed"] = &store.InstallationToken{Token: "ghs_notallowed", InstallationID: 1, AppID: 1, Permissions: map[string]string{"metadata": "read"}, ExpiresAt: fixedTestTime.Add(time.Hour)}

	req, _ := http.NewRequest("GET", "/api/v3/repos/"+repo.FullName+"/git/commits/"+commitHash.String(), nil)
	req.Header.Set("Authorization", "Bearer ghs_notallowed")
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for metadata-only token", w.Code)
	}
}

func TestCreateFileCommitDoesNotOverwriteConcurrentRefUpdate(t *testing.T) {
	stor := memory.NewStorage()
	sig := repoSignature("admin", "admin@example.test")
	base, err := initRepoWithFiles(stor, "main", "base", map[string]string{"README.md": "base\n"}, sig)
	if err != nil {
		t.Fatal(err)
	}
	competing := plumbing.NewHash("1111111111111111111111111111111111111111")
	wrapped := &competingReferenceStorer{
		Storer:    stor,
		target:    plumbing.NewBranchReferenceName("main"),
		competing: plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), competing),
	}
	if _, err := createFileCommitExpected(wrapped, "main", "README.md", "request\n", "request", sig, base); !errors.Is(err, gitStorage.ErrReferenceHasChanged) {
		t.Fatalf("concurrent content write error = %v, want ErrReferenceHasChanged", err)
	}
	ref, err := stor.Reference(plumbing.NewBranchReferenceName("main"))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Hash() != competing {
		t.Fatalf("request overwrote concurrent ref: got %s want %s", ref.Hash(), competing)
	}
}

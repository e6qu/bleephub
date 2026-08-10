package bleephub

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func (s *isolatedServer) semanticRequest(t *testing.T, method, path, accept string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, s.baseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+defaultToken)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestListCommitsHonorsDocumentedSelectorsAndPagination(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "commit-selectors", "", false)
	stor := s.store.GetGitStorage("admin", repo.Name)

	firstWhen := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	secondWhen := firstWhen.Add(24 * time.Hour)
	firstSignature := &object.Signature{Name: admin.Login, Email: "admin@example.test", When: firstWhen}
	secondSignature := &object.Signature{Name: "Other Author", Email: "other@example.test", When: secondWhen}
	firstSHA, err := initRepoWithFiles(stor, "main", "first", map[string]string{"a.txt": "a\n"}, firstSignature)
	if err != nil {
		t.Fatal(err)
	}
	secondSHA, err := createFileCommit(stor, "main", "b.txt", "b\n", "second", secondSignature)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		query string
		want  plumbing.Hash
	}{
		{"path=b.txt", secondSHA},
		{"path=a.txt", firstSHA},
		{"author=admin", firstSHA},
		{"author=other%40example.test", secondSHA},
		{"since=" + url.QueryEscape(secondWhen.Format(time.RFC3339)), secondSHA},
		{"until=" + url.QueryEscape(firstWhen.Format(time.RFC3339)), firstSHA},
	} {
		resp := s.get(t, "/api/v3/repos/admin/"+repo.Name+"/commits?"+tc.query, defaultToken)
		items := decodeJSONArray(t, resp)
		if len(items) != 1 || items[0]["sha"] != tc.want.String() {
			t.Fatalf("%s commits = %v, want only %s", tc.query, items, tc.want)
		}
	}

	resp := s.get(t, "/api/v3/repos/admin/"+repo.Name+"/commits?per_page=1&page=2", defaultToken)
	items := decodeJSONArray(t, resp)
	if len(items) != 1 || items[0]["sha"] != firstSHA.String() {
		t.Fatalf("page 2 commits = %v, want %s", items, firstSHA)
	}
	if link := resp.Header.Get("Link"); !strings.Contains(link, `rel="prev"`) {
		t.Fatalf("page 2 Link = %q, want previous-page relation", link)
	}

	invalid := s.get(t, "/api/v3/repos/admin/"+repo.Name+"/commits?since=not-a-date", defaultToken)
	invalid.Body.Close()
	if invalid.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid since = %d, want 422", invalid.StatusCode)
	}
}

func TestRepositoryReadMediaTypesAndDirectoryShapes(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "content-shapes", "", false)
	stor := s.store.GetGitStorage("admin", repo.Name)

	readmeHash, err := storeBlob(stor, []byte("# Read me\n"))
	if err != nil {
		t.Fatal(err)
	}
	linkHash, err := storeBlob(stor, []byte("README.md"))
	if err != nil {
		t.Fatal(err)
	}
	modulesHash, err := storeBlob(stor, []byte("[submodule \"vendor/lib\"]\n\tpath = vendor/lib\n\turl = https://example.test/lib.git\n"))
	if err != nil {
		t.Fatal(err)
	}
	moduleTree, err := storeTree(stor, nil)
	if err != nil {
		t.Fatal(err)
	}
	moduleCommit, err := storeCommit(stor, moduleTree, plumbing.ZeroHash, "module")
	if err != nil {
		t.Fatal(err)
	}
	entries := []object.TreeEntry{
		{Name: ".gitmodules", Mode: filemode.Regular, Hash: modulesHash},
		{Name: "README.md", Mode: filemode.Regular, Hash: readmeHash},
		{Name: "readme-link", Mode: filemode.Symlink, Hash: linkHash},
		{Name: "vendor", Mode: filemode.Dir},
	}
	vendorTree, err := storeTree(stor, []object.TreeEntry{{Name: "lib", Mode: filemode.Submodule, Hash: moduleCommit}})
	if err != nil {
		t.Fatal(err)
	}
	entries[3].Hash = vendorTree
	sort.Sort(object.TreeEntrySorter(entries))
	rootTree, err := storeTree(stor, entries)
	if err != nil {
		t.Fatal(err)
	}
	head, err := storeCommit(stor, rootTree, plumbing.ZeroHash, "content shapes")
	if err != nil {
		t.Fatal(err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), head)); err != nil {
		t.Fatal(err)
	}

	resp := s.semanticRequest(t, http.MethodGet, "/api/v3/repos/admin/"+repo.Name+"/contents/README.md", "application/vnd.github.raw+json")
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(raw) != "# Read me\n" {
		t.Fatalf("raw contents = %d %q", resp.StatusCode, raw)
	}

	resp = s.semanticRequest(t, http.MethodGet, "/api/v3/repos/admin/"+repo.Name+"/contents/", "application/vnd.github.object+json")
	objectListing := decodeJSON(t, resp)
	if objectListing["type"] != "dir" {
		t.Fatalf("object media directory type = %v", objectListing["type"])
	}
	listed, _ := objectListing["entries"].([]interface{})
	if len(listed) != 4 {
		t.Fatalf("object media entries = %v", listed)
	}
	byName := map[string]map[string]interface{}{}
	for _, rawEntry := range listed {
		entry := rawEntry.(map[string]interface{})
		byName[entry["name"].(string)] = entry
		for _, required := range []string{"name", "path", "sha", "size", "type", "url", "git_url", "html_url", "download_url", "_links"} {
			if _, exists := entry[required]; !exists {
				t.Fatalf("%s missing %s: %v", entry["name"], required, entry)
			}
		}
	}
	if byName["readme-link"]["type"] != "symlink" || byName["readme-link"]["target"] != "README.md" {
		t.Fatalf("symlink shape = %v", byName["readme-link"])
	}
	resp = s.get(t, "/api/v3/repos/admin/"+repo.Name+"/contents/readme-link", defaultToken)
	resolvedLink := decodeJSON(t, resp)
	decodedLink, err := base64.StdEncoding.DecodeString(resolvedLink["content"].(string))
	if err != nil || string(decodedLink) != "# Read me\n" || resolvedLink["type"] != "file" {
		t.Fatalf("resolved symlink contents = %v, decoded %q, err %v", resolvedLink, decodedLink, err)
	}

	resp = s.get(t, "/api/v3/repos/admin/"+repo.Name+"/contents/vendor?ref="+head.String(), defaultToken)
	vendor := decodeJSONArray(t, resp)
	if len(vendor) != 1 || vendor[0]["type"] != "submodule" ||
		vendor[0]["submodule_git_url"] != "https://example.test/lib.git" {
		t.Fatalf("submodule shape = %v", vendor)
	}

	resp = s.semanticRequest(t, http.MethodGet, "/api/v3/repos/admin/"+repo.Name+"/commits/main", "application/vnd.github.sha")
	shaBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(shaBody) != head.String() {
		t.Fatalf("commit SHA media body = %q, want %s", shaBody, head)
	}
	resp = s.semanticRequest(t, http.MethodGet, "/api/v3/repos/admin/"+repo.Name+"/commits/main", "application/vnd.github.diff")
	diffBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(diffBody, []byte("README.md")) {
		t.Fatalf("commit diff body = %q", diffBody)
	}
}

func TestArchiveZipPreservesSymlinks(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "archive-symlink", "", false)
	stor := s.store.GetGitStorage("admin", repo.Name)
	targetHash, err := storeBlob(stor, []byte("target\n"))
	if err != nil {
		t.Fatal(err)
	}
	linkHash, err := storeBlob(stor, []byte("target.txt"))
	if err != nil {
		t.Fatal(err)
	}
	treeHash, err := storeTree(stor, []object.TreeEntry{
		{Name: "link", Mode: filemode.Symlink, Hash: linkHash},
		{Name: "target.txt", Mode: filemode.Regular, Hash: targetHash},
	})
	if err != nil {
		t.Fatal(err)
	}
	head, err := storeCommit(stor, treeHash, plumbing.ZeroHash, "archive")
	if err != nil {
		t.Fatal(err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), head)); err != nil {
		t.Fatal(err)
	}

	redirect := s.noRedirectGet(t, "/api/v3/repos/admin/"+repo.Name+"/zipball/main")
	location := redirect.Header.Get("Location")
	redirect.Body.Close()
	resp := s.get(t, strings.TrimPrefix(location, s.baseURL), defaultToken)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, file := range zr.File {
		if !strings.HasSuffix(file.Name, "/link") {
			continue
		}
		found = true
		if file.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("zip link mode = %v, want symlink", file.Mode())
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		target, _ := io.ReadAll(reader)
		reader.Close()
		if string(target) != "target.txt" {
			t.Fatalf("zip symlink target = %q", target)
		}
	}
	if !found {
		t.Fatal("zip archive did not contain symlink")
	}
}

func TestCompareAndReferenceListsFailLoudlyWithoutGitStorage(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "missing-read-storage", "", false)
	stor := s.store.GetGitStorage("admin", repo.Name)
	s.store.mu.Lock()
	delete(s.store.GitStorages, repo.FullName)
	s.store.mu.Unlock()
	t.Cleanup(func() {
		s.store.mu.Lock()
		s.store.GitStorages[repo.FullName] = stor
		s.store.mu.Unlock()
	})

	for _, path := range []string{
		"/api/v3/repos/" + repo.FullName + "/compare/main...main",
		"/api/v3/repos/" + repo.FullName + "/branches",
		"/api/v3/repos/" + repo.FullName + "/tags",
	} {
		resp := s.get(t, path, defaultToken)
		resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("%s = %d, want 500", path, resp.StatusCode)
		}
	}
}

func TestMergeUpstreamPerformsDivergedMergeWithAuthenticatedAuthor(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	source := s.store.CreateRepo(admin, "merge-source", "", false)
	sourceStor := s.store.GetGitStorage("admin", source.Name)
	signature := &object.Signature{Name: admin.Login, Email: "admin@example.test", When: fixedTestTime}
	if _, err := initRepoWithFiles(sourceStor, "main", "root", map[string]string{"root.txt": "root\n"}, signature); err != nil {
		t.Fatal(err)
	}
	fork := s.store.ForkRepo(admin, source, "merge-fork")
	if fork == nil {
		t.Fatal("create fork")
	}
	forkStor := s.store.GetGitStorage("admin", fork.Name)
	if _, err := createFileCommit(sourceStor, "main", "upstream.txt", "upstream\n", "upstream", signature); err != nil {
		t.Fatal(err)
	}
	if _, err := createFileCommit(forkStor, "main", "fork.txt", "fork\n", "fork", signature); err != nil {
		t.Fatal(err)
	}

	resp := s.post(t, "/api/v3/repos/"+fork.FullName+"/merge-upstream", defaultToken, map[string]interface{}{"branch": "main"})
	out := decodeJSONWithStatus(t, resp, http.StatusOK)
	if out["merge_type"] != "merge" {
		t.Fatalf("merge type = %v, want merge", out["merge_type"])
	}
	headRef, err := forkStor.Reference(plumbing.NewBranchReferenceName("main"))
	if err != nil {
		t.Fatal(err)
	}
	head, err := object.GetCommit(forkStor, headRef.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if head.NumParents() != 2 || head.Author.Name != admin.Name && head.Author.Name != admin.Login {
		t.Fatalf("merge commit = parents %d author %q", head.NumParents(), head.Author.Name)
	}
	for _, path := range []string{"fork.txt", "upstream.txt"} {
		resp := s.get(t, "/api/v3/repos/"+fork.FullName+"/contents/"+path, defaultToken)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("merged %s = %d", path, resp.StatusCode)
		}
	}
}

func TestPullRequestAndIssueCollaborationMetadata(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "collaboration-metadata")
	repo := s.store.GetRepo("admin", "collaboration-metadata")
	created := s.post(t, "/api/v3/repos/"+repo.FullName+"/pulls", defaultToken, map[string]interface{}{
		"title": "Metadata", "head": "feat", "base": "main",
	})
	prJSON := decodeJSONWithStatus(t, created, http.StatusCreated)
	number := int(prJSON["number"].(float64))
	pr := s.store.GetPullRequestByNumber(repo.ID, number)
	s.store.UpdatePullRequest(pr.ID, func(stored *PullRequest) {
		stored.Mergeable = "UNKNOWN"
	})
	s.store.PRReviewComments.CreateRootComment(pr.ID, s.store.UsersByLogin["admin"].ID, "feature.txt", "inline", "", "RIGHT", 1, 0)

	resp := s.get(t, "/api/v3/repos/"+repo.FullName+"/pulls/"+prJSONNumber(number), defaultToken)
	renderedPR := decodeJSON(t, resp)
	if renderedPR["mergeable"] != nil {
		t.Fatalf("unknown mergeable = %v, want null", renderedPR["mergeable"])
	}
	if int(renderedPR["review_comments"].(float64)) != 1 {
		t.Fatalf("review_comments = %v, want 1", renderedPR["review_comments"])
	}

	parentResp := s.post(t, "/api/v3/repos/"+repo.FullName+"/issues", defaultToken, map[string]interface{}{"title": "Parent"})
	parent := decodeJSONWithStatus(t, parentResp, http.StatusCreated)
	childResp := s.post(t, "/api/v3/repos/"+repo.FullName+"/issues", defaultToken, map[string]interface{}{"title": "Child"})
	child := decodeJSONWithStatus(t, childResp, http.StatusCreated)
	parentNumber := int(parent["number"].(float64))
	childNumber := int(child["number"].(float64))
	childIssue := s.store.GetIssueByNumber(repo.ID, childNumber)
	resp = s.post(t, "/api/v3/repos/"+repo.FullName+"/issues/"+prJSONNumber(parentNumber)+"/sub_issues", defaultToken, map[string]interface{}{"sub_issue_id": childIssue.ID})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add sub-issue = %d", resp.StatusCode)
	}
	for _, issueNumber := range []int{childNumber, parentNumber} {
		resp = s.patch(t, "/api/v3/repos/"+repo.FullName+"/issues/"+prJSONNumber(issueNumber), defaultToken, map[string]interface{}{"state": "closed"})
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("close issue %d = %d", issueNumber, resp.StatusCode)
		}
	}
	resp = s.get(t, "/api/v3/repos/"+repo.FullName+"/issues/"+prJSONNumber(parentNumber), defaultToken)
	issue := decodeJSON(t, resp)
	if issue["author_association"] != "OWNER" || !strings.HasSuffix(issue["timeline_url"].(string), "/timeline") {
		t.Fatalf("issue association/timeline = %v / %v", issue["author_association"], issue["timeline_url"])
	}
	closedBy, _ := issue["closed_by"].(map[string]interface{})
	if closedBy["login"] != "admin" {
		t.Fatalf("closed_by = %v", issue["closed_by"])
	}
	summary := issue["sub_issues_summary"].(map[string]interface{})
	if int(summary["total"].(float64)) != 1 || int(summary["completed"].(float64)) != 1 ||
		int(summary["percent_completed"].(float64)) != 100 {
		t.Fatalf("sub_issues_summary = %v", summary)
	}
}

func prJSONNumber(number int) string {
	return strconv.Itoa(number)
}

func TestBranchRestrictionSubresourcesAddSetAndRemoveRequestedEntries(t *testing.T) {
	s := bpTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	reviewer := &User{
		ID:        9821,
		NodeID:    "U_9821",
		Login:     "restriction-reviewer",
		Name:      "Restriction Reviewer",
		Email:     "reviewer@example.test",
		Type:      "User",
		CreatedAt: fixedTestTime,
		UpdatedAt: fixedTestTime,
	}
	s.store.mu.Lock()
	s.store.Users[reviewer.ID] = reviewer
	s.store.UsersByLogin[reviewer.Login] = reviewer
	s.store.mu.Unlock()
	repo := s.store.CreateRepo(admin, "restriction-semantics", "", false)

	put := doBPReq(s, adminPAT, http.MethodPut, "/api/v3/repos/"+repo.FullName+"/branches/main/protection",
		`{"required_status_checks":{"strict":false,"contexts":["ci"]},"restrictions":{"users":[{"login":"admin","id":1,"type":"User"}]}}`)
	if put.Code != http.StatusOK {
		t.Fatalf("seed protection = %d %s", put.Code, put.Body.String())
	}
	empty := doBPReq(s, adminPAT, http.MethodPut, "/api/v3/repos/"+repo.FullName+"/branches/main/protection", "")
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty protection body = %d, want 400", empty.Code)
	}

	path := "/api/v3/repos/" + repo.FullName + "/branches/main/protection/restrictions/users"
	add := doBPReq(s, adminPAT, http.MethodPost, path, `{"users":["restriction-reviewer"]}`)
	if add.Code != http.StatusOK {
		t.Fatalf("add restriction = %d %s", add.Code, add.Body.String())
	}
	var users []map[string]interface{}
	if err := json.Unmarshal(add.Body.Bytes(), &users); err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0]["avatar_url"] == nil || users[1]["avatar_url"] == nil {
		t.Fatalf("full user restriction response = %v", users)
	}

	remove := doBPReq(s, adminPAT, http.MethodDelete, path, `{"users":["restriction-reviewer"]}`)
	if remove.Code != http.StatusOK {
		t.Fatalf("remove restriction = %d %s", remove.Code, remove.Body.String())
	}
	if err := json.Unmarshal(remove.Body.Bytes(), &users); err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0]["login"] != "admin" {
		t.Fatalf("remaining restrictions = %v", users)
	}

	get := doBPReq(s, adminPAT, http.MethodGet,
		"/api/v3/repos/"+repo.FullName+"/branches/main/protection/required_status_checks", "")
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"ci"`) {
		t.Fatalf("empty PUT changed status checks: %d %s", get.Code, get.Body.String())
	}
}

func TestCommentAuthorAssociationUsesRepositoryRelationship(t *testing.T) {
	s := newTestServer()
	admin := s.store.UsersByLogin["admin"]
	contributor := &User{ID: 7411, Login: "comment-contributor", Type: "User", CreatedAt: fixedTestTime, UpdatedAt: fixedTestTime}
	s.store.mu.Lock()
	s.store.Users[contributor.ID] = contributor
	s.store.UsersByLogin[contributor.Login] = contributor
	s.store.mu.Unlock()
	repo := s.store.CreateRepo(admin, "comment-association", "", false)
	if !s.store.AddRepoCollaborator(admin.Login, repo.Name, contributor.Login, "push") {
		t.Fatal("add repository collaborator")
	}

	commitComment := &CommitComment{ID: 1, AuthorID: contributor.ID, RepoID: repo.ID, CommitID: strings.Repeat("a", 40), CreatedAt: fixedTestTime, UpdatedAt: fixedTestTime}
	if got := commitCommentToJSON(commitComment, s.store, "https://example.test", repo)["author_association"]; got != "COLLABORATOR" {
		t.Fatalf("commit comment association = %v", got)
	}
	pr := &PullRequest{ID: 1, Number: 1, RepoID: repo.ID, AuthorID: admin.ID}
	reviewComment := s.store.PRReviewComments.CreateRootComment(pr.ID, contributor.ID, "file.txt", "body", "", "RIGHT", 1, 0)
	if got := prReviewCommentToJSON(reviewComment, s.store, "https://example.test", repo, pr)["author_association"]; got != "COLLABORATOR" {
		t.Fatalf("review comment association = %v", got)
	}
}

func TestContentsHTMLMediaRendersMarkup(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createReadsRepo(t, "content-html", map[string]interface{}{"auto_init": true})
	encoded := base64.StdEncoding.EncodeToString([]byte("# Heading\n"))
	resp := s.put(t, "/api/v3/repos/admin/content-html/contents/doc.md", defaultToken, map[string]interface{}{
		"message": "doc", "content": encoded,
	})
	resp.Body.Close()
	request := s.semanticRequest(t, http.MethodGet, "/api/v3/repos/admin/content-html/contents/doc.md", "application/vnd.github.html+json")
	body, _ := io.ReadAll(request.Body)
	request.Body.Close()
	if request.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("<h1")) {
		t.Fatalf("HTML media = %d %q", request.StatusCode, body)
	}
}

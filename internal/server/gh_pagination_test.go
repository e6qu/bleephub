package bleephub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
)

func pagedJSONRequest(t *testing.T, s *Server, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	return w
}

func TestPaginationDefaults(t *testing.T) {
	repoName := "pg-defaults"
	createTestIssueRepo(t, repoName)

	// Create 3 issues
	for i := 0; i < 3; i++ {
		ghPost(t, "/api/v3/repos/admin/"+repoName+"/issues", defaultToken, map[string]interface{}{
			"title": "Issue",
		}).Body.Close()
	}

	resp := ghGet(t, "/api/v3/repos/admin/"+repoName+"/issues", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	issues := decodeJSONArray(t, resp)
	if len(issues) != 3 {
		t.Fatalf("expected 3 issues, got %d", len(issues))
	}

	// No Link header for single page
	if link := resp.Header.Get("Link"); link != "" {
		t.Fatalf("expected no Link header for single page, got %s", link)
	}
}

func TestPaginationCustomPerPage(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoName := "pg-perpage"
	s.createTestIssueRepo(t, repoName)

	for i := 0; i < 5; i++ {
		s.post(t, "/api/v3/repos/admin/"+repoName+"/issues", defaultToken, map[string]interface{}{
			"title": "Issue",
		}).Body.Close()
	}

	// Page 1 with per_page=2
	resp := s.get(t, "/api/v3/repos/admin/"+repoName+"/issues?per_page=2", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	issues := decodeJSONArray(t, resp)
	if len(issues) != 2 {
		t.Fatalf("expected 2 issues, got %d", len(issues))
	}

	link := resp.Header.Get("Link")
	if link == "" {
		t.Fatal("expected Link header for multi-page result")
	}
	if !strings.Contains(link, `rel="next"`) {
		t.Fatalf("expected rel=next in Link, got %s", link)
	}
	if !strings.Contains(link, `rel="last"`) {
		t.Fatalf("expected rel=last in Link, got %s", link)
	}
	if !strings.Contains(link, "<"+s.baseURL+"/api/v3/") {
		t.Fatalf("expected absolute pagination targets rooted at %s, got %s", s.baseURL, link)
	}
}

func TestPaginationSecondPage(t *testing.T) {
	repoName := "pg-page2"
	createTestIssueRepo(t, repoName)

	for i := 0; i < 5; i++ {
		ghPost(t, "/api/v3/repos/admin/"+repoName+"/issues", defaultToken, map[string]interface{}{
			"title": "Issue",
		}).Body.Close()
	}

	// Page 2 with per_page=2
	resp := ghGet(t, "/api/v3/repos/admin/"+repoName+"/issues?per_page=2&page=2", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	issues := decodeJSONArray(t, resp)
	if len(issues) != 2 {
		t.Fatalf("expected 2 issues on page 2, got %d", len(issues))
	}

	link := resp.Header.Get("Link")
	if !strings.Contains(link, `rel="first"`) {
		t.Fatalf("expected rel=first on page 2, got %s", link)
	}
	if !strings.Contains(link, `rel="prev"`) {
		t.Fatalf("expected rel=prev on page 2, got %s", link)
	}
}

func TestPaginationLastPage(t *testing.T) {
	repoName := "pg-last"
	createTestIssueRepo(t, repoName)

	for i := 0; i < 5; i++ {
		ghPost(t, "/api/v3/repos/admin/"+repoName+"/issues", defaultToken, map[string]interface{}{
			"title": "Issue",
		}).Body.Close()
	}

	// Last page (page 3, per_page=2): should have 1 item
	resp := ghGet(t, "/api/v3/repos/admin/"+repoName+"/issues?per_page=2&page=3", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	issues := decodeJSONArray(t, resp)
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue on last page, got %d", len(issues))
	}

	link := resp.Header.Get("Link")
	// On last page, no next/last, but has first/prev
	if strings.Contains(link, `rel="next"`) {
		t.Fatalf("expected no rel=next on last page, got %s", link)
	}
	if !strings.Contains(link, `rel="first"`) {
		t.Fatalf("expected rel=first on last page, got %s", link)
	}
}

func TestPaginationPerPageMaxClamp(t *testing.T) {
	repoName := "pg-clamp"
	createTestIssueRepo(t, repoName)

	for i := 0; i < 3; i++ {
		ghPost(t, "/api/v3/repos/admin/"+repoName+"/issues", defaultToken, map[string]interface{}{
			"title": "Issue",
		}).Body.Close()
	}

	// per_page=200 should be clamped to 100
	resp := ghGet(t, "/api/v3/repos/admin/"+repoName+"/issues?per_page=200", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	issues := decodeJSONArray(t, resp)
	if len(issues) != 3 {
		t.Fatalf("expected 3 issues, got %d", len(issues))
	}
}

func TestPaginationRejectsMalformedAndNonPositiveValues(t *testing.T) {
	for _, query := range []string{"page=abc", "page=0", "page=-1", "per_page=nope", "per_page=0", "per_page=-1"} {
		resp := ghGet(t, "/api/v3/issues?"+query, defaultToken)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			body := decodeResponseBodyForTest(resp)
			t.Fatalf("%s status = %d, want 422; body=%s", query, resp.StatusCode, body)
		}
		resp.Body.Close()
	}
}

func TestPaginationBeyondRange(t *testing.T) {
	repoName := "pg-beyond"
	createTestIssueRepo(t, repoName)

	ghPost(t, "/api/v3/repos/admin/"+repoName+"/issues", defaultToken, map[string]interface{}{
		"title": "Issue",
	}).Body.Close()

	// Page 99 should return empty
	resp := ghGet(t, "/api/v3/repos/admin/"+repoName+"/issues?page=99", "")
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	defer resp.Body.Close()

	var issues []interface{}
	json.NewDecoder(resp.Body).Decode(&issues)
	if len(issues) != 0 {
		t.Fatalf("expected 0 issues beyond range, got %d", len(issues))
	}
}

func TestPaginationRepoList(t *testing.T) {
	// Create several repos
	for i := 0; i < 3; i++ {
		ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
			"name": "pg-repo-" + http.StatusText(200+i),
		}).Body.Close()
	}

	resp := ghGet(t, "/api/v3/user/repos?per_page=1", defaultToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	defer resp.Body.Close()

	var repos []interface{}
	json.NewDecoder(resp.Body).Decode(&repos)
	if len(repos) != 1 {
		t.Fatalf("expected 1 repo per page, got %d", len(repos))
	}

	link := resp.Header.Get("Link")
	if !strings.Contains(link, `rel="next"`) {
		t.Fatalf("expected Link header with next, got %s", link)
	}
}

func TestCommentSinceFilterIsSharedAndStrict(t *testing.T) {
	type item struct {
		id      int
		updated time.Time
	}
	items := []item{
		{id: 1, updated: time.Date(2035, time.June, 14, 11, 59, 59, 0, time.UTC)},
		{id: 2, updated: time.Date(2035, time.June, 14, 12, 0, 0, 0, time.UTC)},
	}
	request := httptest.NewRequest(http.MethodGet, "/comments?since=2035-06-14T12:00:00Z", nil)
	response := httptest.NewRecorder()
	filtered, ok := filterSince(response, request, "Comment", items, func(value item) time.Time {
		return value.updated
	})
	if !ok || len(filtered) != 1 || filtered[0].id != 2 {
		t.Fatalf("inclusive since filter = %v, %v; want item 2", filtered, ok)
	}

	request = httptest.NewRequest(http.MethodGet, "/comments?since=not-a-timestamp", nil)
	response = httptest.NewRecorder()
	if _, ok := filterSince(response, request, "Comment", items, func(value item) time.Time {
		return value.updated
	}); ok || response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid since = ok %v status %d, want false/422", ok, response.Code)
	}
}

func TestSingleCommitFilesPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "pg-commit-files", "", false)
	stor := s.store.GetGitStorage("admin", repo.Name)
	sig := &object.Signature{Name: admin.Login, Email: "admin@example.test", When: time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)}
	sha, err := initRepoWithFiles(stor, "main", "root", map[string]string{
		"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n",
	}, sig)
	if err != nil {
		t.Fatal(err)
	}
	token := s.store.CreateToken(admin.ID, "repo")

	page1 := tokenRequest(s, http.MethodGet, "/api/v3/repos/admin/"+repo.Name+"/commits/"+sha.String()+"?per_page=2", token.Value)
	if page1.Code != http.StatusOK {
		t.Fatalf("page 1 = %d: %s", page1.Code, page1.Body.String())
	}
	var commit1 map[string]interface{}
	if err := json.Unmarshal(page1.Body.Bytes(), &commit1); err != nil {
		t.Fatal(err)
	}
	files1, _ := commit1["files"].([]interface{})
	if len(files1) != 2 {
		t.Fatalf("page 1 files = %d, want 2", len(files1))
	}
	if link := page1.Header().Get("Link"); !strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 1 Link = %q, want rel=next", link)
	}

	page2 := tokenRequest(s, http.MethodGet, "/api/v3/repos/admin/"+repo.Name+"/commits/"+sha.String()+"?per_page=2&page=2", token.Value)
	var commit2 map[string]interface{}
	if err := json.Unmarshal(page2.Body.Bytes(), &commit2); err != nil {
		t.Fatal(err)
	}
	files2, _ := commit2["files"].([]interface{})
	if len(files2) != 1 {
		t.Fatalf("page 2 files = %d, want 1", len(files2))
	}
	name1, _ := files1[0].(map[string]interface{})["filename"].(string)
	name2, _ := files2[0].(map[string]interface{})["filename"].(string)
	if name1 == name2 {
		t.Fatalf("pages overlap on %q", name1)
	}
}

func TestCompareRefsPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "pg-compare", "", false)
	stor := s.store.GetGitStorage("admin", repo.Name)
	sig := &object.Signature{Name: admin.Login, Email: "admin@example.test", When: time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)}
	base, err := initRepoWithFiles(stor, "main", "root", map[string]string{"a.txt": "a\n"}, sig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createFileCommit(stor, "main", "b.txt", "b\n", "second", sig); err != nil {
		t.Fatal(err)
	}
	head, err := createFileCommit(stor, "main", "c.txt", "c\n", "third", sig)
	if err != nil {
		t.Fatal(err)
	}
	token := s.store.CreateToken(admin.ID, "repo")
	comparePath := "/api/v3/repos/admin/" + repo.Name + "/compare/" + base.String() + "..." + head.String()

	page1 := tokenRequest(s, http.MethodGet, comparePath+"?per_page=1", token.Value)
	if page1.Code != http.StatusOK {
		t.Fatalf("page 1 = %d: %s", page1.Code, page1.Body.String())
	}
	var cmp1 map[string]interface{}
	if err := json.Unmarshal(page1.Body.Bytes(), &cmp1); err != nil {
		t.Fatal(err)
	}
	if total, _ := cmp1["total_commits"].(float64); total != 2 {
		t.Fatalf("page 1 total_commits = %v, want 2", cmp1["total_commits"])
	}
	commits1, _ := cmp1["commits"].([]interface{})
	if len(commits1) != 1 {
		t.Fatalf("page 1 commits = %d, want 1", len(commits1))
	}
	files1, _ := cmp1["files"].([]interface{})
	if len(files1) != 2 {
		t.Fatalf("page 1 files = %d, want the 2 changed files on the first page", len(files1))
	}
	if link := page1.Header().Get("Link"); !strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 1 Link = %q, want rel=next", link)
	}

	page2 := tokenRequest(s, http.MethodGet, comparePath+"?per_page=1&page=2", token.Value)
	var cmp2 map[string]interface{}
	if err := json.Unmarshal(page2.Body.Bytes(), &cmp2); err != nil {
		t.Fatal(err)
	}
	if total, _ := cmp2["total_commits"].(float64); total != 2 {
		t.Fatalf("page 2 total_commits = %v, want 2", cmp2["total_commits"])
	}
	commits2, _ := cmp2["commits"].([]interface{})
	if len(commits2) != 1 {
		t.Fatalf("page 2 commits = %d, want 1", len(commits2))
	}
	files2, _ := cmp2["files"].([]interface{})
	if len(files2) != 0 {
		t.Fatalf("page 2 files = %d, want 0 — the file list only rides the first page", len(files2))
	}
	sha1, _ := commits1[0].(map[string]interface{})["sha"].(string)
	sha2, _ := commits2[0].(map[string]interface{})["sha"].(string)
	if sha1 == sha2 {
		t.Fatalf("pages overlap on commit %s", sha1)
	}

	unpaged := tokenRequest(s, http.MethodGet, comparePath, token.Value)
	var cmpAll map[string]interface{}
	if err := json.Unmarshal(unpaged.Body.Bytes(), &cmpAll); err != nil {
		t.Fatal(err)
	}
	if commits, _ := cmpAll["commits"].([]interface{}); len(commits) != 2 {
		t.Fatalf("unpaged commits = %d, want 2", len(commits))
	}
}

func TestCombinedStatusPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "pg-combined-status", "", false)
	stor := s.store.GetGitStorage("admin", repo.Name)
	sig := &object.Signature{Name: admin.Login, Email: "admin@example.test", When: time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)}
	sha, err := initRepoWithFiles(stor, "main", "root", map[string]string{"a.txt": "a\n"}, sig)
	if err != nil {
		t.Fatal(err)
	}
	token := s.store.CreateToken(admin.ID, "repo")
	for _, ctx := range []string{"ci/build", "ci/test"} {
		resp := pagedJSONRequest(t, s, http.MethodPost, "/api/v3/repos/admin/"+repo.Name+"/statuses/"+sha.String(), token.Value, map[string]interface{}{
			"state":   "success",
			"context": ctx,
		})
		if resp.Code != http.StatusCreated {
			t.Fatalf("create status %s = %d: %s", ctx, resp.Code, resp.Body.String())
		}
	}

	page1 := tokenRequest(s, http.MethodGet, "/api/v3/repos/admin/"+repo.Name+"/commits/"+sha.String()+"/status?per_page=1", token.Value)
	if page1.Code != http.StatusOK {
		t.Fatalf("page 1 = %d: %s", page1.Code, page1.Body.String())
	}
	var combined1 map[string]interface{}
	if err := json.Unmarshal(page1.Body.Bytes(), &combined1); err != nil {
		t.Fatal(err)
	}
	if total, _ := combined1["total_count"].(float64); total != 2 {
		t.Fatalf("page 1 total_count = %v, want 2", combined1["total_count"])
	}
	statuses1, _ := combined1["statuses"].([]interface{})
	if len(statuses1) != 1 {
		t.Fatalf("page 1 statuses = %d, want 1", len(statuses1))
	}
	if link := page1.Header().Get("Link"); !strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 1 Link = %q, want rel=next", link)
	}

	page2 := tokenRequest(s, http.MethodGet, "/api/v3/repos/admin/"+repo.Name+"/commits/"+sha.String()+"/status?per_page=1&page=2", token.Value)
	var combined2 map[string]interface{}
	if err := json.Unmarshal(page2.Body.Bytes(), &combined2); err != nil {
		t.Fatal(err)
	}
	if total, _ := combined2["total_count"].(float64); total != 2 {
		t.Fatalf("page 2 total_count = %v, want 2", combined2["total_count"])
	}
	statuses2, _ := combined2["statuses"].([]interface{})
	if len(statuses2) != 1 {
		t.Fatalf("page 2 statuses = %d, want 1", len(statuses2))
	}
	ctx1, _ := statuses1[0].(map[string]interface{})["context"].(string)
	ctx2, _ := statuses2[0].(map[string]interface{})["context"].(string)
	if ctx1 == ctx2 {
		t.Fatalf("pages overlap on context %q", ctx1)
	}
}

func TestRepoTopicsPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "pg-topics", "", false)
	token := s.store.CreateToken(admin.ID, "repo")
	put := pagedJSONRequest(t, s, http.MethodPut, "/api/v3/repos/admin/"+repo.Name+"/topics", token.Value, map[string]interface{}{
		"names": []string{"alpha", "beta", "gamma"},
	})
	if put.Code != http.StatusOK {
		t.Fatalf("put topics = %d: %s", put.Code, put.Body.String())
	}

	page1 := tokenRequest(s, http.MethodGet, "/api/v3/repos/admin/"+repo.Name+"/topics?per_page=2", token.Value)
	if page1.Code != http.StatusOK {
		t.Fatalf("page 1 = %d: %s", page1.Code, page1.Body.String())
	}
	var topics1 map[string]interface{}
	if err := json.Unmarshal(page1.Body.Bytes(), &topics1); err != nil {
		t.Fatal(err)
	}
	names1, _ := topics1["names"].([]interface{})
	if len(names1) != 2 {
		t.Fatalf("page 1 names = %d, want 2", len(names1))
	}
	if link := page1.Header().Get("Link"); !strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 1 Link = %q, want rel=next", link)
	}

	page2 := tokenRequest(s, http.MethodGet, "/api/v3/repos/admin/"+repo.Name+"/topics?per_page=2&page=2", token.Value)
	var topics2 map[string]interface{}
	if err := json.Unmarshal(page2.Body.Bytes(), &topics2); err != nil {
		t.Fatal(err)
	}
	names2, _ := topics2["names"].([]interface{})
	if len(names2) != 1 {
		t.Fatalf("page 2 names = %d, want 1", len(names2))
	}
}

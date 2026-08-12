package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// These are semantic compatibility vectors, not response-shape snapshots.
// Each reproduces a request accepted by github.com and pins the observable
// behavior that OpenAPI schemas cannot express.

func TestJSONReadsHonorIfNoneMatch(t *testing.T) {
	s := fuzzRoutedServer(t)
	handler := s.requestHandler()
	path := "/api/v3/user"

	request := func(ifNoneMatch string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+defaultToken)
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	first := request("")
	if first.Code != http.StatusOK {
		t.Fatalf("initial status = %d, want 200; body=%s", first.Code, first.Body.String())
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("JSON GET omitted ETag")
	}
	second := request(etag)
	if second.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304; body=%s", second.Code, second.Body.String())
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 response carried %d body bytes", second.Body.Len())
	}
}

func TestSearchRepositoriesTopicQualifierUsesExactTopicMatches(t *testing.T) {
	s := fuzzRoutedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "topic-fidelity-org", "", "")
	if org == nil {
		t.Fatal("create organization")
	}
	golden := s.store.CreateOrgRepo(org, admin, "golden", "", false)
	partial := s.store.CreateOrgRepo(org, admin, "partial", "mentions golden-path", false)
	if golden == nil || partial == nil {
		t.Fatal("create repositories")
	}
	s.store.UpdateRepo(org.Login, golden.Name, func(repo *store.Repo) {
		repo.Topics = []string{"golden-path", "banking"}
	})
	s.store.UpdateRepo(org.Login, partial.Name, func(repo *store.Repo) {
		repo.Topics = []string{"golden"}
	})

	query := url.QueryEscape("org:" + org.Login + " topic:golden-path topic:BANKING")
	w := fuzzServe(s, http.MethodGet, "/api/v3/search/repositories?q="+query, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var result struct {
		TotalCount int `json:"total_count"`
		Items      []struct {
			FullName string `json:"full_name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.TotalCount != 1 || len(result.Items) != 1 || result.Items[0].FullName != golden.FullName {
		t.Fatalf("topic-qualified result = %+v, want only %s", result, golden.FullName)
	}

	for name, path := range map[string]string{
		"missing-q":    "/api/v3/search/repositories",
		"invalid-sort": "/api/v3/search/repositories?q=repo&sort=created",
		"invalid-order": "/api/v3/search/repositories?q=repo&order=" +
			url.QueryEscape("sideways"),
	} {
		t.Run(name, func(t *testing.T) {
			w := fuzzServe(s, http.MethodGet, path, nil)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestPullListHeadQualifierAndLabelsUseOwnerColonRef(t *testing.T) {
	s, admin, baseRepo := pullsTestServer(t)
	org := s.store.CreateOrg(admin, "head-fidelity-org", "", "")
	if org == nil {
		t.Fatal("create organization")
	}
	headRepo := s.store.CreateOrgRepo(org, admin, baseRepo.Name, "", false)
	if headRepo == nil {
		t.Fatal("create head repository")
	}
	seedPullRequestBranches(t, s, headRepo, "feature")
	pr := s.store.CreatePullRequest(baseRepo.ID, admin.ID, "Organization head", "", "feature", "main", false, nil, nil, 0, store.PullRequestOptions{
		HeadRepoID: headRepo.ID,
	})
	if pr == nil {
		t.Fatal("create pull request")
	}

	head := url.QueryEscape(org.Login + ":feature")
	w := doPullsReq(s, http.MethodGet, "/api/v3/repos/"+baseRepo.FullName+"/pulls?head="+head, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	prs := assertJSONArray(t, w)
	if len(prs) != 1 {
		t.Fatalf("head-qualified pull count = %d, want 1; body=%s", len(prs), w.Body.String())
	}
	gotHead := prs[0]["head"].(map[string]interface{})
	gotBase := prs[0]["base"].(map[string]interface{})
	if gotHead["label"] != org.Login+":feature" {
		t.Errorf("head.label = %q, want %q", gotHead["label"], org.Login+":feature")
	}
	if gotBase["label"] != "admin:main" {
		t.Errorf("base.label = %q, want admin:main", gotBase["label"])
	}
}

func TestPullListValidatesAndAppliesDocumentedSortOptions(t *testing.T) {
	s, admin, repo := pullsTestServer(t)
	older := s.store.CreatePullRequest(repo.ID, admin.ID, "Older", "", "feat-a", "main", false, nil, nil, 0)
	newer := s.store.CreatePullRequest(repo.ID, admin.ID, "Newer", "", "feat-b", "main", false, nil, nil, 0)
	if older == nil || newer == nil {
		t.Fatal("create pull requests")
	}
	s.store.Mu.Lock()
	older.CreatedAt = older.CreatedAt.Add(-2 * 24 * time.Hour)
	older.UpdatedAt = older.UpdatedAt.Add(2 * time.Hour)
	newer.UpdatedAt = newer.UpdatedAt.Add(-2 * time.Hour)
	s.store.Mu.Unlock()

	assertOrder := func(query string, wantFirst int) {
		t.Helper()
		w := doPullsReq(s, http.MethodGet, "/api/v3/repos/"+repo.FullName+"/pulls?"+query, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d; body=%s", query, w.Code, w.Body.String())
		}
		items := assertJSONArray(t, w)
		if len(items) != 2 || int(items[0]["number"].(float64)) != wantFirst {
			t.Fatalf("%s order = %+v, want PR #%d first", query, items, wantFirst)
		}
	}
	assertOrder("", newer.Number) // created defaults to descending
	assertOrder("sort=updated", newer.Number)
	assertOrder("sort=updated&direction=desc", older.Number)

	for name, query := range map[string]string{
		"state":     "state=merged",
		"sort":      "sort=comments",
		"direction": "direction=sideways",
	} {
		t.Run("invalid-"+name, func(t *testing.T) {
			w := doPullsReq(s, http.MethodGet, "/api/v3/repos/"+repo.FullName+"/pulls?"+query, "")
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCreateTreeCanonicalizesEntriesMergedWithBaseTree(t *testing.T) {
	s := gitDataTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "tree-order-fidelity", "", false)

	blob := func(content string) string {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"content": content})
		w := doMiscReq(s, http.MethodPost, "/api/v3/repos/"+repo.FullName+"/git/blobs", string(body))
		if w.Code != http.StatusCreated {
			t.Fatalf("create blob status = %d; body=%s", w.Code, w.Body.String())
		}
		var out struct {
			SHA string `json:"sha"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.SHA
	}
	createTree := func(body map[string]interface{}) *jsonTreeResponse {
		t.Helper()
		raw, _ := json.Marshal(body)
		w := doMiscReq(s, http.MethodPost, "/api/v3/repos/"+repo.FullName+"/git/trees", string(raw))
		if w.Code != http.StatusCreated {
			t.Fatalf("create tree status = %d, want 201; body=%s", w.Code, w.Body.String())
		}
		var out jsonTreeResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return &out
	}

	base := createTree(map[string]interface{}{
		"tree": []map[string]interface{}{{"path": "z.txt", "mode": "100644", "type": "blob", "sha": blob("z")}},
	})
	merged := createTree(map[string]interface{}{
		"base_tree": base.SHA,
		"tree":      []map[string]interface{}{{"path": "a.txt", "mode": "100644", "type": "blob", "sha": blob("a")}},
	})
	if len(merged.Tree) != 2 || merged.Tree[0].Path != "a.txt" || merged.Tree[1].Path != "z.txt" {
		t.Fatalf("merged tree order = %+v, want [a.txt z.txt]", merged.Tree)
	}

	// Empty content is still content, nested paths build intermediate trees,
	// and sha:null deletes an entry inherited from base_tree.
	emptyTree := createTree(map[string]interface{}{"tree": []map[string]interface{}{}})
	withNested := createTree(map[string]interface{}{
		"base_tree": merged.SHA,
		"tree": []map[string]interface{}{
			{"path": "empty-dir", "mode": "040000", "type": "tree", "sha": emptyTree.SHA},
			{"path": "nested/empty.txt", "mode": "100644", "type": "blob", "content": ""},
		},
	})
	deleted := createTree(map[string]interface{}{
		"base_tree": withNested.SHA,
		"tree": []map[string]interface{}{{
			"path": "z.txt", "sha": nil,
		}},
	})
	rawRecursive := doMiscReq(s, http.MethodGet, "/api/v3/repos/"+repo.FullName+"/git/trees/"+deleted.SHA+"?recursive=1", "")
	if rawRecursive.Code != http.StatusOK {
		t.Fatalf("get recursive tree status = %d; body=%s", rawRecursive.Code, rawRecursive.Body.String())
	}
	var recursive jsonTreeResponse
	if err := json.Unmarshal(rawRecursive.Body.Bytes(), &recursive); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range recursive.Tree {
		paths = append(paths, entry.Path)
	}
	if len(paths) != 4 || paths[0] != "a.txt" || paths[1] != "empty-dir" ||
		paths[2] != "nested" || paths[3] != "nested/empty.txt" {
		t.Fatalf("tree after nested create/delete = %v, want [a.txt empty-dir nested nested/empty.txt]", paths)
	}

	for name, request := range map[string]map[string]interface{}{
		"missing-tree": {},
		"sha-and-content": {
			"tree": []map[string]interface{}{{
				"path": "bad.txt", "sha": blob("bad"), "content": "also bad",
			}},
		},
		"tree-with-blob-mode": {
			"tree": []map[string]interface{}{{
				"path": "bad-tree", "mode": "100644", "type": "tree", "sha": emptyTree.SHA,
			}},
		},
		"delete-missing": {
			"base_tree": deleted.SHA,
			"tree":      []map[string]interface{}{{"path": "missing.txt", "sha": nil}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(request)
			w := doMiscReq(s, http.MethodPost, "/api/v3/repos/"+repo.FullName+"/git/trees", string(raw))
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCreateUserCodespaceAcceptsPullRequestSelector(t *testing.T) {
	s := fuzzRoutedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "codespace-selector-fidelity", "", false)
	if repo == nil {
		t.Fatal("create repository")
	}
	seedPullRequestBranches(t, s, repo, "codespace-head")
	pr := s.store.CreatePullRequest(repo.ID, admin.ID, "Codespace head", "", "codespace-head", "main", false, nil, nil, 0)
	if pr == nil {
		t.Fatal("create pull request")
	}
	t.Setenv("PATH", t.TempDir()) // exercise the built-in workspace runtime

	body := map[string]interface{}{
		"pull_request": map[string]interface{}{
			"pull_request_number": pr.Number,
			"repository_id":       repo.ID,
		},
		"display_name": "PR Codespace",
	}
	raw, _ := json.Marshal(body)
	w := fuzzServe(s, http.MethodPost, "/api/v3/user/codespaces", raw)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var created map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	cs := s.store.GetCodespaceByName(created["name"].(string))
	if cs == nil || cs.GitRef != pr.HeadRefName || cs.Runtime != "workspace" {
		t.Fatalf("created codespace = %+v, want ref %q using workspace runtime", cs, pr.HeadRefName)
	}
	t.Cleanup(func() {
		_, _ = s.store.DeleteCodespace(cs.ID)
	})

	for name, request := range map[string]map[string]interface{}{
		"neither-selector": {},
		"both-selectors": {
			"repository_id": repo.ID,
			"pull_request": map[string]interface{}{
				"pull_request_number": pr.Number,
				"repository_id":       repo.ID,
			},
		},
		"unknown-pull-request": {
			"pull_request": map[string]interface{}{
				"pull_request_number": 999999,
				"repository_id":       repo.ID,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(request)
			w := fuzzServe(s, http.MethodPost, "/api/v3/user/codespaces", raw)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

type jsonTreeResponse struct {
	SHA  string `json:"sha"`
	Tree []struct {
		Path string `json:"path"`
	} `json:"tree"`
}

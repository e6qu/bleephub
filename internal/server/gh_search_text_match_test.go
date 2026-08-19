package bleephub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const textMatchAccept = "application/vnd.github.text-match+json"

func searchTextMatchServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer()
	s.registerGHSearchRoutes()
	return s
}

// doSearchReq performs an authenticated search with an optional Accept header.
func doSearchReq(s *Server, path, accept string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+adminPAT)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(w, req)
	return w
}

func searchItems(t *testing.T, w *httptest.ResponseRecorder) []map[string]interface{} {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("search status = %d, body = %s", w.Code, w.Body.String())
	}
	var envelope struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	return envelope.Items
}

// requireTextMatch finds the text-match entry for property and checks the
// documented invariants: object metadata, the fragment containing the term,
// and every match's indices addressing exactly its text within the fragment.
func requireTextMatch(t *testing.T, item map[string]interface{}, objectType, property, term string) map[string]interface{} {
	t.Helper()
	raw, ok := item["text_matches"].([]interface{})
	if !ok {
		t.Fatalf("text_matches missing or not an array: %v", item["text_matches"])
	}
	for _, entry := range raw {
		m, ok := entry.(map[string]interface{})
		if !ok || m["property"] != property {
			continue
		}
		if m["object_type"] != objectType {
			t.Fatalf("object_type = %v, want %s", m["object_type"], objectType)
		}
		if objectURL, _ := m["object_url"].(string); objectURL == "" {
			t.Fatalf("object_url missing: %v", m)
		}
		fragment, _ := m["fragment"].(string)
		if !strings.Contains(strings.ToLower(fragment), term) {
			t.Fatalf("fragment %q does not contain %q", fragment, term)
		}
		matches, _ := m["matches"].([]interface{})
		if len(matches) == 0 {
			t.Fatalf("matches empty for property %s", property)
		}
		for _, rawMatch := range matches {
			match := rawMatch.(map[string]interface{})
			indices := match["indices"].([]interface{})
			start, end := int(indices[0].(float64)), int(indices[1].(float64))
			text, _ := match["text"].(string)
			if start < 0 || end > len(fragment) || fragment[start:end] != text {
				t.Fatalf("indices [%d,%d] do not address %q within fragment %q", start, end, text, fragment)
			}
			if !strings.EqualFold(text, term) {
				t.Fatalf("match text = %q, want the term %q", text, term)
			}
		}
		return m
	}
	t.Fatalf("no text-match entry for property %q: %v", property, raw)
	return nil
}

func TestSearchTextMatches_Issues(t *testing.T) {
	s := searchTextMatchServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "tm-issues", "", false)
	padding := strings.Repeat("x", 150)
	s.store.CreateIssue(repo.ID, admin.ID, "font problem",
		padding+" needlemark appears here "+padding, nil, nil, 0)

	path := "/api/v3/search/issues?q=" + url.QueryEscape("needlemark is:issue")

	// Without the media type the payload is unchanged.
	for _, item := range searchItems(t, doSearchReq(s, path, "")) {
		if _, present := item["text_matches"]; present {
			t.Fatal("text_matches present without the text-match Accept header")
		}
	}

	items := searchItems(t, doSearchReq(s, path, textMatchAccept))
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	m := requireTextMatch(t, items[0], "Issue", "body", "needlemark")
	fragment := m["fragment"].(string)
	// The window is ±~100 characters around the hit, not the full 300+ body.
	if len(fragment) > 250 {
		t.Fatalf("fragment len = %d, want a bounded window", len(fragment))
	}
	if objectURL := m["object_url"].(string); !strings.Contains(objectURL, "/api/v3/repos/admin/tm-issues/issues/") {
		t.Fatalf("object_url = %q", objectURL)
	}
}

func TestSearchTextMatches_TitleProperty(t *testing.T) {
	s := searchTextMatchServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "tm-title", "", false)
	s.store.CreateIssue(repo.ID, admin.ID, "quillbrace in the title", "unrelated body", nil, nil, 0)

	items := searchItems(t, doSearchReq(s,
		"/api/v3/search/issues?q="+url.QueryEscape("quillbrace is:issue"), textMatchAccept))
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	requireTextMatch(t, items[0], "Issue", "title", "quillbrace")
	// The body does not contain the term, so no body entry appears.
	for _, entry := range items[0]["text_matches"].([]interface{}) {
		if entry.(map[string]interface{})["property"] == "body" {
			t.Fatalf("unexpected body text-match: %v", entry)
		}
	}
}

func TestSearchTextMatches_CodeAndCommits(t *testing.T) {
	s := searchTextMatchServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "tm-code", "glowfilament marker readme", false)
	if err := s.initRepoFiles(context.Background(), repo, "main", repo.Description, "", "", true); err != nil {
		t.Fatalf("init repo files: %v", err)
	}

	// Code: the term lives in README.md content.
	items := searchItems(t, doSearchReq(s,
		"/api/v3/search/code?q=glowfilament", textMatchAccept))
	if len(items) != 1 {
		t.Fatalf("code items = %d, want 1", len(items))
	}
	m := requireTextMatch(t, items[0], "FileContent", "content", "glowfilament")
	if objectURL := m["object_url"].(string); !strings.HasSuffix(objectURL, "/contents/README.md") {
		t.Fatalf("code object_url = %q", objectURL)
	}

	// Commits: the seeded history's message is "Initial commit".
	items = searchItems(t, doSearchReq(s,
		"/api/v3/search/commits?q="+url.QueryEscape("initial repo:admin/tm-code"), textMatchAccept))
	if len(items) != 1 {
		t.Fatalf("commit items = %d, want 1", len(items))
	}
	requireTextMatch(t, items[0], "Commit", "message", "initial")
}

func TestSearchTextMatches_Repositories(t *testing.T) {
	s := searchTextMatchServer(t)
	admin := s.store.UsersByLogin["admin"]
	s.store.CreateRepo(admin, "tm-repo", "a wondergasket for everyone", false)

	items := searchItems(t, doSearchReq(s,
		"/api/v3/search/repositories?q=wondergasket", textMatchAccept))
	if len(items) != 1 {
		t.Fatalf("repo items = %d, want 1", len(items))
	}
	requireTextMatch(t, items[0], "Repository", "description", "wondergasket")

	// The alternate accepted spelling of the media type works too.
	items = searchItems(t, doSearchReq(s,
		"/api/v3/search/repositories?q=wondergasket", "application/vnd.github.v3.text-match+json"))
	requireTextMatch(t, items[0], "Repository", "description", "wondergasket")
}

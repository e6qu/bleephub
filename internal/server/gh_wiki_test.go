package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func wikiTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer()
	s.registerGHRepoRoutes()
	s.registerGHWikiRoutes()
	return s
}

func enableWiki(s *Server, repo *store.Repo) {
	s.store.Mu.Lock()
	if r := s.store.ReposByName[repo.FullName]; r != nil {
		r.HasWiki = true
	}
	s.store.Mu.Unlock()
}

func doWikiReq(s *Server, token, method, path string, body []byte) *httptest.ResponseRecorder {
	return serveTestRequest(s, bearerHeader(token), method, path, body)
}

func TestWiki_PutGetListUpdateDelete(t *testing.T) {
	s := wikiTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "wiki-repo", "", false)
	enableWiki(s, repo)
	base := "/ui-data/repos/" + repo.FullName + "/wiki/pages"

	// create
	w := doWikiReq(s, adminPAT, "PUT", base+"/home", []byte(`{"title":"Home","body":"# Welcome"}`))
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", w.Code, w.Body.String())
	}
	var created map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &created)
	if created["title"] != "Home" || created["body"] != "# Welcome" || created["slug"] != "home" {
		t.Fatalf("created page = %v", created)
	}

	// get
	w = doWikiReq(s, adminPAT, "GET", base+"/home", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d", w.Code)
	}

	// list
	w = doWikiReq(s, adminPAT, "GET", base, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	var list []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}

	// update (same slug → 200, not 201)
	w = doWikiReq(s, adminPAT, "PUT", base+"/home", []byte(`{"title":"Home","body":"# Updated"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200", w.Code)
	}
	w = doWikiReq(s, adminPAT, "GET", base+"/home", nil)
	var got map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got["body"] != "# Updated" {
		t.Fatalf("body after update = %v", got["body"])
	}

	// delete
	w = doWikiReq(s, adminPAT, "DELETE", base+"/home", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", w.Code)
	}
	w = doWikiReq(s, adminPAT, "GET", base+"/home", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", w.Code)
	}
}

func TestWiki_DisabledWikiIs404(t *testing.T) {
	s := wikiTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "no-wiki", "", false)
	s.store.UpdateRepo(admin.Login, "no-wiki", func(r *store.Repo) { r.HasWiki = false }) // github default is on; disable to test the 404 path

	w := doWikiReq(s, adminPAT, "GET", "/ui-data/repos/"+repo.FullName+"/wiki/pages", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled-wiki list status = %d, want 404", w.Code)
	}
	w = doWikiReq(s, adminPAT, "PUT", "/ui-data/repos/"+repo.FullName+"/wiki/pages/home", []byte(`{"title":"X","body":"y"}`))
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled-wiki put status = %d, want 404", w.Code)
	}
}

func TestWiki_PutMissingTitleIs422(t *testing.T) {
	s := wikiTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "wiki-missing", "", false)
	enableWiki(s, repo)

	w := doWikiReq(s, adminPAT, "PUT", "/ui-data/repos/"+repo.FullName+"/wiki/pages/home", []byte(`{"body":"no title"}`))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing-title status = %d, want 422", w.Code)
	}
}

func TestWiki_ReadCollaboratorCannotWrite(t *testing.T) {
	s := wikiTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "wiki-priv", "", true)
	enableWiki(s, repo)

	s.store.Mu.Lock()
	other := &store.User{ID: s.store.NextUser, Login: "reader", Type: "User", Email: "reader@bleephub.local"}
	s.store.NextUser++
	s.store.Users[other.ID] = other
	s.store.UsersByLogin[other.Login] = other
	s.store.Mu.Unlock()
	s.store.AddRepoCollaborator(repo.Owner.Login, repo.Name, other.Login, "pull")
	readerToken := s.store.CreateToken(other.ID, "repo").Value

	// A pull-only collaborator can read the wiki but not write it.
	w := doWikiReq(s, readerToken, "GET", "/ui-data/repos/"+repo.FullName+"/wiki/pages", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("reader list status = %d, want 200", w.Code)
	}
	w = doWikiReq(s, readerToken, "PUT", "/ui-data/repos/"+repo.FullName+"/wiki/pages/home", []byte(`{"title":"Home","body":"x"}`))
	if w.Code != http.StatusForbidden {
		t.Fatalf("reader write status = %d, want 403", w.Code)
	}
}

func TestWiki_PrivateNoReadIs404(t *testing.T) {
	s := wikiTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "wiki-secret", "", true)
	enableWiki(s, repo)
	s.store.UpsertWikiPage(repo.FullName, "home", "Home", "secret", "admin", "")

	w := doWikiReq(s, "", "GET", "/ui-data/repos/"+repo.FullName+"/wiki/pages", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unauthed private wiki list = %d, want 404", w.Code)
	}
}

func TestWikiSlug(t *testing.T) {
	cases := map[string]string{
		"Home":            "home",
		"Getting Started": "getting-started",
		"  Spaces  Here ": "spaces-here",
		"API/Reference":   "api-reference",
		"__weird__name__": "weird-name",
	}
	for in, want := range cases {
		if got := store.WikiSlug(in); got != want {
			t.Errorf("WikiSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

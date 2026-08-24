package bleephub

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func TestWikiRevisions_AppendedOnPutAndListedNewestFirst(t *testing.T) {
	s := wikiTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "wiki-rev", "", false)
	enableWiki(s, repo)
	base := "/ui-data/repos/" + repo.FullName + "/wiki/pages"

	for _, put := range []string{
		`{"title":"Home","body":"first body","message":"initial"}`,
		`{"title":"Home","body":"second body"}`,
		`{"title":"Home","body":"third body","message":"typo fix"}`,
	} {
		w := doWikiReq(s, adminPAT, "PUT", base+"/home", []byte(put))
		if w.Code != http.StatusOK && w.Code != http.StatusCreated {
			t.Fatalf("put status = %d, body = %s", w.Code, w.Body.String())
		}
	}

	w := doWikiReq(s, adminPAT, "GET", base+"/home/revisions", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list revisions status = %d, body = %s", w.Code, w.Body.String())
	}
	var revisions []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &revisions)
	if len(revisions) != 3 {
		t.Fatalf("revisions len = %d, want 3", len(revisions))
	}
	newest := revisions[0]
	if newest["id"] != float64(3) || newest["message"] != "typo fix" || newest["editor"] != "admin" {
		t.Fatalf("newest revision = %v", newest)
	}
	if _, hasBody := newest["body"]; hasBody {
		t.Fatalf("revision listing must not carry full bodies: %v", newest)
	}
	if newest["body_preview"] != "third body" {
		t.Fatalf("body_preview = %v", newest["body_preview"])
	}
	if revisions[2]["id"] != float64(1) || revisions[2]["message"] != "initial" {
		t.Fatalf("oldest revision = %v", revisions[2])
	}

	// Full body via the single-revision read; reverting is a client PUT of it.
	w = doWikiReq(s, adminPAT, "GET", base+"/home/revisions/2", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get revision status = %d, body = %s", w.Code, w.Body.String())
	}
	var rev map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &rev)
	if rev["body"] != "second body" || rev["id"] != float64(2) {
		t.Fatalf("revision 2 = %v", rev)
	}

	// Unknown revision and unknown page both 404.
	if w := doWikiReq(s, adminPAT, "GET", base+"/home/revisions/99", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown revision status = %d, want 404", w.Code)
	}
	if w := doWikiReq(s, adminPAT, "GET", base+"/ghost/revisions", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown page revisions status = %d, want 404", w.Code)
	}
}

func TestWikiRevisions_PrivateRepoIs404ForOutsiders(t *testing.T) {
	s := wikiTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "wiki-rev-secret", "", true)
	enableWiki(s, repo)
	s.store.UpsertWikiPage(repo.FullName, "home", "Home", "secret", "admin", "")

	s.store.Mu.Lock()
	outsider := &store.User{ID: s.store.NextUser, Login: "rev-outsider", Type: "User"}
	s.store.NextUser++
	s.store.Users[outsider.ID] = outsider
	s.store.UsersByLogin[outsider.Login] = outsider
	s.store.Mu.Unlock()
	outsiderToken := s.store.CreateToken(outsider.ID, "repo").Value

	base := "/ui-data/repos/" + repo.FullName + "/wiki/pages/home/revisions"
	if w := doWikiReq(s, outsiderToken, "GET", base, nil); w.Code != http.StatusNotFound {
		t.Fatalf("outsider revision list status = %d, want 404", w.Code)
	}
	if w := doWikiReq(s, outsiderToken, "GET", base+"/1", nil); w.Code != http.StatusNotFound {
		t.Fatalf("outsider revision read status = %d, want 404", w.Code)
	}
}

func TestWikiRevisions_HistoryCapDropsOldest(t *testing.T) {
	s := wikiTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "wiki-rev-cap", "", false)
	enableWiki(s, repo)

	// Every save carries a different body: a revision is a commit that changed
	// the page's file, so re-saving identical text is not a new revision.
	for i := 0; i < store.MaxWikiPageRevisions+5; i++ {
		s.store.UpsertWikiPage(repo.FullName, "home", "Home", "body "+strconv.Itoa(i), "admin", "")
	}
	revisions := s.store.ListWikiPageRevisions(repo.FullName, "home")
	if len(revisions) != store.MaxWikiPageRevisions {
		t.Fatalf("revisions len = %d, want cap %d", len(revisions), store.MaxWikiPageRevisions)
	}
	if revisions[0].ID != store.MaxWikiPageRevisions+5 {
		t.Fatalf("newest ID = %d, want %d", revisions[0].ID, store.MaxWikiPageRevisions+5)
	}
	if revisions[len(revisions)-1].ID != 6 {
		t.Fatalf("oldest retained ID = %d, want 6 (oldest five dropped)", revisions[len(revisions)-1].ID)
	}
}

func TestWikiRevisions_DeletedPageStartsFreshHistory(t *testing.T) {
	s := wikiTestServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "wiki-rev-del", "", false)
	enableWiki(s, repo)
	base := "/ui-data/repos/" + repo.FullName + "/wiki/pages"

	doWikiReq(s, adminPAT, "PUT", base+"/home", []byte(`{"title":"Home","body":"v1"}`))
	doWikiReq(s, adminPAT, "DELETE", base+"/home", nil)
	doWikiReq(s, adminPAT, "PUT", base+"/home", []byte(`{"title":"Home","body":"v2"}`))

	w := doWikiReq(s, adminPAT, "GET", base+"/home/revisions", nil)
	var revisions []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &revisions)
	if len(revisions) != 1 || revisions[0]["id"] != float64(1) {
		t.Fatalf("recreated page history = %v, want a single fresh revision", revisions)
	}
}

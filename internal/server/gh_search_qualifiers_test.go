package bleephub

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"testing"
)

func searchIssueTitles(t *testing.T, s *isolatedServer, query string) []string {
	t.Helper()
	resp := s.authedGet(t, "/api/v3/search/issues?q="+url.QueryEscape(query))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("search %q: status %d body=%s", query, resp.StatusCode, body)
	}
	var out struct {
		Items []struct {
			Title string `json:"title"`
		} `json:"items"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	titles := make([]string, 0, len(out.Items))
	for _, it := range out.Items {
		titles = append(titles, it.Title)
	}
	sort.Strings(titles)
	return titles
}

func eqTitles(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Issue-search now honors author:/assignee:/milestone:/no:* and ANDs multiple
// label: qualifiers (previously 422'd or silently dropped — round-2 product
// semantics).
func TestSearchIssueQualifiers(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "iq", "", false)
	alice := s.createTestUser(t, "alice")
	bob := s.createTestUser(t, "bob")
	bug := s.store.CreateLabel(repo.ID, "regression", "", "d73a4a")
	urgent := s.store.CreateLabel(repo.ID, "urgent", "", "b60205")
	ms := s.store.CreateMilestone(repo.ID, admin.ID, "v1", "", "open", nil)

	// one: by alice, assigned bob, labels regression+urgent, milestone v1.
	s.store.CreateIssue(repo.ID, alice.ID, "one", "hello @carol", []int{bug.ID, urgent.ID}, []int{bob.ID}, ms.ID)
	// two: by bob, no assignee, label regression only, no milestone.
	s.store.CreateIssue(repo.ID, bob.ID, "two", "body", []int{bug.ID}, nil, 0)

	cases := []struct {
		q    string
		want []string
	}{
		{"repo:admin/iq author:alice", []string{"one"}},
		{"repo:admin/iq author:bob", []string{"two"}},
		{"repo:admin/iq assignee:bob", []string{"one"}},
		{"repo:admin/iq milestone:v1", []string{"one"}},
		{"repo:admin/iq label:regression label:urgent", []string{"one"}}, // multi-label AND (was a silent-drop bug)
		{"repo:admin/iq label:regression", []string{"one", "two"}},
		{"repo:admin/iq no:assignee", []string{"two"}},
		{"repo:admin/iq no:milestone", []string{"two"}},
		{"repo:admin/iq mentions:carol", []string{"one"}},
	}
	for _, c := range cases {
		got := searchIssueTitles(t, s, c.q)
		if !eqTitles(got, c.want) {
			t.Errorf("q=%q got %v, want %v", c.q, got, c.want)
		}
	}
}

func searchUserLogins(t *testing.T, s *isolatedServer, query string) map[string]bool {
	t.Helper()
	resp := s.authedGet(t, "/api/v3/search/users?q="+url.QueryEscape(query))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("search users %q: status %d body=%s", query, resp.StatusCode, body)
	}
	var out struct {
		Items []struct {
			Login string `json:"login"`
		} `json:"items"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	set := map[string]bool{}
	for _, it := range out.Items {
		set[it.Login] = true
	}
	return set
}

// User search now honors in:/repos:/location:/created: (round-3: closing the
// round-2 named remainder) instead of 422'ing them.
func TestSearchUserQualifiers(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	alice := s.createTestUser(t, "alice-u")
	bob := s.createTestUser(t, "bob-u")
	alice.Location = "Berlin"
	bob.Location = "Paris"
	// alice owns a Go repo; bob owns none.
	aliceRepo := s.store.CreateRepo(alice, "alice-repo", "", false)
	aliceRepo.Language = "Go"

	loc := searchUserLogins(t, s, "location:Berlin")
	if !loc["alice-u"] || loc["bob-u"] {
		t.Errorf("location:Berlin = %v, want alice-u only", loc)
	}
	repos := searchUserLogins(t, s, "repos:>=1")
	if !repos["alice-u"] || repos["bob-u"] {
		t.Errorf("repos:>=1 = %v, want alice-u (has a repo), not bob-u", repos)
	}
	// language: matches a user who owns a public repo in that language (round-4:
	// closing the round-3 named remainder) instead of 422'ing.
	lang := searchUserLogins(t, s, "language:Go")
	if !lang["alice-u"] || lang["bob-u"] {
		t.Errorf("language:Go = %v, want alice-u (owns a Go repo), not bob-u", lang)
	}
	// in:login restricts the free-text match to the login field.
	inLogin := searchUserLogins(t, s, "alice-u in:login")
	if !inLogin["alice-u"] {
		t.Errorf("`alice-u in:login` = %v, want alice-u", inLogin)
	}
}

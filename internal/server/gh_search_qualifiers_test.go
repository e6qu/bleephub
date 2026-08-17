package bleephub

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"testing"
)

// searchIssueTitles runs GET /search/issues?q=<query> and returns the sorted
// titles of the results (or fails on a non-200).
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
	bug := s.store.CreateLabel(repo.ID, "bug", "", "d73a4a")
	urgent := s.store.CreateLabel(repo.ID, "urgent", "", "b60205")
	ms := s.store.CreateMilestone(repo.ID, admin.ID, "v1", "", "open", nil)

	// one: by alice, assigned bob, labels bug+urgent, milestone v1.
	s.store.CreateIssue(repo.ID, alice.ID, "one", "hello @carol", []int{bug.ID, urgent.ID}, []int{bob.ID}, ms.ID)
	// two: by bob, no assignee, label bug only, no milestone.
	s.store.CreateIssue(repo.ID, bob.ID, "two", "body", []int{bug.ID}, nil, 0)

	cases := []struct {
		q    string
		want []string
	}{
		{"repo:admin/iq author:alice", []string{"one"}},
		{"repo:admin/iq author:bob", []string{"two"}},
		{"repo:admin/iq assignee:bob", []string{"one"}},
		{"repo:admin/iq milestone:v1", []string{"one"}},
		{"repo:admin/iq label:bug label:urgent", []string{"one"}}, // multi-label AND (was a silent-drop bug)
		{"repo:admin/iq label:bug", []string{"one", "two"}},
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

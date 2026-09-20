package bleephub

import (
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestAnOrganizationsPullRequestCapGovernsItsRepositories pins that the cap an
// organization sets is a cap, not a value that only reads back: a repository
// with no cap of its own is held to it, a repository's own cap takes its place,
// and a repository outside the organization is untouched.
func TestAnOrganizationsPullRequestCapGovernsItsRepositories(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "cap-org", "Cap Org", "")
	inside := s.store.CreateOrgRepo(org, admin, "inside", "", false)
	personal := s.store.CreateRepo(admin, "cap-personal", "", false)
	author, _ := s.newUser(t, "cap-author")
	seedPullRequestBranches(t, s.Server, inside, "one")
	seedPullRequestBranches(t, s.Server, personal, "one")

	open := func(repo *store.Repo, head string, draft bool) {
		t.Helper()
		if s.store.CreatePullRequest(repo.ID, author.ID, head, "", head, "main", draft, nil, nil, 0) == nil {
			t.Fatalf("create pull request %s", head)
		}
	}
	open(inside, "one", false)
	open(personal, "one", false)

	// Permitted: no cap anywhere.
	if !s.store.CanCreatePullRequest(inside.ID, author.ID, author.Login) {
		t.Fatal("with no cap, a second pull request was refused")
	}

	path := "/api/v3/orgs/" + org.Login + "/interaction-limits/pulls/creation-cap"
	set := decodeBody(t, s.patch(t, path, defaultToken, map[string]interface{}{"enabled": true, "max_open_pull_requests": 1}), http.StatusOK)
	if set["include_drafts"] != true {
		t.Fatalf("a new cap reports include_drafts = %v, want drafts counted by default", set["include_drafts"])
	}

	// Refused: the organization's cap holds its repository…
	if s.store.CanCreatePullRequest(inside.ID, author.ID, author.Login) {
		t.Error("the organization's cap did not hold a repository with no cap of its own")
	}
	// …and nothing else.
	if !s.store.CanCreatePullRequest(personal.ID, author.ID, author.Login) {
		t.Error("an organization's cap reached a repository outside it")
	}

	// A repository's own cap takes the organization's place.
	s.store.SetPRCreationCap(inside.FullName, store.PRCreationCap{Enabled: true, MaxOpenPullRequests: 5, IncludeDrafts: true})
	if !s.store.CanCreatePullRequest(inside.ID, author.ID, author.Login) {
		t.Error("a repository's own, looser cap did not take the organization's place")
	}
}

// TestDraftsCountTowardTheCapOnlyWhenItSaysSo covers include_drafts in both
// directions over the same pull requests.
func TestDraftsCountTowardTheCapOnlyWhenItSaysSo(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "cap-drafts", "", false)
	author, _ := s.newUser(t, "draft-author")
	seedPullRequestBranches(t, s.Server, repo, "wip")
	if s.store.CreatePullRequest(repo.ID, author.ID, "wip", "", "wip", "main", true, nil, nil, 0) == nil {
		t.Fatal("create the draft")
	}
	path := "/api/v3/repos/" + repo.FullName + "/interaction-limits/pulls/creation-cap"

	decodeBody(t, s.patch(t, path, defaultToken, map[string]interface{}{"enabled": true, "max_open_pull_requests": 1, "include_drafts": true}), http.StatusOK)
	if s.store.CanCreatePullRequest(repo.ID, author.ID, author.Login) {
		t.Error("with drafts counted, an author at the cap through a draft could open another")
	}

	updated := decodeBody(t, s.patch(t, path, defaultToken, map[string]interface{}{"enabled": true, "include_drafts": false}), http.StatusOK)
	if updated["include_drafts"] != false || updated["max_open_pull_requests"] != float64(1) {
		t.Fatalf("updated cap = %v, want include_drafts false and the limit kept", updated)
	}
	if !s.store.CanCreatePullRequest(repo.ID, author.ID, author.Login) {
		t.Error("with drafts not counted, a draft still used up the cap")
	}
	if got := decodeBody(t, s.get(t, path, defaultToken), http.StatusOK)["include_drafts"]; got != false {
		t.Errorf("stored include_drafts = %v", got)
	}
}

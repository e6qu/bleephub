package bleephub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"testing"
)

type installationSearchEnvelope struct {
	TotalCount int                      `json:"total_count"`
	Items      []map[string]interface{} `json:"items"`
}

func installationSearch(t *testing.T, s *Server, token, path string) installationSearchEnvelope {
	t.Helper()
	response := authedReqScheme(s, http.MethodGet, path, "bearer "+token, "")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body=%s", path, response.Code, response.Body.String())
	}
	var envelope installationSearchEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode GET %s: %v", path, err)
	}
	if envelope.TotalCount != len(envelope.Items) {
		t.Fatalf("GET %s total_count=%d items=%d", path, envelope.TotalCount, len(envelope.Items))
	}
	return envelope
}

func repositoriesInSearch(envelope installationSearchEnvelope) []string {
	names := make([]string, 0, len(envelope.Items))
	for _, item := range envelope.Items {
		if fullName, ok := item["full_name"].(string); ok {
			names = append(names, fullName)
			continue
		}
		if repository, ok := item["repository"].(map[string]interface{}); ok {
			if fullName, ok := repository["full_name"].(string); ok {
				names = append(names, fullName)
			}
		}
	}
	sort.Strings(names)
	return names
}

func requireSearchRepositories(t *testing.T, envelope installationSearchEnvelope, want ...string) {
	t.Helper()
	got := repositoriesInSearch(envelope)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("search repositories=%v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("search repositories=%v, want %v", got, want)
		}
	}
}

func TestInstallationTokensSearchTheirRepositorySelection(t *testing.T) {
	s := fuzzRoutedServer(t)
	admin := s.store.UsersByLogin["admin"]
	installedOrg := s.store.CreateOrg(admin, "search-installation", "", "")
	outsideOrg := s.store.CreateOrg(admin, "search-installation-outside", "", "")
	if installedOrg == nil || outsideOrg == nil {
		t.Fatal("create search organizations")
	}

	first := s.store.CreateOrgRepo(installedOrg, admin, "first-private", "installation-search-marker", true)
	second := s.store.CreateOrgRepo(installedOrg, admin, "second-private", "installation-search-marker", true)
	outsidePrivate := s.store.CreateOrgRepo(outsideOrg, admin, "outside-private", "installation-search-marker", true)
	outsidePublic := s.store.CreateOrgRepo(outsideOrg, admin, "outside-public", "installation-search-marker", false)
	if first == nil || second == nil || outsidePrivate == nil || outsidePublic == nil {
		t.Fatal("create search repositories")
	}

	app := s.store.CreateApp(admin.ID, "Installation search", "", nil, nil)
	installation := s.store.CreateInstallation(
		app.ID, "Organization", installedOrg.ID, installedOrg.Login, nil, nil,
	)
	allToken := s.store.CreateInstallationToken(installation.ID, app.ID, nil, nil)
	scopedToken := s.store.CreateInstallationToken(installation.ID, app.ID, nil, []int{first.ID})

	// Metadata read is mandatory and survives every persisted grant snapshot,
	// even when callers register the app and mint the token with no explicit
	// permissions.
	for name, permissions := range map[string]map[string]string{
		"app": app.Permissions, "installation": installation.Permissions, "token": allToken.Permissions,
	} {
		if permissions["metadata"] != "read" {
			t.Fatalf("%s metadata permission=%q, want read", name, permissions["metadata"])
		}
	}

	query := url.QueryEscape("installation-search-marker fork:true")
	all := installationSearch(t, s, allToken.Token, "/api/v3/search/repositories?q="+query)
	requireSearchRepositories(t, all, first.FullName, second.FullName, outsidePublic.FullName)

	scoped := installationSearch(t, s, scopedToken.Token, "/api/v3/search/repositories?q="+query)
	requireSearchRepositories(t, scoped, first.FullName, outsidePublic.FullName)

	// Installation selection and token selection are intersected. Public
	// repositories remain public, but neither selection can expose a private
	// repository on another account.
	if !s.store.SetInstallationRepositorySelection(installation.ID, "selected", []int{second.ID}) {
		t.Fatal("select installation repositories")
	}
	selectedToken := s.store.CreateInstallationToken(installation.ID, app.ID, nil, nil)
	selected := installationSearch(t, s, selectedToken.Token, "/api/v3/search/repositories?q="+query)
	requireSearchRepositories(t, selected, second.FullName, outsidePublic.FullName)

	// A GitHub App user access token is also the intersection of its user and
	// the app's installations, not the user's wider repository membership.
	if !s.store.SetInstallationRepositorySelection(installation.ID, "all", nil) {
		t.Fatal("restore all-repositories installation")
	}
	userToken, err := s.store.CreateUserToServerToken(admin.ID, app.ID, "", "repo", 0, false)
	if err != nil {
		t.Fatalf("create GitHub App user token: %v", err)
	}
	userSearch := installationSearch(t, s, userToken.Token, "/api/v3/search/repositories?q="+query)
	requireSearchRepositories(t, userSearch, first.FullName, second.FullName, outsidePublic.FullName)
}

func TestInstallationTokenWorksAcrossRepositoryBackedSearch(t *testing.T) {
	s := fuzzRoutedServer(t)
	admin := s.store.UsersByLogin["admin"]
	installedOrg := s.store.CreateOrg(admin, "search-families", "", "")
	outsideOrg := s.store.CreateOrg(admin, "search-families-outside", "", "")
	if installedOrg == nil || outsideOrg == nil {
		t.Fatal("create search organizations")
	}
	repo := s.store.CreateOrgRepo(installedOrg, admin, "private-searchable", "octokit-code-marker", true)
	outside := s.store.CreateOrgRepo(outsideOrg, admin, "private-hidden", "octokit-code-marker", true)
	if repo == nil || outside == nil {
		t.Fatal("create private search repositories")
	}
	for _, candidate := range []*Repo{repo, outside} {
		if err := s.initRepoFiles(context.Background(), candidate, "main", candidate.Description, "", "", true); err != nil {
			t.Fatalf("initialize %s: %v", candidate.FullName, err)
		}
	}
	s.store.UpdateRepo(installedOrg.Login, repo.Name, func(current *Repo) {
		current.Topics = []string{"octokit-installation-topic"}
	})
	label := s.store.CreateLabel(repo.ID, "octokit-installation-label", "searchable label", "123456")
	hiddenLabel := s.store.CreateLabel(outside.ID, "octokit-installation-label", "hidden label", "123456")
	s.store.CreateIssue(repo.ID, admin.ID, "octokit-installation-issue", "", []int{label.ID}, nil, 0)
	s.store.CreateIssue(outside.ID, admin.ID, "octokit-installation-issue", "", []int{hiddenLabel.ID}, nil, 0)
	if pull := s.store.CreatePullRequest(repo.ID, admin.ID, "octokit-installation-pull", "", "main", "main", false, nil, nil, 0); pull == nil {
		t.Fatal("create installed pull request")
	}
	if pull := s.store.CreatePullRequest(outside.ID, admin.ID, "octokit-installation-pull", "", "main", "main", false, nil, nil, 0); pull == nil {
		t.Fatal("create outside pull request")
	}

	app := s.store.CreateApp(admin.ID, "Search families", "", nil, nil)
	installation := s.store.CreateInstallation(
		app.ID, "Organization", installedOrg.ID, installedOrg.Login, nil, nil,
	)
	token := s.store.CreateInstallationToken(installation.ID, app.ID, nil, nil)

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "issues",
			path: "/api/v3/search/issues?q=" + url.QueryEscape("octokit-installation-issue is:issue"),
			want: repo.FullName,
		},
		{
			name: "pull requests",
			path: "/api/v3/search/issues?q=" + url.QueryEscape("octokit-installation-pull is:pr"),
			want: repo.FullName,
		},
		{
			name: "code",
			path: "/api/v3/search/code?q=" + url.QueryEscape("octokit-code-marker"),
			want: repo.FullName,
		},
		{
			name: "commits",
			path: "/api/v3/search/commits?q=" + url.QueryEscape("Initial commit"),
			want: repo.FullName,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			envelope := installationSearch(t, s, token.Token, test.path)
			requireSearchRepositories(t, envelope, test.want)
		})
	}

	labels := installationSearch(t, s, token.Token,
		"/api/v3/search/labels?repository_id="+itoa(repo.ID)+"&q=octokit-installation-label")
	if labels.TotalCount != 1 || labels.Items[0]["name"] != "octokit-installation-label" {
		t.Fatalf("installation label search=%#v", labels)
	}
	hiddenLabels := authedReqScheme(s, http.MethodGet,
		"/api/v3/search/labels?repository_id="+itoa(outside.ID)+"&q=octokit-installation-label",
		"bearer "+token.Token, "")
	if hiddenLabels.Code != http.StatusNotFound {
		t.Fatalf("outside private label search=%d, want 404; body=%s", hiddenLabels.Code, hiddenLabels.Body.String())
	}

	topics := installationSearch(t, s, token.Token,
		"/api/v3/search/topics?q=octokit-installation-topic")
	if topics.TotalCount != 1 || topics.Items[0]["name"] != "octokit-installation-topic" {
		t.Fatalf("installation topic search=%#v", topics)
	}
}

func TestCodeSearchRequiresAuthentication(t *testing.T) {
	s := fuzzRoutedServer(t)
	response := authedReqScheme(s, http.MethodGet, "/api/v3/search/code?q=README", "", "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous code search=%d, want 401; body=%s", response.Code, response.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["message"] != "Requires authentication" {
		t.Fatalf("anonymous code search message=%q", body["message"])
	}
}

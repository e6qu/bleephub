package bleephub

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The forks endpoint takes GitHub's newest|oldest|stargazers|watchers sort
// vocabulary, which the repo-list sort switch does not understand. Before the
// fix `oldest` fell through to created-desc (reversed) and stargazers/watchers
// were ignored. filterSortRepos must now honor an explicit created-asc order
// and the stargazers key the forks handler maps to.
func TestFilterSortReposHonorsForkSortKeys(t *testing.T) {
	mk := func(name string, created time.Time, stars int) *Repo {
		return &Repo{FullName: name, CreatedAt: created, StargazersCount: stars}
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	repos := []*Repo{
		mk("a/old", base, 5),
		mk("a/new", base.Add(48*time.Hour), 1),
		mk("a/mid", base.Add(24*time.Hour), 9),
	}

	order := func(rs []*Repo) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = r.FullName
		}
		return out
	}

	// oldest -> created ascending
	oldest := filterSortRepos(cloneRepoSlice(repos), RepoListOptions{Sort: "created", Direction: "asc", NoPaginate: true})
	if got := order(oldest); got[0] != "a/old" || got[2] != "a/new" {
		t.Fatalf("oldest order = %v, want oldest-first", got)
	}

	// stargazers -> star count descending
	byStars := filterSortRepos(cloneRepoSlice(repos), RepoListOptions{Sort: "stargazers", Direction: "desc", NoPaginate: true})
	if got := order(byStars); got[0] != "a/mid" || got[2] != "a/new" {
		t.Fatalf("stargazers order = %v, want highest-star first (a/mid, a/old, a/new)", got)
	}
}

func cloneRepoSlice(in []*Repo) []*Repo {
	out := make([]*Repo, len(in))
	copy(out, in)
	return out
}

// make_latest:"false" must exclude a release from GET .../releases/latest, and
// flipping it back to "true" on update must restore eligibility. Before the fix
// the field was decoded into nothing and silently dropped.
func TestReleaseMakeLatestControlsLatest(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const repo = "make-latest-repo"
	created := srv.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": repo, "auto_init": true,
	})
	created.Body.Close()

	first := srv.post(t, "/api/v3/repos/admin/"+repo+"/releases", defaultToken, map[string]interface{}{
		"tag_name": "v1.0.0",
	})
	if first.StatusCode != http.StatusCreated {
		first.Body.Close()
		t.Fatalf("create v1.0.0 status = %d", first.StatusCode)
	}
	first.Body.Close()

	second := srv.post(t, "/api/v3/repos/admin/"+repo+"/releases", defaultToken, map[string]interface{}{
		"tag_name": "v2.0.0", "make_latest": "false",
	})
	if second.StatusCode != http.StatusCreated {
		second.Body.Close()
		t.Fatalf("create v2.0.0 status = %d", second.StatusCode)
	}
	v2 := decodeJSON(t, second)
	v2ID := int(v2["id"].(float64))

	// v2 is excluded, so the newest *eligible* release is still v1.0.0.
	latest := decodeJSON(t, srv.get(t, "/api/v3/repos/admin/"+repo+"/releases/latest", defaultToken))
	if latest["tag_name"] != "v1.0.0" {
		t.Fatalf("latest = %v, want v1.0.0 (v2 marked make_latest:false)", latest["tag_name"])
	}

	// Promote v2 back to latest via update.
	upd := srv.patch(t, "/api/v3/repos/admin/"+repo+"/releases/"+itoa(v2ID), defaultToken, map[string]interface{}{
		"make_latest": "true",
	})
	upd.Body.Close()
	latest2 := decodeJSON(t, srv.get(t, "/api/v3/repos/admin/"+repo+"/releases/latest", defaultToken))
	if latest2["tag_name"] != "v2.0.0" {
		t.Fatalf("latest after promote = %v, want v2.0.0", latest2["tag_name"])
	}
}

// generate_release_notes autogenerates the name (when absent) and a changelog
// body; before the fix it was decoded into nothing.
func TestReleaseGenerateReleaseNotes(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const repo = "gen-notes-repo"
	created := srv.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": repo, "auto_init": true})
	created.Body.Close()

	resp := srv.post(t, "/api/v3/repos/admin/"+repo+"/releases", defaultToken, map[string]interface{}{
		"tag_name": "v1.0.0", "generate_release_notes": true,
	})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create release status = %d", resp.StatusCode)
	}
	rel := decodeJSON(t, resp)
	if body, _ := rel["body"].(string); !strings.Contains(body, "Full Changelog") {
		t.Fatalf("generated body missing changelog, got %q", rel["body"])
	}
	if rel["name"] != "v1.0.0" {
		t.Fatalf("generated name = %v, want v1.0.0", rel["name"])
	}
}

// discussion_category_name links a new discussion (surfaced as discussion_url)
// and rejects an unknown category with 422; before the fix it was dropped.
func TestReleaseDiscussionCategory(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const repo = "disc-cat-repo"
	created := srv.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": repo, "auto_init": true})
	created.Body.Close()
	r := srv.store.GetRepo("admin", repo)
	if r == nil {
		t.Fatal("repo not created")
	}

	bad := srv.post(t, "/api/v3/repos/admin/"+repo+"/releases", defaultToken, map[string]interface{}{
		"tag_name": "v0.0.1", "discussion_category_name": "Nonexistent",
	})
	if bad.StatusCode != http.StatusUnprocessableEntity {
		bad.Body.Close()
		t.Fatalf("unknown category status = %d, want 422", bad.StatusCode)
	}
	bad.Body.Close()

	srv.store.CreateDiscussionCategory(r.ID, "Announcements", "📣", "Release notes", false)
	ok := srv.post(t, "/api/v3/repos/admin/"+repo+"/releases", defaultToken, map[string]interface{}{
		"tag_name": "v1.0.0", "discussion_category_name": "Announcements",
	})
	if ok.StatusCode != http.StatusCreated {
		ok.Body.Close()
		t.Fatalf("linked-discussion release status = %d", ok.StatusCode)
	}
	rel := decodeJSON(t, ok)
	if url, _ := rel["discussion_url"].(string); !strings.Contains(url, "/discussions/") {
		t.Fatalf("discussion_url = %v, want a discussions URL", rel["discussion_url"])
	}
}

// A GraphQL createIssue with projectIds adds the new issue to each named
// ProjectV2 board; before the fix the list was accepted and dropped.
func TestGraphQLCreateIssueHonorsProjectIds(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const repo = "gql-projectids-repo"
	created := srv.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": repo})
	created.Body.Close()
	repoData := decodeJSON(t, srv.get(t, "/api/v3/repos/admin/"+repo, defaultToken))
	repoNodeID, _ := repoData["node_id"].(string)

	me := decodeJSON(t, srv.get(t, "/api/v3/user", defaultToken))
	adminID := int(me["id"].(float64))
	project := srv.store.ProjectsV2.CreateProject(adminID, "User", "Roadmap", adminID)

	resp := decodeJSON(t, srv.post(t, "/api/graphql", defaultToken, map[string]interface{}{
		"query": `mutation($input: CreateIssueInput!){ createIssue(input:$input){ issue { id } } }`,
		"variables": map[string]interface{}{"input": map[string]interface{}{
			"repositoryId": repoNodeID, "title": "tracked", "projectIds": []interface{}{project.NodeID},
		}},
	}))
	data, _ := resp["data"].(map[string]interface{})
	ci, _ := data["createIssue"].(map[string]interface{})
	iss, _ := ci["issue"].(map[string]interface{})
	issueNodeID, _ := iss["id"].(string)
	if issueNodeID == "" {
		t.Fatalf("createIssue returned no issue: %v", resp)
	}
	issue := findIssueByNodeID(srv.store, issueNodeID)
	if issue == nil {
		t.Fatal("issue not found by node id")
	}
	if items := srv.store.ProjectsV2.ListItemsForIssue(issue.ID); len(items) == 0 {
		t.Fatal("issue was not added to the project named in projectIds")
	}
}

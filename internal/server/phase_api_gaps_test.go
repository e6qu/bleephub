package bleephub

import (
	"net/http"
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
	const repo = "make-latest-repo"
	created := ghPost(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{
		"name": repo, "auto_init": true,
	})
	created.Body.Close()

	first := ghPost(t, "/api/v3/repos/admin/"+repo+"/releases", defaultToken, map[string]interface{}{
		"tag_name": "v1.0.0",
	})
	if first.StatusCode != http.StatusCreated {
		first.Body.Close()
		t.Fatalf("create v1.0.0 status = %d", first.StatusCode)
	}
	first.Body.Close()

	second := ghPost(t, "/api/v3/repos/admin/"+repo+"/releases", defaultToken, map[string]interface{}{
		"tag_name": "v2.0.0", "make_latest": "false",
	})
	if second.StatusCode != http.StatusCreated {
		second.Body.Close()
		t.Fatalf("create v2.0.0 status = %d", second.StatusCode)
	}
	v2 := decodeJSON(t, second)
	v2ID := int(v2["id"].(float64))

	// v2 is excluded, so the newest *eligible* release is still v1.0.0.
	latest := decodeJSON(t, ghGet(t, "/api/v3/repos/admin/"+repo+"/releases/latest", defaultToken))
	if latest["tag_name"] != "v1.0.0" {
		t.Fatalf("latest = %v, want v1.0.0 (v2 marked make_latest:false)", latest["tag_name"])
	}

	// Promote v2 back to latest via update.
	upd := ghPatch(t, "/api/v3/repos/admin/"+repo+"/releases/"+itoa(v2ID), defaultToken, map[string]interface{}{
		"make_latest": "true",
	})
	upd.Body.Close()
	latest2 := decodeJSON(t, ghGet(t, "/api/v3/repos/admin/"+repo+"/releases/latest", defaultToken))
	if latest2["tag_name"] != "v2.0.0" {
		t.Fatalf("latest after promote = %v, want v2.0.0", latest2["tag_name"])
	}
}

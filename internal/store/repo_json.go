package store

import (
	"strings"
	"time"
)

// RepoToJSON converts a Repo to the GitHub `repository` shape (also a
// valid `minimal-repository`). The hypermedia *_url members carry the
// literal URI-template placeholders real GitHub emits ({/sha}, {+path},
// …). Counters for features bleephub does not model (forks, size) are
// 0; watchers mirrors stargazers exactly as on real GitHub; the has_*
// toggles reflect the surfaces bleephub actually serves. Must not be
// called with st.Mu held: it derives open_issues_count from the store.
func RepoToJSON(repo *Repo, st *Store, baseURL string) map[string]interface{} {
	return RepoToJSONForViewer(repo, st, baseURL, nil)
}

func RepoToJSONForViewer(repo *Repo, st *Store, baseURL string, viewer *User) map[string]interface{} {
	// Read every mutable repo field off a private snapshot: UpdateRepo mutates
	// description, topics, homepage, timestamps, etc. under st.Mu.Lock, so
	// reading the live pointer here would race a concurrent writer. The
	// snapshot takes st.Mu only for the copy — the store lookups below
	// (GetOrg / CountOpenIssues / …) take their own locks, so they must run
	// after the snapshot releases the lock, never nested under it.
	repo = st.SnapRepo(repo)
	ownerJSON := map[string]interface{}{}
	if repo.OwnerType == "Organization" {
		parts := strings.SplitN(repo.FullName, "/", 2)
		if len(parts) == 2 {
			if org := st.GetOrg(parts[0]); org != nil {
				ownerJSON = OrgAsSimpleUserJSON(org)
			}
		}
	} else if repo.Owner != nil {
		ownerJSON = UserToJSON(repo.Owner)
	}

	topics := repo.Topics
	if topics == nil {
		topics = []string{}
	}

	host := baseURL
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	api := baseURL + "/api/v3/repos/" + repo.FullName
	openIssues := st.CountOpenIssues(repo.ID)
	forks := st.CountForks(repo.ID)

	return map[string]interface{}{
		"id":                repo.ID,
		"node_id":           repo.NodeID,
		"name":              repo.Name,
		"full_name":         repo.FullName,
		"owner":             ownerJSON,
		"private":           repo.Private,
		"html_url":          baseURL + "/" + repo.FullName,
		"description":       repo.Description,
		"fork":              repo.Fork,
		"url":               api,
		"archive_url":       api + "/{archive_format}{/ref}",
		"assignees_url":     api + "/assignees{/user}",
		"blobs_url":         api + "/git/blobs{/sha}",
		"branches_url":      api + "/branches{/branch}",
		"collaborators_url": api + "/collaborators{/collaborator}",
		"comments_url":      api + "/comments{/number}",
		"commits_url":       api + "/commits{/sha}",
		"compare_url":       api + "/compare/{base}...{head}",
		"contents_url":      api + "/contents/{+path}",
		"contributors_url":  api + "/contributors",
		"deployments_url":   api + "/deployments",
		"downloads_url":     api + "/downloads",
		"events_url":        api + "/events",
		"forks_url":         api + "/forks",
		"git_commits_url":   api + "/git/commits{/sha}",
		"git_refs_url":      api + "/git/refs{/sha}",
		"git_tags_url":      api + "/git/tags{/sha}",
		"hooks_url":         api + "/hooks",
		"issue_comment_url": api + "/issues/comments{/number}",
		"issue_events_url":  api + "/issues/events{/number}",
		"issues_url":        api + "/issues{/number}",
		"keys_url":          api + "/keys{/key_id}",
		"labels_url":        api + "/labels{/name}",
		"languages_url":     api + "/languages",
		"merges_url":        api + "/merges",
		"milestones_url":    api + "/milestones{/number}",
		"notifications_url": api + "/notifications{?since,all,participating}",
		"pulls_url":         api + "/pulls{/number}",
		"releases_url":      api + "/releases{/id}",
		"stargazers_url":    api + "/stargazers",
		"statuses_url":      api + "/statuses/{sha}",
		"subscribers_url":   api + "/subscribers",
		"subscription_url":  api + "/subscription",
		"tags_url":          api + "/tags",
		"teams_url":         api + "/teams",
		"trees_url":         api + "/git/trees{/sha}",
		"clone_url":         baseURL + "/" + repo.FullName + ".git",
		"git_url":           "git://" + host + "/" + repo.FullName + ".git",
		"ssh_url":           SshGitURL(repo.FullName),
		"svn_url":           baseURL + "/" + repo.FullName,
		"mirror_url":        nil,
		"homepage":          NilOrString(repo.Homepage),
		"license":           LicenseJSON(repo),
		"default_branch":    repo.DefaultBranch,
		"visibility":        repo.Visibility,
		"language":          repo.Language,
		"archived":          repo.Archived,
		"disabled":          false,
		"forks":             forks,
		"forks_count":       forks,
		"size":              st.RepoSize(repo.FullName),
		"stargazers_count":  repo.StargazersCount,
		"watchers":          repo.StargazersCount,
		"watchers_count":    repo.StargazersCount,
		"open_issues":       openIssues,
		"open_issues_count": openIssues,
		"has_issues":        repo.HasIssues,
		"has_projects":      repo.HasProjects,
		"has_wiki":          repo.HasWiki,
		"has_pages":         st.HasPagesSite(repo.ID),
		"has_downloads":     false,
		"has_discussions":   RepoHasDiscussions(repo),
		"has_pull_requests": repo.HasPullRequests,
		"topics":            topics,
		"permissions":       repoPermissionsJSON(st, viewer, repo),
		"created_at":        repo.CreatedAt.Format(time.RFC3339),
		"updated_at":        repo.UpdatedAt.Format(time.RFC3339),
		"pushed_at":         NullableTimestamp(repo.PushedAt),
	}
}

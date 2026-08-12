package store

import "time"

// MigrationRepoExportData gathers lightweight metadata for a migration archive.
func (st *Store) MigrationRepoExportData(repoID int) map[string]interface{} {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	var issues []map[string]interface{}
	for _, issue := range st.Issues {
		if issue.RepoID == repoID {
			issues = append(issues, map[string]interface{}{
				"number": issue.Number,
				"title":  issue.Title,
				"state":  issue.State,
			})
		}
	}

	var pulls []map[string]interface{}
	for _, pr := range st.PullRequests {
		if pr.RepoID == repoID {
			pulls = append(pulls, map[string]interface{}{
				"number": pr.Number,
				"title":  pr.Title,
				"state":  pr.State,
			})
		}
	}

	releases := st.Releases.List(repoID)
	relOut := make([]map[string]interface{}, len(releases))
	for i, r := range releases {
		relOut[i] = map[string]interface{}{
			"id":           r.ID,
			"tag_name":     r.TagName,
			"name":         r.Name,
			"draft":        r.Draft,
			"prerelease":   r.Prerelease,
			"created_at":   r.CreatedAt.Format(time.RFC3339),
			"published_at": nil,
		}
		if r.PublishedAt != nil {
			relOut[i]["published_at"] = r.PublishedAt.Format(time.RFC3339)
		}
	}

	return map[string]interface{}{
		"issues":        issues,
		"pull_requests": pulls,
		"releases":      relOut,
	}
}

package store

import (
	"sort"
	"strings"
)

// OrgImmutableReleasesSettings is the org enforcement policy.
type OrgImmutableReleasesSettings struct {
	EnforcedRepositories  string `json:"enforced_repositories"`
	SelectedRepositoryIDs []int  `json:"selected_repository_ids"`
}

// GetOrgImmutableReleasesSettings returns the org policy; an org that never
// configured one holds the "none" default.
func (st *Store) GetOrgImmutableReleasesSettings(orgLogin string) *OrgImmutableReleasesSettings {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if s := st.OrgImmutableReleases[orgLogin]; s != nil {
		// Detach (STORE-021): the add/remove repo mutators compact
		// SelectedRepositoryIDs in place.
		cp := *s
		cp.SelectedRepositoryIDs = append([]int(nil), s.SelectedRepositoryIDs...)
		return &cp
	}
	return &OrgImmutableReleasesSettings{EnforcedRepositories: "none"}
}

// SetOrgImmutableReleasesSettings replaces the org policy.
func (st *Store) SetOrgImmutableReleasesSettings(orgLogin, enforced string, selectedIDs []int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	settings := &OrgImmutableReleasesSettings{EnforcedRepositories: enforced}
	if enforced == "selected" {
		settings.SelectedRepositoryIDs = selectedIDs
	}
	st.OrgImmutableReleases[orgLogin] = settings
	if st.Persist != nil {
		st.Persist.MustPut("org_immutable_releases", orgLogin, settings)
	}
}

// AddOrgImmutableReleasesRepo adds one repository to the selected list.
func (st *Store) AddOrgImmutableReleasesRepo(orgLogin string, repoID int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	settings := st.OrgImmutableReleases[orgLogin]
	if settings == nil {
		return
	}
	for _, id := range settings.SelectedRepositoryIDs {
		if id == repoID {
			return
		}
	}
	settings.SelectedRepositoryIDs = append(settings.SelectedRepositoryIDs, repoID)
	if st.Persist != nil {
		st.Persist.MustPut("org_immutable_releases", orgLogin, settings)
	}
}

// RemoveOrgImmutableReleasesRepo removes one repository from the selected list.
func (st *Store) RemoveOrgImmutableReleasesRepo(orgLogin string, repoID int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	settings := st.OrgImmutableReleases[orgLogin]
	if settings == nil {
		return
	}
	out := settings.SelectedRepositoryIDs[:0]
	for _, id := range settings.SelectedRepositoryIDs {
		if id != repoID {
			out = append(out, id)
		}
	}
	settings.SelectedRepositoryIDs = out
	if st.Persist != nil {
		st.Persist.MustPut("org_immutable_releases", orgLogin, settings)
	}
}

// ListOrgImmutableReleasesRepos returns the selected repositories sorted by ID.
func (st *Store) ListOrgImmutableReleasesRepos(orgLogin string) []*Repo {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	settings := st.OrgImmutableReleases[orgLogin]
	if settings == nil {
		return nil
	}
	out := make([]*Repo, 0, len(settings.SelectedRepositoryIDs))
	for _, id := range settings.SelectedRepositoryIDs {
		if repo := st.Repos[id]; repo != nil {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotRepos(out)
}

// RepoImmutableReleasesState reports whether immutable releases are enabled
// for the repo and whether the owner's policy enforces them.
func (st *Store) RepoImmutableReleasesState(repo *Repo) (enabled, enforcedByOwner bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	owner, _, _ := strings.Cut(repo.FullName, "/")
	if settings := st.OrgImmutableReleases[owner]; settings != nil {
		switch settings.EnforcedRepositories {
		case "all":
			enforcedByOwner = true
		case "selected":
			for _, id := range settings.SelectedRepositoryIDs {
				if id == repo.ID {
					enforcedByOwner = true
					break
				}
			}
		}
	}
	return st.RepoImmutableReleases[repo.FullName] || enforcedByOwner, enforcedByOwner
}

// SetRepoImmutableReleases records the repo-level toggle.
func (st *Store) SetRepoImmutableReleases(repoKey string, enabled bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	st.RepoImmutableReleases[repoKey] = enabled
	if st.Persist != nil {
		st.Persist.MustPut("repo_immutable_releases", repoKey, enabled)
	}
}

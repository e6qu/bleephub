package store

import (
	"fmt"
	"sort"
	"time"
)

// CreateRepoAutolink creates a new autolink reference on the repository.
func (st *Store) CreateRepoAutolink(repoKey, keyPrefix, urlTemplate string, isAlphanumeric bool) *RepoAutolink {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)

	autolink := &RepoAutolink{
		ID:             st.NextAutolinkID,
		NodeID:         fmt.Sprintf("AL_kgDO%08d", st.NextAutolinkID),
		RepoKey:        repoKey,
		KeyPrefix:      keyPrefix,
		URLTemplate:    urlTemplate,
		IsAlphanumeric: isAlphanumeric,
		CreatedAt:      time.Now().UTC(),
	}
	st.NextAutolinkID++
	if st.RepoAutolinks[repoKey] == nil {
		st.RepoAutolinks[repoKey] = map[int]*RepoAutolink{}
	}
	st.RepoAutolinks[repoKey][autolink.ID] = autolink
	if st.Persist != nil {
		st.Persist.MustPut("repo_autolinks", repoKey, st.RepoAutolinks[repoKey])
	}
	return autolink
}

// ListRepoAutolinks returns all autolinks for a repository, sorted by ID.
func (st *Store) ListRepoAutolinks(repoKey string) []*RepoAutolink {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)

	m := st.RepoAutolinks[repoKey]
	out := make([]*RepoAutolink, 0, len(m))
	for _, a := range m {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotSlice(out)
}

// GetRepoAutolink returns an autolink by ID, or nil.
func (st *Store) GetRepoAutolink(repoKey string, id int) *RepoAutolink {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)
	if st.RepoAutolinks[repoKey] == nil {
		return nil
	}
	return st.RepoAutolinks[repoKey][id]
}

// DeleteRepoAutolink removes an autolink by ID. Returns true if it existed.
func (st *Store) DeleteRepoAutolink(repoKey string, id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)

	if st.RepoAutolinks[repoKey] == nil {
		return false
	}
	if _, ok := st.RepoAutolinks[repoKey][id]; !ok {
		return false
	}
	delete(st.RepoAutolinks[repoKey], id)
	if st.Persist != nil {
		st.Persist.MustPut("repo_autolinks", repoKey, st.RepoAutolinks[repoKey])
	}
	return true
}

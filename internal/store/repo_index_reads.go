package store

import "sort"

// ListEveryRepo returns a detached snapshot of every repository on the
// instance, ordered by full name (STORE-021). The traversal lives here behind
// the store lock because reading st.ReposByName from a caller is a
// process-fatal map race (AUTH-043).
func (st *Store) ListEveryRepo() []*Repo {
	st.Mu.RLock()
	repos := make([]*Repo, 0, len(st.ReposByName))
	for _, repo := range st.ReposByName {
		repos = append(repos, repo)
	}
	st.Mu.RUnlock()
	sort.Slice(repos, func(i, j int) bool { return repos[i].FullName < repos[j].FullName })
	return snapshotRepos(repos)
}

// ListAccountSSHKeys returns a detached snapshot of the account's SSH
// authentication keys, oldest first. The traversal lives here because the
// index is guarded by Misc.Mu.
func (st *Store) ListAccountSSHKeys(userID int) []*UserKey {
	st.Misc.Mu.RLock()
	keys := make([]*UserKey, 0, len(st.Misc.KeysByUser[userID]))
	keys = append(keys, st.Misc.KeysByUser[userID]...)
	st.Misc.Mu.RUnlock()
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return snapshotSlice(keys)
}

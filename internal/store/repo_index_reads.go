package store

import "sort"

// ListEveryRepo returns a detached snapshot of every repository on the
// instance, ordered by full name.
//
// The account-wide GraphQL connections (a user's authored issues, pull
// requests and comments) are questions about the whole instance rather than
// about one owner, and the caller then narrows the answer to what the request
// may read. Reading st.ReposByName directly from a caller would be a
// process-fatal map race (AUTH-043), so the traversal lives here behind the
// store lock and hands back snapshots (STORE-021).
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

// ListAccountSSHKeys returns a detached snapshot of the account's registered
// SSH authentication keys, oldest first.
//
// GraphQL's User.publicKeys reads them, and the index is guarded by Misc.Mu,
// so the traversal belongs here rather than in a caller that would have to
// take that lock itself.
func (st *Store) ListAccountSSHKeys(userID int) []*UserKey {
	st.Misc.Mu.RLock()
	keys := make([]*UserKey, 0, len(st.Misc.KeysByUser[userID]))
	keys = append(keys, st.Misc.KeysByUser[userID]...)
	st.Misc.Mu.RUnlock()
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return snapshotSlice(keys)
}

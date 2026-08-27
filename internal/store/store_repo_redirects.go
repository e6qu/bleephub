package store

// Repository redirects: a rename or transfer leaves the old "owner/name"
// resolving to the repository under its new name, and every resolver (REST and
// both git transports) answers a move with a redirect, not a 404. The record is
// a fold-insensitive map from vacated "owner/name" to repository id, resolved by
// id at redirect time so it survives further renames.

// recordRepoRedirectLocked records that oldFull resolves to repoID and retires
// any redirect at newFull — a live name does not redirect. Caller holds st.Mu (write).
func (st *Store) recordRepoRedirectLocked(oldFull, newFull string, repoID int) {
	if st.RepoRedirects == nil {
		st.RepoRedirects = make(map[string]int)
	}
	delete(st.RepoRedirects, FoldName(newFull))
	st.RepoRedirects[FoldName(oldFull)] = repoID
	// A repository that moves away and back must not redirect to itself.
	for from, id := range st.RepoRedirects {
		if id == repoID && from == FoldName(newFull) {
			delete(st.RepoRedirects, from)
		}
	}
}

// dropRepoRedirectsLocked retires every redirect resolving to repoID (a deleted
// repository leaves nothing to redirect to). Caller holds st.Mu (write).
func (st *Store) dropRepoRedirectsLocked(repoID int) {
	for from, id := range st.RepoRedirects {
		if id == repoID {
			delete(st.RepoRedirects, from)
		}
	}
}

// RedirectedRepo resolves a repository by a name it used to answer to,
// returning a detached snapshot (STORE-021) or nil. A live name wins over its
// own redirect, so this reports a move only for a name nothing occupies.
func (st *Store) RedirectedRepo(fullName string) *Repo {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if st.RepoByNameLocked(fullName) != nil {
		return nil
	}
	repoID, ok := st.RepoRedirects[FoldName(fullName)]
	if !ok {
		return nil
	}
	repo := st.Repos[repoID]
	if repo == nil {
		return nil
	}
	clone := *repo
	return &clone
}

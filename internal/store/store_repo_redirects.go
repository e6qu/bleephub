package store

// Repository redirects.
//
// GitHub keeps the name a repository used to answer to. A rename or a transfer
// leaves the old "owner/name" pointing at the repository under its new name,
// and every surface that resolves a repository — the REST API and both git
// transports — answers a moved repository with a redirect rather than a 404.
// That is what keeps a clone already on a developer's machine, a stored URL, or
// a webhook receiver's bookmark working across a rename.
//
// The record is a fold-insensitive map from the vacated "owner/name" to the
// repository id, so it survives further renames: the repository is resolved by
// id at redirect time, not by the name it happened to carry when it moved.

// recordRepoRedirectLocked remembers that oldFull now resolves to repoID, and
// retires any redirect that pointed at newFull — a name that is live again is
// not a name that redirects. Caller must hold st.Mu for writing.
func (st *Store) recordRepoRedirectLocked(oldFull, newFull string, repoID int) {
	if st.RepoRedirects == nil {
		st.RepoRedirects = make(map[string]int)
	}
	delete(st.RepoRedirects, FoldName(newFull))
	st.RepoRedirects[FoldName(oldFull)] = repoID
	// A repository that moves away and back again would otherwise redirect to
	// itself forever, so a name is never both live and redirecting.
	for from, id := range st.RepoRedirects {
		if id == repoID && from == FoldName(newFull) {
			delete(st.RepoRedirects, from)
		}
	}
}

// dropRepoRedirectsLocked retires every redirect that resolves to repoID. A
// deleted repository leaves nothing to redirect to. Caller must hold st.Mu for
// writing.
func (st *Store) dropRepoRedirectsLocked(repoID int) {
	for from, id := range st.RepoRedirects {
		if id == repoID {
			delete(st.RepoRedirects, from)
		}
	}
}

// RedirectedRepo resolves a repository by a name it used to answer to,
// returning a detached snapshot of the repository that name now names, or nil
// when the name never moved. A name that is live again wins over its own
// redirect, so this reports a move only for a name nothing currently occupies.
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

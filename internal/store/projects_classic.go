package store

func (st *Store) GetRepoByName(fullName string) *Repo {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.ReposByName[fullName]
}

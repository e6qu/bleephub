package store

// GetDependabotRepositoryAccessDefaultLevel returns the org's default access
// level for Dependabot updates ("public" until changed).
func (st *Store) GetDependabotRepositoryAccessDefaultLevel(orgLogin string) string {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if level := st.DependabotRepoAccessDefaultLevel[orgLogin]; level != "" {
		return level
	}
	return "public"
}

func (st *Store) SetDependabotRepositoryAccessDefaultLevel(orgLogin, level string) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	st.DependabotRepoAccessDefaultLevel[orgLogin] = level
	if st.Persist != nil {
		st.Persist.MustPut("dependabot_repo_access_default_level", orgLogin, level)
	}
}

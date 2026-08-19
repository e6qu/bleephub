package store

import (
	"strconv"
	"strings"
)

// ListOrgPinnedRepos returns a detached copy of the org's ordered pinned-repo
// full names, or an empty slice. The bool reports whether the org exists.
func (st *Store) ListOrgPinnedRepos(orgLogin string) ([]string, bool) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	org, ok := st.OrgsByLogin[orgLogin]
	if !ok {
		return nil, false
	}
	out := make([]string, len(org.PinnedRepos))
	copy(out, org.PinnedRepos)
	return out, true
}

// SetOrgPinnedRepos replaces the org's pinned list with the given full names,
// mirroring the user pin semantics (SetPinnedRepos): order preserved,
// duplicates and repos that do not exist dropped, capped at MaxPinnedRepos.
// Org pins additionally require the repository be owned by the org — a repo
// under any other owner is dropped. Returns the stored list. Persists the org.
func (st *Store) SetOrgPinnedRepos(orgLogin string, fullNames []string) ([]string, bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	org, ok := st.OrgsByLogin[orgLogin]
	if !ok {
		return nil, false
	}
	seen := map[string]bool{}
	pinned := make([]string, 0, MaxPinnedRepos)
	for _, fn := range fullNames {
		if len(pinned) >= MaxPinnedRepos {
			break
		}
		if seen[fn] {
			continue
		}
		owner, _, valid := SplitRepoFullName(fn)
		if !valid || !strings.EqualFold(owner, org.Login) {
			continue
		}
		if _, exists := st.ReposByName[fn]; !exists {
			continue
		}
		seen[fn] = true
		pinned = append(pinned, fn)
	}
	org.PinnedRepos = pinned
	org.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("orgs", strconv.Itoa(org.ID), org)
	}
	out := make([]string, len(pinned))
	copy(out, pinned)
	return out, true
}

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
	org := st.OrgByLoginLocked(orgLogin)
	if org == nil {
		return nil, false
	}
	out := make([]string, len(org.PinnedRepos))
	copy(out, org.PinnedRepos)
	return out, true
}

// SetOrgPinnedRepos replaces the org's pinned list, mirroring SetPinnedRepos
// (order preserved, duplicates and nonexistent repos dropped, capped at
// MaxPinnedRepos) and additionally requiring each repo be owned by the org.
// Returns the stored list.
func (st *Store) SetOrgPinnedRepos(orgLogin string, fullNames []string) ([]string, bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	org := st.OrgByLoginLocked(orgLogin)
	if org == nil {
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

package store

import "strconv"

// MaxPinnedRepos matches github.com's limit of six pinned items on a profile.
const MaxPinnedRepos = 6

// ListPinnedRepos returns a detached copy of the user's ordered pinned-repo full
// names, or an empty slice.
func (st *Store) ListPinnedRepos(userID int) []string {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	user, ok := st.Users[userID]
	if !ok || len(user.PinnedRepos) == 0 {
		return []string{}
	}
	out := make([]string, len(user.PinnedRepos))
	copy(out, user.PinnedRepos)
	return out
}

// SetPinnedRepos replaces the user's pinned list, preserving order, dropping
// duplicates and nonexistent repos, and capping at MaxPinnedRepos. Returns the
// stored list.
func (st *Store) SetPinnedRepos(userID int, fullNames []string) ([]string, bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	user, ok := st.Users[userID]
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
		if _, exists := st.ReposByName[fn]; !exists {
			continue
		}
		seen[fn] = true
		pinned = append(pinned, fn)
	}
	user.PinnedRepos = pinned
	user.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("users", strconv.Itoa(user.ID), user)
	}
	out := make([]string, len(pinned))
	copy(out, pinned)
	return out, true
}

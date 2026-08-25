package store

import "sort"

func SortHistory(history []*GistHistory) {
	sort.Slice(history, func(i, j int) bool {
		return history[i].CommittedAt.After(history[j].CommittedAt)
	})
}

// GistStargazerIDs returns the ids of the users who have starred the gist,
// oldest-account first. The star bookkeeping is keyed by user (userID → gistID
// → starred) with no reverse index, so the reverse lookup is a scan; it is the
// only way to count or list a gist's stargazers, which Gist.stargazerCount and
// Gist.stargazers both need. The result is a fresh slice callers may keep.
func (st *Store) GistStargazerIDs(gistID string) []int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if gistID == "" || st.Gists[gistID] == nil {
		return nil
	}
	var ids []int
	for userID, stars := range st.StarredGists {
		if stars[gistID] {
			ids = append(ids, userID)
		}
	}
	sort.Ints(ids)
	return ids
}

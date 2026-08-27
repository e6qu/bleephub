package store

import "sort"

func SortHistory(history []*GistHistory) {
	sort.Slice(history, func(i, j int) bool {
		return history[i].CommittedAt.After(history[j].CommittedAt)
	})
}

// GistStargazerIDs returns the ids of the users who starred the gist, oldest
// account first. Star bookkeeping has no reverse index, so this scans. The
// result is a fresh slice.
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

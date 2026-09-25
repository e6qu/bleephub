package store

import (
	"slices"
	"strconv"
)

// A "relates to" relationship has no direction: each issue lists the other,
// so it is recorded on both sides at once and removed from both at once.

// AddIssueRelatesTo relates two issues. It returns false when they are
// already related.
func (st *Store) AddIssueRelatesTo(issueID, relatedID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if slices.Contains(st.IssueRelatesTo[issueID], relatedID) {
		return false
	}
	st.IssueRelatesTo[issueID] = append(st.IssueRelatesTo[issueID], relatedID)
	st.IssueRelatesTo[relatedID] = append(st.IssueRelatesTo[relatedID], issueID)
	st.persistRelatesToLocked(issueID)
	st.persistRelatesToLocked(relatedID)
	return true
}

// RemoveIssueRelatesTo removes the relationship between two issues. It
// returns false when they are not related.
func (st *Store) RemoveIssueRelatesTo(issueID, relatedID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if !slices.Contains(st.IssueRelatesTo[issueID], relatedID) {
		return false
	}
	for _, pair := range [][2]int{{issueID, relatedID}, {relatedID, issueID}} {
		kept := slices.DeleteFunc(st.IssueRelatesTo[pair[0]], func(id int) bool { return id == pair[1] })
		if len(kept) == 0 {
			delete(st.IssueRelatesTo, pair[0])
		} else {
			st.IssueRelatesTo[pair[0]] = kept
		}
		st.persistRelatesToLocked(pair[0])
	}
	return true
}

// ListIssueRelatesTo returns the IDs of the issues related to issueID, in the
// order the relationships were made.
func (st *Store) ListIssueRelatesTo(issueID int) []int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return slices.Clone(st.IssueRelatesTo[issueID])
}

func (st *Store) persistRelatesToLocked(issueID int) {
	if st.Persist == nil {
		return
	}
	if related, ok := st.IssueRelatesTo[issueID]; ok {
		st.Persist.MustPut("issue_relates_to", strconv.Itoa(issueID), related)
	} else {
		st.Persist.MustDelete("issue_relates_to", strconv.Itoa(issueID))
	}
}

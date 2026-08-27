package store

import (
	"errors"
	"strconv"
)

// AddSubIssue links child under parent. replaceParent detaches the child
// from a previous parent first.
func (st *Store) AddSubIssue(parentID, childID int, replaceParent bool) error {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if parentID == childID {
		return errSubIssueSelf
	}
	if cur, ok := st.SubIssueParent[childID]; ok {
		if cur == parentID {
			return errSubIssueDuplicate
		}
		if !replaceParent {
			return errSubIssueHasParent
		}
	}
	// Reject cycles: no ancestor of the parent may be the child.
	for ancestor := parentID; ; {
		next, ok := st.SubIssueParent[ancestor]
		if !ok {
			break
		}
		if next == childID {
			return errSubIssueCycle
		}
		ancestor = next
	}
	if cur, ok := st.SubIssueParent[childID]; ok {
		st.removeSubIssueLocked(cur, childID)
		st.persistSubIssuesLocked(cur)
	}
	st.SubIssueLists[parentID] = append(st.SubIssueLists[parentID], childID)
	st.SubIssueParent[childID] = parentID
	st.persistSubIssuesLocked(parentID)
	return nil
}

// RemoveSubIssue unlinks child from parent.
func (st *Store) RemoveSubIssue(parentID, childID int) error {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.SubIssueParent[childID] != parentID {
		return errSubIssueNotLinked
	}
	st.removeSubIssueLocked(parentID, childID)
	st.persistSubIssuesLocked(parentID)
	return nil
}

func (st *Store) removeSubIssueLocked(parentID, childID int) {
	children := st.SubIssueLists[parentID]
	for i, id := range children {
		if id == childID {
			st.SubIssueLists[parentID] = append(children[:i], children[i+1:]...)
			break
		}
	}
	if len(st.SubIssueLists[parentID]) == 0 {
		delete(st.SubIssueLists, parentID)
	}
	delete(st.SubIssueParent, childID)
}

// ReprioritizeSubIssue moves child within parent's list, placing it after
// afterID or before beforeID.
func (st *Store) ReprioritizeSubIssue(parentID, childID int, afterID, beforeID *int) error {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.SubIssueParent[childID] != parentID {
		return errSubIssueNotLinked
	}
	children := st.SubIssueLists[parentID]
	without := make([]int, 0, len(children))
	for _, id := range children {
		if id != childID {
			without = append(without, id)
		}
	}
	pos := len(without)
	switch {
	case afterID != nil:
		found := false
		for i, id := range without {
			if id == *afterID {
				pos = i + 1
				found = true
				break
			}
		}
		if !found {
			return errSubIssueNotLinked
		}
	case beforeID != nil:
		found := false
		for i, id := range without {
			if id == *beforeID {
				pos = i
				found = true
				break
			}
		}
		if !found {
			return errSubIssueNotLinked
		}
	}
	reordered := make([]int, 0, len(children))
	reordered = append(reordered, without[:pos]...)
	reordered = append(reordered, childID)
	reordered = append(reordered, without[pos:]...)
	st.SubIssueLists[parentID] = reordered
	st.persistSubIssuesLocked(parentID)
	return nil
}

// ListSubIssues returns parent's sub-issue IDs in priority order.
func (st *Store) ListSubIssues(parentID int) []int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := make([]int, len(st.SubIssueLists[parentID]))
	copy(out, st.SubIssueLists[parentID])
	return out
}

func (st *Store) persistSubIssuesLocked(parentID int) {
	if st.Persist == nil {
		return
	}
	if children, ok := st.SubIssueLists[parentID]; ok {
		st.Persist.MustPut("sub_issues", strconv.Itoa(parentID), children)
	} else {
		st.Persist.MustDelete("sub_issues", strconv.Itoa(parentID))
	}
}

// AddIssueBlockedBy records that issue is blocked by blocker. Returns false
// when the link already exists.
func (st *Store) AddIssueBlockedBy(issueID, blockerID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	for _, id := range st.IssueBlockedBy[issueID] {
		if id == blockerID {
			return false
		}
	}
	st.IssueBlockedBy[issueID] = append(st.IssueBlockedBy[issueID], blockerID)
	st.persistBlockedByLocked(issueID)
	return true
}

// RemoveIssueBlockedBy removes a blocked-by link. Returns false when the
// link does not exist.
func (st *Store) RemoveIssueBlockedBy(issueID, blockerID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	blockers := st.IssueBlockedBy[issueID]
	for i, id := range blockers {
		if id == blockerID {
			st.IssueBlockedBy[issueID] = append(blockers[:i], blockers[i+1:]...)
			if len(st.IssueBlockedBy[issueID]) == 0 {
				delete(st.IssueBlockedBy, issueID)
			}
			st.persistBlockedByLocked(issueID)
			return true
		}
	}
	return false
}

// ListIssueBlockedBy returns the IDs of the issues blocking issueID.
func (st *Store) ListIssueBlockedBy(issueID int) []int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := make([]int, len(st.IssueBlockedBy[issueID]))
	copy(out, st.IssueBlockedBy[issueID])
	return out
}

// ListIssueBlocking returns the IDs of the issues issueID blocks (the reverse
// of the blocked-by links).
func (st *Store) ListIssueBlocking(issueID int) []int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []int
	for blocked, blockers := range st.IssueBlockedBy {
		for _, id := range blockers {
			if id == issueID {
				out = append(out, blocked)
				break
			}
		}
	}
	return out
}

func (st *Store) GetSubIssueParent(issueID int) int {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.SubIssueParent[issueID]
}

func (st *Store) persistBlockedByLocked(issueID int) {
	if st.Persist == nil {
		return
	}
	if blockers, ok := st.IssueBlockedBy[issueID]; ok {
		st.Persist.MustPut("issue_blocked_by", strconv.Itoa(issueID), blockers)
	} else {
		st.Persist.MustDelete("issue_blocked_by", strconv.Itoa(issueID))
	}
}

var (
	errSubIssueSelf      = errors.New("an issue may not be its own sub-issue")
	errSubIssueHasParent = errors.New("the issue is already a sub-issue of another issue")
	errSubIssueCycle     = errors.New("the sub-issue relationship would create a cycle")
	errSubIssueDuplicate = errors.New("the issue is already a sub-issue of this issue")
	errSubIssueNotLinked = errors.New("the issue is not a sub-issue of this issue")
)

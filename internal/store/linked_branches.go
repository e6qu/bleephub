package store

import "strconv"

// A linked branch is the association GitHub's "create a branch for this issue"
// control makes. It is a link, not the branch: unlinking leaves the branch in
// place. It lives on the issue row, so it persists and loads with the issue.

// LinkedBranch is one branch linked to an issue.
type LinkedBranch struct {
	// RepoID holds the branch's repository, which GitHub allows to differ from
	// the issue's.
	RepoID int
	// Ref is the fully qualified reference, e.g. refs/heads/42-fix-the-thing.
	Ref string
}

// LinkedBranchNodeIDPrefix is the type prefix of a linked branch's global id.
const LinkedBranchNodeIDPrefix = "LB"

// LinkedBranchNodeID renders a linked branch's global id from its (issue, ref)
// pair, of which there is at most one — so the id needs no counter and is
// stable across restarts.
func LinkedBranchNodeID(issueID int, ref string) string {
	return GitObjectNodeID(LinkedBranchNodeIDPrefix, issueID, ref)
}

// ParseLinkedBranchNodeID decodes a linked branch's global id.
func ParseLinkedBranchNodeID(nodeID string) (issueID int, ref string, ok bool) {
	prefix, id, value, ok := ParseGitObjectNodeID(nodeID)
	if !ok || prefix != LinkedBranchNodeIDPrefix {
		return 0, "", false
	}
	return id, value, true
}

// LinkIssueBranch links a branch to an issue, reporting whether the issue
// exists and whether this call created the link. Relinking is idempotent.
func (st *Store) LinkIssueBranch(issueID, repoID int, ref string) (found, created bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	issue, ok := st.Issues[issueID]
	if !ok {
		return false, false
	}
	for _, linked := range issue.LinkedBranches {
		if linked.Ref == ref && linked.RepoID == repoID {
			return true, false
		}
	}
	issue.LinkedBranches = append(issue.LinkedBranches, LinkedBranch{RepoID: repoID, Ref: ref})
	issue.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("issues", strconv.Itoa(issue.ID), issue)
	}
	return true, true
}

// UnlinkIssueBranch removes a link, reporting whether one was removed. The
// branch itself is left in place.
func (st *Store) UnlinkIssueBranch(issueID int, ref string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	issue, ok := st.Issues[issueID]
	if !ok {
		return false
	}
	kept := issue.LinkedBranches[:0:0]
	removed := false
	for _, linked := range issue.LinkedBranches {
		if linked.Ref == ref && !removed {
			removed = true
			continue
		}
		kept = append(kept, linked)
	}
	if !removed {
		return false
	}
	issue.LinkedBranches = kept
	issue.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("issues", strconv.Itoa(issue.ID), issue)
	}
	return true
}

// ListLinkedBranches returns an issue's links as a detached snapshot
// (STORE-021).
func (st *Store) ListLinkedBranches(issueID int) []LinkedBranch {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	issue, ok := st.Issues[issueID]
	if !ok {
		return nil
	}
	return append([]LinkedBranch(nil), issue.LinkedBranches...)
}

// FindIssueByLinkedBranchNodeID resolves a linked branch's global id to the
// issue that carries it and the link itself, reporting false when no such
// link currently exists.
func FindIssueByLinkedBranchNodeID(st *Store, nodeID string) (*Issue, LinkedBranch, bool) {
	issueID, ref, ok := ParseLinkedBranchNodeID(nodeID)
	if !ok {
		return nil, LinkedBranch{}, false
	}
	issue := st.GetIssue(issueID)
	if issue == nil {
		return nil, LinkedBranch{}, false
	}
	for _, linked := range issue.LinkedBranches {
		if linked.Ref == ref {
			return issue, linked, true
		}
	}
	return nil, LinkedBranch{}, false
}

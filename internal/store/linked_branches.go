package store

import "strconv"

// A linked branch is the association GitHub's "create a branch for this issue"
// control makes: a branch, in some repository, recorded as the work on an
// issue. It is a link and not the branch itself — deleting the link leaves the
// branch in place, which is why the association is stored here rather than
// inferred from a naming convention on the ref.
//
// The association is carried on the issue because that is its whole lifetime:
// it is created against an issue, listed from an issue, and disappears with
// one. That also means it persists and loads with the issue row, so there is no
// second bucket that can fall out of step with the first.

// LinkedBranch is one branch linked to an issue.
type LinkedBranch struct {
	// RepoID is the repository holding the branch. It is recorded explicitly
	// because GitHub allows the branch to live in a repository other than the
	// issue's.
	RepoID int
	// Ref is the fully qualified reference, e.g. refs/heads/42-fix-the-thing.
	Ref string
}

// LinkedBranchNodeIDPrefix is the type prefix of a linked branch's global id.
const LinkedBranchNodeIDPrefix = "LB"

// LinkedBranchNodeID renders a linked branch's global id. A link is identified
// by the issue it belongs to and the reference it names — there is at most one
// link per pair — so the identifier needs no counter of its own and stays
// stable across a restart.
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

// LinkIssueBranch records a branch as the work on an issue. It reports whether
// the issue exists and whether this call created the link: linking a branch
// that is already linked is not an error and does not duplicate the entry.
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

// UnlinkIssueBranch removes a link. It reports whether a link was removed; the
// branch itself is untouched, exactly as GitHub's unlink leaves it.
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
// issue that carries it and the link itself. It reports false when the
// identifier does not name a link that exists, so a caller cannot act on an
// identifier for a link that has already been removed.
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

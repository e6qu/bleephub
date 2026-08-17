package store

// GraphQL node-ID finders: resolve a global node id to its store record.
// Node-ID codecs live in store so the GraphQL resolver layer, the REST
// handlers, and their tests share one format (ARCH-003). Each finder tries
// the O(1) decoded-database-id fast path (guarded by NodeID equality) and
// falls back to a scan.

func FindDiscussionByNodeID(st *Store, nodeID string) *Discussion {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "D_kgDO"); ok {
		if d := st.Discussions[id]; d != nil && d.NodeID == nodeID && !d.Deleted {
			return d
		}
	}
	for _, d := range st.Discussions {
		if d.NodeID == nodeID && !d.Deleted {
			return d
		}
	}
	return nil
}

func FindDiscussionCategoryByNodeID(st *Store, nodeID string) *DiscussionCategory {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "DGC_kgDO"); ok {
		if cat := st.DiscussionCategories[id]; cat != nil && cat.NodeID == nodeID {
			return cat
		}
	}
	for _, cat := range st.DiscussionCategories {
		if cat.NodeID == nodeID {
			return cat
		}
	}
	return nil
}

func FindDiscussionCommentByNodeID(st *Store, nodeID string) *DiscussionComment {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "DC_kgDO"); ok {
		if c := st.DiscussionComments[id]; c != nil && c.NodeID == nodeID && !c.Deleted {
			return c
		}
	}
	for _, c := range st.DiscussionComments {
		if c.NodeID == nodeID && !c.Deleted {
			return c
		}
	}
	return nil
}

func FindRepoByNodeID(st *Store, nodeID string) *Repo {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "R_kgDO"); ok {
		if r := st.Repos[id]; r != nil && r.NodeID == nodeID {
			return r
		}
	}
	for _, r := range st.Repos {
		if r.NodeID == nodeID {
			return r
		}
	}
	return nil
}

func FindIssueByNodeID(st *Store, nodeID string) *Issue {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "I_kgDO"); ok {
		if i := st.Issues[id]; i != nil && i.NodeID == nodeID {
			return i
		}
	}
	for _, i := range st.Issues {
		if i.NodeID == nodeID {
			return i
		}
	}
	return nil
}

func FindLabelByNodeID(st *Store, nodeID string) *IssueLabel {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "LA_kgDO"); ok {
		if l := st.Labels[id]; l != nil && l.NodeID == nodeID {
			return l
		}
	}
	for _, l := range st.Labels {
		if l.NodeID == nodeID {
			return l
		}
	}
	return nil
}

func FindMilestoneByNodeID(st *Store, nodeID string) *Milestone {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "MI_kgDO"); ok {
		if ms := st.Milestones[id]; ms != nil && ms.NodeID == nodeID {
			return ms
		}
	}
	for _, ms := range st.Milestones {
		if ms.NodeID == nodeID {
			return ms
		}
	}
	return nil
}

func FindUserByNodeID(st *Store, nodeID string) *User {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "U_kgDO"); ok {
		if u := st.Users[id]; u != nil && u.NodeID == nodeID {
			return u
		}
	}
	for _, u := range st.Users {
		if u.NodeID == nodeID {
			return u
		}
	}
	return nil
}

// FindIssueTypeByNodeID resolves an issue-type node id (moved here from the
// issue-types REST file in ARCH-003; it has no REST callers).
func FindIssueTypeByNodeID(st *Store, nodeID string) *IssueType {
	if nodeID == "" {
		return nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	// O(1) fast path: the node ID embeds the globally-unique database id, so
	// parse it and look the type up directly instead of walking every org's map
	// under the lock (GQL-024). The NodeID equality guard rejects a decoded id
	// that resolves to a different node shape; the scan below remains as a
	// robustness fallback, matching the other node finders.
	if id, ok := DecodeNodeDBID(nodeID, "IT_kwDO"); ok {
		if it := st.IssueTypesByID[id]; it != nil && it.NodeID == nodeID {
			return it
		}
	}
	for _, types := range st.OrgIssueTypes {
		for _, it := range types {
			if it.NodeID == nodeID {
				return it
			}
		}
	}
	return nil
}

func FindPullRequestByNodeID(st *Store, nodeID string) *PullRequest {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "PR_kgDO"); ok {
		if pr := st.PullRequests[id]; pr != nil && pr.NodeID == nodeID {
			return pr
		}
	}
	for _, pr := range st.PullRequests {
		if pr.NodeID == nodeID {
			return pr
		}
	}
	return nil
}

// FindReviewByNodeID resolves a pull-request review from its GraphQL node id
// (PRR_kgDO…). Reviews are keyed by numeric id, so this scans the review map.
func FindReviewByNodeID(st *Store, nodeID string) *PullRequestReview {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, review := range st.PRReviews {
		if review.NodeID == nodeID {
			return review
		}
	}
	return nil
}

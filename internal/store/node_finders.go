package store

// GraphQL node-ID finders resolve a global node id to its store record. Codecs
// live in store so GraphQL, REST, and tests share one format (ARCH-003). Each
// tries an O(1) decoded-database-id fast path (guarded by NodeID equality), then
// falls back to a scan. Unlike Get*, these return the live row, not a snapshot.

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

// FindIssueTypeByNodeID resolves an issue-type node id.
func FindIssueTypeByNodeID(st *Store, nodeID string) *IssueType {
	if nodeID == "" {
		return nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
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

// FindReviewByNodeID resolves a PR review (PRR_kgDO…); reviews lack a fast-path
// index, so this scans.
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

// FindIssueCommentByNodeID resolves an issue/PR conversation comment (IC_kgDO…).
func FindIssueCommentByNodeID(st *Store, nodeID string) *Comment {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "IC_kgDO"); ok {
		if c := st.Comments[id]; c != nil && c.NodeID == nodeID {
			return c
		}
	}
	for _, c := range st.Comments {
		if c.NodeID == nodeID {
			return c
		}
	}
	return nil
}

// FindCommitCommentByNodeID resolves a commit comment (CC_kgDO…).
func FindCommitCommentByNodeID(st *Store, nodeID string) *CommitComment {
	st.CommitComments.Mu.RLock()
	defer st.CommitComments.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "CC_kgDO"); ok {
		if c := st.CommitComments.ByID[id]; c != nil && c.NodeID == nodeID {
			return c
		}
	}
	for _, c := range st.CommitComments.ByID {
		if c.NodeID == nodeID {
			return c
		}
	}
	return nil
}

// FindPullRequestReviewCommentByNodeID resolves a PR review comment (PRRC_kgDO…).
func FindPullRequestReviewCommentByNodeID(st *Store, nodeID string) *PRReviewComment {
	st.PRReviewComments.Mu.RLock()
	defer st.PRReviewComments.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "PRRC_kgDO"); ok {
		if c := st.PRReviewComments.ByID[id]; c != nil && c.NodeID == nodeID {
			return c
		}
	}
	for _, c := range st.PRReviewComments.ByID {
		if c.NodeID == nodeID {
			return c
		}
	}
	return nil
}

// FindReleaseByNodeID resolves a release (RE_kgDO…).
func FindReleaseByNodeID(st *Store, nodeID string) *Release {
	st.Releases.Mu.RLock()
	defer st.Releases.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "RE_kgDO"); ok {
		if r := st.Releases.ByID[id]; r != nil && r.NodeID == nodeID {
			return r
		}
	}
	for _, r := range st.Releases.ByID {
		if r.NodeID == nodeID {
			return r
		}
	}
	return nil
}

// FindTeamByNodeID resolves a team's node id to its live row and owning org.
func FindTeamByNodeID(st *Store, nodeID string) (*Team, *Org) {
	if nodeID == "" {
		return nil, nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, team := range st.Teams {
		if team.NodeID == nodeID {
			return team, st.Orgs[team.OrgID]
		}
	}
	return nil, nil
}

// FindPackageVersionByNodeID resolves a package version's node id to its live row and owning package.
func FindPackageVersionByNodeID(st *Store, nodeID string) (*PackageVersion, *Package) {
	if nodeID == "" {
		return nil, nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, version := range st.PackageVersions {
		if version.NodeID == nodeID {
			return version, st.Packages[version.PackageID]
		}
	}
	return nil, nil
}

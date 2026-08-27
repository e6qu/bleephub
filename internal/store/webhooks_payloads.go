package store

// The Snap* helpers return value copies of a shared store entity taken under the
// read lock, so webhook payload builders read a private copy rather than the
// live pointer a concurrent Update* writer mutates. Never call one while holding
// the read lock — RWMutex read locks are not reentrant.
func (st *Store) SnapRepo(r *Repo) *Repo {
	if r == nil {
		return nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	cp := *r
	return &cp
}

func (st *Store) SnapIssue(i *Issue) *Issue {
	if i == nil {
		return nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	cp := *i
	cp.AssigneeIDs = append([]int(nil), i.AssigneeIDs...)
	cp.LabelIDs = append([]int(nil), i.LabelIDs...)
	return &cp
}

func (st *Store) SnapPR(pr *PullRequest) *PullRequest {
	if pr == nil {
		return nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	cp := *pr
	cp.AssigneeIDs = append([]int(nil), pr.AssigneeIDs...)
	cp.LabelIDs = append([]int(nil), pr.LabelIDs...)
	cp.RequestedReviewerIDs = append([]int(nil), pr.RequestedReviewerIDs...)
	cp.RequestedTeamIDs = append([]int(nil), pr.RequestedTeamIDs...)
	return &cp
}

func (st *Store) SnapComment(comment *Comment) *Comment {
	if comment == nil {
		return nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	cp := *comment
	return &cp
}

func (st *Store) SnapPullRequestReview(review *PullRequestReview) *PullRequestReview {
	if review == nil {
		return nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	cp := *review
	return &cp
}

func (st *Store) SnapUser(u *User) *User {
	if u == nil {
		return nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	cp := *u
	return &cp
}

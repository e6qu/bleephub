package store

// snapRepo / snapIssue / snapPR / snapUser return shallow value copies of a
// shared store entity taken under the store read lock. Webhook payload
// builders read mutable scalar fields (title, body, state, description,
// timestamps) off these entities; a concurrent Update* writer mutates the
// same fields under st.Mu.Lock, so the builders must read a private copy
// rather than the live pointer. Each snapshot takes and releases the read
// lock independently — they are never nested, so a queued writer cannot
// deadlock them (sync.RWMutex read locks are not reentrant).
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

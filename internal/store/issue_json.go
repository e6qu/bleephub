package store

// TimelineCommentToJSON renders a comment in the issue-timeline shape.
func TimelineCommentToJSON(c *Comment, st *Store, baseURL, repoFullName string, issueNumber int, repo *Repo) map[string]interface{} {
	out := CommentToJSON(c, st, baseURL, repoFullName, issueNumber)
	out["event"] = "commented"
	out["actor"] = out["user"]
	out["author_association"] = AuthorAssociation(st, c.AuthorID, repo)
	out["performed_via_github_app"] = nil
	return out
}

// IssueEventForTimelineToJSON renders an IssueEvent to the timeline-event shape.
func IssueEventForTimelineToJSON(e *IssueEvent, st *Store, baseURL, repoFullName string) map[string]interface{} {
	out := IssueEventBase(e, st, baseURL, repoFullName)
	out["performed_via_github_app"] = nil

	switch e.Event {
	case "labeled", "unlabeled":
		st.Mu.RLock()
		var labelJSON interface{}
		if l, ok := st.Labels[e.LabelID]; ok {
			labelJSON = IssueEventLabelToJSON(l)
		}
		st.Mu.RUnlock()
		out["label"] = labelJSON
	case "assigned", "unassigned":
		st.Mu.RLock()
		var assigneeJSON interface{}
		if u, ok := st.Users[e.AssigneeID]; ok {
			assigneeJSON = UserToJSON(u, baseURL)
		}
		st.Mu.RUnlock()
		out["assignee"] = assigneeJSON
	case "milestoned", "demilestoned":
		st.Mu.RLock()
		var milestoneJSON interface{}
		if ms, ok := st.Milestones[e.MilestoneID]; ok {
			milestoneJSON = IssueEventMilestoneToJSON(ms)
		}
		st.Mu.RUnlock()
		out["milestone"] = milestoneJSON
	case "renamed":
		out["rename"] = map[string]interface{}{
			"from": e.RenameFrom,
			"to":   e.RenameTo,
		}
	case "locked", "unlocked":
		lockReason := interface{}(nil)
		if e.Event == "locked" && e.LockReason != "" {
			lockReason = e.LockReason
		}
		out["lock_reason"] = lockReason
	case "review_requested", "review_request_removed":
		st.Mu.RLock()
		var requesterJSON, reviewerJSON interface{}
		if u, ok := st.Users[e.ActorID]; ok {
			requesterJSON = UserToJSON(u, baseURL)
		}
		if u, ok := st.Users[e.RequestedReviewerID]; ok {
			reviewerJSON = UserToJSON(u, baseURL)
		}
		st.Mu.RUnlock()
		// GitHub's actor on review-request events is the requester.
		out["review_requester"] = requesterJSON
		out["requested_reviewer"] = reviewerJSON
	default:
		// opened/closed/reopened/merged map to state-change-issue-event.
		out["state_reason"] = nil
	}
	return out
}

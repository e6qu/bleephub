package store

import (
	"strconv"
	"time"
)

func CommentToJSON(c *Comment, st *Store, baseURL, repoFullName string, issueNumber int) map[string]interface{} {
	c = st.SnapComment(c)
	var authorJSON map[string]interface{}
	st.Mu.RLock()
	if u, ok := st.Users[c.AuthorID]; ok {
		authorJSON = UserToJSON(u, baseURL)
	}
	st.Mu.RUnlock()

	reactions := st.Reactions.SummarizeReactions("issue_comment", c.ID)
	reactions["url"] = baseURL + "/api/v3/repos/" + repoFullName + "/issues/comments/" + strconv.Itoa(c.ID) + "/reactions"

	return map[string]interface{}{
		"id":         c.ID,
		"node_id":    c.NodeID,
		"url":        baseURL + "/api/v3/repos/" + repoFullName + "/issues/comments/" + strconv.Itoa(c.ID),
		"html_url":   baseURL + "/" + repoFullName + "/issues/" + strconv.Itoa(issueNumber) + "#issuecomment-" + strconv.Itoa(c.ID),
		"issue_url":  baseURL + "/api/v3/repos/" + repoFullName + "/issues/" + strconv.Itoa(issueNumber),
		"body":       c.Body,
		"user":       authorJSON,
		"created_at": c.CreatedAt.Format(time.RFC3339),
		"updated_at": c.UpdatedAt.Format(time.RFC3339),
		"reactions":  reactions,
	}
}

// IssueEventBase returns the common fields shared by every issue-event
// response shape.
func IssueEventBase(e *IssueEvent, st *Store, baseURL, repoFullName string) map[string]interface{} {
	st.Mu.RLock()
	var actorJSON map[string]interface{}
	if u, ok := st.Users[e.ActorID]; ok {
		actorJSON = UserToJSON(u, baseURL)
	}
	st.Mu.RUnlock()

	var commitID interface{}
	if e.CommitID != "" {
		commitID = e.CommitID
	}
	var commitURL interface{}
	if e.CommitURL != "" {
		commitURL = e.CommitURL
	} else if e.CommitID != "" {
		commitURL = baseURL + "/api/v3/repos/" + repoFullName + "/commits/" + e.CommitID
	}

	return map[string]interface{}{
		"id":         e.ID,
		"node_id":    e.NodeID,
		"url":        baseURL + "/api/v3/repos/" + repoFullName + "/issues/events/" + strconv.Itoa(e.ID),
		"actor":      actorJSON,
		"event":      e.Event,
		"commit_id":  commitID,
		"commit_url": commitURL,
		"created_at": e.CreatedAt.Format(time.RFC3339),
	}
}

// IssueEventLabelToJSON returns the slim label shape used inside issue
// events (name + color only).
func IssueEventLabelToJSON(l *IssueLabel) map[string]interface{} {
	return map[string]interface{}{
		"name":  l.Name,
		"color": l.Color,
	}
}

// IssueEventMilestoneToJSON returns the slim milestone shape used inside
// issue events (title only).
func IssueEventMilestoneToJSON(ms *Milestone) map[string]interface{} {
	return map[string]interface{}{
		"title": ms.Title,
	}
}

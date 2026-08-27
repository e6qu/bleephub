package store

func clonePRReviewComment(comment *PRReviewComment) *PRReviewComment {
	if comment == nil {
		return nil
	}
	copy := *comment
	copy.Position = cloneInt(comment.Position)
	copy.OriginalPosition = cloneInt(comment.OriginalPosition)
	copy.Line = cloneInt(comment.Line)
	copy.OriginalLine = cloneInt(comment.OriginalLine)
	copy.StartLine = cloneInt(comment.StartLine)
	copy.OriginalStartLine = cloneInt(comment.OriginalStartLine)
	return &copy
}

func newPRReviewCommentRecord(c *PRReviewComment) prReviewCommentRecord {
	return prReviewCommentRecord{
		PRReviewComment: c,
		PullRequestID:   c.PullRequestID,
		AuthorID:        c.AuthorID,
		ThreadID:        c.ThreadID,
		Resolved:        c.Resolved,
		ResolvedByID:    c.ResolvedByID,
	}
}

// ReviewThread groups PR review comments by thread root.
type ReviewThread struct {
	ID           int                `json:"id"`
	IsResolved   bool               `json:"isResolved"`
	ResolvedByID int                `json:"-"` // user who resolved (0 when unresolved)
	Comments     []*PRReviewComment `json:"comments"`
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

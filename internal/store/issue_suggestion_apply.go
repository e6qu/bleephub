package store

import "fmt"

// Applying a pending issue suggestion. The performing switch lives here, not in
// the HTTP handler, because two surfaces reach it — the approve REST route and
// the applyPendingIssueSuggestions GraphQL mutation — and a second copy could
// diverge on what it accepts.

// ErrIssueSuggestionTarget reports a suggestion whose target no longer resolves.
type ErrIssueSuggestionTarget struct {
	Field  string
	Reason string
}

func (e *ErrIssueSuggestionTarget) Error() string {
	return fmt.Sprintf("issue suggestion %s is %s", e.Field, e.Reason)
}

// PerformIssueSuggestion applies one pending suggestion and returns the issue
// event it recorded. It does not touch the suggestion's own state — that is
// ResolveIssueSuggestion's job — so the two steps stay separately committable.
func (st *Store) PerformIssueSuggestion(repo *Repo, issue *Issue, suggestion *IssueSuggestion, userID int) (*IssueEvent, error) {
	if repo == nil || issue == nil || suggestion == nil {
		return nil, &ErrIssueSuggestionTarget{Field: "suggestion", Reason: "invalid"}
	}
	invalidTarget := &ErrIssueSuggestionTarget{Field: "target_id", Reason: "invalid"}
	switch suggestion.Action {
	case "close_issue":
		now := st.CurrentTime()
		st.UpdateIssue(issue.ID, func(item *Issue) {
			item.State = "CLOSED"
			item.ClosedAt = &now
		})
	case "add_label":
		if suggestion.TargetID == nil || st.GetLabel(*suggestion.TargetID) == nil {
			return nil, invalidTarget
		}
		st.AddIssueLabels(repo.FullName, issue.Number, []int{*suggestion.TargetID})
	case "add_assignee":
		if suggestion.TargetID == nil || st.GetUserByID(*suggestion.TargetID) == nil {
			return nil, invalidTarget
		}
		st.AddIssueAssignees(repo.ID, issue.Number, []int{*suggestion.TargetID}, userID)
	case "set_type":
		if suggestion.TargetID == nil || st.GetAssignableIssueTypeForRepo(repo, *suggestion.TargetID) == nil {
			return nil, invalidTarget
		}
		st.UpdateIssue(issue.ID, func(item *Issue) { item.IssueTypeID = *suggestion.TargetID })
	case "add_field":
		if suggestion.TargetID == nil {
			return nil, invalidTarget
		}
		st.AddIssueFieldValues(issue.ID, map[int]interface{}{*suggestion.TargetID: suggestion.TargetValue})
	default:
		return nil, &ErrIssueSuggestionTarget{Field: "action", Reason: "invalid"}
	}
	return st.RecordIssueEvent(repo.ID, issue.ID, userID, "issue_suggestion_approved", map[string]interface{}{}), nil
}

// PendingIssueSuggestion returns the issue's pending suggestion with this id,
// or nil when it is absent or already resolved.
func (st *Store) PendingIssueSuggestion(repoFullName string, issueID, suggestionID int) *IssueSuggestion {
	for _, suggestion := range st.ListIssueSuggestions(repoFullName, issueID) {
		if suggestion.ID == suggestionID && suggestion.State == "pending" {
			return suggestion
		}
	}
	return nil
}

package store

import "fmt"

// Applying a pending issue suggestion.
//
// A suggestion is a triage change an agent proposed rather than performed: a
// label to add, an assignee to set, a type, a custom field value, or closing
// the issue outright. Approving one performs the change and records the
// issue_suggestion_approved event; dismissing one records the decision and
// changes nothing.
//
// The performance lives here rather than in the HTTP handler because two
// surfaces reach it — POST .../suggestions/{id}/approve and the
// applyPendingIssueSuggestions GraphQL mutation — and a second copy of the
// switch is a second chance for one of them to apply a suggestion the other
// would refuse.

// ErrIssueSuggestionTarget reports a suggestion whose target no longer names
// anything the change can be applied to.
type ErrIssueSuggestionTarget struct {
	Field  string
	Reason string
}

func (e *ErrIssueSuggestionTarget) Error() string {
	return fmt.Sprintf("issue suggestion %s is %s", e.Field, e.Reason)
}

// PerformIssueSuggestion applies one pending suggestion to its issue and
// answers the issue event it recorded. It does not change the suggestion's own
// state; ResolveIssueSuggestion does that, so a caller that fails to record
// the resolution has not silently performed half the work.
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
// or nil when there is none (or it has already been resolved).
func (st *Store) PendingIssueSuggestion(repoFullName string, issueID, suggestionID int) *IssueSuggestion {
	for _, suggestion := range st.ListIssueSuggestions(repoFullName, issueID) {
		if suggestion.ID == suggestionID && suggestion.State == "pending" {
			return suggestion
		}
	}
	return nil
}

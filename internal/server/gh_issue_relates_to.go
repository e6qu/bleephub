package bleephub

import (
	"net/http"
	"strconv"

	"github.com/e6qu/bleephub/internal/store"
)

// "Relates to" links between issues: a relationship with no direction, which
// may join issues in different repositories.
// https://docs.github.com/rest/issues/issues#list-issues-related-to-an-issue

func (s *Server) handleListIssueRelatesTo(w http.ResponseWriter, r *http.Request) {
	_, issue := s.issueFromNumberPath(w, r)
	if issue == nil {
		return
	}
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0)
	for _, id := range s.store.ListIssueRelatesTo(issue.ID) {
		// A related issue in a repository the viewer cannot read is not listed:
		// the link does not make it visible.
		related, relatedRepo := s.readableRelatedIssue(r, id)
		if related != nil {
			out = append(out, issueToJSON(related, s.store, base, relatedRepo.FullName))
		}
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleAddIssueRelatesTo(w http.ResponseWriter, r *http.Request) {
	repo, issue := s.issueFromNumberPath(w, r)
	if issue == nil || s.rejectIfArchived(w, repo) {
		return
	}
	var req struct {
		IssueID *int `json:"issue_id"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.IssueID == nil {
		store.WriteGHValidationError(w, "IssueRelatesTo", "issue_id", "missing_field")
		return
	}
	related, relatedRepo := s.readableRelatedIssue(r, *req.IssueID)
	if related == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if related.ID == issue.ID {
		writeGHError(w, http.StatusUnprocessableEntity, "An issue may not be related to itself")
		return
	}
	if !s.store.AddIssueRelatesTo(issue.ID, related.ID) {
		writeGHError(w, http.StatusUnprocessableEntity, "The issues are already related")
		return
	}
	s.emitIssueRelatesTo(r, "relates_to_added", repo, issue, relatedRepo, related)
	relatedJSON := issueToJSON(related, s.store, s.baseURL(r), relatedRepo.FullName)
	writeJSONCreated(w, jsonStringField(relatedJSON, "url"), relatedJSON)
}

func (s *Server) handleRemoveIssueRelatesTo(w http.ResponseWriter, r *http.Request) {
	repo, issue := s.issueFromNumberPath(w, r)
	if issue == nil || s.rejectIfArchived(w, repo) {
		return
	}
	relatedID, err := strconv.Atoi(r.PathValue("issue_id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "issue_id must be an integer")
		return
	}
	related, relatedRepo := s.readableRelatedIssue(r, relatedID)
	if related == nil || !s.store.RemoveIssueRelatesTo(issue.ID, related.ID) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.emitIssueRelatesTo(r, "relates_to_removed", repo, issue, relatedRepo, related)
	writeJSON(w, http.StatusOK, issueToJSON(related, s.store, s.baseURL(r), relatedRepo.FullName))
}

// readableRelatedIssue resolves the issue a relationship names, with its
// repository, when the viewer may read that repository's issues; otherwise it
// returns nil, which the caller answers as not found so that an unreadable
// issue's existence is not disclosed.
func (s *Server) readableRelatedIssue(r *http.Request, issueID int) (*store.Issue, *store.Repo) {
	related := s.store.GetIssue(issueID)
	if related == nil {
		return nil, nil
	}
	relatedRepo := s.store.GetRepoByID(related.RepoID)
	if relatedRepo == nil || !s.viewerHasRepoPermission(r.Context(), relatedRepo, store.ScopeIssues, store.PermRead) {
		return nil, nil
	}
	return related, relatedRepo
}

// emitIssueRelatesTo delivers the issue_relates_to event. Between issues of one
// repository GitHub sends one delivery naming both; between repositories it
// sends one to each, naming only that repository's issue, so that neither
// repository learns the other's details.
// https://docs.github.com/webhooks/webhook-events-and-payloads#issue-relates-to
func (s *Server) emitIssueRelatesTo(r *http.Request, action string, repo *store.Repo, issue *store.Issue, relatedRepo *store.Repo, related *store.Issue) {
	sender := ghUserFromContext(r.Context())
	base := s.baseURL(r)
	payload := func(onRepo *store.Repo, own *store.Issue) map[string]interface{} {
		return map[string]interface{}{
			"action":     action,
			"issue_id":   own.ID,
			"issue":      issueToJSON(own, s.store, base, onRepo.FullName),
			"repository": repoPayload(s.store.SnapRepo(onRepo), base),
			"sender":     senderPayload(s.store.SnapUser(sender), base),
		}
	}
	if relatedRepo.ID == repo.ID {
		sameRepo := payload(repo, issue)
		sameRepo["related_issue_id"] = related.ID
		sameRepo["related_issue"] = issueToJSON(related, s.store, base, repo.FullName)
		s.emitWebhookEvent(repo.FullName, "issue_relates_to", action, sameRepo)
		return
	}
	s.emitWebhookEvent(repo.FullName, "issue_relates_to", action, payload(repo, issue))
	s.emitWebhookEvent(relatedRepo.FullName, "issue_relates_to", action, payload(relatedRepo, related))
}

package bleephub

import (
	"net/http"
	"strconv"

	"github.com/e6qu/bleephub/internal/store"
)

// Sub-issues and issue dependencies. Both are bidirectional links in the issues
// store: a sub-issue knows its parent, and a blocked issue shows as blocking on
// the other side.

// issueFromNumberPath resolves {owner}/{repo} + "number" to the repo and issue,
// writing a 404 when either is missing. It enforces private-repo visibility:
// the read routes that share it (sub-issues, parent, dependencies, label list,
// assignee check) dispatch through an ungated GET, so without this gate a
// private repo's issue data would leak to unauthorized viewers. Write callers
// are already requirePerm(PermWrite)-gated, for which this read gate is a no-op.
func (s *Server) issueFromNumberPath(w http.ResponseWriter, r *http.Request) (*store.Repo, *store.Issue) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return nil, nil
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	issue := s.store.GetIssueByNumber(repo.ID, number)
	if issue == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	return repo, issue
}

func (s *Server) handleListSubIssues(w http.ResponseWriter, r *http.Request) {
	repo, issue := s.issueFromNumberPath(w, r)
	if issue == nil {
		return
	}
	ids := s.store.ListSubIssues(issue.ID)
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(ids))
	for _, id := range ids {
		if child := s.store.GetIssue(id); child != nil {
			out = append(out, issueToJSON(child, s.store, base, repo.FullName))
		}
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleCreateSubIssue(w http.ResponseWriter, r *http.Request) {
	repo, issue := s.issueFromNumberPath(w, r)
	if issue == nil {
		return
	}
	var req struct {
		SubIssueID    *int     `json:"sub_issue_id"`
		ReplaceParent flexBool `json:"replace_parent"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.SubIssueID == nil {
		store.WriteGHValidationError(w, "SubIssue", "sub_issue_id", "missing_field")
		return
	}
	child := s.store.GetIssue(*req.SubIssueID)
	if child == nil || child.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if err := s.store.AddSubIssue(issue.ID, child.ID, bool(req.ReplaceParent)); err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	subJSON := issueToJSON(s.store.GetIssue(issue.ID), s.store, s.baseURL(r), repo.FullName)
	writeJSONCreated(w, jsonStringField(subJSON, "url"), subJSON)
}

func (s *Server) handleRemoveSubIssue(w http.ResponseWriter, r *http.Request) {
	repo, issue := s.issueFromNumberPath(w, r)
	if issue == nil {
		return
	}
	var req struct {
		SubIssueID *int `json:"sub_issue_id"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.SubIssueID == nil {
		store.WriteGHValidationError(w, "SubIssue", "sub_issue_id", "missing_field")
		return
	}
	child := s.store.GetIssue(*req.SubIssueID)
	if child == nil || child.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if err := s.store.RemoveSubIssue(issue.ID, child.ID); err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, issueToJSON(child, s.store, s.baseURL(r), repo.FullName))
}

func (s *Server) handleReprioritizeSubIssue(w http.ResponseWriter, r *http.Request) {
	repo, issue := s.issueFromNumberPath(w, r)
	if issue == nil {
		return
	}
	var req struct {
		SubIssueID *int `json:"sub_issue_id"`
		AfterID    *int `json:"after_id"`
		BeforeID   *int `json:"before_id"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.SubIssueID == nil {
		writeGHValidationErrorSimple(w, "sub_issue_id is missing")
		return
	}
	if req.AfterID != nil && req.BeforeID != nil {
		writeGHValidationErrorSimple(w, "after_id and before_id are mutually exclusive")
		return
	}
	if err := s.store.ReprioritizeSubIssue(issue.ID, *req.SubIssueID, req.AfterID, req.BeforeID); err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, issueToJSON(s.store.GetIssue(issue.ID), s.store, s.baseURL(r), repo.FullName))
}

func (s *Server) handleListIssueDependenciesBlockedBy(w http.ResponseWriter, r *http.Request) {
	repo, issue := s.issueFromNumberPath(w, r)
	if issue == nil {
		return
	}
	ids := s.store.ListIssueBlockedBy(issue.ID)
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(ids))
	for _, id := range ids {
		if blocker := s.store.GetIssue(id); blocker != nil {
			out = append(out, issueToJSON(blocker, s.store, base, repo.FullName))
		}
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleListIssueDependenciesBlocking(w http.ResponseWriter, r *http.Request) {
	repo, issue := s.issueFromNumberPath(w, r)
	if issue == nil {
		return
	}
	ids := s.store.ListIssueBlocking(issue.ID)
	out := make([]map[string]interface{}, 0, len(ids))
	for _, id := range ids {
		if blocked := s.store.GetIssue(id); blocked != nil {
			out = append(out, issueToJSON(blocked, s.store, s.baseURL(r), repo.FullName))
		}
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleGetIssueParent(w http.ResponseWriter, r *http.Request) {
	repo, issue := s.issueFromNumberPath(w, r)
	if issue == nil {
		return
	}
	parent := s.store.GetIssue(s.store.GetSubIssueParent(issue.ID))
	if parent == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, issueToJSON(parent, s.store, s.baseURL(r), repo.FullName))
}

func (s *Server) handleAddIssueDependencyBlockedBy(w http.ResponseWriter, r *http.Request) {
	repo, issue := s.issueFromNumberPath(w, r)
	if issue == nil {
		return
	}
	var req struct {
		IssueID *int `json:"issue_id"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.IssueID == nil {
		store.WriteGHValidationError(w, "IssueDependency", "issue_id", "missing_field")
		return
	}
	blocker := s.store.GetIssue(*req.IssueID)
	if blocker == nil || blocker.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if blocker.ID == issue.ID {
		writeGHError(w, http.StatusUnprocessableEntity, "An issue may not block itself")
		return
	}
	if !s.store.AddIssueBlockedBy(issue.ID, blocker.ID) {
		writeGHError(w, http.StatusUnprocessableEntity, "The issue dependency already exists")
		return
	}
	blockerJSON := issueToJSON(blocker, s.store, s.baseURL(r), repo.FullName)
	writeJSONCreated(w, jsonStringField(blockerJSON, "url"), blockerJSON)
}

func (s *Server) handleRemoveIssueDependencyBlockedBy(w http.ResponseWriter, r *http.Request) {
	repo, issue := s.issueFromNumberPath(w, r)
	if issue == nil {
		return
	}
	blockerID, err := strconv.Atoi(r.PathValue("issue_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	blocker := s.store.GetIssue(blockerID)
	if blocker == nil || blocker.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.store.RemoveIssueBlockedBy(issue.ID, blocker.ID) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, issueToJSON(blocker, s.store, s.baseURL(r), repo.FullName))
}

package bleephub

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// --- Issue handlers ---

func (s *Server) handleCreateIssue(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var req struct {
		Title       string   `json:"title"`
		Body        string   `json:"body"`
		Labels      []string `json:"labels"`
		Assignees   []string `json:"assignees"`
		Milestone   int      `json:"milestone"` // milestone number
		IssueTypeID *int     `json:"issue_type_id"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Title == "" {
		store.WriteGHValidationError(w, "Issue", "title", "missing_field")
		return
	}

	// Resolve label names to IDs
	var labelIDs []int
	for _, name := range req.Labels {
		l := s.store.GetLabelByName(repo.ID, name)
		if l != nil {
			labelIDs = append(labelIDs, l.ID)
		}
	}

	// Resolve assignee logins to IDs
	var assigneeIDs []int
	for _, login := range req.Assignees {
		u := s.store.LookupUserByLogin(login)
		if u != nil {
			assigneeIDs = append(assigneeIDs, u.ID)
		}
	}

	// Resolve milestone number to ID
	var milestoneID int
	if req.Milestone > 0 {
		ms := s.store.GetMilestoneByNumber(repo.ID, req.Milestone)
		if ms != nil {
			milestoneID = ms.ID
		}
	}

	var issueTypeID int
	if req.IssueTypeID != nil && *req.IssueTypeID > 0 {
		it := s.store.GetAssignableIssueTypeForRepo(repo, *req.IssueTypeID)
		if it == nil {
			store.WriteGHValidationError(w, "Issue", "issue_type_id", "invalid")
			return
		}
		issueTypeID = it.ID
	}

	issue := s.store.CreateIssue(repo.ID, user.ID, req.Title, req.Body, labelIDs, assigneeIDs, milestoneID)
	if issue == nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Issue creation failed")
		return
	}
	if issueTypeID > 0 {
		s.store.UpdateIssue(issue.ID, func(i *store.Issue) {
			i.IssueTypeID = issueTypeID
		})
		issue = s.store.GetIssue(issue.ID)
	}
	repoKey := owner + "/" + name
	s.emitWebhookEvent(repoKey, "issues", "opened", buildIssuesPayload(s.store, repo, issue, user, "opened"))

	s.recordAuditEvent("issues.create", user.Login, "", map[string]interface{}{"repo": repoKey, "issue_id": issue.ID, "title": issue.Title})
	issueJSON := issueToJSON(issue, s.store, s.baseURL(r), repo.FullName)
	writeJSONCreated(w, jsonStringField(issueJSON, "url"), issueJSON)
}

func (s *Server) handleListIssues(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}

	// Map REST state to internal state
	var stateFilter string
	switch state {
	case "open":
		stateFilter = "OPEN"
	case "closed":
		stateFilter = "CLOSED"
	case "all":
		stateFilter = "all"
	default:
		store.WriteGHValidationError(w, "Issue", "state", "invalid")
		return
	}

	query := r.URL.Query()
	var labelNames []string
	if labelsParam := query.Get("labels"); labelsParam != "" {
		labelNames = strings.Split(labelsParam, ",")
	}
	assignee := query.Get("assignee")
	var assigneeID int
	if assignee != "" && assignee != "*" && assignee != "none" {
		if u := s.store.LookupUserByLogin(assignee); u != nil {
			assigneeID = u.ID
		} else {
			// An unknown assignee is a valid filter with no matches. Treating
			// zero as "no filter" widened the result to every issue.
			assigneeID = -1
		}
	}
	selected := func(labelIDs, assigneeIDs []int) bool {
		if !store.LabelIDsCoverNames(s.store, labelIDs, labelNames) {
			return false
		}
		switch assignee {
		case "":
			return true
		case "*":
			return len(assigneeIDs) > 0
		case "none":
			return len(assigneeIDs) == 0
		}
		for _, aid := range assigneeIDs {
			if aid == assigneeID {
				return true
			}
		}
		return false
	}

	var creatorID int
	if creator := query.Get("creator"); creator != "" {
		if u := s.store.LookupUserByLogin(creator); u != nil {
			creatorID = u.ID
		} else {
			creatorID = -1
		}
	}
	milestoneFilter := query.Get("milestone")
	var milestoneID int
	if milestoneFilter != "" && milestoneFilter != "*" && milestoneFilter != "none" {
		number, err := strconv.Atoi(milestoneFilter)
		if err != nil || number < 1 {
			store.WriteGHValidationError(w, "Issue", "milestone", "invalid")
			return
		}
		milestone := s.store.GetMilestoneByNumber(repo.ID, number)
		if milestone == nil {
			// Like an unknown assignee, a valid but absent milestone number
			// selects an empty set rather than dropping the filter.
			milestoneID = -1
		} else {
			milestoneID = milestone.ID
		}
	}
	var since time.Time
	if rawSince := query.Get("since"); rawSince != "" {
		parsed, err := time.Parse(time.RFC3339, rawSince)
		if err != nil {
			store.WriteGHValidationError(w, "Issue", "since", "invalid")
			return
		}
		since = parsed
	}
	sortField := query.Get("sort")
	if sortField == "" {
		sortField = "created"
	}
	if sortField != "created" && sortField != "updated" && sortField != "comments" {
		store.WriteGHValidationError(w, "Issue", "sort", "invalid")
		return
	}
	direction := query.Get("direction")
	if direction == "" {
		direction = "desc"
	}
	if direction != "asc" && direction != "desc" {
		store.WriteGHValidationError(w, "Issue", "direction", "invalid")
		return
	}
	mentioned := strings.ToLower(strings.TrimSpace(query.Get("mentioned")))
	matchesMention := func(body, parentType string, parentID int) bool {
		if mentioned == "" {
			return true
		}
		needle := "@" + mentioned
		if strings.Contains(strings.ToLower(body), needle) {
			return true
		}
		for _, comment := range s.store.ListCommentsFor(parentType, parentID) {
			if strings.Contains(strings.ToLower(comment.Body), needle) {
				return true
			}
		}
		return false
	}
	matchesCommon := func(authorID, itemMilestoneID int, updatedAt time.Time, body, parentType string, parentID int) bool {
		if creatorID != 0 && authorID != creatorID {
			return false
		}
		switch milestoneFilter {
		case "":
		case "*":
			if itemMilestoneID == 0 {
				return false
			}
		case "none":
			if itemMilestoneID != 0 {
				return false
			}
		default:
			if itemMilestoneID != milestoneID {
				return false
			}
		}
		return (since.IsZero() || !updatedAt.Before(since)) &&
			matchesMention(body, parentType, parentID)
	}

	// Every pull request is also an issue on GitHub, so this listing returns
	// both and the `pull_request` member is what tells them apart. Omitting
	// them made pull requests invisible to any client that reaches for
	// issues — `gh issue list` among them.
	base := s.baseURL(r)
	type issueRow struct {
		number       int
		createdAt    time.Time
		updatedAt    time.Time
		commentCount int
		json         map[string]interface{}
	}
	var rows []issueRow
	for _, storedIssue := range s.store.ListIssues(repo.ID, stateFilter) {
		issue := s.store.SnapIssue(storedIssue)
		if !selected(issue.LabelIDs, issue.AssigneeIDs) ||
			!matchesCommon(issue.AuthorID, issue.MilestoneID, issue.UpdatedAt, issue.Body, "issue", issue.ID) {
			continue
		}
		rows = append(rows, issueRow{
			number: issue.Number, createdAt: issue.CreatedAt, updatedAt: issue.UpdatedAt,
			commentCount: len(s.store.ListCommentsFor("issue", issue.ID)),
			json:         issueToJSON(issue, s.store, base, repo.FullName),
		})
	}
	for _, storedPR := range s.store.ListPullRequests(repo.ID, stateFilter) {
		pr := s.store.SnapPR(storedPR)
		if !selected(pr.LabelIDs, pr.AssigneeIDs) ||
			!matchesCommon(pr.AuthorID, pr.MilestoneID, pr.UpdatedAt, pr.Body, "pull_request", pr.ID) {
			continue
		}
		rows = append(rows, issueRow{
			number: pr.Number, createdAt: pr.CreatedAt, updatedAt: pr.UpdatedAt,
			commentCount: len(s.store.ListCommentsFor("pull_request", pr.ID)),
			json:         issueToJSONForPR(pr, s.store, base, repo.FullName),
		})
	}

	// Newest first, GitHub's default sort. Both stores iterate a map, so
	// without this the page a caller gets back is a different subset each
	// time they ask.
	sort.Slice(rows, func(i, j int) bool {
		var less bool
		switch sortField {
		case "updated":
			less = rows[i].updatedAt.Before(rows[j].updatedAt)
			if rows[i].updatedAt.Equal(rows[j].updatedAt) {
				less = rows[i].number < rows[j].number
			}
		case "comments":
			less = rows[i].commentCount < rows[j].commentCount
			if rows[i].commentCount == rows[j].commentCount {
				less = rows[i].number < rows[j].number
			}
		default:
			less = rows[i].createdAt.Before(rows[j].createdAt)
			if rows[i].createdAt.Equal(rows[j].createdAt) {
				less = rows[i].number < rows[j].number
			}
		}
		if direction == "desc" {
			return !less && rows[i].number != rows[j].number
		}
		return less
	})

	result := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		result = append(result, row.json)
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	numStr := r.PathValue("number")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	issue := s.store.GetIssueByNumber(repo.ID, num)
	if issue == nil {
		// Issues and pull requests share one number sequence, and a pull
		// request is reachable through the issues endpoint on GitHub. 404 here
		// hid every pull request from clients that address it as an issue.
		if pr := s.store.GetPullRequestByNumber(repo.ID, num); pr != nil {
			writeJSON(w, http.StatusOK, issueToJSONForPR(pr, s.store, s.baseURL(r), repo.FullName))
			return
		}
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	writeJSON(w, http.StatusOK, issueToJSON(issue, s.store, s.baseURL(r), repo.FullName))
}

func (s *Server) handleUpdateIssue(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	numStr := r.PathValue("number")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	issue := s.store.GetIssueByNumber(repo.ID, num)
	if issue == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// Optimistic concurrency: an If-Match must match the issue's current ETag,
	// or a stale client would clobber a concurrent edit (STORE-016).
	if !checkIfMatch(w, r, issueToJSON(issue, s.store, s.baseURL(r), repo.FullName)) {
		return
	}

	var req map[string]interface{}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// Resolve milestone, labels, and assignees before taking the write
	// lock so an invalid milestone number 422s without mutating anything.
	// An explicit `"milestone": null` clears the milestone; an absent
	// member keeps it — the map lookup distinguishes the two.
	var milestoneID *int
	if v, present := req["milestone"]; present {
		switch mv := v.(type) {
		case nil:
			cleared := 0
			milestoneID = &cleared
		case float64:
			ms := s.store.GetMilestoneByNumber(repo.ID, int(mv))
			if ms == nil {
				store.WriteGHValidationError(w, "Issue", "milestone", "invalid")
				return
			}
			milestoneID = &ms.ID
		default:
			store.WriteGHValidationError(w, "Issue", "milestone", "invalid")
			return
		}
	}
	var labelIDs *[]int
	if v, present := req["labels"]; present {
		entries, ok := v.([]interface{})
		if !ok {
			store.WriteGHValidationError(w, "Issue", "labels", "invalid")
			return
		}
		ids := make([]int, 0, len(entries))
		for _, entry := range entries {
			// The documented body allows bare strings or {"name": ...}
			// objects; unknown label names are dropped, matching the
			// add-labels endpoint's semantics.
			name, ok := entry.(string)
			if !ok {
				obj, isObj := entry.(map[string]interface{})
				if !isObj {
					store.WriteGHValidationError(w, "Issue", "labels", "invalid")
					return
				}
				if name, ok = obj["name"].(string); !ok {
					store.WriteGHValidationError(w, "Issue", "labels", "invalid")
					return
				}
			}
			if l := s.store.GetLabelByName(repo.ID, name); l != nil {
				ids = append(ids, l.ID)
			}
		}
		labelIDs = &ids
	}
	var assigneeIDs *[]int
	if v, present := req["assignees"]; present {
		entries, ok := v.([]interface{})
		if !ok {
			store.WriteGHValidationError(w, "Issue", "assignees", "invalid")
			return
		}
		ids := make([]int, 0, len(entries))
		for _, entry := range entries {
			login, ok := entry.(string)
			if !ok {
				store.WriteGHValidationError(w, "Issue", "assignees", "invalid")
				return
			}
			if u := s.store.LookupUserByLogin(login); u != nil {
				ids = append(ids, u.ID)
			}
		}
		assigneeIDs = &ids
	}
	var issueTypeID *int
	if v, present := req["issue_type_id"]; present {
		switch tv := v.(type) {
		case nil:
			cleared := 0
			issueTypeID = &cleared
		case float64:
			if tv <= 0 {
				cleared := 0
				issueTypeID = &cleared
				break
			}
			it := s.store.GetAssignableIssueTypeForRepo(repo, int(tv))
			if it == nil {
				store.WriteGHValidationError(w, "Issue", "issue_type_id", "invalid")
				return
			}
			resolved := it.ID
			issueTypeID = &resolved
		default:
			store.WriteGHValidationError(w, "Issue", "issue_type_id", "invalid")
			return
		}
	}

	s.store.Mu.RLock()
	previousState := issue.State
	s.store.Mu.RUnlock()
	s.store.UpdateIssue(issue.ID, func(i *store.Issue) {
		if v, ok := req["title"].(string); ok {
			i.Title = v
		}
		if v, ok := req["body"].(string); ok {
			i.Body = v
		}
		if milestoneID != nil {
			i.MilestoneID = *milestoneID
		}
		if labelIDs != nil {
			i.LabelIDs = *labelIDs
		}
		if assigneeIDs != nil {
			i.AssigneeIDs = *assigneeIDs
		}
		if issueTypeID != nil {
			i.IssueTypeID = *issueTypeID
		}
		if v, ok := req["state"].(string); ok {
			switch v {
			case "closed":
				i.State = "CLOSED"
				now := time.Now()
				i.ClosedAt = &now
				if i.StateReason == "" {
					i.StateReason = "COMPLETED"
				}
			case "open":
				i.State = "OPEN"
				i.ClosedAt = nil
				i.StateReason = ""
			}
		}
		if v, ok := req["state_reason"].(string); ok {
			i.StateReason = strings.ToUpper(v)
		}
	})

	updated := s.store.GetIssue(issue.ID)

	if v, ok := req["state"].(string); ok {
		action := "edited"
		if v == "closed" && previousState != "CLOSED" {
			action = "closed"
			s.store.RecordIssueEvent(repo.ID, issue.ID, user.ID, action, nil)
		} else if v == "open" && previousState != "OPEN" {
			action = "reopened"
			s.store.RecordIssueEvent(repo.ID, issue.ID, user.ID, action, nil)
		}
		repoKey := owner + "/" + repoName
		s.emitWebhookEvent(repoKey, "issues", action, buildIssuesPayload(s.store, repo, updated, user, action))
	}

	writeJSON(w, http.StatusOK, issueToJSON(updated, s.store, s.baseURL(r), repo.FullName))
}

// --- Comment handlers ---

func (s *Server) handleCreateIssueComment(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	numStr := r.PathValue("number")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// /issues/{n}/comments routes resolve to either an Issue or a PR by
	// number — real GitHub treats PRs as issues for this endpoint. The
	// resolver reads the mutable Locked flag under the store lock.
	parentType, parentID, parentNumber, locked, found := s.store.ResolveCommentParent(repo.ID, num)
	if !found {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if locked {
		writeGHError(w, http.StatusForbidden, "Conversation is locked.")
		return
	}

	var req struct {
		Body string `json:"body"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Body == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}

	comment := s.store.CreateCommentFor(parentType, parentID, user.ID, req.Body)
	if comment == nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Comment creation failed")
		return
	}

	s.emitWebhookEvent(repo.FullName, "issue_comment", "created",
		buildIssueCommentPayload(s.store, repo, comment, user, "created", s.baseURL(r), parentNumber))
	commentJSON := store.CommentToJSON(comment, s.store, s.baseURL(r), repo.FullName, parentNumber)
	writeJSONCreated(w, jsonStringField(commentJSON, "url"), commentJSON)
}

func (s *Server) handleListIssueComments(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	numStr := r.PathValue("number")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	parentType := "issue"
	var parentID, parentNumber int
	if issue := s.store.GetIssueByNumber(repo.ID, num); issue != nil {
		parentID, parentNumber = issue.ID, issue.Number
	} else if pr := s.store.GetPullRequestByNumber(repo.ID, num); pr != nil {
		parentType = "pull_request"
		parentID, parentNumber = pr.ID, pr.Number
	} else {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	comments := s.store.ListCommentsFor(parentType, parentID)
	comments, ok := filterSince(w, r, "IssueComment", comments, func(comment *store.Comment) time.Time {
		return comment.UpdatedAt
	})
	if !ok {
		return
	}
	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(comments))
	for _, c := range comments {
		result = append(result, store.CommentToJSON(c, s.store, base, repo.FullName, parentNumber))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

// --- Issue label management handlers ---

// labelIDsToJSON resolves a slice of label IDs into GitHub label JSON, in the
// stored order, skipping any that no longer resolve.
func (s *Server) labelIDsToJSON(labelIDs []int, base, repoFullName string) []map[string]interface{} {
	labels := make([]map[string]interface{}, 0, len(labelIDs))
	for _, lid := range labelIDs {
		if l := s.store.GetLabel(lid); l != nil {
			labels = append(labels, issueLabelToJSON(l, base, repoFullName))
		}
	}
	return labels
}

func (s *Server) handleAddIssueLabels(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	numStr := r.PathValue("number")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	issue := s.store.GetIssueByNumber(repo.ID, num)
	pr := (*store.PullRequest)(nil)
	if issue == nil {
		if pr = s.store.GetPullRequestByNumber(repo.ID, num); pr == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
	}

	labelNames, ok := decodeIssueLabelsBody(w, r)
	if !ok {
		return
	}

	// Resolve label names to IDs before taking write lock
	var newLabelIDs []int
	for _, name := range labelNames {
		l := s.store.GetLabelByName(repo.ID, name)
		if l != nil {
			newLabelIDs = append(newLabelIDs, l.ID)
		}
	}

	base := s.baseURL(r)
	if pr != nil {
		// Pull requests carry labels through the same surface real GitHub
		// exposes; PRs share the issue number space.
		s.store.AddPullRequestLabels(repo.ID, pr.Number, newLabelIDs, user.ID)
		updated := s.store.GetPullRequestByNumber(repo.ID, pr.Number)
		writeJSON(w, http.StatusOK, s.labelIDsToJSON(updated.LabelIDs, base, repo.FullName))
		return
	}

	s.store.UpdateIssue(issue.ID, func(i *store.Issue) {
		for _, lid := range newLabelIDs {
			found := false
			for _, existing := range i.LabelIDs {
				if existing == lid {
					found = true
					break
				}
			}
			if !found {
				i.LabelIDs = append(i.LabelIDs, lid)
			}
		}
	})

	// Return current labels
	updated := s.store.GetIssue(issue.ID)
	writeJSON(w, http.StatusOK, s.labelIDsToJSON(updated.LabelIDs, base, repo.FullName))
}

func (s *Server) handleRemoveIssueLabel(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	numStr := r.PathValue("number")
	labelName := r.PathValue("name")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	issue := s.store.GetIssueByNumber(repo.ID, num)
	pr := (*store.PullRequest)(nil)
	if issue == nil {
		if pr = s.store.GetPullRequestByNumber(repo.ID, num); pr == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
	}

	label := s.store.GetLabelByName(repo.ID, labelName)
	if label == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	if pr != nil {
		s.store.RemovePullRequestLabel(repo.ID, pr.Number, label.ID, user.ID)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	s.store.UpdateIssue(issue.ID, func(i *store.Issue) {
		for idx, lid := range i.LabelIDs {
			if lid == label.ID {
				i.LabelIDs = append(i.LabelIDs[:idx], i.LabelIDs[idx+1:]...)
				break
			}
		}
	})

	w.WriteHeader(http.StatusNoContent)
}

// --- Repo-level comment handlers ---

func (s *Server) handleListRepoIssueComments(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	comments := s.store.ListRepoIssueComments(repo.ID)
	comments, ok := filterSince(w, r, "IssueComment", comments, func(comment *store.Comment) time.Time {
		return comment.UpdatedAt
	})
	if !ok {
		return
	}
	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(comments))
	for _, c := range comments {
		parentNumber := 0
		if issue := s.store.GetIssue(c.IssueID); issue != nil {
			parentNumber = issue.Number
		}
		result = append(result, store.CommentToJSON(c, s.store, base, repo.FullName, parentNumber))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleGetIssueComment(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	idStr := r.PathValue("comment_id")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	id, err := strconv.Atoi(idStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	comment := s.store.GetIssueComment(id)
	if comment == nil || comment.ParentType != "issue" || s.store.GetIssue(comment.IssueID) == nil || s.store.GetIssue(comment.IssueID).RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	parentNumber := commentParentNumber(s.store, comment)
	writeJSON(w, http.StatusOK, store.CommentToJSON(comment, s.store, s.baseURL(r), repo.FullName, parentNumber))
}

// --- Issue label set/clear handlers ---

func (s *Server) handleSetIssueLabels(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	numStr := r.PathValue("number")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	issue := s.store.GetIssueByNumber(repo.ID, num)
	pr := (*store.PullRequest)(nil)
	if issue == nil {
		if pr = s.store.GetPullRequestByNumber(repo.ID, num); pr == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
	}

	labelNames, ok := decodeIssueLabelsBody(w, r)
	if !ok {
		return
	}

	var labelIDs []int
	for _, name := range labelNames {
		if l := s.store.GetLabelByName(repo.ID, name); l != nil {
			labelIDs = append(labelIDs, l.ID)
		}
	}

	base := s.baseURL(r)
	if pr != nil {
		s.store.SetPullRequestLabels(repo.ID, pr.Number, labelIDs, user.ID)
		updated := s.store.GetPullRequestByNumber(repo.ID, pr.Number)
		writeJSON(w, http.StatusOK, s.labelIDsToJSON(updated.LabelIDs, base, repo.FullName))
		return
	}

	s.store.SetIssueLabels(repo.ID, issue.Number, labelIDs, user.ID)

	updated := s.store.GetIssue(issue.ID)
	writeJSON(w, http.StatusOK, s.labelIDsToJSON(updated.LabelIDs, base, repo.FullName))
}

func (s *Server) handleClearIssueLabels(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	numStr := r.PathValue("number")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	issue := s.store.GetIssueByNumber(repo.ID, num)
	if issue == nil {
		if pr := s.store.GetPullRequestByNumber(repo.ID, num); pr != nil {
			s.store.ClearPullRequestLabels(repo.ID, pr.Number, user.ID)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	s.store.ClearIssueLabels(repo.ID, issue.Number, user.ID)
	w.WriteHeader(http.StatusNoContent)
}

// --- Issue assignee handlers ---

func (s *Server) handleAddIssueAssignees(w http.ResponseWriter, r *http.Request) {
	repo, issue, ok := s.resolveRepoIssue(w, r)
	if !ok {
		return
	}
	user := ghUserFromContext(r.Context())

	var req struct {
		Assignees []string `json:"assignees"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	assigneeIDs := resolveUserIDs(s.store, req.Assignees)
	s.store.AddIssueAssignees(repo.ID, issue.Number, assigneeIDs, user.ID)
	// Real GitHub responds 201 Created when adding assignees.
	issueJSON := issueToJSON(s.store.GetIssue(issue.ID), s.store, s.baseURL(r), repo.FullName)
	writeJSONCreated(w, jsonStringField(issueJSON, "url"), issueJSON)
}

func (s *Server) handleRemoveIssueAssignees(w http.ResponseWriter, r *http.Request) {
	repo, issue, ok := s.resolveRepoIssue(w, r)
	if !ok {
		return
	}
	user := ghUserFromContext(r.Context())

	var req struct {
		Assignees []string `json:"assignees"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	assigneeIDs := resolveUserIDs(s.store, req.Assignees)
	s.store.RemoveIssueAssignees(repo.ID, issue.Number, assigneeIDs, user.ID)
	writeJSON(w, http.StatusOK, issueToJSON(s.store.GetIssue(issue.ID), s.store, s.baseURL(r), repo.FullName))
}

// --- Comment pin handlers ---

func (s *Server) handlePinIssueComment(w http.ResponseWriter, r *http.Request) {
	repo, comment, ok := s.resolveRepoIssueComment(w, r)
	if !ok {
		return
	}

	s.store.PinIssueComment(comment.ID)
	parentNumber := commentParentNumber(s.store, comment)
	writeJSON(w, http.StatusOK, store.CommentToJSON(s.store.GetIssueComment(comment.ID), s.store, s.baseURL(r), repo.FullName, parentNumber))
}

func (s *Server) handleUnpinIssueComment(w http.ResponseWriter, r *http.Request) {
	repo, comment, ok := s.resolveRepoIssueComment(w, r)
	if !ok {
		return
	}

	s.store.UnpinIssueComment(comment.ID)
	parentNumber := commentParentNumber(s.store, comment)
	writeJSON(w, http.StatusOK, store.CommentToJSON(s.store.GetIssueComment(comment.ID), s.store, s.baseURL(r), repo.FullName, parentNumber))
}

// resolveRepoIssue resolves owner/repo/{number} and returns the repo + issue,
// writing the appropriate error response on failure.
func (s *Server) resolveRepoIssue(w http.ResponseWriter, r *http.Request) (*store.Repo, *store.Issue, bool) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	numStr := r.PathValue("number")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil, false
	}

	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil, false
	}

	issue := s.store.GetIssueByNumber(repo.ID, num)
	if issue == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil, false
	}
	return repo, issue, true
}

// resolveRepoIssueComment resolves owner/repo/{comment_id} and returns the repo
// + issue comment, writing the appropriate error response on failure.
func (s *Server) resolveRepoIssueComment(w http.ResponseWriter, r *http.Request) (*store.Repo, *store.Comment, bool) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	idStr := r.PathValue("comment_id")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil, false
	}

	id, err := strconv.Atoi(idStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil, false
	}

	comment := s.store.GetIssueComment(id)
	if comment == nil || comment.ParentType != "issue" {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil, false
	}
	issue := s.store.GetIssue(comment.IssueID)
	if issue == nil || issue.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil, false
	}
	return repo, comment, true
}

func resolveUserIDs(st *store.Store, logins []string) []int {
	var ids []int
	for _, login := range logins {
		if u := st.LookupUserByLogin(login); u != nil {
			ids = append(ids, u.ID)
		}
	}
	return ids
}

// --- Issue timeline + events handlers ---

func (s *Server) handleListIssueTimeline(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	numStr := r.PathValue("number")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// Pull requests share the issue number space and are timeline-capable
	// on real GitHub; resolve the number to whichever exists.
	if issue := s.store.GetIssueByNumber(repo.ID, num); issue != nil {
		timeline := s.store.BuildIssueTimeline(repo, issue.ID, s.baseURL(r))
		timeline = s.mergeCrossReferencedEvents(repo, num, s.baseURL(r), timeline)
		writeJSON(w, http.StatusOK, paginateAndLink(w, r, timeline))
		return
	}
	pr := s.store.GetPullRequestByNumber(repo.ID, num)
	if pr == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	timeline, err := s.buildPullRequestTimeline(repo, pr, s.baseURL(r))
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "timeline derivation failed")
		return
	}
	timeline = s.mergeCrossReferencedEvents(repo, num, s.baseURL(r), timeline)
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, timeline))
}

// timelineRefRE matches a GitHub issue/PR reference: an optional owner/repo
// prefix then #<number>. Mirrors the closing-issue reference grammar used by
// the GraphQL closingIssuesReferences resolver.
var timelineRefRE = regexp.MustCompile(`(?:\b([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+))?#([0-9]+)`)

// mergeCrossReferencedEvents appends "cross-referenced" timeline events for
// every other issue or pull request in the repo whose body references this
// issue/PR by number (#N or owner/repo#N), then re-sorts by created_at. github.com
// threads these into the conversation timeline; bleephub derives them at read
// time from the referencing bodies (no persisted event), matching the
// timeline-cross-referenced-event schema (event/actor/created_at/updated_at/source).
func (s *Server) mergeCrossReferencedEvents(repo *store.Repo, targetNumber int, baseURL string, timeline []map[string]interface{}) []map[string]interface{} {
	refsTarget := func(body string) bool {
		for _, m := range timelineRefRE.FindAllStringSubmatch(body, -1) {
			// m[1] = optional owner/repo, m[2] = number
			if m[1] != "" && !strings.EqualFold(m[1], repo.FullName) {
				continue
			}
			if n, err := strconv.Atoi(m[2]); err == nil && n == targetNumber {
				return true
			}
		}
		return false
	}

	// Issue sources.
	for _, src := range s.store.ListIssues(repo.ID, "all") {
		if src.Number == targetNumber || !refsTarget(src.Body) {
			continue
		}
		timeline = append(timeline, crossRefEvent(
			s.store, baseURL, repo.FullName, src.AuthorID, src.CreatedAt, src.UpdatedAt,
			issueToJSON(src, s.store, baseURL, repo.FullName)))
	}
	// Pull request sources (share the number space; render as an issue-shaped source).
	for _, src := range s.store.ListPullRequests(repo.ID, "all") {
		if src.Number == targetNumber || !refsTarget(src.Body) {
			continue
		}
		timeline = append(timeline, crossRefEvent(
			s.store, baseURL, repo.FullName, src.AuthorID, src.CreatedAt, src.UpdatedAt,
			issueToJSONForPullRequest(src, s.store, baseURL, repo.FullName)))
	}

	sort.SliceStable(timeline, func(i, j int) bool {
		return timelineItemTime(timeline[i]) < timelineItemTime(timeline[j])
	})
	return timeline
}

// crossRefEvent builds one timeline-cross-referenced-event map.
func crossRefEvent(st *store.Store, baseURL, repoFullName string, actorID int, createdAt, updatedAt time.Time, sourceIssue map[string]interface{}) map[string]interface{} {
	var actor interface{}
	st.Mu.RLock()
	if u, ok := st.Users[actorID]; ok {
		actor = store.UserToJSON(u)
	}
	st.Mu.RUnlock()
	return map[string]interface{}{
		"event":      "cross-referenced",
		"actor":      actor,
		"created_at": createdAt.Format(time.RFC3339),
		"updated_at": updatedAt.Format(time.RFC3339),
		"source": map[string]interface{}{
			"type":  "issue",
			"issue": sourceIssue,
		},
	}
}

// timelineItemTime reads a timeline entry's sort timestamp (created_at, then
// submitted_at). RFC3339 strings sort chronologically as plain strings.
func timelineItemTime(m map[string]interface{}) string {
	if v, ok := m["created_at"].(string); ok && v != "" {
		return v
	}
	if v, ok := m["submitted_at"].(string); ok && v != "" {
		return v
	}
	return ""
}

func (s *Server) handleListIssueEvents(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	numStr := r.PathValue("number")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// Pull requests share the issue number space; their events serve
	// through this endpoint too, as on real GitHub.
	var events []*store.IssueEvent
	if issue := s.store.GetIssueByNumber(repo.ID, num); issue != nil {
		events = s.store.ListIssueEvents(repo.ID, issue.ID)
	} else if pr := s.store.GetPullRequestByNumber(repo.ID, num); pr != nil {
		events = s.store.ListPullRequestEvents(repo.ID, pr.ID)
	} else {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(events))
	for _, e := range events {
		result = append(result, issueEventForIssueToJSON(e, s.store, base, repo.FullName))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleListRepoIssueEvents(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	events := s.store.ListRepoIssueEvents(repo.ID)
	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(events))
	for _, e := range events {
		result = append(result, issueEventToJSON(e, s.store, base, repo.FullName))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleGetIssueEvent(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	idStr := r.PathValue("event_id")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if repo.Private && !s.viewerCanReadRepo(r.Context(), repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	id, err := strconv.Atoi(idStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	event := s.store.GetIssueEvent(id)
	if event == nil || event.RepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	writeJSON(w, http.StatusOK, issueEventToJSON(event, s.store, s.baseURL(r), repo.FullName))
}

// --- JSON converters ---

func issueToJSON(issue *store.Issue, st *store.Store, baseURL, repoFullName string) map[string]interface{} {
	// Every mutable field of *issue is read under the store read lock and
	// captured into locals here: UpdateIssue / SetIssueOrPRLock mutate these
	// fields under st.mu.Lock, so reading them after RUnlock (title, body,
	// state, lock flags, timestamps) would race a concurrent writer.
	var authorJSON map[string]interface{}
	st.Mu.RLock()
	if u, ok := st.Users[issue.AuthorID]; ok {
		authorJSON = store.UserToJSON(u)
	}

	// Resolve labels
	labels := make([]map[string]interface{}, 0)
	for _, lid := range issue.LabelIDs {
		if l, ok := st.Labels[lid]; ok {
			labels = append(labels, issueLabelToJSON(l, baseURL, repoFullName))
		}
	}

	// Resolve assignees
	assignees := make([]map[string]interface{}, 0)
	for _, aid := range issue.AssigneeIDs {
		if u, ok := st.Users[aid]; ok {
			assignees = append(assignees, store.UserToJSON(u))
		}
	}

	// Grab the milestone pointer; conversion happens after unlock because
	// milestoneToJSON derives issue counts under its own lock.
	var milestone *store.Milestone
	if issue.MilestoneID > 0 {
		milestone = st.Milestones[issue.MilestoneID]
	}
	repo := st.Repos[issue.RepoID]
	var issueType *store.IssueType
	if storedType := st.IssueTypeForIssueLocked(issue); storedType != nil {
		copied := *storedType
		issueType = &copied
	}
	// Count comments via the maintained index while holding the lock.
	commentCount := st.CountCommentsForLocked("issue", issue.ID)

	// Snapshot the mutable scalar fields before releasing the lock.
	issueID := issue.ID
	repoID := issue.RepoID
	authorID := issue.AuthorID
	issueNumber := issue.Number
	nodeID := issue.NodeID
	title := issue.Title
	body := issue.Body
	rawState := issue.State
	// StateReason is stored in the GraphQL upper-case form (COMPLETED,
	// NOT_PLANNED, REOPENED); the REST enum is lower-case (PAR-010).
	stateReason := strings.ToLower(issue.StateReason)
	locked := issue.Locked
	activeLockReason := issue.ActiveLockReason
	createdAt := issue.CreatedAt
	updatedAt := issue.UpdatedAt
	var closedAtCopy *time.Time
	if issue.ClosedAt != nil {
		c := *issue.ClosedAt
		closedAtCopy = &c
	}
	st.Mu.RUnlock()

	reactions := st.Reactions.SummarizeReactions("issue", issueID)
	reactions["url"] = baseURL + "/api/v3/repos/" + repoFullName + "/issues/" + strconv.Itoa(issueNumber) + "/reactions"

	var milestoneJSON interface{}
	if milestone != nil {
		milestoneJSON = milestoneToJSON(milestone, st, baseURL, repoFullName)
	}

	// GitHub's assignee is the first assignee, null when unassigned.
	var assignee interface{}
	if len(assignees) > 0 {
		assignee = assignees[0]
	}

	// REST uses lowercase state
	state := strings.ToLower(rawState)

	var closedAt interface{}
	if closedAtCopy != nil {
		closedAt = closedAtCopy.Format(time.RFC3339)
	}

	var activeLockReasonJSON interface{}
	if locked {
		activeLockReasonJSON = activeLockReason
	}

	numStr := strconv.Itoa(issueNumber)
	api := baseURL + "/api/v3/repos/" + repoFullName + "/issues/" + numStr
	subIssueIDs := st.ListSubIssues(issueID)
	completedSubIssues := 0
	for _, childID := range subIssueIDs {
		if child := st.GetIssue(childID); child != nil && child.State == "CLOSED" {
			completedSubIssues++
		}
	}
	percentComplete := 0
	if len(subIssueIDs) > 0 {
		percentComplete = completedSubIssues * 100 / len(subIssueIDs)
	}

	out := map[string]interface{}{
		"id":                 issueID,
		"node_id":            nodeID,
		"url":                api,
		"html_url":           baseURL + "/" + repoFullName + "/issues/" + numStr,
		"repository_url":     baseURL + "/api/v3/repos/" + repoFullName,
		"comments_url":       api + "/comments",
		"events_url":         api + "/events",
		"timeline_url":       api + "/timeline",
		"labels_url":         api + "/labels{/name}",
		"number":             issueNumber,
		"title":              title,
		"body":               body,
		"state":              state,
		"state_reason":       nullIfEmpty(stateReason),
		"user":               authorJSON,
		"labels":             labels,
		"assignee":           assignee,
		"assignees":          assignees,
		"milestone":          milestoneJSON,
		"locked":             locked,
		"active_lock_reason": activeLockReasonJSON,
		"comments":           commentCount,
		"created_at":         createdAt.Format(time.RFC3339),
		"updated_at":         updatedAt.Format(time.RFC3339),
		"closed_at":          closedAt,
		"closed_by":          issueClosedByJSON(st, repoID, issueID, rawState),
		"author_association": store.AuthorAssociation(st, authorID, repo),
		"draft":              false,
		"sub_issues_summary": map[string]interface{}{
			"total":             len(subIssueIDs),
			"completed":         completedSubIssues,
			"percent_completed": percentComplete,
		},
		"reactions": reactions,
	}
	if issueType != nil {
		out["type"] = issueTypeJSON(issueType)
	}
	return out
}

func issueClosedByJSON(st *store.Store, repoID, issueID int, state string) interface{} {
	if state != "CLOSED" {
		return nil
	}
	events := st.ListIssueEvents(repoID, issueID)
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Event != "closed" {
			continue
		}
		st.Mu.RLock()
		actor := store.ActorUserLocked(st, events[i].ActorID)
		var out interface{}
		if actor != nil {
			out = store.UserToJSON(actor)
		}
		st.Mu.RUnlock()
		return out
	}
	return nil
}

// issueEventToJSON renders an IssueEvent to the repo-level GitHub
// issue-event shape.
func issueEventToJSON(e *store.IssueEvent, st *store.Store, baseURL, repoFullName string) map[string]interface{} {
	st.Mu.RLock()
	var labelJSON interface{}
	if l, ok := st.Labels[e.LabelID]; ok {
		labelJSON = store.IssueEventLabelToJSON(l)
	}
	var assigneeJSON interface{}
	if u, ok := st.Users[e.AssigneeID]; ok {
		assigneeJSON = store.UserToJSON(u)
	}
	var assignerJSON interface{}
	if u, ok := st.Users[e.AssignerID]; ok {
		assignerJSON = store.UserToJSON(u)
	}
	var milestoneJSON interface{}
	if ms, ok := st.Milestones[e.MilestoneID]; ok {
		milestoneJSON = store.IssueEventMilestoneToJSON(ms)
	}
	st.Mu.RUnlock()

	out := store.IssueEventBase(e, st, baseURL, repoFullName)
	out["performed_via_github_app"] = nil
	// label and milestone are optional, non-nullable on the generic issue-event
	// schema: present only on the events that carry them, omitted otherwise.
	if labelJSON != nil {
		out["label"] = labelJSON
	}
	out["assignee"] = assigneeJSON
	out["assigner"] = assignerJSON
	if milestoneJSON != nil {
		out["milestone"] = milestoneJSON
	}
	return out
}

// issueEventForIssueToJSON renders an IssueEvent to the per-issue
// issue-event-for-issue shape, which is a discriminated union of specific
// event schemas rather than a generic object.
func issueEventForIssueToJSON(e *store.IssueEvent, st *store.Store, baseURL, repoFullName string) map[string]interface{} {
	out := store.IssueEventBase(e, st, baseURL, repoFullName)
	out["performed_via_github_app"] = nil

	switch e.Event {
	case "labeled", "unlabeled":
		st.Mu.RLock()
		var labelJSON interface{}
		if l, ok := st.Labels[e.LabelID]; ok {
			labelJSON = store.IssueEventLabelToJSON(l)
		}
		st.Mu.RUnlock()
		out["label"] = labelJSON
	case "assigned", "unassigned":
		st.Mu.RLock()
		var assigneeJSON, assignerJSON interface{}
		if u, ok := st.Users[e.AssigneeID]; ok {
			assigneeJSON = store.UserToJSON(u)
		}
		if u, ok := st.Users[e.AssignerID]; ok {
			assignerJSON = store.UserToJSON(u)
		}
		st.Mu.RUnlock()
		out["assignee"] = assigneeJSON
		out["assigner"] = assignerJSON
	case "milestoned", "demilestoned":
		st.Mu.RLock()
		var milestoneJSON interface{}
		if ms, ok := st.Milestones[e.MilestoneID]; ok {
			milestoneJSON = store.IssueEventMilestoneToJSON(ms)
		}
		st.Mu.RUnlock()
		out["milestone"] = milestoneJSON
	case "renamed":
		out["rename"] = map[string]interface{}{
			"from": e.RenameFrom,
			"to":   e.RenameTo,
		}
	case "review_requested", "review_request_removed":
		st.Mu.RLock()
		var requesterJSON, reviewerJSON interface{}
		if u, ok := st.Users[e.ActorID]; ok {
			requesterJSON = store.UserToJSON(u)
		}
		if u, ok := st.Users[e.RequestedReviewerID]; ok {
			reviewerJSON = store.UserToJSON(u)
		}
		st.Mu.RUnlock()
		// GitHub's actor on review-request events is the requester.
		out["review_requester"] = requesterJSON
		out["requested_reviewer"] = reviewerJSON
	default:
		// opened, closed, reopened, merged, locked, unlocked, etc. map to
		// the locked-issue-event schema which only adds a nullable
		// lock_reason.
		lockReason := interface{}(nil)
		if e.Event == "locked" && e.LockReason != "" {
			lockReason = e.LockReason
		}
		out["lock_reason"] = lockReason
	}
	return out
}

// issueHasAllLabels / labelIDsCoverNames moved to internal/store
// (ARCH-003): pure label predicates shared by REST and GraphQL.

// handleListOrgIssues implements GET /orgs/{org}/issues: issues across the
// organization's repositories that involve the authenticated user, selected
// by the `filter` parameter exactly as on real GitHub (assigned is the
// default; `repos`/`all` widen to every org-repo issue the user can see).
func (s *Server) handleListOrgIssues(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	q := r.URL.Query()
	filter := q.Get("filter")
	if filter == "" {
		filter = "assigned"
	}
	stateFilter := q.Get("state")
	if stateFilter == "" {
		stateFilter = "open"
	}
	var since time.Time
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			store.WriteGHValidationError(w, "Issue", "since", "invalid")
			return
		}
		since = t
	}
	var labelNames []string
	if v := q.Get("labels"); v != "" {
		labelNames = strings.Split(v, ",")
	}

	// Gather the org's issues under the read lock, render outside it.
	s.store.Mu.RLock()
	orgRepos := map[int]*store.Repo{}
	for _, repo := range s.store.Repos {
		if repo.OwnerType == "Organization" && repo.OwnerID == org.ID {
			orgRepos[repo.ID] = repo
		}
	}
	type issueRow struct {
		issue *store.Issue
		repo  *store.Repo
	}
	commentedIssueIDs := map[int]bool{}
	for _, c := range s.store.Comments {
		if c.ParentType == "issue" && c.AuthorID == user.ID {
			commentedIssueIDs[c.IssueID] = true
		}
	}
	var rows []issueRow
	for _, issue := range s.store.Issues {
		repo := orgRepos[issue.RepoID]
		if repo == nil {
			continue
		}
		switch stateFilter {
		case "open":
			if issue.State != "OPEN" {
				continue
			}
		case "closed":
			if issue.State != "CLOSED" {
				continue
			}
		}
		assigned := false
		for _, aid := range issue.AssigneeIDs {
			if aid == user.ID {
				assigned = true
				break
			}
		}
		switch filter {
		case "assigned":
			if !assigned {
				continue
			}
		case "created":
			if issue.AuthorID != user.ID {
				continue
			}
		case "mentioned":
			if !strings.Contains(issue.Body, "@"+user.Login) {
				continue
			}
		case "subscribed":
			// Participation auto-subscribes on real GitHub: authored,
			// assigned, or commented issues.
			if issue.AuthorID != user.ID && !assigned && !commentedIssueIDs[issue.ID] {
				continue
			}
		case "repos", "all":
			// Every issue across the org's repositories.
		default:
			continue
		}
		if !since.IsZero() && issue.UpdatedAt.Before(since) {
			continue
		}
		rows = append(rows, issueRow{issue: issue, repo: repo})
	}
	s.store.Mu.RUnlock()

	if len(labelNames) > 0 {
		kept := rows[:0]
		for _, row := range rows {
			if store.IssueHasAllLabels(s.store, row.issue, labelNames, row.repo.ID) {
				kept = append(kept, row)
			}
		}
		rows = kept
	}
	// Private repos the caller cannot read never surface.
	readable := rows[:0]
	for _, row := range rows {
		if s.viewerCanReadRepo(r.Context(), row.repo) {
			readable = append(readable, row)
		}
	}
	rows = readable

	sortKey := q.Get("sort")
	asc := q.Get("direction") == "asc"
	sort.SliceStable(rows, func(i, j int) bool {
		var before bool
		switch sortKey {
		case "updated":
			before = rows[i].issue.UpdatedAt.Before(rows[j].issue.UpdatedAt)
		case "comments":
			before = rows[i].issue.ID < rows[j].issue.ID
		default: // created
			before = rows[i].issue.CreatedAt.Before(rows[j].issue.CreatedAt)
		}
		if asc {
			return before
		}
		return !before
	})

	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		issueJSON := issueToJSON(row.issue, s.store, base, row.repo.FullName)
		issueJSON["repository"] = store.RepoToJSON(row.repo, s.store, base)
		out = append(out, issueJSON)
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

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

// Issue handlers

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
	if s.rejectIfArchived(w, repo) {
		return
	}
	if s.rejectIfInteractionLimited(w, user, repo) {
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

	labelIDs, ok := s.resolveAssignableLabelNames(w, repo.ID, req.Labels)
	if !ok {
		return
	}

	var assigneeIDs []int
	for _, login := range req.Assignees {
		u := s.store.LookupUserByLogin(login)
		if u != nil {
			assigneeIDs = append(assigneeIDs, u.ID)
		}
	}

	var milestoneID int
	if req.Milestone > 0 {
		ms := s.store.GetMilestoneByNumber(repo.ID, req.Milestone)
		if ms == nil {
			// GitHub 422s an unknown milestone on create, as the PATCH path does;
			// dropping it silently and returning 201 with milestone:null diverges.
			store.WriteGHValidationError(w, "Issue", "milestone", "invalid")
			return
		}
		milestoneID = ms.ID
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
	s.emitWebhookEvent(repoKey, "issues", "opened", buildIssuesPayload(s.store, repo, issue, user, "opened", s.baseURL(r)))

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
			// An unknown assignee is a valid filter with no matches (not "no
			// filter", which would widen to every issue).
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
			// A valid but absent milestone number selects an empty set, not
			// "no filter".
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

	// Every PR is also an issue on GitHub; this listing returns both, with the
	// `pull_request` member telling them apart.
	base := s.baseURL(r)
	descending := direction == "desc"
	type issueRow struct {
		number       int
		createdAt    time.Time
		updatedAt    time.Time
		commentCount int
		issue        *store.Issue       // set for issue rows
		pr           *store.PullRequest // set for pull-request rows
	}
	// rowLess is the total order: the requested sort key, tie-broken by the
	// per-repo number (unique across issues and PRs), inverted for desc.
	rowLess := func(a, b issueRow) bool {
		var less bool
		switch sortField {
		case "updated":
			less = a.updatedAt.Before(b.updatedAt)
			if a.updatedAt.Equal(b.updatedAt) {
				less = a.number < b.number
			}
		case "comments":
			less = a.commentCount < b.commentCount
			if a.commentCount == b.commentCount {
				less = a.number < b.number
			}
		default:
			less = a.createdAt.Before(b.createdAt)
			if a.createdAt.Equal(b.createdAt) {
				less = a.number < b.number
			}
		}
		if descending {
			return !less && a.number != b.number
		}
		return less
	}
	// Issues arrive from the per-repo creation-order index already sorted (both
	// directions); PR rows come from a map and are ordered below. JSON is
	// rendered after pagination, for the one page returned, not every issue.
	var issueRows []issueRow
	for _, issue := range s.store.ListIssuesOrderedByCreation(repo.ID, stateFilter, descending) {
		if !selected(issue.LabelIDs, issue.AssigneeIDs) ||
			!matchesCommon(issue.AuthorID, issue.MilestoneID, issue.UpdatedAt, issue.Body, "issue", issue.ID) {
			continue
		}
		issueRows = append(issueRows, issueRow{
			number: issue.Number, createdAt: issue.CreatedAt, updatedAt: issue.UpdatedAt,
			commentCount: s.store.CountCommentsFor("issue", issue.ID),
			issue:        issue,
		})
	}
	var prRows []issueRow
	for _, pr := range s.store.ListPullRequests(repo.ID, stateFilter) {
		if !selected(pr.LabelIDs, pr.AssigneeIDs) ||
			!matchesCommon(pr.AuthorID, pr.MilestoneID, pr.UpdatedAt, pr.Body, "pull_request", pr.ID) {
			continue
		}
		prRows = append(prRows, issueRow{
			number: pr.Number, createdAt: pr.CreatedAt, updatedAt: pr.UpdatedAt,
			commentCount: s.store.CountCommentsFor("pull_request", pr.ID),
			pr:           pr,
		})
	}

	var rows []issueRow
	if sortField == "created" {
		// issueRows are pre-sorted; order the (usually fewer) PR rows and merge.
		sort.Slice(prRows, func(i, j int) bool { return rowLess(prRows[i], prRows[j]) })
		rows = make([]issueRow, 0, len(issueRows)+len(prRows))
		for len(issueRows) > 0 && len(prRows) > 0 {
			if rowLess(prRows[0], issueRows[0]) {
				rows = append(rows, prRows[0])
				prRows = prRows[1:]
			} else {
				rows = append(rows, issueRows[0])
				issueRows = issueRows[1:]
			}
		}
		rows = append(rows, issueRows...)
		rows = append(rows, prRows...)
	} else {
		rows = append(issueRows, prRows...)
		sort.Slice(rows, func(i, j int) bool { return rowLess(rows[i], rows[j]) })
	}

	pageRows := paginateAndLink(w, r, rows)
	result := make([]map[string]interface{}, 0, len(pageRows))
	for _, row := range pageRows {
		if row.issue != nil {
			result = append(result, issueToJSON(row.issue, s.store, base, repo.FullName))
		} else {
			result = append(result, issueToJSONForPR(row.pr, s.store, base, repo.FullName))
		}
	}
	writeJSON(w, http.StatusOK, result)
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
		// PRs share the issue number space and are reachable through the issues
		// endpoint.
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

	// If-Match must match the current ETag or a stale client clobbers a
	// concurrent edit (STORE-016).
	if !checkIfMatch(w, r, issueToJSON(issue, s.store, s.baseURL(r), repo.FullName)) {
		return
	}

	var req map[string]interface{}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// Resolve milestone, labels, and assignees before the write lock so an
	// invalid milestone number 422s without mutating. Explicit
	// `"milestone": null` clears it; an absent member keeps it.
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
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			// The body allows bare strings or {"name": ...} objects; unknown
			// label names are dropped, as the add-labels endpoint does.
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
			names = append(names, name)
		}
		ids, ok := s.resolveAssignableLabelNames(w, repo.ID, names)
		if !ok {
			return
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

	// GitHub restricts state_reason to a fixed set (null clears it); anything
	// else is a 422 rather than being stored verbatim.
	if v, ok := req["state_reason"].(string); ok && v != "" {
		switch strings.ToLower(v) {
		case "completed", "not_planned", "reopened":
		default:
			store.WriteGHValidationError(w, "Issue", "state_reason", "invalid")
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
				// GitHub stamps state_reason "reopened" on reopen, not null.
				i.StateReason = "REOPENED"
			}
		}
		if v, ok := req["state_reason"].(string); ok {
			i.StateReason = strings.ToUpper(v)
		}
	})

	updated := s.store.GetIssue(issue.ID)

	// One PATCH fans out to several events (edit, triage changes, state
	// transition). `issue` is the pre-update snapshot the diffs run against.
	change := store.SubjectChange{
		LabelsFrom:    issue.LabelIDs,
		LabelsTo:      labelIDs,
		AssigneesFrom: issue.AssigneeIDs,
		AssigneesTo:   assigneeIDs,
		MilestoneFrom: issue.MilestoneID,
		MilestoneTo:   milestoneID,
		StateFrom:     previousState,
		StateTo:       updated.State,
	}
	if v, ok := req["title"].(string); ok && v != issue.Title {
		change.TitleFrom = &issue.Title
	}
	if v, ok := req["body"].(string); ok && v != issue.Body {
		change.BodyFrom = &issue.Body
	}

	// Record the same timeline events the dedicated sub-resource endpoints do,
	// diffing against the requested new sets.
	if labelIDs != nil {
		added, removed := intSetDelta(issue.LabelIDs, *labelIDs)
		for _, id := range added {
			s.store.RecordIssueEvent(repo.ID, issue.ID, user.ID, "labeled", map[string]interface{}{"label_id": id})
		}
		for _, id := range removed {
			s.store.RecordIssueEvent(repo.ID, issue.ID, user.ID, "unlabeled", map[string]interface{}{"label_id": id})
		}
	}
	if assigneeIDs != nil {
		added, removed := intSetDelta(issue.AssigneeIDs, *assigneeIDs)
		for _, id := range added {
			s.store.RecordIssueEvent(repo.ID, issue.ID, user.ID, "assigned", map[string]interface{}{"assignee_id": id, "assigner_id": user.ID})
		}
		for _, id := range removed {
			s.store.RecordIssueEvent(repo.ID, issue.ID, user.ID, "unassigned", map[string]interface{}{"assignee_id": id, "assigner_id": user.ID})
		}
	}
	if milestoneID != nil && *milestoneID != issue.MilestoneID {
		if *milestoneID != 0 {
			s.store.RecordIssueEvent(repo.ID, issue.ID, user.ID, "milestoned", map[string]interface{}{"milestone_id": *milestoneID})
		} else {
			s.store.RecordIssueEvent(repo.ID, issue.ID, user.ID, "demilestoned", map[string]interface{}{"milestone_id": issue.MilestoneID})
		}
	}

	// A title edit is the `renamed` issue event, carrying old and new titles.
	if v, ok := req["title"].(string); ok && v != issue.Title {
		s.store.RecordIssueEvent(repo.ID, issue.ID, user.ID, "renamed", map[string]interface{}{
			"rename_from": issue.Title,
			"rename_to":   v,
		})
	}

	if v, ok := req["state"].(string); ok {
		if v == "closed" && previousState != "CLOSED" {
			s.store.RecordIssueEvent(repo.ID, issue.ID, user.ID, "closed", nil)
		} else if v == "open" && previousState != "OPEN" {
			s.store.RecordIssueEvent(repo.ID, issue.ID, user.ID, "reopened", nil)
		}
	}

	s.issueEmitter(repo, updated, user).emitChanges(change)

	writeJSON(w, http.StatusOK, issueToJSON(updated, s.store, s.baseURL(r), repo.FullName))
}

// Comment handlers

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
	if s.rejectIfArchived(w, repo) {
		return
	}
	if s.rejectIfInteractionLimited(w, user, repo) {
		return
	}

	num, err := strconv.Atoi(numStr)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// Resolves to either an Issue or a PR by number (PRs are issues here). The
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

// Issue label management handlers

// labelIDsToJSON renders label IDs as label JSON in stored order, skipping any
// that no longer resolve.
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

	newLabelIDs, ok := s.resolveAssignableLabelNames(w, repo.ID, labelNames)
	if !ok {
		return
	}

	base := s.baseURL(r)
	if pr != nil {
		if !s.store.AddPullRequestLabels(repo.ID, pr.Number, newLabelIDs, user.ID) {
			store.WriteGHValidationError(w, "Label", "labels", "archived")
			return
		}
		updated := s.store.GetPullRequestByNumber(repo.ID, pr.Number)
		s.pullRequestEmitter(repo, updated, user).emitLabelDelta(pr.LabelIDs, updated.LabelIDs)
		writeJSON(w, http.StatusOK, s.labelIDsToJSON(updated.LabelIDs, base, repo.FullName))
		return
	}

	// SetIssueLabels (not a raw mutator) records the `labeled` event per
	// addition that the timeline and events surfaces read.
	next := append([]int(nil), issue.LabelIDs...)
	for _, lid := range newLabelIDs {
		found := false
		for _, existing := range next {
			if existing == lid {
				found = true
				break
			}
		}
		if !found {
			next = append(next, lid)
		}
	}
	if !s.store.SetIssueLabels(repo.ID, issue.Number, next, user.ID) {
		store.WriteGHValidationError(w, "Label", "labels", "archived")
		return
	}

	updated := s.store.GetIssue(issue.ID)
	s.issueEmitter(repo, updated, user).emitLabelDelta(issue.LabelIDs, updated.LabelIDs)
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
		updated := s.store.GetPullRequestByNumber(repo.ID, pr.Number)
		s.pullRequestEmitter(repo, updated, user).emitLabelDelta(pr.LabelIDs, updated.LabelIDs)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// SetIssueLabels so the removal leaves an `unlabeled` event behind.
	next := make([]int, 0, len(issue.LabelIDs))
	for _, lid := range issue.LabelIDs {
		if lid != label.ID {
			next = append(next, lid)
		}
	}
	s.store.SetIssueLabels(repo.ID, issue.Number, next, user.ID)

	updated := s.store.GetIssue(issue.ID)
	s.issueEmitter(repo, updated, user).emitLabelDelta(issue.LabelIDs, updated.LabelIDs)
	w.WriteHeader(http.StatusNoContent)
}

// Repo-level comment handlers

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

// Issue label set/clear handlers

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

	labelIDs, ok := s.resolveAssignableLabelNames(w, repo.ID, labelNames)
	if !ok {
		return
	}

	base := s.baseURL(r)
	if pr != nil {
		if !s.store.SetPullRequestLabels(repo.ID, pr.Number, labelIDs, user.ID) {
			store.WriteGHValidationError(w, "Label", "labels", "archived")
			return
		}
		updated := s.store.GetPullRequestByNumber(repo.ID, pr.Number)
		s.pullRequestEmitter(repo, updated, user).emitLabelDelta(pr.LabelIDs, updated.LabelIDs)
		writeJSON(w, http.StatusOK, s.labelIDsToJSON(updated.LabelIDs, base, repo.FullName))
		return
	}

	if !s.store.SetIssueLabels(repo.ID, issue.Number, labelIDs, user.ID) {
		store.WriteGHValidationError(w, "Label", "labels", "archived")
		return
	}

	updated := s.store.GetIssue(issue.ID)
	s.issueEmitter(repo, updated, user).emitLabelDelta(issue.LabelIDs, updated.LabelIDs)
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
			updated := s.store.GetPullRequestByNumber(repo.ID, pr.Number)
			s.pullRequestEmitter(repo, updated, user).emitLabelDelta(pr.LabelIDs, updated.LabelIDs)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	s.store.ClearIssueLabels(repo.ID, issue.Number, user.ID)
	updated := s.store.GetIssue(issue.ID)
	s.issueEmitter(repo, updated, user).emitLabelDelta(issue.LabelIDs, updated.LabelIDs)
	w.WriteHeader(http.StatusNoContent)
}

// Issue assignee handlers

func (s *Server) handleAddIssueAssignees(w http.ResponseWriter, r *http.Request) {
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
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

	issue := s.store.GetIssueByNumber(repo.ID, num)
	if issue == nil {
		// PRs share the issue number space and are assignable through the issues
		// endpoint; the sibling label handler already falls back this way.
		pr := s.store.GetPullRequestByNumber(repo.ID, num)
		if pr == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		s.store.AddPullRequestAssignees(repo.ID, pr.Number, assigneeIDs, user.ID)
		updated := s.store.GetPullRequestByNumber(repo.ID, pr.Number)
		s.pullRequestEmitter(repo, updated, user).emitAssigneeDelta(pr.AssigneeIDs, updated.AssigneeIDs)
		prJSON := issueToJSONForPR(updated, s.store, s.baseURL(r), repo.FullName)
		writeJSONCreated(w, jsonStringField(prJSON, "url"), prJSON)
		return
	}

	s.store.AddIssueAssignees(repo.ID, issue.Number, assigneeIDs, user.ID)
	updated := s.store.GetIssue(issue.ID)
	s.issueEmitter(repo, updated, user).emitAssigneeDelta(issue.AssigneeIDs, updated.AssigneeIDs)
	// Adding assignees is a 201.
	issueJSON := issueToJSON(updated, s.store, s.baseURL(r), repo.FullName)
	writeJSONCreated(w, jsonStringField(issueJSON, "url"), issueJSON)
}

func (s *Server) handleRemoveIssueAssignees(w http.ResponseWriter, r *http.Request) {
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
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

	issue := s.store.GetIssueByNumber(repo.ID, num)
	if issue == nil {
		pr := s.store.GetPullRequestByNumber(repo.ID, num)
		if pr == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		s.store.RemovePullRequestAssignees(repo.ID, pr.Number, assigneeIDs, user.ID)
		updated := s.store.GetPullRequestByNumber(repo.ID, pr.Number)
		s.pullRequestEmitter(repo, updated, user).emitAssigneeDelta(pr.AssigneeIDs, updated.AssigneeIDs)
		writeJSON(w, http.StatusOK, issueToJSONForPR(updated, s.store, s.baseURL(r), repo.FullName))
		return
	}

	s.store.RemoveIssueAssignees(repo.ID, issue.Number, assigneeIDs, user.ID)
	updated := s.store.GetIssue(issue.ID)
	s.issueEmitter(repo, updated, user).emitAssigneeDelta(issue.AssigneeIDs, updated.AssigneeIDs)
	writeJSON(w, http.StatusOK, issueToJSON(updated, s.store, s.baseURL(r), repo.FullName))
}

// Comment pin handlers

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

// resolveRepoIssueComment resolves owner/repo/{comment_id}, writing the error
// response on failure.
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

// Issue timeline + events handlers

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

	// PRs share the issue number space and are timeline-capable; resolve to
	// whichever exists.
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

// timelineRefRE matches an issue/PR reference: optional owner/repo prefix then
// #<number>.
var timelineRefRE = regexp.MustCompile(`(?:\b([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+))?#([0-9]+)`)

// mergeCrossReferencedEvents appends a "cross-referenced" event for every other
// issue or PR in the repo whose body references this one (#N or owner/repo#N),
// then re-sorts by created_at. Derived at read time from the referencing
// bodies; no persisted event.
func (s *Server) mergeCrossReferencedEvents(repo *store.Repo, targetNumber int, baseURL string, timeline []map[string]interface{}) []map[string]interface{} {
	refsTarget := func(body string) bool {
		for _, m := range timelineRefRE.FindAllStringSubmatch(body, -1) {
			// m[1] = optional owner/repo, m[2] = number.
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
	// PR sources (render as an issue-shaped source).
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
		actor = store.UserToJSON(u, baseURL)
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
// submitted_at); RFC3339 strings sort chronologically as plain strings.
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

	// PRs share the issue number space; their events serve through here too.
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
		// The issue-events surface excludes comments (they live on the comments
		// endpoint; a "commented" row's id is not a comment id).
		if e.Event == "commented" {
			continue
		}
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
		if e.Event == "commented" {
			continue
		}
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

// JSON converters

func issueToJSON(issue *store.Issue, st *store.Store, baseURL, repoFullName string) map[string]interface{} {
	// Read every mutable field into locals under the read lock: UpdateIssue /
	// SetIssueOrPRLock write these under st.mu.Lock, so reading them after
	// RUnlock would race a concurrent writer.
	var authorJSON map[string]interface{}
	st.Mu.RLock()
	if u, ok := st.Users[issue.AuthorID]; ok {
		authorJSON = store.UserToJSON(u, baseURL)
	}

	labels := make([]map[string]interface{}, 0)
	for _, lid := range issue.LabelIDs {
		if l, ok := st.Labels[lid]; ok {
			labels = append(labels, issueLabelToJSON(l, baseURL, repoFullName))
		}
	}

	assignees := make([]map[string]interface{}, 0)
	for _, aid := range issue.AssigneeIDs {
		if u, ok := st.Users[aid]; ok {
			assignees = append(assignees, store.UserToJSON(u, baseURL))
		}
	}

	// Convert the milestone after unlock; milestoneToJSON derives counts under
	// its own lock.
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
	commentCount := st.CountCommentsForLocked("issue", issue.ID)

	// Snapshot mutable scalars before releasing the lock.
	issueID := issue.ID
	repoID := issue.RepoID
	authorID := issue.AuthorID
	issueNumber := issue.Number
	nodeID := issue.NodeID
	title := issue.Title
	body := issue.Body
	rawState := issue.State
	// StateReason is stored upper-case (GraphQL form); REST is lower-case (PAR-010).
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

	// assignee is the first assignee, null when unassigned.
	var assignee interface{}
	if len(assignees) > 0 {
		assignee = assignees[0]
	}

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
		"closed_by":          issueClosedByJSON(st, repoID, issueID, rawState, baseURL),
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

func issueClosedByJSON(st *store.Store, repoID, issueID int, state, baseURL string) interface{} {
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
			out = store.UserToJSON(actor, baseURL)
		}
		st.Mu.RUnlock()
		return out
	}
	return nil
}

// issueEventToJSON renders an IssueEvent to the repo-level issue-event shape.
func issueEventToJSON(e *store.IssueEvent, st *store.Store, baseURL, repoFullName string) map[string]interface{} {
	st.Mu.RLock()
	var labelJSON interface{}
	if l, ok := st.Labels[e.LabelID]; ok {
		labelJSON = store.IssueEventLabelToJSON(l)
	}
	var assigneeJSON interface{}
	if u, ok := st.Users[e.AssigneeID]; ok {
		assigneeJSON = store.UserToJSON(u, baseURL)
	}
	var assignerJSON interface{}
	if u, ok := st.Users[e.AssignerID]; ok {
		assignerJSON = store.UserToJSON(u, baseURL)
	}
	var milestoneJSON interface{}
	if ms, ok := st.Milestones[e.MilestoneID]; ok {
		milestoneJSON = store.IssueEventMilestoneToJSON(ms)
	}
	st.Mu.RUnlock()

	out := store.IssueEventBase(e, st, baseURL, repoFullName)
	out["performed_via_github_app"] = nil
	// label and milestone are optional non-nullable: present only on events
	// that carry them.
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
// issue-event-for-issue shape, a discriminated union of per-event schemas.
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
			assigneeJSON = store.UserToJSON(u, baseURL)
		}
		if u, ok := st.Users[e.AssignerID]; ok {
			assignerJSON = store.UserToJSON(u, baseURL)
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
			requesterJSON = store.UserToJSON(u, baseURL)
		}
		if u, ok := st.Users[e.RequestedReviewerID]; ok {
			reviewerJSON = store.UserToJSON(u, baseURL)
		}
		st.Mu.RUnlock()
		// The actor on review-request events is the requester.
		out["review_requester"] = requesterJSON
		out["requested_reviewer"] = reviewerJSON
	default:
		// opened/closed/reopened/merged/locked/unlocked etc. map to the
		// locked-issue-event schema, which only adds a nullable lock_reason.
		lockReason := interface{}(nil)
		if e.Event == "locked" && e.LockReason != "" {
			lockReason = e.LockReason
		}
		out["lock_reason"] = lockReason
	}
	return out
}

// handleListOrgIssues lists org-repo issues involving the authenticated user,
// selected by `filter` (default assigned; `repos`/`all` widen to every org-repo
// issue the user can see).
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

	// Gather the org's issues and pull requests under the read lock, render
	// outside it. Every pull request is an issue on GitHub, so both appear here.
	s.store.Mu.RLock()
	orgRepos := map[int]*store.Repo{}
	for _, repo := range s.store.Repos {
		if repo.OwnerType == "Organization" && repo.OwnerID == org.ID {
			orgRepos[repo.ID] = repo
		}
	}
	commentedIssueIDs := map[int]bool{}
	commentedPRIDs := map[int]bool{}
	for _, c := range s.store.Comments {
		if c.AuthorID != user.ID {
			continue
		}
		switch c.ParentType {
		case "issue":
			commentedIssueIDs[c.IssueID] = true
		case "pull_request":
			commentedPRIDs[c.IssueID] = true
		}
	}
	// orgFilterMatches applies the `filter` values uniformly to an issue or a
	// pull request; `commented` is whether the caller has commented on it, which
	// counts as participation for `subscribed`.
	orgFilterMatches := func(authorID int, assigneeIDs []int, body string, commented bool) bool {
		assigned := false
		for _, aid := range assigneeIDs {
			if aid == user.ID {
				assigned = true
				break
			}
		}
		switch filter {
		case "assigned":
			return assigned
		case "created":
			return authorID == user.ID
		case "mentioned":
			return strings.Contains(body, "@"+user.Login)
		case "subscribed":
			// Participation (authored, assigned, or commented) auto-subscribes.
			return authorID == user.ID || assigned || commented
		case "repos", "all":
			return true
		default:
			return false
		}
	}
	var rows []crossRepoIssueRow
	for _, issue := range s.store.Issues {
		repo := orgRepos[issue.RepoID]
		if repo == nil {
			continue
		}
		if !issueMatchesStateFilter(issue.State, stateFilter) {
			continue
		}
		if !orgFilterMatches(issue.AuthorID, issue.AssigneeIDs, issue.Body, commentedIssueIDs[issue.ID]) {
			continue
		}
		if !since.IsZero() && issue.UpdatedAt.Before(since) {
			continue
		}
		if len(labelNames) > 0 && !labelIDsHaveNames(s.store, issue.LabelIDs, labelNames) {
			continue
		}
		rows = append(rows, crossRepoIssueRow{
			number: issue.Number, commentCount: s.store.CountCommentsForLocked("issue", issue.ID),
			issue: issue, repo: repo,
			createdAtVal: issue.CreatedAt, updatedAtVal: issue.UpdatedAt,
		})
	}
	for _, pr := range s.store.PullRequests {
		repo := orgRepos[pr.RepoID]
		if repo == nil {
			continue
		}
		if !prMatchesStateFilter(pr, stateFilter) {
			continue
		}
		if !orgFilterMatches(pr.AuthorID, pr.AssigneeIDs, pr.Body, commentedPRIDs[pr.ID]) {
			continue
		}
		if !since.IsZero() && pr.UpdatedAt.Before(since) {
			continue
		}
		if len(labelNames) > 0 && !labelIDsHaveNames(s.store, pr.LabelIDs, labelNames) {
			continue
		}
		rows = append(rows, crossRepoIssueRow{
			number: pr.Number, commentCount: s.store.CountCommentsForLocked("pull_request", pr.ID),
			pr: pr, repo: repo,
			createdAtVal: pr.CreatedAt, updatedAtVal: pr.UpdatedAt,
		})
	}
	s.store.Mu.RUnlock()

	// Private repos the caller cannot read never surface.
	readable := rows[:0]
	for _, row := range rows {
		if s.viewerCanReadRepo(r.Context(), row.repo) {
			readable = append(readable, row)
		}
	}
	rows = readable

	less := crossRepoRowLess(q.Get("sort"), q.Get("direction") == "asc")
	sort.SliceStable(rows, func(i, j int) bool { return less(rows[i], rows[j]) })

	s.renderCrossRepoIssueRows(w, r, s.baseURL(r), rows)
}

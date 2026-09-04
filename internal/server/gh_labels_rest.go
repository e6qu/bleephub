package bleephub

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHIssueRoutes() {
	// issues:write covers labels, as real GitHub conflates the two.
	s.route("POST /api/v3/repos/{owner}/{repo}/labels", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleCreateLabel))
	s.route("GET /api/v3/repos/{owner}/{repo}/labels", s.handleListLabels)
	s.route("GET /api/v3/repos/{owner}/{repo}/labels/{name}", s.handleGetLabel)
	s.route("PATCH /api/v3/repos/{owner}/{repo}/labels/{name}", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleUpdateLabel))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/labels/{name}", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleDeleteLabel))

	// Milestones
	s.route("POST /api/v3/repos/{owner}/{repo}/milestones", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleCreateMilestone))
	s.route("GET /api/v3/repos/{owner}/{repo}/milestones", s.handleListMilestones)
	s.route("GET /api/v3/repos/{owner}/{repo}/milestones/{number}/labels", s.handleListMilestoneLabels)
	s.route("GET /api/v3/repos/{owner}/{repo}/milestones/{number}", s.handleGetMilestone)
	s.route("PATCH /api/v3/repos/{owner}/{repo}/milestones/{number}", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleUpdateMilestone))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/milestones/{number}", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleDeleteMilestone))

	// Issues
	s.route("POST /api/v3/repos/{owner}/{repo}/issues", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleCreateIssue))
	s.route("GET /api/v3/repos/{owner}/{repo}/issues", s.handleListIssues)
	s.route("GET /api/v3/orgs/{org}/issues", s.handleListOrgIssues)
	s.route("GET /api/v3/repos/{owner}/{repo}/issues/{number}", s.handleGetIssue)
	s.route("PATCH /api/v3/repos/{owner}/{repo}/issues/{number}", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleUpdateIssue))

	// Go 1.22's mux can't disambiguate literal /issues/comments/{id} from
	// wildcard /issues/{n}/..., so all two-segment issue GETs dispatch here.
	s.route("GET /api/v3/repos/{owner}/{repo}/issues/{p1}/{p2}", s.handleIssuesTwoSegGetDispatch)

	s.route("POST /api/v3/repos/{owner}/{repo}/issues/{number}/comments", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleCreateIssueComment))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/issues/comments/{comment_id}", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleUpdateIssueComment))

	// Two-segment issue DELETEs (comment-by-id vs lock/unlock) collide in the
	// mux, so they dispatch through one handler.
	s.route("DELETE /api/v3/repos/{owner}/{repo}/issues/{p1}/{p2}", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleIssuesDeleteDispatch))
	s.route("PUT /api/v3/repos/{owner}/{repo}/issues/{number}/lock", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleLockIssue))

	// Issue label management
	s.route("POST /api/v3/repos/{owner}/{repo}/issues/{number}/labels", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleAddIssueLabels))
	s.route("PUT /api/v3/repos/{owner}/{repo}/issues/{number}/labels", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleSetIssueLabels))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/issues/{number}/labels", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleClearIssueLabels))

	// Issue comments (repo-level)
	s.route("GET /api/v3/repos/{owner}/{repo}/issues/comments", s.handleListRepoIssueComments)
	s.route("PUT /api/v3/repos/{owner}/{repo}/issues/comments/{comment_id}/pin", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handlePinIssueComment))

	// Issue assignees
	s.route("POST /api/v3/repos/{owner}/{repo}/issues/{number}/assignees", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleAddIssueAssignees))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/issues/{number}/assignees", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleRemoveIssueAssignees))

	// Issue timeline + events
	s.route("GET /api/v3/repos/{owner}/{repo}/issues/events", s.handleListRepoIssueEvents)

	// Sub-issues + issue dependencies (gh_sub_issues.go). List GETs and removals
	// dispatch through the shared wildcard handlers below.
	s.route("POST /api/v3/repos/{owner}/{repo}/issues/{number}/sub_issues", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleCreateSubIssue))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/issues/{number}/sub_issues/priority", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleReprioritizeSubIssue))
	s.route("POST /api/v3/repos/{owner}/{repo}/issues/{number}/dependencies/blocked_by", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleAddIssueDependencyBlockedBy))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/issues/{number}/dependencies/blocked_by/{issue_id}", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleRemoveIssueDependencyBlockedBy))

	// Three-segment issue DELETEs dispatch from one handler; the direct
	// labels/sub-issues routes above are more specific and take precedence.
	s.route("DELETE /api/v3/repos/{owner}/{repo}/issues/{p1}/{p2}/{p3}", s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleIssuesThreeSegDeleteDispatch))

	// Three-segment issue GETs likewise dispatch from one handler.
	s.route("GET /api/v3/repos/{owner}/{repo}/issues/{p1}/{p2}/{p3}", s.handleIssuesThreeSegGetDispatch)
}

// Label handlers

// buildLabelPayload assembles the `label` webhook event body.
func buildLabelPayload(repo *store.Repo, labelJSON map[string]interface{}, sender *store.User, action, baseURL string) map[string]interface{} {
	return map[string]interface{}{
		"action":     action,
		"label":      labelJSON,
		"repository": repoPayload(repo, baseURL),
		"sender":     store.UserToJSON(sender, baseURL),
	}
}

// buildMilestonePayload assembles the `milestone` webhook event body.
func buildMilestonePayload(repo *store.Repo, milestoneJSON map[string]interface{}, sender *store.User, action, baseURL string) map[string]interface{} {
	return map[string]interface{}{
		"action":     action,
		"milestone":  milestoneJSON,
		"repository": repoPayload(repo, baseURL),
		"sender":     store.UserToJSON(sender, baseURL),
	}
}

func (s *Server) handleCreateLabel(w http.ResponseWriter, r *http.Request) {
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
		Name        string `json:"name"`
		Color       string `json:"color"`
		Description string `json:"description"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name == "" {
		store.WriteGHValidationError(w, "Label", "name", "missing_field")
		return
	}

	label := s.store.CreateLabel(repo.ID, req.Name, req.Description, req.Color)
	if label == nil {
		store.WriteGHValidationError(w, "Label", "name", "already_exists")
		return
	}

	repoKey := owner + "/" + name
	s.recordAuditEvent("label.create", user.Login, "", map[string]interface{}{"repo": repoKey, "label_id": label.ID, "name": label.Name})
	labelJSON := issueLabelToJSON(label, s.baseURL(r), repo.FullName)
	s.emitWebhookEvent(repoKey, "label", "created", buildLabelPayload(repo, labelJSON, user, "created", s.baseURL(r)))
	writeJSONCreated(w, jsonStringField(labelJSON, "url"), labelJSON)
}

func (s *Server) handleListLabels(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	labels := s.store.ListLabels(repo.ID)
	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(labels))
	for _, l := range labels {
		result = append(result, issueLabelToJSON(l, base, repo.FullName))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleGetLabel(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	labelName := r.PathValue("name")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	label := s.store.GetLabelByName(repo.ID, labelName)
	if label == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	writeJSON(w, http.StatusOK, issueLabelToJSON(label, s.baseURL(r), repo.FullName))
}

func (s *Server) handleUpdateLabel(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	labelName := r.PathValue("name")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	label := s.store.GetLabelByName(repo.ID, labelName)
	if label == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// Reject a stale If-Match with 412 (STORE-016).
	if !checkIfMatch(w, r, issueLabelToJSON(label, s.baseURL(r), repo.FullName)) {
		return
	}

	var req struct {
		NewName     *string `json:"new_name"`
		Color       *string `json:"color"`
		Description *string `json:"description"`
		Archived    *bool   `json:"archived"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// Renaming onto a name another label in the repo already holds is refused, the
	// same invariant the create path enforces; without this the repo ended up with
	// two identically-named labels and label-by-name resolution became ambiguous.
	if req.NewName != nil && *req.NewName != label.Name {
		if existing := s.store.GetLabelByName(repo.ID, *req.NewName); existing != nil && existing.ID != label.ID {
			store.WriteGHValidationError(w, "Label", "name", "already_exists")
			return
		}
	}

	s.store.UpdateLabel(label.ID, func(l *store.IssueLabel) {
		if req.NewName != nil {
			l.Name = *req.NewName
		}
		if req.Color != nil {
			l.Color = *req.Color
		}
		if req.Description != nil {
			l.Description = *req.Description
		}
		if req.Archived != nil {
			l.Archived = *req.Archived
		}
	})

	updated := s.store.GetLabel(label.ID)
	updatedJSON := issueLabelToJSON(updated, s.baseURL(r), repo.FullName)
	s.emitWebhookEvent(owner+"/"+repoName, "label", "edited", buildLabelPayload(repo, updatedJSON, user, "edited", s.baseURL(r)))
	writeJSON(w, http.StatusOK, updatedJSON)
}

func (s *Server) handleDeleteLabel(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	labelName := r.PathValue("name")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	label := s.store.GetLabelByName(repo.ID, labelName)
	if label == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	repoKey := owner + "/" + repoName
	labelJSON := issueLabelToJSON(label, s.baseURL(r), repo.FullName)
	s.store.DeleteLabel(label.ID)
	s.recordAuditEvent("label.delete", user.Login, "", map[string]interface{}{"repo": repoKey, "label_id": label.ID})
	s.emitWebhookEvent(repoKey, "label", "deleted", buildLabelPayload(repo, labelJSON, user, "deleted", s.baseURL(r)))
	w.WriteHeader(http.StatusNoContent)
}

// Milestone handlers

func (s *Server) handleCreateMilestone(w http.ResponseWriter, r *http.Request) {
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
		Title       string `json:"title"`
		Description string `json:"description"`
		State       string `json:"state"`
		DueOn       string `json:"due_on"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Title == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}

	var dueOn *time.Time
	if req.DueOn != "" {
		t, err := time.Parse(time.RFC3339, req.DueOn)
		if err == nil {
			dueOn = &t
		}
	}

	ms := s.store.CreateMilestone(repo.ID, user.ID, req.Title, req.Description, req.State, dueOn)
	if ms == nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}

	repoKey := owner + "/" + name
	s.recordAuditEvent("milestone.create", user.Login, "", map[string]interface{}{"repo": repoKey, "milestone_id": ms.ID, "title": ms.Title})
	msJSON := milestoneToJSON(ms, s.store, s.baseURL(r), repo.FullName)
	s.emitWebhookEvent(repoKey, "milestone", "created", buildMilestonePayload(repo, msJSON, user, "created", s.baseURL(r)))
	writeJSONCreated(w, jsonStringField(msJSON, "url"), msJSON)
}

func (s *Server) handleListMilestones(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}

	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}

	milestones := s.store.ListMilestones(repo.ID, state)
	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(milestones))
	for _, ms := range milestones {
		result = append(result, milestoneToJSON(ms, s.store, base, repo.FullName))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleGetMilestone(w http.ResponseWriter, r *http.Request) {
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

	ms := s.store.GetMilestoneByNumber(repo.ID, num)
	if ms == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	writeJSON(w, http.StatusOK, milestoneToJSON(ms, s.store, s.baseURL(r), repo.FullName))
}

func (s *Server) handleUpdateMilestone(w http.ResponseWriter, r *http.Request) {
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

	ms := s.store.GetMilestoneByNumber(repo.ID, num)
	if ms == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// Reject a stale If-Match with 412 (STORE-016).
	if !checkIfMatch(w, r, milestoneToJSON(ms, s.store, s.baseURL(r), repo.FullName)) {
		return
	}
	oldState := string(ms.State)

	var req map[string]interface{}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	s.store.UpdateMilestone(ms.ID, func(m *store.Milestone) {
		if v, ok := req["title"].(string); ok {
			m.Title = v
		}
		if v, ok := req["description"].(string); ok {
			m.Description = v
		}
		if v, ok := req["state"].(string); ok {
			if v == "closed" && m.State != "closed" {
				now := time.Now().UTC()
				m.ClosedAt = &now
			} else if v == "open" {
				m.ClosedAt = nil
			}
			m.State = store.MilestoneState(v)
		}
		// GitHub's milestone update accepts due_on (and a null clears it); the
		// handler previously ignored it, so a due date could never be set or
		// changed after creation. Parse leniently, matching the create path.
		if raw, present := req["due_on"]; present {
			switch v := raw.(type) {
			case string:
				if v == "" {
					m.DueOn = nil
				} else if t, err := time.Parse(time.RFC3339, v); err == nil {
					m.DueOn = &t
				}
			case nil:
				m.DueOn = nil
			}
		}
	})

	updated := s.store.GetMilestone(ms.ID)
	msJSON := milestoneToJSON(updated, s.store, s.baseURL(r), repo.FullName)
	// Closing fires `closed`, reopening `opened`, any other change `edited`.
	action := "edited"
	if newState := string(updated.State); newState != oldState {
		if newState == "closed" {
			action = "closed"
		} else if newState == "open" {
			action = "opened"
		}
	}
	s.emitWebhookEvent(owner+"/"+repoName, "milestone", action, buildMilestonePayload(repo, msJSON, user, action, s.baseURL(r)))
	writeJSON(w, http.StatusOK, msJSON)
}

func (s *Server) handleDeleteMilestone(w http.ResponseWriter, r *http.Request) {
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

	ms := s.store.GetMilestoneByNumber(repo.ID, num)
	if ms == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	repoKey := owner + "/" + repoName
	// Snapshot before deletion so the webhook payload still carries it.
	msJSON := milestoneToJSON(ms, s.store, s.baseURL(r), repo.FullName)
	s.store.DeleteMilestone(ms.ID)
	s.recordAuditEvent("milestone.delete", user.Login, "", map[string]interface{}{"repo": repoKey, "milestone_id": ms.ID})
	s.emitWebhookEvent(repoKey, "milestone", "deleted", buildMilestonePayload(repo, msJSON, user, "deleted", s.baseURL(r)))
	w.WriteHeader(http.StatusNoContent)
}

// handleListMilestoneLabels lists each distinct label on any issue or PR in the
// milestone (PRs are issues).
func (s *Server) handleListMilestoneLabels(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	num, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	ms := s.store.GetMilestoneByNumber(repo.ID, num)
	if ms == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	s.store.Mu.RLock()
	seen := map[int]bool{}
	var labels []*store.IssueLabel
	collect := func(labelIDs []int) {
		for _, lid := range labelIDs {
			if seen[lid] {
				continue
			}
			if l, ok := s.store.Labels[lid]; ok {
				seen[lid] = true
				labels = append(labels, l)
			}
		}
	}
	for _, issue := range s.store.Issues {
		if issue.MilestoneID == ms.ID {
			collect(issue.LabelIDs)
		}
	}
	for _, pr := range s.store.PullRequests {
		if pr.MilestoneID == ms.ID {
			collect(pr.LabelIDs)
		}
	}
	s.store.Mu.RUnlock()

	sort.Slice(labels, func(i, j int) bool { return labels[i].ID < labels[j].ID })
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(labels))
	for _, l := range labels {
		out = append(out, issueLabelToJSON(l, base, repo.FullName))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

// JSON converters

func issueLabelToJSON(l *store.IssueLabel, baseURL, repoFullName string) map[string]interface{} {
	return map[string]interface{}{
		"id":          l.ID,
		"node_id":     l.NodeID,
		"url":         baseURL + "/api/v3/repos/" + repoFullName + "/labels/" + l.Name,
		"name":        l.Name,
		"description": l.Description,
		"color":       l.Color,
		"default":     l.Default,
	}
}

func (s *Server) resolveAssignableLabelNames(w http.ResponseWriter, repoID int, names []string) ([]int, bool) {
	ids := make([]int, 0, len(names))
	for _, name := range names {
		label := s.store.GetLabelByName(repoID, name)
		if label == nil {
			continue
		}
		if label.Archived {
			store.WriteGHValidationError(w, "Label", "labels", "archived")
			return nil, false
		}
		ids = append(ids, label.ID)
	}
	return ids, true
}

// milestoneToJSON renders the `milestone` shape, deriving open/closed issue
// counts live from attached issues and PRs (PRs are issues). Must not be called
// with st.mu held.
func milestoneToJSON(ms *store.Milestone, st *store.Store, baseURL, repoFullName string) map[string]interface{} {
	var dueOn interface{}
	if ms.DueOn != nil {
		dueOn = ms.DueOn.Format(time.RFC3339)
	}
	var closedAt interface{}
	if ms.ClosedAt != nil {
		closedAt = ms.ClosedAt.Format(time.RFC3339)
	}

	st.Mu.RLock()
	var creatorJSON interface{}
	if u, ok := st.Users[ms.CreatorID]; ok {
		creatorJSON = store.UserToJSON(u, baseURL)
	}
	// A milestone belongs to one repository, so its issues/PRs all live in that
	// repo — scan only the per-repo indexes, not the global cross-repo maps
	// (which made listing N milestones O(N × all-issues-in-the-instance)).
	openIssues, closedIssues := 0, 0
	for _, issue := range st.IssuesByRepo[ms.RepoID] {
		if issue.MilestoneID != ms.ID {
			continue
		}
		if issue.State == "OPEN" {
			openIssues++
		} else {
			closedIssues++
		}
	}
	for _, pr := range st.PullsByRepo[ms.RepoID] {
		if pr.MilestoneID != ms.ID {
			continue
		}
		if pr.State == "OPEN" {
			openIssues++
		} else {
			closedIssues++
		}
	}
	st.Mu.RUnlock()

	return map[string]interface{}{
		"id":            ms.ID,
		"node_id":       ms.NodeID,
		"url":           baseURL + "/api/v3/repos/" + repoFullName + "/milestones/" + strconv.Itoa(ms.Number),
		"html_url":      baseURL + "/" + repoFullName + "/milestone/" + strconv.Itoa(ms.Number),
		"labels_url":    baseURL + "/api/v3/repos/" + repoFullName + "/milestones/" + strconv.Itoa(ms.Number) + "/labels",
		"number":        ms.Number,
		"title":         ms.Title,
		"description":   ms.Description,
		"state":         ms.State,
		"creator":       creatorJSON,
		"open_issues":   openIssues,
		"closed_issues": closedIssues,
		"due_on":        dueOn,
		"closed_at":     closedAt,
		"created_at":    ms.CreatedAt.Format(time.RFC3339),
		"updated_at":    ms.UpdatedAt.Format(time.RFC3339),
	}
}

// handleIssuesTwoSegGetDispatch routes GET /issues/{p1}/{p2} to the comment/
// event lookups or the per-issue sub-resources the mux cannot separate.
func (s *Server) handleIssuesTwoSegGetDispatch(w http.ResponseWriter, r *http.Request) {
	p1 := r.PathValue("p1")
	p2 := r.PathValue("p2")
	switch {
	case p1 == "comments":
		r.SetPathValue("comment_id", p2)
		s.handleGetIssueComment(w, r)
	case p1 == "events":
		r.SetPathValue("event_id", p2)
		s.handleGetIssueEvent(w, r)
	case p2 == "comments":
		r.SetPathValue("number", p1)
		s.handleListIssueComments(w, r)
	case p2 == "timeline":
		r.SetPathValue("number", p1)
		s.handleListIssueTimeline(w, r)
	case p2 == "events":
		r.SetPathValue("number", p1)
		s.handleListIssueEvents(w, r)
	case p2 == "reactions":
		r.SetPathValue("number", p1)
		s.handleListReactions("issue", "number")(w, r)
	case p2 == "labels":
		r.SetPathValue("number", p1)
		s.handleListIssueLabels(w, r)
	case p2 == "parent":
		r.SetPathValue("number", p1)
		s.handleGetIssueParent(w, r)
	case p2 == "sub_issues" || p2 == "sub_issue":
		r.SetPathValue("number", p1)
		s.handleListSubIssues(w, r)
	case p2 == "issue-field-values":
		r.SetPathValue("number", p1)
		s.handleListIssueFieldValues(w, r)
	default:
		writeGHError(w, http.StatusNotFound, "Not Found")
	}
}

// handleIssuesThreeSegDeleteDispatch routes DELETE /issues/{p1}/{p2}/{p3} to the
// correct handler where the mux cannot separate literal from wildcard segments.
func (s *Server) handleIssuesThreeSegDeleteDispatch(w http.ResponseWriter, r *http.Request) {
	p1 := r.PathValue("p1")
	p2 := r.PathValue("p2")
	p3 := r.PathValue("p3")
	switch {
	case p1 == "comments" && p3 == "pin":
		r.SetPathValue("comment_id", p2)
		s.handleUnpinIssueComment(w, r)
	case p2 == "reactions":
		r.SetPathValue("number", p1)
		r.SetPathValue("reaction_id", p3)
		s.handleDeleteReaction("issue", "number")(w, r)
	case p2 == "labels":
		r.SetPathValue("number", p1)
		r.SetPathValue("name", p3)
		s.handleRemoveIssueLabel(w, r)
	case p2 == "issue-field-values":
		r.SetPathValue("number", p1)
		r.SetPathValue("issue_field_id", p3)
		s.handleDeleteIssueFieldValue(w, r)
	default:
		writeGHError(w, http.StatusNotFound, "Not Found")
	}
}

// handleIssuesThreeSegGetDispatch routes GET /issues/{p1}/{p2}/{p3} to the
// correct handler where the mux cannot separate literal from wildcard segments.
func (s *Server) handleIssuesThreeSegGetDispatch(w http.ResponseWriter, r *http.Request) {
	p1 := r.PathValue("p1")
	p2 := r.PathValue("p2")
	p3 := r.PathValue("p3")
	switch {
	case p1 == "comments" && p3 == "reactions":
		r.SetPathValue("comment_id", p2)
		s.handleListReactions("issue_comment", "comment_id")(w, r)
	case p2 == "dependencies" && p3 == "blocked_by":
		r.SetPathValue("number", p1)
		s.handleListIssueDependenciesBlockedBy(w, r)
	case p2 == "dependencies" && p3 == "blocking":
		r.SetPathValue("number", p1)
		s.handleListIssueDependenciesBlocking(w, r)
	case p2 == "assignees":
		r.SetPathValue("number", p1)
		r.SetPathValue("assignee", p3)
		s.handleCheckIssueAssignee(w, r)
	default:
		writeGHError(w, http.StatusNotFound, "Not Found")
	}
}

func (s *Server) handleListIssueLabels(w http.ResponseWriter, r *http.Request) {
	repo, issue := s.issueFromNumberPath(w, r)
	if issue == nil {
		return
	}
	s.store.Mu.RLock()
	labels := make([]*store.IssueLabel, 0, len(issue.LabelIDs))
	for _, id := range issue.LabelIDs {
		if label := s.store.Labels[id]; label != nil {
			labels = append(labels, label)
		}
	}
	s.store.Mu.RUnlock()
	out := make([]map[string]interface{}, 0, len(labels))
	for _, label := range labels {
		out = append(out, issueLabelToJSON(label, s.baseURL(r), repo.FullName))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleCheckIssueAssignee(w http.ResponseWriter, r *http.Request) {
	_, issue := s.issueFromNumberPath(w, r)
	if issue == nil {
		return
	}
	user := s.store.LookupUserByLogin(r.PathValue("assignee"))
	if user == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	for _, id := range issue.AssigneeIDs {
		if id == user.ID {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	writeGHError(w, http.StatusNotFound, "Not Found")
}

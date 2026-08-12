package bleephub

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHPullExtensionRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/interaction-limits/pulls/creation-cap",
		s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleGetPRCreationCap))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/interaction-limits/pulls/creation-cap",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleUpdatePRCreationCap))
	s.route("GET /api/v3/orgs/{org}/interaction-limits/pulls/creation-cap",
		s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.handleGetOrgPRCreationCap))
	s.route("PATCH /api/v3/orgs/{org}/interaction-limits/pulls/creation-cap",
		s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleUpdateOrgPRCreationCap))
	s.route("GET /api/v3/repos/{owner}/{repo}/interaction-limits/pulls/bypass-list",
		s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleGetPRCreationBypass))
	s.route("PUT /api/v3/repos/{owner}/{repo}/interaction-limits/pulls/bypass-list",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleAddPRCreationBypass))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/interaction-limits/pulls/bypass-list",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleRemovePRCreationBypass))

	s.route("GET /api/v3/repos/{owner}/{repo}/stacks",
		s.requirePerm(store.ScopePullRequests, store.PermRead, s.handleListPullRequestStacks))
	s.route("POST /api/v3/repos/{owner}/{repo}/stacks",
		s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handleCreatePullRequestStack))
	s.route("GET /api/v3/repos/{owner}/{repo}/stacks/{stack_number}",
		s.requirePerm(store.ScopePullRequests, store.PermRead, s.handleGetPullRequestStack))
	s.route("POST /api/v3/repos/{owner}/{repo}/stacks/{stack_number}/add",
		s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handleAddPullRequestsToStack))
	s.route("POST /api/v3/repos/{owner}/{repo}/stacks/{stack_number}/unstack",
		s.requirePerm(store.ScopePullRequests, store.PermWrite, s.handleUnstackPullRequests))

	s.route("GET /api/v3/repos/{owner}/{repo}/issues/{issue_number}/suggestions",
		s.requirePerm(store.ScopeIssues, store.PermRead, s.handleListIssueSuggestions))
	s.route("POST /api/v3/repos/{owner}/{repo}/issues/{issue_number}/suggestions/{suggestion_id}/approve",
		s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleApproveIssueSuggestion))
	s.route("POST /api/v3/repos/{owner}/{repo}/issues/{issue_number}/suggestions/{suggestion_id}/dismiss",
		s.requirePerm(store.ScopeIssues, store.PermWrite, s.handleDismissIssueSuggestion))
}

func (s *Server) requireRepoAdminForPullExtension(w http.ResponseWriter, r *http.Request) *store.Repo {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return nil
	}
	if !s.viewerCanAdminRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have admin rights to Repository.")
		return nil
	}
	return repo
}

func (s *Server) handleGetPRCreationCap(w http.ResponseWriter, r *http.Request) {
	repo := s.requireRepoAdminForPullExtension(w, r)
	if repo != nil {
		writeJSON(w, http.StatusOK, s.store.GetPRCreationCap(repo.FullName))
	}
}

func (s *Server) handleUpdatePRCreationCap(w http.ResponseWriter, r *http.Request) {
	repo := s.requireRepoAdminForPullExtension(w, r)
	if repo == nil {
		return
	}
	var request struct {
		Enabled             *bool `json:"enabled"`
		MaxOpenPullRequests *int  `json:"max_open_pull_requests"`
	}
	if !decodeJSONBody(w, r, &request) {
		return
	}
	if request.Enabled == nil {
		store.WriteGHValidationError(w, "PullRequestCreationCap", "enabled", "missing_field")
		return
	}
	current := s.store.GetPRCreationCap(repo.FullName)
	current.Enabled = *request.Enabled
	if request.MaxOpenPullRequests != nil {
		if *request.MaxOpenPullRequests < 1 || *request.MaxOpenPullRequests > 1000 {
			store.WriteGHValidationError(w, "PullRequestCreationCap", "max_open_pull_requests", "invalid")
			return
		}
		current.MaxOpenPullRequests = *request.MaxOpenPullRequests
	}
	writeJSON(w, http.StatusOK, s.store.SetPRCreationCap(repo.FullName, current))
}

func (s *Server) handleGetOrgPRCreationCap(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	writeJSON(w, http.StatusOK, s.store.GetOrgPRCreationCap(org.Login))
}

func (s *Server) handleUpdateOrgPRCreationCap(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	var request struct {
		Enabled             *bool `json:"enabled"`
		MaxOpenPullRequests *int  `json:"max_open_pull_requests"`
	}
	if !decodeJSONBody(w, r, &request) {
		return
	}
	if request.Enabled == nil {
		store.WriteGHValidationError(w, "PullRequestCreationCap", "enabled", "missing_field")
		return
	}
	current := s.store.GetOrgPRCreationCap(org.Login)
	current.Enabled = *request.Enabled
	if request.MaxOpenPullRequests != nil {
		if *request.MaxOpenPullRequests < 1 || *request.MaxOpenPullRequests > 1000 {
			store.WriteGHValidationError(w, "PullRequestCreationCap", "max_open_pull_requests", "invalid")
			return
		}
		current.MaxOpenPullRequests = *request.MaxOpenPullRequests
	}
	writeJSON(w, http.StatusOK, s.store.SetOrgPRCreationCap(org.Login, current))
}

func (s *Server) handleGetPRCreationBypass(w http.ResponseWriter, r *http.Request) {
	repo := s.requireRepoAdminForPullExtension(w, r)
	if repo == nil {
		return
	}
	users := s.store.PRCreationBypassUsers(repo.FullName)
	out := make([]map[string]interface{}, 0, len(users))
	for _, user := range paginateAndLink(w, r, users) {
		out = append(out, store.UserToJSON(user))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) changePRCreationBypass(w http.ResponseWriter, r *http.Request, add bool) {
	repo := s.requireRepoAdminForPullExtension(w, r)
	if repo == nil {
		return
	}
	var request struct {
		Users []string `json:"users"`
	}
	if !decodeJSONBody(w, r, &request) {
		return
	}
	if len(request.Users) == 0 {
		store.WriteGHValidationError(w, "PullRequestCreationCapBypass", "users", "missing_field")
		return
	}
	for _, login := range request.Users {
		if s.store.LookupUserByLogin(login) == nil {
			store.WriteGHValidationError(w, "PullRequestCreationCapBypass", "users", "invalid")
			return
		}
	}
	s.store.ChangePRCreationBypass(repo.FullName, request.Users, add)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAddPRCreationBypass(w http.ResponseWriter, r *http.Request) {
	s.changePRCreationBypass(w, r, true)
}
func (s *Server) handleRemovePRCreationBypass(w http.ResponseWriter, r *http.Request) {
	s.changePRCreationBypass(w, r, false)
}

func (s *Server) resolveStackPulls(w http.ResponseWriter, repo *store.Repo, numbers []int, minimum int) []*store.PullRequest {
	if len(numbers) < minimum || len(numbers) > 100 {
		store.WriteGHValidationError(w, "PullRequestStack", "pull_requests", "invalid")
		return nil
	}
	pulls := make([]*store.PullRequest, 0, len(numbers))
	for _, number := range numbers {
		pull := s.store.GetPullRequestByNumber(repo.ID, number)
		if pull == nil {
			store.WriteGHValidationError(w, "PullRequestStack", "pull_requests", "invalid")
			return nil
		}
		pulls = append(pulls, pull)
	}
	for index := 1; index < len(pulls); index++ {
		if pulls[index].BaseRefName != pulls[index-1].HeadRefName {
			store.WriteGHValidationError(w, "PullRequestStack", "pull_requests", "invalid_stack_order")
			return nil
		}
	}
	return pulls
}

func (s *Server) pullRequestStackJSON(r *http.Request, repo *store.Repo, stack *store.PullRequestStack, minimal bool) map[string]interface{} {
	pulls := make([]map[string]interface{}, 0, len(stack.PullRequests))
	open := false
	for _, number := range stack.PullRequests {
		if pull := s.store.GetPullRequestByNumber(repo.ID, number); pull != nil {
			if pull.State == "OPEN" {
				open = true
			}
			full := pullRequestToJSON(pull, s.store, s.baseURL(r), repo.FullName)
			if minimal {
				pulls = append(pulls, map[string]interface{}{
					"number":    full["number"],
					"state":     full["state"],
					"draft":     full["draft"],
					"merged_at": full["merged_at"],
					"head": map[string]interface{}{
						"ref": full["head"].(map[string]interface{})["ref"],
						"sha": full["head"].(map[string]interface{})["sha"],
					},
				})
			} else {
				minimalRepo := func(value interface{}) interface{} {
					repoJSON, ok := value.(map[string]interface{})
					if !ok || repoJSON == nil {
						return nil
					}
					return map[string]interface{}{
						"id": repoJSON["id"], "url": repoJSON["url"], "name": repoJSON["name"],
					}
				}
				compactRef := func(value interface{}) map[string]interface{} {
					ref := value.(map[string]interface{})
					return map[string]interface{}{
						"ref": ref["ref"], "sha": ref["sha"], "repo": minimalRepo(ref["repo"]),
					}
				}
				pulls = append(pulls, map[string]interface{}{
					"id":        full["id"],
					"number":    full["number"],
					"node_id":   full["node_id"],
					"url":       full["url"],
					"html_url":  full["html_url"],
					"title":     full["title"],
					"state":     full["state"],
					"merged_at": full["merged_at"],
					"draft":     full["draft"],
					"user":      full["user"],
					"head":      compactRef(full["head"]),
					"base":      compactRef(full["base"]),
				})
			}
		}
	}
	return map[string]interface{}{
		"id": stack.ID, "number": stack.Number,
		"node_id": fmt.Sprintf("PRS_kwDO%08d", stack.ID),
		"url":     fmt.Sprintf("%s/api/v3/repos/%s/stacks/%d", s.baseURL(r), repo.FullName, stack.Number),
		"base":    map[string]interface{}{"ref": stack.BaseRef},
		"open":    open, "created_at": stack.CreatedAt.UTC().Format(time.RFC3339),
		"pull_requests": pulls,
	}
}

func (s *Server) handleListPullRequestStacks(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	stacks := paginateAndLink(w, r, s.store.ListPullRequestStacks(repo.ID))
	out := make([]map[string]interface{}, 0, len(stacks))
	for _, stack := range stacks {
		out = append(out, s.pullRequestStackJSON(r, repo, stack, true))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreatePullRequestStack(w http.ResponseWriter, r *http.Request) {
	repo := s.requireRepoAdminForPullExtension(w, r)
	if repo == nil {
		return
	}
	var request struct {
		PullRequests []int `json:"pull_requests"`
	}
	if !decodeJSONBody(w, r, &request) {
		return
	}
	pulls := s.resolveStackPulls(w, repo, request.PullRequests, 2)
	if pulls == nil {
		return
	}
	stack, err := s.store.CreatePullRequestStack(repo, pulls)
	if err != nil {
		writeGHError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.pullRequestStackJSON(r, repo, stack, false))
}

func (s *Server) stackFromPath(w http.ResponseWriter, r *http.Request, admin bool) (*store.Repo, *store.PullRequestStack) {
	var repo *store.Repo
	if admin {
		repo = s.requireRepoAdminForPullExtension(w, r)
	} else {
		repo = s.lookupReadableRepoFromPath(w, r)
	}
	if repo == nil {
		return nil, nil
	}
	number, err := strconv.Atoi(r.PathValue("stack_number"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	stack := s.store.GetPullRequestStack(repo.FullName, number)
	if stack == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	return repo, stack
}

func (s *Server) handleGetPullRequestStack(w http.ResponseWriter, r *http.Request) {
	repo, stack := s.stackFromPath(w, r, false)
	if stack != nil {
		writeJSON(w, http.StatusOK, s.pullRequestStackJSON(r, repo, stack, false))
	}
}

func (s *Server) handleAddPullRequestsToStack(w http.ResponseWriter, r *http.Request) {
	repo, stack := s.stackFromPath(w, r, true)
	if stack == nil {
		return
	}
	var request struct {
		PullRequests []int `json:"pull_requests"`
	}
	if !decodeJSONBody(w, r, &request) {
		return
	}
	pulls := s.resolveStackPulls(w, repo, request.PullRequests, 1)
	if pulls == nil {
		return
	}
	top := s.store.GetPullRequestByNumber(repo.ID, stack.PullRequests[len(stack.PullRequests)-1])
	if top == nil || pulls[0].BaseRefName != top.HeadRefName {
		store.WriteGHValidationError(w, "PullRequestStack", "pull_requests", "invalid_stack_order")
		return
	}
	updated, err := s.store.AddPullRequestsToStack(repo.FullName, stack.Number, pulls)
	if err != nil {
		writeGHError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.pullRequestStackJSON(r, repo, updated, false))
}

func (s *Server) handleUnstackPullRequests(w http.ResponseWriter, r *http.Request) {
	repo, stack := s.stackFromPath(w, r, true)
	if stack == nil {
		return
	}
	if !s.store.DeletePullRequestStack(repo.FullName, stack.Number) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) issueForSuggestionPath(w http.ResponseWriter, r *http.Request, write bool) (*store.Repo, *store.Issue) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return nil, nil
	}
	if write && !s.viewerCanPushRepo(r.Context(), repo) {
		writeGHError(w, http.StatusForbidden, "Must have push access to Repository.")
		return nil, nil
	}
	number, err := strconv.Atoi(r.PathValue("issue_number"))
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

func (s *Server) handleListIssueSuggestions(w http.ResponseWriter, r *http.Request) {
	repo, issue := s.issueForSuggestionPath(w, r, false)
	if issue == nil {
		return
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, s.store.ListIssueSuggestions(repo.FullName, issue.ID)))
}

func (s *Server) resolveIssueSuggestion(w http.ResponseWriter, r *http.Request, approve bool) {
	repo, issue := s.issueForSuggestionPath(w, r, true)
	if issue == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("suggestion_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var target *store.IssueSuggestion
	for _, suggestion := range s.store.ListIssueSuggestions(repo.FullName, issue.ID) {
		if suggestion.ID == id {
			target = suggestion
			break
		}
	}
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if target.State != "pending" {
		store.WriteGHValidationError(w, "IssueSuggestion", "state", "invalid")
		return
	}
	user := ghUserFromContext(r.Context())
	state := "dismissed"
	var eventID *int
	if approve {
		state = "approved"
		switch target.Action {
		case "close_issue":
			s.store.UpdateIssue(issue.ID, func(item *store.Issue) {
				item.State = "CLOSED"
				now := time.Now().UTC()
				item.ClosedAt = &now
			})
		case "add_label":
			if target.TargetID == nil || s.store.GetLabel(*target.TargetID) == nil {
				store.WriteGHValidationError(w, "IssueSuggestion", "target_id", "invalid")
				return
			}
			s.store.AddIssueLabels(repo.FullName, issue.Number, []int{*target.TargetID})
		case "add_assignee":
			if target.TargetID == nil || s.store.GetUserByID(*target.TargetID) == nil {
				store.WriteGHValidationError(w, "IssueSuggestion", "target_id", "invalid")
				return
			}
			s.store.AddIssueAssignees(repo.ID, issue.Number, []int{*target.TargetID}, user.ID)
		case "set_type":
			if target.TargetID == nil || s.store.GetAssignableIssueTypeForRepo(repo, *target.TargetID) == nil {
				store.WriteGHValidationError(w, "IssueSuggestion", "target_id", "invalid")
				return
			}
			s.store.UpdateIssue(issue.ID, func(item *store.Issue) { item.IssueTypeID = *target.TargetID })
		case "add_field":
			if target.TargetID == nil {
				store.WriteGHValidationError(w, "IssueSuggestion", "target_id", "invalid")
				return
			}
			s.store.AddIssueFieldValues(issue.ID, map[int]interface{}{*target.TargetID: target.TargetValue})
		default:
			store.WriteGHValidationError(w, "IssueSuggestion", "action", "invalid")
			return
		}
		event := s.store.RecordIssueEvent(repo.ID, issue.ID, user.ID, "issue_suggestion_approved", map[string]interface{}{})
		eventID = &event.ID
	}
	resolved := s.store.ResolveIssueSuggestion(repo.FullName, issue.ID, id, user.ID, state, eventID)
	if resolved == nil {
		store.WriteGHValidationError(w, "IssueSuggestion", "state", "invalid")
		return
	}
	writeJSON(w, http.StatusOK, resolved)
}

func (s *Server) handleApproveIssueSuggestion(w http.ResponseWriter, r *http.Request) {
	s.resolveIssueSuggestion(w, r, true)
}
func (s *Server) handleDismissIssueSuggestion(w http.ResponseWriter, r *http.Request) {
	s.resolveIssueSuggestion(w, r, false)
}

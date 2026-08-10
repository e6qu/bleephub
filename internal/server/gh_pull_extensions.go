package bleephub

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"
)

type PRCreationCap struct {
	Enabled             bool `json:"enabled"`
	MaxOpenPullRequests int  `json:"max_open_pull_requests"`
}

type PullRequestStack struct {
	ID           int       `json:"id"`
	Number       int       `json:"number"`
	RepoID       int       `json:"repo_id"`
	BaseRef      string    `json:"base_ref"`
	PullRequests []int     `json:"pull_requests"`
	CreatedAt    time.Time `json:"created_at"`
}

type IssueSuggestion struct {
	ID           int         `json:"id"`
	IssueID      int         `json:"issue_id"`
	Action       string      `json:"action"`
	State        string      `json:"state"`
	TargetID     *int        `json:"target_id"`
	TargetValue  interface{} `json:"target_value"`
	Rationale    *string     `json:"rationale"`
	Confidence   *string     `json:"confidence"`
	ActorID      *int        `json:"actor_id"`
	IssueEventID *int        `json:"issue_event_id"`
	ResolvedBy   *int        `json:"resolved_by"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
}

func (s *Server) registerGHPullExtensionRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/interaction-limits/pulls/creation-cap",
		s.requirePerm(scopeAdministration, permRead, s.handleGetPRCreationCap))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/interaction-limits/pulls/creation-cap",
		s.requirePerm(scopeAdministration, permWrite, s.handleUpdatePRCreationCap))
	s.route("GET /api/v3/orgs/{org}/interaction-limits/pulls/creation-cap",
		s.requirePerm(scopeOrgAdministration, permRead, s.handleGetOrgPRCreationCap))
	s.route("PATCH /api/v3/orgs/{org}/interaction-limits/pulls/creation-cap",
		s.requirePerm(scopeOrgAdministration, permWrite, s.handleUpdateOrgPRCreationCap))
	s.route("GET /api/v3/repos/{owner}/{repo}/interaction-limits/pulls/bypass-list",
		s.requirePerm(scopeAdministration, permRead, s.handleGetPRCreationBypass))
	s.route("PUT /api/v3/repos/{owner}/{repo}/interaction-limits/pulls/bypass-list",
		s.requirePerm(scopeAdministration, permWrite, s.handleAddPRCreationBypass))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/interaction-limits/pulls/bypass-list",
		s.requirePerm(scopeAdministration, permWrite, s.handleRemovePRCreationBypass))

	s.route("GET /api/v3/repos/{owner}/{repo}/stacks",
		s.requirePerm(scopePullRequests, permRead, s.handleListPullRequestStacks))
	s.route("POST /api/v3/repos/{owner}/{repo}/stacks",
		s.requirePerm(scopePullRequests, permWrite, s.handleCreatePullRequestStack))
	s.route("GET /api/v3/repos/{owner}/{repo}/stacks/{stack_number}",
		s.requirePerm(scopePullRequests, permRead, s.handleGetPullRequestStack))
	s.route("POST /api/v3/repos/{owner}/{repo}/stacks/{stack_number}/add",
		s.requirePerm(scopePullRequests, permWrite, s.handleAddPullRequestsToStack))
	s.route("POST /api/v3/repos/{owner}/{repo}/stacks/{stack_number}/unstack",
		s.requirePerm(scopePullRequests, permWrite, s.handleUnstackPullRequests))

	s.route("GET /api/v3/repos/{owner}/{repo}/issues/{issue_number}/suggestions",
		s.requirePerm(scopeIssues, permRead, s.handleListIssueSuggestions))
	s.route("POST /api/v3/repos/{owner}/{repo}/issues/{issue_number}/suggestions/{suggestion_id}/approve",
		s.requirePerm(scopeIssues, permWrite, s.handleApproveIssueSuggestion))
	s.route("POST /api/v3/repos/{owner}/{repo}/issues/{issue_number}/suggestions/{suggestion_id}/dismiss",
		s.requirePerm(scopeIssues, permWrite, s.handleDismissIssueSuggestion))
}

func (st *Store) GetPRCreationCap(repoKey string) PRCreationCap {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if cap := st.PRCreationCaps[repoKey]; cap != nil {
		return *cap
	}
	return PRCreationCap{Enabled: false, MaxOpenPullRequests: 10}
}

func (st *Store) SetPRCreationCap(repoKey string, cap PRCreationCap) PRCreationCap {
	st.mu.Lock()
	defer st.mu.Unlock()
	copy := cap
	st.PRCreationCaps[repoKey] = &copy
	if st.persist != nil {
		st.persist.MustPut("pr_creation_caps", repoKey, &copy)
	}
	return copy
}

func (st *Store) GetOrgPRCreationCap(orgLogin string) PRCreationCap {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if cap := st.OrgPRCreationCaps[orgLogin]; cap != nil {
		return *cap
	}
	return PRCreationCap{Enabled: false, MaxOpenPullRequests: 10}
}

func (st *Store) SetOrgPRCreationCap(orgLogin string, cap PRCreationCap) PRCreationCap {
	st.mu.Lock()
	defer st.mu.Unlock()
	copy := cap
	st.OrgPRCreationCaps[orgLogin] = &copy
	if st.persist != nil {
		st.persist.MustPut("org_pr_creation_caps", orgLogin, &copy)
	}
	return copy
}

func (st *Store) PRCreationBypassUsers(repoKey string) []*User {
	st.mu.RLock()
	defer st.mu.RUnlock()
	var out []*User
	for login := range st.PRCreationBypass[repoKey] {
		if user := st.UsersByLogin[login]; user != nil {
			copy := *user
			out = append(out, &copy)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Login < out[j].Login })
	return out
}

func (st *Store) ChangePRCreationBypass(repoKey string, logins []string, add bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.PRCreationBypass[repoKey] == nil {
		st.PRCreationBypass[repoKey] = map[string]bool{}
	}
	for _, login := range logins {
		if add {
			st.PRCreationBypass[repoKey][login] = true
		} else {
			delete(st.PRCreationBypass[repoKey], login)
		}
	}
	if st.persist != nil {
		st.persist.MustPut("pr_creation_bypass", repoKey, st.PRCreationBypass[repoKey])
	}
}

func (st *Store) CanCreatePullRequest(repoID, userID int, login string) bool {
	st.mu.RLock()
	defer st.mu.RUnlock()
	repo := st.Repos[repoID]
	if repo == nil {
		return false
	}
	cap := st.PRCreationCaps[repo.FullName]
	if cap == nil || !cap.Enabled || st.PRCreationBypass[repo.FullName][login] {
		return true
	}
	open := 0
	for _, pull := range st.PullRequests {
		if pull.RepoID == repoID && pull.AuthorID == userID && pull.State == "OPEN" {
			open++
		}
	}
	return open < cap.MaxOpenPullRequests
}

func (s *Server) requireRepoAdminForPullExtension(w http.ResponseWriter, r *http.Request) *Repo {
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
		writeGHValidationError(w, "PullRequestCreationCap", "enabled", "missing_field")
		return
	}
	current := s.store.GetPRCreationCap(repo.FullName)
	current.Enabled = *request.Enabled
	if request.MaxOpenPullRequests != nil {
		if *request.MaxOpenPullRequests < 1 || *request.MaxOpenPullRequests > 1000 {
			writeGHValidationError(w, "PullRequestCreationCap", "max_open_pull_requests", "invalid")
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
		writeGHValidationError(w, "PullRequestCreationCap", "enabled", "missing_field")
		return
	}
	current := s.store.GetOrgPRCreationCap(org.Login)
	current.Enabled = *request.Enabled
	if request.MaxOpenPullRequests != nil {
		if *request.MaxOpenPullRequests < 1 || *request.MaxOpenPullRequests > 1000 {
			writeGHValidationError(w, "PullRequestCreationCap", "max_open_pull_requests", "invalid")
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
		out = append(out, userToJSON(user))
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
		writeGHValidationError(w, "PullRequestCreationCapBypass", "users", "missing_field")
		return
	}
	for _, login := range request.Users {
		if s.store.LookupUserByLogin(login) == nil {
			writeGHValidationError(w, "PullRequestCreationCapBypass", "users", "invalid")
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

func clonePullRequestStack(stack *PullRequestStack) *PullRequestStack {
	if stack == nil {
		return nil
	}
	copy := *stack
	copy.PullRequests = append([]int(nil), stack.PullRequests...)
	return &copy
}

func (st *Store) ListPullRequestStacks(repoID int) []*PullRequestStack {
	st.mu.RLock()
	defer st.mu.RUnlock()
	var out []*PullRequestStack
	repo := st.Repos[repoID]
	if repo == nil {
		return snapshotPullRequestStacks(out)
	}
	for _, stack := range st.PullRequestStacks[repo.FullName] {
		out = append(out, clonePullRequestStack(stack))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return snapshotPullRequestStacks(out)
}

func (st *Store) GetPullRequestStack(repoKey string, number int) *PullRequestStack {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return clonePullRequestStack(st.PullRequestStacks[repoKey][number])
}

func (st *Store) CreatePullRequestStack(repo *Repo, pulls []*PullRequest) (*PullRequestStack, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.PullRequestStacks[repo.FullName] == nil {
		st.PullRequestStacks[repo.FullName] = map[int]*PullRequestStack{}
	}
	seen := map[int]bool{}
	for _, pull := range pulls {
		if seen[pull.Number] {
			return nil, fmt.Errorf("duplicate pull request")
		}
		seen[pull.Number] = true
		for _, existing := range st.PullRequestStacks[repo.FullName] {
			for _, number := range existing.PullRequests {
				if number == pull.Number {
					return nil, fmt.Errorf("pull request already belongs to a stack")
				}
			}
		}
	}
	number := 1
	for st.PullRequestStacks[repo.FullName][number] != nil {
		number++
	}
	stack := &PullRequestStack{
		ID: st.NextPullRequestStackID, Number: number, RepoID: repo.ID,
		BaseRef: pulls[0].BaseRefName, CreatedAt: time.Now().UTC(),
	}
	st.NextPullRequestStackID++
	for _, pull := range pulls {
		stack.PullRequests = append(stack.PullRequests, pull.Number)
	}
	st.PullRequestStacks[repo.FullName][number] = stack
	if st.persist != nil {
		st.persist.MustPut("pull_request_stacks", repo.FullName, st.PullRequestStacks[repo.FullName])
	}
	return clonePullRequestStack(stack), nil
}

func (st *Store) AddPullRequestsToStack(repoKey string, stackNumber int, pulls []*PullRequest) (*PullRequestStack, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	stack := st.PullRequestStacks[repoKey][stackNumber]
	if stack == nil {
		return nil, nil
	}
	present := map[int]bool{}
	for _, number := range stack.PullRequests {
		present[number] = true
	}
	for _, pull := range pulls {
		if present[pull.Number] {
			return nil, fmt.Errorf("pull request already belongs to stack")
		}
		for _, existing := range st.PullRequestStacks[repoKey] {
			for _, number := range existing.PullRequests {
				if number == pull.Number {
					return nil, fmt.Errorf("pull request already belongs to a stack")
				}
			}
		}
		present[pull.Number] = true
		stack.PullRequests = append(stack.PullRequests, pull.Number)
	}
	if st.persist != nil {
		st.persist.MustPut("pull_request_stacks", repoKey, st.PullRequestStacks[repoKey])
	}
	return clonePullRequestStack(stack), nil
}

func (st *Store) DeletePullRequestStack(repoKey string, number int) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.PullRequestStacks[repoKey][number] == nil {
		return false
	}
	delete(st.PullRequestStacks[repoKey], number)
	if st.persist != nil {
		st.persist.MustPut("pull_request_stacks", repoKey, st.PullRequestStacks[repoKey])
	}
	return true
}

func (s *Server) resolveStackPulls(w http.ResponseWriter, repo *Repo, numbers []int, minimum int) []*PullRequest {
	if len(numbers) < minimum || len(numbers) > 100 {
		writeGHValidationError(w, "PullRequestStack", "pull_requests", "invalid")
		return nil
	}
	pulls := make([]*PullRequest, 0, len(numbers))
	for _, number := range numbers {
		pull := s.store.GetPullRequestByNumber(repo.ID, number)
		if pull == nil {
			writeGHValidationError(w, "PullRequestStack", "pull_requests", "invalid")
			return nil
		}
		pulls = append(pulls, pull)
	}
	for index := 1; index < len(pulls); index++ {
		if pulls[index].BaseRefName != pulls[index-1].HeadRefName {
			writeGHValidationError(w, "PullRequestStack", "pull_requests", "invalid_stack_order")
			return nil
		}
	}
	return pulls
}

func (s *Server) pullRequestStackJSON(r *http.Request, repo *Repo, stack *PullRequestStack, minimal bool) map[string]interface{} {
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

func (s *Server) stackFromPath(w http.ResponseWriter, r *http.Request, admin bool) (*Repo, *PullRequestStack) {
	var repo *Repo
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
		writeGHValidationError(w, "PullRequestStack", "pull_requests", "invalid_stack_order")
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

func issueSuggestionKey(repoKey string, issueID int) string {
	return fmt.Sprintf("%s#%d", repoKey, issueID)
}

func cloneIssueSuggestion(suggestion *IssueSuggestion) *IssueSuggestion {
	if suggestion == nil {
		return nil
	}
	copy := *suggestion
	return &copy
}

// CreateIssueSuggestion is the ingestion seam for coding agents. The public
// REST surface deliberately exposes review, approval, and dismissal only.
func (st *Store) CreateIssueSuggestion(repoKey string, issueID int, suggestion IssueSuggestion) *IssueSuggestion {
	st.mu.Lock()
	defer st.mu.Unlock()
	key := issueSuggestionKey(repoKey, issueID)
	if st.IssueSuggestions[key] == nil {
		st.IssueSuggestions[key] = map[int]*IssueSuggestion{}
	}
	suggestion.ID = st.NextIssueSuggestionID
	st.NextIssueSuggestionID++
	suggestion.IssueID = issueID
	suggestion.State = "pending"
	suggestion.CreatedAt = time.Now().UTC()
	suggestion.UpdatedAt = suggestion.CreatedAt
	st.IssueSuggestions[key][suggestion.ID] = &suggestion
	if st.persist != nil {
		st.persist.MustPut("issue_suggestions", key, st.IssueSuggestions[key])
	}
	return cloneIssueSuggestion(&suggestion)
}

func (st *Store) ListIssueSuggestions(repoKey string, issueID int) []*IssueSuggestion {
	st.mu.RLock()
	defer st.mu.RUnlock()
	var out []*IssueSuggestion
	for _, suggestion := range st.IssueSuggestions[issueSuggestionKey(repoKey, issueID)] {
		out = append(out, cloneIssueSuggestion(suggestion))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotIssueSuggestions(out)
}

func (st *Store) ResolveIssueSuggestion(repoKey string, issueID, suggestionID, userID int, state string, eventID *int) *IssueSuggestion {
	st.mu.Lock()
	defer st.mu.Unlock()
	key := issueSuggestionKey(repoKey, issueID)
	suggestion := st.IssueSuggestions[key][suggestionID]
	if suggestion == nil || suggestion.State != "pending" {
		return nil
	}
	suggestion.State = state
	suggestion.ResolvedBy = &userID
	suggestion.IssueEventID = eventID
	suggestion.UpdatedAt = time.Now().UTC()
	if st.persist != nil {
		st.persist.MustPut("issue_suggestions", key, st.IssueSuggestions[key])
	}
	return cloneIssueSuggestion(suggestion)
}

func (s *Server) issueForSuggestionPath(w http.ResponseWriter, r *http.Request, write bool) (*Repo, *Issue) {
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
	var target *IssueSuggestion
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
		writeGHValidationError(w, "IssueSuggestion", "state", "invalid")
		return
	}
	user := ghUserFromContext(r.Context())
	state := "dismissed"
	var eventID *int
	if approve {
		state = "approved"
		switch target.Action {
		case "close_issue":
			s.store.UpdateIssue(issue.ID, func(item *Issue) {
				item.State = "CLOSED"
				now := time.Now().UTC()
				item.ClosedAt = &now
			})
		case "add_label":
			if target.TargetID == nil || s.store.GetLabel(*target.TargetID) == nil {
				writeGHValidationError(w, "IssueSuggestion", "target_id", "invalid")
				return
			}
			s.store.AddIssueLabels(repo.FullName, issue.Number, []int{*target.TargetID})
		case "add_assignee":
			if target.TargetID == nil || s.store.GetUserByID(*target.TargetID) == nil {
				writeGHValidationError(w, "IssueSuggestion", "target_id", "invalid")
				return
			}
			s.store.AddIssueAssignees(repo.ID, issue.Number, []int{*target.TargetID}, user.ID)
		case "set_type":
			if target.TargetID == nil || s.store.GetAssignableIssueTypeForRepo(repo, *target.TargetID) == nil {
				writeGHValidationError(w, "IssueSuggestion", "target_id", "invalid")
				return
			}
			s.store.UpdateIssue(issue.ID, func(item *Issue) { item.IssueTypeID = *target.TargetID })
		case "add_field":
			if target.TargetID == nil {
				writeGHValidationError(w, "IssueSuggestion", "target_id", "invalid")
				return
			}
			s.store.AddIssueFieldValues(issue.ID, map[int]interface{}{*target.TargetID: target.TargetValue})
		default:
			writeGHValidationError(w, "IssueSuggestion", "action", "invalid")
			return
		}
		event := s.store.RecordIssueEvent(repo.ID, issue.ID, user.ID, "issue_suggestion_approved", map[string]interface{}{})
		eventID = &event.ID
	}
	resolved := s.store.ResolveIssueSuggestion(repo.FullName, issue.ID, id, user.ID, state, eventID)
	if resolved == nil {
		writeGHValidationError(w, "IssueSuggestion", "state", "invalid")
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

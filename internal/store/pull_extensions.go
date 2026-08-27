package store

import (
	"fmt"
	"sort"
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

func clonePullRequestStack(stack *PullRequestStack) *PullRequestStack {
	if stack == nil {
		return nil
	}
	copy := *stack
	copy.PullRequests = append([]int(nil), stack.PullRequests...)
	return &copy
}

func cloneIssueSuggestion(suggestion *IssueSuggestion) *IssueSuggestion {
	if suggestion == nil {
		return nil
	}
	copy := *suggestion
	return &copy
}

func (st *Store) GetPRCreationCap(repoKey string) PRCreationCap {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if cap := st.PRCreationCaps[repoKey]; cap != nil {
		return *cap
	}
	return PRCreationCap{Enabled: false, MaxOpenPullRequests: 10}
}

func (st *Store) SetPRCreationCap(repoKey string, cap PRCreationCap) PRCreationCap {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	copy := cap
	st.PRCreationCaps[repoKey] = &copy
	if st.Persist != nil {
		st.Persist.MustPut("pr_creation_caps", repoKey, &copy)
	}
	return copy
}

func (st *Store) GetOrgPRCreationCap(orgLogin string) PRCreationCap {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if cap := st.OrgPRCreationCaps[orgLogin]; cap != nil {
		return *cap
	}
	return PRCreationCap{Enabled: false, MaxOpenPullRequests: 10}
}

func (st *Store) SetOrgPRCreationCap(orgLogin string, cap PRCreationCap) PRCreationCap {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	copy := cap
	st.OrgPRCreationCaps[orgLogin] = &copy
	if st.Persist != nil {
		st.Persist.MustPut("org_pr_creation_caps", orgLogin, &copy)
	}
	return copy
}

func (st *Store) PRCreationBypassUsers(repoKey string) []*User {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
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
	st.Mu.Lock()
	defer st.Mu.Unlock()
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
	if st.Persist != nil {
		st.Persist.MustPut("pr_creation_bypass", repoKey, st.PRCreationBypass[repoKey])
	}
}

func (st *Store) CanCreatePullRequest(repoID, userID int, login string) bool {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
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

func (st *Store) ListPullRequestStacks(repoID int) []*PullRequestStack {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
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
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return clonePullRequestStack(st.PullRequestStacks[repoKey][number])
}

func (st *Store) CreatePullRequestStack(repo *Repo, pulls []*PullRequest) (*PullRequestStack, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
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
	if st.Persist != nil {
		st.Persist.MustPut("pull_request_stacks", repo.FullName, st.PullRequestStacks[repo.FullName])
	}
	return clonePullRequestStack(stack), nil
}

func (st *Store) AddPullRequestsToStack(repoKey string, stackNumber int, pulls []*PullRequest) (*PullRequestStack, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
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
	if st.Persist != nil {
		st.Persist.MustPut("pull_request_stacks", repoKey, st.PullRequestStacks[repoKey])
	}
	return clonePullRequestStack(stack), nil
}

func (st *Store) DeletePullRequestStack(repoKey string, number int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.PullRequestStacks[repoKey][number] == nil {
		return false
	}
	delete(st.PullRequestStacks[repoKey], number)
	if st.Persist != nil {
		st.Persist.MustPut("pull_request_stacks", repoKey, st.PullRequestStacks[repoKey])
	}
	return true
}

// CreateIssueSuggestion is the ingestion seam for coding agents; the public
// REST surface exposes only review, approval, and dismissal.
func (st *Store) CreateIssueSuggestion(repoKey string, issueID int, suggestion IssueSuggestion) *IssueSuggestion {
	st.Mu.Lock()
	defer st.Mu.Unlock()
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
	if st.Persist != nil {
		st.Persist.MustPut("issue_suggestions", key, st.IssueSuggestions[key])
	}
	return cloneIssueSuggestion(&suggestion)
}

func (st *Store) ListIssueSuggestions(repoKey string, issueID int) []*IssueSuggestion {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*IssueSuggestion
	for _, suggestion := range st.IssueSuggestions[issueSuggestionKey(repoKey, issueID)] {
		out = append(out, cloneIssueSuggestion(suggestion))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotIssueSuggestions(out)
}

func (st *Store) ResolveIssueSuggestion(repoKey string, issueID, suggestionID, userID int, state string, eventID *int) *IssueSuggestion {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	key := issueSuggestionKey(repoKey, issueID)
	suggestion := st.IssueSuggestions[key][suggestionID]
	if suggestion == nil || suggestion.State != "pending" {
		return nil
	}
	suggestion.State = state
	suggestion.ResolvedBy = &userID
	suggestion.IssueEventID = eventID
	suggestion.UpdatedAt = time.Now().UTC()
	if st.Persist != nil {
		st.Persist.MustPut("issue_suggestions", key, st.IssueSuggestions[key])
	}
	return cloneIssueSuggestion(suggestion)
}

func issueSuggestionKey(repoKey string, issueID int) string {
	return fmt.Sprintf("%s#%d", repoKey, issueID)
}

package bleephub

import (
	"fmt"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) actionsRepoPayload(repoKey string) (map[string]interface{}, *store.Repo) {
	repo := s.store.GetRepoByFullName(repoKey)
	if repo == nil {
		return nil, nil
	}
	return repoPayload(repo, s.publicOrigin()), repo
}

func (s *Server) WorkflowRunEvent(wf *store.Workflow, action string) {
	repoJSON, repo := s.actionsRepoPayload(wf.RepoFullName)
	if repo == nil {
		return
	}
	base := fmt.Sprintf("http://%s", s.addr)
	s.store.Mu.RLock()
	runJSON := workflowRunJSON(wf, base, wf.RepoFullName, repoJSON)
	var wfFileJSON map[string]any
	if f := s.store.WorkflowFiles[wf.WorkflowFileID]; f != nil {
		wfFileJSON = workflowFileJSON(f, base, wf.RepoFullName)
	}
	s.store.Mu.RUnlock()

	payload := map[string]interface{}{
		"action":       action,
		"workflow_run": runJSON,
		"repository":   repoJSON,
		"sender":       senderPayload(s.workflowSender(wf), s.publicOrigin()),
	}
	if wfFileJSON != nil {
		payload["workflow"] = wfFileJSON
	}
	s.emitWebhookEvent(wf.RepoFullName, "workflow_run", action, payload)
}

func (s *Server) WorkflowJobEvent(wf *store.Workflow, job *store.WorkflowJob, action string) {
	repoJSON, repo := s.actionsRepoPayload(wf.RepoFullName)
	if repo == nil {
		return
	}
	base := fmt.Sprintf("http://%s", s.addr)
	jobJSON := s.workflowJobJSON(wf, job, base, wf.RepoFullName)
	payload := map[string]interface{}{
		"action":       action,
		"workflow_job": jobJSON,
		"repository":   repoJSON,
		"sender":       senderPayload(s.workflowSender(wf), s.publicOrigin()),
	}
	s.emitWebhookEvent(wf.RepoFullName, "workflow_job", action, payload)
}

func (s *Server) CheckRunEvent(repoKey string, checkRunID int64, action string) {
	repoJSON, repo := s.actionsRepoPayload(repoKey)
	if repo == nil {
		return
	}
	cr := s.store.GetCheckRun(checkRunID)
	if cr == nil {
		return
	}
	payload := map[string]interface{}{
		"action":     action,
		"check_run":  s.checkRunToJSON(cr, s.externalURL),
		"repository": repoJSON,
		"sender":     ghostSenderPayload(s.publicOrigin()),
	}
	s.emitWebhookEvent(repoKey, "check_run", action, payload)
	if action == "completed" {
		s.advanceMergeQueuesForRepo(repo)
	}
}

func (s *Server) CheckSuiteEvent(repoKey string, suiteID int64, action string) {
	repoJSON, repo := s.actionsRepoPayload(repoKey)
	if repo == nil {
		return
	}
	suite := s.store.GetCheckSuite(suiteID)
	if suite == nil {
		return
	}
	payload := map[string]interface{}{
		"action":      action,
		"check_suite": s.checkSuiteToJSON(suite, s.externalURL),
		"repository":  repoJSON,
		"sender":      ghostSenderPayload(s.publicOrigin()),
	}
	s.emitWebhookEvent(repoKey, "check_suite", action, payload)
	if action == "completed" {
		s.advanceMergeQueuesForRepo(repo)
	}
}

func (s *Server) workflowSender(wf *store.Workflow) *store.User {
	if wf.EventPayload == nil {
		return nil
	}
	sender, _ := wf.EventPayload["sender"].(map[string]interface{})
	if sender == nil {
		return nil
	}
	login, _ := sender["login"].(string)
	if login == "" {
		return nil
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return s.store.UsersByLogin[login]
}

type checksState struct {
	MissingRequired []string // required contexts not green (absent/pending/failed)
	AnyPending      bool
	AnyFailing      bool
	RequiredFailing bool // a required context has a terminal failure (not merely pending)
}

func (s *Server) evaluateChecksForMerge(repo *store.Repo, baseBranch, headSha string) checksState {
	state := checksState{}
	if headSha == "" {
		return state
	}
	repoKey := repo.FullName
	runs := s.store.ListCheckRunsForCommit(repoKey, headSha, "", "", 0)
	byName := map[string]*store.CheckRun{}
	for _, cr := range runs {
		// Latest run per name wins; reruns create new check runs.
		if prev, ok := byName[cr.Name]; !ok || cr.ID > prev.ID {
			byName[cr.Name] = cr
		}
		if cr.Status != "completed" {
			state.AnyPending = true
		} else if cr.Conclusion == "failure" || cr.Conclusion == "timed_out" || cr.Conclusion == "startup_failure" {
			state.AnyFailing = true
		}
	}
	// Classic commit statuses satisfy required contexts and contribute to
	// pending/failing like check runs, matching GitHub: most external CI reports
	// through the statuses API, so ignoring them leaves mergeable_state "blocked"
	// forever after the required context succeeds.
	_, _, statuses := s.store.CommitStatuses.Combined(repoKey, headSha)
	statusByCtx := map[string]string{}
	for _, cs := range statuses {
		statusByCtx[cs.Context] = string(cs.State)
		switch string(cs.State) {
		case "pending":
			state.AnyPending = true
		case "failure", "error":
			state.AnyFailing = true
		}
	}
	for _, ctx := range s.requiredCheckContexts(repo.ID, baseBranch) {
		if statusByCtx[ctx] == "success" {
			continue
		}
		if statusByCtx[ctx] == "failure" || statusByCtx[ctx] == "error" {
			state.RequiredFailing = true
		}
		cr, ok := byName[ctx]
		if !ok || cr.Status != "completed" || (cr.Conclusion != "success" && cr.Conclusion != "neutral" && cr.Conclusion != "skipped") {
			state.MissingRequired = append(state.MissingRequired, ctx)
			if ok && cr.Status == "completed" && (cr.Conclusion == "failure" || cr.Conclusion == "timed_out" || cr.Conclusion == "startup_failure") {
				state.RequiredFailing = true
			}
		}
	}
	return state
}

func (s *Server) prHeadSha(repo *store.Repo, pr *store.PullRequest) string {
	stor, _ := store.PullRequestGitStorage(s.store, repo, pr)
	return store.ResolveBranchSha(stor, pr.HeadRefName)
}

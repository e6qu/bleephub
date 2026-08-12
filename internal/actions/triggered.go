package actions

import (
	"context"
	"fmt"
	"strings"
)

// SubmitTriggeredWorkflow parses, expands, and submits one workflow file
// for an event. Event metadata must travel INTO SubmitWorkflow: it
// resolves the originating workflow file from RepoFullName at submit
// time (the run's workflow_id), and the workflow becomes visible to
// other goroutines the moment it is stored — patching fields afterwards
// would both mis-derive the file id and race those readers.
func (s *Engine) SubmitTriggeredWorkflow(fileName string, content []byte, meta *WorkflowEventMeta) (*Workflow, error) {
	wfDef, err := ParseWorkflow(content)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", fileName, err)
	}

	expandedDef := ExpandMatrixJobs(wfDef)

	if expandedDef.Env == nil {
		expandedDef.Env = make(map[string]string)
	}
	expandedDef.Env["__defaultImage"] = ""

	serverURL := fmt.Sprintf("http://%s", s.addr)
	expandedDef.Env["__serverURL"] = serverURL

	return s.SubmitWorkflow(context.Background(), serverURL, expandedDef, "", meta)
}

// WorkflowFileDisabled reports whether the registered workflow file for
// (repo, filename) was manually disabled — disabled workflows never
// trigger, matching real GitHub.
func (s *Engine) WorkflowFileDisabled(repoKey, filename string) bool {
	path := ".github/workflows/" + filename
	for _, f := range s.store.ListWorkflowFiles(repoKey) {
		if f.Path == path {
			return strings.HasPrefix(f.State, "disabled")
		}
	}
	return false
}

// pullRequestIsFromFork reports whether the event payload's pull request has
// its head in a repository other than repoKey.
func pullRequestIsFromFork(payload map[string]interface{}, repoKey string) bool {
	pr, _ := payload["pull_request"].(map[string]interface{})
	if pr == nil || repoKey == "" {
		return false
	}
	head, _ := pr["head"].(map[string]interface{})
	if head == nil {
		return false
	}
	headRepo, _ := head["repo"].(map[string]interface{})
	if headRepo == nil {
		return false
	}
	headFullName, _ := headRepo["full_name"].(string)
	return headFullName != "" && !strings.EqualFold(headFullName, repoKey)
}

// OSFromDescription maps a runner's free-form OS description onto the
// runners API's os vocabulary (linux / windows / macos).
func OSFromDescription(desc string) string {
	d := strings.ToLower(desc)
	switch {
	case strings.Contains(d, "linux"):
		return "linux"
	case strings.Contains(d, "windows"):
		return "windows"
	case strings.Contains(d, "darwin"), strings.Contains(d, "macos"):
		return "macos"
	default:
		return "linux"
	}
}

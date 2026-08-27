package actions

import (
	"context"
	"fmt"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// SubmitTriggeredWorkflow parses, expands, and submits one workflow file for an
// event. Metadata must pass into SubmitWorkflow, not be patched afterwards:
// SubmitWorkflow derives the run's workflow_id from it, and the workflow becomes
// visible to other goroutines the instant it is stored.
func (s *Engine) SubmitTriggeredWorkflow(fileName string, content []byte, meta *WorkflowEventMeta) (*store.Workflow, error) {
	wfDef, err := store.ParseWorkflow(content)
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

// WorkflowFileDisabled reports whether (repo, filename) was manually disabled.
// Disabled workflows never trigger, matching real GitHub.
func (s *Engine) WorkflowFileDisabled(repoKey, filename string) bool {
	path := ".github/workflows/" + filename
	for _, f := range s.store.ListWorkflowFiles(repoKey) {
		if f.Path == path {
			return strings.HasPrefix(f.State, "disabled")
		}
	}
	return false
}

// pullRequestIsFromFork reports whether the payload's pull request has its head
// in a repository other than repoKey.
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

// OSFromDescription maps a runner's free-form OS description onto the runners
// API's os vocabulary (linux / windows / macos).
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

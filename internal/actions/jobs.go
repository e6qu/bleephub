package actions

import (
	"fmt"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/google/uuid"
)

// SubmitRequest is the simplified job submission format. HostMode runs the job
// directly on the runner (jobContainer null), as GitHub does for jobs without
// `container:`.
type SubmitRequest struct {
	Image    string       `json:"image"`
	HostMode bool         `json:"hostMode"`
	Steps    []SubmitStep `json:"steps"`
}

type SubmitStep struct {
	Run string `json:"run"`
}

// ExpandMatrixJobs expands matrix strategies into one job entry per combination.
func ExpandMatrixJobs(wf *store.WorkflowDef) *store.WorkflowDef {
	expanded := &store.WorkflowDef{
		Name:        wf.Name,
		RunName:     wf.RunName,
		Env:         wf.Env,
		Permissions: wf.Permissions,
		Defaults:    wf.Defaults,
		Jobs:        make(map[string]*store.JobDef),
		Concurrency: wf.Concurrency,
	}

	// Rewrite `needs` in a second pass: wf.Jobs is a map, so an interleaved
	// single pass can rewrite a dependent against expansions not yet produced,
	// leaving it pointing at a key that no longer exists.
	expandedKeysFor := make(map[string][]string, len(wf.Jobs))

	for key, jd := range wf.Jobs {
		var combos []map[string]interface{}
		if jd.Strategy != nil {
			// Gate on the expansion, not on Values: an `include:`-only matrix
			// has no Values but is still a valid matrix.
			combos = ExpandMatrix(&jd.Strategy.Matrix)
		}
		if len(combos) == 0 {
			single := *jd
			single.Needs = append([]string(nil), jd.Needs...)
			expanded.Jobs[key] = &single
			continue
		}

		keys := make([]string, 0, len(combos))
		for i, combo := range combos {
			newKey := fmt.Sprintf("%s_%d", key, i)
			newJD := *jd
			newJD.Name = MatrixJobName(key, combo, jd.Strategy.Matrix.Order)
			newJD.Needs = append([]string(nil), jd.Needs...)
			newJD.MatrixGroup = key
			newJD.MatrixMaxParallel = jd.Strategy.MaxParallel
			newJD.MatrixValues = make(map[string]interface{}, len(combo))
			for matrixKey, matrixValue := range combo {
				newJD.MatrixValues[matrixKey] = matrixValue
			}

			// Deep-copy env; matrix values stay out of it (their own context).
			newJD.Env = make(map[string]string, len(jd.Env))
			for k, v := range jd.Env {
				newJD.Env[k] = v
			}

			expanded.Jobs[newKey] = &newJD
			keys = append(keys, newKey)
		}
		expandedKeysFor[key] = keys
	}

	for _, jd := range expanded.Jobs {
		newNeeds := make([]string, 0, len(jd.Needs))
		for _, dep := range jd.Needs {
			if keys, ok := expandedKeysFor[dep]; ok {
				newNeeds = append(newNeeds, keys...)
				continue
			}
			newNeeds = append(newNeeds, dep)
		}
		jd.Needs = newNeeds
	}

	return expanded
}

// jobContainerValue renders the job message's jobContainer: a bare string for
// container jobs, null for host-mode jobs (GitHub's shape with no `container:`).
func jobContainerValue(image string) interface{} {
	if image == "" {
		return nil
	}
	return image
}

// BuildJobMessage builds the AgentJobRequestMessage the official GitHub Actions
// runner expects, matching ChristopherHX/runner.server's PipelineContextData +
// TemplateToken serialization. scopeID names the plan; jobToken is the runtime
// bearer credential the caller minted for it.
func BuildJobMessage(serverURL, jobID, planID, timelineID string, requestID int64, req *SubmitRequest, scopeID, jobToken string) map[string]interface{} {

	// Only user-defined steps; the runner adds setup/cleanup internally.
	steps := make([]map[string]interface{}, 0, len(req.Steps))

	for i, step := range req.Steps {
		stepID := uuid.New().String()
		displayName := fmt.Sprintf("Run %s", truncateDisplay(step.Run, 40))
		// Inputs must be a TemplateToken MappingToken: {"type":2,"map":[{"Key":k,"Value":v},...]}.
		contextName := fmt.Sprintf("__run_%d", i+1)
		steps = append(steps, map[string]interface{}{
			"type": "action",
			"id":   stepID,
			"name": contextName,
			"reference": map[string]interface{}{
				"type": "script",
			},
			"displayNameToken": displayName,
			"contextName":      contextName,
			"condition":        "success()",
			"inputs": map[string]interface{}{
				"type": 2,
				"map": []interface{}{
					map[string]interface{}{
						"Key":   map[string]interface{}{"type": 0, "lit": "script"},
						"Value": templateToken(step.Run),
					},
				},
			},
		})
	}

	return map[string]interface{}{
		"messageType": "PipelineAgentJobRequest",
		"plan": map[string]interface{}{
			"scopeIdentifier": scopeID,
			"planId":          planID,
			"planType":        "free",
			"planGroup":       "free",
			"version":         12,
			"owner": map[string]interface{}{
				"id":   0,
				"name": "Community",
			},
		},
		"timeline": map[string]interface{}{
			"id":       timelineID,
			"changeId": 1,
			"location": nil,
		},
		"jobId":                jobID,
		"jobDisplayName":       "test",
		"jobName":              "test",
		"requestId":            requestID,
		"lockedUntil":          "0001-01-01T00:00:00",
		"jobContainer":         jobContainerValue(req.Image),
		"jobServiceContainers": nil,
		"jobOutputs":           nil,
		"resources": map[string]interface{}{
			"endpoints": []map[string]interface{}{
				{
					"name": "SystemVssConnection",
					"url":  serverURL + "/",
					"authorization": map[string]interface{}{
						"scheme": "OAuth",
						"parameters": map[string]string{
							"AccessToken": jobToken,
						},
					},
					"data": map[string]string{
						"CacheServerUrl":    serverURL + "/",
						"ResultsServiceUrl": serverURL + "/",
					},
					"isShared": false,
					"isReady":  true,
				},
			},
			"repositories": []interface{}{},
			"containers":   []interface{}{},
		},
		// contextData uses PipelineContextData format: strings are bare JSON
		// strings; a Dictionary is {"t": 2, "d": [{"k":"key","v":<value>}, ...]}.
		"contextData": map[string]interface{}{
			"github": DictContextData(
				"server_url", serverURL,
				"api_url", serverURL,
				"repository", "",
				"repository_owner", "",
				"run_id", "1",
				"run_number", "1",
				"workflow", "test",
				"job", "test",
				"event_name", "push",
				"sha", "",
				"ref", "",
				"action", "__run",
				"workspace", "/github/workspace",
				"token", jobToken,
			),
			// Runner-agnostic; the broker rebinds this to the leasing agent at
			// delivery (ACT-051, rebindRunnerContext).
			"runner":   RunnerContextData(nil),
			"env":      DictContextData(),
			"vars":     DictContextData(),
			"needs":    DictContextData(),
			"inputs":   nil,
			"matrix":   nil,
			"strategy": nil,
		},
		"variables": map[string]interface{}{
			"system.github.job":                      varVal("test"),
			"system.github.runid":                    varVal("1"),
			"system.github.token":                    varSecret(jobToken),
			"github_token":                           varSecret(jobToken),
			"system.phaseDisplayName":                varVal("test"),
			"system.runnerGroupName":                 varVal("Default"),
			"DistributedTask.NewActionMetadata":      varVal("true"),
			"DistributedTask.EnableCompositeActions": varVal("true"),
		},
		"mask":                 []interface{}{},
		"steps":                steps,
		"workspace":            map[string]interface{}{},
		"defaults":             nil,
		"environmentVariables": nil,
		"actionsEnvironment":   nil,
		"fileTable":            []string{".github/workflows/ci.yml"},
	}
}

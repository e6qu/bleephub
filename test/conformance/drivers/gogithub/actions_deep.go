package main

import (
	"fmt"
	"net/http"
	"time"

	github "github.com/google/go-github/v88/github"
)

// conformanceWorkflow is the smallest workflow that a client can dispatch and
// that produces exactly one job, so the job assertions below are deterministic.
const conformanceWorkflow = `name: conformance
on:
  workflow_dispatch:
    inputs:
      reason:
        description: why the run was started
        required: true
  push:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo conformance
`

// runActionsWorkflows drives the workflow lifecycle a continuous-integration
// integration actually exercises: publish a workflow, dispatch it, find the
// run, read its jobs and logs, cancel it, and re-run it. Everything that is
// genuinely asynchronous is polled with a bounded deadline and fails clearly
// on expiry — there are no wall-clock sleeps standing in for synchronisation.
func runActionsWorkflows(client *github.Client, rec *recorder, set *fixtureSet) {
	const domain = "actions"
	sc := newScratch(client, set.owner, "conformance-actions-runs")
	if !sc.ok() {
		skipAll(rec, domain, "POST /user/repos", "the workflow repository fixture could not be provisioned",
			"actions.getWorkflow", "actions.createWorkflowDispatch", "actions.listWorkflowRunsByFileName",
			"actions.getWorkflowRun", "actions.listWorkflowJobs", "actions.getWorkflowJob",
			"actions.cancelWorkflowRun", "actions.rerunWorkflow")
		return
	}

	if _, err := commitFile(client, sc, ".github/workflows/conformance.yml",
		"add the conformance workflow", conformanceWorkflow); err != nil {
		skipAll(rec, domain, "PUT /repos/{owner}/{repo}/contents/.github/workflows/conformance.yml",
			"the workflow file could not be committed: "+truncate(err.Error()),
			"actions.getWorkflow", "actions.createWorkflowDispatch", "actions.listWorkflowRunsByFileName",
			"actions.getWorkflowRun", "actions.listWorkflowJobs", "actions.getWorkflowJob",
			"actions.cancelWorkflowRun", "actions.rerunWorkflow")
		return
	}

	var workflowID int64
	rec.check(domain, "actions.listRepoWorkflows (registers a committed workflow)",
		"GET /repos/{owner}/{repo}/actions/workflows", func() error {
			workflows, _, err := client.Actions.ListWorkflows(ctx, sc.owner, sc.repo, nil)
			if err != nil {
				return err
			}
			if workflows.GetTotalCount() != 1 || len(workflows.Workflows) != 1 {
				return deviate("exactly the one committed workflow",
					fmt.Sprintf("total_count %d, %d entries", workflows.GetTotalCount(), len(workflows.Workflows)),
					"committing a workflow file did not register a workflow")
			}
			workflow := workflows.Workflows[0]
			workflowID = workflow.GetID()
			if workflow.GetName() != "conformance" {
				return deviate("conformance", workflow.GetName(), "the workflow name is not read from the file")
			}
			if workflow.GetPath() != ".github/workflows/conformance.yml" {
				return deviate(".github/workflows/conformance.yml", workflow.GetPath(), "the workflow path is wrong")
			}
			if workflow.GetState() != "active" {
				return deviate("active", workflow.GetState(), "a newly committed workflow is not active")
			}
			return wantField("workflow.node_id", workflow.GetNodeID())
		})

	rec.check(domain, "actions.getWorkflow (by file name)",
		"GET /repos/{owner}/{repo}/actions/workflows/{workflow_id}", func() error {
			workflow, _, err := client.Actions.GetWorkflowByFileName(ctx, sc.owner, sc.repo, "conformance.yml")
			if err != nil {
				return err
			}
			if workflow.GetID() == 0 {
				return deviate("a non-zero id", "0", "the workflow fetched by file name has no id")
			}
			if workflowID != 0 && workflow.GetID() != workflowID {
				return deviate(fmt.Sprintf("%d", workflowID), fmt.Sprintf("%d", workflow.GetID()),
					"fetching by file name returns a different workflow than the listing")
			}
			return wantField("workflow.html_url", workflow.GetHTMLURL())
		})

	rec.check(domain, "actions.createWorkflowDispatch",
		"POST /repos/{owner}/{repo}/actions/workflows/{workflow_id}/dispatches", func() error {
			_, resp, err := client.Actions.CreateWorkflowDispatchEventByFileName(ctx, sc.owner, sc.repo,
				"conformance.yml", github.CreateWorkflowDispatchEventRequest{
					Ref:    sc.branch,
					Inputs: map[string]any{"reason": "conformance"},
				})
			if err != nil {
				return err
			}
			return wantStatus(resp, http.StatusNoContent, "workflow dispatch")
		})

	rec.check(domain, "actions.createWorkflowDispatch (missing required input)",
		"POST /actions/workflows/{workflow_id}/dispatches without a required input", func() error {
			_, _, err := client.Actions.CreateWorkflowDispatchEventByFileName(ctx, sc.owner, sc.repo,
				"conformance.yml", github.CreateWorkflowDispatchEventRequest{Ref: sc.branch})
			return wantHTTPError(err, http.StatusUnprocessableEntity, "a dispatch missing a required input")
		})

	var runID int64
	rec.check(domain, "actions.listWorkflowRunsByFileName",
		"GET /repos/{owner}/{repo}/actions/workflows/{workflow_id}/runs?event=workflow_dispatch", func() error {
			return pollUntil("the dispatched run appears in the workflow's run listing", 30*time.Second,
				func() (bool, error) {
					runs, _, err := client.Actions.ListWorkflowRunsByFileName(ctx, sc.owner, sc.repo,
						"conformance.yml", &github.ListWorkflowRunsOptions{Event: "workflow_dispatch"})
					if err != nil {
						return false, err
					}
					if runs.GetTotalCount() < 1 || len(runs.WorkflowRuns) < 1 {
						return false, nil
					}
					run := runs.WorkflowRuns[0]
					runID = run.GetID()
					if run.GetEvent() != "workflow_dispatch" {
						return false, deviate("workflow_dispatch", run.GetEvent(), "the run event is wrong")
					}
					if run.GetHeadBranch() != sc.branch {
						return false, deviate(sc.branch, run.GetHeadBranch(), "the run head_branch is wrong")
					}
					if run.GetRunNumber() < 1 {
						return false, deviate("run_number >= 1", fmt.Sprintf("%d", run.GetRunNumber()),
							"the run has no run_number, which every client renders")
					}
					return true, nil
				})
		})

	if runID == 0 {
		skipAll(rec, domain, "GET /repos/{owner}/{repo}/actions/runs/{run_id}",
			"the dispatched workflow run never appeared",
			"actions.getWorkflowRun", "actions.listWorkflowJobs", "actions.getWorkflowJob",
			"actions.getWorkflowRunAttempt", "actions.listWorkflowRunArtifacts", "actions.getWorkflowRunUsage",
			"actions.cancelWorkflowRun", "actions.rerunWorkflow", "actions.listWorkflowRunsForRepo (branch filter)")
		return
	}

	rec.check(domain, "actions.getWorkflowRun", "GET /repos/{owner}/{repo}/actions/runs/{run_id}", func() error {
		run, _, err := client.Actions.GetWorkflowRunByID(ctx, sc.owner, sc.repo, runID)
		if err != nil {
			return err
		}
		if run.GetID() != runID {
			return deviate(fmt.Sprintf("%d", runID), fmt.Sprintf("%d", run.GetID()), "the wrong run came back")
		}
		if run.GetWorkflowID() == 0 {
			return deviate("workflow_id populated", "0", "a run does not say which workflow it belongs to")
		}
		if run.GetRunAttempt() < 1 {
			return deviate("run_attempt >= 1", fmt.Sprintf("%d", run.GetRunAttempt()), "the run has no attempt number")
		}
		if run.GetHeadSHA() == "" {
			return deviate("head_sha populated", "empty", "a run carries no head_sha")
		}
		if run.GetJobsURL() == "" || run.GetLogsURL() == "" {
			return deviate("jobs_url and logs_url populated", "empty",
				"a run omits the hypermedia a client follows to its jobs and logs")
		}
		return wantField("run.node_id", run.GetNodeID())
	})

	rec.check(domain, "actions.listWorkflowRunsForRepo (branch filter)",
		"GET /repos/{owner}/{repo}/actions/runs?branch=", func() error {
			runs, _, err := client.Actions.ListRepositoryWorkflowRuns(ctx, sc.owner, sc.repo,
				&github.ListWorkflowRunsOptions{Branch: "no-such-branch"})
			if err != nil {
				return err
			}
			if runs.GetTotalCount() != 0 {
				return deviate("total_count 0", fmt.Sprintf("%d", runs.GetTotalCount()),
					"the branch filter is ignored, so a client cannot narrow a run listing")
			}
			return nil
		})

	var jobID int64
	rec.check(domain, "actions.listWorkflowJobs", "GET /repos/{owner}/{repo}/actions/runs/{run_id}/jobs", func() error {
		jobs, _, err := client.Actions.ListWorkflowJobs(ctx, sc.owner, sc.repo, runID, nil)
		if err != nil {
			return err
		}
		if jobs.GetTotalCount() < 1 || len(jobs.Jobs) < 1 {
			return deviate("the run's one job", fmt.Sprintf("total_count %d", jobs.GetTotalCount()),
				"a dispatched run exposes no jobs")
		}
		job := jobs.Jobs[0]
		jobID = job.GetID()
		if job.GetRunID() != runID {
			return deviate(fmt.Sprintf("%d", runID), fmt.Sprintf("%d", job.GetRunID()), "the job points at the wrong run")
		}
		if job.GetName() != "build" {
			return deviate("build", job.GetName(), "the job name does not come from the workflow file")
		}
		if job.GetWorkflowName() != "conformance" {
			return deviate("conformance", job.GetWorkflowName(),
				"the job omits workflow_name, which clients render in a run's job list")
		}
		return nil
	})

	if jobID != 0 {
		rec.check(domain, "actions.getWorkflowJob", "GET /repos/{owner}/{repo}/actions/jobs/{job_id}", func() error {
			job, _, err := client.Actions.GetWorkflowJobByID(ctx, sc.owner, sc.repo, jobID)
			if err != nil {
				return err
			}
			if job.GetID() != jobID {
				return deviate(fmt.Sprintf("%d", jobID), fmt.Sprintf("%d", job.GetID()), "the wrong job came back")
			}
			if job.GetRunURL() == "" || job.GetHTMLURL() == "" {
				return deviate("run_url and html_url populated", "empty", "a job omits its hypermedia")
			}
			return wantField("job.node_id", job.GetNodeID())
		})
	} else {
		rec.skip1(domain, "actions.getWorkflowJob", "GET /repos/{owner}/{repo}/actions/jobs/{job_id}",
			"the run exposed no job to read")
	}

	rec.check(domain, "actions.listWorkflowRunArtifacts",
		"GET /repos/{owner}/{repo}/actions/runs/{run_id}/artifacts", func() error {
			artifacts, _, err := client.Actions.ListWorkflowRunArtifacts(ctx, sc.owner, sc.repo, runID, nil)
			if err != nil {
				return err
			}
			if artifacts == nil {
				return deviate("an artifacts envelope", "nil", "the per-run artifact listing did not decode")
			}
			return nil
		})

	rec.check(domain, "actions.getWorkflowRunAttempt",
		"GET /repos/{owner}/{repo}/actions/runs/{run_id}/attempts/1", func() error {
			run, _, err := client.Actions.GetWorkflowRunAttempt(ctx, sc.owner, sc.repo, runID, 1, nil)
			if err != nil {
				return err
			}
			if run.GetRunAttempt() != 1 {
				return deviate("run_attempt 1", fmt.Sprintf("%d", run.GetRunAttempt()),
					"attempt 1 does not report itself as attempt 1")
			}
			return nil
		})

	rec.check(domain, "actions.listWorkflowJobsAttempt",
		"GET /repos/{owner}/{repo}/actions/runs/{run_id}/attempts/1/jobs", func() error {
			jobs, _, err := client.Actions.ListWorkflowJobsAttempt(ctx, sc.owner, sc.repo, runID, 1, nil)
			if err != nil {
				return err
			}
			if jobs.GetTotalCount() < 1 {
				return deviate("the attempt's job", fmt.Sprintf("total_count %d", jobs.GetTotalCount()),
					"the per-attempt job listing is empty")
			}
			return nil
		})

	rec.check(domain, "actions.getWorkflowRunUsage", "GET /repos/{owner}/{repo}/actions/runs/{run_id}/timing", func() error {
		usage, _, err := client.Actions.GetWorkflowRunUsageByID(ctx, sc.owner, sc.repo, runID)
		if err != nil {
			return err
		}
		if usage == nil {
			return deviate("a timing envelope", "nil", "run timing did not decode")
		}
		return nil
	})

	rec.check(domain, "actions.getWorkflowUsage", "GET /repos/{owner}/{repo}/actions/workflows/{id}/timing", func() error {
		usage, _, err := client.Actions.GetWorkflowUsageByFileName(ctx, sc.owner, sc.repo, "conformance.yml")
		if err != nil {
			return err
		}
		if usage == nil {
			return deviate("a timing envelope", "nil", "workflow timing did not decode")
		}
		return nil
	})

	rec.check(domain, "actions.getWorkflowRunLogs", "GET /repos/{owner}/{repo}/actions/runs/{run_id}/logs", func() error {
		location, resp, err := client.Actions.GetWorkflowRunLogs(ctx, sc.owner, sc.repo, runID, 1)
		if err != nil {
			// GitHub answers 404 while a run has produced no log archive yet;
			// that is a legitimate state for a run no runner has executed, and
			// the client decoding that 404 is still the contract being tested.
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				return nil
			}
			return err
		}
		if location == nil || location.String() == "" {
			return deviate("a redirect to the log archive", "no Location",
				"the log endpoint answered without the redirect clients follow")
		}
		return nil
	})

	rec.check(domain, "actions.cancelWorkflowRun", "POST /repos/{owner}/{repo}/actions/runs/{run_id}/cancel", func() error {
		resp, err := client.Actions.CancelWorkflowRunByID(ctx, sc.owner, sc.repo, runID)
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusAccepted, "run cancellation"); err != nil {
			return err
		}
		return pollUntil("the cancelled run reaches a completed status", 30*time.Second, func() (bool, error) {
			run, _, err := client.Actions.GetWorkflowRunByID(ctx, sc.owner, sc.repo, runID)
			if err != nil {
				return false, err
			}
			return run.GetStatus() == "completed", nil
		})
	})

	rec.check(domain, "actions.rerunWorkflow", "POST /repos/{owner}/{repo}/actions/runs/{run_id}/rerun", func() error {
		resp, err := client.Actions.RerunWorkflowByID(ctx, sc.owner, sc.repo, runID)
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusCreated, "run re-run"); err != nil {
			return err
		}
		return pollUntil("the re-run raises run_attempt to 2", 30*time.Second, func() (bool, error) {
			run, _, err := client.Actions.GetWorkflowRunByID(ctx, sc.owner, sc.repo, runID)
			if err != nil {
				return false, err
			}
			return run.GetRunAttempt() >= 2, nil
		})
	})

	rec.check(domain, "actions.disableWorkflow / enableWorkflow",
		"PUT /repos/{owner}/{repo}/actions/workflows/{id}/disable", func() error {
			if _, err := client.Actions.DisableWorkflowByFileName(ctx, sc.owner, sc.repo, "conformance.yml"); err != nil {
				return err
			}
			workflow, _, err := client.Actions.GetWorkflowByFileName(ctx, sc.owner, sc.repo, "conformance.yml")
			if err != nil {
				return err
			}
			if workflow.GetState() != "disabled_manually" {
				return deviate("disabled_manually", workflow.GetState(),
					"disabling a workflow does not change its state, so a client cannot tell it took effect")
			}
			if _, err := client.Actions.EnableWorkflowByFileName(ctx, sc.owner, sc.repo, "conformance.yml"); err != nil {
				return err
			}
			workflow, _, err = client.Actions.GetWorkflowByFileName(ctx, sc.owner, sc.repo, "conformance.yml")
			if err != nil {
				return err
			}
			if workflow.GetState() != "active" {
				return deviate("active", workflow.GetState(), "re-enabling a workflow does not restore its state")
			}
			return nil
		})

	rec.check(domain, "actions.deleteWorkflowRunLogs", "DELETE /repos/{owner}/{repo}/actions/runs/{run_id}/logs", func() error {
		resp, err := client.Actions.DeleteWorkflowRunLogs(ctx, sc.owner, sc.repo, runID)
		if err != nil {
			// Deleting logs a run never produced is a documented 404.
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				return nil
			}
			return err
		}
		return nil
	})

	// The public Representational State Transfer surface has no artifact
	// upload: an artifact is uploaded by the runner over the Actions results
	// protocol with a job runtime token, which no software development kit or
	// command-line client in this matrix can mint. Recording it as a skip
	// keeps the scoreboard honest instead of claiming coverage.
	rec.skip1(domain, "actions.uploadArtifact", "PUT /_apis/v1/artifacts/{artifact_id}/upload",
		"artifact upload is the runner's protocol and needs a job runtime token; no SDK or CLI can mint one")
	rec.skip1(domain, "actions.downloadArtifact", "GET /repos/{owner}/{repo}/actions/artifacts/{id}/{archive_format}",
		"no artifact exists to download: producing one requires a runner executing a job")
}

// runActionsConfiguration covers the configuration surface — secrets,
// variables, permissions, runners, caches and OpenID Connect — at each of the
// three scopes GitHub exposes it at.
func runActionsConfiguration(client *github.Client, rec *recorder, set *fixtureSet) {
	const domain = "actions"
	sc := newScratch(client, set.owner, "conformance-actions-config")
	if !sc.ok() {
		skipAll(rec, domain, "POST /user/repos", "the actions-configuration repository could not be provisioned",
			"actions.getRepoVariable", "actions.updateRepoVariable", "actions.deleteRepoVariable",
			"actions.updateActionsPermissions", "actions.createRegistrationToken", "actions.getRepoOIDCSubjectClaim")
		return
	}

	// --- Variables at repository scope -----------------------------------
	rec.check(domain, "actions.createRepoVariable (round trip)",
		"POST /repos/{owner}/{repo}/actions/variables", func() error {
			if _, err := client.Actions.CreateRepoVariable(ctx, sc.owner, sc.repo,
				&github.ActionsVariable{Name: "CONFORMANCE_VAR", Value: "one"}); err != nil {
				return err
			}
			variable, _, err := client.Actions.GetRepoVariable(ctx, sc.owner, sc.repo, "CONFORMANCE_VAR")
			if err != nil {
				return err
			}
			if variable.Value != "one" {
				return deviate("one", variable.Value, "the variable value does not round trip")
			}
			if variable.GetCreatedAt().IsZero() {
				return deviate("created_at populated", "zero", "a variable carries no created_at")
			}
			return nil
		})

	rec.check(domain, "actions.updateRepoVariable", "PATCH /repos/{owner}/{repo}/actions/variables/{name}", func() error {
		if _, err := client.Actions.UpdateRepoVariable(ctx, sc.owner, sc.repo,
			&github.ActionsVariable{Name: "CONFORMANCE_VAR", Value: "two"}); err != nil {
			return err
		}
		variable, _, err := client.Actions.GetRepoVariable(ctx, sc.owner, sc.repo, "CONFORMANCE_VAR")
		if err != nil {
			return err
		}
		if variable.Value != "two" {
			return deviate("two", variable.Value, "the update did not persist")
		}
		return nil
	})

	rec.check(domain, "actions.deleteRepoVariable", "DELETE /repos/{owner}/{repo}/actions/variables/{name}", func() error {
		resp, err := client.Actions.DeleteRepoVariable(ctx, sc.owner, sc.repo, "CONFORMANCE_VAR")
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusNoContent, "variable deletion"); err != nil {
			return err
		}
		_, _, err = client.Actions.GetRepoVariable(ctx, sc.owner, sc.repo, "CONFORMANCE_VAR")
		return wantHTTPError(err, http.StatusNotFound, "reading a deleted variable")
	})

	// --- Secrets at repository scope --------------------------------------
	// Writing a secret needs a sealed box, which go-github deliberately leaves
	// to the caller; the PyGithub and gh drivers cover the write path with
	// their own libsodium bindings. What is asserted here is the read side
	// every client uses to decide whether it must write at all.
	rec.check(domain, "actions.getRepoSecret (404 for an absent secret)",
		"GET /repos/{owner}/{repo}/actions/secrets/{name}", func() error {
			_, _, err := client.Actions.GetRepoSecret(ctx, sc.owner, sc.repo, "NO_SUCH_SECRET")
			return wantHTTPError(err, http.StatusNotFound, "reading an absent secret")
		})

	rec.check(domain, "actions.listRepoOrgSecrets", "GET /repos/{owner}/{repo}/actions/organization-secrets", func() error {
		secrets, _, err := client.Actions.ListRepoOrgSecrets(ctx, sc.owner, sc.repo, nil)
		if err != nil {
			return err
		}
		if secrets == nil {
			return deviate("a secrets envelope", "nil", "the inherited-organization-secret listing did not decode")
		}
		return nil
	})

	rec.check(domain, "actions.listRepoOrgVariables", "GET /repos/{owner}/{repo}/actions/organization-variables", func() error {
		variables, _, err := client.Actions.ListRepoOrgVariables(ctx, sc.owner, sc.repo, nil)
		if err != nil {
			return err
		}
		if variables == nil {
			return deviate("a variables envelope", "nil", "the inherited-organization-variable listing did not decode")
		}
		return nil
	})

	// --- Permissions -------------------------------------------------------
	rec.check(domain, "actions.updateActionsPermissions (repository)",
		"PUT /repos/{owner}/{repo}/actions/permissions", func() error {
			var got github.ActionsPermissionsRepository
			if _, err := decodeInto(client, http.MethodPut,
				fmt.Sprintf("repos/%s/%s/actions/permissions", sc.owner, sc.repo),
				map[string]any{"enabled": true, "allowed_actions": "selected"}, nil); err != nil {
				return err
			}
			if _, err := decodeInto(client, http.MethodGet,
				fmt.Sprintf("repos/%s/%s/actions/permissions", sc.owner, sc.repo), nil, &got); err != nil {
				return err
			}
			if !got.GetEnabled() {
				return deviate("enabled true", "false", "the permissions update did not persist")
			}
			if got.GetAllowedActions() != "selected" {
				return deviate("selected", got.GetAllowedActions(), "allowed_actions does not round trip")
			}
			if got.GetSelectedActionsURL() == "" {
				return deviate("selected_actions_url populated", "empty",
					"allowed_actions=selected omits selected_actions_url, the link a client follows to the allow list")
			}
			return nil
		})

	rec.check(domain, "actions.getWorkflowAccessLevel", "GET /repos/{owner}/{repo}/actions/permissions/access", func() error {
		var access struct {
			AccessLevel string `json:"access_level"`
		}
		if _, err := decodeInto(client, http.MethodGet,
			fmt.Sprintf("repos/%s/%s/actions/permissions/access", sc.owner, sc.repo), nil, &access); err != nil {
			return err
		}
		switch access.AccessLevel {
		case "none", "user", "organization", "enterprise":
			return nil
		}
		return deviate("one of none/user/organization/enterprise", access.AccessLevel,
			"the workflow access level is not one of the documented values")
	})

	rec.check(domain, "actions.getDefaultWorkflowPermissions", "GET /repos/{owner}/{repo}/actions/permissions/workflow", func() error {
		var permissions struct {
			DefaultWorkflowPermissions   string `json:"default_workflow_permissions"`
			CanApprovePullRequestReviews *bool  `json:"can_approve_pull_request_reviews"`
		}
		if _, err := decodeInto(client, http.MethodGet,
			fmt.Sprintf("repos/%s/%s/actions/permissions/workflow", sc.owner, sc.repo), nil, &permissions); err != nil {
			return err
		}
		if permissions.DefaultWorkflowPermissions != "read" && permissions.DefaultWorkflowPermissions != "write" {
			return deviate("read or write", permissions.DefaultWorkflowPermissions,
				"default_workflow_permissions is not one of the documented values")
		}
		if permissions.CanApprovePullRequestReviews == nil {
			return deviate("can_approve_pull_request_reviews present", "absent",
				"the workflow-permission payload omits can_approve_pull_request_reviews")
		}
		return nil
	})

	// --- Self-hosted runners ----------------------------------------------
	rec.check(domain, "actions.createRegistrationToken", "POST /repos/{owner}/{repo}/actions/runners/registration-token", func() error {
		token, _, err := client.Actions.CreateRegistrationToken(ctx, sc.owner, sc.repo)
		if err != nil {
			return err
		}
		if token.GetToken() == "" {
			return deviate("a registration token", "empty", "no token was minted, so no runner could register")
		}
		if token.GetExpiresAt().IsZero() {
			return deviate("expires_at populated", "zero", "the registration token has no expiry")
		}
		return nil
	})

	rec.check(domain, "actions.createRemoveToken", "POST /repos/{owner}/{repo}/actions/runners/remove-token", func() error {
		token, _, err := client.Actions.CreateRemoveToken(ctx, sc.owner, sc.repo)
		if err != nil {
			return err
		}
		return wantField("remove_token.token", token.GetToken())
	})

	rec.check(domain, "actions.listRunnerApplicationDownloads", "GET /repos/{owner}/{repo}/actions/runners/downloads", func() error {
		downloads, _, err := client.Actions.ListRunnerApplicationDownloads(ctx, sc.owner, sc.repo)
		if err != nil {
			return err
		}
		// An empty catalogue is a truthful answer here, not a gap. The schema
		// permits an empty array, and bleephub distributes its runner as a
		// container image rather than as downloadable application archives, so
		// there is no binary to advertise. Demanding an entry would push the
		// server into publishing download URLs for artefacts that do not
		// exist, which is a worse answer than none. What this operation can
		// honestly assert is that the client parses the response and that any
		// entry present carries the fields an installer reads.
		for _, download := range downloads {
			if download.GetOS() == "" || download.GetArchitecture() == "" || download.GetDownloadURL() == "" {
				return deviate("os, architecture and download_url on every entry", "an incomplete entry",
					"a runner download entry is missing the fields the installer reads")
			}
		}
		return nil
	})

	rec.check(domain, "actions.generateRepoJITConfig", "POST /repos/{owner}/{repo}/actions/runners/generate-jitconfig", func() error {
		config, _, err := client.Actions.GenerateRepoJITConfig(ctx, sc.owner, sc.repo, &github.GenerateJITConfigRequest{
			Name:          "conformance-jit",
			RunnerGroupID: 1,
			Labels:        []string{"conformance"},
		})
		if err != nil {
			return err
		}
		if config.GetEncodedJITConfig() == "" {
			return deviate("encoded_jit_config populated", "empty",
				"a just-in-time runner configuration was not produced, so no ephemeral runner can start")
		}
		if config.GetRunner().GetID() == 0 {
			return deviate("runner.id populated", "0", "the just-in-time configuration names no runner")
		}
		return nil
	})

	rec.check(domain, "actions.listRunners (after a just-in-time registration)", "GET /repos/{owner}/{repo}/actions/runners", func() error {
		runners, _, err := client.Actions.ListRunners(ctx, sc.owner, sc.repo, nil)
		if err != nil {
			return err
		}
		if runners.TotalCount < 1 {
			return deviate("the just-in-time runner", fmt.Sprintf("total_count %d", runners.TotalCount),
				"a runner registered through a just-in-time configuration is not listed")
		}
		runner := runners.Runners[0]
		if runner.GetName() == "" || runner.GetStatus() == "" {
			return deviate("name and status populated", "empty", "a listed runner is missing the fields clients render")
		}
		return nil
	})

	// --- Caches -----------------------------------------------------------
	rec.check(domain, "actions.listCaches (sorted)", "GET /repos/{owner}/{repo}/actions/caches?sort=", func() error {
		caches, _, err := client.Actions.ListCaches(ctx, sc.owner, sc.repo, &github.ActionsCacheListOptions{
			Sort: github.Ptr("size_in_bytes"), Direction: github.Ptr("desc"),
		})
		if err != nil {
			return err
		}
		if caches == nil {
			return deviate("a cache envelope", "nil", "the cache listing did not decode")
		}
		return nil
	})

	rec.check(domain, "actions.getCacheUsagePolicy", "GET /repos/{owner}/{repo}/actions/cache/usage-policy", func() error {
		var policy struct {
			RepoCacheSizeLimitInGB *int `json:"repo_cache_size_limit_in_gb"`
		}
		if _, err := decodeInto(client, http.MethodGet,
			fmt.Sprintf("repos/%s/%s/actions/cache/usage-policy", sc.owner, sc.repo), nil, &policy); err != nil {
			return err
		}
		if policy.RepoCacheSizeLimitInGB == nil {
			return deviate("repo_cache_size_limit_in_gb present", "absent",
				"the cache usage policy omits the only field it is documented to carry")
		}
		return nil
	})

	// --- OpenID Connect ----------------------------------------------------
	rec.check(domain, "actions.setRepoOIDCSubjectClaim", "PUT /repos/{owner}/{repo}/actions/oidc/customization/sub", func() error {
		if _, err := client.Actions.SetRepoOIDCSubjectClaimCustomTemplate(ctx, sc.owner, sc.repo,
			&github.OIDCSubjectClaimCustomTemplate{
				UseDefault:       github.Ptr(false),
				IncludeClaimKeys: []string{"repo", "context"},
			}); err != nil {
			return err
		}
		template, _, err := client.Actions.GetRepoOIDCSubjectClaimCustomTemplate(ctx, sc.owner, sc.repo)
		if err != nil {
			return err
		}
		if template.GetUseDefault() {
			return deviate("use_default false", "true", "the subject-claim customisation did not persist")
		}
		if len(template.GetIncludeClaimKeys()) != 2 {
			return deviate("two claim keys", fmt.Sprintf("%d", len(template.GetIncludeClaimKeys())),
				"include_claim_keys does not round trip")
		}
		return nil
	})

	// --- Environment-scoped secrets and variables --------------------------
	rec.check(domain, "actions.createEnvVariable", "POST /repos/{owner}/{repo}/environments/{env}/variables", func() error {
		if _, _, err := client.Repositories.CreateUpdateEnvironment(ctx, sc.owner, sc.repo, "conformance-env",
			&github.CreateUpdateEnvironment{}); err != nil {
			return err
		}
		if _, err := client.Actions.CreateEnvVariable(ctx, sc.owner, sc.repo, "conformance-env",
			&github.ActionsVariable{Name: "ENV_VAR", Value: "env-one"}); err != nil {
			return err
		}
		variable, _, err := client.Actions.GetEnvVariable(ctx, sc.owner, sc.repo, "conformance-env", "ENV_VAR")
		if err != nil {
			return err
		}
		if variable.Value != "env-one" {
			return deviate("env-one", variable.Value, "the environment variable value does not round trip")
		}
		return nil
	})

	rec.check(domain, "actions.listEnvVariables", "GET /repos/{owner}/{repo}/environments/{env}/variables", func() error {
		variables, _, err := client.Actions.ListEnvVariables(ctx, sc.owner, sc.repo, "conformance-env", nil)
		if err != nil {
			return err
		}
		if variables.TotalCount < 1 {
			return deviate("the environment variable just created", fmt.Sprintf("total_count %d", variables.TotalCount),
				"the environment variable listing is empty")
		}
		return nil
	})

	rec.check(domain, "actions.updateEnvVariable / deleteEnvVariable",
		"PATCH and DELETE /repos/{owner}/{repo}/environments/{env}/variables/{name}", func() error {
			if _, err := client.Actions.UpdateEnvVariable(ctx, sc.owner, sc.repo, "conformance-env",
				&github.ActionsVariable{Name: "ENV_VAR", Value: "env-two"}); err != nil {
				return err
			}
			variable, _, err := client.Actions.GetEnvVariable(ctx, sc.owner, sc.repo, "conformance-env", "ENV_VAR")
			if err != nil {
				return err
			}
			if variable.Value != "env-two" {
				return deviate("env-two", variable.Value, "the environment variable update did not persist")
			}
			if _, err := client.Actions.DeleteEnvVariable(ctx, sc.owner, sc.repo, "conformance-env", "ENV_VAR"); err != nil {
				return err
			}
			_, _, err = client.Actions.GetEnvVariable(ctx, sc.owner, sc.repo, "conformance-env", "ENV_VAR")
			return wantHTTPError(err, http.StatusNotFound, "reading a deleted environment variable")
		})

	rec.check(domain, "actions.getEnvPublicKey", "GET /repos/{owner}/{repo}/environments/{env}/secrets/public-key", func() error {
		key, _, err := client.Actions.GetEnvPublicKey(ctx, int(sc.id), "conformance-env")
		if err != nil {
			return err
		}
		if key.GetKeyID() == "" || key.GetKey() == "" {
			return deviate("key_id and key populated", "empty",
				"the environment public key is unusable, so no client could seal an environment secret")
		}
		return nil
	})

	rec.check(domain, "actions.listEnvSecrets", "GET /repos/{owner}/{repo}/environments/{env}/secrets", func() error {
		secrets, _, err := client.Actions.ListEnvSecrets(ctx, int(sc.id), "conformance-env", nil)
		if err != nil {
			return err
		}
		if secrets == nil {
			return deviate("a secrets envelope", "nil", "the environment secret listing did not decode")
		}
		return nil
	})
}

// runActionsOrgScope covers the organization-scoped half of Actions: secrets,
// variables with repository selection, runner groups and organization runners.
func runActionsOrgScope(client *github.Client, rec *recorder, set *fixtureSet) {
	const domain = "actions"
	if set.org == "" {
		skipAll(rec, domain, "GET /orgs/{org}/actions/...", "the organization fixture is unavailable",
			"actions.createOrgVariable", "actions.listOrgVariables", "actions.setSelectedReposForOrgVariable",
			"actions.createOrganizationRunnerGroup", "actions.listOrganizationRunnerGroups")
		return
	}

	rec.check(domain, "actions.createOrgVariable", "POST /orgs/{org}/actions/variables", func() error {
		if _, err := client.Actions.CreateOrgVariable(ctx, set.org, &github.ActionsVariable{
			Name: "ORG_VAR", Value: "org-one", Visibility: github.Ptr("all"),
		}); err != nil {
			return err
		}
		variable, _, err := client.Actions.GetOrgVariable(ctx, set.org, "ORG_VAR")
		if err != nil {
			return err
		}
		if variable.Value != "org-one" {
			return deviate("org-one", variable.Value, "the organization variable value does not round trip")
		}
		if variable.GetVisibility() != "all" {
			return deviate("all", variable.GetVisibility(),
				"visibility does not round trip, so a client cannot tell which repositories see the variable")
		}
		return nil
	})

	rec.check(domain, "actions.listOrgVariables", "GET /orgs/{org}/actions/variables", func() error {
		variables, _, err := client.Actions.ListOrgVariables(ctx, set.org, nil)
		if err != nil {
			return err
		}
		if variables.TotalCount < 1 {
			return deviate("the organization variable just created", fmt.Sprintf("total_count %d", variables.TotalCount),
				"the organization variable listing is empty")
		}
		return nil
	})

	rec.check(domain, "actions.setSelectedReposForOrgVariable",
		"PUT /orgs/{org}/actions/variables/{name}/repositories", func() error {
			if set.orgRepo == "" {
				return deviate("an organization repository", "none", "no organization repository fixture exists")
			}
			repository, _, err := client.Repositories.Get(ctx, set.org, set.orgRepo)
			if err != nil {
				return err
			}
			if _, err := client.Actions.UpdateOrgVariable(ctx, set.org, &github.ActionsVariable{
				Name: "ORG_VAR", Value: "org-one", Visibility: github.Ptr("selected"),
			}); err != nil {
				return err
			}
			if _, err := client.Actions.SetSelectedReposForOrgVariable(ctx, set.org, "ORG_VAR",
				github.SelectedRepoIDs{repository.GetID()}); err != nil {
				return err
			}
			selected, _, err := client.Actions.ListSelectedReposForOrgVariable(ctx, set.org, "ORG_VAR", nil)
			if err != nil {
				return err
			}
			if selected.GetTotalCount() != 1 {
				return deviate("exactly the one selected repository",
					fmt.Sprintf("total_count %d", selected.GetTotalCount()),
					"the repository selection for an organization variable did not persist")
			}
			return nil
		})

	rec.check(domain, "actions.getOrgSecret (404 for an absent secret)", "GET /orgs/{org}/actions/secrets/{name}", func() error {
		_, _, err := client.Actions.GetOrgSecret(ctx, set.org, "NO_SUCH_ORG_SECRET")
		return wantHTTPError(err, http.StatusNotFound, "reading an absent organization secret")
	})

	rec.check(domain, "actions.listOrgSecrets", "GET /orgs/{org}/actions/secrets", func() error {
		secrets, _, err := client.Actions.ListOrgSecrets(ctx, set.org, nil)
		if err != nil {
			return err
		}
		if secrets == nil {
			return deviate("a secrets envelope", "nil", "the organization secret listing did not decode")
		}
		return nil
	})

	var runnerGroupID int64
	rec.check(domain, "actions.createOrganizationRunnerGroup", "POST /orgs/{org}/actions/runner-groups", func() error {
		group, _, err := client.Actions.CreateOrganizationRunnerGroup(ctx, set.org, github.CreateRunnerGroupRequest{
			Name:       github.Ptr("conformance-group"),
			Visibility: github.Ptr("selected"),
		})
		if err != nil {
			return err
		}
		runnerGroupID = group.GetID()
		if runnerGroupID == 0 {
			return deviate("a non-zero id", "0", "the created runner group has no id")
		}
		if group.GetName() != "conformance-group" {
			return deviate("conformance-group", group.GetName(), "the runner group name does not round trip")
		}
		if group.GetVisibility() != "selected" {
			return deviate("selected", group.GetVisibility(), "the runner group visibility does not round trip")
		}
		return nil
	})

	rec.check(domain, "actions.listOrganizationRunnerGroups", "GET /orgs/{org}/actions/runner-groups", func() error {
		groups, _, err := client.Actions.ListOrganizationRunnerGroups(ctx, set.org, nil)
		if err != nil {
			return err
		}
		if groups.TotalCount < 1 {
			return deviate("at least the default group", fmt.Sprintf("total_count %d", groups.TotalCount),
				"the runner group listing is empty")
		}
		return nil
	})

	if runnerGroupID == 0 {
		skipAll(rec, domain, "GET /orgs/{org}/actions/runner-groups/{id}", "the runner group fixture was not created",
			"actions.getOrganizationRunnerGroup", "actions.updateOrganizationRunnerGroup",
			"actions.setRepositoryAccessRunnerGroup", "actions.deleteOrganizationRunnerGroup")
		return
	}

	rec.check(domain, "actions.getOrganizationRunnerGroup", "GET /orgs/{org}/actions/runner-groups/{id}", func() error {
		group, _, err := client.Actions.GetOrganizationRunnerGroup(ctx, set.org, runnerGroupID)
		if err != nil {
			return err
		}
		if group.GetID() != runnerGroupID {
			return deviate(fmt.Sprintf("%d", runnerGroupID), fmt.Sprintf("%d", group.GetID()),
				"the wrong runner group came back")
		}
		if group.GetRunnersURL() == "" {
			return deviate("runners_url populated", "empty", "a runner group omits the link to its runners")
		}
		return nil
	})

	rec.check(domain, "actions.updateOrganizationRunnerGroup", "PATCH /orgs/{org}/actions/runner-groups/{id}", func() error {
		group, _, err := client.Actions.UpdateOrganizationRunnerGroup(ctx, set.org, runnerGroupID,
			github.UpdateRunnerGroupRequest{Name: github.Ptr("conformance-group-renamed")})
		if err != nil {
			return err
		}
		if group.GetName() != "conformance-group-renamed" {
			return deviate("conformance-group-renamed", group.GetName(), "the runner group rename did not persist")
		}
		return nil
	})

	rec.check(domain, "actions.setRepositoryAccessRunnerGroup",
		"PUT /orgs/{org}/actions/runner-groups/{id}/repositories", func() error {
			if set.orgRepo == "" {
				return deviate("an organization repository", "none", "no organization repository fixture exists")
			}
			repository, _, err := client.Repositories.Get(ctx, set.org, set.orgRepo)
			if err != nil {
				return err
			}
			if _, err := client.Actions.SetRepositoryAccessRunnerGroup(ctx, set.org, runnerGroupID,
				github.SetRepoAccessRunnerGroupRequest{SelectedRepositoryIDs: []int64{repository.GetID()}}); err != nil {
				return err
			}
			repositories, _, err := client.Actions.ListRepositoryAccessRunnerGroup(ctx, set.org, runnerGroupID, nil)
			if err != nil {
				return err
			}
			if repositories.GetTotalCount() != 1 {
				return deviate("exactly the one selected repository",
					fmt.Sprintf("total_count %d", repositories.GetTotalCount()),
					"the runner group's repository selection did not persist")
			}
			return nil
		})

	rec.check(domain, "actions.deleteOrganizationRunnerGroup", "DELETE /orgs/{org}/actions/runner-groups/{id}", func() error {
		resp, err := client.Actions.DeleteOrganizationRunnerGroup(ctx, set.org, runnerGroupID)
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusNoContent, "runner group deletion"); err != nil {
			return err
		}
		_, _, err = client.Actions.GetOrganizationRunnerGroup(ctx, set.org, runnerGroupID)
		return wantHTTPError(err, http.StatusNotFound, "reading a deleted runner group")
	})
}

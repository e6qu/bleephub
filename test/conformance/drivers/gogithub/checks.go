package main

import (
	"fmt"
	"net/http"
	"time"

	github "github.com/google/go-github/v88/github"
)

// runChecks exercises the Checks API together with the older commit-status
// API. They are one domain from a client's point of view: `gh pr checks`,
// every continuous-integration dashboard and every merge gate reads check runs
// and statuses in the same breath and reconciles them against the combined
// status.
func runChecks(client *github.Client, rec *recorder, set *fixtureSet) {
	const domain = "checks"
	sc := newScratch(client, set.owner, "conformance-checks")
	if !sc.ok() || sc.sha == "" {
		skipAll(rec, domain, "POST /user/repos", "the checks repository fixture could not be provisioned",
			"checks.create", "checks.get", "checks.update", "checks.listForRef",
			"checks.listAnnotations", "checks.rerequestRun", "checks.createSuite",
			"checks.getSuite", "checks.listSuitesForRef", "checks.listForSuite",
			"checks.rerequestSuite", "checks.setSuitePreferences",
			"repos.createCommitStatus (failure)", "repos.listStatuses",
			"repos.getCombinedStatusForRef (aggregate)")
		return
	}

	var checkRunID int64
	rec.check(domain, "checks.create", "POST /repos/{owner}/{repo}/check-runs", func() error {
		run, resp, err := client.Checks.CreateCheckRun(ctx, sc.owner, sc.repo, github.CreateCheckRunOptions{
			Name:       "ci/conformance",
			HeadSHA:    sc.sha,
			Status:     github.Ptr("in_progress"),
			DetailsURL: github.Ptr("https://example.invalid/run/1"),
			ExternalID: github.Ptr("conformance-external-1"),
			StartedAt:  &github.Timestamp{Time: time.Now().UTC()},
			Output: &github.CheckRunOutput{
				Title:   github.Ptr("conformance"),
				Summary: github.Ptr("a check run created by the conformance harness"),
				Annotations: []*github.CheckRunAnnotation{{
					Path:            github.Ptr("README.md"),
					StartLine:       github.Ptr(1),
					EndLine:         github.Ptr(1),
					AnnotationLevel: github.Ptr("warning"),
					Message:         github.Ptr("conformance annotation"),
					Title:           github.Ptr("annotation title"),
				}},
			},
		})
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusCreated, "check-run creation"); err != nil {
			return err
		}
		checkRunID = run.GetID()
		if checkRunID == 0 {
			return deviate("a non-zero id", "0", "the created check run has no id")
		}
		if run.GetHeadSHA() != sc.sha {
			return deviate(sc.sha, run.GetHeadSHA(), "head_sha does not round trip")
		}
		if run.GetStatus() != "in_progress" {
			return deviate("in_progress", run.GetStatus(), "status does not round trip")
		}
		if run.GetExternalID() != "conformance-external-1" {
			return deviate("conformance-external-1", run.GetExternalID(), "external_id does not round trip")
		}
		if run.GetCheckSuite().GetID() == 0 {
			return deviate("check_suite.id populated", "absent",
				"a check run carries no check_suite, so a client cannot navigate run to suite")
		}
		return wantField("check_run.node_id", run.GetNodeID())
	})

	if checkRunID == 0 {
		skipAll(rec, domain, "GET /repos/{owner}/{repo}/check-runs/{id}", "the check run fixture was not created",
			"checks.get", "checks.update", "checks.listForRef", "checks.listAnnotations", "checks.rerequestRun")
	} else {
		rec.check(domain, "checks.get", "GET /repos/{owner}/{repo}/check-runs/{id}", func() error {
			run, _, err := client.Checks.GetCheckRun(ctx, sc.owner, sc.repo, checkRunID)
			if err != nil {
				return err
			}
			if run.GetID() != checkRunID {
				return deviate(fmt.Sprintf("%d", checkRunID), fmt.Sprintf("%d", run.GetID()), "the wrong check run came back")
			}
			if run.GetOutput().GetTitle() != "conformance" {
				return deviate("conformance", run.GetOutput().GetTitle(), "output.title was not persisted")
			}
			if run.GetOutput().GetAnnotationsCount() != 1 {
				return deviate("annotations_count 1", fmt.Sprintf("%d", run.GetOutput().GetAnnotationsCount()),
					"the annotation submitted with the check run is not counted")
			}
			return nil
		})

		rec.check(domain, "checks.update", "PATCH /repos/{owner}/{repo}/check-runs/{id}", func() error {
			run, _, err := client.Checks.UpdateCheckRun(ctx, sc.owner, sc.repo, checkRunID, github.UpdateCheckRunOptions{
				Name:        "ci/conformance",
				Status:      github.Ptr("completed"),
				Conclusion:  github.Ptr("failure"),
				CompletedAt: &github.Timestamp{Time: time.Now().UTC()},
			})
			if err != nil {
				return err
			}
			if run.GetStatus() != "completed" {
				return deviate("completed", run.GetStatus(), "the update did not change status")
			}
			if run.GetConclusion() != "failure" {
				return deviate("failure", run.GetConclusion(), "the update did not change conclusion")
			}
			if run.GetCompletedAt().IsZero() {
				return deviate("completed_at populated", "zero",
					"a completed check run carries no completed_at, which clients render as duration")
			}
			return nil
		})

		rec.check(domain, "checks.listForRef", "GET /repos/{owner}/{repo}/commits/{ref}/check-runs", func() error {
			result, _, err := client.Checks.ListCheckRunsForRef(ctx, sc.owner, sc.repo, sc.sha, nil)
			if err != nil {
				return err
			}
			if result.GetTotal() < 1 {
				return deviate("at least the check run just created", fmt.Sprintf("total_count %d", result.GetTotal()),
					"check runs are not listed for the commit they were created against")
			}
			for _, run := range result.CheckRuns {
				if run.GetID() == checkRunID {
					return nil
				}
			}
			return deviate("the created check run in the listing", "absent",
				"the check run is not returned by the per-ref listing")
		})

		rec.check(domain, "checks.listForRef (check_name filter)", "GET /commits/{ref}/check-runs?check_name=", func() error {
			result, _, err := client.Checks.ListCheckRunsForRef(ctx, sc.owner, sc.repo, sc.sha,
				&github.ListCheckRunsOptions{CheckName: github.Ptr("no-such-check")})
			if err != nil {
				return err
			}
			if result.GetTotal() != 0 {
				return deviate("total_count 0", fmt.Sprintf("%d", result.GetTotal()),
					"the check_name filter is ignored, so a client cannot select one check")
			}
			return nil
		})

		rec.check(domain, "checks.listAnnotations", "GET /repos/{owner}/{repo}/check-runs/{id}/annotations", func() error {
			annotations, _, err := client.Checks.ListCheckRunAnnotations(ctx, sc.owner, sc.repo, checkRunID, nil)
			if err != nil {
				return err
			}
			if len(annotations) != 1 {
				return deviate("1 annotation", fmt.Sprintf("%d", len(annotations)),
					"the annotation submitted with the check run is not readable back")
			}
			first := annotations[0]
			if first.GetPath() != "README.md" {
				return deviate("README.md", first.GetPath(), "annotation path does not round trip")
			}
			if first.GetStartLine() != 1 || first.GetEndLine() != 1 {
				return deviate("start_line and end_line 1",
					fmt.Sprintf("%d/%d", first.GetStartLine(), first.GetEndLine()),
					"annotation line numbers do not round trip")
			}
			if first.GetAnnotationLevel() != "warning" {
				return deviate("warning", first.GetAnnotationLevel(), "annotation_level does not round trip")
			}
			return wantField("annotation.message", first.GetMessage())
		})

		rec.check(domain, "checks.rerequestRun", "POST /repos/{owner}/{repo}/check-runs/{id}/rerequest", func() error {
			resp, err := client.Checks.ReRequestCheckRun(ctx, sc.owner, sc.repo, checkRunID)
			if err != nil {
				return err
			}
			return wantStatus(resp, http.StatusCreated, "check-run rerequest")
		})
	}

	var suiteID int64
	rec.check(domain, "checks.createSuite", "POST /repos/{owner}/{repo}/check-suites", func() error {
		suite, _, err := client.Checks.CreateCheckSuite(ctx, sc.owner, sc.repo, github.CreateCheckSuiteOptions{
			HeadSHA: sc.sha,
		})
		if err != nil {
			return err
		}
		suiteID = suite.GetID()
		if suiteID == 0 {
			return deviate("a non-zero id", "0", "the created check suite has no id")
		}
		if suite.GetHeadSHA() != sc.sha {
			return deviate(sc.sha, suite.GetHeadSHA(), "check suite head_sha does not round trip")
		}
		return wantField("check_suite.node_id", suite.GetNodeID())
	})

	if suiteID == 0 {
		skipAll(rec, domain, "GET /repos/{owner}/{repo}/check-suites/{id}", "the check suite fixture was not created",
			"checks.getSuite", "checks.listForSuite", "checks.rerequestSuite")
	} else {
		rec.check(domain, "checks.getSuite", "GET /repos/{owner}/{repo}/check-suites/{id}", func() error {
			suite, _, err := client.Checks.GetCheckSuite(ctx, sc.owner, sc.repo, suiteID)
			if err != nil {
				return err
			}
			if suite.GetID() != suiteID {
				return deviate(fmt.Sprintf("%d", suiteID), fmt.Sprintf("%d", suite.GetID()), "the wrong check suite came back")
			}
			return wantField("check_suite.status", suite.GetStatus())
		})

		rec.check(domain, "checks.listForSuite", "GET /repos/{owner}/{repo}/check-suites/{id}/check-runs", func() error {
			result, _, err := client.Checks.ListCheckRunsCheckSuite(ctx, sc.owner, sc.repo, suiteID, nil)
			if err != nil {
				return err
			}
			if result == nil {
				return deviate("a check-runs envelope", "nil", "the per-suite check run listing did not decode")
			}
			return nil
		})

		rec.check(domain, "checks.rerequestSuite", "POST /repos/{owner}/{repo}/check-suites/{id}/rerequest", func() error {
			resp, err := client.Checks.ReRequestCheckSuite(ctx, sc.owner, sc.repo, suiteID)
			if err != nil {
				return err
			}
			return wantStatus(resp, http.StatusCreated, "check-suite rerequest")
		})
	}

	rec.check(domain, "checks.listSuitesForRef", "GET /repos/{owner}/{repo}/commits/{ref}/check-suites", func() error {
		result, _, err := client.Checks.ListCheckSuitesForRef(ctx, sc.owner, sc.repo, sc.sha, nil)
		if err != nil {
			return err
		}
		if result.GetTotal() < 1 {
			return deviate("at least one check suite", fmt.Sprintf("total_count %d", result.GetTotal()),
				"no check suite is listed for a commit that has check runs")
		}
		return nil
	})

	rec.check(domain, "checks.setSuitePreferences", "PATCH /repos/{owner}/{repo}/check-suites/preferences", func() error {
		result, _, err := client.Checks.SetCheckSuitePreferences(ctx, sc.owner, sc.repo, github.CheckSuitePreferenceOptions{
			AutoTriggerChecks: []*github.AutoTriggerCheck{{AppID: github.Ptr(int64(1)), Setting: github.Ptr(false)}},
		})
		if err != nil {
			return err
		}
		if result.GetRepository().GetName() == "" {
			return deviate("repository populated", "empty",
				"the preferences response omits the repository object clients read back")
		}
		return nil
	})

	// --- The commit status API -------------------------------------------
	// Statuses and check runs are separate systems that a client reconciles
	// through the combined status, so the aggregation rule is the contract:
	// one failure makes the whole reference fail.
	rec.check(domain, "repos.createCommitStatus (failure)", "POST /repos/{owner}/{repo}/statuses/{sha}", func() error {
		status, _, err := client.Repositories.CreateStatus(ctx, sc.owner, sc.repo, sc.sha, github.RepoStatus{
			State:       github.Ptr("failure"),
			Context:     github.Ptr("conformance/failing"),
			Description: github.Ptr("deliberately failing"),
			TargetURL:   github.Ptr("https://example.invalid/status"),
		})
		if err != nil {
			return err
		}
		if status.GetState() != "failure" {
			return deviate("failure", status.GetState(), "status state does not round trip")
		}
		if status.GetContext() != "conformance/failing" {
			return deviate("conformance/failing", status.GetContext(), "status context does not round trip")
		}
		if status.GetCreator().GetLogin() == "" {
			return deviate("creator.login populated", "empty", "a commit status has no creator")
		}
		return wantField("status.node_id", status.GetNodeID())
	})

	rec.check(domain, "repos.createCommitStatus (success)", "POST /repos/{owner}/{repo}/statuses/{sha}", func() error {
		_, _, err := client.Repositories.CreateStatus(ctx, sc.owner, sc.repo, sc.sha, github.RepoStatus{
			State:   github.Ptr("success"),
			Context: github.Ptr("conformance/passing"),
		})
		return err
	})

	rec.check(domain, "repos.listStatuses", "GET /repos/{owner}/{repo}/commits/{ref}/statuses", func() error {
		statuses, _, err := client.Repositories.ListStatuses(ctx, sc.owner, sc.repo, sc.sha, nil)
		if err != nil {
			return err
		}
		if len(statuses) < 2 {
			return deviate("both statuses just created", fmt.Sprintf("%d", len(statuses)),
				"the per-reference status listing does not return every status")
		}
		return nil
	})

	rec.check(domain, "repos.getCombinedStatusForRef (aggregate)", "GET /repos/{owner}/{repo}/commits/{ref}/status", func() error {
		combined, _, err := client.Repositories.GetCombinedStatus(ctx, sc.owner, sc.repo, sc.sha, nil)
		if err != nil {
			return err
		}
		if combined.GetState() != "failure" {
			return deviate("failure", combined.GetState(),
				"one failing context must make the combined state failure; a client that merges on `success` would merge a broken commit")
		}
		if combined.GetTotalCount() < 2 {
			return deviate("total_count of at least 2", fmt.Sprintf("%d", combined.GetTotalCount()),
				"the combined status does not count every context")
		}
		if combined.GetSHA() != sc.sha {
			return deviate(sc.sha, combined.GetSHA(), "the combined status reports the wrong sha")
		}
		if combined.GetCommitURL() == "" {
			return deviate("commit_url populated", "empty",
				"the combined status omits commit_url, which the contract marks required")
		}
		return nil
	})

	rec.check(domain, "repos.getCombinedStatusForRef (required keys)",
		"GET /repos/{owner}/{repo}/commits/{ref}/status", func() error {
			// combined-commit-status marks state, sha, total_count, statuses,
			// repository, commit_url and url required. go-github's struct has
			// no field for `repository` or `url`, so they are read straight off
			// the decoded body: a required key a typed client cannot see is
			// still a key another client will miss.
			var body map[string]any
			if _, err := decodeInto(client, http.MethodGet,
				fmt.Sprintf("repos/%s/%s/commits/%s/status", sc.owner, sc.repo, sc.sha), nil, &body); err != nil {
				return err
			}
			for _, key := range []string{"state", "sha", "total_count", "statuses", "repository", "commit_url", "url"} {
				if _, present := body[key]; !present {
					return deviate("the required key "+key, "absent",
						"the combined status omits %q, which combined-commit-status marks required", key)
				}
			}
			if repository, _ := body["repository"].(map[string]any); repository["full_name"] == nil {
				return deviate("repository.full_name populated", "absent",
					"the combined status's repository object is not a repository")
			}
			return nil
		})
}

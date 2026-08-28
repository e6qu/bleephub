package bleephub

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/stretchr/testify/require"
)

const enableAutoMergeDoc = `mutation($input:EnablePullRequestAutoMergeInput!){enablePullRequestAutoMerge(input:$input){actor{login} pullRequest{autoMergeRequest{mergeMethod enabledBy{login}}}}}`
const disableAutoMergeDoc = `mutation($input:DisablePullRequestAutoMergeInput!){disablePullRequestAutoMerge(input:$input){pullRequest{autoMergeRequest{mergeMethod}}}}`

// autoMergeFixture provisions an admin repo (auto-merge allowed) whose main
// branch requires the "ci" status check with enforce_admins on, plus one open
// feat→main pull request — the blocked state auto-merge arms against.
func autoMergeFixture(t *testing.T, s *isolatedServer, name string) (*store.Repo, *store.PullRequest, string) {
	t.Helper()
	s.createTestPRRepo(t, name)
	repo := s.store.GetRepo("admin", name)
	require.NotNil(t, repo)
	resp := s.patch(t, "/api/v3/repos/admin/"+name, defaultToken, map[string]interface{}{
		"allow_auto_merge": true,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	resp = s.put(t, "/api/v3/repos/admin/"+name+"/branches/main/protection", defaultToken, map[string]interface{}{
		"required_status_checks": map[string]interface{}{"strict": false, "contexts": []string{"ci"}},
		"enforce_admins":         true,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	resp = s.post(t, "/api/v3/repos/admin/"+name+"/pulls", defaultToken, map[string]interface{}{
		"title": "auto-merge me", "head": "feat", "base": "main",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()
	pr := s.store.GetPullRequestByNumber(repo.ID, 1)
	require.NotNil(t, pr)
	headSHA := s.prHeadSha(repo, pr)
	require.NotEmpty(t, headSHA)
	return repo, pr, headSHA
}

func gqlErrs(env map[string]interface{}) []interface{} {
	errs, _ := env["errors"].([]interface{})
	return errs
}

func TestAutoMergeEnablePendingChecksThenGreenCheckMerges(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo, pr, headSHA := autoMergeFixture(t, s, "am-green")

	env := s.gqlAuthzPost(t, defaultToken, enableAutoMergeDoc, map[string]interface{}{
		"input": map[string]interface{}{
			"pullRequestId":  pr.NodeID,
			"mergeMethod":    "SQUASH",
			"commitHeadline": "auto-squashed",
		},
	})
	require.Empty(t, gqlErrs(env), "enable was refused: %v", env)

	// Armed but still open: the required check is red.
	armed := s.store.GetPullRequest(pr.ID)
	require.Equal(t, "OPEN", armed.State)
	require.NotNil(t, armed.AutoMerge)
	require.Equal(t, "SQUASH", armed.AutoMerge.MergeMethod)
	require.Equal(t, "admin", s.store.GetUserByID(armed.AutoMerge.EnabledByID).Login)

	// The REST pull payload carries the armed auto_merge object.
	rest := decodeJSON(t, s.get(t, "/api/v3/repos/"+repo.FullName+"/pulls/1", defaultToken))
	autoMerge, ok := rest["auto_merge"].(map[string]interface{})
	require.True(t, ok, "auto_merge = %v, want an object while armed", rest["auto_merge"])
	require.Equal(t, "squash", autoMerge["merge_method"])
	require.Equal(t, "auto-squashed", autoMerge["commit_title"])
	enabledBy, _ := autoMerge["enabled_by"].(map[string]interface{})
	require.Equal(t, "admin", enabledBy["login"])

	// The required check turning green is the trigger: the PR merges through
	// the standard merge path without any further API call.
	resp := s.post(t, "/api/v3/repos/"+repo.FullName+"/check-runs", defaultToken, map[string]interface{}{
		"name": "ci", "head_sha": headSHA, "status": "completed", "conclusion": "success",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	merged := s.store.GetPullRequest(pr.ID)
	require.Equal(t, "MERGED", merged.State)
	require.NotEmpty(t, merged.MergeCommitSHA)
	require.Equal(t, "admin", s.store.GetUserByID(merged.MergedByID).Login)
	require.Nil(t, merged.AutoMerge, "the armed request must be retired by the merge")

	// The standard merged timeline landed: auto_merge_enabled, then the
	// merged + closed pair the merge path records.
	var names []string
	for _, e := range s.store.ListPullRequestEvents(repo.ID, pr.ID) {
		names = append(names, e.Event)
	}
	require.Contains(t, names, "auto_merge_enabled")
	require.Contains(t, names, "merged")
	require.Contains(t, names, "closed")

	rest = decodeJSON(t, s.get(t, "/api/v3/repos/"+repo.FullName+"/pulls/1", defaultToken))
	require.Equal(t, true, rest["merged"])
	require.Nil(t, rest["auto_merge"])
}

func TestAutoMergeDisableStopsTheMerge(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo, pr, headSHA := autoMergeFixture(t, s, "am-disable")

	env := s.gqlAuthzPost(t, defaultToken, enableAutoMergeDoc, map[string]interface{}{
		"input": map[string]interface{}{"pullRequestId": pr.NodeID},
	})
	require.Empty(t, gqlErrs(env), "enable was refused: %v", env)
	require.NotNil(t, s.store.GetPullRequest(pr.ID).AutoMerge)

	env = s.gqlAuthzPost(t, defaultToken, disableAutoMergeDoc, map[string]interface{}{
		"input": map[string]interface{}{"pullRequestId": pr.NodeID},
	})
	require.Empty(t, gqlErrs(env), "disable was refused: %v", env)
	require.Nil(t, s.store.GetPullRequest(pr.ID).AutoMerge)

	// The check turning green must no longer merge anything.
	resp := s.post(t, "/api/v3/repos/"+repo.FullName+"/check-runs", defaultToken, map[string]interface{}{
		"name": "ci", "head_sha": headSHA, "status": "completed", "conclusion": "success",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()
	require.Equal(t, "OPEN", s.store.GetPullRequest(pr.ID).State)

	var names []string
	for _, e := range s.store.ListPullRequestEvents(repo.ID, pr.ID) {
		names = append(names, e.Event)
	}
	require.Contains(t, names, "auto_merge_disabled")

	// The enabled-after-green race cannot arise: with the required check
	// already green the PR could merge right now, so re-enabling is refused
	// as clean — no polling loop is needed to catch a missed trigger.
	env = s.gqlAuthzPost(t, defaultToken, enableAutoMergeDoc, map[string]interface{}{
		"input": map[string]interface{}{"pullRequestId": pr.NodeID},
	})
	errs := gqlErrs(env)
	require.NotEmpty(t, errs, "enabling with green checks must be refused: %v", env)
	require.Contains(t, fmt.Sprint(errs[0]), "clean status")
	require.Nil(t, s.store.GetPullRequest(pr.ID).AutoMerge)
}

func TestAutoMergeEnableRefusals(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)

	// Already-clean PR: auto-merge only arms while something blocks the merge.
	s.createTestPRRepo(t, "am-clean")
	cleanRepo := s.store.GetRepo("admin", "am-clean")
	resp := s.patch(t, "/api/v3/repos/admin/am-clean", defaultToken, map[string]interface{}{
		"allow_auto_merge": true,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	resp = s.post(t, "/api/v3/repos/admin/am-clean/pulls", defaultToken, map[string]interface{}{
		"title": "clean", "head": "feat", "base": "main",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()
	cleanPR := s.store.GetPullRequestByNumber(cleanRepo.ID, 1)
	env := s.gqlAuthzPost(t, defaultToken, enableAutoMergeDoc, map[string]interface{}{
		"input": map[string]interface{}{"pullRequestId": cleanPR.NodeID},
	})
	errs := gqlErrs(env)
	require.NotEmpty(t, errs, "a clean pull request armed auto-merge: %v", env)
	require.Contains(t, fmt.Sprint(errs[0]), "clean status")
	require.Nil(t, s.store.GetPullRequest(cleanPR.ID).AutoMerge)

	// Repository that does not allow auto-merge.
	_, blockedPR, _ := autoMergeFixture(t, s, "am-disallowed")
	resp = s.patch(t, "/api/v3/repos/admin/am-disallowed", defaultToken, map[string]interface{}{
		"allow_auto_merge": false,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	env = s.gqlAuthzPost(t, defaultToken, enableAutoMergeDoc, map[string]interface{}{
		"input": map[string]interface{}{"pullRequestId": blockedPR.NodeID},
	})
	errs = gqlErrs(env)
	require.NotEmpty(t, errs, "a repo with auto-merge disallowed armed it: %v", env)
	require.Contains(t, fmt.Sprint(errs[0]), "not allowed for this repository")
	require.Nil(t, s.store.GetPullRequest(blockedPR.ID).AutoMerge)

	// A collaborator without write access is refused by the policy table.
	_, gatedPR, _ := autoMergeFixture(t, s, "am-nowrite")
	reader := s.createTestUser(t, "am-reader")
	require.NotNil(t, reader)
	readerTok := s.store.CreateToken(reader.ID, "repo")
	env = s.gqlAuthzPost(t, readerTok.Value, enableAutoMergeDoc, map[string]interface{}{
		"input": map[string]interface{}{"pullRequestId": gatedPR.NodeID},
	})
	errs = gqlErrs(env)
	require.NotEmpty(t, errs, "a caller without push access armed auto-merge: %v", env)
	require.Contains(t, fmt.Sprint(errs[0]), "push access")
	require.Nil(t, s.store.GetPullRequest(gatedPR.ID).AutoMerge)

	// Disable without an armed request is refused.
	_, unarmedPR, _ := autoMergeFixture(t, s, "am-unarmed")
	env = s.gqlAuthzPost(t, defaultToken, disableAutoMergeDoc, map[string]interface{}{
		"input": map[string]interface{}{"pullRequestId": unarmedPR.NodeID},
	})
	require.NotEmpty(t, gqlErrs(env), "disabling an unarmed pull request succeeded: %v", env)
}

// TestAutoMergeApprovalTriggersMerge covers the review-shaped trigger: with a
// required approving review as the blocking condition, the approval landing
// is what merges the armed PR.
func TestAutoMergeApprovalTriggersMerge(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "am-review")
	repo := s.store.GetRepo("admin", "am-review")
	resp := s.patch(t, "/api/v3/repos/admin/am-review", defaultToken, map[string]interface{}{
		"allow_auto_merge": true,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	resp = s.put(t, "/api/v3/repos/admin/am-review/branches/main/protection", defaultToken, map[string]interface{}{
		"required_pull_request_reviews": map[string]interface{}{"required_approving_review_count": 1},
		"enforce_admins":                true,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	resp = s.post(t, "/api/v3/repos/admin/am-review/pulls", defaultToken, map[string]interface{}{
		"title": "needs a review", "head": "feat", "base": "main",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()
	pr := s.store.GetPullRequestByNumber(repo.ID, 1)

	env := s.gqlAuthzPost(t, defaultToken, enableAutoMergeDoc, map[string]interface{}{
		"input": map[string]interface{}{"pullRequestId": pr.NodeID},
	})
	require.Empty(t, gqlErrs(env), "enable was refused: %v", env)
	require.Equal(t, "OPEN", s.store.GetPullRequest(pr.ID).State)

	reviewer := s.createTestUser(t, "am-reviewer")
	require.NotNil(t, reviewer)
	reviewerTok := s.store.CreateToken(reviewer.ID, "repo")
	resp = s.post(t, "/api/v3/repos/admin/am-review/pulls/1/reviews", reviewerTok.Value, map[string]interface{}{
		"body": "LGTM", "event": "APPROVE",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	merged := s.store.GetPullRequest(pr.ID)
	require.Equal(t, "MERGED", merged.State, "the approval landing must trigger the armed merge")
	require.Nil(t, merged.AutoMerge)
}

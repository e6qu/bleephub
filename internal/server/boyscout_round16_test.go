package bleephub

import (
	"net/http"
	"testing"
)

// TestCollaboratorDefaultPermissionIsPush pins that adding a collaborator with
// no explicit permission grants push (write) — GitHub's documented default —
// not pull.
func TestCollaboratorDefaultPermissionIsPush(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.seedRepo(t, "collab-default", false)
	s.newUser(t, "collab-invitee")

	resp := s.put(t, "/api/v3/repos/admin/collab-default/collaborators/collab-invitee", defaultToken, map[string]interface{}{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add collaborator (empty body) = %d, want 201", resp.StatusCode)
	}
	inv := decodeJSON(t, resp)
	if inv["permissions"] != "write" {
		t.Fatalf("default collaborator permission = %v, want write (push)", inv["permissions"])
	}
}

// TestDeleteInstallationRevokesTokens pins that uninstalling a GitHub App
// installation immediately invalidates its installation access tokens (GitHub
// parity), rather than leaving a ghs_ token live until expiry.
func TestDeleteInstallationRevokesTokens(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	app := s.store.CreateApp(admin.ID, "Revoke Test", "", map[string]string{"contents": "read"}, nil)
	inst := s.store.CreateInstallation(app.ID, "User", admin.ID, admin.Login, map[string]string{"contents": "read"}, nil)
	tok := s.store.CreateInstallationToken(inst.ID, app.ID, map[string]string{"contents": "read"}, nil)

	s.store.Mu.RLock()
	_, present := s.store.InstallationTokens[tok.Token]
	s.store.Mu.RUnlock()
	if !present {
		t.Fatalf("installation token not stored before delete")
	}

	if !s.store.DeleteInstallation(inst.ID) {
		t.Fatalf("DeleteInstallation returned false")
	}

	s.store.Mu.RLock()
	_, stillThere := s.store.InstallationTokens[tok.Token]
	s.store.Mu.RUnlock()
	if stillThere {
		t.Fatalf("installation token survived the uninstall")
	}
}

// TestCrossRepoAddBlockedByRejected pins that the GraphQL addBlockedBy mutation
// cannot name a blocking issue in a repository the caller has no access to —
// closing an IDOR that leaked a private issue's title/body back in the response.
func TestCrossRepoAddBlockedByRejected(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	attacker := newGQLAuthzFixture(t, s.Server, "blockedby-attacker", false)
	victim := newGQLAuthzFixture(t, s.Server, "blockedby-victim", true) // private

	env := s.gqlAuthzPost(t, attacker.ownerToken,
		`mutation($input:AddBlockedByInput!){addBlockedBy(input:$input){blockingIssue{title body}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"issueId":         attacker.issue.NodeID,
			"blockingIssueId": victim.issue.NodeID,
		}})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Fatalf("cross-repo addBlockedBy leaked a private issue: %v", env)
	}
	// The link must not have been created either.
	if blockers := s.store.ListIssueBlockedBy(attacker.issue.ID); len(blockers) != 0 {
		t.Fatalf("a cross-repo block relation was persisted: %v", blockers)
	}
}

// TestGraphQLAddCommentRejectedOnLockedIssue pins that GraphQL addComment
// honors a conversation lock: a non-collaborator cannot comment on a locked
// issue (the REST path already enforces this).
func TestGraphQLAddCommentRejectedOnLockedIssue(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	repo := s.seedRepo(t, "lockable", false) // public
	issue := s.store.CreateIssue(repo.ID, admin.ID, "heated", "body", nil, nil, 0)
	s.store.Mu.Lock()
	s.store.Issues[issue.ID].Locked = true
	s.store.Mu.Unlock()

	_, strangerTok := s.newUser(t, "lock-stranger")
	env := s.gqlAuthzPost(t, strangerTok,
		`mutation($input:AddCommentInput!){addComment(input:$input){commentEdge{node{body}}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"subjectId": issue.NodeID, "body": "sneaking in",
		}})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Fatalf("non-collaborator commented on a locked issue via GraphQL: %v", env)
	}
	// The owner (push access) can still comment on the locked conversation.
	ownerEnv := s.gqlAuthzPost(t, defaultToken,
		`mutation($input:AddCommentInput!){addComment(input:$input){commentEdge{node{body}}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"subjectId": issue.NodeID, "body": "moderator note",
		}})
	if errs := gqlAuthzErrors(ownerEnv); len(errs) > 0 {
		t.Fatalf("a repo admin was blocked from commenting on a locked issue: %v", errs)
	}
}

// TestMergeQueueEnqueueRequiresApproval pins that a PR failing its required
// reviews cannot be added to the merge queue (GitHub refuses the enqueue), so
// the queue can no longer merge an unreviewed PR.
func TestMergeQueueEnqueueRequiresApproval(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestPRRepo(t, "mq-reviews")
	s.put(t, "/api/v3/repos/admin/mq-reviews/branches/main/protection", defaultToken, map[string]interface{}{
		"required_pull_request_reviews": map[string]interface{}{"required_approving_review_count": 1},
		"enforce_admins":                true,
	}).Body.Close()
	s.post(t, "/api/v3/repos/admin/mq-reviews/pulls", defaultToken, map[string]interface{}{
		"title": "unreviewed", "head": "feat", "base": "main",
	}).Body.Close()

	pr := s.store.GetPullRequestByNumber(s.store.GetRepo("admin", "mq-reviews").ID, 1)
	env := s.gqlAuthzPost(t, defaultToken,
		`mutation($input:EnqueuePullRequestInput!){enqueuePullRequest(input:$input){mergeQueueEntry{id}}}`,
		map[string]interface{}{"input": map[string]interface{}{"pullRequestId": pr.NodeID}})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Fatalf("an unreviewed PR was accepted into the merge queue: %v", env)
	}
	// It must not have merged.
	if merged := s.store.GetPullRequest(pr.ID); merged.State != "OPEN" {
		t.Fatalf("the unreviewed PR was merged from the queue: state=%s", merged.State)
	}
}

// TestRunnerVersionDeprecationSchedule pins the newly-documented
// GET .../actions/runners/deprecations/{version} operation (org + repo): it
// echoes the requested version with a null end-of-life schedule (bleephub sets
// none), and shares the two-segment runner path space without shadowing the
// runner-labels route.
func TestRunnerVersionDeprecationSchedule(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.seedRepo(t, "runner-dep", false)
	s.seedTestOrg(t, "runner-dep-org")

	for _, path := range []string{
		"/api/v3/repos/admin/runner-dep/actions/runners/deprecations/2.300.0",
		"/api/v3/orgs/runner-dep-org/actions/runners/deprecations/2.300.0",
	} {
		resp := s.get(t, path, defaultToken)
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
		}
		body := decodeJSON(t, resp)
		if body["runner_version"] != "2.300.0" {
			t.Errorf("%s: runner_version = %v, want 2.300.0", path, body["runner_version"])
		}
		if body["registration_deprecates_at"] != nil || body["runtime_deprecates_at"] != nil {
			t.Errorf("%s: expected a null deprecation schedule, got %v / %v", path,
				body["registration_deprecates_at"], body["runtime_deprecates_at"])
		}
	}

	// The runner-labels route in the same two-segment space still resolves.
	resp := s.get(t, "/api/v3/repos/admin/runner-dep/actions/runners/12345/labels", defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("labels route for a missing runner = %d, want 404 (route must still resolve)", resp.StatusCode)
	}
}

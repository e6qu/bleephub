package bleephub

import (
	"net/http"
	"strconv"
	"testing"
)

func itoa64(i int64) string { return strconv.FormatInt(i, 10) }

// Sub-resources addressed by their own global id — a check run, a check suite,
// a deployment status, a review comment, an issue comment — are reachable only
// through a repository path. Nothing about the id itself says which repository
// it belongs to, so every by-id handler has to walk back to the repository and
// compare it with the one in the URL. Where it does not, an id is a
// cross-tenant handle: the caller supplies a repository they legitimately
// control and a stranger's id, and the credential check passes because it was
// asked about the wrong repository.
//
// Each case below is the same shape: the attacker addresses the victim's
// sub-resource through the attacker's own repository. The answer must be 404 —
// not 403, which would confirm the id exists.

type crossTenantFixture struct {
	victim       *User
	victimRepo   *Repo
	victimToken  string
	attacker     *User
	attackerRepo *Repo
	attackerTok  string
}

func (s *isolatedServer) newCrossTenantFixture(t *testing.T, tag string) *crossTenantFixture {
	t.Helper()
	store := s.store
	now := fixedTestTime.UTC()

	mkUser := func(login string) *User {
		store.Mu.Lock()
		defer store.Mu.Unlock()
		u := &User{ID: store.NextUser, Login: login, Type: "User", CreatedAt: now, UpdatedAt: now}
		store.Users[u.ID] = u
		store.UsersByLogin[u.Login] = u
		store.NextUser++
		return u
	}

	f := &crossTenantFixture{
		victim:   mkUser("xtenant-victim-" + tag),
		attacker: mkUser("xtenant-attacker-" + tag),
	}
	f.victimRepo = store.CreateRepo(f.victim, "xtenant-victim-repo", "private fixture", true)
	if f.victimRepo == nil {
		t.Fatalf("could not create the victim repository")
	}
	f.attackerRepo = store.CreateRepo(f.attacker, "xtenant-attacker-repo", "attacker's own repository", false)
	if f.attackerRepo == nil {
		t.Fatalf("could not create the attacker repository")
	}
	victimTok := store.CreateToken(f.victim.ID, "repo, workflow, admin:org")
	attackerTok := store.CreateToken(f.attacker.ID, "repo, workflow, admin:org")
	if victimTok == nil || attackerTok == nil {
		t.Fatalf("could not mint fixture tokens")
	}
	f.victimToken = victimTok.Value
	f.attackerTok = attackerTok.Value
	return f
}

// assertStatus closes the response and fails unless the code matches.
func assertStatus(t *testing.T, resp *http.Response, want int, what string) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Errorf("%s: status = %d, want %d", what, resp.StatusCode, want)
	}
}

func TestCrossTenantCheckRunsAndSuitesAreNotReachableByID(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newCrossTenantFixture(t, "checks")
	store := s.store

	suite := store.CreateCheckSuite(f.victimRepo.FullName, "main", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", 0)
	if suite == nil {
		t.Fatal("could not create the victim's check suite fixture")
	}
	run := store.CreateCheckRun(f.victimRepo.FullName, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "victim-only", 0, suite.ID)
	if run == nil {
		t.Fatalf("could not create the victim's check fixtures")
	}
	victimBase := "/api/v3/repos/" + f.victimRepo.FullName
	attackerBase := "/api/v3/repos/" + f.attackerRepo.FullName
	runPath := func(base string) string { return base + "/check-runs/" + itoa64(run.ID) }
	suitePath := func(base string) string { return base + "/check-suites/" + itoa64(suite.ID) }

	// Positive control: the owner reaches its own check run and suite, so a 404
	// below means the guard fired rather than the fixture being unreachable.
	assertStatus(t, s.get(t, runPath(victimBase), f.victimToken), http.StatusOK, "owner GET check run")
	assertStatus(t, s.get(t, suitePath(victimBase), f.victimToken), http.StatusOK, "owner GET check suite")

	assertStatus(t, s.get(t, runPath(attackerBase), f.attackerTok),
		http.StatusNotFound, "cross-tenant GET check run")
	assertStatus(t, s.get(t, runPath(attackerBase)+"/annotations", f.attackerTok),
		http.StatusNotFound, "cross-tenant GET check run annotations")
	assertStatus(t, s.patch(t, runPath(attackerBase), f.attackerTok, map[string]interface{}{"name": "pwned"}),
		http.StatusNotFound, "cross-tenant PATCH check run")
	assertStatus(t, s.get(t, suitePath(attackerBase), f.attackerTok),
		http.StatusNotFound, "cross-tenant GET check suite")
	assertStatus(t, s.get(t, suitePath(attackerBase)+"/check-runs", f.attackerTok),
		http.StatusNotFound, "cross-tenant GET check suite runs")

	if got := store.GetCheckRun(run.ID); got == nil || got.Name != "victim-only" {
		t.Errorf("check run name = %q, want it untouched by the cross-tenant PATCH", got.Name)
	}
}

func TestCrossTenantDeploymentStatusIsNotReachableByID(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newCrossTenantFixture(t, "deployments")
	deployments := s.store.Deployments

	victimDep := deployments.CreateDeployment(f.victimRepo.ID, f.victim.ID, "main", "sha", "deploy", "production", "victim", nil, true, false)
	victimStatus, _ := deployments.AddStatus(victimDep.ID, f.victim.ID, "success", "victim-only", "", "", "", "production", false)
	attackerDep := deployments.CreateDeployment(f.attackerRepo.ID, f.attacker.ID, "main", "sha", "deploy", "production", "attacker", nil, true, false)
	if victimStatus == nil || attackerDep == nil {
		t.Fatalf("could not create the deployment fixtures")
	}

	victimPath := "/api/v3/repos/" + f.victimRepo.FullName +
		"/deployments/" + itoa(victimDep.ID) + "/statuses/" + itoa(victimStatus.ID)
	assertStatus(t, s.get(t, victimPath, f.victimToken), http.StatusOK, "owner GET deployment status")

	// The attacker's own repository, the victim's deployment and status ids.
	assertStatus(t, s.get(t, "/api/v3/repos/"+f.attackerRepo.FullName+
		"/deployments/"+itoa(victimDep.ID)+"/statuses/"+itoa(victimStatus.ID), f.attackerTok),
		http.StatusNotFound, "cross-tenant GET deployment status")

	// The attacker's own deployment, the victim's status id: the status must be
	// bound to the deployment in the path, not merely to a readable repository.
	assertStatus(t, s.get(t, "/api/v3/repos/"+f.attackerRepo.FullName+
		"/deployments/"+itoa(attackerDep.ID)+"/statuses/"+itoa(victimStatus.ID), f.attackerTok),
		http.StatusNotFound, "foreign status under an owned deployment")
}

func TestCrossTenantPRReviewCommentIsNotReachableByID(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newCrossTenantFixture(t, "prcomments")
	store := s.store

	seedStorePullRequestBranches(t, store, f.victimRepo, "feature")
	pr := store.CreatePullRequest(f.victimRepo.ID, f.victim.ID, "victim pr", "", "feature", "main", false, nil, nil, 0)
	if pr == nil {
		t.Fatalf("could not create the victim pull request")
	}
	comment := store.PRReviewComments.CreateRootComment(pr.ID, f.victim.ID, "a.txt", "victim-only", "sha", "RIGHT", 1, 0)
	if comment == nil {
		t.Fatalf("could not create the victim review comment")
	}

	victimPath := "/api/v3/repos/" + f.victimRepo.FullName + "/pulls/comments/" + itoa(comment.ID)
	attackerPath := "/api/v3/repos/" + f.attackerRepo.FullName + "/pulls/comments/" + itoa(comment.ID)

	assertStatus(t, s.get(t, victimPath, f.victimToken), http.StatusOK, "owner GET review comment")

	assertStatus(t, s.get(t, attackerPath, f.attackerTok),
		http.StatusNotFound, "cross-tenant GET review comment")
	assertStatus(t, s.patch(t, attackerPath, f.attackerTok, map[string]interface{}{"body": "pwned"}),
		http.StatusNotFound, "cross-tenant PATCH review comment")
	assertStatus(t, s.delete(t, attackerPath, f.attackerTok),
		http.StatusNotFound, "cross-tenant DELETE review comment")

	got := store.PRReviewComments.Get(comment.ID)
	if got == nil {
		t.Fatalf("the victim's review comment was deleted through another repository")
	}
	if got.Body != "victim-only" {
		t.Errorf("review comment body = %q, want it untouched by the cross-tenant PATCH", got.Body)
	}
}

func TestCrossTenantIssueCommentIsNotReachableByID(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newCrossTenantFixture(t, "issuecomments")
	store := s.store

	issue := store.CreateIssue(f.victimRepo.ID, f.victim.ID, "victim issue", "", nil, nil, 0)
	if issue == nil {
		t.Fatalf("could not create the victim issue")
	}
	comment := store.CreateComment(issue.ID, f.victim.ID, "victim-only")
	doomed := store.CreateComment(issue.ID, f.victim.ID, "delete-probe")
	if comment == nil || doomed == nil {
		t.Fatalf("could not create the victim comments")
	}

	victimPath := "/api/v3/repos/" + f.victimRepo.FullName + "/issues/comments/" + itoa(comment.ID)
	attackerPath := "/api/v3/repos/" + f.attackerRepo.FullName + "/issues/comments/" + itoa(comment.ID)

	assertStatus(t, s.patch(t, victimPath, f.victimToken, map[string]interface{}{"body": "owner edit"}),
		http.StatusOK, "owner PATCH issue comment")

	assertStatus(t, s.patch(t, attackerPath, f.attackerTok, map[string]interface{}{"body": "pwned"}),
		http.StatusNotFound, "cross-tenant PATCH issue comment")
	assertStatus(t, s.delete(t, "/api/v3/repos/"+f.attackerRepo.FullName+"/issues/comments/"+itoa(doomed.ID), f.attackerTok),
		http.StatusNotFound, "cross-tenant DELETE issue comment")

	if got := store.GetComment(comment.ID); got == nil || got.Body != "owner edit" {
		t.Errorf("issue comment body = %q, want it untouched by the cross-tenant PATCH", got.Body)
	}
	if store.GetComment(doomed.ID) == nil {
		t.Errorf("the victim's issue comment was deleted through another repository")
	}

	// Positive control on the delete path, so the 404 above is the guard and
	// not a route that refuses everybody.
	assertStatus(t, s.delete(t, "/api/v3/repos/"+f.victimRepo.FullName+"/issues/comments/"+itoa(doomed.ID), f.victimToken),
		http.StatusNoContent, "owner DELETE issue comment")
}

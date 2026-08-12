package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestCurrentRESTSurfacesPersistAcrossRestart covers the state introduced by
// the current GitHub REST description. A route is not complete if a successful
// mutation disappears when Bleephub restarts.
func TestCurrentRESTSurfacesPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)

	p1, err := store.NewPersistence()
	if err != nil {
		t.Fatalf("open persistence: %v", err)
	}
	st1 := store.NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatalf("attach persistence: %v", err)
	}
	st1.SeedDefaultUser()
	admin := st1.UsersByLogin["admin"]
	org := st1.CreateOrg(admin, "current-surface-org", "Current surfaces", "")
	repo := st1.CreateOrgRepo(org, admin, "current-surface-repo", "", false)

	job := st1.CreateArtifactDeploymentJob(&store.ArtifactDeploymentJob{
		OrgID: org.ID, Cluster: "cluster-a", Status: "completed", TotalCount: 2,
	})
	st1.PutCodeQualityFinding(&store.CodeQualityFinding{
		Number: 1, RepoKey: repo.FullName, State: "open",
		Rule:     store.CodeQualityFindingRule{ID: "go/no-dead-code", Title: "Dead code", Severity: "warning", Category: "maintainability"},
		Location: store.CodeQualityFindingLocation{Path: "main.go", StartLine: 4},
		Message:  store.CodeQualityFindingMessage{Text: "Unused declaration", Markdown: "Unused declaration"},
	})
	pattern := st1.CreateSecretScanningCustomPatterns(customPatternScope("repo", repo.FullName), []store.SecretScanningPatternCreate{{
		Name: "Internal token", Pattern: `bleep_[0-9a-f]{16}`,
	}})[0]
	st1.SetPRCreationCap(repo.FullName, store.PRCreationCap{Enabled: true, MaxOpenPullRequests: 7})
	st1.SetOrgPRCreationCap(org.Login, store.PRCreationCap{Enabled: true, MaxOpenPullRequests: 4})
	st1.ChangePRCreationBypass(repo.FullName, []string{admin.Login}, true)
	suggestion := st1.CreateIssueSuggestion(repo.FullName, 42, store.IssueSuggestion{
		Action: "close_issue", Confidence: stringPtr("HIGH"),
	})
	first := &store.PullRequest{ID: 101, Number: 1, RepoID: repo.ID, BaseRefName: "main", HeadRefName: "feature-a", State: "OPEN"}
	second := &store.PullRequest{ID: 102, Number: 2, RepoID: repo.ID, BaseRefName: "feature-a", HeadRefName: "feature-b", State: "OPEN"}
	stack, err := st1.CreatePullRequestStack(repo, []*store.PullRequest{first, second})
	if err != nil {
		t.Fatalf("create pull request stack: %v", err)
	}

	if err := p1.Close(); err != nil {
		t.Fatalf("close persistence: %v", err)
	}
	p2, err := store.NewPersistence()
	if err != nil {
		t.Fatalf("reopen persistence: %v", err)
	}
	defer p2.Close()
	st2 := store.NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatalf("reload persistence: %v", err)
	}

	if got := st2.GetArtifactDeploymentJob(org.ID, job.ID, "cluster-a"); got == nil || got.TotalCount != 2 {
		t.Errorf("artifact deployment job did not reload: %#v", got)
	}
	if got := st2.GetCodeQualityFinding(repo.FullName, 1); got == nil || got.Rule.ID != "go/no-dead-code" {
		t.Errorf("code quality finding did not reload: %#v", got)
	}
	patterns := st2.ListSecretScanningCustomPatterns(customPatternScope("repo", repo.FullName))
	if len(patterns) != 1 || patterns[0].ID != pattern.ID {
		t.Errorf("secret scanning custom patterns did not reload: %#v", patterns)
	}
	if st2.NextSecretScanningPatternID <= pattern.ID {
		t.Errorf("secret scanning pattern counter was not restored: %d", st2.NextSecretScanningPatternID)
	}
	if got := st2.GetPRCreationCap(repo.FullName); !got.Enabled || got.MaxOpenPullRequests != 7 {
		t.Errorf("pull request creation cap did not reload: %#v", got)
	}
	if got := st2.GetOrgPRCreationCap(org.Login); !got.Enabled || got.MaxOpenPullRequests != 4 {
		t.Errorf("org pull request creation cap did not reload: %#v", got)
	}
	users := st2.PRCreationBypassUsers(repo.FullName)
	if len(users) != 1 || users[0].Login != admin.Login {
		t.Errorf("pull request bypass list did not reload: %#v", users)
	}
	suggestions := st2.ListIssueSuggestions(repo.FullName, 42)
	if len(suggestions) != 1 || suggestions[0].ID != suggestion.ID {
		t.Errorf("issue suggestions did not reload: %#v", suggestions)
	}
	if st2.NextIssueSuggestionID <= suggestion.ID {
		t.Errorf("issue suggestion counter was not restored: %d", st2.NextIssueSuggestionID)
	}
	gotStack := st2.GetPullRequestStack(repo.FullName, stack.Number)
	if gotStack == nil || len(gotStack.PullRequests) != 2 {
		t.Errorf("pull request stack did not reload: %#v", gotStack)
	}
	if st2.NextPullRequestStackID <= stack.ID {
		t.Errorf("pull request stack counter was not restored: %d", st2.NextPullRequestStackID)
	}
	if st2.NextArtifactDeploymentJobID <= job.ID {
		t.Errorf("artifact deployment job counter was not restored: %d", st2.NextArtifactDeploymentJobID)
	}
}

func stringPtr(value string) *string {
	return &value
}

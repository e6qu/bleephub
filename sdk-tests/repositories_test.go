package sdktests

import (
	"net/http"
	"strings"
	"testing"

	github "github.com/google/go-github/v88/github"
)

// TestRepositoriesCRUD covers Create / Get / List / Edit / Delete through the
// typed client, asserting real field values round-trip.
func TestRepositoriesCRUD(t *testing.T) {
	name := uniqueName("repo-crud")

	created, _, err := client.Repositories.Create(ctx(), "", &github.Repository{
		Name:        github.Ptr(name),
		Description: github.Ptr("initial description"),
		Private:     github.Ptr(false),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.GetID() == 0 {
		t.Error("created repo has zero ID")
	}
	if created.GetName() != name {
		t.Errorf("created name = %q, want %q", created.GetName(), name)
	}
	if created.GetOwner().GetLogin() != "admin" {
		t.Errorf("owner login = %q, want admin", created.GetOwner().GetLogin())
	}
	if created.GetFullName() != "admin/"+name {
		t.Errorf("full_name = %q, want admin/%s", created.GetFullName(), name)
	}
	t.Cleanup(func() { _, _ = client.Repositories.Delete(ctx(), "admin", name) })

	// Get
	got, _, err := client.Repositories.Get(ctx(), "admin", name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GetID() != created.GetID() {
		t.Errorf("Get ID = %d, want %d", got.GetID(), created.GetID())
	}
	if got.GetDescription() != "initial description" {
		t.Errorf("Get description = %q, want %q", got.GetDescription(), "initial description")
	}

	// List by authenticated user
	repos, _, err := client.Repositories.ListByUser(ctx(), "admin", nil)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	found := false
	for _, r := range repos {
		if r.GetName() == name {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListByUser did not include %q (got %d repos)", name, len(repos))
	}

	// Edit
	edited, _, err := client.Repositories.Edit(ctx(), "admin", name, &github.Repository{
		Description: github.Ptr("edited description"),
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if edited.GetDescription() != "edited description" {
		t.Errorf("Edit description = %q, want %q", edited.GetDescription(), "edited description")
	}

	// Delete
	if _, err := client.Repositories.Delete(ctx(), "admin", name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, resp, err := client.Repositories.Get(ctx(), "admin", name)
	if err == nil {
		t.Errorf("Get after delete succeeded, want 404 (resp=%v)", resp)
	} else if resp == nil {
		t.Errorf("Get after delete returned no HTTP response, want 404 (err=%v)", err)
	} else if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Get after delete status = %d, want 404 (err=%v)", resp.StatusCode, err)
	}
}

// TestRepositoriesBranches lists branches. A freshly created repo has its
// default branch; assert that the default branch shows up.
func TestRepositoriesBranches(t *testing.T) {
	name := uniqueName("repo-branches")
	repo := createRepo(t, name)

	branches, _, err := client.Repositories.ListBranches(ctx(), "admin", name, nil)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	// bleephub may report zero branches for an empty repo (no commits yet);
	// what we assert is the call decodes cleanly into []*Branch. If branches
	// are present, the default branch must be among them.
	if def := repo.GetDefaultBranch(); def != "" && len(branches) > 0 {
		found := false
		for _, b := range branches {
			if b.GetName() == def {
				found = true
			}
			if b.GetName() == "" {
				t.Error("branch with empty name decoded")
			}
		}
		if !found {
			t.Errorf("default branch %q not in branch list", def)
		}
	}
}

// TestRepositoryCodeAndComparisonSemantics drives the commit selectors,
// pagination, ref-aware contents and comparison response through the official
// typed client. OpenAPI checks the shapes, but not that these selectors
// identify the same git graph and tree.
func TestRepositoryCodeAndComparisonSemantics(t *testing.T) {
	name := uniqueName("repo-code-semantics")
	createRepo(t, name)
	base := createSDKDefaultBranch(t, name)
	head := createSDKCommit(t, name, "add guide", "guide.md", "# Guide\n", []*github.Commit{base})
	if _, _, err := client.Git.UpdateRef(ctx(), "admin", name, "heads/main", github.UpdateRef{
		SHA: head.GetSHA(),
	}); err != nil {
		t.Fatalf("Git.UpdateRef(main): %v", err)
	}

	filtered, _, err := client.Repositories.ListCommits(ctx(), "admin", name, &github.CommitsListOptions{
		SHA:    "main",
		Path:   "guide.md",
		Author: "sdk@example.test",
	})
	if err != nil {
		t.Fatalf("Repositories.ListCommits(filtered): %v", err)
	}
	if len(filtered) != 1 || filtered[0].GetSHA() != head.GetSHA() {
		t.Fatalf("filtered commits = %+v, want only %s", filtered, head.GetSHA())
	}

	secondPage, response, err := client.Repositories.ListCommits(ctx(), "admin", name, &github.CommitsListOptions{
		SHA: "main",
		ListOptions: github.ListOptions{
			Page:    2,
			PerPage: 1,
		},
	})
	if err != nil {
		t.Fatalf("Repositories.ListCommits(page 2): %v", err)
	}
	if len(secondPage) != 1 || secondPage[0].GetSHA() != base.GetSHA() || response.PrevPage != 1 {
		t.Fatalf("page 2 = %+v response=%+v, want base %s and previous page", secondPage, response, base.GetSHA())
	}

	comparison, _, err := client.Repositories.CompareCommits(ctx(), "admin", name, base.GetSHA(), head.GetSHA(), nil)
	if err != nil {
		t.Fatalf("Repositories.CompareCommits: %v", err)
	}
	hasGuide := false
	for _, file := range comparison.Files {
		if file.GetFilename() == "guide.md" && file.GetStatus() == "added" {
			hasGuide = true
		}
	}
	if comparison.GetStatus() != "ahead" || comparison.GetAheadBy() != 1 ||
		len(comparison.Commits) != 1 || !hasGuide {
		t.Fatalf("comparison = %+v", comparison)
	}

	file, directory, _, err := client.Repositories.GetContents(
		ctx(),
		"admin",
		name,
		"guide.md",
		&github.RepositoryContentGetOptions{Ref: head.GetSHA()},
	)
	if err != nil {
		t.Fatalf("Repositories.GetContents(commit ref): %v", err)
	}
	content, err := file.GetContent()
	if err != nil || content != "# Guide\n" || len(directory) != 0 {
		t.Fatalf("contents file=%+v directory=%+v decoded=%q err=%v", file, directory, content, err)
	}
}

// TestRepositoryBranchProtectionState pins the cross-endpoint contract that
// the official client relies on after it updates a branch protection resource.
func TestRepositoryBranchProtectionState(t *testing.T) {
	name := uniqueName("repo-protected-branch")
	_, _, err := client.Repositories.Create(ctx(), "", &github.Repository{
		Name:     github.Ptr(name),
		AutoInit: github.Ptr(true),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Repositories.Delete(ctx(), "admin", name) })

	contexts := []string{"ci"}
	if _, _, err := client.Repositories.UpdateBranchProtection(
		ctx(),
		"admin",
		name,
		"main",
		&github.ProtectionRequest{
			RequiredStatusChecks: &github.RequiredStatusChecks{
				Strict: true, Contexts: &contexts,
			},
		},
	); err != nil {
		t.Fatalf("UpdateBranchProtection: %v", err)
	}

	branch, _, err := client.Repositories.GetBranch(ctx(), "admin", name, "main", 0)
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if !branch.GetProtected() {
		t.Fatal("GetBranch protected = false after UpdateBranchProtection")
	}
	if branch.Protection == nil {
		t.Fatal("GetBranch omitted protection")
	}
	if got := branch.GetProtectionURL(); !strings.Contains(got, "/api/v3/repos/admin/"+name+"/branches/main/protection") {
		t.Errorf("GetBranch protection_url = %q", got)
	}

	onlyProtected, _, err := client.Repositories.ListBranches(ctx(), "admin", name, &github.BranchListOptions{
		Protected: github.Ptr(true),
	})
	if err != nil {
		t.Fatalf("ListBranches(protected=true): %v", err)
	}
	if len(onlyProtected) != 1 || onlyProtected[0].GetName() != "main" || !onlyProtected[0].GetProtected() {
		t.Fatalf("ListBranches(protected=true) = %+v", onlyProtected)
	}

	if _, err := client.Repositories.RemoveBranchProtection(ctx(), "admin", name, "main"); err != nil {
		t.Fatalf("RemoveBranchProtection: %v", err)
	}
	branch, _, err = client.Repositories.GetBranch(ctx(), "admin", name, "main", 0)
	if err != nil {
		t.Fatalf("GetBranch after removal: %v", err)
	}
	if branch.GetProtected() {
		t.Fatal("GetBranch protected = true after RemoveBranchProtection")
	}
}

// TestRepositorySearchTopicQualifier pins query-language fidelity through the
// official client. The OpenAPI parameter is only a string named q; its schema
// cannot say that topic: is accepted or that it performs an exact topic match.
func TestRepositorySearchTopicQualifier(t *testing.T) {
	name := uniqueName("repo-topic-search")
	createRepo(t, name)
	if _, _, err := client.Repositories.ReplaceAllTopics(ctx(), "admin", name, []string{"golden-path", "banking"}); err != nil {
		t.Fatalf("ReplaceAllTopics: %v", err)
	}

	result, _, err := client.Search.Repositories(ctx(), "user:admin topic:golden-path topic:BANKING", nil)
	if err != nil {
		t.Fatalf("Search.Repositories(topic:): %v", err)
	}
	found := false
	for _, repo := range result.Repositories {
		if repo.GetFullName() == "admin/"+name {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("topic-qualified search omitted admin/%s from %+v", name, result.Repositories)
	}
}

// TestRepositorySearchExclusionAndStatusQualifiers drives the documented
// repository query grammar through go-github. OpenAPI can validate the
// envelope, but only an SDK lifecycle can pin -QUALIFIER set-complement
// semantics and combinations such as archived:false + fork:false.
func TestRepositorySearchExclusionAndStatusQualifiers(t *testing.T) {
	marker := uniqueName("repo-search-marker")
	includedName := uniqueName("repo-search-included")
	topicName := uniqueName("repo-search-topic")
	archivedName := uniqueName("repo-search-archived")
	createRepo(t, includedName)
	createRepo(t, topicName)
	createRepo(t, archivedName)

	for _, name := range []string{includedName, topicName, archivedName} {
		if _, _, err := client.Repositories.Edit(ctx(), "admin", name, &github.Repository{
			Description: github.Ptr(marker),
		}); err != nil {
			t.Fatalf("Repositories.Edit(%s): %v", name, err)
		}
	}
	if _, _, err := client.Repositories.ReplaceAllTopics(ctx(), "admin", topicName, []string{"web"}); err != nil {
		t.Fatalf("ReplaceAllTopics: %v", err)
	}
	if _, _, err := client.Repositories.Edit(ctx(), "admin", archivedName, &github.Repository{
		Archived: github.Ptr(true),
	}); err != nil {
		t.Fatalf("archive repository: %v", err)
	}

	query := marker + " user:admin -topic:web archived:false fork:false"
	result, _, err := client.Search.Repositories(ctx(), query, &github.SearchOptions{
		Sort:  "updated",
		Order: "desc",
	})
	if err != nil {
		t.Fatalf("Search.Repositories(%q): %v", query, err)
	}
	got := map[string]bool{}
	for _, repo := range result.Repositories {
		got[repo.GetName()] = true
	}
	if !got[includedName] || got[topicName] || got[archivedName] {
		t.Fatalf("combined qualifier results=%v, want only %s", got, includedName)
	}
}

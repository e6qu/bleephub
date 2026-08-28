package graphqlapi

import (
	"context"
	"testing"
)

// communityRepoFiles is one repository's worth of community-health content:
// every file the git-content-backed Repository fields read.
var communityRepoFiles = map[string]string{
	".github/CONTRIBUTING.md":    "# Contributing\n\nOpen a pull request.\n",
	".github/CODE_OF_CONDUCT.md": "# Contributor Covenant Code of Conduct\n",
	".github/SECURITY.md":        "Report privately.\n",
	".github/CODEOWNERS":         "* @admin\ndocs/ @nobody-at-all\n",
	".github/FUNDING.yml":        "github: [admin, second]\ncustom: https://pay.example.test/admin\n",
	".github/ISSUE_TEMPLATE/config.yml": "blank_issues_enabled: false\n" +
		"contact_links:\n  - name: Chat\n    url: https://chat.example.test\n    about: Ask a question\n",
	".github/ISSUE_TEMPLATE/bug.md": "---\nname: Bug report\nabout: Something is broken\ntitle: '[bug] '\n" +
		"labels: bug, triage\nassignees: admin\n---\n\nDescribe the bug.\n",
	".github/PULL_REQUEST_TEMPLATE.md": "### What changed\n",
	".gitmodules":                      "[submodule \"vendor/lib\"]\n\tpath = vendor/lib\n\turl = https://github.test/lib.git\n\tbranch = main\n",
}

// TestRepositoryCommunityFieldsReadTheRepositoryContent pins that the
// community-health family reads the repository's own default-branch files
// rather than reporting a blanket "none".
func TestRepositoryCommunityFieldsReadTheRepositoryContent(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.store.UsersByLogin["admin"]
	repo := h.store.CreateRepo(owner, "community", "", false)
	if repo == nil {
		t.Fatal("repository not created")
	}
	// The bug template names `bug` and `triage`; a new repository is seeded
	// with GitHub's default labels, which include `bug` but not `triage`, so
	// the template's label connection resolves exactly one of the two.
	if h.store.GetLabelByName(repo.ID, "bug") == nil {
		t.Fatal("the default label set does not carry `bug`")
	}
	h.commitRepoFiles(repo, communityRepoFiles)

	data := h.query(owner, `{
	  repository(owner:"admin", name:"community") {
	    isBlankIssuesEnabled
	    isSecurityPolicyEnabled
	    securityPolicyUrl
	    contributingGuidelines { body resourcePath }
	    codeOfConduct { key name }
	    fundingLinks { platform url }
	    contactLinks { name url about }
	    issueTemplates {
	      filename name about title
	      labels(first:10) { nodes { name } }
	      assignees(first:10) { nodes { login } }
	    }
	    pullRequestTemplates { filename body repository { nameWithOwner } }
	    codeowners { errors { line kind path } }
	    submodules(first:10) { totalCount nodes { name path gitUrl branch } }
	  }
	}`, nil)

	repoData, _ := at(t, data, "repository").(map[string]interface{})

	if got := repoData["isBlankIssuesEnabled"]; got != false {
		t.Errorf("isBlankIssuesEnabled = %v, want false (the template config turns it off)", got)
	}
	if got := repoData["isSecurityPolicyEnabled"]; got != true {
		t.Errorf("isSecurityPolicyEnabled = %v, want true", got)
	}
	if got, _ := repoData["securityPolicyUrl"].(string); got == "" {
		t.Error("securityPolicyUrl is empty, want the SECURITY.md blob URL")
	}

	if got := at(t, data, "repository", "contributingGuidelines", "resourcePath"); got != "/admin/community/blob/main/.github/CONTRIBUTING.md" {
		t.Errorf("contributingGuidelines.resourcePath = %v", got)
	}
	if got := at(t, data, "repository", "codeOfConduct", "key"); got != "contributor_covenant" {
		t.Errorf("codeOfConduct.key = %v, want contributor_covenant", got)
	}

	funding, _ := repoData["fundingLinks"].([]interface{})
	if len(funding) != 3 {
		t.Fatalf("fundingLinks = %#v, want the two github handles plus the custom URL", funding)
	}
	first, _ := funding[0].(map[string]interface{})
	if first["platform"] != "GITHUB" || first["url"] != "https://github.com/sponsors/admin" {
		t.Errorf("first funding link = %#v", first)
	}

	contacts, _ := repoData["contactLinks"].([]interface{})
	if len(contacts) != 1 {
		t.Fatalf("contactLinks = %#v, want one", contacts)
	}
	if contact, _ := contacts[0].(map[string]interface{}); contact["name"] != "Chat" {
		t.Errorf("contact link = %#v", contact)
	}

	templates, _ := repoData["issueTemplates"].([]interface{})
	if len(templates) != 1 {
		t.Fatalf("issueTemplates = %#v, want one", templates)
	}
	template, _ := templates[0].(map[string]interface{})
	if template["filename"] != "bug.md" || template["name"] != "Bug report" {
		t.Errorf("issue template = %#v", template)
	}
	if template["about"] != "Something is broken" || template["title"] != "[bug] " {
		t.Errorf("issue template front matter = %#v", template)
	}
	labels := at(t, template, "labels", "nodes").([]interface{})
	if len(labels) != 1 {
		t.Errorf("template labels = %#v, want only the label that exists on the repository", labels)
	}
	assignees := at(t, template, "assignees", "nodes").([]interface{})
	if len(assignees) != 1 {
		t.Fatalf("template assignees = %#v, want admin", assignees)
	}

	prTemplates, _ := repoData["pullRequestTemplates"].([]interface{})
	if len(prTemplates) != 1 {
		t.Fatalf("pullRequestTemplates = %#v, want one", prTemplates)
	}
	prTemplate, _ := prTemplates[0].(map[string]interface{})
	if prTemplate["filename"] != "PULL_REQUEST_TEMPLATE.md" {
		t.Errorf("pull-request template = %#v", prTemplate)
	}
	if got := at(t, prTemplate, "repository", "nameWithOwner"); got != "admin/community" {
		t.Errorf("pull-request template repository = %v", got)
	}

	// CODEOWNERS names one real account and one that does not exist, so
	// exactly one line is an error — the same classification the REST
	// codeowners-errors endpoint reports.
	codeownersErrors := at(t, data, "repository", "codeowners", "errors").([]interface{})
	if len(codeownersErrors) != 1 {
		t.Fatalf("codeowners errors = %#v, want one", codeownersErrors)
	}
	problem, _ := codeownersErrors[0].(map[string]interface{})
	if problem["kind"] != "Unknown owner" || problem["path"] != ".github/CODEOWNERS" {
		t.Errorf("codeowners error = %#v", problem)
	}

	if got := at(t, data, "repository", "submodules", "totalCount"); got != float64(1) {
		t.Errorf("submodules totalCount = %v, want 1", got)
	}
	submodules := at(t, data, "repository", "submodules", "nodes").([]interface{})
	submodule, _ := submodules[0].(map[string]interface{})
	if submodule["name"] != "vendor/lib" || submodule["path"] != "vendor/lib" ||
		submodule["gitUrl"] != "https://github.test/lib.git" || submodule["branch"] != "main" {
		t.Errorf("submodule = %#v", submodule)
	}
}

// TestRepositoryWithoutCommunityFilesReportsNone pins the other half of the
// contract: a repository that genuinely carries no health files answers null
// (or an empty list where GitHub's type is non-null), not an error.
func TestRepositoryWithoutCommunityFilesReportsNone(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.store.UsersByLogin["admin"]
	if h.store.CreateRepo(owner, "bare", "", false) == nil {
		t.Fatal("repository not created")
	}

	data := h.query(owner, `{
	  repository(owner:"admin", name:"bare") {
	    isBlankIssuesEnabled
	    isSecurityPolicyEnabled
	    securityPolicyUrl
	    contributingGuidelines { body }
	    codeOfConduct { key }
	    fundingLinks { platform }
	    contactLinks { name }
	    issueTemplates { filename }
	    pullRequestTemplates { filename }
	    codeowners { errors { line } }
	    submodules(first:10) { totalCount }
	  }
	}`, nil)

	repoData, _ := at(t, data, "repository").(map[string]interface{})
	for _, field := range []string{
		"securityPolicyUrl", "contributingGuidelines", "codeOfConduct",
		"contactLinks", "issueTemplates", "pullRequestTemplates", "codeowners",
	} {
		if got := repoData[field]; got != nil {
			t.Errorf("%s = %#v, want null on a repository with no such file", field, got)
		}
	}
	if got := repoData["isBlankIssuesEnabled"]; got != true {
		t.Errorf("isBlankIssuesEnabled = %v, want true (GitHub's default with no config)", got)
	}
	if got := repoData["isSecurityPolicyEnabled"]; got != false {
		t.Errorf("isSecurityPolicyEnabled = %v, want false", got)
	}
	if got, _ := repoData["fundingLinks"].([]interface{}); len(got) != 0 {
		t.Errorf("fundingLinks = %#v, want an empty list (the field is non-null)", got)
	}
	if got := at(t, data, "repository", "submodules", "totalCount"); got != float64(0) {
		t.Errorf("submodules totalCount = %v, want 0", got)
	}
}

// TestRepositoryCommunityFieldsRefuseAStranger is the authorization test for
// the git-content family: a private repository's health files, CODEOWNERS and
// submodules must not leak to an account with no access.
func TestRepositoryCommunityFieldsRefuseAStranger(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.store.UsersByLogin["admin"]
	stranger := h.user("outsider")
	repo := h.store.CreateRepo(owner, "sealed", "", true)
	if repo == nil {
		t.Fatal("repository not created")
	}
	h.commitRepoFiles(repo, communityRepoFiles)

	document := `query($owner:String!, $name:String!) {
	  repository(owner:$owner, name:$name) {
	    contributingGuidelines { body }
	    codeowners { errors { line } }
	    fundingLinks { platform }
	    submodules(first:10) { totalCount }
	  }
	}`
	variables := map[string]interface{}{"owner": "admin", "name": "sealed"}

	ownerView := h.query(owner, document, variables)
	if at(t, ownerView, "repository", "contributingGuidelines", "body") == nil {
		t.Fatal("owner cannot read the contributing guidelines of their own private repository")
	}

	// A stranger is refused the repository outright, and no content-backed
	// field answers from it.
	strangerView, errors := h.queryWithErrors(stranger, document, variables)
	if len(errors) == 0 {
		t.Fatal("a stranger's query for a private repository succeeded")
	}
	if repoData, ok := strangerView["repository"].(map[string]interface{}); ok {
		for field, value := range repoData {
			if value != nil {
				t.Errorf("stranger read %s = %#v from a private repository", field, value)
			}
		}
	}
}

// TestPrivateRepositoryContentFieldsRefuseAStrangerBehindTheSource covers the
// field-level half of the gate: the resolvers must refuse on their own, not only
// because the root field did, called with a source a stranger could hold if
// another path ever rendered one.
func TestPrivateRepositoryContentFieldsRefuseAStrangerBehindTheSource(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.store.UsersByLogin["admin"]
	stranger := h.user("outsider")
	repo := h.store.CreateRepo(owner, "sealed", "", true)
	if repo == nil {
		t.Fatal("repository not created")
	}
	h.commitRepoFiles(repo, communityRepoFiles)

	strangerCtx := context.WithValue(context.Background(), accountViewerKey{}, stranger)
	readable, err := h.res.readableRepoFromSource(strangerCtx, map[string]interface{}{"nameWithOwner": "admin/sealed"})
	if err != nil {
		t.Fatal(err)
	}
	if readable != nil {
		t.Fatal("readableRepoFromSource handed a stranger a private repository")
	}

	ownerCtx := context.WithValue(context.Background(), accountViewerKey{}, owner)
	visible, err := h.res.readableRepoFromSource(ownerCtx, map[string]interface{}{"nameWithOwner": "admin/sealed"})
	if err != nil {
		t.Fatal(err)
	}
	if visible == nil {
		t.Fatal("readableRepoFromSource refused the repository owner")
	}
}

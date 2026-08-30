package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// Refusal and entitled halves for the mutation surface in
// internal/graphqlapi/gh_mutations_*_graphql.go: a stranger must be refused by
// every row and the owner served by every row, since a guard that refuses
// everybody is not a guard. A mutation that authorizes correctly and changes
// nothing is a stub, so each family also reads the store back afterwards.

var gqlSurfaceMutationCases = []gqlMutationCase{
	{
		name: "createLabel",
		doc:  `mutation($input:CreateLabelInput!){createLabel(input:$input){label{name color}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID,
				"name":         "from-the-surface-table",
				"color":        "ededed",
			}
		},
	},
	{
		name: "updateLabel",
		doc:  `mutation($input:UpdateLabelInput!){updateLabel(input:$input){label{name color}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.label.NodeID, "color": "00ff00"}
		},
	},
	{
		name: "deleteLabel",
		doc:  `mutation($input:DeleteLabelInput!){deleteLabel(input:$input){clientMutationId}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"id": f.label.NodeID}
		},
	},
	{
		name: "updateTopics",
		doc:  `mutation($input:UpdateTopicsInput!){updateTopics(input:$input){invalidTopicNames repository{name}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID,
				"topicNames":   []interface{}{"golang", "simulator"},
			}
		},
	},
	{
		name: "acceptTopicSuggestion",
		doc:  `mutation($input:AcceptTopicSuggestionInput!){acceptTopicSuggestion(input:$input){topic{name}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"repositoryId": f.repo.NodeID, "name": "accepted-topic"}
		},
	},
	{
		name: "declineTopicSuggestion",
		doc:  `mutation($input:DeclineTopicSuggestionInput!){declineTopicSuggestion(input:$input){topic{name}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID,
				"name":         "declined-topic",
				"reason":       "NOT_RELEVANT",
			}
		},
	},
	{
		name: "archiveRepository",
		doc:  `mutation($input:ArchiveRepositoryInput!){archiveRepository(input:$input){repository{isArchived}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"repositoryId": f.repo.NodeID}
		},
	},
	{
		name: "unarchiveRepository",
		doc:  `mutation($input:UnarchiveRepositoryInput!){unarchiveRepository(input:$input){repository{isArchived}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"repositoryId": f.repo.NodeID}
		},
	},
	{
		name: "updateRepository",
		doc:  `mutation($input:UpdateRepositoryInput!){updateRepository(input:$input){repository{description}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID,
				"description":  "edited through the mutation surface",
			}
		},
	},
	{
		name: "updateRepositoryWebCommitSignoffSetting",
		doc:  `mutation($input:UpdateRepositoryWebCommitSignoffSettingInput!){updateRepositoryWebCommitSignoffSetting(input:$input){message repository{webCommitSignoffRequired}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId":             f.repo.NodeID,
				"webCommitSignoffRequired": true,
			}
		},
	},
}

func TestGraphQLSurfaceMutationsRefuseAnUnrelatedAuthenticatedUser(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlSurfaceMutationCases {
		f := newGQLAuthzFixture(t, s.Server, "surface-stranger-"+tc.name, true)
		if tc.setup != nil {
			tc.setup(t, s, f)
		}
		env := s.gqlAuthzPost(t, f.strangerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if len(gqlAuthzErrors(env)) == 0 {
			t.Errorf("%s: an account with no access to the repository was served: %v", tc.name, env)
		}
		s.assertGQLSurfaceFixtureUntouched(t, tc.name, f)
	}
}

// assertGQLSurfaceFixtureUntouched re-reads the records this table's mutations
// address, so a refusal reported after the write landed still fails.
func (s *isolatedServer) assertGQLSurfaceFixtureUntouched(t *testing.T, what string, f *gqlAuthzFixture) {
	t.Helper()
	repo := s.store.GetRepoByID(f.repo.ID)
	switch {
	case repo == nil:
		t.Errorf("%s: the repository disappeared", what)
		return
	case repo.Archived:
		t.Errorf("%s: the repository was archived by a stranger", what)
	case len(repo.Topics) != 0:
		t.Errorf("%s: the repository was topic-tagged by a stranger: %v", what, repo.Topics)
	case len(repo.DeclinedTopics) != 0:
		t.Errorf("%s: a topic was declined by a stranger: %v", what, repo.DeclinedTopics)
	case repo.Description != "":
		t.Errorf("%s: the repository description was edited by a stranger: %q", what, repo.Description)
	case repo.WebCommitSignoffRequired:
		t.Errorf("%s: web commit signoff was demanded by a stranger", what)
	}
	label := s.store.GetLabel(f.label.ID)
	switch {
	case label == nil:
		t.Errorf("%s: the label was deleted by a stranger", what)
	case label.Color != "d73a4a":
		t.Errorf("%s: the label was recoloured by a stranger: %q", what, label.Color)
	}
	if extra := s.store.GetLabelByName(f.repo.ID, "from-the-surface-table"); extra != nil {
		t.Errorf("%s: a stranger created a label", what)
	}
}

func TestGraphQLSurfaceMutationsStillServeTheirEntitledCaller(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlSurfaceMutationCases {
		f := newGQLAuthzFixture(t, s.Server, "surface-owner-"+tc.name, true)
		if tc.setup != nil {
			tc.setup(t, s, f)
		}
		env := s.gqlAuthzPost(t, f.ownerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Errorf("%s: the repository owner was refused: %v", tc.name, errs)
		}
	}
}

// behavioural: labels

func TestGraphQLLabelMutationsWriteTheStore(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "label-crud", false)

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:CreateLabelInput!){createLabel(input:$input){label{id name color description}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"repositoryId": f.repo.NodeID,
			"name":         "needs-triage",
			"color":        "#fbca04",
			"description":  "waiting on a maintainer",
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("createLabel: %v", errs)
	}
	created := s.store.GetLabelByName(f.repo.ID, "needs-triage")
	if created == nil {
		t.Fatalf("createLabel returned a payload but the store holds no such label")
	}
	// GitHub stores the colour without the leading hash either way, so the REST
	// and GraphQL label shapes agree.
	if created.Color != "fbca04" {
		t.Errorf("created colour = %q, want fbca04", created.Color)
	}
	if created.Description != "waiting on a maintainer" {
		t.Errorf("created description = %q", created.Description)
	}

	// A second label of the same name is refused rather than silently merged.
	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:CreateLabelInput!){createLabel(input:$input){label{name}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"repositoryId": f.repo.NodeID, "name": "needs-triage", "color": "ededed",
		}})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("createLabel accepted a duplicate name")
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UpdateLabelInput!){updateLabel(input:$input){label{name color description}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"id": created.NodeID, "name": "triaged", "color": "0e8a16",
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("updateLabel: %v", errs)
	}
	updated := s.store.GetLabel(created.ID)
	switch {
	case updated == nil:
		t.Fatalf("updateLabel lost the label")
	case updated.Name != "triaged":
		t.Errorf("name = %q, want triaged", updated.Name)
	case updated.Color != "0e8a16":
		t.Errorf("colour = %q, want 0e8a16", updated.Color)
	case updated.Description != "waiting on a maintainer":
		// An omitted member leaves the stored value alone rather than blanking it.
		t.Errorf("description = %q, want the original", updated.Description)
	}

	// Deleting an attached label must also detach it, the store primitive the
	// REST delete relies on.
	s.store.SetIssueLabels(f.repo.ID, f.issue.Number, []int{updated.ID}, f.owner.ID)
	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:DeleteLabelInput!){deleteLabel(input:$input){clientMutationId}}`,
		map[string]interface{}{"input": map[string]interface{}{"id": updated.NodeID}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("deleteLabel: %v", errs)
	}
	if s.store.GetLabel(updated.ID) != nil {
		t.Errorf("deleteLabel returned a payload but the label is still stored")
	}
	if issue := s.store.GetIssue(f.issue.ID); issue == nil || len(issue.LabelIDs) != 0 {
		t.Errorf("deleteLabel left the label attached to the issue: %+v", issue)
	}
}

// behavioural: topics

func TestGraphQLUpdateTopicsWritesValidNamesAndReportsTheRest(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "topics", false)

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UpdateTopicsInput!){updateTopics(input:$input){invalidTopicNames repository{repositoryTopics(first:10){nodes{topic{name}}}}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"repositoryId": f.repo.NodeID,
			"topicNames":   []interface{}{"Go", "web server", "graphql"},
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("updateTopics: %v", errs)
	}
	repo := s.store.GetRepoByID(f.repo.ID)
	if repo == nil {
		t.Fatalf("the repository disappeared")
	}
	// Names normalise to lower case as on github.com; the one carrying a space is
	// reported back rather than stored.
	if len(repo.Topics) != 2 || repo.Topics[0] != "go" || repo.Topics[1] != "graphql" {
		t.Errorf("stored topics = %v, want [go graphql]", repo.Topics)
	}
	invalid := nestedStringSlice(t, env, "data", "updateTopics", "invalidTopicNames")
	if len(invalid) != 1 || invalid[0] != "web server" {
		t.Errorf("invalidTopicNames = %v, want [\"web server\"]", invalid)
	}
}

func TestGraphQLTopicSuggestionMutationsRecordTheDecision(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "topic-suggestions", false)

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:AcceptTopicSuggestionInput!){acceptTopicSuggestion(input:$input){topic{name}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"repositoryId": f.repo.NodeID, "name": "Distributed-Systems",
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("acceptTopicSuggestion: %v", errs)
	}
	if repo := s.store.GetRepoByID(f.repo.ID); repo == nil || len(repo.Topics) != 1 || repo.Topics[0] != "distributed-systems" {
		t.Fatalf("accepting a suggestion did not add the topic: %+v", repo)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:DeclineTopicSuggestionInput!){declineTopicSuggestion(input:$input){topic{name}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"repositoryId": f.repo.NodeID, "name": "distributed-systems", "reason": "TOO_GENERAL",
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("declineTopicSuggestion: %v", errs)
	}
	repo := s.store.GetRepoByID(f.repo.ID)
	if repo == nil {
		t.Fatalf("the repository disappeared")
	}
	if len(repo.Topics) != 0 {
		t.Errorf("declining left the topic on the repository: %v", repo.Topics)
	}
	if len(repo.DeclinedTopics) != 1 || repo.DeclinedTopics[0] != "distributed-systems" {
		t.Errorf("declined topics = %v, want [distributed-systems]", repo.DeclinedTopics)
	}
}

// behavioural: repository settings

func TestGraphQLUpdateRepositoryWritesEverySettingItAccepts(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "repo-settings", false)

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UpdateRepositoryInput!){updateRepository(input:$input){repository{`+
			`name description homepageUrl hasIssuesEnabled hasWikiEnabled hasProjectsEnabled `+
			`hasDiscussionsEnabled hasPullRequestsEnabled hasSponsorshipsEnabled isTemplate}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"repositoryId":              f.repo.NodeID,
			"name":                      "gqlauthz-repo-renamed",
			"description":               "a renamed repository",
			"homepageUrl":               "https://example.invalid/home",
			"hasIssuesEnabled":          false,
			"hasWikiEnabled":            false,
			"hasProjectsEnabled":        false,
			"hasDiscussionsEnabled":     true,
			"hasPullRequestsEnabled":    false,
			"hasSponsorshipsEnabled":    false,
			"template":                  true,
			"issueCreationPolicy":       "COLLABORATORS_ONLY",
			"pullRequestCreationPolicy": "COLLABORATORS_ONLY",
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("updateRepository: %v", errs)
	}
	repo := s.store.GetRepoByID(f.repo.ID)
	if repo == nil {
		t.Fatalf("the repository disappeared")
	}
	switch {
	case repo.Name != "gqlauthz-repo-renamed":
		t.Errorf("name = %q, want the renamed one", repo.Name)
	case repo.Description != "a renamed repository":
		t.Errorf("description = %q", repo.Description)
	case repo.Homepage != "https://example.invalid/home":
		t.Errorf("homepage = %q", repo.Homepage)
	case repo.HasIssues || repo.HasWiki || repo.HasProjects || repo.HasPullRequests:
		t.Errorf("a feature switch stayed on: %+v", repo)
	case !store.RepoHasDiscussions(repo):
		t.Errorf("discussions stayed off")
	case repo.HasSponsorships == nil || *repo.HasSponsorships:
		t.Errorf("sponsorships = %v, want an explicit false", repo.HasSponsorships)
	case !repo.IsTemplate:
		t.Errorf("the repository is not a template")
	case repo.IssueCreationPolicy != "collaborators_only":
		t.Errorf("issue creation policy = %q", repo.IssueCreationPolicy)
	case repo.PullRequestCreationPolicy != "collaborators_only":
		t.Errorf("pull request creation policy = %q", repo.PullRequestCreationPolicy)
	}
	// The rename went through the same helper PATCH /repos/{o}/{r} uses, so the
	// repository answers to its new name.
	if s.store.GetRepo(f.owner.Login, "gqlauthz-repo-renamed") == nil {
		t.Errorf("the renamed repository is not reachable by its new name")
	}
}

func TestGraphQLArchiveAndUnarchiveRepository(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "archival", false)

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:ArchiveRepositoryInput!){archiveRepository(input:$input){repository{isArchived archivedAt}}}`,
		map[string]interface{}{"input": map[string]interface{}{"repositoryId": f.repo.NodeID}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("archiveRepository: %v", errs)
	}
	repo := s.store.GetRepoByID(f.repo.ID)
	if repo == nil || !repo.Archived || repo.ArchivedAt == nil {
		t.Fatalf("archiveRepository did not archive: %+v", repo)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UnarchiveRepositoryInput!){unarchiveRepository(input:$input){repository{isArchived}}}`,
		map[string]interface{}{"input": map[string]interface{}{"repositoryId": f.repo.NodeID}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("unarchiveRepository: %v", errs)
	}
	repo = s.store.GetRepoByID(f.repo.ID)
	if repo == nil || repo.Archived || repo.ArchivedAt != nil {
		t.Fatalf("unarchiveRepository did not unarchive: %+v", repo)
	}
}

func TestGraphQLUpdateRepositoryWebCommitSignoffSetting(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "signoff", false)

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UpdateRepositoryWebCommitSignoffSettingInput!){`+
			`updateRepositoryWebCommitSignoffSetting(input:$input){message repository{webCommitSignoffRequired}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"repositoryId": f.repo.NodeID, "webCommitSignoffRequired": true,
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("updateRepositoryWebCommitSignoffSetting: %v", errs)
	}
	if repo := s.store.GetRepoByID(f.repo.ID); repo == nil || !repo.WebCommitSignoffRequired {
		t.Fatalf("the setting was not written: %+v", repo)
	}
}

// nestedStringSlice reads a [String!] member out of a GraphQL envelope by path.
func nestedStringSlice(t *testing.T, env map[string]interface{}, path ...string) []string {
	t.Helper()
	var cursor interface{} = env
	for _, step := range path {
		object, ok := cursor.(map[string]interface{})
		if !ok {
			t.Fatalf("path %v: %q is not an object in %v", path, step, env)
		}
		cursor = object[step]
	}
	items, ok := cursor.([]interface{})
	if !ok {
		t.Fatalf("path %v is not a list in %v", path, env)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, _ := item.(string)
		out = append(out, text)
	}
	return out
}

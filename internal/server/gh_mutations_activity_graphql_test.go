package bleephub

import (
	"testing"
)

// The refusal and entitled halves for the activity and account-policy family
// (internal/graphqlapi/gh_mutations_activity_graphql.go), followed by
// behavioural tests that read the store back — a mutation that authorizes
// correctly and writes nothing is a stub.

// gqlActivityMutationCases are the rows whose subject is a repository or the
// viewer's own account, driven over the private-repository fixture.
var gqlActivityMutationCases = []gqlMutationCase{
	{
		name: "addStar",
		doc:  `mutation($input:AddStarInput!){addStar(input:$input){starrable{... on Repository{stargazerCount}}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"starrableId": f.repo.NodeID}
		},
	},
	{
		name: "removeStar",
		doc:  `mutation($input:RemoveStarInput!){removeStar(input:$input){starrable{... on Repository{stargazerCount}}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"starrableId": f.repo.NodeID}
		},
	},
	{
		name: "updateSubscription",
		doc:  `mutation($input:UpdateSubscriptionInput!){updateSubscription(input:$input){subscribable{... on Repository{viewerSubscription}}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{"subscribableId": f.repo.NodeID, "state": "SUBSCRIBED"}
		},
	},
	{
		name: "setRepositoryInteractionLimit",
		doc:  `mutation($input:SetRepositoryInteractionLimitInput!){setRepositoryInteractionLimit(input:$input){repository{name}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"repositoryId": f.repo.NodeID,
				"limit":        "COLLABORATORS_ONLY",
				"expiry":       "ONE_WEEK",
			}
		},
	},
	{
		// The subject is the caller's own account, so the refusing half names
		// the repository owner's account with the stranger's credential: a
		// mutation that let one account set another's limit would be an
		// account takeover of the other's moderation settings.
		name: "setUserInteractionLimit",
		doc:  `mutation($input:SetUserInteractionLimitInput!){setUserInteractionLimit(input:$input){user{login}}}`,
		input: func(f *gqlAuthzFixture) map[string]interface{} {
			return map[string]interface{}{
				"userId": f.owner.NodeID,
				"limit":  "EXISTING_USERS",
				"expiry": "ONE_DAY",
			}
		},
	},
}

func TestGraphQLActivityMutationsRefuseAnUnrelatedAuthenticatedUser(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlActivityMutationCases {
		f := newGQLAuthzFixture(t, s.Server, "activity-stranger-"+tc.name, true)
		env := s.gqlAuthzPost(t, f.strangerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if len(gqlAuthzErrors(env)) == 0 {
			t.Errorf("%s: an account with no access was served: %v", tc.name, env)
		}
		repo := s.store.GetRepoByID(f.repo.ID)
		switch {
		case repo == nil:
			t.Errorf("%s: the repository disappeared", tc.name)
			continue
		case repo.InteractionLimit != "":
			t.Errorf("%s: a stranger set an interaction limit: %q", tc.name, repo.InteractionLimit)
		}
		if s.store.IsRepoStarredBy(f.stranger.ID, f.owner.Login, f.repo.Name) {
			t.Errorf("%s: a stranger starred a repository they cannot read", tc.name)
		}
		if s.store.GetRepoSubscription(f.stranger.ID, f.repo.ID) != nil {
			t.Errorf("%s: a stranger subscribed to a repository they cannot read", tc.name)
		}
		if owner := s.store.GetUserByID(f.owner.ID); owner == nil || owner.InteractionLimit != "" {
			t.Errorf("%s: a stranger set the owner's account interaction limit: %+v", tc.name, owner)
		}
	}
}

func TestGraphQLActivityMutationsStillServeTheirEntitledCaller(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlActivityMutationCases {
		f := newGQLAuthzFixture(t, s.Server, "activity-owner-"+tc.name, true)
		env := s.gqlAuthzPost(t, f.ownerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Errorf("%s: the entitled caller was refused: %v", tc.name, errs)
		}
	}
}

// gqlActivityOrgMutationCases are the rows whose subject is an organization.
// The refusing caller owns a different organization, so the refusal is
// cross-tenant rather than a bare lack of standing anywhere.
var gqlActivityOrgMutationCases = []gqlIssueOrgCase{
	{
		name: "setOrganizationInteractionLimit",
		doc:  `mutation($input:SetOrganizationInteractionLimitInput!){setOrganizationInteractionLimit(input:$input){organization{login}}}`,
		input: func(f *gqlIssueOrgFixture) map[string]interface{} {
			return map[string]interface{}{
				"organizationId": f.org.NodeID,
				"limit":          "CONTRIBUTORS_ONLY",
				"expiry":         "ONE_MONTH",
			}
		},
	},
	{
		name: "removeOutsideCollaborator",
		doc:  `mutation($input:RemoveOutsideCollaboratorInput!){removeOutsideCollaborator(input:$input){removedUser{login}}}`,
		input: func(f *gqlIssueOrgFixture) map[string]interface{} {
			return map[string]interface{}{"organizationId": f.org.NodeID, "userId": f.stranger.NodeID}
		},
	},
	{
		name: "updateOrganizationAllowPrivateRepositoryForkingSetting",
		doc: `mutation($input:UpdateOrganizationAllowPrivateRepositoryForkingSettingInput!){` +
			`updateOrganizationAllowPrivateRepositoryForkingSetting(input:$input){message organization{login}}}`,
		input: func(f *gqlIssueOrgFixture) map[string]interface{} {
			return map[string]interface{}{"organizationId": f.org.NodeID, "forkingEnabled": true}
		},
	},
	{
		name: "updateOrganizationWebCommitSignoffSetting",
		doc: `mutation($input:UpdateOrganizationWebCommitSignoffSettingInput!){` +
			`updateOrganizationWebCommitSignoffSetting(input:$input){message organization{login}}}`,
		input: func(f *gqlIssueOrgFixture) map[string]interface{} {
			return map[string]interface{}{"organizationId": f.org.NodeID, "webCommitSignoffRequired": true}
		},
	},
}

func TestGraphQLActivityOrgMutationsRefuseAnotherOrganizationsOwner(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlActivityOrgMutationCases {
		f := newGQLIssueOrgFixture(t, s, "activity-refuse-"+tc.name)
		if !s.store.AddRepoCollaborator(f.org.Login, f.repo.Name, f.stranger.Login, "push") {
			t.Fatalf("%s: could not seed the outside collaborator", tc.name)
		}
		env := s.gqlAuthzPost(t, f.strangerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if len(gqlAuthzErrors(env)) == 0 {
			t.Errorf("%s: the owner of another organization was served: %v", tc.name, env)
		}
		org := s.store.GetOrgByID(f.org.ID)
		switch {
		case org == nil:
			t.Errorf("%s: the organization disappeared", tc.name)
			continue
		case org.WebCommitSignoffRequired:
			t.Errorf("%s: a stranger demanded web commit signoff", tc.name)
		case org.MembersCanForkPrivateRepositories != nil && *org.MembersCanForkPrivateRepositories:
			t.Errorf("%s: a stranger enabled private repository forking", tc.name)
		}
		if s.store.GetOrgInteractionLimit(f.org.Login) != nil {
			t.Errorf("%s: a stranger set the organization's interaction limit", tc.name)
		}
		if len(s.store.ListOutsideCollaborators(f.org.Login)) != 1 {
			t.Errorf("%s: a stranger removed the outside collaborator", tc.name)
		}
	}
}

func TestGraphQLActivityOrgMutationsStillServeTheOrganizationOwner(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlActivityOrgMutationCases {
		f := newGQLIssueOrgFixture(t, s, "activity-serve-"+tc.name)
		if !s.store.AddRepoCollaborator(f.org.Login, f.repo.Name, f.stranger.Login, "push") {
			t.Fatalf("%s: could not seed the outside collaborator", tc.name)
		}
		env := s.gqlAuthzPost(t, f.ownerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Errorf("%s: the organization's owner was refused: %v", tc.name, errs)
		}
	}
}

// --- behavioural ------------------------------------------------------------

func TestGraphQLStarMutationsWriteTheStarStore(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "stars", false)

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:AddStarInput!){addStar(input:$input){starrable{... on Repository{`+
			`stargazerCount viewerHasStarred stargazers(first:10){totalCount nodes{login} edges{cursor starredAt}}}}}}`,
		map[string]interface{}{"input": map[string]interface{}{"starrableId": f.repo.NodeID}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("addStar: %v", errs)
	}
	if !s.store.IsRepoStarredBy(f.owner.ID, f.owner.Login, f.repo.Name) {
		t.Fatalf("addStar returned a payload but the store records no star")
	}
	// The payload re-reads the repository, so the count it reports is the one
	// the star store now holds rather than the pre-mutation snapshot.
	if got := nestedInt(t, env, "data", "addStar", "starrable", "stargazerCount"); got != 1 {
		t.Errorf("stargazerCount = %d, want 1", got)
	}
	if got := nestedInt(t, env, "data", "addStar", "starrable", "stargazers", "totalCount"); got != 1 {
		t.Errorf("stargazers.totalCount = %d, want 1", got)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:RemoveStarInput!){removeStar(input:$input){starrable{... on Repository{stargazerCount}}}}`,
		map[string]interface{}{"input": map[string]interface{}{"starrableId": f.repo.NodeID}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("removeStar: %v", errs)
	}
	if s.store.IsRepoStarredBy(f.owner.ID, f.owner.Login, f.repo.Name) {
		t.Fatalf("removeStar returned a payload but the star is still recorded")
	}
}

func TestGraphQLUpdateSubscriptionWritesTheWatchRecord(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "subscription", false)
	doc := `mutation($input:UpdateSubscriptionInput!){updateSubscription(input:$input){subscribable{... on Repository{viewerSubscription}}}}`

	env := s.gqlAuthzPost(t, f.ownerToken, doc,
		map[string]interface{}{"input": map[string]interface{}{"subscribableId": f.repo.NodeID, "state": "SUBSCRIBED"}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("updateSubscription SUBSCRIBED: %v", errs)
	}
	sub := s.store.GetRepoSubscription(f.owner.ID, f.repo.ID)
	if sub == nil || !sub.Subscribed || sub.Ignored {
		t.Fatalf("SUBSCRIBED did not write a watch record: %+v", sub)
	}

	env = s.gqlAuthzPost(t, f.ownerToken, doc,
		map[string]interface{}{"input": map[string]interface{}{"subscribableId": f.repo.NodeID, "state": "IGNORED"}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("updateSubscription IGNORED: %v", errs)
	}
	sub = s.store.GetRepoSubscription(f.owner.ID, f.repo.ID)
	if sub == nil || sub.Subscribed || !sub.Ignored {
		t.Fatalf("IGNORED did not write a mute record: %+v", sub)
	}

	env = s.gqlAuthzPost(t, f.ownerToken, doc,
		map[string]interface{}{"input": map[string]interface{}{"subscribableId": f.repo.NodeID, "state": "UNSUBSCRIBED"}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("updateSubscription UNSUBSCRIBED: %v", errs)
	}
	if sub := s.store.GetRepoSubscription(f.owner.ID, f.repo.ID); sub != nil {
		t.Fatalf("UNSUBSCRIBED left a watch record: %+v", sub)
	}
}

func TestGraphQLInteractionLimitMutationsWriteTheSameRecordsREST(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "limits", false)

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:SetRepositoryInteractionLimitInput!){setRepositoryInteractionLimit(input:$input){repository{name}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"repositoryId": f.repo.NodeID, "limit": "COLLABORATORS_ONLY", "expiry": "ONE_WEEK",
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("setRepositoryInteractionLimit: %v", errs)
	}
	repo := s.store.GetRepoByID(f.repo.ID)
	switch {
	case repo == nil:
		t.Fatalf("the repository disappeared")
	case repo.InteractionLimit != "collaborators_only":
		// The stored spelling is the one GET /repos/{o}/{r}/interaction-limits
		// reports, so a GraphQL write and a REST read cannot disagree.
		t.Errorf("stored limit = %q, want collaborators_only", repo.InteractionLimit)
	case repo.InteractionLimitExpiry == nil:
		t.Errorf("the limit was stored with no expiry")
	}

	// NO_LIMIT is GitHub's spelling of the DELETE route, so it clears rather
	// than storing a fourth group.
	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:SetRepositoryInteractionLimitInput!){setRepositoryInteractionLimit(input:$input){repository{name}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"repositoryId": f.repo.NodeID, "limit": "NO_LIMIT",
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("setRepositoryInteractionLimit NO_LIMIT: %v", errs)
	}
	if repo := s.store.GetRepoByID(f.repo.ID); repo == nil || repo.InteractionLimit != "" {
		t.Errorf("NO_LIMIT did not clear the limit: %+v", repo)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:SetUserInteractionLimitInput!){setUserInteractionLimit(input:$input){user{login}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"userId": f.owner.NodeID, "limit": "EXISTING_USERS", "expiry": "THREE_DAYS",
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("setUserInteractionLimit: %v", errs)
	}
	if limit, _ := s.store.GetUserInteractionLimit(f.owner.ID); limit != "existing_users" {
		t.Errorf("account limit = %q, want existing_users", limit)
	}
}

func TestGraphQLOrganizationActivityMutationsWriteTheStore(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLIssueOrgFixture(t, s, "org-activity")
	if !s.store.AddRepoCollaborator(f.org.Login, f.repo.Name, f.stranger.Login, "push") {
		t.Fatalf("could not seed the outside collaborator")
	}

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:SetOrganizationInteractionLimitInput!){setOrganizationInteractionLimit(input:$input){organization{login}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"organizationId": f.org.NodeID, "limit": "CONTRIBUTORS_ONLY", "expiry": "SIX_MONTHS",
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("setOrganizationInteractionLimit: %v", errs)
	}
	limit := s.store.GetOrgInteractionLimit(f.org.Login)
	if limit == nil || limit.Limit != "contributors_only" {
		t.Fatalf("the organization limit was not written: %+v", limit)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UpdateOrganizationWebCommitSignoffSettingInput!){`+
			`updateOrganizationWebCommitSignoffSetting(input:$input){message organization{login}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"organizationId": f.org.NodeID, "webCommitSignoffRequired": true,
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("updateOrganizationWebCommitSignoffSetting: %v", errs)
	}
	if org := s.store.GetOrgByID(f.org.ID); org == nil || !org.WebCommitSignoffRequired {
		t.Fatalf("web commit signoff was not written: %+v", org)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UpdateOrganizationAllowPrivateRepositoryForkingSettingInput!){`+
			`updateOrganizationAllowPrivateRepositoryForkingSetting(input:$input){message organization{login}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"organizationId": f.org.NodeID, "forkingEnabled": true,
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("updateOrganizationAllowPrivateRepositoryForkingSetting: %v", errs)
	}
	org := s.store.GetOrgByID(f.org.ID)
	if org == nil || org.MembersCanForkPrivateRepositories == nil || !*org.MembersCanForkPrivateRepositories {
		t.Fatalf("private repository forking was not enabled: %+v", org)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:RemoveOutsideCollaboratorInput!){removeOutsideCollaborator(input:$input){removedUser{login}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"organizationId": f.org.NodeID, "userId": f.stranger.NodeID,
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("removeOutsideCollaborator: %v", errs)
	}
	if collaborators := s.store.ListOutsideCollaborators(f.org.Login); len(collaborators) != 0 {
		t.Errorf("the outside collaborator survived: %+v", collaborators)
	}
	// The removal goes through the same store primitive DELETE
	// /orgs/{org}/outside_collaborators/{user} uses, so the collaborator row
	// is gone from the repository as well as from the organization listing.
	if s.store.GetRepoCollaboratorPermission(f.org.Login, f.repo.Name, f.stranger.Login) != "" {
		t.Errorf("the collaborator still holds push on the repository")
	}
}

// nestedInt reads an Int member out of a GraphQL response envelope by path.
func nestedInt(t *testing.T, env map[string]interface{}, path ...string) int {
	t.Helper()
	var cursor interface{} = env
	for _, step := range path {
		object, ok := cursor.(map[string]interface{})
		if !ok {
			t.Fatalf("path %v: %q is not an object in %v", path, step, env)
		}
		cursor = object[step]
	}
	number, ok := cursor.(float64)
	if !ok {
		t.Fatalf("path %v is not a number in %v", path, env)
	}
	return int(number)
}

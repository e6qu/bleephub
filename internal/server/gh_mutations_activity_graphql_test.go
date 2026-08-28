package bleephub

import (
	"testing"
	"time"
)

// Refusal and entitled halves for the activity and account-policy family, plus
// behavioural tests that read the store back — a mutation that authorizes but
// writes nothing is a stub.

// gqlActivityMutationCases are the rows whose subject is a repository or the viewer's own account.
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
		// The subject is the caller's own account, so the refusing half aims the stranger's credential at the owner's account: letting one account set another's limit would be a takeover of its moderation settings.
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

// gqlActivityOrgMutationCases are the org-subject rows; the refusing caller owns a different org, making the refusal cross-tenant rather than a bare lack of standing.
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
	// The payload re-reads the repository, so its count is the star store's current one, not the pre-mutation snapshot.
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
		// The stored spelling is the one GET /repos/{o}/{r}/interaction-limits reports, so a GraphQL write and a REST read cannot disagree.
		t.Errorf("stored limit = %q, want collaborators_only", repo.InteractionLimit)
	case repo.InteractionLimitExpiry == nil:
		t.Errorf("the limit was stored with no expiry")
	}

	// NO_LIMIT is GitHub's spelling of the DELETE route, so it clears rather than storing a fourth group.
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
	// The removal uses the same store primitive as DELETE /orgs/{org}/outside_collaborators/{user}, so the collaborator is gone from the repository too, not just the org listing.
	if s.store.GetRepoCollaboratorPermission(f.org.Login, f.repo.Name, f.stranger.Login) != "" {
		t.Errorf("the collaborator still holds push on the repository")
	}
}

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

// TestGraphQLStarredAtReportsTheRealStarInstant pins GQL-093: the non-null
// starredAt on Repository.stargazers and User.starredRepositories is the instant
// the user starred the repo, not the repository's createdAt stand-in.
func TestGraphQLStarredAtReportsTheRealStarInstant(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAuthzFixture(t, s.Server, "star-time", false)

	// Freeze the clock to an instant distinct from the repo's creation; the edges must report this instant, not createdAt.
	starInstant := time.Date(2021, 6, 15, 12, 0, 0, 0, time.UTC)
	prev := s.replaceClockNow(func() time.Time { return starInstant })
	t.Cleanup(func() { s.replaceClockNow(prev) })
	if !s.store.StarRepo(f.owner.ID, f.owner.Login, f.repo.Name) {
		t.Fatal("StarRepo failed")
	}
	const want = "2021-06-15T12:00:00Z"

	env := s.gqlAuthzPost(t, f.ownerToken,
		`query($o:String!,$n:String!){repository(owner:$o,name:$n){createdAt stargazers(first:10){edges{starredAt node{login}}}}}`,
		map[string]interface{}{"o": f.owner.Login, "n": f.repo.Name})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("stargazers query: %v", errs)
	}
	repoMap := starTestObj(t, env, "data", "repository")
	if repoMap["createdAt"] == want {
		t.Fatalf("test setup: repo createdAt coincides with starInstant; choose a distinct instant")
	}
	if got := starTestFirstEdge(t, repoMap, "stargazers")["starredAt"]; got != want {
		t.Errorf("Repository.stargazers edge starredAt = %v, want %s", got, want)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`query($l:String!){user(login:$l){starredRepositories(first:10){edges{starredAt node{name}}}}}`,
		map[string]interface{}{"l": f.owner.Login})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("starredRepositories query: %v", errs)
	}
	userMap := starTestObj(t, env, "data", "user")
	if got := starTestFirstEdge(t, userMap, "starredRepositories")["starredAt"]; got != want {
		t.Errorf("User.starredRepositories edge starredAt = %v, want %s", got, want)
	}
}

func starTestObj(t *testing.T, m map[string]interface{}, path ...string) map[string]interface{} {
	t.Helper()
	cur := m
	for _, k := range path {
		next, ok := cur[k].(map[string]interface{})
		if !ok {
			t.Fatalf("path %v: %q is not an object (%T)", path, k, cur[k])
		}
		cur = next
	}
	return cur
}

func starTestFirstEdge(t *testing.T, obj map[string]interface{}, connField string) map[string]interface{} {
	t.Helper()
	conn, ok := obj[connField].(map[string]interface{})
	if !ok {
		t.Fatalf("%q is not a connection: %T", connField, obj[connField])
	}
	edges, ok := conn["edges"].([]interface{})
	if !ok || len(edges) == 0 {
		t.Fatalf("%q has no edges: %v", connField, conn)
	}
	edge, ok := edges[0].(map[string]interface{})
	if !ok {
		t.Fatalf("%q edge[0] not an object: %T", connField, edges[0])
	}
	return edge
}

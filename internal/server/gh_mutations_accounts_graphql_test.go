package bleephub

import (
	"fmt"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// The refusal and entitled halves for the account-scoped mutations.
//
// These name no repository, so the refusal that matters is a different one:
// for the mutations that change the viewer's own account it is a credential
// that does not carry a grant over that account (a fine-grained token
// belonging to somebody else), and for the ones that name an existing list or
// an organization it is an account with no standing over it. Both refusals are
// exercised here; the entitled half is the account's own session.

// gqlAccountFixture is an account with a list and an organization, plus a
// second account with no relationship to either and a fine-grained token that
// reaches only its own resource owner.
type gqlAccountFixture struct {
	owner         *store.User
	ownerToken    string
	stranger      *store.User
	strangerToken string
	// strangerFineGrained is a token of the stranger's whose resource owner is
	// the stranger, so it grants nothing over the owner's account and nothing
	// over the owner's organization.
	strangerFineGrained string
	org                 *store.Org
	list                *store.UserList
	repo                *store.Repo
	target              *store.User
}

func newGQLAccountFixture(t *testing.T, s *isolatedServer, tag string) *gqlAccountFixture {
	t.Helper()
	st := s.store
	now := fixedTestTime.UTC()

	mkUser := func(login string) *store.User {
		st.Mu.Lock()
		defer st.Mu.Unlock()
		u := &store.User{
			ID:        st.NextUser,
			NodeID:    fmt.Sprintf("U_acct%08d", st.NextUser),
			Login:     login,
			Type:      "User",
			CreatedAt: now,
			UpdatedAt: now,
		}
		st.Users[u.ID] = u
		st.UsersByLogin[u.Login] = u
		st.NextUser++
		return u
	}

	f := &gqlAccountFixture{}
	f.owner = mkUser("gqlacct-owner-" + tag)
	f.stranger = mkUser("gqlacct-stranger-" + tag)
	f.target = mkUser("gqlacct-target-" + tag)

	f.org = st.CreateOrg(f.owner, "gqlacct-org-"+tag, "Fixture Org", "")
	if f.org == nil {
		t.Fatalf("fixture %s: could not create the organization", tag)
	}
	f.repo = st.CreateRepo(f.owner, "gqlacct-repo", "", false)
	if f.repo == nil {
		t.Fatalf("fixture %s: could not create the repository", tag)
	}
	f.list = st.CreateUserList(f.owner.ID, "Fixture List", "seeded", false)
	if f.list == nil {
		t.Fatalf("fixture %s: could not create the list", tag)
	}

	ownerTok := st.CreateToken(f.owner.ID, "repo")
	strangerTok := st.CreateToken(f.stranger.ID, "repo")
	fineGrained := st.CreateToken(f.stranger.ID, "")
	if ownerTok == nil || strangerTok == nil || fineGrained == nil {
		t.Fatalf("fixture %s: could not mint tokens", tag)
	}
	st.Mu.Lock()
	fineGrained.FineGrained = true
	fineGrained.FineGrainedID = f.stranger.ID
	fineGrained.ResourceOwner = f.stranger.Login
	fineGrained.RepositorySelection = "none"
	st.Mu.Unlock()

	f.ownerToken = ownerTok.Value
	f.strangerToken = strangerTok.Value
	f.strangerFineGrained = fineGrained.Value
	return f
}

// gqlAccountMutationCase is one row of the account-scoped surface. refuseWith
// selects which of the fixture's two refusing credentials the mutation is
// refused by: a mutation that acts on the viewer's own account is refused by a
// credential without the grant, and one that names another account's record is
// refused by any stranger.
type gqlAccountMutationCase struct {
	name       string
	doc        string
	input      func(f *gqlAccountFixture) map[string]interface{}
	byStranger bool
}

var gqlAccountMutationCases = []gqlAccountMutationCase{
	{
		name: "followUser",
		doc:  `mutation($input:FollowUserInput!){followUser(input:$input){user{login}}}`,
		input: func(f *gqlAccountFixture) map[string]interface{} {
			return map[string]interface{}{"userId": f.target.NodeID}
		},
	},
	{
		name: "unfollowUser",
		doc:  `mutation($input:UnfollowUserInput!){unfollowUser(input:$input){user{login}}}`,
		input: func(f *gqlAccountFixture) map[string]interface{} {
			return map[string]interface{}{"userId": f.target.NodeID}
		},
	},
	{
		name: "followOrganization",
		doc:  `mutation($input:FollowOrganizationInput!){followOrganization(input:$input){organization{login}}}`,
		input: func(f *gqlAccountFixture) map[string]interface{} {
			return map[string]interface{}{"organizationId": f.org.NodeID}
		},
	},
	{
		name: "unfollowOrganization",
		doc:  `mutation($input:UnfollowOrganizationInput!){unfollowOrganization(input:$input){organization{login}}}`,
		input: func(f *gqlAccountFixture) map[string]interface{} {
			return map[string]interface{}{"organizationId": f.org.NodeID}
		},
	},
	{
		name: "changeUserStatus",
		doc:  `mutation($input:ChangeUserStatusInput!){changeUserStatus(input:$input){status{message}}}`,
		input: func(f *gqlAccountFixture) map[string]interface{} {
			return map[string]interface{}{"message": "reviewing pull requests", "emoji": ":eyes:"}
		},
	},
	{
		name: "createUserList",
		doc:  `mutation($input:CreateUserListInput!){createUserList(input:$input){list{name slug}}}`,
		input: func(f *gqlAccountFixture) map[string]interface{} {
			return map[string]interface{}{"name": "Reading list", "description": "later"}
		},
	},
	{
		name: "updateUserListsForItem",
		doc:  `mutation($input:UpdateUserListsForItemInput!){updateUserListsForItem(input:$input){lists{name}}}`,
		input: func(f *gqlAccountFixture) map[string]interface{} {
			return map[string]interface{}{
				"itemId":  f.repo.NodeID,
				"listIds": []interface{}{f.list.NodeID},
			}
		},
	},
	{
		name:       "updateUserList",
		byStranger: true,
		doc:        `mutation($input:UpdateUserListInput!){updateUserList(input:$input){list{name}}}`,
		input: func(f *gqlAccountFixture) map[string]interface{} {
			return map[string]interface{}{"listId": f.list.NodeID, "name": "Renamed list"}
		},
	},
	{
		name:       "deleteUserList",
		byStranger: true,
		doc:        `mutation($input:DeleteUserListInput!){deleteUserList(input:$input){user{login}}}`,
		input: func(f *gqlAccountFixture) map[string]interface{} {
			return map[string]interface{}{"listId": f.list.NodeID}
		},
	},
	{
		name:       "updateNotificationRestrictionSetting",
		byStranger: true,
		doc: `mutation($input:UpdateNotificationRestrictionSettingInput!){` +
			`updateNotificationRestrictionSetting(input:$input){owner{... on Organization{login}}}}`,
		input: func(f *gqlAccountFixture) map[string]interface{} {
			return map[string]interface{}{"ownerId": f.org.NodeID, "settingValue": "ENABLED"}
		},
	},
}

func TestGraphQLAccountMutationsRefuseACredentialWithoutStanding(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlAccountMutationCases {
		f := newGQLAccountFixture(t, s, "refuse-"+tc.name)
		token := f.strangerFineGrained
		if tc.byStranger {
			token = f.strangerToken
		}
		env := s.gqlAuthzPost(t, token, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if len(gqlAuthzErrors(env)) == 0 {
			t.Errorf("%s: a credential with no standing was served: %v", tc.name, env)
		}
		// The refusal has to be a refusal, not an error reported after the
		// write landed.
		if list := s.store.GetUserList(f.list.ID); list == nil || list.Name != "Fixture List" {
			t.Errorf("%s: the list was changed or destroyed: %+v", tc.name, list)
		}
		if org := s.store.GetOrgByID(f.org.ID); org == nil || org.NotificationDeliveryRestrictionEnabled {
			t.Errorf("%s: the organization's notification restriction was switched on", tc.name)
		}
		if status := s.store.GetUserStatus(f.stranger.ID); status != nil {
			t.Errorf("%s: a status was written for the refused caller: %+v", tc.name, status)
		}
		if s.store.LoginFollows(f.stranger.Login, f.target.Login) {
			t.Errorf("%s: a follow edge was written for the refused caller", tc.name)
		}
	}
}

func TestGraphQLAccountMutationsStillServeTheirEntitledCaller(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlAccountMutationCases {
		f := newGQLAccountFixture(t, s, "serve-"+tc.name)
		env := s.gqlAuthzPost(t, f.ownerToken, tc.doc, map[string]interface{}{"input": tc.input(f)})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Errorf("%s: the account's own session was refused: %v", tc.name, errs)
		}
	}
}

// behavioural

func TestGraphQLFollowMutationsWriteTheFollowGraph(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAccountFixture(t, s, "follow-graph")

	post := func(doc string, input map[string]interface{}) {
		t.Helper()
		env := s.gqlAuthzPost(t, f.ownerToken, doc, map[string]interface{}{"input": input})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Fatalf("%s: %v", doc, errs)
		}
	}

	post(`mutation($input:FollowUserInput!){followUser(input:$input){user{login}}}`,
		map[string]interface{}{"userId": f.target.NodeID})
	if !s.store.LoginFollows(f.owner.Login, f.target.Login) {
		t.Errorf("followUser did not record the edge")
	}
	// The REST follow surface reads the same graph, so the edge is visible
	// through GET /user/following/{username} as well.
	resp := s.get(t, "/api/v3/user/following/"+f.target.Login, f.ownerToken)
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("REST following check = %d, want 204", resp.StatusCode)
	}

	post(`mutation($input:UnfollowUserInput!){unfollowUser(input:$input){user{login}}}`,
		map[string]interface{}{"userId": f.target.NodeID})
	if s.store.LoginFollows(f.owner.Login, f.target.Login) {
		t.Errorf("unfollowUser left the edge in place")
	}

	post(`mutation($input:FollowOrganizationInput!){followOrganization(input:$input){organization{login}}}`,
		map[string]interface{}{"organizationId": f.org.NodeID})
	if !s.store.LoginFollows(f.owner.Login, f.org.Login) {
		t.Errorf("followOrganization did not record the edge")
	}
	post(`mutation($input:UnfollowOrganizationInput!){unfollowOrganization(input:$input){organization{login}}}`,
		map[string]interface{}{"organizationId": f.org.NodeID})
	if s.store.LoginFollows(f.owner.Login, f.org.Login) {
		t.Errorf("unfollowOrganization left the edge in place")
	}
}

func TestGraphQLChangeUserStatusWritesAndClearsTheStatus(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAccountFixture(t, s, "status")
	expiry := fixedTestTime.Add(time.Hour).UTC().Format(time.RFC3339)

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:ChangeUserStatusInput!){changeUserStatus(input:$input){status{`+
			`id emoji message indicatesLimitedAvailability expiresAt organization{login} user{login}}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"emoji":               ":palm_tree:",
			"message":             "on holiday",
			"limitedAvailability": true,
			"expiresAt":           expiry,
			"organizationId":      f.org.NodeID,
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("changeUserStatus: %v", errs)
	}
	status := s.store.GetUserStatus(f.owner.ID)
	switch {
	case status == nil:
		t.Fatalf("changeUserStatus returned a payload but stored no status")
	case status.Message != "on holiday":
		t.Errorf("message = %q", status.Message)
	case status.Emoji != ":palm_tree:":
		t.Errorf("emoji = %q", status.Emoji)
	case !status.LimitedAvailability:
		t.Errorf("the busy flag was not stored")
	case status.ExpiresAt == nil:
		t.Errorf("the expiry was not stored")
	case status.OrganizationID != f.org.ID:
		t.Errorf("organization = %d, want %d", status.OrganizationID, f.org.ID)
	}

	// A status with neither emoji nor message takes the status down, which is
	// how the profile editor clears one.
	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:ChangeUserStatusInput!){changeUserStatus(input:$input){status{message}}}`,
		map[string]interface{}{"input": map[string]interface{}{}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("clearing changeUserStatus: %v", errs)
	}
	if status := s.store.GetUserStatus(f.owner.ID); status != nil {
		t.Errorf("the status was not cleared: %+v", status)
	}
}

func TestGraphQLUserListMutationsWriteTheStore(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAccountFixture(t, s, "lists")

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:CreateUserListInput!){createUserList(input:$input){list{id name slug isPrivate description}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"name": "Go Tooling", "description": "worth reading", "isPrivate": true,
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("createUserList: %v", errs)
	}
	nodeID := nestedString(t, env, "data", "createUserList", "list", "id")
	created := store.FindUserListByNodeID(s.store, nodeID)
	if created == nil {
		t.Fatalf("createUserList returned a payload but stored no list")
	}
	if created.Slug != "go-tooling" {
		t.Errorf("slug = %q, want go-tooling", created.Slug)
	}
	if !created.IsPrivate {
		t.Errorf("the list was not stored private")
	}

	// The same name twice is refused: the two would share a slug and the
	// second would be unreachable.
	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:CreateUserListInput!){createUserList(input:$input){list{id}}}`,
		map[string]interface{}{"input": map[string]interface{}{"name": "go tooling"}})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Errorf("createUserList accepted a colliding slug")
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UpdateUserListInput!){updateUserList(input:$input){list{name slug isPrivate description}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"listId": nodeID, "name": "Go Libraries", "isPrivate": false,
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("updateUserList: %v", errs)
	}
	updated := s.store.GetUserList(created.ID)
	switch {
	case updated == nil:
		t.Fatalf("updateUserList lost the list")
	case updated.Name != "Go Libraries":
		t.Errorf("name = %q", updated.Name)
	case updated.Slug != "go-libraries":
		t.Errorf("slug = %q, want the renamed slug", updated.Slug)
	case updated.IsPrivate:
		t.Errorf("the list stayed private")
	case updated.Description != "worth reading":
		t.Errorf("an omitted member blanked the description: %q", updated.Description)
	}

	// Putting the repository on the list and then taking it off again both go
	// through updateUserListsForItem's replace-the-set semantics.
	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UpdateUserListsForItemInput!){updateUserListsForItem(input:$input){`+
			`item{... on Repository{nameWithOwner}} lists{name} user{login}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"itemId": f.repo.NodeID, "listIds": []interface{}{nodeID},
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("updateUserListsForItem: %v", errs)
	}
	if list := s.store.GetUserList(created.ID); list == nil || len(list.RepoIDs) != 1 || list.RepoIDs[0] != f.repo.ID {
		t.Fatalf("the repository was not put on the list: %+v", list)
	}
	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UpdateUserListsForItemInput!){updateUserListsForItem(input:$input){lists{name}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"itemId": f.repo.NodeID, "listIds": []interface{}{},
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("updateUserListsForItem (clearing): %v", errs)
	}
	if list := s.store.GetUserList(created.ID); list == nil || len(list.RepoIDs) != 0 {
		t.Fatalf("the repository was not taken off the list: %+v", list)
	}

	env = s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:DeleteUserListInput!){deleteUserList(input:$input){user{login}}}`,
		map[string]interface{}{"input": map[string]interface{}{"listId": nodeID}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("deleteUserList: %v", errs)
	}
	if s.store.GetUserList(created.ID) != nil {
		t.Errorf("deleteUserList returned a payload but the list is still stored")
	}
}

func TestGraphQLUpdateNotificationRestrictionSettingWritesTheOwner(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGQLAccountFixture(t, s, "notif")

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:UpdateNotificationRestrictionSettingInput!){`+
			`updateNotificationRestrictionSetting(input:$input){owner{... on Organization{`+
			`login notificationDeliveryRestrictionEnabledSetting}}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"ownerId": f.org.NodeID, "settingValue": "ENABLED",
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("updateNotificationRestrictionSetting: %v", errs)
	}
	if org := s.store.GetOrgByID(f.org.ID); org == nil || !org.NotificationDeliveryRestrictionEnabled {
		t.Fatalf("the setting was not written: %+v", org)
	}
	if got := nestedString(t, env, "data", "updateNotificationRestrictionSetting", "owner",
		"notificationDeliveryRestrictionEnabledSetting"); got != "ENABLED" {
		t.Errorf("payload setting = %q, want ENABLED", got)
	}
}

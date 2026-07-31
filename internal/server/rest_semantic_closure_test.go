package bleephub

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestPublicUserOrganizationsExcludeConcealedMemberships(t *testing.T) {
	s := newTestServer()
	s.registerGHOrgRoutes()

	admin := s.store.LookupUserByLogin("admin")
	subject := seedTestUser(s, "public-org-subject")
	hidden := s.store.CreateOrg(admin, "concealed-membership-org", "", "")
	public := s.store.CreateOrg(admin, "public-membership-org", "", "")
	s.store.SetMembership(hidden.Login, subject.ID, OrgRoleMember, MembershipStateActive)
	s.store.SetMembership(public.Login, subject.ID, OrgRoleMember, MembershipStateActive)
	if !s.store.SetMembershipPublic(public.Login, subject.ID, true) {
		t.Fatal("publicize membership")
	}

	got := tokenRequest(s, http.MethodGet, "/api/v3/users/"+subject.Login+"/orgs", "")
	if got.Code != http.StatusOK {
		t.Fatalf("public user organizations = %d: %s", got.Code, got.Body.String())
	}
	var orgs []map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &orgs); err != nil {
		t.Fatal(err)
	}
	if len(orgs) != 1 || orgs[0]["login"] != public.Login {
		t.Fatalf("public user organizations = %v, want only %s", orgs, public.Login)
	}

	subjectToken := s.store.CreateToken(subject.ID, "read:org")
	mine := tokenRequest(s, http.MethodGet, "/api/v3/user/orgs", subjectToken.Value)
	if mine.Code != http.StatusOK {
		t.Fatalf("authenticated user organizations = %d: %s", mine.Code, mine.Body.String())
	}
	orgs = nil
	if err := json.Unmarshal(mine.Body.Bytes(), &orgs); err != nil {
		t.Fatal(err)
	}
	if len(orgs) != 2 {
		t.Fatalf("authenticated user organizations = %v, want public and concealed", orgs)
	}
}

func TestMemberRepositoryCreationHonorsOrganizationPolicy(t *testing.T) {
	s := newTestServer()
	s.registerGHOrgRoutes()

	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "member-create-policy", "", "")
	member := seedTestUser(s, "member-create-policy-user")
	s.store.SetMembership(org.Login, member.ID, OrgRoleMember, MembershipStateActive)
	memberToken := s.store.CreateToken(member.ID, "repo")
	disabled := false
	s.store.mu.Lock()
	org.MembersCanCreateRepositories = &disabled
	s.store.mu.Unlock()

	denied := teamCredentialRequest(t, s, http.MethodPost,
		"/api/v3/orgs/"+org.Login+"/repos", memberToken.Value, `{"name":"denied"}`)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("member repository create = %d: %s", denied.Code, denied.Body.String())
	}
	allowed := teamCredentialRequest(t, s, http.MethodPost,
		"/api/v3/orgs/"+org.Login+"/repos", AdminToken(), `{"name":"owner-allowed"}`)
	if allowed.Code != http.StatusCreated {
		t.Fatalf("owner repository create = %d: %s", allowed.Code, allowed.Body.String())
	}
}

func TestChildTeamsRequireOrganizationReach(t *testing.T) {
	s := newTestServer()
	s.registerGHTeamRoutes()

	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "private-team-tree", "", "")
	parent := s.store.CreateTeam(org.Login, "Secret Parent", TeamOptions{Privacy: TeamPrivacySecret})
	s.store.CreateTeam(org.Login, "Secret Child", TeamOptions{Privacy: TeamPrivacySecret, ParentID: parent.ID})
	outsider := seedTestUser(s, "private-team-tree-outsider")
	outsiderToken := s.store.CreateToken(outsider.ID, "read:org")

	path := "/api/v3/orgs/" + org.Login + "/teams/" + parent.Slug + "/teams"
	if got := tokenRequest(s, http.MethodGet, path, outsiderToken.Value); got.Code != http.StatusNotFound {
		t.Fatalf("outsider child-team listing = %d: %s", got.Code, got.Body.String())
	}

	app := s.store.CreateApp(admin.ID, "Team Tree Reader", "", map[string]string{"members": "read"}, nil)
	installation := s.store.CreateInstallation(app.ID, "Organization", org.ID, org.Login, app.Permissions, nil)
	appToken := s.store.CreateInstallationToken(installation.ID, app.ID, installation.Permissions, nil)
	if got := tokenRequest(s, http.MethodGet, path, appToken.Token); got.Code != http.StatusOK {
		t.Fatalf("members:read child-team listing = %d: %s", got.Code, got.Body.String())
	}
}

func TestStarredRepositoryListingsRespectViewerReach(t *testing.T) {
	s := newTestServer()
	s.registerGHRepoRoutes()

	admin := s.store.LookupUserByLogin("admin")
	privateRepo := s.store.CreateRepo(admin, "private-star", "", true)
	subject := seedTestUser(s, "private-star-subject")
	if !s.store.StarRepo(subject.ID, privateRepo.Owner.Login, privateRepo.Name) {
		t.Fatal("star private repository fixture")
	}
	path := "/api/v3/users/" + subject.Login + "/starred"

	got := tokenRequest(s, http.MethodGet, path, "")
	if got.Code != http.StatusOK || got.Body.String() != "[]\n" {
		t.Fatalf("anonymous starred listing = %d %s, want empty", got.Code, got.Body.String())
	}
	got = tokenRequest(s, http.MethodGet, path, AdminToken())
	if got.Code != http.StatusOK {
		t.Fatalf("owner-visible starred listing = %d: %s", got.Code, got.Body.String())
	}
	var repos []map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &repos); err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0]["full_name"] != privateRepo.FullName {
		t.Fatalf("owner-visible starred listing = %v", repos)
	}
}

func TestCollaboratorReadsRequireMetadataPermission(t *testing.T) {
	s := newTestServer()
	s.registerGHRepoRoutes()

	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "collaborator-scope-org", "", "")
	repo := s.store.CreateOrgRepo(org, admin, "private", "", true)
	app := s.store.CreateApp(admin.ID, "Collaborator Reader", "", map[string]string{"metadata": "read"}, nil)
	installation := s.store.CreateInstallation(app.ID, "Organization", org.ID, org.Login, app.Permissions, nil)
	metadata := s.store.CreateInstallationToken(installation.ID, app.ID, map[string]string{"metadata": "read"}, []int{repo.ID})
	path := "/api/v3/repos/" + repo.FullName + "/collaborators"

	if got := tokenRequest(s, http.MethodGet, path, metadata.Token); got.Code != http.StatusOK {
		t.Fatalf("metadata:read collaborator listing = %d: %s", got.Code, got.Body.String())
	}
}

func TestReactionDeletionRequiresCreator(t *testing.T) {
	s := newTestServer()
	s.registerGHRepoRoutes()
	s.registerGHIssueRoutes()
	s.registerGHReactionsRoutes()

	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "reaction-owner", "", false)
	issue := s.store.CreateIssue(repo.ID, admin.ID, "ownership", "", nil, nil, 0)
	other := seedTestUser(s, "reaction-owner-other")
	if !s.store.AddRepoCollaborator(repo.Owner.Login, repo.Name, other.Login, "push") {
		t.Fatal("grant reaction deleter repository write fixture")
	}
	otherToken := s.store.CreateToken(other.ID, "repo")
	path := "/api/v3/repos/" + repo.FullName + "/issues/" + itoa(issue.Number) + "/reactions"

	created := teamCredentialRequest(t, s, http.MethodPost, path, AdminToken(), `{"content":"heart"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create reaction = %d: %s", created.Code, created.Body.String())
	}
	var reaction map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &reaction); err != nil {
		t.Fatal(err)
	}
	deletePath := path + "/" + itoa(int(reaction["id"].(float64)))
	if got := tokenRequest(s, http.MethodDelete, deletePath, otherToken.Value); got.Code != http.StatusNotFound {
		t.Fatalf("non-owner delete reaction = %d: %s", got.Code, got.Body.String())
	}
	if got := tokenRequest(s, http.MethodDelete, deletePath, AdminToken()); got.Code != http.StatusNoContent {
		t.Fatalf("owner delete reaction = %d: %s", got.Code, got.Body.String())
	}
}

func TestInternalNetworkSettingsSeedRequiresSiteAdmin(t *testing.T) {
	s := newTestServer()
	s.registerGHNetworkConfigurationRoutes()

	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "network-seed-auth", "", "")
	nonAdmin := seedTestUser(s, "network-seed-auth-user")
	nonAdminToken := s.store.CreateToken(nonAdmin.ID, "repo")
	path := "/internal/orgs/" + org.Login + "/network-settings"

	if got := tokenRequest(s, http.MethodPost, path, ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous network settings seed = %d: %s", got.Code, got.Body.String())
	}
	if got := tokenRequest(s, http.MethodPost, path, nonAdminToken.Value); got.Code != http.StatusForbidden {
		t.Fatalf("non-admin network settings seed = %d: %s", got.Code, got.Body.String())
	}
}

func TestZeroRequiredApprovalsKeepsReviewProtectionEnabled(t *testing.T) {
	s := newTestServer()
	got := s.applyBranchProtectionRequest(&BranchProtection{}, &bpRequest{
		RequiredPullRequestReviews: &BPPullRequestReviews{
			RequiredApprovingReviewCount: 0,
		},
	})
	if got.RequiredPullRequestReviews == nil {
		t.Fatal("required_approving_review_count: 0 disabled the review rule")
	}
	if got.RequiredPullRequestReviews.RequiredApprovingReviewCount != 0 {
		t.Fatalf("required approving review count = %d", got.RequiredPullRequestReviews.RequiredApprovingReviewCount)
	}
}

func TestRepositoryActionsPermissionsRequireEnabled(t *testing.T) {
	s := newTestServer()
	s.registerGHActionsPermissionsRoutes()
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "actions-enabled-required", "", false)
	before := s.store.GetRepoActionsPermissions(repo.FullName)

	got := teamCredentialRequest(t, s, http.MethodPut,
		"/api/v3/repos/"+repo.FullName+"/actions/permissions", AdminToken(), `{"allowed_actions":"selected"}`)
	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing enabled = %d: %s", got.Code, got.Body.String())
	}
	after := s.store.GetRepoActionsPermissions(repo.FullName)
	if after.Enabled != before.Enabled || after.AllowedActions != before.AllowedActions {
		t.Fatalf("invalid request mutated actions permissions: before=%+v after=%+v", before, after)
	}
}

func TestCreatePullRequestReviewValidatesBeforeMutation(t *testing.T) {
	s := newTestServer()
	s.registerGHPullRoutes()
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "review-validation", "", false)
	seedPullRequestBranches(t, s, repo, "feature")
	pr := s.store.CreatePullRequest(repo.ID, admin.ID, "Review validation", "", "feature", "main", false, nil, nil, 0)
	if pr == nil {
		t.Fatal("create pull request fixture")
	}
	path := "/api/v3/repos/" + repo.FullName + "/pulls/" + itoa(pr.Number) + "/reviews"

	got := teamCredentialRequest(t, s, http.MethodPost, path, AdminToken(), `{"event":"APPROVED"}`)
	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid review event = %d: %s", got.Code, got.Body.String())
	}
	got = teamCredentialRequest(t, s, http.MethodPost, path, AdminToken(),
		`{"comments":[{"path":"one.go","body":"valid"},{"path":"","body":"invalid"}]}`)
	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid review comment batch = %d: %s", got.Code, got.Body.String())
	}
	if reviews := s.store.ListPullRequestReviews(repo.FullName, pr.Number); len(reviews) != 0 {
		t.Fatalf("invalid review requests left %d partial reviews", len(reviews))
	}
}

func TestReleaseListingHidesDraftsFromReaders(t *testing.T) {
	s := newTestServer()
	s.registerGHReleasesRoutes()
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "draft-release-privacy", "", false)
	s.store.Releases.Create(repo.ID, admin.ID, "v1-draft", "main", "Draft", "", true, false)
	s.store.Releases.Create(repo.ID, admin.ID, "v1", "main", "Published", "", false, false)
	path := "/api/v3/repos/" + repo.FullName + "/releases"

	got := tokenRequest(s, http.MethodGet, path, "")
	if got.Code != http.StatusOK {
		t.Fatalf("anonymous release list = %d: %s", got.Code, got.Body.String())
	}
	var releases []map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &releases); err != nil {
		t.Fatal(err)
	}
	if len(releases) != 1 || releases[0]["tag_name"] != "v1" {
		t.Fatalf("anonymous release list = %v, want published release only", releases)
	}

	got = tokenRequest(s, http.MethodGet, path, AdminToken())
	if err := json.Unmarshal(got.Body.Bytes(), &releases); err != nil {
		t.Fatal(err)
	}
	if len(releases) != 2 {
		t.Fatalf("push-authorized release list = %v, want draft and published", releases)
	}
}

func TestSetRunnerLabelsAssignsDistinctIDs(t *testing.T) {
	agent := &Agent{Labels: []Label{{ID: 1, Name: "self-hosted", Type: "system"}}}
	agent.SetLabels([]string{"gpu", "arm64", "large"})
	seen := map[int]bool{}
	for _, label := range agent.Labels {
		if seen[label.ID] {
			t.Fatalf("duplicate label id %d in %+v", label.ID, agent.Labels)
		}
		seen[label.ID] = true
	}
}

func TestCustomPropertyValuesDoNotAliasAcrossRepositories(t *testing.T) {
	s := newTestServer()
	value := []interface{}{"linux", "arm64"}
	payload := []customPropertyValuePayload{{PropertyName: "targets", Value: value}}
	s.store.SetRepoCustomPropertyValues("acme/one", payload)
	s.store.SetRepoCustomPropertyValues("acme/two", payload)

	s.store.mu.Lock()
	first := s.store.RepoCustomPropertyValues["acme/one"]["targets"].([]interface{})
	second := s.store.RepoCustomPropertyValues["acme/two"]["targets"].([]interface{})
	first[0] = "mutated"
	gotSecond := second[0]
	s.store.mu.Unlock()
	if gotSecond != "linux" {
		t.Fatalf("second repository value changed through first repository alias: %v", gotSecond)
	}
}

func TestTeamRenameCannotShadowExistingSlug(t *testing.T) {
	s := newTestServer()
	s.registerGHTeamRoutes()
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "team-rename-collision", "", "")
	first := s.store.CreateTeam(org.Login, "Platform", TeamOptions{})
	second := s.store.CreateTeam(org.Login, "Security", TeamOptions{})

	got := teamCredentialRequest(t, s, http.MethodPatch,
		"/api/v3/orgs/"+org.Login+"/teams/"+second.Slug, AdminToken(), `{"name":"Platform"}`)
	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("colliding team rename = %d: %s", got.Code, got.Body.String())
	}
	if resolved := s.store.GetTeam(org.Login, first.Slug); resolved == nil || resolved.ID != first.ID {
		t.Fatalf("original slug no longer resolves to original team: %+v", resolved)
	}
	if resolved := s.store.GetTeam(org.Login, second.Slug); resolved == nil || resolved.ID != second.ID || resolved.Name != "Security" {
		t.Fatalf("rejected rename partially mutated source team: %+v", resolved)
	}
}

func TestDeletingPullRequestReviewCommentDeletesItsReactions(t *testing.T) {
	s := newTestServer()
	s.registerGHPRCommentsRoutes()
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "review-comment-reactions", "", false)
	seedPullRequestBranches(t, s, repo, "feature")
	pr := s.store.CreatePullRequest(repo.ID, admin.ID, "Comment reactions", "", "feature", "main", false, nil, nil, 0)
	comment := s.store.PRReviewComments.CreateRootComment(pr.ID, admin.ID, "main.go", "note", "abc", "RIGHT", 1, 0)
	if _, _, err := s.store.Reactions.AddReaction("pull_request_review_comment", comment.ID, admin.ID, "heart"); err != nil {
		t.Fatal(err)
	}

	path := "/api/v3/repos/" + repo.FullName + "/pulls/comments/" + itoa(comment.ID)
	if got := tokenRequest(s, http.MethodDelete, path, AdminToken()); got.Code != http.StatusNoContent {
		t.Fatalf("delete review comment = %d: %s", got.Code, got.Body.String())
	}
	if reactions := s.store.Reactions.ListReactions("pull_request_review_comment", comment.ID, ""); len(reactions) != 0 {
		t.Fatalf("deleted review comment retained reactions: %+v", reactions)
	}
}

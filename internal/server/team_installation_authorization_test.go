package bleephub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func teamCredentialRequest(t *testing.T, s *Server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.requestHandler().ServeHTTP(rec, req)
	return rec
}

func TestInstallationMembersPermissionCoversTheWholeTeamJourney(t *testing.T) {
	s := newTestServer()
	s.registerGHTeamRoutes()
	s.registerGHLegacyTeamRoutes()

	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "installation-teams", "", "")
	repo := s.store.CreateOrgRepo(org, admin, "widget", "", false)
	target := seedTestUser(s, "installation-team-target")
	if org == nil || repo == nil || target == nil {
		t.Fatal("create team authorization fixtures")
	}
	app := s.store.CreateApp(admin.ID, "Team Provisioner", "", map[string]string{
		"members":        "write",
		"administration": "write",
	}, nil)
	installation := s.store.CreateInstallation(app.ID, "Organization", org.ID, org.Login, app.Permissions, nil)
	token := s.store.CreateInstallationToken(installation.ID, app.ID, installation.Permissions, nil)

	created := teamCredentialRequest(t, s, http.MethodPost, "/api/v3/orgs/"+org.Login+"/teams", token.Token, `{"name":"Platform"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("installation POST team = %d: %s", created.Code, created.Body.String())
	}
	var teamBody struct {
		ID   int    `json:"id"`
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &teamBody); err != nil {
		t.Fatalf("decode created team: %v", err)
	}
	if teamBody.ID == 0 || teamBody.Slug != "platform" {
		t.Fatalf("created team = %+v", teamBody)
	}
	if _, botBecameMember := s.store.GetTeamMembership(org.Login, teamBody.Slug, -app.ID); botBecameMember {
		t.Fatal("installation-token team creation persisted the synthetic App bot as a team member")
	}

	if got := teamCredentialRequest(t, s, http.MethodGet, "/api/v3/orgs/"+org.Login+"/teams", token.Token, ""); got.Code != http.StatusOK {
		t.Fatalf("installation GET teams = %d: %s", got.Code, got.Body.String())
	}
	updated := teamCredentialRequest(t, s, http.MethodPatch,
		"/api/v3/orgs/"+org.Login+"/teams/"+teamBody.Slug, token.Token,
		`{"name":"Platform Engineering"}`)
	if updated.Code != http.StatusOK {
		t.Fatalf("installation PATCH team = %d: %s", updated.Code, updated.Body.String())
	}
	teamBody.Slug = "platform-engineering"

	membershipPath := "/api/v3/orgs/" + org.Login + "/teams/" + teamBody.Slug + "/memberships/" + target.Login
	if got := teamCredentialRequest(t, s, http.MethodPut, membershipPath, token.Token, `{"role":"maintainer"}`); got.Code != http.StatusOK {
		t.Fatalf("installation PUT team membership = %d: %s", got.Code, got.Body.String())
	}
	if got := teamCredentialRequest(t, s, http.MethodDelete, membershipPath, token.Token, ""); got.Code != http.StatusNoContent {
		t.Fatalf("installation DELETE team membership = %d: %s", got.Code, got.Body.String())
	}

	repoPath := "/api/v3/orgs/" + org.Login + "/teams/" + teamBody.Slug + "/repos/" + repo.FullName
	if got := teamCredentialRequest(t, s, http.MethodPut, repoPath, token.Token, `{"permission":"push"}`); got.Code != http.StatusNoContent {
		t.Fatalf("installation PUT team repository = %d: %s", got.Code, got.Body.String())
	}
	if got := teamCredentialRequest(t, s, http.MethodGet, repoPath, token.Token, ""); got.Code != http.StatusNoContent {
		t.Fatalf("installation GET team repository = %d: %s", got.Code, got.Body.String())
	}

	legacyBase := "/api/v3/teams/" + strconv.Itoa(teamBody.ID)
	if got := teamCredentialRequest(t, s, http.MethodGet, legacyBase, token.Token, ""); got.Code != http.StatusOK {
		t.Fatalf("installation GET legacy team = %d: %s", got.Code, got.Body.String())
	}
	if got := teamCredentialRequest(t, s, http.MethodPatch, legacyBase, token.Token, `{"description":"managed by app"}`); got.Code != http.StatusOK {
		t.Fatalf("installation PATCH legacy team = %d: %s", got.Code, got.Body.String())
	}
	if got := teamCredentialRequest(t, s, http.MethodDelete, legacyBase+"/repos/"+repo.FullName, token.Token, ""); got.Code != http.StatusNoContent {
		t.Fatalf("installation DELETE legacy team repository = %d: %s", got.Code, got.Body.String())
	}
	if got := teamCredentialRequest(t, s, http.MethodDelete, legacyBase, token.Token, ""); got.Code != http.StatusNoContent {
		t.Fatalf("installation DELETE legacy team = %d: %s", got.Code, got.Body.String())
	}
}

func TestInstallationTeamAuthorizationRequiresMembersGrantAndTargetOrg(t *testing.T) {
	s := newTestServer()
	s.registerGHTeamRoutes()

	admin := s.store.LookupUserByLogin("admin")
	installed := s.store.CreateOrg(admin, "team-grant-installed", "", "")
	other := s.store.CreateOrg(admin, "team-grant-other", "", "")
	team := s.store.CreateTeam(installed.Login, "Visible", TeamOptions{})
	app := s.store.CreateApp(admin.ID, "Team Grant Boundary", "", map[string]string{"members": "write"}, nil)
	installation := s.store.CreateInstallation(app.ID, "Organization", installed.ID, installed.Login, app.Permissions, nil)
	readToken := s.store.CreateInstallationToken(installation.ID, app.ID, map[string]string{"members": "read"}, nil)

	if got := teamCredentialRequest(t, s, http.MethodGet, "/api/v3/orgs/"+installed.Login+"/teams/"+team.Slug, readToken.Token, ""); got.Code != http.StatusOK {
		t.Fatalf("members:read GET team = %d: %s", got.Code, got.Body.String())
	}
	if got := teamCredentialRequest(t, s, http.MethodPost, "/api/v3/orgs/"+installed.Login+"/teams", readToken.Token, `{"name":"Denied"}`); got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), "Resource not accessible by integration") {
		t.Fatalf("members:read POST team = %d: %s", got.Code, got.Body.String())
	}

	writeToken := s.store.CreateInstallationToken(installation.ID, app.ID, installation.Permissions, nil)
	if got := teamCredentialRequest(t, s, http.MethodPost, "/api/v3/orgs/"+other.Login+"/teams", writeToken.Token, `{"name":"Wrong target"}`); got.Code != http.StatusForbidden {
		t.Fatalf("other-org installation POST team = %d: %s", got.Code, got.Body.String())
	}

	metadataToken := s.store.CreateInstallationToken(installation.ID, app.ID, nil, nil)
	if got := teamCredentialRequest(t, s, http.MethodGet, "/api/v3/orgs/"+installed.Login+"/teams", metadataToken.Token, ""); got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), "Resource not accessible by integration") {
		t.Fatalf("metadata-only GET teams = %d: %s", got.Code, got.Body.String())
	}

	repo := s.store.CreateOrgRepo(installed, admin, "permission-conjunction", "", false)
	teamRepoPath := "/api/v3/orgs/" + installed.Login + "/teams/" + team.Slug + "/repos/" + repo.FullName
	membersOnly := s.store.CreateInstallationToken(installation.ID, app.ID, map[string]string{"members": "write"}, nil)
	if got := teamCredentialRequest(t, s, http.MethodPut, teamRepoPath, membersOnly.Token, `{"permission":"push"}`); got.Code != http.StatusForbidden {
		t.Fatalf("members-only PUT team repository = %d: %s", got.Code, got.Body.String())
	}
}

func TestInstallationOrgPermissionsDoNotRequireSyntheticBotMembership(t *testing.T) {
	s := newTestServer()
	s.registerGHOrgRoutes()
	s.registerGHOrgsPeopleRoutes()

	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "installation-org-people", "", "")
	invitee := seedTestUser(s, "installation-org-invitee")
	member := seedTestUser(s, "installation-org-member")
	blocked := seedTestUser(s, "installation-org-blocked")
	if org == nil || invitee == nil || member == nil || blocked == nil {
		t.Fatal("create organization authorization fixtures")
	}
	app := s.store.CreateApp(admin.ID, "Organization Provisioner", "", map[string]string{
		"members":                     "write",
		"organization_administration": "write",
		"organization_hooks":          "write",
	}, nil)
	installation := s.store.CreateInstallation(app.ID, "Organization", org.ID, org.Login, app.Permissions, nil)
	token := s.store.CreateInstallationToken(installation.ID, app.ID, installation.Permissions, nil)

	update := teamCredentialRequest(t, s, http.MethodPatch,
		"/api/v3/orgs/"+org.Login, token.Token,
		`{"description":"managed by installation"}`)
	if update.Code != http.StatusOK {
		t.Fatalf("installation PATCH organization = %d: %s", update.Code, update.Body.String())
	}

	invitation := teamCredentialRequest(t, s, http.MethodPost,
		"/api/v3/orgs/"+org.Login+"/invitations", token.Token,
		`{"invitee_id":`+strconv.Itoa(invitee.ID)+`}`)
	if invitation.Code != http.StatusCreated {
		t.Fatalf("installation POST invitation = %d: %s", invitation.Code, invitation.Body.String())
	}

	membershipPath := "/api/v3/orgs/" + org.Login + "/memberships/" + member.Login
	if got := teamCredentialRequest(t, s, http.MethodPut, membershipPath, token.Token, `{"role":"member"}`); got.Code != http.StatusOK {
		t.Fatalf("installation PUT organization membership = %d: %s", got.Code, got.Body.String())
	}
	if got := teamCredentialRequest(t, s, http.MethodGet, membershipPath, token.Token, ""); got.Code != http.StatusOK {
		t.Fatalf("installation GET organization membership = %d: %s", got.Code, got.Body.String())
	}

	blockPath := "/api/v3/orgs/" + org.Login + "/blocks/" + blocked.Login
	if got := teamCredentialRequest(t, s, http.MethodPut, blockPath, token.Token, ""); got.Code != http.StatusNoContent {
		t.Fatalf("installation PUT organization block = %d: %s", got.Code, got.Body.String())
	}

	hook := teamCredentialRequest(t, s, http.MethodPost,
		"/api/v3/orgs/"+org.Login+"/hooks", token.Token,
		`{"name":"web","config":{"url":"https://example.com/hook","content_type":"json"},"events":["push"]}`)
	if hook.Code != http.StatusCreated {
		t.Fatalf("installation POST organization hook = %d: %s", hook.Code, hook.Body.String())
	}
}

func TestOrganizationMembersCreateTeamsAndMaintainersManageThem(t *testing.T) {
	s := newTestServer()
	s.registerGHTeamRoutes()

	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "human-team-standing", "", "")
	member := seedTestUser(s, "human-team-member")
	s.store.SetMembership(org.Login, member.ID, OrgRoleMember, MembershipStateActive)
	token := s.store.CreateToken(member.ID, "admin:org").Value

	created := teamCredentialRequest(t, s, http.MethodPost, "/api/v3/orgs/"+org.Login+"/teams", token, `{"name":"Member Created"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("organization member POST team = %d: %s", created.Code, created.Body.String())
	}
	if got := teamCredentialRequest(t, s, http.MethodPatch, "/api/v3/orgs/"+org.Login+"/teams/member-created", token, `{"description":"maintained"}`); got.Code != http.StatusOK {
		t.Fatalf("team maintainer PATCH team = %d: %s", got.Code, got.Body.String())
	}
	if got := teamCredentialRequest(t, s, http.MethodDelete, "/api/v3/orgs/"+org.Login+"/teams/member-created", token, ""); got.Code != http.StatusNoContent {
		t.Fatalf("team maintainer DELETE team = %d: %s", got.Code, got.Body.String())
	}
}

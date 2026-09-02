package bleephub

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHTeamRoutes() {
	s.route("GET /api/v3/user/teams", s.handleListAuthUserTeams)
	s.route("POST /api/v3/orgs/{org}/teams", s.requirePerm(store.ScopeMembers, store.PermWrite, s.handleCreateTeam))
	s.route("GET /api/v3/orgs/{org}/teams", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleListTeams))
	s.route("GET /api/v3/orgs/{org}/teams/{team_slug}", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleGetTeam))
	s.route("PATCH /api/v3/orgs/{org}/teams/{team_slug}", s.requirePerm(store.ScopeMembers, store.PermWrite, s.handleUpdateTeam))
	s.route("DELETE /api/v3/orgs/{org}/teams/{team_slug}", s.requirePerm(store.ScopeMembers, store.PermWrite, s.handleDeleteTeam))
	s.route("GET /api/v3/orgs/{org}/teams/{team_slug}/teams", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleListChildTeams))

	s.route("GET /api/v3/orgs/{org}/teams/{team_slug}/members", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleListTeamMembers))
	s.route("GET /api/v3/orgs/{org}/teams/{team_slug}/memberships/{username}", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleGetTeamMembership))
	s.route("PUT /api/v3/orgs/{org}/teams/{team_slug}/memberships/{username}", s.requirePerm(store.ScopeMembers, store.PermWrite, s.handleAddTeamMember))
	s.route("DELETE /api/v3/orgs/{org}/teams/{team_slug}/memberships/{username}", s.requirePerm(store.ScopeMembers, store.PermWrite, s.handleRemoveTeamMember))

	s.route("GET /api/v3/orgs/{org}/teams/{team_slug}/repos", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleListTeamRepos))
	s.route("GET /api/v3/orgs/{org}/teams/{team_slug}/repos/{owner}/{repo}", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleCheckTeamRepo))
	teamRepoWrite := []permissionGrant{
		{scope: store.ScopeMembers, level: store.PermRead},
		{scope: store.ScopeAdministration, level: store.PermWrite},
	}
	s.route("PUT /api/v3/orgs/{org}/teams/{team_slug}/repos/{owner}/{repo}", s.requirePerms(teamRepoWrite, s.handleAddTeamRepo))
	s.route("DELETE /api/v3/orgs/{org}/teams/{team_slug}/repos/{owner}/{repo}", s.requirePerms(teamRepoWrite, s.handleRemoveTeamRepo))
}

// validTeamEnums checks the privacy/permission/notification_setting enums ("" = absent, allowed), returning the offending field name.
func validTeamEnums(privacy, permission, notification string) (string, bool) {
	switch store.TeamPrivacy(privacy) {
	case "", store.TeamPrivacyClosed, store.TeamPrivacySecret:
	default:
		return "privacy", false
	}
	switch store.TeamPermission(permission) {
	// GitHub's team-level default permission is pull or push only; "admin" (and
	// the other fine-grained roles) are set per-repository, gated by repo admin.
	// Accepting "admin" here let a team maintainer raise the default to admin and
	// retroactively promote the team on every repo linked at the default.
	case "", store.TeamPermissionPull, store.TeamPermissionPush:
	default:
		return "permission", false
	}
	switch store.TeamNotificationSetting(notification) {
	case "", store.TeamNotificationsEnabled, store.TeamNotificationsDisabled:
	default:
		return "notification_setting", false
	}
	return "", true
}

func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	orgLogin := r.PathValue("org")
	org := s.store.GetOrg(orgLogin)
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	if !s.viewerCanCreateTeam(r.Context(), org.Login) {
		writeGHError(w, http.StatusForbidden, "Must be an organization member.")
		return
	}

	var req struct {
		Name                string   `json:"name"`
		Description         string   `json:"description"`
		Privacy             string   `json:"privacy"`
		Permission          string   `json:"permission"`
		NotificationSetting string   `json:"notification_setting"`
		ParentTeamID        flexInt  `json:"parent_team_id"`
		Maintainers         []string `json:"maintainers"`
		RepoNames           []string `json:"repo_names"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	if field, ok := validTeamEnums(req.Privacy, req.Permission, req.NotificationSetting); !ok {
		store.WriteGHValidationError(w, "Team", field, "invalid")
		return
	}
	if req.ParentTeamID != 0 {
		parent := s.store.GetTeamByID(int(req.ParentTeamID))
		if parent == nil || parent.OrgID != org.ID {
			store.WriteGHValidationError(w, "Team", "parent_team_id", "invalid")
			return
		}
	}

	// Resolve seeded maintainers and repos before creating the team, so an unknown
	// entry rejects the request instead of leaving a half-built team.
	maintainerIDs := make([]int, 0, len(req.Maintainers))
	for _, login := range req.Maintainers {
		maintainer := s.store.LookupUserByLogin(login)
		if maintainer == nil {
			store.WriteGHValidationError(w, "Team", "maintainers", "invalid")
			return
		}
		maintainerIDs = append(maintainerIDs, maintainer.ID)
	}
	for _, fullName := range req.RepoNames {
		owner, name, found := strings.Cut(fullName, "/")
		repo := s.store.GetRepo(owner, name)
		if !found || repo == nil {
			store.WriteGHValidationError(w, "Team", "repo_names", "invalid")
			return
		}
		// Adding a repository to a team requires admin on that repository (GitHub
		// parity). Without this, any org member could seed a private repo they
		// cannot even read into a new team and grant themselves access via the
		// team's default permission.
		if !s.viewerHasRepoPermission(r.Context(), repo, store.ScopeAdministration, store.PermWrite) {
			writeGHError(w, http.StatusForbidden, "You must be an admin of the repository to add it to a team.")
			return
		}
	}

	team := s.store.CreateTeam(orgLogin, req.Name, store.TeamOptions{
		Description:         req.Description,
		Privacy:             store.TeamPrivacy(req.Privacy),
		Permission:          store.TeamPermission(req.Permission),
		NotificationSetting: store.TeamNotificationSetting(req.NotificationSetting),
		ParentID:            int(req.ParentTeamID),
	})
	if team == nil {
		writeGHError(w, http.StatusUnprocessableEntity, "Validation Failed")
		return
	}
	// GitHub makes the human creator a maintainer even when omitted from maintainers.
	if ghInstallationTokenFromContext(r.Context()) == nil {
		s.store.SetTeamMembership(orgLogin, team.Slug, user.ID, store.TeamRoleMaintainer)
	}
	for _, id := range maintainerIDs {
		if id != user.ID {
			s.store.SetTeamMembership(orgLogin, team.Slug, id, store.TeamRoleMaintainer)
		}
	}
	for _, fullName := range req.RepoNames {
		s.store.SetTeamRepoPermission(orgLogin, team.Slug, fullName, "")
	}

	s.recordAuditEvent("team.create", user.Login, orgLogin, map[string]interface{}{"team_id": team.ID, "team_slug": team.Slug})
	teamJSON := teamToJSON(team, org, s.store, s.baseURL(r))
	writeJSONCreated(w, jsonStringField(teamJSON, "url"), teamJSON)
}

func (s *Server) handleListTeams(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	orgLogin := r.PathValue("org")
	org := s.store.GetOrg(orgLogin)
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanReadOrgTeams(r.Context(), orgLogin) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	teams := s.store.ListTeams(orgLogin)
	result := make([]map[string]interface{}, 0, len(teams))
	base := s.baseURL(r)
	for _, team := range teams {
		result = append(result, teamSimpleJSON(team, org, s.store, base))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleGetTeam(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	orgLogin := r.PathValue("org")
	org := s.store.GetOrg(orgLogin)
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanReadOrgTeams(r.Context(), orgLogin) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	slug := r.PathValue("team_slug")
	team := s.store.GetTeam(orgLogin, slug)
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	writeJSON(w, http.StatusOK, teamToJSON(team, org, s.store, s.baseURL(r)))
}

func (s *Server) handleUpdateTeam(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	orgLogin := r.PathValue("org")
	org := s.store.GetOrg(orgLogin)
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	slug := r.PathValue("team_slug")
	team := s.store.GetTeam(orgLogin, slug)
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.canManageTeam(r.Context(), user, org, team, false) {
		writeGHError(w, http.StatusForbidden, "Must be an organization owner or team maintainer.")
		return
	}

	s.applyTeamUpdate(w, r, org, team)
}

// applyTeamUpdate validates and applies a team PATCH body, writing the
// team-full response or a validation error. Shared by the slug- and
// legacy ID-addressed update endpoints.
func (s *Server) applyTeamUpdate(w http.ResponseWriter, r *http.Request, org *store.Org, team *store.Team) {
	orgLogin := org.Login
	slug := team.Slug

	var req map[string]interface{}
	if !decodeJSONBody(w, r, &req) {
		return
	}

	privacy, _ := req["privacy"].(string)
	permission, _ := req["permission"].(string)
	notification, _ := req["notification_setting"].(string)
	if field, ok := validTeamEnums(privacy, permission, notification); !ok {
		store.WriteGHValidationError(w, "Team", field, "invalid")
		return
	}

	// parent_team_id: a number re-parents, explicit null detaches.
	parentID := -1 // -1 = absent
	if raw, present := req["parent_team_id"]; present {
		switch v := raw.(type) {
		case nil:
			parentID = 0
		case float64:
			parentID = int(v)
		default:
			store.WriteGHValidationError(w, "Team", "parent_team_id", "invalid")
			return
		}
	}
	if parentID > 0 {
		parent := s.store.GetTeamByID(parentID)
		if parent == nil || parent.OrgID != org.ID {
			store.WriteGHValidationError(w, "Team", "parent_team_id", "invalid")
			return
		}
		if parentID == team.ID || s.store.TeamParentWouldCycle(team.ID, parentID) {
			store.WriteGHValidationError(w, "Team", "parent_team_id", "invalid")
			return
		}
	}

	err := s.store.UpdateTeamChecked(orgLogin, slug, func(t *store.Team) {
		if v, ok := req["name"].(string); ok {
			t.Name = v
			t.Slug = store.Slugify(v)
		}
		if v, ok := req["description"].(string); ok {
			t.Description = v
		}
		if privacy != "" {
			t.Privacy = store.TeamPrivacy(privacy)
		}
		if permission != "" {
			t.Permission = store.TeamPermission(permission)
		}
		if notification != "" {
			t.NotificationSetting = store.TeamNotificationSetting(notification)
		}
		if parentID >= 0 {
			t.ParentID = parentID
		}
	})
	if errors.Is(err, store.ErrTeamSlugConflict) {
		store.WriteGHValidationError(w, "Team", "name", "already_exists")
		return
	}
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// Re-fetch by ID: a name change re-keys the slug index.
	updated := s.store.GetTeamByID(team.ID)
	if updated == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, teamToJSON(updated, org, s.store, s.baseURL(r)))
}

func (s *Server) handleListChildTeams(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	orgLogin := r.PathValue("org")
	org := s.store.GetOrg(orgLogin)
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerIsOrgMember(r.Context(), orgLogin) {
		// Team hierarchy (including secret teams) is private org data; the
		// members:read installation grant is admitted upstream by requirePerm.
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	team := s.store.GetTeam(orgLogin, r.PathValue("team_slug"))
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	children := s.store.ListChildTeams(orgLogin, team.ID)
	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(children))
	for _, child := range children {
		result = append(result, teamSimpleJSON(child, org, s.store, base))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleDeleteTeam(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	orgLogin := r.PathValue("org")
	org := s.store.GetOrg(orgLogin)
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	slug := r.PathValue("team_slug")
	team := s.store.GetTeam(orgLogin, slug)
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.canManageTeam(r.Context(), user, org, team, false) {
		writeGHError(w, http.StatusForbidden, "Must be an organization owner or team maintainer.")
		return
	}
	if !s.store.DeleteTeam(orgLogin, slug) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.recordAuditEvent("team.delete", user.Login, orgLogin, map[string]interface{}{"team_slug": slug})
	w.WriteHeader(http.StatusNoContent)
}

// handleListAuthUserTeams returns every team the caller belongs to across all
// orgs, each with an embedded "organization". OIDC relying parties call it to
// map team membership to roles at sign-in.
func (s *Server) handleListAuthUserTeams(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	teams := s.store.ListTeamsByUser(user.ID)
	s.logger.Debug().Int("user_id", user.ID).Str("user_login", user.Login).Int("team_count", len(teams)).Msg("GET /api/v3/user/teams")
	result := make([]map[string]interface{}, 0, len(teams))
	base := s.baseURL(r)
	for _, team := range teams {
		org := s.store.GetOrgByID(team.OrgID)
		if org == nil {
			continue
		}
		result = append(result, teamToJSON(team, org, s.store, base))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

// viewerCanReadOrgTeams: an installation token is authorized by its Members
// grant over the org, a human by org membership. (The token represents the
// installation, not the synthetic bot user in ctxUser.)
func (s *Server) viewerCanReadOrgTeams(ctx context.Context, orgLogin string) bool {
	if ghInstallationTokenFromContext(ctx) != nil {
		return s.credentialGrantsAccount(ctx, store.OrganizationAccount, orgLogin, store.ScopeMembers, store.PermRead)
	}
	return s.viewerIsOrgMember(ctx, orgLogin)
}

// viewerCanCreateTeam: org members may create teams (GitHub's default policy);
// an installation token acts through Members:write instead.
func (s *Server) viewerCanCreateTeam(ctx context.Context, orgLogin string) bool {
	if ghInstallationTokenFromContext(ctx) != nil {
		return s.credentialGrantsAccount(ctx, store.OrganizationAccount, orgLogin, store.ScopeMembers, store.PermWrite)
	}
	return s.viewerIsOrgMember(ctx, orgLogin)
}

// canManageTeam reports whether the request may mutate a team, its membership,
// or its repo grants. Installation tokens are authorized by Members:write;
// humans by owner/maintainer, and only owners may promote another to maintainer.
func (s *Server) canManageTeam(ctx context.Context, user *store.User, org *store.Org, team *store.Team, addingMaintainer bool) bool {
	if ghInstallationTokenFromContext(ctx) != nil {
		return s.credentialGrantsAccount(ctx, store.OrganizationAccount, org.Login, store.ScopeMembers, store.PermWrite)
	}
	ctx = contextWithUser(ctx, user)
	if s.viewerCanAdminOrg(ctx, org.Login) {
		return true
	}
	role, isMember := team.RoleOf(user.ID)
	return isMember && role == store.TeamRoleMaintainer && !addingMaintainer
}

// canManageTeamRepository: installation tokens need Members:read on the org,
// humans the owner/maintainer rule. The Administration:write repo half is
// enforced by requirePerms upstream.
func (s *Server) canManageTeamRepository(ctx context.Context, user *store.User, org *store.Org, team *store.Team, repo *store.Repo) bool {
	if !s.viewerHasRepoPermission(ctx, repo, store.ScopeAdministration, store.PermWrite) {
		return false
	}
	if ghInstallationTokenFromContext(ctx) != nil {
		return s.credentialGrantsAccount(ctx, store.OrganizationAccount, org.Login, store.ScopeMembers, store.PermRead)
	}
	return s.canManageTeam(ctx, user, org, team, false)
}

func (s *Server) handleListTeamMembers(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	orgLogin := r.PathValue("org")
	if s.store.GetOrg(orgLogin) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanReadOrgTeams(r.Context(), orgLogin) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	slug := r.PathValue("team_slug")
	team := s.store.GetTeam(orgLogin, slug)
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// ?role = member|maintainer|all (default all).
	role := r.URL.Query().Get("role")
	if role == "" {
		role = "all"
	}
	if role != "all" && role != "member" && role != "maintainer" {
		store.WriteGHValidationError(w, "TeamMembership", "role", "invalid")
		return
	}

	members := s.store.ListTeamMembers(orgLogin, slug)
	result := make([]map[string]interface{}, 0, len(members))
	for _, u := range members {
		if role != "all" {
			memberRole, ok := team.RoleOf(u.ID)
			if !ok || string(memberRole) != role {
				continue
			}
		}
		result = append(result, store.UserToJSON(u, s.baseURL(r)))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleAddTeamMember(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	orgLogin := r.PathValue("org")
	org := s.store.GetOrg(orgLogin)
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	slug := r.PathValue("team_slug")
	team := s.store.GetTeam(orgLogin, slug)
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	username := r.PathValue("username")
	target := s.store.LookupUserByLogin(username)
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	var req struct {
		Role string `json:"role"`
	}
	if !decodeJSONBodyOptional(w, r, &req) {
		return
	}
	role := store.TeamRole(req.Role)
	if role == "" {
		role = store.TeamRoleMember
	}
	if role != store.TeamRoleMember && role != store.TeamRoleMaintainer {
		store.WriteGHValidationError(w, "TeamMembership", "role", "invalid")
		return
	}

	if !s.canManageTeam(r.Context(), user, org, team, role == store.TeamRoleMaintainer) {
		writeGHError(w, http.StatusForbidden, "Must be an organization owner or team maintainer.")
		return
	}

	orgMembership := s.store.GetMembership(orgLogin, target.ID)
	if orgMembership == nil {
		// Inviting an unaffiliated user into the org is an owner-only action; a
		// team maintainer may only add existing active org members (GitHub 422s
		// otherwise). Without this, a maintainer could mint org invitations.
		if !s.viewerCanAdminOrg(r.Context(), org.Login) {
			writeGHError(w, http.StatusUnprocessableEntity,
				"Cannot add an organization member. The user must already be a member of the organization.")
			return
		}
		orgMembership = s.store.SetMembership(orgLogin, target.ID, store.OrgRoleMember, store.MembershipStatePending)
		s.emitOrgMembershipEvent(org, "member_invited", orgMembership, target, user)
	}

	s.store.SetTeamMembership(orgLogin, slug, target.ID, role)

	writeJSON(w, http.StatusOK, teamMembershipJSON(s.baseURL(r), orgLogin, slug, target, team, org, role, orgMembership.State))
}

// handleGetTeamMembership reports the membership; its state mirrors the user's
// org membership, so a member with a pending org invite reads as pending.
func (s *Server) handleGetTeamMembership(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	orgLogin := r.PathValue("org")
	org := s.store.GetOrg(orgLogin)
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanReadOrgTeams(r.Context(), orgLogin) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	slug := r.PathValue("team_slug")
	team := s.store.GetTeam(orgLogin, slug)
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	username := r.PathValue("username")
	target := s.store.LookupUserByLogin(username)
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	role, isMember := s.store.GetTeamMembership(orgLogin, slug, target.ID)
	if !isMember {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	state := store.MembershipStateActive
	if m := s.store.GetMembership(orgLogin, target.ID); m != nil {
		state = m.State
	}
	writeJSON(w, http.StatusOK, teamMembershipJSON(s.baseURL(r), orgLogin, slug, target, team, org, role, state))
}

func teamMembershipJSON(baseURL, orgLogin, slug string, user *store.User, team *store.Team, org *store.Org, role store.TeamRole, state store.MembershipState) map[string]interface{} {
	api := baseURL + "/api/v3/orgs/" + orgLogin + "/teams/" + slug
	return map[string]interface{}{
		"url":   api + "/memberships/" + user.Login,
		"role":  role,
		"state": state,
	}
}

func (s *Server) handleRemoveTeamMember(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	orgLogin := r.PathValue("org")
	org := s.store.GetOrg(orgLogin)
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	slug := r.PathValue("team_slug")
	team := s.store.GetTeam(orgLogin, slug)
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	if !s.canManageTeam(r.Context(), user, org, team, false) {
		writeGHError(w, http.StatusForbidden, "Must be an organization owner or team maintainer.")
		return
	}

	username := r.PathValue("username")
	target := s.store.LookupUserByLogin(username)
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	if !s.store.RemoveTeamMembership(orgLogin, slug, target.ID) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListTeamRepos(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	orgLogin := r.PathValue("org")
	if s.store.GetOrg(orgLogin) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanReadOrgTeams(r.Context(), orgLogin) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	team := s.store.GetTeam(orgLogin, r.PathValue("team_slug"))
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	repos := s.store.ListTeamRepos(orgLogin, team.Slug)
	page := paginateAndLink(w, r, repos)
	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(page))
	for _, repo := range page {
		perm, _ := s.store.GetTeamRepoPermission(orgLogin, team.Slug, repo.FullName)
		perms, roleName := teamRepoPermissionsJSON(perm)
		j := store.RepoToJSON(repo, s.store, base)
		j["permissions"] = perms
		j["role_name"] = roleName
		result = append(result, j)
	}
	writeJSON(w, http.StatusOK, result)
}

// handleCheckTeamRepo answers 204 when the team manages the repo, 200 with the repository body under the repository media type, 404 otherwise.
func (s *Server) handleCheckTeamRepo(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	orgLogin := r.PathValue("org")
	if s.store.GetOrg(orgLogin) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanReadOrgTeams(r.Context(), orgLogin) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	team := s.store.GetTeam(orgLogin, r.PathValue("team_slug"))
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.writeTeamRepoCheck(w, r, orgLogin, team)
}

// writeTeamRepoCheck answers the check for an already-resolved team: 204 when
// linked, 200 with the team-repository body under the repository media type,
// 404 otherwise. Shared by the slug- and legacy ID-addressed endpoints.
func (s *Server) writeTeamRepoCheck(w http.ResponseWriter, r *http.Request, orgLogin string, team *store.Team) {
	owner, name := r.PathValue("owner"), r.PathValue("repo")
	fullName := owner + "/" + name
	if _, linked := s.store.GetTeamRepoPermission(orgLogin, team.Slug, fullName); !linked {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	repo := s.store.GetRepo(owner, name)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "vnd.github.v3.repository") {
		j := store.RepoToJSON(repo, s.store, s.baseURL(r))
		perm, _ := s.store.GetTeamRepoPermission(orgLogin, team.Slug, fullName)
		perms, roleName := teamRepoPermissionsJSON(perm)
		j["permissions"] = perms
		j["role_name"] = roleName
		// team-repository omits has_discussions and has_pull_requests.
		delete(j, "has_discussions")
		delete(j, "has_pull_requests")
		writeJSON(w, http.StatusOK, j)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAddTeamRepo(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	orgLogin := r.PathValue("org")
	org := s.store.GetOrg(orgLogin)
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	slug := r.PathValue("team_slug")
	team := s.store.GetTeam(orgLogin, slug)
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.canManageTeamRepository(r.Context(), user, org, team, repo) {
		writeGHError(w, http.StatusForbidden, "Must be an organization owner or team maintainer.")
		return
	}
	fullName := owner + "/" + repoName

	var req struct {
		Permission string `json:"permission"`
	}
	if !decodeJSONBodyOptional(w, r, &req) {
		return
	}
	perm := store.TeamPermission(req.Permission)
	switch perm {
	case "", store.TeamPermissionPull, store.TeamPermissionPush, store.TeamPermissionAdmin:
	default:
		store.WriteGHValidationError(w, "TeamRepo", "permission", "invalid")
		return
	}

	s.store.SetTeamRepoPermission(orgLogin, slug, fullName, perm)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveTeamRepo(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}

	orgLogin := r.PathValue("org")
	org := s.store.GetOrg(orgLogin)
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	slug := r.PathValue("team_slug")
	team := s.store.GetTeam(orgLogin, slug)
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.canManageTeamRepository(r.Context(), user, org, team, repo) {
		writeGHError(w, http.StatusForbidden, "Must be an organization owner or team maintainer.")
		return
	}
	fullName := owner + "/" + repoName

	if !s.store.RemoveTeamRepo(orgLogin, slug, fullName) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// teamRepoPermissionsJSON expands a permission level into the boolean permissions object plus role_name.
func teamRepoPermissionsJSON(perm store.TeamPermission) (map[string]interface{}, string) {
	perms := map[string]interface{}{
		"pull":     true,
		"triage":   perm == store.TeamPermissionPush || perm == store.TeamPermissionAdmin,
		"push":     perm == store.TeamPermissionPush || perm == store.TeamPermissionAdmin,
		"maintain": perm == store.TeamPermissionAdmin,
		"admin":    perm == store.TeamPermissionAdmin,
	}
	switch perm {
	case store.TeamPermissionPush:
		return perms, "write"
	case store.TeamPermissionAdmin:
		return perms, "admin"
	default:
		return perms, "read"
	}
}

// teamRefJSON renders the flat `team-simple` shape used for a team's `parent`.
// All bleephub teams are org-owned, so type is always "organization".
func teamRefJSON(team *store.Team, org *store.Org, baseURL string) map[string]interface{} {
	api := baseURL + "/api/v3/orgs/" + org.Login + "/teams/" + team.Slug
	return map[string]interface{}{
		"id":                   team.ID,
		"node_id":              team.NodeID,
		"url":                  api,
		"html_url":             baseURL + "/orgs/" + org.Login + "/teams/" + team.Slug,
		"name":                 team.Name,
		"slug":                 team.Slug,
		"description":          team.Description,
		"privacy":              team.Privacy,
		"notification_setting": team.NotificationSetting,
		"permission":           team.Permission,
		"members_url":          api + "/members{/member}",
		"repositories_url":     api + "/repos",
		"type":                 "organization",
	}
}

// teamSimpleJSON renders the `team` shape (team-simple plus a nullable parent).
// Must not be called with st.mu held (parent resolution takes RLock).
func teamSimpleJSON(team *store.Team, org *store.Org, st *store.Store, baseURL string) map[string]interface{} {
	out := teamRefJSON(team, org, baseURL)
	out["parent"] = nil
	if team.ParentID != 0 {
		if parent := st.GetTeamByID(team.ParentID); parent != nil {
			out["parent"] = teamRefJSON(parent, org, baseURL)
		}
	}
	return out
}

// teamToJSON renders the `team-full` shape served by single-team operations.
// Must not be called with st.mu held (the embedded organization-full derives counts).
func teamToJSON(team *store.Team, org *store.Org, st *store.Store, baseURL string) map[string]interface{} {
	out := teamSimpleJSON(team, org, st, baseURL)
	orgJSON := orgToJSON(org, st, baseURL)
	// The embedded org uses the `team-organization` schema, which omits
	// members_can_create_teams (only `organization-full` carries it).
	delete(orgJSON, "members_can_create_teams")
	out["organization"] = orgJSON
	out["members_count"] = len(team.MemberIDs)
	out["repos_count"] = len(team.RepoNames)
	out["created_at"] = team.CreatedAt.Format(time.RFC3339)
	out["updated_at"] = team.UpdatedAt.Format(time.RFC3339)
	return out
}

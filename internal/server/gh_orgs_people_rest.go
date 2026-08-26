package bleephub

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// REST surface for organization people management: organization
// invitations (+ failed invitations and per-team invitation lists),
// outside collaborators, organization user blocks, organization
// interaction limits, organization roles (predefined catalog +
// team/user assignment), security managers, organization-member
// codespace administration lives in gh_codespaces.go, Copilot seat
// lookup, and org-wide security-product enablement.

func (s *Server) registerGHOrgsPeopleRoutes() {
	s.registerGHOrganizationSCIMRoutes()
	s.registerGHExternalIdentityRoutes()

	// Organization invitations.
	s.route("GET /api/v3/orgs/{org}/invitations", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleListOrgInvitations))
	s.route("POST /api/v3/orgs/{org}/invitations", s.requirePerm(store.ScopeMembers, store.PermWrite, s.handleCreateOrgInvitation))
	s.route("DELETE /api/v3/orgs/{org}/invitations/{invitation_id}", s.requirePerm(store.ScopeMembers, store.PermWrite, s.handleCancelOrgInvitation))
	s.route("GET /api/v3/orgs/{org}/invitations/{invitation_id}/teams", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleListOrgInvitationTeams))
	s.route("GET /api/v3/orgs/{org}/failed_invitations", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleListFailedOrgInvitations))
	s.route("GET /api/v3/orgs/{org}/teams/{team_slug}/invitations", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleListTeamInvitations))

	// Outside collaborators.
	s.route("GET /api/v3/orgs/{org}/outside_collaborators", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleListOutsideCollaborators))
	s.route("PUT /api/v3/orgs/{org}/outside_collaborators/{username}", s.requirePerm(store.ScopeMembers, store.PermWrite, s.handleConvertMemberToOutsideCollaborator))
	s.route("DELETE /api/v3/orgs/{org}/outside_collaborators/{username}", s.requirePerm(store.ScopeMembers, store.PermWrite, s.handleRemoveOutsideCollaborator))

	// Organization user blocks.
	s.route("GET /api/v3/orgs/{org}/blocks", s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.handleListOrgBlocks))
	s.route("GET /api/v3/orgs/{org}/blocks/{username}", s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.handleCheckOrgBlock))
	s.route("PUT /api/v3/orgs/{org}/blocks/{username}", s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleBlockOrgUser))
	s.route("DELETE /api/v3/orgs/{org}/blocks/{username}", s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleUnblockOrgUser))

	// Organization interaction limits.
	s.route("GET /api/v3/orgs/{org}/interaction-limits", s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.handleGetOrgInteractionLimits))
	s.route("PUT /api/v3/orgs/{org}/interaction-limits", s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleSetOrgInteractionLimits))
	s.route("DELETE /api/v3/orgs/{org}/interaction-limits", s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleDeleteOrgInteractionLimits))

	s.registerGHOrgRolesRoutes()
}

func (s *Server) registerGHOrgRolesRoutes() {
	s.registerGHOrgGovernanceRoutes()

	// Organization roles.
	s.route("GET /api/v3/orgs/{org}/organization-roles", s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.handleListOrganizationRoles))
	s.route("POST /api/v3/orgs/{org}/organization-roles", s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleCreateOrganizationRole))
	s.route("GET /api/v3/orgs/{org}/organization-roles/{role_id}", s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.handleGetOrganizationRole))
	s.route("PATCH /api/v3/orgs/{org}/organization-roles/{role_id}", s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleUpdateOrganizationRole))
	s.route("DELETE /api/v3/orgs/{org}/organization-roles/{role_id}", s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleDeleteOrganizationRole))
	s.route("GET /api/v3/orgs/{org}/organization-roles/{role_id}/teams", s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.handleListOrganizationRoleTeams))
	s.route("GET /api/v3/orgs/{org}/organization-roles/{role_id}/users", s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.handleListOrganizationRoleUsers))
	s.route("PUT /api/v3/orgs/{org}/organization-roles/teams/{team_slug}/{role_id}", s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleAssignOrganizationRoleToTeam))
	s.route("DELETE /api/v3/orgs/{org}/organization-roles/teams/{team_slug}/{role_id}", s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleRevokeOrganizationRoleFromTeam))
	s.route("DELETE /api/v3/orgs/{org}/organization-roles/teams/{team_slug}", s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleRevokeAllOrganizationRolesFromTeam))
	s.route("PUT /api/v3/orgs/{org}/organization-roles/users/{username}/{role_id}", s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleAssignOrganizationRoleToUser))
	s.route("DELETE /api/v3/orgs/{org}/organization-roles/users/{username}/{role_id}", s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleRevokeOrganizationRoleFromUser))
	s.route("DELETE /api/v3/orgs/{org}/organization-roles/users/{username}", s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleRevokeAllOrganizationRolesFromUser))

	// Security managers (the team-assignment alias of the
	// security_manager organization role).
	s.route("GET /api/v3/orgs/{org}/security-managers", s.requirePerm(store.ScopeOrgAdministration, store.PermRead, s.handleListSecurityManagerTeams))
	s.route("PUT /api/v3/orgs/{org}/security-managers/teams/{team_slug}", s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleAddSecurityManagerTeam))
	s.route("DELETE /api/v3/orgs/{org}/security-managers/teams/{team_slug}", s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleRemoveSecurityManagerTeam))

	// Org-wide security-product enablement.
	s.route("POST /api/v3/orgs/{org}/{security_product}/{enablement}", s.requirePerm(store.ScopeOrgAdministration, store.PermWrite, s.handleOrgSecurityProductEnablement))
}

// resolveOrgOwner resolves the {org} path parameter and requires the
// authenticated caller to be an active organization owner, writing the
// appropriate error otherwise.
func (s *Server) resolveOrgOwner(w http.ResponseWriter, r *http.Request) (*store.Org, *store.User) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return nil, nil
	}
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	if !s.viewerCanAdminOrg(r.Context(), org.Login) {
		writeGHError(w, http.StatusForbidden, "Must be an organization owner.")
		return nil, nil
	}
	return org, user
}

// resolveOrgMember resolves the {org} path parameter and requires the
// authenticated caller to be an active organization member — the org's
// internal structure reads as 404 to everyone else, like real GitHub.
func (s *Server) resolveOrgMember(w http.ResponseWriter, r *http.Request) (*store.Org, *store.User) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return nil, nil
	}
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	if !s.viewerIsOrgMember(r.Context(), org.Login) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	return org, user
}

// --- organization invitations ---

// orgInvitationJSON renders the GitHub `organization-invitation` shape.
func (s *Server) orgInvitationJSON(inv *store.OrgInvitation, org *store.Org, baseURL string) map[string]interface{} {
	var login, email interface{}
	if inv.Login != "" {
		login = inv.Login
	}
	if inv.Email != "" {
		email = inv.Email
	}
	inviter := map[string]interface{}(nil)
	if u := s.store.GetUserByID(inv.InviterID); u != nil {
		inviter = store.UserToJSON(u, baseURL)
	}
	var failedAt, failedReason interface{}
	if inv.FailedAt != nil {
		failedAt = inv.FailedAt.UTC().Format(time.RFC3339)
		failedReason = inv.FailedReason
	}
	return map[string]interface{}{
		"id":                   inv.ID,
		"node_id":              inv.NodeID,
		"login":                login,
		"email":                email,
		"role":                 inv.Role,
		"created_at":           inv.CreatedAt.UTC().Format(time.RFC3339),
		"failed_at":            failedAt,
		"failed_reason":        failedReason,
		"inviter":              inviter,
		"team_count":           len(inv.TeamIDs),
		"invitation_teams_url": baseURL + "/api/v3/orgs/" + org.Login + "/invitations/" + strconv.Itoa(inv.ID) + "/teams",
		"invitation_source":    inv.Source,
	}
}

func (s *Server) handleListOrgInvitations(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	roleFilter := r.URL.Query().Get("role")
	switch roleFilter {
	case "", "all", "admin", "direct_member", "billing_manager", "hiring_manager":
	default:
		store.WriteGHValidationError(w, "OrganizationInvitation", "role", "invalid")
		return
	}
	sourceFilter := r.URL.Query().Get("invitation_source")
	switch sourceFilter {
	case "", "all", "member", "scim":
	default:
		store.WriteGHValidationError(w, "OrganizationInvitation", "invitation_source", "invalid")
		return
	}

	invitations := s.store.ListPendingOrgInvitations(org.Login)
	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(invitations))
	for _, inv := range invitations {
		if roleFilter != "" && roleFilter != "all" && inv.Role != roleFilter {
			continue
		}
		if sourceFilter != "" && sourceFilter != "all" && inv.Source != sourceFilter {
			continue
		}
		result = append(result, s.orgInvitationJSON(inv, org, base))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleCreateOrgInvitation(w http.ResponseWriter, r *http.Request) {
	org, user := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}

	var req struct {
		InviteeID flexInt      `json:"invitee_id"`
		Email     string       `json:"email"`
		Role      string       `json:"role"`
		TeamIDs   flexIntSlice `json:"team_ids"`
	}
	if !decodeJSONBodyOptional(w, r, &req) {
		return
	}

	role := req.Role
	if role == "" {
		role = "direct_member"
	}
	switch role {
	case "direct_member", "admin", "billing_manager":
	case "reinstate":
		// bleephub keeps no record of removed members' previous roles, so
		// a reinstate invitation has no role to restore.
		writeGHError(w, http.StatusUnprocessableEntity, "Invitee was not previously a member of this organization, so there is no role to reinstate.")
		return
	default:
		store.WriteGHValidationError(w, "OrganizationInvitation", "role", "invalid")
		return
	}

	if req.InviteeID == 0 && req.Email == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "One of invitee_id or email is required.")
		return
	}
	if req.InviteeID != 0 && req.Email != "" {
		writeGHError(w, http.StatusUnprocessableEntity, "Only one of invitee_id or email may be specified.")
		return
	}

	var invitee *store.User
	email := req.Email
	if req.InviteeID != 0 {
		invitee = s.store.GetUserByID(int(req.InviteeID))
		if invitee == nil {
			store.WriteGHValidationError(w, "OrganizationInvitation", "invitee_id", "invalid")
			return
		}
	} else if u := s.store.LookupUserByEmail(email); u != nil {
		// An email invitation addressed to an existing account resolves to
		// that account, exactly as real GitHub links the invite.
		invitee = u
	}

	if invitee != nil && s.store.IsUserBlockedByOrg(org.Login, invitee.ID) {
		writeGHError(w, http.StatusUnprocessableEntity, "Invitee is blocked from this organization.")
		return
	}

	teamIDs := make([]int, 0, len(req.TeamIDs))
	for _, id := range req.TeamIDs {
		team := s.store.GetTeamByID(id)
		if team == nil || team.OrgID != org.ID {
			store.WriteGHValidationError(w, "OrganizationInvitation", "team_ids", "invalid")
			return
		}
		teamIDs = append(teamIDs, id)
	}

	inv, reason := s.store.CreateOrgInvitation(org, user, invitee, email, role, teamIDs)
	if inv == nil {
		writeGHError(w, http.StatusUnprocessableEntity, reason)
		return
	}
	if invitee != nil {
		if m := s.store.GetMembership(org.Login, invitee.ID); m != nil {
			s.emitOrgMembershipEvent(org, "member_invited", m, invitee, user)
		}
	}
	s.recordAuditEvent("org.invite_member", user.Login, org.Login, map[string]interface{}{"invitation_id": inv.ID, "role": role, "email": inv.Email})
	writeJSON(w, http.StatusCreated, s.orgInvitationJSON(inv, org, s.baseURL(r)))
}

func (s *Server) handleCancelOrgInvitation(w http.ResponseWriter, r *http.Request) {
	org, user := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("invitation_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.store.CancelOrgInvitation(org.Login, id) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.recordAuditEvent("org.cancel_invitation", user.Login, org.Login, map[string]interface{}{"invitation_id": id})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListOrgInvitationTeams(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("invitation_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	inv := s.store.GetOrgInvitation(org.Login, id)
	if inv == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(inv.TeamIDs))
	for _, teamID := range inv.TeamIDs {
		if team := s.store.GetTeamByID(teamID); team != nil {
			result = append(result, teamSimpleJSON(team, org, s.store, base))
		}
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleListFailedOrgInvitations(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	invitations := s.store.ListFailedOrgInvitations(org.Login)
	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(invitations))
	for _, inv := range invitations {
		result = append(result, s.orgInvitationJSON(inv, org, base))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

// handleListTeamInvitations — GET /api/v3/orgs/{org}/teams/{team_slug}/invitations:
// the org's pending invitations that carry this team.
func (s *Server) handleListTeamInvitations(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgMember(w, r)
	if org == nil {
		return
	}
	team := s.store.GetTeam(org.Login, r.PathValue("team_slug"))
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	invitations := s.store.ListPendingOrgInvitationsForTeam(org.Login, team.ID)
	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(invitations))
	for _, inv := range invitations {
		result = append(result, s.orgInvitationJSON(inv, org, base))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

// --- outside collaborators ---

func (s *Server) handleListOutsideCollaborators(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgMember(w, r)
	if org == nil {
		return
	}
	filter := r.URL.Query().Get("filter")
	switch filter {
	case "", "all", "2fa_disabled", "2fa_insecure":
	default:
		store.WriteGHValidationError(w, "OutsideCollaborator", "filter", "invalid")
		return
	}
	collaborators := s.store.ListOutsideCollaborators(org.Login)
	// bleephub has no two-factor-authentication model, so every account
	// genuinely lacks 2FA: 2fa_disabled matches everyone and 2fa_insecure
	// (insecure 2FA methods) matches no one.
	if filter == "2fa_insecure" {
		collaborators = nil
	}
	result := make([]map[string]interface{}, 0, len(collaborators))
	for _, u := range collaborators {
		result = append(result, store.UserToJSON(u, s.baseURL(r)))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleConvertMemberToOutsideCollaborator(w http.ResponseWriter, r *http.Request) {
	org, user := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	target := s.store.LookupUserByLogin(r.PathValue("username"))
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	m := s.store.GetMembership(org.Login, target.ID)
	if m == nil || m.State != store.MembershipStateActive {
		writeGHError(w, http.StatusNotFound, r.PathValue("username")+" is not a member of the "+org.Login+" organization.")
		return
	}
	if m.Role == store.OrgRoleAdmin {
		writeGHError(w, http.StatusForbidden, "Cannot convert an organization owner to an outside collaborator.")
		return
	}
	// The converted member keeps the repository access their team
	// memberships confer, materialized as direct collaborator grants,
	// then loses the membership itself.
	s.store.GrantTeamRepoAccessAsCollaborator(org.Login, target)
	s.store.RemoveMembership(org.Login, target.ID)
	s.emitOrgMembershipEvent(org, "member_removed", m, target, user)
	s.recordAuditEvent("org.convert_member_to_outside_collaborator", user.Login, org.Login, map[string]interface{}{"user": target.Login})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveOutsideCollaborator(w http.ResponseWriter, r *http.Request) {
	org, user := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	target := s.store.LookupUserByLogin(r.PathValue("username"))
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if m := s.store.GetMembership(org.Login, target.ID); m != nil && m.State == store.MembershipStateActive {
		writeGHError(w, http.StatusUnprocessableEntity, "You cannot specify an organization member to remove as an outside collaborator.")
		return
	}
	s.store.RemoveOutsideCollaborator(org.Login, target.Login)
	s.recordAuditEvent("org.remove_outside_collaborator", user.Login, org.Login, map[string]interface{}{"user": target.Login})
	w.WriteHeader(http.StatusNoContent)
}

// --- organization user blocks ---

func (s *Server) handleListOrgBlocks(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	blocked := s.store.ListOrgBlockedUsers(org.Login)
	result := make([]map[string]interface{}, 0, len(blocked))
	for _, u := range blocked {
		result = append(result, store.UserToJSON(u, s.baseURL(r)))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleCheckOrgBlock(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	target := s.store.LookupUserByLogin(r.PathValue("username"))
	if target == nil || !s.store.IsUserBlockedByOrg(org.Login, target.ID) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBlockOrgUser(w http.ResponseWriter, r *http.Request) {
	org, user := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	target := s.store.LookupUserByLogin(r.PathValue("username"))
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if target.ID == user.ID {
		writeGHError(w, http.StatusUnprocessableEntity, "You cannot block yourself.")
		return
	}
	if m := s.store.GetMembership(org.Login, target.ID); m != nil && m.State == store.MembershipStateActive {
		writeGHError(w, http.StatusUnprocessableEntity, "You cannot block a member of this organization.")
		return
	}
	s.store.BlockUserForOrg(org.Login, target.ID)
	s.recordAuditEvent("org.block_user", user.Login, org.Login, map[string]interface{}{"blocked_user": target.Login})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUnblockOrgUser(w http.ResponseWriter, r *http.Request) {
	org, user := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	target := s.store.LookupUserByLogin(r.PathValue("username"))
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.UnblockUserForOrg(org.Login, target.ID)
	s.recordAuditEvent("org.unblock_user", user.Login, org.Login, map[string]interface{}{"blocked_user": target.Login})
	w.WriteHeader(http.StatusNoContent)
}

// --- organization interaction limits ---

// orgInteractionExpiryDurations maps GitHub's interaction-expiry enum to
// the concrete durations the restrictions run for.
var orgInteractionExpiryDurations = map[string]time.Duration{
	"one_day":    24 * time.Hour,
	"three_days": 3 * 24 * time.Hour,
	"one_week":   7 * 24 * time.Hour,
	"one_month":  30 * 24 * time.Hour,
	"six_months": 180 * 24 * time.Hour,
}

func orgInteractionLimitJSON(lim *store.OrgInteractionLimit) map[string]interface{} {
	return map[string]interface{}{
		"limit":      lim.Limit,
		"origin":     "organization",
		"expires_at": lim.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

func (s *Server) handleGetOrgInteractionLimits(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	lim := s.store.GetOrgInteractionLimit(org.Login)
	if lim == nil {
		// No active restriction reads as an empty object, per the
		// documented anyOf response.
		writeJSON(w, http.StatusOK, map[string]interface{}{})
		return
	}
	writeJSON(w, http.StatusOK, orgInteractionLimitJSON(lim))
}

func (s *Server) handleSetOrgInteractionLimits(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	var req struct {
		Limit  string `json:"limit"`
		Expiry string `json:"expiry"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	switch req.Limit {
	case "existing_users", "contributors_only", "collaborators_only":
	default:
		store.WriteGHValidationError(w, "InteractionLimit", "limit", "invalid")
		return
	}
	expiry := req.Expiry
	if expiry == "" {
		expiry = "one_day"
	}
	duration, ok := orgInteractionExpiryDurations[expiry]
	if !ok {
		store.WriteGHValidationError(w, "InteractionLimit", "expiry", "invalid")
		return
	}
	lim := s.store.SetOrgInteractionLimit(org.Login, req.Limit, s.currentTime().Add(duration))
	writeJSON(w, http.StatusOK, orgInteractionLimitJSON(lim))
}

func (s *Server) handleDeleteOrgInteractionLimits(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	s.store.DeleteOrgInteractionLimit(org.Login)
	w.WriteHeader(http.StatusNoContent)
}

// --- organization roles ---

// predefinedOrgRole is one entry of GitHub's predefined organization
// role catalog served by GET /orgs/{org}/organization-roles.
type predefinedOrgRole struct {
	ID          int
	Name        string
	Description string
	BaseRole    string
	Permissions []string
}

type organizationRoleView struct {
	ID          int
	Name        string
	Description *string
	BaseRole    *string
	Source      string
	Permissions []string
	OrgLogin    string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// securityManagerOrgRoleID is the predefined security_manager role —
// the role the deprecated security-managers endpoints alias.
const securityManagerOrgRoleID = 143

// predefinedOrgRoles is GitHub's predefined organization role catalog:
// the five all-repository access roles (which grant only their base
// repository role, hence empty permission lists) plus security_manager.
// The IDs are bleephub's stable predefined-role identifiers, mirroring
// github.com's numbering for the all-repository family.
var predefinedOrgRoles = []predefinedOrgRole{
	{ID: 138, Name: "all_repo_read", Description: "Grants read access to all repositories in the organization.", BaseRole: "read"},
	{ID: 139, Name: "all_repo_triage", Description: "Grants triage access to all repositories in the organization.", BaseRole: "triage"},
	{ID: 140, Name: "all_repo_write", Description: "Grants write access to all repositories in the organization.", BaseRole: "write"},
	{ID: 141, Name: "all_repo_maintain", Description: "Grants maintenance access to all repositories in the organization.", BaseRole: "maintain"},
	{ID: 142, Name: "all_repo_admin", Description: "Grants admin access to all repositories in the organization.", BaseRole: "admin"},
	{ID: securityManagerOrgRoleID, Name: "security_manager", Description: "Grants the ability to manage security policies, security alerts, and security configurations for an organization and all its repositories.", BaseRole: "read", Permissions: []string{"manage_security_products"}},
}

func predefinedOrgRoleByID(id int) *predefinedOrgRole {
	for i := range predefinedOrgRoles {
		if predefinedOrgRoles[i].ID == id {
			return &predefinedOrgRoles[i]
		}
	}
	return nil
}

// orgRoleJSON renders the GitHub `organization-role` shape. Predefined
// roles carry a null organization and exist from the organization's
// creation.
func predefinedOrgRoleView(role *predefinedOrgRole, org *store.Org) *organizationRoleView {
	description, baseRole := role.Description, role.BaseRole
	return &organizationRoleView{
		ID: role.ID, Name: role.Name, Description: &description, BaseRole: &baseRole,
		Source: "Predefined", Permissions: append([]string(nil), role.Permissions...),
		CreatedAt: org.CreatedAt, UpdatedAt: org.CreatedAt,
	}
}

func customOrgRoleView(role *store.OrgCustomOrganizationRole) *organizationRoleView {
	if role == nil {
		return nil
	}
	return &organizationRoleView{
		ID: role.ID, Name: role.Name, Description: role.Description, BaseRole: role.BaseRole,
		Source: "Organization", Permissions: append([]string(nil), role.Permissions...),
		OrgLogin: role.OrgLogin, CreatedAt: role.CreatedAt, UpdatedAt: role.UpdatedAt,
	}
}

func orgRoleJSON(role *organizationRoleView, org *store.Org, baseURL string) map[string]interface{} {
	permissions := role.Permissions
	if permissions == nil {
		permissions = []string{}
	}
	var organization interface{}
	if role.Source == "Organization" {
		organization = orgRoleOrganizationJSON(org, baseURL)
	}
	return map[string]interface{}{
		"id":           role.ID,
		"name":         role.Name,
		"description":  role.Description,
		"base_role":    role.BaseRole,
		"source":       role.Source,
		"permissions":  permissions,
		"organization": organization,
		"created_at":   role.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":   role.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// resolveOrgRoleID parses the {role_id} path parameter into a
// predefined or organization-defined role, writing a 404 when it doesn't
// resolve.
func (s *Server) resolveOrgRoleID(w http.ResponseWriter, r *http.Request, orgLogin string) *organizationRoleView {
	id, err := strconv.Atoi(r.PathValue("role_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	if role := predefinedOrgRoleByID(id); role != nil {
		org := s.store.GetOrg(orgLogin)
		if org != nil {
			return predefinedOrgRoleView(role, org)
		}
	}
	s.store.Mu.RLock()
	role := customOrgRoleView(s.store.OrgCustomRoles[orgLogin][id])
	s.store.Mu.RUnlock()
	if role != nil {
		return role
	}
	writeGHError(w, http.StatusNotFound, "Not Found")
	return nil
}

func (s *Server) handleListOrganizationRoles(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgMember(w, r)
	if org == nil {
		return
	}
	roles := make([]map[string]interface{}, 0, len(predefinedOrgRoles))
	for i := range predefinedOrgRoles {
		roles = append(roles, orgRoleJSON(predefinedOrgRoleView(&predefinedOrgRoles[i], org), org, s.baseURL(r)))
	}
	s.store.Mu.RLock()
	custom := make([]*store.OrgCustomOrganizationRole, 0, len(s.store.OrgCustomRoles[org.Login]))
	for _, role := range s.store.OrgCustomRoles[org.Login] {
		copyRole := *role
		copyRole.Permissions = append([]string(nil), role.Permissions...)
		custom = append(custom, &copyRole)
	}
	s.store.Mu.RUnlock()
	sort.Slice(custom, func(i, j int) bool { return custom[i].ID < custom[j].ID })
	for _, role := range custom {
		roles = append(roles, orgRoleJSON(customOrgRoleView(role), org, s.baseURL(r)))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count": len(roles),
		"roles":       roles,
	})
}

func (s *Server) handleGetOrganizationRole(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgMember(w, r)
	if org == nil {
		return
	}
	role := s.resolveOrgRoleID(w, r, org.Login)
	if role == nil {
		return
	}
	writeJSON(w, http.StatusOK, orgRoleJSON(role, org, s.baseURL(r)))
}

func (s *Server) handleListOrganizationRoleTeams(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgMember(w, r)
	if org == nil {
		return
	}
	role := s.resolveOrgRoleID(w, r, org.Login)
	if role == nil {
		return
	}
	teams := s.store.ListTeamsWithOrgRole(org.Login, role.ID)
	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(teams))
	for _, team := range teams {
		j := teamSimpleJSON(team, org, s.store, base)
		j["assignment"] = "direct"
		result = append(result, j)
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleListOrganizationRoleUsers(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgMember(w, r)
	if org == nil {
		return
	}
	role := s.resolveOrgRoleID(w, r, org.Login)
	if role == nil {
		return
	}
	assignments := s.store.ListUsersWithOrgRole(org.Login, role.ID)
	userIDs := make([]int, 0, len(assignments))
	for id := range assignments {
		userIDs = append(userIDs, id)
	}
	sort.Ints(userIDs)
	result := make([]map[string]interface{}, 0, len(userIDs))
	for _, id := range userIDs {
		u := s.store.GetUserByID(id)
		if u == nil {
			continue
		}
		j := store.UserToJSON(u, s.baseURL(r))
		j["assignment"] = assignments[id]
		result = append(result, j)
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleAssignOrganizationRoleToTeam(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	team := s.store.GetTeam(org.Login, r.PathValue("team_slug"))
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	role := s.resolveOrgRoleID(w, r, org.Login)
	if role == nil {
		return
	}
	s.store.AssignOrgRoleToTeam(org.Login, role.ID, team.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRevokeOrganizationRoleFromTeam(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	team := s.store.GetTeam(org.Login, r.PathValue("team_slug"))
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	role := s.resolveOrgRoleID(w, r, org.Login)
	if role == nil {
		return
	}
	s.store.UnassignOrgRoleFromTeam(org.Login, role.ID, team.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRevokeAllOrganizationRolesFromTeam(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	team := s.store.GetTeam(org.Login, r.PathValue("team_slug"))
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.UnassignAllOrgRolesFromTeam(org.Login, team.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAssignOrganizationRoleToUser(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	target := s.store.LookupUserByLogin(r.PathValue("username"))
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	role := s.resolveOrgRoleID(w, r, org.Login)
	if role == nil {
		return
	}
	if m := s.store.GetMembership(org.Login, target.ID); m == nil || m.State != store.MembershipStateActive {
		writeGHError(w, http.StatusUnprocessableEntity, "User must be an active member of the organization to be assigned an organization role.")
		return
	}
	s.store.AssignOrgRoleToUser(org.Login, role.ID, target.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRevokeOrganizationRoleFromUser(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	target := s.store.LookupUserByLogin(r.PathValue("username"))
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	role := s.resolveOrgRoleID(w, r, org.Login)
	if role == nil {
		return
	}
	s.store.UnassignOrgRoleFromUser(org.Login, role.ID, target.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRevokeAllOrganizationRolesFromUser(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	target := s.store.LookupUserByLogin(r.PathValue("username"))
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.UnassignAllOrgRolesFromUser(org.Login, target.ID)
	w.WriteHeader(http.StatusNoContent)
}

// --- security managers ---

func (s *Server) handleListSecurityManagerTeams(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgMember(w, r)
	if org == nil {
		return
	}
	teams := s.store.ListTeamsWithOrgRole(org.Login, securityManagerOrgRoleID)
	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(teams))
	for _, team := range teams {
		result = append(result, teamRefJSON(team, org, base))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleAddSecurityManagerTeam(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	team := s.store.GetTeam(org.Login, r.PathValue("team_slug"))
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.AssignOrgRoleToTeam(org.Login, securityManagerOrgRoleID, team.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveSecurityManagerTeam(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	team := s.store.GetTeam(org.Login, r.PathValue("team_slug"))
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.UnassignOrgRoleFromTeam(org.Login, securityManagerOrgRoleID, team.ID)
	w.WriteHeader(http.StatusNoContent)
}

// --- org-wide security-product enablement ---

// orgSecurityProductRepoFlags maps the security products bleephub
// models per-repository onto their Repo flag fields: Dependabot alerts
// are the vulnerability-alerts setting and Dependabot security updates
// are the automated-security-fixes setting — the same state the
// /repos/{owner}/{repo}/vulnerability-alerts and
// /repos/{owner}/{repo}/automated-security-fixes endpoints flip.
var orgSecurityProductRepoFlags = map[string]string{
	"dependabot_alerts":           "vulnerability_alerts_enabled",
	"dependabot_security_updates": "automated_security_fixes_enabled",
}

// orgSecurityProductsUnavailable lists the documented security products
// bleephub has no per-repository setting for; enabling them fails the
// same way a GitHub Enterprise Server instance without the feature's
// licensing does.
var orgSecurityProductsUnavailable = map[string]string{
	"dependency_graph":                "Dependency graph is not available for this organization.",
	"advanced_security":               "GitHub Advanced Security is not available for this organization.",
	"code_scanning_default_setup":     "Code scanning default setup is not available for this organization.",
	"secret_scanning":                 "Secret scanning is not available for this organization.",
	"secret_scanning_push_protection": "Secret scanning push protection is not available for this organization.",
}

// handleOrgSecurityProductEnablement — POST /api/v3/orgs/{org}/{security_product}/{enablement}.
func (s *Server) handleOrgSecurityProductEnablement(w http.ResponseWriter, r *http.Request) {
	product := r.PathValue("security_product")
	enablement := r.PathValue("enablement")
	_, flagged := orgSecurityProductRepoFlags[product]
	_, unavailable := orgSecurityProductsUnavailable[product]
	if !flagged && !unavailable {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if enablement != "enable_all" && enablement != "disable_all" {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	org, user := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	if unavailable {
		writeGHError(w, http.StatusUnprocessableEntity, orgSecurityProductsUnavailable[product])
		return
	}
	enable := enablement == "enable_all"
	field := orgSecurityProductRepoFlags[product]
	for _, repo := range s.store.ListReposForOrg(org.Login, store.RepoListOptions{NoPaginate: true}) {
		s.store.SetRepoFlag(repo.ID, field, enable)
	}
	s.recordAuditEvent("org.security_product_enablement", user.Login, org.Login, map[string]interface{}{"security_product": product, "enablement": enablement})
	w.WriteHeader(http.StatusNoContent)
}

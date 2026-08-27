package bleephub

import (
	"context"
	"net/http"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHMemberRoutes() {
	s.route("GET /api/v3/orgs/{org}/members", s.handleListOrgMembers)
	s.route("GET /api/v3/orgs/{org}/members/{username}", s.handleCheckOrgMember)
	s.route("DELETE /api/v3/orgs/{org}/members/{username}", s.requirePerm(store.ScopeMembers, store.PermWrite, s.handleRemoveOrgMember))
	s.route("GET /api/v3/orgs/{org}/public_members", s.handleListPublicOrgMembers)
	s.route("GET /api/v3/orgs/{org}/public_members/{username}", s.handleCheckPublicOrgMember)
	s.route("PUT /api/v3/orgs/{org}/public_members/{username}", s.handlePublicizeOrgMembership)
	s.route("DELETE /api/v3/orgs/{org}/public_members/{username}", s.handleConcealOrgMembership)
	s.route("GET /api/v3/orgs/{org}/memberships/{username}", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleGetOrgMembership))
	s.route("PUT /api/v3/orgs/{org}/memberships/{username}", s.requirePerm(store.ScopeMembers, store.PermWrite, s.handleSetOrgMembership))
	s.route("DELETE /api/v3/orgs/{org}/memberships/{username}", s.requirePerm(store.ScopeMembers, store.PermWrite, s.handleRemoveOrgMembership))

	// The authenticated user's own memberships (invitee side: list, inspect, accept).
	s.route("GET /api/v3/user/memberships/orgs", s.handleListAuthUserMemberships)
	s.route("GET /api/v3/user/memberships/orgs/{org}", s.handleGetAuthUserMembership)
	s.route("PATCH /api/v3/user/memberships/orgs/{org}", s.handleUpdateAuthUserMembership)
}

func (s *Server) handleListOrgMembers(w http.ResponseWriter, r *http.Request) {
	orgLogin := r.PathValue("org")
	org := s.store.GetOrg(orgLogin)
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// Non-members and anonymous callers see only publicized members.
	// ?role (all|admin|member) and ?filter (all|2fa_disabled) per GitHub.
	role := r.URL.Query().Get("role")
	if role == "" {
		role = "all"
	}
	if role != "all" && role != "admin" && role != "member" {
		store.WriteGHValidationError(w, "Membership", "role", "invalid")
		return
	}
	if filter := r.URL.Query().Get("filter"); filter != "" && filter != "all" && filter != "2fa_disabled" {
		store.WriteGHValidationError(w, "Membership", "filter", "invalid")
		return
	}

	var members []*store.User
	if s.viewerCanReadOrgMembers(r.Context(), orgLogin) {
		members = s.store.ListOrgMembers(orgLogin)
	} else {
		members = s.store.ListPublicOrgMembers(orgLogin)
	}
	result := make([]map[string]interface{}, 0, len(members))
	for _, u := range members {
		if role != "all" {
			m := s.store.GetMembership(orgLogin, u.ID)
			if m == nil || string(m.Role) != role {
				continue
			}
		}
		result = append(result, store.UserToJSON(u, s.baseURL(r)))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

// viewerCanReadOrgMembers reports whether the caller may see the full member
// list: an org member, or an installation holding Members:read.
func (s *Server) viewerCanReadOrgMembers(ctx context.Context, orgLogin string) bool {
	if ghInstallationTokenFromContext(ctx) != nil {
		return s.credentialGrantsAccount(ctx, store.OrganizationAccount, orgLogin, store.ScopeMembers, store.PermRead)
	}
	return s.viewerIsOrgMember(ctx, orgLogin)
}

func (s *Server) handleGetOrgMembership(w http.ResponseWriter, r *http.Request) {
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

	username := r.PathValue("username")
	target := s.store.LookupUserByLogin(username)
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	m := s.store.GetMembership(orgLogin, target.ID)
	if m == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	writeJSON(w, http.StatusOK, membershipToJSON(m, target, org, s.baseURL(r)))
}

func (s *Server) handleSetOrgMembership(w http.ResponseWriter, r *http.Request) {
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

	if !s.viewerCanAdminOrg(r.Context(), org.Login) {
		writeGHError(w, http.StatusForbidden, "Must be an organization owner.")
		return
	}

	username := r.PathValue("username")
	target := s.store.LookupUserByLogin(username)
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	// An enterprise that requires 2FA rejects a member who has not enrolled a second factor.
	if s.enterpriseRequiresTwoFactor(org) && !s.store.TwoFactorEnabled(target.ID) {
		writeGHError(w, http.StatusUnprocessableEntity,
			// target.Login is the store's spelling, not the request's, so no request string reaches the body.
			"Validation Failed: "+target.Login+" is not enrolled in two-factor authentication, which an enterprise policy requires.")
		return
	}

	var req struct {
		Role string `json:"role"`
	}
	if !decodeJSONBodyOptional(w, r, &req) {
		return
	}
	role := store.OrgRole(req.Role)
	if role == "" {
		role = store.OrgRoleMember
	}
	if role != store.OrgRoleAdmin && role != store.OrgRoleMember {
		store.WriteGHValidationError(w, "Membership", "role", "invalid")
		return
	}

	// Adding a new member creates a pending invitation (accepted via PATCH
	// /user/memberships/orgs/{org}); updating an existing one only changes the
	// role. Self-PUT by an existing member stays active.
	existing := s.store.GetMembership(orgLogin, target.ID)
	state := store.MembershipStatePending
	if existing != nil {
		state = existing.State
	} else if target.ID == user.ID {
		state = store.MembershipStateActive
	}
	m := s.store.SetMembership(orgLogin, target.ID, role, state)
	if m == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if existing == nil {
		action := "member_invited"
		if state == store.MembershipStateActive {
			action = "member_added"
		}
		s.emitOrgMembershipEvent(org, action, m, target, user)
	}

	writeJSON(w, http.StatusOK, membershipToJSON(m, target, org, s.baseURL(r)))
}

func (s *Server) handleRemoveOrgMembership(w http.ResponseWriter, r *http.Request) {
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

	if !s.viewerCanAdminOrg(r.Context(), org.Login) {
		writeGHError(w, http.StatusForbidden, "Must be an organization owner.")
		return
	}

	username := r.PathValue("username")
	target := s.store.LookupUserByLogin(username)
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	m := s.store.GetMembership(orgLogin, target.ID)
	if m == nil || !s.store.RemoveMembership(orgLogin, target.ID) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.emitOrgMembershipEvent(org, "member_removed", m, target, user)

	w.WriteHeader(http.StatusNoContent)
}

// handleCheckOrgMember — GET /api/v3/orgs/{org}/members/{username}. 204 for an
// active member, 404 otherwise. (GitHub's 302 non-member-requester variant does
// not apply: bleephub requesters are unscoped.)
func (s *Server) handleCheckOrgMember(w http.ResponseWriter, r *http.Request) {
	if ghUserFromContext(r.Context()) == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	orgLogin := r.PathValue("org")
	if s.store.GetOrg(orgLogin) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	target := s.store.LookupUserByLogin(r.PathValue("username"))
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	m := s.store.GetMembership(orgLogin, target.ID)
	if m == nil || m.State != store.MembershipStateActive {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRemoveOrgMember — DELETE /api/v3/orgs/{org}/members/{username}.
// Removes the member and their team memberships; 404s for a non-member.
func (s *Server) handleRemoveOrgMember(w http.ResponseWriter, r *http.Request) {
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
	if !s.viewerCanAdminOrg(r.Context(), org.Login) {
		writeGHError(w, http.StatusForbidden, "Must be an organization owner.")
		return
	}
	target := s.store.LookupUserByLogin(r.PathValue("username"))
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	m := s.store.GetMembership(orgLogin, target.ID)
	if m == nil || !s.store.RemoveMembership(orgLogin, target.ID) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.emitOrgMembershipEvent(org, "member_removed", m, target, user)
	w.WriteHeader(http.StatusNoContent)
}

// handleListPublicOrgMembers — GET /api/v3/orgs/{org}/public_members. Anonymous-readable.
func (s *Server) handleListPublicOrgMembers(w http.ResponseWriter, r *http.Request) {
	orgLogin := r.PathValue("org")
	if s.store.GetOrg(orgLogin) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	members := s.store.ListPublicOrgMembers(orgLogin)
	result := make([]map[string]interface{}, 0, len(members))
	for _, u := range members {
		result = append(result, store.UserToJSON(u, s.baseURL(r)))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

// handleCheckPublicOrgMember — GET /api/v3/orgs/{org}/public_members/{username}.
func (s *Server) handleCheckPublicOrgMember(w http.ResponseWriter, r *http.Request) {
	orgLogin := r.PathValue("org")
	if s.store.GetOrg(orgLogin) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	target := s.store.LookupUserByLogin(r.PathValue("username"))
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	m := s.store.GetMembership(orgLogin, target.ID)
	if m == nil || m.State != store.MembershipStateActive || !m.Public {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePublicizeOrgMembership — PUT /api/v3/orgs/{org}/public_members/{username}.
// Only the user's own membership may be publicized (403 otherwise).
func (s *Server) handlePublicizeOrgMembership(w http.ResponseWriter, r *http.Request) {
	s.setMembershipVisibility(w, r, true)
}

// handleConcealOrgMembership — DELETE /api/v3/orgs/{org}/public_members/{username}.
func (s *Server) handleConcealOrgMembership(w http.ResponseWriter, r *http.Request) {
	s.setMembershipVisibility(w, r, false)
}

func (s *Server) setMembershipVisibility(w http.ResponseWriter, r *http.Request, public bool) {
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
	target := s.store.LookupUserByLogin(r.PathValue("username"))
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if target.ID != user.ID {
		writeGHError(w, http.StatusForbidden, "You can only publicize or conceal your own membership.")
		return
	}
	if !s.store.SetMembershipPublic(orgLogin, target.ID, public) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListAuthUserMemberships — GET /api/v3/user/memberships/orgs. Filters on ?state (active | pending).
func (s *Server) handleListAuthUserMemberships(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	state := store.MembershipState(r.URL.Query().Get("state"))
	switch state {
	case "", store.MembershipStateActive, store.MembershipStatePending:
	default:
		store.WriteGHValidationError(w, "Membership", "state", "invalid")
		return
	}
	memberships := s.store.ListMembershipsByUser(user.ID, state)
	base := s.baseURL(r)
	result := make([]map[string]interface{}, 0, len(memberships))
	for _, m := range memberships {
		org := s.store.GetOrgByID(m.OrgID)
		if org == nil {
			continue
		}
		result = append(result, membershipToJSON(m, user, org, base))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

// handleGetAuthUserMembership — GET /api/v3/user/memberships/orgs/{org}.
func (s *Server) handleGetAuthUserMembership(w http.ResponseWriter, r *http.Request) {
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
	m := s.store.GetMembership(orgLogin, user.ID)
	if m == nil || !s.viewerReachesOrg(r.Context(), orgLogin) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, membershipToJSON(m, user, org, s.baseURL(r)))
}

// handleUpdateAuthUserMembership — PATCH /api/v3/user/memberships/orgs/{org}.
// The accept half of the invitation flow: {"state":"active"} turns a pending membership active.
func (s *Server) handleUpdateAuthUserMembership(w http.ResponseWriter, r *http.Request) {
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
	var req struct {
		State string `json:"state"`
	}
	if !decodeJSONBodyOptional(w, r, &req) {
		return
	}
	if store.MembershipState(req.State) != store.MembershipStateActive {
		store.WriteGHValidationError(w, "Membership", "state", "invalid")
		return
	}
	m := s.store.GetMembership(orgLogin, user.ID)
	if m == nil || !s.viewerReachesOrg(r.Context(), orgLogin) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if m.State == store.MembershipStatePending {
		m = s.store.SetMembership(orgLogin, user.ID, m.Role, store.MembershipStateActive)
		s.emitOrgMembershipEvent(org, "member_added", m, user, user)
	}
	writeJSON(w, http.StatusOK, membershipToJSON(m, user, org, s.baseURL(r)))
}

// membershipToJSON renders a Membership as the GitHub `org-membership` shape.
func membershipToJSON(m *store.Membership, user *store.User, org *store.Org, baseURL string) map[string]interface{} {
	return map[string]interface{}{
		"url":              baseURL + "/api/v3/orgs/" + org.Login + "/memberships/" + user.Login,
		"organization_url": baseURL + "/api/v3/orgs/" + org.Login,
		"state":            m.State,
		"role":             m.Role,
		"user":             store.UserToJSON(user, baseURL),
		"organization":     orgSimpleJSON(org, baseURL),
	}
}

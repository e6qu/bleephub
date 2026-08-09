package bleephub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// OrgCustomRepositoryRole is an organization-defined repository role.
type OrgCustomRepositoryRole struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	BaseRole    string    `json:"base_role"`
	Permissions []string  `json:"permissions"`
	OrgLogin    string    `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// OrgCustomOrganizationRole is an organization-defined organization role.
type OrgCustomOrganizationRole struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	BaseRole    *string   `json:"base_role"`
	Permissions []string  `json:"permissions"`
	OrgLogin    string    `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type fineGrainedPermission struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type nullableStringUpdate struct {
	Set   bool
	Value *string
}

func (value *nullableStringUpdate) UnmarshalJSON(raw []byte) error {
	value.Set = true
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		value.Value = nil
		return nil
	}
	var decoded string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	value.Value = &decoded
	return nil
}

// These catalogs are deliberately centralized: role validation and all three
// discovery endpoints consume the same definitions, so an advertised
// permission can always be used and an unadvertised one cannot leak through.
var repositoryFineGrainedPermissions = []fineGrainedPermission{
	{Name: "add_assignee", Description: "Assign or remove a user"},
	{Name: "remove_assignee", Description: "Remove an assigned user"},
	{Name: "add_label", Description: "Add or remove a label"},
	{Name: "remove_label", Description: "Remove a label"},
	{Name: "bypass_branch_protection", Description: "Push commits to protected branches"},
	{Name: "close_issue", Description: "Close an issue"},
	{Name: "close_pull_request", Description: "Close a pull request"},
	{Name: "create_tag", Description: "Create a tag"},
	{Name: "delete_tag", Description: "Delete a tag"},
	{Name: "manage_deployments", Description: "Manage deployments"},
	{Name: "manage_merge_queue", Description: "Manage the merge queue"},
	{Name: "manage_repo_access", Description: "Manage repository access"},
	{Name: "manage_security_products", Description: "Manage security products"},
	{Name: "mark_as_duplicate", Description: "Mark issues and pull requests as duplicates"},
	{Name: "request_pr_review", Description: "Request pull request reviews"},
	{Name: "resolve_review_thread", Description: "Resolve pull request review threads"},
	{Name: "view_secret_scanning_alerts", Description: "View secret scanning alerts"},
	{Name: "write_code_scanning", Description: "Manage code scanning settings and alerts"},
	{Name: "write_dependabot", Description: "Manage Dependabot settings and alerts"},
}

var organizationFineGrainedPermissions = []fineGrainedPermission{
	{Name: "read_organization_custom_org_role", Description: "View organization roles"},
	{Name: "write_organization_custom_org_role", Description: "Manage custom organization roles"},
	{Name: "read_organization_custom_properties", Description: "View organization custom properties"},
	{Name: "write_organization_custom_properties", Description: "Manage organization custom properties"},
	{Name: "read_organization_members", Description: "View organization members"},
	{Name: "write_organization_members", Description: "Manage organization members"},
	{Name: "read_organization_actions", Description: "View organization Actions settings"},
	{Name: "write_organization_actions", Description: "Manage organization Actions settings"},
	{Name: "read_organization_hooks", Description: "View organization webhooks"},
	{Name: "write_organization_hooks", Description: "Manage organization webhooks"},
}

func (s *Server) registerGHOrgGovernanceRoutes() {
	s.route("GET /api/v3/orgs/{org}/announcement", s.requirePerm(scopeOrgAdministration, permRead, s.handleGetOrgAnnouncement))
	s.route("PATCH /api/v3/orgs/{org}/announcement", s.requirePerm(scopeOrgAdministration, permWrite, s.handleSetOrgAnnouncement))
	s.route("DELETE /api/v3/orgs/{org}/announcement", s.requirePerm(scopeOrgAdministration, permWrite, s.handleDeleteOrgAnnouncement))

	s.route("GET /api/v3/orgs/{org}/custom-repository-roles", s.requirePerm(scopeOrgAdministration, permRead, s.handleListCustomRepositoryRoles))
	s.route("POST /api/v3/orgs/{org}/custom-repository-roles", s.requirePerm(scopeOrgAdministration, permWrite, s.handleCreateCustomRepositoryRole))
	s.route("GET /api/v3/orgs/{org}/custom-repository-roles/{role_id}", s.requirePerm(scopeOrgAdministration, permRead, s.handleGetCustomRepositoryRole))
	s.route("PATCH /api/v3/orgs/{org}/custom-repository-roles/{role_id}", s.requirePerm(scopeOrgAdministration, permWrite, s.handleUpdateCustomRepositoryRole))
	s.route("DELETE /api/v3/orgs/{org}/custom-repository-roles/{role_id}", s.requirePerm(scopeOrgAdministration, permWrite, s.handleDeleteCustomRepositoryRole))

	// Deprecated aliases remain part of GitHub's Enterprise Cloud contract.
	s.route("GET /api/v3/organizations/{organization_id}/custom_roles", s.requirePerm(scopeOrgAdministration, permRead, s.handleListCustomRepositoryRolesByOrgID))
	s.route("POST /api/v3/orgs/{org}/custom_roles", s.requirePerm(scopeOrgAdministration, permWrite, s.handleCreateCustomRepositoryRole))
	s.route("GET /api/v3/orgs/{org}/custom_roles/{role_id}", s.requirePerm(scopeOrgAdministration, permRead, s.handleGetCustomRepositoryRole))
	s.route("PATCH /api/v3/orgs/{org}/custom_roles/{role_id}", s.requirePerm(scopeOrgAdministration, permWrite, s.handleUpdateCustomRepositoryRole))
	s.route("DELETE /api/v3/orgs/{org}/custom_roles/{role_id}", s.requirePerm(scopeOrgAdministration, permWrite, s.handleDeleteCustomRepositoryRole))

	s.route("GET /api/v3/orgs/{org}/fine_grained_permissions", s.requirePerm(scopeOrgAdministration, permRead, s.handleListRepositoryFineGrainedPermissions))
	s.route("GET /api/v3/orgs/{org}/repository-fine-grained-permissions", s.requirePerm(scopeOrgAdministration, permRead, s.handleListRepositoryFineGrainedPermissions))
	s.route("GET /api/v3/orgs/{org}/organization-fine-grained-permissions", s.requirePerm(scopeOrgAdministration, permRead, s.handleListOrganizationFineGrainedPermissions))
}

func copyAnnouncement(value *EnterpriseAnnouncement) EnterpriseAnnouncement {
	if value == nil {
		return EnterpriseAnnouncement{}
	}
	return *value
}

func (s *Server) handleGetOrgAnnouncement(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgMember(w, r)
	if org == nil {
		return
	}
	s.store.mu.RLock()
	announcement := copyAnnouncement(s.store.OrgAnnouncements[org.Login])
	s.store.mu.RUnlock()
	writeJSON(w, http.StatusOK, announcement)
}

func (s *Server) handleSetOrgAnnouncement(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	var req enterpriseAnnouncementRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Announcement == nil {
		writeGHValidationError(w, "Announcement", "announcement", "missing_field")
		return
	}
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, *req.ExpiresAt); err != nil {
			writeGHValidationError(w, "Announcement", "expires_at", "invalid")
			return
		}
	}
	announcement := &EnterpriseAnnouncement{Announcement: *req.Announcement}
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		expiry := *req.ExpiresAt
		announcement.ExpiresAt = &expiry
	}
	if req.UserDismissible != nil {
		announcement.UserDismissible = *req.UserDismissible
	}
	s.store.mu.Lock()
	s.store.OrgAnnouncements[org.Login] = announcement
	if s.store.persist != nil {
		s.store.persist.MustPut("org_announcements", org.Login, announcement)
	}
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, announcement)
}

func (s *Server) handleDeleteOrgAnnouncement(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	s.store.mu.Lock()
	delete(s.store.OrgAnnouncements, org.Login)
	if s.store.persist != nil {
		s.store.persist.MustDelete("org_announcements", org.Login)
	}
	s.store.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func orgRoleOrganizationJSON(org *Org, baseURL string) map[string]interface{} {
	apiURL := baseURL + "/api/v3"
	return map[string]interface{}{
		"login":               org.Login,
		"id":                  org.ID,
		"node_id":             org.NodeID,
		"avatar_url":          org.AvatarURL,
		"gravatar_id":         "",
		"url":                 apiURL + "/orgs/" + org.Login,
		"html_url":            baseURL + "/orgs/" + org.Login,
		"followers_url":       apiURL + "/users/" + org.Login + "/followers",
		"following_url":       apiURL + "/users/" + org.Login + "/following{/other_user}",
		"gists_url":           apiURL + "/users/" + org.Login + "/gists{/gist_id}",
		"starred_url":         apiURL + "/users/" + org.Login + "/starred{/owner}{/repo}",
		"subscriptions_url":   apiURL + "/users/" + org.Login + "/subscriptions",
		"organizations_url":   apiURL + "/users/" + org.Login + "/orgs",
		"repos_url":           apiURL + "/users/" + org.Login + "/repos",
		"events_url":          apiURL + "/users/" + org.Login + "/events{/privacy}",
		"received_events_url": apiURL + "/users/" + org.Login + "/received_events",
		"type":                "Organization",
		"site_admin":          false,
		"user_view_type":      "public",
	}
}

func customRepositoryRoleJSON(role *OrgCustomRepositoryRole, org *Org, baseURL string) map[string]interface{} {
	permissions := append([]string(nil), role.Permissions...)
	return map[string]interface{}{
		"id":           role.ID,
		"name":         role.Name,
		"description":  role.Description,
		"base_role":    role.BaseRole,
		"permissions":  permissions,
		"organization": orgRoleOrganizationJSON(org, baseURL),
		"created_at":   role.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":   role.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func validBaseRepositoryRole(value string) bool {
	switch value {
	case "read", "triage", "write", "maintain":
		return true
	default:
		return false
	}
}

func validPermissions(values []string, catalog []fineGrainedPermission) bool {
	allowed := make(map[string]struct{}, len(catalog))
	for _, permission := range catalog {
		allowed[permission.Name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, ok := allowed[value]; !ok {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

type customRepositoryRoleRequest struct {
	Name        *string              `json:"name"`
	Description nullableStringUpdate `json:"description"`
	BaseRole    *string              `json:"base_role"`
	Permissions *[]string            `json:"permissions"`
}

func validateCustomRepositoryRoleRequest(w http.ResponseWriter, req customRepositoryRoleRequest, create bool) bool {
	if create && req.Name == nil {
		writeGHValidationError(w, "CustomRepositoryRole", "name", "missing_field")
		return false
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		writeGHValidationError(w, "CustomRepositoryRole", "name", "invalid")
		return false
	}
	if create && req.BaseRole == nil {
		writeGHValidationError(w, "CustomRepositoryRole", "base_role", "missing_field")
		return false
	}
	if req.BaseRole != nil && !validBaseRepositoryRole(*req.BaseRole) {
		writeGHValidationError(w, "CustomRepositoryRole", "base_role", "invalid")
		return false
	}
	if create && req.Permissions == nil {
		writeGHValidationError(w, "CustomRepositoryRole", "permissions", "missing_field")
		return false
	}
	if req.Permissions != nil && !validPermissions(*req.Permissions, repositoryFineGrainedPermissions) {
		writeGHValidationError(w, "CustomRepositoryRole", "permissions", "invalid")
		return false
	}
	return true
}

func (st *Store) reserveOrgCustomRoleIDLocked() int {
	id := st.NextOrgCustomRoleID
	if st.persist != nil {
		reserved, err := st.persist.AllocateCounterValue("next_org_custom_role_id", int64(id))
		if err != nil {
			panic(&persistenceFailure{op: "counter", bucket: "counters", key: "next_org_custom_role_id", err: err})
		}
		id = int(reserved)
	}
	st.NextOrgCustomRoleID = id + 1
	return id
}

func (s *Server) listCustomRepositoryRoles(w http.ResponseWriter, r *http.Request, org *Org) {
	s.store.mu.RLock()
	roles := make([]*OrgCustomRepositoryRole, 0, len(s.store.OrgCustomRepoRoles[org.Login]))
	for _, role := range s.store.OrgCustomRepoRoles[org.Login] {
		copyRole := *role
		copyRole.Permissions = append([]string(nil), role.Permissions...)
		roles = append(roles, &copyRole)
	}
	s.store.mu.RUnlock()
	sort.Slice(roles, func(i, j int) bool { return roles[i].ID < roles[j].ID })
	result := make([]map[string]interface{}, 0, len(roles))
	for _, role := range roles {
		result = append(result, customRepositoryRoleJSON(role, org, s.baseURL(r)))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"total_count": len(result), "custom_roles": result})
}

func (s *Server) handleListCustomRepositoryRoles(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgMember(w, r)
	if org != nil {
		s.listCustomRepositoryRoles(w, r, org)
	}
}

func (s *Server) handleListCustomRepositoryRolesByOrgID(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("organization_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	org := s.store.GetOrgByID(id)
	if org == nil || !s.viewerIsOrgMember(r.Context(), org.Login) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.listCustomRepositoryRoles(w, r, org)
}

func (s *Server) handleCreateCustomRepositoryRole(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	var req customRepositoryRoleRequest
	if !decodeJSONBody(w, r, &req) || !validateCustomRepositoryRoleRequest(w, req, true) {
		return
	}
	now := s.currentTime()
	role := &OrgCustomRepositoryRole{
		Name: strings.TrimSpace(*req.Name), BaseRole: *req.BaseRole,
		Permissions: append([]string(nil), (*req.Permissions)...),
		OrgLogin:    org.Login, CreatedAt: now, UpdatedAt: now,
	}
	if req.Description.Set {
		role.Description = req.Description.Value
	}
	s.store.mu.Lock()
	if s.store.OrgCustomRepoRoles[org.Login] == nil {
		s.store.OrgCustomRepoRoles[org.Login] = map[int]*OrgCustomRepositoryRole{}
	}
	for _, existing := range s.store.OrgCustomRepoRoles[org.Login] {
		if strings.EqualFold(existing.Name, role.Name) {
			s.store.mu.Unlock()
			writeGHValidationError(w, "CustomRepositoryRole", "name", "already_exists")
			return
		}
	}
	role.ID = s.store.reserveOrgCustomRoleIDLocked()
	s.store.OrgCustomRepoRoles[org.Login][role.ID] = role
	if s.store.persist != nil {
		s.store.persist.MustPut("org_custom_repo_roles", org.Login, s.store.OrgCustomRepoRoles[org.Login])
	}
	s.store.mu.Unlock()
	writeJSON(w, http.StatusCreated, customRepositoryRoleJSON(role, org, s.baseURL(r)))
}

func parseRoleID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("role_id"))
	if err != nil || id <= 0 {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return 0, false
	}
	return id, true
}

func (s *Server) getCustomRepositoryRole(orgLogin string, id int) *OrgCustomRepositoryRole {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	role := s.store.OrgCustomRepoRoles[orgLogin][id]
	if role == nil {
		return nil
	}
	result := *role
	result.Permissions = append([]string(nil), role.Permissions...)
	return &result
}

func (s *Server) handleGetCustomRepositoryRole(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgMember(w, r)
	if org == nil {
		return
	}
	id, ok := parseRoleID(w, r)
	if !ok {
		return
	}
	role := s.getCustomRepositoryRole(org.Login, id)
	if role == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, customRepositoryRoleJSON(role, org, s.baseURL(r)))
}

func (s *Server) handleUpdateCustomRepositoryRole(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	id, ok := parseRoleID(w, r)
	if !ok {
		return
	}
	var req customRepositoryRoleRequest
	if !decodeJSONBody(w, r, &req) || !validateCustomRepositoryRoleRequest(w, req, false) {
		return
	}
	s.store.mu.Lock()
	role := s.store.OrgCustomRepoRoles[org.Login][id]
	if role == nil {
		s.store.mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if req.Name != nil {
		for existingID, existing := range s.store.OrgCustomRepoRoles[org.Login] {
			if existingID != id && strings.EqualFold(existing.Name, strings.TrimSpace(*req.Name)) {
				s.store.mu.Unlock()
				writeGHValidationError(w, "CustomRepositoryRole", "name", "already_exists")
				return
			}
		}
		role.Name = strings.TrimSpace(*req.Name)
	}
	if req.Description.Set {
		role.Description = req.Description.Value
	}
	if req.BaseRole != nil {
		role.BaseRole = *req.BaseRole
	}
	if req.Permissions != nil {
		role.Permissions = append([]string(nil), (*req.Permissions)...)
	}
	role.UpdatedAt = s.currentTime()
	if s.store.persist != nil {
		s.store.persist.MustPut("org_custom_repo_roles", org.Login, s.store.OrgCustomRepoRoles[org.Login])
	}
	result := *role
	result.Permissions = append([]string(nil), role.Permissions...)
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, customRepositoryRoleJSON(&result, org, s.baseURL(r)))
}

func (s *Server) handleDeleteCustomRepositoryRole(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	id, ok := parseRoleID(w, r)
	if !ok {
		return
	}
	s.store.mu.Lock()
	if s.store.OrgCustomRepoRoles[org.Login][id] == nil {
		s.store.mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	delete(s.store.OrgCustomRepoRoles[org.Login], id)
	if s.store.persist != nil {
		s.store.persist.MustPut("org_custom_repo_roles", org.Login, s.store.OrgCustomRepoRoles[org.Login])
	}
	s.store.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListRepositoryFineGrainedPermissions(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgMember(w, r)
	if org == nil {
		return
	}
	writeJSON(w, http.StatusOK, repositoryFineGrainedPermissions)
}

func (s *Server) handleListOrganizationFineGrainedPermissions(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgMember(w, r)
	if org == nil {
		return
	}
	writeJSON(w, http.StatusOK, organizationFineGrainedPermissions)
}

type customOrganizationRoleRequest struct {
	Name        *string              `json:"name"`
	Description nullableStringUpdate `json:"description"`
	BaseRole    *string              `json:"base_role"`
	Permissions *[]string            `json:"permissions"`
}

func validBaseOrganizationRole(value string, update bool) bool {
	switch value {
	case "read", "triage", "write", "maintain", "admin":
		return true
	case "none":
		return update
	default:
		return false
	}
}

func validateCustomOrganizationRoleRequest(w http.ResponseWriter, req customOrganizationRoleRequest, create bool) bool {
	if create && req.Name == nil {
		writeGHValidationError(w, "CustomOrganizationRole", "name", "missing_field")
		return false
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		writeGHValidationError(w, "CustomOrganizationRole", "name", "invalid")
		return false
	}
	if create && req.Permissions == nil {
		writeGHValidationError(w, "CustomOrganizationRole", "permissions", "missing_field")
		return false
	}
	if req.Permissions != nil && !validPermissions(*req.Permissions, organizationFineGrainedPermissions) {
		writeGHValidationError(w, "CustomOrganizationRole", "permissions", "invalid")
		return false
	}
	if req.BaseRole != nil && !validBaseOrganizationRole(*req.BaseRole, !create) {
		writeGHValidationError(w, "CustomOrganizationRole", "base_role", "invalid")
		return false
	}
	return true
}

func (s *Server) handleCreateOrganizationRole(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	var req customOrganizationRoleRequest
	if !decodeJSONBody(w, r, &req) || !validateCustomOrganizationRoleRequest(w, req, true) {
		return
	}
	now := s.currentTime()
	role := &OrgCustomOrganizationRole{
		Name: strings.TrimSpace(*req.Name), Permissions: append([]string(nil), (*req.Permissions)...),
		OrgLogin: org.Login, CreatedAt: now, UpdatedAt: now,
	}
	if req.Description.Set {
		role.Description = req.Description.Value
	}
	if req.BaseRole != nil {
		baseRole := *req.BaseRole
		role.BaseRole = &baseRole
	}
	s.store.mu.Lock()
	if s.store.OrgCustomRoles[org.Login] == nil {
		s.store.OrgCustomRoles[org.Login] = map[int]*OrgCustomOrganizationRole{}
	}
	for _, existing := range s.store.OrgCustomRoles[org.Login] {
		if strings.EqualFold(existing.Name, role.Name) {
			s.store.mu.Unlock()
			writeGHError(w, http.StatusConflict, "An organization role with this name already exists.")
			return
		}
	}
	role.ID = s.store.reserveOrgCustomRoleIDLocked()
	s.store.OrgCustomRoles[org.Login][role.ID] = role
	if s.store.persist != nil {
		s.store.persist.MustPut("org_custom_roles", org.Login, s.store.OrgCustomRoles[org.Login])
	}
	s.store.mu.Unlock()
	writeJSON(w, http.StatusCreated, orgRoleJSON(customOrgRoleView(role), org, s.baseURL(r)))
}

func (s *Server) handleUpdateOrganizationRole(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	id, ok := parseRoleID(w, r)
	if !ok {
		return
	}
	var req customOrganizationRoleRequest
	if !decodeJSONBody(w, r, &req) || !validateCustomOrganizationRoleRequest(w, req, false) {
		return
	}
	s.store.mu.Lock()
	role := s.store.OrgCustomRoles[org.Login][id]
	if role == nil {
		s.store.mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if req.Name != nil {
		for existingID, existing := range s.store.OrgCustomRoles[org.Login] {
			if existingID != id && strings.EqualFold(existing.Name, strings.TrimSpace(*req.Name)) {
				s.store.mu.Unlock()
				writeGHError(w, http.StatusConflict, "An organization role with this name already exists.")
				return
			}
		}
		role.Name = strings.TrimSpace(*req.Name)
	}
	if req.Description.Set {
		role.Description = req.Description.Value
	}
	if req.BaseRole != nil {
		if *req.BaseRole == "none" {
			role.BaseRole = nil
		} else {
			baseRole := *req.BaseRole
			role.BaseRole = &baseRole
		}
	}
	if req.Permissions != nil {
		role.Permissions = append([]string(nil), (*req.Permissions)...)
	}
	role.UpdatedAt = s.currentTime()
	if s.store.persist != nil {
		s.store.persist.MustPut("org_custom_roles", org.Login, s.store.OrgCustomRoles[org.Login])
	}
	result := *role
	result.Permissions = append([]string(nil), role.Permissions...)
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, orgRoleJSON(customOrgRoleView(&result), org, s.baseURL(r)))
}

func (s *Server) handleDeleteOrganizationRole(w http.ResponseWriter, r *http.Request) {
	org, _ := s.resolveOrgOwner(w, r)
	if org == nil {
		return
	}
	id, ok := parseRoleID(w, r)
	if !ok {
		return
	}
	s.store.mu.Lock()
	if s.store.OrgCustomRoles[org.Login][id] == nil {
		s.store.mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	delete(s.store.OrgCustomRoles[org.Login], id)
	delete(s.store.OrgRoleTeamAssignments[org.Login], id)
	delete(s.store.OrgRoleUserAssignments[org.Login], id)
	// One transaction: the role and its team and user assignments are deleted
	// together, so a crash cannot leave a dangling assignment to a deleted role
	// (STORE-001/002). Unlock before any panic so recovery's reload is not
	// deadlocked by a still-held write lock (this handler unlocks explicitly).
	batch := newPersistBatch(s.store.persist)
	batch.Put("org_custom_roles", org.Login, s.store.OrgCustomRoles[org.Login])
	batch.Put("org_role_team_assignments", org.Login, s.store.OrgRoleTeamAssignments[org.Login])
	batch.Put("org_role_user_assignments", org.Login, s.store.OrgRoleUserAssignments[org.Login])
	commitErr := batch.Commit()
	s.store.mu.Unlock()
	if commitErr != nil {
		panic(&persistenceFailure{op: "batch", bucket: "org_custom_roles", err: commitErr})
	}
	w.WriteHeader(http.StatusNoContent)
}

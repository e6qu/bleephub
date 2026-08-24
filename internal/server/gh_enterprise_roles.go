package bleephub

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

type predefinedEnterpriseRole struct {
	ID          int
	Name        string
	Description string
	Permissions []string
}

var predefinedEnterpriseRoles = []predefinedEnterpriseRole{
	{ID: 8030, Name: "Security Manager", Description: "A role for security managers",
		Permissions: []string{"read_enterprise_custom_enterprise_role", "write_enterprise_security_configuration"}},
	{ID: 8031, Name: "Enterprise Auditor", Description: "Permissions to read enterprise audit logs and security settings",
		Permissions: []string{"read_enterprise_audit_logs", "read_enterprise_security_configuration"}},
}

var enterpriseRoleCreatedAt = time.Date(2022, time.July, 4, 22, 19, 11, 0, time.UTC)

func (s *Server) registerGHEnterpriseRoleRoutes() {
	s.route("GET /api/v3/enterprises/{enterprise}/enterprise-roles", s.requireEnterpriseOwner(s.handleListEnterpriseRoles))
	s.route("GET /api/v3/enterprises/{enterprise}/enterprise-roles/{role_id}", s.requireEnterpriseOwner(s.handleGetEnterpriseRole))
	s.route("GET /api/v3/enterprises/{enterprise}/enterprise-roles/{role_id}/teams", s.requireEnterpriseOwner(s.handleListEnterpriseRoleTeams))
	s.route("GET /api/v3/enterprises/{enterprise}/enterprise-roles/{role_id}/users", s.requireEnterpriseOwner(s.handleListEnterpriseRoleUsers))
	s.route("PUT /api/v3/enterprises/{enterprise}/enterprise-roles/teams/{team_slug}/{role_id}", s.requireEnterpriseOwner(s.handleAssignEnterpriseRoleToTeam))
	s.route("DELETE /api/v3/enterprises/{enterprise}/enterprise-roles/teams/{team_slug}/{role_id}", s.requireEnterpriseOwner(s.handleRevokeEnterpriseRoleFromTeam))
	s.route("DELETE /api/v3/enterprises/{enterprise}/enterprise-roles/teams/{team_slug}", s.requireEnterpriseOwner(s.handleRevokeAllEnterpriseRolesFromTeam))
	s.route("PUT /api/v3/enterprises/{enterprise}/enterprise-roles/users/{username}/{role_id}", s.requireEnterpriseOwner(s.handleAssignEnterpriseRoleToUser))
	s.route("DELETE /api/v3/enterprises/{enterprise}/enterprise-roles/users/{username}/{role_id}", s.requireEnterpriseOwner(s.handleRevokeEnterpriseRoleFromUser))
	s.route("DELETE /api/v3/enterprises/{enterprise}/enterprise-roles/users/{username}", s.requireEnterpriseOwner(s.handleRevokeAllEnterpriseRolesFromUser))
}

func predefinedEnterpriseRoleByID(id int) *predefinedEnterpriseRole {
	for i := range predefinedEnterpriseRoles {
		if predefinedEnterpriseRoles[i].ID == id {
			return &predefinedEnterpriseRoles[i]
		}
	}
	return nil
}

func (s *Server) resolveEnterpriseRole(w http.ResponseWriter, r *http.Request) *predefinedEnterpriseRole {
	id, err := strconv.Atoi(r.PathValue("role_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	role := predefinedEnterpriseRoleByID(id)
	if role == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
	}
	return role
}

func (s *Server) enterpriseAccountJSON(baseURL string) map[string]interface{} {
	slug := s.enterpriseSlug()
	return map[string]interface{}{
		"id": 1, "slug": slug, "name": slug, "node_id": "E_kgAB",
		"avatar_url":  baseURL + "/identicons/" + slug + ".png",
		"description": "", "website_url": nil,
		"html_url":   baseURL + "/enterprises/" + slug,
		"created_at": enterpriseRoleCreatedAt.Format(time.RFC3339),
		"updated_at": enterpriseRoleCreatedAt.Format(time.RFC3339),
	}
}

func (s *Server) enterpriseRoleJSON(role *predefinedEnterpriseRole, baseURL string) map[string]interface{} {
	return map[string]interface{}{
		"id": role.ID, "name": role.Name, "description": role.Description,
		"permissions": role.Permissions, "enterprise": s.enterpriseAccountJSON(baseURL),
		"created_at": enterpriseRoleCreatedAt.Format(time.RFC3339),
		"updated_at": enterpriseRoleCreatedAt.Add(time.Minute).Format(time.RFC3339),
		"source":     "Enterprise",
	}
}

func (s *Server) handleListEnterpriseRoles(w http.ResponseWriter, r *http.Request) {
	roles := make([]map[string]interface{}, len(predefinedEnterpriseRoles))
	for i := range predefinedEnterpriseRoles {
		roles[i] = s.enterpriseRoleJSON(&predefinedEnterpriseRoles[i], s.baseURL(r))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"total_count": len(roles), "roles": roles})
}

func (s *Server) handleGetEnterpriseRole(w http.ResponseWriter, r *http.Request) {
	if role := s.resolveEnterpriseRole(w, r); role != nil {
		writeJSON(w, http.StatusOK, s.enterpriseRoleJSON(role, s.baseURL(r)))
	}
}

func (s *Server) handleListEnterpriseRoleTeams(w http.ResponseWriter, r *http.Request) {
	role := s.resolveEnterpriseRole(w, r)
	if role == nil {
		return
	}
	s.store.Mu.RLock()
	ids := append([]int(nil), s.store.EnterpriseSettings.EnterpriseRoleTeamAssignments[role.ID]...)
	teams := make([]*store.EnterpriseTeam, 0, len(ids))
	for _, id := range ids {
		if team := s.store.EnterpriseTeams[id]; team != nil {
			teams = append(teams, team)
		}
	}
	s.store.Mu.RUnlock()
	sort.Slice(teams, func(i, j int) bool { return teams[i].ID < teams[j].ID })
	result := make([]map[string]interface{}, len(teams))
	for i, team := range teams {
		result[i] = s.enterpriseTeamJSON(team, s.baseURL(r))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleListEnterpriseRoleUsers(w http.ResponseWriter, r *http.Request) {
	role := s.resolveEnterpriseRole(w, r)
	if role == nil {
		return
	}
	s.store.Mu.RLock()
	assignments := map[int][]*store.EnterpriseTeam{}
	for _, userID := range s.store.EnterpriseSettings.EnterpriseRoleUserAssignments[role.ID] {
		assignments[userID] = nil
	}
	for _, teamID := range s.store.EnterpriseSettings.EnterpriseRoleTeamAssignments[role.ID] {
		team := s.store.EnterpriseTeams[teamID]
		if team == nil {
			continue
		}
		for _, userID := range team.MemberIDs {
			if _, direct := assignments[userID]; !direct {
				assignments[userID] = append(assignments[userID], team)
			}
		}
	}
	s.store.Mu.RUnlock()
	ids := make([]int, 0, len(assignments))
	for id := range assignments {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	result := make([]map[string]interface{}, 0, len(ids))
	for _, id := range ids {
		user := s.store.GetUserByID(id)
		if user == nil {
			continue
		}
		item := store.UserToJSON(user, s.baseURL(r))
		inherited := assignments[id]
		if inherited == nil {
			item["assignment"] = "direct"
			item["inherited_from"] = []interface{}{}
		} else {
			item["assignment"] = "indirect"
			teams := make([]map[string]interface{}, len(inherited))
			for i, team := range inherited {
				teams[i] = s.enterpriseTeamJSON(team, s.baseURL(r))
			}
			item["inherited_from"] = teams
		}
		result = append(result, item)
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func appendUniqueInt(values []int, value int) []int {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func removeInt(values []int, value int) []int {
	result := values[:0]
	for _, existing := range values {
		if existing != value {
			result = append(result, existing)
		}
	}
	return result
}

func (s *Server) mutateEnterpriseRoleAssignment(w http.ResponseWriter, r *http.Request, kind string, removeAll, remove bool) {
	var targetID int
	if kind == "teams" {
		team := s.store.GetEnterpriseTeam(r.PathValue("team_slug"))
		if team == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		targetID = team.ID
	} else {
		user := s.store.LookupUserByLogin(r.PathValue("username"))
		if user == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		targetID = user.ID
	}
	roleID := 0
	if !removeAll {
		role := s.resolveEnterpriseRole(w, r)
		if role == nil {
			return
		}
		roleID = role.ID
	}
	s.store.Mu.Lock()
	assignments := s.store.EnterpriseSettings.EnterpriseRoleUserAssignments
	if kind == "teams" {
		assignments = s.store.EnterpriseSettings.EnterpriseRoleTeamAssignments
	}
	if removeAll {
		for id, targets := range assignments {
			assignments[id] = removeInt(targets, targetID)
		}
	} else if remove {
		assignments[roleID] = removeInt(assignments[roleID], targetID)
	} else {
		assignments[roleID] = appendUniqueInt(assignments[roleID], targetID)
	}
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAssignEnterpriseRoleToTeam(w http.ResponseWriter, r *http.Request) {
	s.mutateEnterpriseRoleAssignment(w, r, "teams", false, false)
}

func (s *Server) handleRevokeEnterpriseRoleFromTeam(w http.ResponseWriter, r *http.Request) {
	s.mutateEnterpriseRoleAssignment(w, r, "teams", false, true)
}

func (s *Server) handleRevokeAllEnterpriseRolesFromTeam(w http.ResponseWriter, r *http.Request) {
	s.mutateEnterpriseRoleAssignment(w, r, "teams", true, true)
}

func (s *Server) handleAssignEnterpriseRoleToUser(w http.ResponseWriter, r *http.Request) {
	s.mutateEnterpriseRoleAssignment(w, r, "users", false, false)
}

func (s *Server) handleRevokeEnterpriseRoleFromUser(w http.ResponseWriter, r *http.Request) {
	s.mutateEnterpriseRoleAssignment(w, r, "users", false, true)
}

func (s *Server) handleRevokeAllEnterpriseRolesFromUser(w http.ResponseWriter, r *http.Request) {
	s.mutateEnterpriseRoleAssignment(w, r, "users", true, true)
}

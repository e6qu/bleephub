package bleephub

import (
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

type teamSyncGroupRequest struct {
	Groups []struct {
		ID          string `json:"group_id"`
		Name        string `json:"group_name"`
		Description string `json:"group_description"`
	} `json:"groups"`
}

func (s *Server) registerGHExternalIdentityRoutes() {
	s.route("GET /api/v3/orgs/{org}/external-groups", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleListOrgExternalGroups))
	s.route("GET /api/v3/orgs/{org}/external-group/{group_id}", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleGetOrgExternalGroup))
	s.route("GET /api/v3/orgs/{org}/team-sync/groups", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleListOrgTeamSyncGroups))

	s.route("GET /api/v3/orgs/{org}/teams/{team_slug}/external-groups", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleListTeamExternalGroups))
	s.route("PATCH /api/v3/orgs/{org}/teams/{team_slug}/external-groups", s.requirePerm(store.ScopeMembers, store.PermWrite, s.handleLinkTeamExternalGroup))
	s.route("DELETE /api/v3/orgs/{org}/teams/{team_slug}/external-groups", s.requirePerm(store.ScopeMembers, store.PermWrite, s.handleUnlinkTeamExternalGroups))
	s.route("GET /api/v3/orgs/{org}/teams/{team_slug}/team-sync/group-mappings", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleGetTeamSyncMappings))
	s.route("PATCH /api/v3/orgs/{org}/teams/{team_slug}/team-sync/group-mappings", s.requirePerm(store.ScopeMembers, store.PermWrite, s.handleSetTeamSyncMappings))

	s.route("GET /api/v3/teams/{team_id}/team-sync/group-mappings", s.requirePerm(store.ScopeMembers, store.PermRead, s.handleGetLegacyTeamSyncMappings))
	s.route("PATCH /api/v3/teams/{team_id}/team-sync/group-mappings", s.requirePerm(store.ScopeMembers, store.PermWrite, s.handleSetLegacyTeamSyncMappings))
}

func (s *Server) resolveOrgExternalIdentityAdmin(w http.ResponseWriter, r *http.Request) *store.Org {
	org, _ := s.resolveOrgOwner(w, r)
	return org
}

func (s *Server) resolveOrgTeamForExternalIdentity(w http.ResponseWriter, r *http.Request) (*store.Org, *store.Team) {
	org := s.resolveOrgExternalIdentityAdmin(w, r)
	if org == nil {
		return nil, nil
	}
	team := s.store.GetTeam(org.Login, r.PathValue("team_slug"))
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	return org, team
}

func (s *Server) resolveLegacyTeamForExternalIdentity(w http.ResponseWriter, r *http.Request) (*store.Org, *store.Team) {
	id, err := strconv.Atoi(r.PathValue("team_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	team := s.store.GetTeamByID(id)
	if team == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	org := s.store.GetOrgByID(team.OrgID)
	if org == nil || !s.viewerCanAdminOrg(r.Context(), org.Login) {
		writeGHError(w, http.StatusForbidden, "Must be an organization owner.")
		return nil, nil
	}
	return org, team
}

func externalGroupSummary(group *store.OrgExternalIdentityGroup) map[string]interface{} {
	return map[string]interface{}{
		"group_id": group.NumericID, "group_name": group.Name,
		"updated_at": group.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func teamSyncGroupJSON(group *store.OrgExternalIdentityGroup, mapped bool) map[string]interface{} {
	status := "unsynced"
	var syncedAt interface{}
	if mapped {
		status = "synced"
		syncedAt = group.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return map[string]interface{}{
		"group_id": group.ID, "group_name": group.Name, "group_description": group.Description,
		"status": status, "synced_at": syncedAt,
	}
}

func copyExternalGroups(groups map[string]*store.OrgExternalIdentityGroup) []*store.OrgExternalIdentityGroup {
	result := make([]*store.OrgExternalIdentityGroup, 0, len(groups))
	for _, group := range groups {
		copyGroup := *group
		copyGroup.MemberIDs = append([]int(nil), group.MemberIDs...)
		result = append(result, &copyGroup)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].NumericID < result[j].NumericID })
	return result
}

func (s *Server) handleListOrgExternalGroups(w http.ResponseWriter, r *http.Request) {
	org := s.resolveOrgExternalIdentityAdmin(w, r)
	if org == nil {
		return
	}
	s.store.Mu.RLock()
	groups := copyExternalGroups(s.store.OrgExternalGroups[org.Login])
	s.store.Mu.RUnlock()
	result := make([]map[string]interface{}, 0, len(groups))
	for _, group := range groups {
		result = append(result, externalGroupSummary(group))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"groups": paginateAndLink(w, r, result)})
}

func (s *Server) externalGroupByNumericID(orgLogin string, id int) *store.OrgExternalIdentityGroup {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, group := range s.store.OrgExternalGroups[orgLogin] {
		if group.NumericID == id {
			copyGroup := *group
			copyGroup.MemberIDs = append([]int(nil), group.MemberIDs...)
			return &copyGroup
		}
	}
	return nil
}

func (s *Server) handleGetOrgExternalGroup(w http.ResponseWriter, r *http.Request) {
	org := s.resolveOrgExternalIdentityAdmin(w, r)
	if org == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("group_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	group := s.externalGroupByNumericID(org.Login, id)
	if group == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.RLock()
	teams := make([]map[string]interface{}, 0)
	for teamID, groupIDs := range s.store.TeamExternalGroupIDs {
		if !slices.Contains(groupIDs, group.ID) {
			continue
		}
		if team := s.store.Teams[teamID]; team != nil && team.OrgID == org.ID {
			teams = append(teams, map[string]interface{}{"team_id": team.ID, "team_name": team.Name})
		}
	}
	members := make([]map[string]interface{}, 0, len(group.MemberIDs))
	for _, userID := range group.MemberIDs {
		if user := s.store.Users[userID]; user != nil {
			members = append(members, map[string]interface{}{
				"member_id": user.ID, "member_login": user.Login,
				"member_name": user.Name, "member_email": user.Email,
			})
		}
	}
	s.store.Mu.RUnlock()
	sort.Slice(teams, func(i, j int) bool { return teams[i]["team_id"].(int) < teams[j]["team_id"].(int) })
	sort.Slice(members, func(i, j int) bool { return members[i]["member_id"].(int) < members[j]["member_id"].(int) })
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"group_id": group.NumericID, "group_name": group.Name,
		"updated_at": group.UpdatedAt.UTC().Format(time.RFC3339),
		"teams":      teams, "members": members,
	})
}

func (s *Server) handleListOrgTeamSyncGroups(w http.ResponseWriter, r *http.Request) {
	org := s.resolveOrgExternalIdentityAdmin(w, r)
	if org == nil {
		return
	}
	s.store.Mu.RLock()
	groups := copyExternalGroups(s.store.OrgExternalGroups[org.Login])
	s.store.Mu.RUnlock()
	result := make([]map[string]interface{}, 0, len(groups))
	for _, group := range groups {
		result = append(result, teamSyncGroupJSON(group, false))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"groups": paginateAndLink(w, r, result)})
}

func (s *Server) listTeamExternalGroups(w http.ResponseWriter, r *http.Request, org *store.Org, team *store.Team) {
	s.store.Mu.RLock()
	ids := append([]string(nil), s.store.TeamExternalGroupIDs[team.ID]...)
	groups := make([]*store.OrgExternalIdentityGroup, 0, len(ids))
	for _, id := range ids {
		if group := s.store.OrgExternalGroups[org.Login][id]; group != nil {
			copyGroup := *group
			groups = append(groups, &copyGroup)
		}
	}
	s.store.Mu.RUnlock()
	result := make([]map[string]interface{}, 0, len(groups))
	for _, group := range groups {
		result = append(result, externalGroupSummary(group))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"groups": result})
}

func (s *Server) handleListTeamExternalGroups(w http.ResponseWriter, r *http.Request) {
	org, team := s.resolveOrgTeamForExternalIdentity(w, r)
	if team != nil {
		s.listTeamExternalGroups(w, r, org, team)
	}
}

func (s *Server) persistTeamExternalGroupsLocked(orgLogin string, teamID int) {
	// One transaction: the org's external-group set and the team's binding commit
	// together, so a crash cannot leave one without the other (STORE-001/002).
	batch := store.NewPersistBatch(s.store.Persist)
	batch.Put("org_external_groups", orgLogin, s.store.OrgExternalGroups[orgLogin])
	batch.Put("team_external_group_ids", strconv.Itoa(teamID), s.store.TeamExternalGroupIDs[teamID])
	if err := batch.Commit(); err != nil {
		panic(&store.PersistenceFailure{Op: "batch", Bucket: "org_external_groups", Err: err})
	}
}

func (s *Server) syncExternalTeamMembersLocked(org *store.Org, team *store.Team) {
	for _, groupID := range s.store.TeamExternalGroupIDs[team.ID] {
		group := s.store.OrgExternalGroups[org.Login][groupID]
		if group == nil {
			continue
		}
		for _, userID := range group.MemberIDs {
			if !slices.Contains(team.MemberIDs, userID) {
				team.MemberIDs = append(team.MemberIDs, userID)
			}
		}
	}
	sort.Ints(team.MemberIDs)
	team.UpdatedAt = s.currentTime()
	if s.store.Persist != nil {
		s.store.Persist.MustPut("teams", strconv.Itoa(team.ID), team)
	}
}

func (s *Server) handleLinkTeamExternalGroup(w http.ResponseWriter, r *http.Request) {
	org, team := s.resolveOrgTeamForExternalIdentity(w, r)
	if team == nil {
		return
	}
	var req struct {
		GroupID *int `json:"group_id"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.GroupID == nil {
		store.WriteGHValidationError(w, "ExternalGroup", "group_id", "missing_field")
		return
	}
	group := s.externalGroupByNumericID(org.Login, *req.GroupID)
	if group == nil {
		store.WriteGHValidationError(w, "ExternalGroup", "group_id", "invalid")
		return
	}
	s.store.Mu.Lock()
	s.store.TeamExternalGroupIDs[team.ID] = []string{group.ID}
	group.UpdatedAt = s.currentTime()
	s.store.OrgExternalGroups[org.Login][group.ID] = group
	s.syncExternalTeamMembersLocked(org, s.store.Teams[team.ID])
	s.persistTeamExternalGroupsLocked(org.Login, team.ID)
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, externalGroupSummary(group))
}

func (s *Server) handleUnlinkTeamExternalGroups(w http.ResponseWriter, r *http.Request) {
	org, team := s.resolveOrgTeamForExternalIdentity(w, r)
	if team == nil {
		return
	}
	s.store.Mu.Lock()
	delete(s.store.TeamExternalGroupIDs, team.ID)
	if s.store.Persist != nil {
		s.store.Persist.MustDelete("team_external_group_ids", strconv.Itoa(team.ID))
	}
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
	_ = org
}

func (s *Server) teamSyncMappings(w http.ResponseWriter, r *http.Request, org *store.Org, team *store.Team) {
	s.store.Mu.RLock()
	ids := append([]string(nil), s.store.TeamExternalGroupIDs[team.ID]...)
	groups := make([]*store.OrgExternalIdentityGroup, 0, len(ids))
	for _, id := range ids {
		if group := s.store.OrgExternalGroups[org.Login][id]; group != nil {
			copyGroup := *group
			groups = append(groups, &copyGroup)
		}
	}
	s.store.Mu.RUnlock()
	result := make([]map[string]interface{}, 0, len(groups))
	for _, group := range groups {
		result = append(result, teamSyncGroupJSON(group, true))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"groups": result})
}

func (s *Server) handleGetTeamSyncMappings(w http.ResponseWriter, r *http.Request) {
	org, team := s.resolveOrgTeamForExternalIdentity(w, r)
	if team != nil {
		s.teamSyncMappings(w, r, org, team)
	}
}

func (s *Server) handleGetLegacyTeamSyncMappings(w http.ResponseWriter, r *http.Request) {
	org, team := s.resolveLegacyTeamForExternalIdentity(w, r)
	if team != nil {
		s.teamSyncMappings(w, r, org, team)
	}
}

func (s *Server) setTeamSyncMappings(w http.ResponseWriter, r *http.Request, org *store.Org, team *store.Team) {
	var req teamSyncGroupRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	seen := map[string]bool{}
	for _, group := range req.Groups {
		if strings.TrimSpace(group.ID) == "" || strings.TrimSpace(group.Name) == "" || seen[group.ID] {
			store.WriteGHValidationError(w, "TeamSyncGroup", "groups", "invalid")
			return
		}
		seen[group.ID] = true
	}
	now := s.currentTime()
	s.store.Mu.Lock()
	if s.store.OrgExternalGroups[org.Login] == nil {
		s.store.OrgExternalGroups[org.Login] = map[string]*store.OrgExternalIdentityGroup{}
	}
	groupIDs := make([]string, 0, len(req.Groups))
	for _, input := range req.Groups {
		group := s.store.OrgExternalGroups[org.Login][input.ID]
		if group == nil {
			group = &store.OrgExternalIdentityGroup{
				ID: input.ID, NumericID: s.store.NextOrgExternalGroupID,
			}
			s.store.NextOrgExternalGroupID++
			s.store.OrgExternalGroups[org.Login][input.ID] = group
		}
		group.Name, group.Description, group.UpdatedAt = input.Name, input.Description, now
		groupIDs = append(groupIDs, input.ID)
	}
	s.store.TeamExternalGroupIDs[team.ID] = groupIDs
	s.syncExternalTeamMembersLocked(org, s.store.Teams[team.ID])
	s.persistTeamExternalGroupsLocked(org.Login, team.ID)
	s.store.Mu.Unlock()
	s.teamSyncMappings(w, r, org, team)
}

func (s *Server) handleSetTeamSyncMappings(w http.ResponseWriter, r *http.Request) {
	org, team := s.resolveOrgTeamForExternalIdentity(w, r)
	if team != nil {
		s.setTeamSyncMappings(w, r, org, team)
	}
}

func (s *Server) handleSetLegacyTeamSyncMappings(w http.ResponseWriter, r *http.Request) {
	org, team := s.resolveLegacyTeamForExternalIdentity(w, r)
	if team != nil {
		s.setTeamSyncMappings(w, r, org, team)
	}
}

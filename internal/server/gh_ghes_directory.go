package bleephub

import (
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHESDirectoryRoutes(admin func(http.HandlerFunc) http.HandlerFunc) {
	s.route("PATCH /api/v3/admin/ldap/teams/{team_id}/mapping", admin(s.handleUpdateGHESTeamLDAPMapping))
	s.route("POST /api/v3/admin/ldap/teams/{team_id}/sync", admin(s.handleSyncGHESTeamLDAPMapping))
	s.route("PATCH /api/v3/admin/ldap/users/{username}/mapping", admin(s.handleUpdateGHESUserLDAPMapping))
	s.route("POST /api/v3/admin/ldap/users/{username}/sync", admin(s.handleSyncGHESUserLDAPMapping))
	s.route("PATCH /api/v3/admin/organizations/{org}", admin(s.handleRenameGHESOrganization))
	s.route("GET /api/v3/repos/{owner}/{repo}/replicas/caches", admin(s.handleListGHESRepositoryReplicaCaches))
}

func (s *Server) ghesLDAPTeam(w http.ResponseWriter, r *http.Request) (*store.Team, *store.Org) {
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
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, nil
	}
	return team, org
}

func (s *Server) ghesLDAPTeamJSON(team *store.Team, org *store.Org, r *http.Request) map[string]interface{} {
	out := teamSimpleJSON(team, org, s.store, s.baseURL(r))
	s.store.Mu.RLock()
	out["ldap_dn"] = s.store.EnterpriseSettings.GHESLDAPTeamMappings[team.ID]
	s.store.Mu.RUnlock()
	out["type"] = "organization"
	out["organization_id"] = org.ID
	out["enterprise_id"] = 1
	return out
}

func (s *Server) handleUpdateGHESTeamLDAPMapping(w http.ResponseWriter, r *http.Request) {
	team, org := s.ghesLDAPTeam(w, r)
	if team == nil {
		return
	}
	var req struct {
		LDAPDN string `json:"ldap_dn"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.LDAPDN) == "" {
		store.WriteGHValidationError(w, "Team", "ldap_dn", "missing_field")
		return
	}
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.GHESLDAPTeamMappings[team.ID] = req.LDAPDN
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, s.ghesLDAPTeamJSON(team, org, r))
}

func (s *Server) handleSyncGHESTeamLDAPMapping(w http.ResponseWriter, r *http.Request) {
	team, _ := s.ghesLDAPTeam(w, r)
	if team == nil {
		return
	}
	s.store.Mu.RLock()
	mapped := s.store.EnterpriseSettings.GHESLDAPTeamMappings[team.ID] != ""
	s.store.Mu.RUnlock()
	if !mapped {
		writeGHError(w, http.StatusConflict, "Team is not mapped to LDAP")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{"status": "queued"})
}

func (s *Server) ghesLDAPUser(w http.ResponseWriter, r *http.Request) *store.User {
	user := s.store.LookupUserByLogin(r.PathValue("username"))
	if user == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
	}
	return user
}

func (s *Server) ghesLDAPUserJSON(user *store.User) map[string]interface{} {
	out := s.fullUserJSON(user, s.publicOrigin())
	s.store.Mu.RLock()
	out["ldap_dn"] = s.store.EnterpriseSettings.GHESLDAPUserMappings[user.Login]
	s.store.Mu.RUnlock()
	return out
}

func (s *Server) handleUpdateGHESUserLDAPMapping(w http.ResponseWriter, r *http.Request) {
	user := s.ghesLDAPUser(w, r)
	if user == nil {
		return
	}
	var req struct {
		LDAPDN string `json:"ldap_dn"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.LDAPDN) == "" {
		store.WriteGHValidationError(w, "User", "ldap_dn", "missing_field")
		return
	}
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.GHESLDAPUserMappings[user.Login] = req.LDAPDN
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, s.ghesLDAPUserJSON(user))
}

func (s *Server) handleSyncGHESUserLDAPMapping(w http.ResponseWriter, r *http.Request) {
	user := s.ghesLDAPUser(w, r)
	if user == nil {
		return
	}
	s.store.Mu.RLock()
	mapped := s.store.EnterpriseSettings.GHESLDAPUserMappings[user.Login] != ""
	s.store.Mu.RUnlock()
	if !mapped {
		writeGHError(w, http.StatusConflict, "User is not mapped to LDAP")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{"status": "queued"})
}

func (s *Server) handleRenameGHESOrganization(w http.ResponseWriter, r *http.Request) {
	oldLogin := r.PathValue("org")
	var req struct {
		Login string `json:"login"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	newLogin := normalizeGitHubLogin(req.Login)
	if newLogin == "" {
		store.WriteGHValidationError(w, "Organization", "login", "missing_field")
		return
	}
	if oldLogin != newLogin && !s.renameGHESOrganization(oldLogin, newLogin) {
		store.WriteGHValidationError(w, "Organization", "login", "already_exists")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"message": "Organization rename scheduled.",
		"url":     s.baseURL(r) + "/api/v3/orgs/" + newLogin,
	})
}

// renameGHESOrganization changes the namespace synchronously even though GHES
// reports the operation as merely accepted.
func (s *Server) renameGHESOrganization(oldLogin, newLogin string) bool {
	s.store.Mu.Lock()
	org := s.store.OrgByLoginLocked(oldLogin)
	if org == nil || (s.store.OrgByLoginLocked(newLogin) != nil && s.store.OrgByLoginLocked(newLogin) != org) ||
		s.store.UserByLoginLocked(newLogin) != nil {
		s.store.Mu.Unlock()
		return false
	}
	oldLogin = org.Login
	s.store.OrgsByLogin[newLogin] = org
	s.store.IndexOrgLoginLocked(newLogin)
	org.Login = newLogin
	org.UpdatedAt = s.store.CurrentTime()
	repoNames := make([]string, 0)
	for _, repo := range s.store.Repos {
		if repo.OwnerType == "Organization" && repo.OwnerID == org.ID {
			repoNames = append(repoNames, repo.Name)
		}
	}
	s.store.Mu.Unlock()
	sort.Strings(repoNames)
	for _, name := range repoNames {
		if !s.store.TransferRepo(oldLogin, name, newLogin) {
			return false
		}
	}

	s.store.Mu.Lock()
	// Stage the whole re-key phase into one transaction so a crash cannot leave
	// the org split across its old and new login (STORE-001/002).
	batch := store.NewPersistBatch(s.store.Persist)
	delete(s.store.OrgsByLogin, oldLogin)
	s.store.UnindexOrgLoginLocked(oldLogin)
	for key, membership := range s.store.Memberships {
		if membership.OrgID != org.ID {
			continue
		}
		nextKey := store.MembershipKey(newLogin, membership.UserID)
		delete(s.store.Memberships, key)
		s.store.Memberships[nextKey] = membership
		batch.Delete("memberships", key)
		batch.Put("memberships", nextKey, membership)
	}
	for key, team := range s.store.TeamsBySlug {
		if team.OrgID != org.ID {
			continue
		}
		delete(s.store.TeamsBySlug, key)
		s.store.TeamsBySlug[store.TeamSlugKey(newLogin, team.Slug)] = team
	}
	for _, installation := range s.store.Installations {
		if installation.TargetType == "Organization" && installation.TargetID == org.ID {
			installation.TargetLogin = newLogin
			batch.Put("installations", strconv.Itoa(installation.ID), installation)
		}
	}
	moveGHESOrgScopedState(s.store, oldLogin, newLogin)
	batch.Put("orgs", strconv.Itoa(org.ID), org)
	batch.Put("enterprise_settings", "enterprise", s.store.EnterpriseSettings)
	err := batch.Commit()
	s.store.Mu.Unlock()
	if err != nil {
		panic(&store.PersistenceFailure{Op: "batch", Bucket: "orgs", Key: strconv.Itoa(org.ID), Err: err})
	}
	return true
}

func moveMapKey[T any](values map[string]T, oldKey, newKey string) {
	if value, ok := values[oldKey]; ok {
		values[newKey] = value
		delete(values, oldKey)
	}
}

func moveGHESOrgScopedState(st *store.Store, oldLogin, newLogin string) {
	moveMapKey(st.OrgHooks, oldLogin, newLogin)
	for _, hook := range st.OrgHooks[newLogin] {
		hook.OrgLogin = newLogin
	}
	moveMapKey(st.OrgSecrets, oldLogin, newLogin)
	moveMapKey(st.OrgVariables, oldLogin, newLogin)
	moveMapKey(st.OrgActionsPermissions, oldLogin, newLogin)
	moveMapKey(st.OrgOIDCPropertyInclusions, oldLogin, newLogin)
	moveMapKey(st.CodeSecurityConfigs, oldLogin, newLogin)
	moveMapKey(st.CodeSecurityRepoAttachments, oldLogin, newLogin)
	moveMapKey(st.OrgCustomProperties, oldLogin, newLogin)
	moveMapKey(st.OrgIssueTypes, oldLogin, newLogin)
	moveMapKey(st.OrgIssueFields, oldLogin, newLogin)
	moveMapKey(st.OrgCampaigns, oldLogin, newLogin)
	moveMapKey(st.OrgPrivateRegistries, oldLogin, newLogin)
	moveMapKey(st.OrgNetworkConfigurations, oldLogin, newLogin)
	moveMapKey(st.OrgNetworkSettings, oldLogin, newLogin)
	moveMapKey(st.OrgImmutableReleases, oldLogin, newLogin)
	moveMapKey(st.OrgBlocks, oldLogin, newLogin)
	moveMapKey(st.OrgInteractionLimits, oldLogin, newLogin)
	moveMapKey(st.OrgRoleTeamAssignments, oldLogin, newLogin)
	moveMapKey(st.OrgRoleUserAssignments, oldLogin, newLogin)
	moveMapKey(st.OrgAnnouncements, oldLogin, newLogin)
	moveMapKey(st.OrgCustomRepoRoles, oldLogin, newLogin)
	moveMapKey(st.OrgCustomRoles, oldLogin, newLogin)
	moveMapKey(st.OrgSCIMUsers, oldLogin, newLogin)
	moveMapKey(st.OrgExternalGroups, oldLogin, newLogin)
	moveMapKey(st.OrgBudgets, oldLogin, newLogin)
	moveMapKey(st.OrgPATGrantRequests, oldLogin, newLogin)
	moveMapKey(st.OrgPATGrants, oldLogin, newLogin)
	moveMapKey(st.OrgCodespacesAccess, oldLogin, newLogin)
	moveMapKey(st.DependabotRepoAccessDefaultLevel, oldLogin, newLogin)
	moveMapKey(st.SecretScanningPatternConfigs, oldLogin, newLogin)
	moveMapKey(st.EnterpriseSettings.GHESOrgPreReceiveOverrides, oldLogin, newLogin)
	persistMovedOrgRow(st, "org_hooks", oldLogin, newLogin, st.OrgHooks)
	persistMovedOrgRow(st, "org_secrets", oldLogin, newLogin, st.OrgSecrets)
	persistMovedOrgRow(st, "org_variables", oldLogin, newLogin, st.OrgVariables)
	persistMovedOrgRow(st, "org_actions_permissions", oldLogin, newLogin, st.OrgActionsPermissions)
	persistMovedOrgRow(st, "org_oidc_property_inclusions", oldLogin, newLogin, st.OrgOIDCPropertyInclusions)
	persistMovedOrgRow(st, "code_security_configurations", oldLogin, newLogin, st.CodeSecurityConfigs)
	persistMovedOrgRow(st, "code_security_repo_attachments", oldLogin, newLogin, st.CodeSecurityRepoAttachments)
	persistMovedOrgRow(st, "org_custom_properties", oldLogin, newLogin, st.OrgCustomProperties)
	persistMovedOrgRow(st, "org_issue_types", oldLogin, newLogin, st.OrgIssueTypes)
	persistMovedOrgRow(st, "org_issue_fields", oldLogin, newLogin, st.OrgIssueFields)
	persistMovedOrgRow(st, "org_campaigns", oldLogin, newLogin, st.OrgCampaigns)
	persistMovedOrgRow(st, "org_private_registries", oldLogin, newLogin, st.OrgPrivateRegistries)
	persistMovedOrgRow(st, "org_network_configurations", oldLogin, newLogin, st.OrgNetworkConfigurations)
	persistMovedOrgRow(st, "org_network_settings", oldLogin, newLogin, st.OrgNetworkSettings)
	persistMovedOrgRow(st, "org_immutable_releases", oldLogin, newLogin, st.OrgImmutableReleases)
	persistMovedOrgRow(st, "org_blocks", oldLogin, newLogin, st.OrgBlocks)
	persistMovedOrgRow(st, "org_interaction_limits", oldLogin, newLogin, st.OrgInteractionLimits)
	persistMovedOrgRow(st, "org_role_team_assignments", oldLogin, newLogin, st.OrgRoleTeamAssignments)
	persistMovedOrgRow(st, "org_role_user_assignments", oldLogin, newLogin, st.OrgRoleUserAssignments)
	persistMovedOrgRow(st, "org_announcements", oldLogin, newLogin, st.OrgAnnouncements)
	persistMovedOrgRow(st, "org_custom_repo_roles", oldLogin, newLogin, st.OrgCustomRepoRoles)
	persistMovedOrgRow(st, "org_custom_roles", oldLogin, newLogin, st.OrgCustomRoles)
	persistMovedOrgRow(st, "org_scim_users", oldLogin, newLogin, st.OrgSCIMUsers)
	persistMovedOrgRow(st, "org_external_groups", oldLogin, newLogin, st.OrgExternalGroups)
	persistMovedOrgRow(st, "org_budgets", oldLogin, newLogin, st.OrgBudgets)
	persistMovedOrgRow(st, "org_pat_grant_requests", oldLogin, newLogin, st.OrgPATGrantRequests)
	persistMovedOrgRow(st, "org_pat_grants", oldLogin, newLogin, st.OrgPATGrants)
	persistMovedOrgRow(st, "org_codespaces_access", oldLogin, newLogin, st.OrgCodespacesAccess)
	persistMovedOrgRow(st, "dependabot_repo_access_default_level", oldLogin, newLogin, st.DependabotRepoAccessDefaultLevel)
	persistMovedOrgRow(st, "secret_scanning_pattern_configs", oldLogin, newLogin, st.SecretScanningPatternConfigs)
}

func persistMovedOrgRow[T any](st *store.Store, bucket, oldKey, newKey string, values map[string]T) {
	if st.Persist == nil {
		return
	}
	if value, ok := values[newKey]; ok {
		st.Persist.MustPut(bucket, newKey, value)
		st.Persist.MustDelete(bucket, oldKey)
	}
}

func (s *Server) handleListGHESRepositoryReplicaCaches(w http.ResponseWriter, r *http.Request) {
	repo := s.store.GetRepo(r.PathValue("owner"), r.PathValue("repo"))
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	host := strings.TrimSpace(os.Getenv("BLEEPHUB_REPLICA_HOST"))
	location := strings.TrimSpace(os.Getenv("BLEEPHUB_REPLICA_LOCATION"))
	if host == "" {
		host = "primary"
		if external, err := url.Parse(s.baseURL(r)); err == nil && external.Hostname() != "" {
			host = external.Hostname()
		}
	}
	if location == "" {
		location = "primary"
	}
	lastSync := repo.UpdatedAt
	if lastSync.IsZero() {
		lastSync = s.currentTime()
	}
	writeJSON(w, http.StatusOK, []map[string]interface{}{{
		"host": host, "location": location,
		"git": map[string]interface{}{
			"sync_status": "in_sync", "last_sync": lastSync.UTC().Format(time.RFC3339),
		},
	}})
}

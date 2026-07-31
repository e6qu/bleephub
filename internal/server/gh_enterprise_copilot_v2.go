package bleephub

import (
	"fmt"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
)

func (s *Server) enterpriseCopilotScope() string {
	return "enterprise:" + s.enterpriseSlug()
}

func (s *Server) registerGHEnterpriseCopilotV2Routes() {
	s.route("GET /api/v3/enterprises/{enterprise}/copilot/billing/seats", s.requireEnterpriseOwner(s.handleListEnterpriseCopilotSeats))
	s.route("POST /api/v3/enterprises/{enterprise}/copilot/billing/selected_enterprise_teams", s.requireEnterpriseOwner(s.handleAddEnterpriseCopilotTeams))
	s.route("DELETE /api/v3/enterprises/{enterprise}/copilot/billing/selected_enterprise_teams", s.requireEnterpriseOwner(s.handleDeleteEnterpriseCopilotTeams))
	s.route("POST /api/v3/enterprises/{enterprise}/copilot/billing/selected_users", s.requireEnterpriseOwner(s.handleAddEnterpriseCopilotUsers))
	s.route("DELETE /api/v3/enterprises/{enterprise}/copilot/billing/selected_users", s.requireEnterpriseOwner(s.handleDeleteEnterpriseCopilotUsers))
	s.route("GET /api/v3/enterprises/{enterprise}/members/{username}/copilot", s.requireEnterpriseOwner(s.handleGetEnterpriseMemberCopilot))

	aiRead := func(next http.HandlerFunc) http.HandlerFunc {
		return s.requireEnterpriseOwnerOrPermission("enterprise_ai_controls", "read", next)
	}
	aiWrite := func(next http.HandlerFunc) http.HandlerFunc {
		return s.requireEnterpriseOwnerOrPermission("enterprise_ai_controls", "write", next)
	}
	s.route("GET /api/v3/enterprises/{enterprise}/copilot/content_exclusion", aiRead(s.handleGetEnterpriseCopilotContentExclusion))
	s.route("PUT /api/v3/enterprises/{enterprise}/copilot/content_exclusion", aiWrite(s.handlePutEnterpriseCopilotContentExclusion))
	s.route("GET /api/v3/enterprises/{enterprise}/copilot/custom-agents", aiRead(s.handleGetEnterpriseCopilotCustomAgents))
	s.route("GET /api/v3/enterprises/{enterprise}/copilot/custom-agents/source", aiRead(s.handleGetEnterpriseCopilotCustomAgentsSource))
	s.route("PUT /api/v3/enterprises/{enterprise}/copilot/custom-agents/source", aiWrite(s.handlePutEnterpriseCopilotCustomAgentsSource))
	s.route("DELETE /api/v3/enterprises/{enterprise}/copilot/custom-agents/source", aiWrite(s.handleDeleteEnterpriseCopilotCustomAgentsSource))
	s.route("GET /api/v3/enterprises/{enterprise}/copilot/usage-records", s.requireEnterpriseOwnerOrPermission("copilot_metrics", "read", s.handleGetEnterpriseCopilotUsageRecords))
}

func (s *Server) requireEnterpriseOwnerOrPermission(permission, level string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.resolveEnterprise(w, r) {
			return
		}
		if user := ghUserFromContext(r.Context()); user != nil && user.SiteAdmin {
			next(w, r)
			return
		}
		allowed := func(permissions map[string]string) bool {
			actual := permissions[permission]
			return actual == "write" || level == "read" && actual == "read"
		}
		if token := ghInstallationTokenFromContext(r.Context()); token != nil && allowed(token.Permissions) {
			next(w, r)
			return
		}
		if token := ghUserToServerTokenFromContext(r.Context()); token != nil && allowed(token.Permissions) {
			next(w, r)
			return
		}
		writeGHError(w, http.StatusForbidden, "Resource not accessible by integration")
	}
}

func enterpriseCopilotSeatKey(userID, teamID int) string {
	if teamID == 0 {
		return "user:" + strconv.Itoa(userID)
	}
	return "team:" + strconv.Itoa(teamID) + ":user:" + strconv.Itoa(userID)
}

func (s *Server) expireEnterpriseCopilotSeatsLocked() {
	today := s.store.currentTime().Format("2006-01-02")
	for key, seat := range s.store.EnterpriseSettings.EnterpriseCopilotSeats {
		if seat.PendingCancellationDate != "" && seat.PendingCancellationDate <= today {
			delete(s.store.EnterpriseSettings.EnterpriseCopilotSeats, key)
		}
	}
}

func (s *Server) addEnterpriseCopilotSeatLocked(userID int, team *EnterpriseTeam) bool {
	teamID, teamSlug := 0, ""
	if team != nil {
		teamID, teamSlug = team.ID, team.Slug
	}
	key := enterpriseCopilotSeatKey(userID, teamID)
	now := s.store.currentTime()
	if seat := s.store.EnterpriseSettings.EnterpriseCopilotSeats[key]; seat != nil {
		if seat.PendingCancellationDate != "" {
			seat.PendingCancellationDate = ""
			seat.UpdatedAt = now
			return true
		}
		return false
	}
	s.store.EnterpriseSettings.EnterpriseCopilotSeats[key] = &CopilotSeat{
		OrgLogin: s.enterpriseCopilotScope(), UserID: userID, AssigningTeamSlug: teamSlug,
		CreatedAt: now, UpdatedAt: now,
	}
	return true
}

func (s *Server) enterpriseCopilotSeatJSON(seat *CopilotSeat, baseURL string) map[string]interface{} {
	var assignee interface{}
	if user := s.store.GetUserByID(seat.UserID); user != nil {
		assignee = userToJSON(user)
	}
	var assigningTeam interface{}
	if seat.AssigningTeamSlug != "" {
		if team := s.store.GetEnterpriseTeam(seat.AssigningTeamSlug); team != nil {
			assigningTeam = s.enterpriseTeamJSON(team, baseURL)
		}
	}
	var cancellation interface{}
	if seat.PendingCancellationDate != "" {
		cancellation = seat.PendingCancellationDate
	}
	return map[string]interface{}{
		"created_at": seat.CreatedAt.Format(time.RFC3339), "updated_at": seat.UpdatedAt.Format(time.RFC3339),
		"pending_cancellation_date": cancellation, "last_activity_at": nil,
		"last_activity_editor": nil, "last_authenticated_at": nil, "plan_type": "business",
		"assignee": assignee, "assigning_team": assigningTeam,
	}
}

func (s *Server) enterpriseCopilotSeatRows() []*CopilotSeat {
	s.store.mu.Lock()
	s.expireEnterpriseCopilotSeatsLocked()
	rows := make([]*CopilotSeat, 0, len(s.store.EnterpriseSettings.EnterpriseCopilotSeats))
	for _, seat := range s.store.EnterpriseSettings.EnterpriseCopilotSeats {
		copy := *seat
		rows = append(rows, &copy)
	}
	s.store.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].UserID != rows[j].UserID {
			return rows[i].UserID < rows[j].UserID
		}
		return rows[i].AssigningTeamSlug < rows[j].AssigningTeamSlug
	})
	return rows
}

func (s *Server) handleListEnterpriseCopilotSeats(w http.ResponseWriter, r *http.Request) {
	rows := s.enterpriseCopilotSeatRows()
	unique := map[int]bool{}
	for _, seat := range rows {
		unique[seat.UserID] = true
	}
	page := paginateAndLink(w, r, rows)
	result := make([]map[string]interface{}, len(page))
	for i, seat := range page {
		result[i] = s.enterpriseCopilotSeatJSON(seat, s.baseURL(r))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"total_seats": len(unique), "seats": result})
}

func (s *Server) enterpriseCopilotUsers(w http.ResponseWriter, usernames []string) []*User {
	if len(usernames) == 0 {
		writeGHValidationError(w, "CopilotSeat", "selected_usernames", "missing_field")
		return nil
	}
	users := make([]*User, 0, len(usernames))
	for _, username := range usernames {
		user := s.store.LookupUserByLogin(username)
		if user == nil || user.Type == "Bot" {
			writeGHError(w, http.StatusUnprocessableEntity, "User is not an enterprise member: "+username)
			return nil
		}
		users = append(users, user)
	}
	return users
}

func (s *Server) enterpriseCopilotTeams(w http.ResponseWriter, names []string) []*EnterpriseTeam {
	if len(names) == 0 {
		writeGHValidationError(w, "CopilotSeat", "selected_enterprise_teams", "missing_field")
		return nil
	}
	teams := make([]*EnterpriseTeam, 0, len(names))
	for _, name := range names {
		team := s.store.GetEnterpriseTeam(slugify(name))
		if team == nil {
			writeGHError(w, http.StatusUnprocessableEntity, "Enterprise team does not exist: "+name)
			return nil
		}
		teams = append(teams, team)
	}
	return teams
}

func (s *Server) handleAddEnterpriseCopilotUsers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Usernames []string `json:"selected_usernames"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	users := s.enterpriseCopilotUsers(w, req.Usernames)
	if users == nil {
		return
	}
	s.store.mu.Lock()
	s.expireEnterpriseCopilotSeatsLocked()
	created := 0
	for _, user := range users {
		if s.addEnterpriseCopilotSeatLocked(user.ID, nil) {
			created++
		}
	}
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]interface{}{"seats_created": created})
}

func (s *Server) handleDeleteEnterpriseCopilotUsers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Usernames []string `json:"selected_usernames"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	users := s.enterpriseCopilotUsers(w, req.Usernames)
	if users == nil {
		return
	}
	s.store.mu.Lock()
	now := s.store.currentTime()
	cancelled := 0
	for _, user := range users {
		if seat := s.store.EnterpriseSettings.EnterpriseCopilotSeats[enterpriseCopilotSeatKey(user.ID, 0)]; seat != nil &&
			seat.PendingCancellationDate == "" {
			seat.PendingCancellationDate = copilotNextCycleDate(now)
			seat.UpdatedAt = now
			cancelled++
		}
	}
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{"seats_cancelled": cancelled})
}

func (s *Server) handleAddEnterpriseCopilotTeams(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Teams []string `json:"selected_enterprise_teams"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	teams := s.enterpriseCopilotTeams(w, req.Teams)
	if teams == nil {
		return
	}
	s.store.mu.Lock()
	s.expireEnterpriseCopilotSeatsLocked()
	created := 0
	for _, team := range teams {
		for _, userID := range team.MemberIDs {
			if s.addEnterpriseCopilotSeatLocked(userID, team) {
				created++
			}
		}
	}
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]interface{}{"seats_created": created})
}

func (s *Server) handleDeleteEnterpriseCopilotTeams(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Teams []string `json:"selected_enterprise_teams"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	teams := s.enterpriseCopilotTeams(w, req.Teams)
	if teams == nil {
		return
	}
	s.store.mu.Lock()
	now := s.store.currentTime()
	cancelled := 0
	for _, team := range teams {
		for _, userID := range team.MemberIDs {
			if seat := s.store.EnterpriseSettings.EnterpriseCopilotSeats[enterpriseCopilotSeatKey(userID, team.ID)]; seat != nil &&
				seat.PendingCancellationDate == "" {
				seat.PendingCancellationDate = copilotNextCycleDate(now)
				seat.UpdatedAt = now
				cancelled++
			}
		}
	}
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{"seats_cancelled": cancelled})
}

func (s *Server) handleGetEnterpriseMemberCopilot(w http.ResponseWriter, r *http.Request) {
	user := s.store.LookupUserByLogin(r.PathValue("username"))
	if user == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	all := s.enterpriseCopilotSeatRows()
	rows := make([]map[string]interface{}, 0)
	for _, seat := range all {
		if seat.UserID == user.ID {
			rows = append(rows, s.enterpriseCopilotSeatJSON(seat, s.baseURL(r)))
		}
	}
	if len(rows) == 0 {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"total_seats": len(rows), "seats": rows})
}

func (s *Server) handleGetEnterpriseCopilotContentExclusion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.store.GetCopilotContentExclusion(s.enterpriseCopilotScope()))
}

func (s *Server) handlePutEnterpriseCopilotContentExclusion(w http.ResponseWriter, r *http.Request) {
	var rules map[string][]interface{}
	if !decodeJSONBody(w, r, &rules) {
		return
	}
	if rules == nil {
		rules = map[string][]interface{}{}
	}
	for scope, entries := range rules {
		for _, entry := range entries {
			if !validContentExclusionRule(entry) {
				writeGHError(w, http.StatusUnprocessableEntity,
					fmt.Sprintf("Invalid content exclusion rule for %q.", scope))
				return
			}
		}
	}
	s.store.SetCopilotContentExclusion(s.enterpriseCopilotScope(), rules)
	writeJSON(w, http.StatusOK, map[string]interface{}{"message": "Content exclusion settings updated."})
}

func (s *Server) enterpriseCustomAgentSource() (*Org, *Repo) {
	s.store.mu.RLock()
	org := s.store.Orgs[s.store.EnterpriseSettings.CopilotCustomAgentsSourceOrgID]
	s.store.mu.RUnlock()
	if org == nil {
		return nil, nil
	}
	return org, s.store.GetRepo(org.Login, ".github-private")
}

func customAgentSourceJSON(org *Org, repo *Repo) map[string]interface{} {
	return map[string]interface{}{
		"organization": map[string]interface{}{"id": org.ID, "login": org.Login, "avatar_url": org.AvatarURL},
		"repository":   map[string]interface{}{"id": repo.ID, "name": repo.Name, "full_name": repo.FullName},
	}
}

func (s *Server) handleGetEnterpriseCopilotCustomAgentsSource(w http.ResponseWriter, _ *http.Request) {
	org, repo := s.enterpriseCustomAgentSource()
	if org == nil || repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, customAgentSourceJSON(org, repo))
}

func (s *Server) handlePutEnterpriseCopilotCustomAgentsSource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrganizationID int   `json:"organization_id"`
		CreateRuleset  *bool `json:"create_ruleset"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	s.store.mu.RLock()
	org := s.store.Orgs[req.OrganizationID]
	s.store.mu.RUnlock()
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	repo := s.store.GetRepo(org.Login, ".github-private")
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "The organization must have a .github-private repository.")
		return
	}
	createRuleset := req.CreateRuleset == nil || *req.CreateRuleset
	s.store.mu.Lock()
	s.store.EnterpriseSettings.CopilotCustomAgentsSourceOrgID = org.ID
	needsRuleset := createRuleset && s.store.EnterpriseSettings.CopilotCustomAgentsRulesetID == 0
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	if needsRuleset {
		ruleset := s.store.CreateEnterpriseRuleset(s.enterpriseSlug(), &Ruleset{
			Name: "Protect Copilot custom agents", Target: "branch", Enforcement: "active",
			Rules: []Rule{{Type: "file_path_restriction", Parameters: map[string]interface{}{
				"restricted_file_paths": []string{"agents/*.md", ".github/agents/*.md"},
				"repository_id":         repo.ID,
			}}},
		})
		s.store.mu.Lock()
		s.store.EnterpriseSettings.CopilotCustomAgentsRulesetID = ruleset.ID
		s.store.persistEnterpriseSettings()
		s.store.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, customAgentSourceJSON(org, repo))
}

func (s *Server) handleDeleteEnterpriseCopilotCustomAgentsSource(w http.ResponseWriter, _ *http.Request) {
	s.store.mu.Lock()
	if s.store.EnterpriseSettings.CopilotCustomAgentsSourceOrgID == 0 {
		s.store.mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.EnterpriseSettings.CopilotCustomAgentsSourceOrgID = 0
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func customAgentName(filePath string) string {
	name := strings.TrimSuffix(path.Base(filePath), path.Ext(filePath))
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' })
	for i := range parts {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, " ")
}

func (s *Server) handleGetEnterpriseCopilotCustomAgents(w http.ResponseWriter, r *http.Request) {
	org, repo := s.enterpriseCustomAgentSource()
	if org == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"custom_agents": nil})
		return
	}
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	tree, _, err := s.repoTreeAtRef(repo, repo.DefaultBranch)
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	agents := make([]map[string]interface{}, 0)
	files := tree.Files()
	err = files.ForEach(func(file *object.File) error {
		if strings.HasPrefix(file.Name, "agents/") && strings.HasSuffix(file.Name, ".md") {
			agents = append(agents, map[string]interface{}{
				"name": customAgentName(file.Name), "file_path": file.Name,
				"url": s.baseURL(r) + "/" + org.Login + "/.github-private/blob/" + repo.DefaultBranch + "/" + file.Name,
			})
		}
		return nil
	})
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "Git object unavailable")
		return
	}
	sort.Slice(agents, func(i, j int) bool {
		return agents[i]["file_path"].(string) < agents[j]["file_path"].(string)
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"custom_agents": agents})
}

func (s *Server) handleGetEnterpriseCopilotUsageRecords(w http.ResponseWriter, r *http.Request) {
	if !copilotMetricsWindow(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, []interface{}{})
}

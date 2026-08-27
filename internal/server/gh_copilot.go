package bleephub

// GitHub Copilot organization REST surface: seat billing, seat details, usage
// metrics, content exclusion, coding-agent permissions, and cloud-agent config.

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHCopilotRoutes() {
	s.route("GET /api/v3/orgs/{org}/copilot/billing", s.handleGetCopilotOrganizationDetails)
	s.route("GET /api/v3/orgs/{org}/copilot/billing/seats", s.handleListCopilotSeats)
	s.route("POST /api/v3/orgs/{org}/copilot/billing/selected_users", s.handleAddCopilotSeatsForUsers)
	s.route("DELETE /api/v3/orgs/{org}/copilot/billing/selected_users", s.handleCancelCopilotSeatsForUsers)
	s.route("POST /api/v3/orgs/{org}/copilot/billing/selected_teams", s.handleAddCopilotSeatsForTeams)
	s.route("DELETE /api/v3/orgs/{org}/copilot/billing/selected_teams", s.handleCancelCopilotSeatsForTeams)
	s.route("GET /api/v3/orgs/{org}/members/{username}/copilot", s.handleGetCopilotSeatDetailsForUser)
	s.route("GET /api/v3/orgs/{org}/copilot/metrics", s.handleCopilotMetricsForOrganization)
	s.route("GET /api/v3/orgs/{org}/team/{team_slug}/copilot/metrics", s.handleCopilotMetricsForTeam)
	s.route("GET /api/v3/orgs/{org}/copilot/metrics/reports/organization-1-day", s.handleCopilotOneDayReport)
	s.route("GET /api/v3/orgs/{org}/copilot/metrics/reports/users-1-day", s.handleCopilotOneDayReport)
	s.route("GET /api/v3/orgs/{org}/copilot/metrics/reports/user-teams-1-day", s.handleCopilotOneDayReport)
	s.route("GET /api/v3/orgs/{org}/copilot/metrics/reports/repos-1-day", s.handleCopilotOneDayReport)
	s.route("GET /api/v3/orgs/{org}/copilot/metrics/reports/organization-28-day/latest", s.handleCopilotLatest28DayReport)
	s.route("GET /api/v3/orgs/{org}/copilot/metrics/reports/users-28-day/latest", s.handleCopilotLatest28DayReport)
	s.route("GET /api/v3/orgs/{org}/copilot/content_exclusion", s.handleGetCopilotContentExclusion)
	s.route("PUT /api/v3/orgs/{org}/copilot/content_exclusion", s.handleSetCopilotContentExclusion)
	s.route("GET /api/v3/orgs/{org}/copilot/coding-agent/permissions", s.handleGetCopilotCodingAgentPermissions)
	s.route("PUT /api/v3/orgs/{org}/copilot/coding-agent/permissions", s.handleSetCopilotCodingAgentPermissions)
	s.route("GET /api/v3/orgs/{org}/copilot/coding-agent/permissions/repositories", s.handleListCopilotCodingAgentRepos)
	s.route("PUT /api/v3/orgs/{org}/copilot/coding-agent/permissions/repositories", s.handleSetCopilotCodingAgentRepos)
	s.route("PUT /api/v3/orgs/{org}/copilot/coding-agent/permissions/repositories/{repository_id}", s.handleEnableCopilotCodingAgentRepo)
	s.route("DELETE /api/v3/orgs/{org}/copilot/coding-agent/permissions/repositories/{repository_id}", s.handleDisableCopilotCodingAgentRepo)
	s.route("GET /api/v3/repos/{owner}/{repo}/copilot/cloud-agent/configuration", s.handleGetCopilotCloudAgentConfiguration)
}

// copilotOrgAdmin resolves {org} and requires an authenticated org owner — the
// audience GitHub grants the Copilot billing/metrics/policy surface. Writes
// 401/404/403 and returns nil on failure.
func (s *Server) copilotOrgAdmin(w http.ResponseWriter, r *http.Request) *store.Org {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return nil
	}
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	if !s.viewerCanAdminOrg(r.Context(), org.Login) {
		writeGHError(w, http.StatusForbidden, "Must be an organization owner.")
		return nil
	}
	return org
}

func (s *Server) copilotSeatJSON(seat *store.CopilotSeat, org *store.Org, baseURL string) map[string]interface{} {
	var assignee interface{}
	if u := s.store.GetUserByID(seat.UserID); u != nil {
		assignee = store.UserToJSON(u, baseURL)
	}
	var assigningTeam interface{}
	if seat.AssigningTeamSlug != "" {
		if team := s.store.GetTeam(org.Login, seat.AssigningTeamSlug); team != nil {
			assigningTeam = teamSimpleJSON(team, org, s.store, baseURL)
		}
	}
	var pendingCancellation interface{}
	if seat.PendingCancellationDate != "" {
		pendingCancellation = seat.PendingCancellationDate
	}
	// An unused seat reads null on both activity fields; no timestamp is invented.
	lastActivityAt, lastActivityEditor := s.CopilotSeatActivityJSON(org.Login, seat.UserID)
	return map[string]interface{}{
		"assignee":                  assignee,
		"organization":              orgSimpleJSON(org, baseURL),
		"assigning_team":            assigningTeam,
		"pending_cancellation_date": pendingCancellation,
		"last_activity_at":          lastActivityAt,
		"last_activity_editor":      lastActivityEditor,
		"created_at":                seat.CreatedAt.Format(time.RFC3339),
		"updated_at":                seat.UpdatedAt.Format(time.RFC3339),
		"plan_type":                 s.store.CopilotPolicies.GetCopilotOrgPolicy(org.Login).PlanType,
	}
}

func (s *Server) handleGetCopilotOrganizationDetails(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	body := map[string]interface{}{"seat_breakdown": s.CopilotSeatBreakdown(org, s.currentTime())}
	for key, value := range copilotPolicyJSON(s.store.CopilotPolicies.GetCopilotOrgPolicy(org.Login)) {
		body[key] = value
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleListCopilotSeats(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	seats := s.store.ListCopilotSeats(org.Login)
	total := len(seats)
	page := paginateAndLink(w, r, seats)
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(page))
	for _, seat := range page {
		out = append(out, s.copilotSeatJSON(seat, org, base))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_seats": total,
		"seats":       out,
	})
}

// resolveCopilotSeatUsers maps usernames to active org members, writing a 422
// and returning nil if any does not resolve — GitHub validates the whole batch
// before assigning any seat.
func (s *Server) resolveCopilotSeatUsers(w http.ResponseWriter, org *store.Org, usernames []string) []int {
	if len(usernames) == 0 {
		store.WriteGHValidationError(w, "CopilotSeat", "selected_usernames", "missing_field")
		return nil
	}
	ids := make([]int, 0, len(usernames))
	var invalid []string
	for _, login := range usernames {
		u := s.store.LookupUserByLogin(login)
		if u == nil {
			invalid = append(invalid, login)
			continue
		}
		m := s.store.GetMembership(org.Login, u.ID)
		if m == nil || m.State != store.MembershipStateActive {
			invalid = append(invalid, login)
			continue
		}
		ids = append(ids, u.ID)
	}
	if len(invalid) > 0 {
		writeGHError(w, http.StatusUnprocessableEntity,
			"Copilot seats cannot be managed for users that are not active members of this organization: "+strings.Join(invalid, ", "))
		return nil
	}
	return ids
}

func (s *Server) handleAddCopilotSeatsForUsers(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	var req struct {
		SelectedUsernames []string `json:"selected_usernames"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	ids := s.resolveCopilotSeatUsers(w, org, req.SelectedUsernames)
	if ids == nil {
		return
	}
	created := s.store.AddCopilotSeats(org.Login, ids, "")
	writeJSON(w, http.StatusCreated, map[string]interface{}{"seats_created": created})
}

func (s *Server) handleCancelCopilotSeatsForUsers(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	var req struct {
		SelectedUsernames []string `json:"selected_usernames"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	ids := s.resolveCopilotSeatUsers(w, org, req.SelectedUsernames)
	if ids == nil {
		return
	}
	cancelled, teamAssigned := s.store.CancelCopilotSeatsForUsers(org.Login, ids)
	if len(teamAssigned) > 0 {
		logins := make([]string, 0, len(teamAssigned))
		for _, id := range teamAssigned {
			if u := s.store.GetUserByID(id); u != nil {
				logins = append(logins, u.Login)
			}
		}
		writeGHError(w, http.StatusUnprocessableEntity,
			"Copilot seats assigned via a team cannot be cancelled individually: "+strings.Join(logins, ", "))
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"seats_cancelled": cancelled})
}

// resolveCopilotSeatTeams maps team names or slugs to the org's teams, writing a
// 422 and returning nil if any does not resolve.
func (s *Server) resolveCopilotSeatTeams(w http.ResponseWriter, org *store.Org, names []string) []*store.Team {
	if len(names) == 0 {
		store.WriteGHValidationError(w, "CopilotSeat", "selected_teams", "missing_field")
		return nil
	}
	teams := make([]*store.Team, 0, len(names))
	var invalid []string
	for _, name := range names {
		team := s.store.GetTeam(org.Login, name)
		if team == nil {
			team = s.store.GetTeam(org.Login, store.Slugify(name))
		}
		if team == nil {
			invalid = append(invalid, name)
			continue
		}
		teams = append(teams, team)
	}
	if len(invalid) > 0 {
		writeGHError(w, http.StatusUnprocessableEntity,
			"Copilot seats cannot be managed for teams that do not belong to this organization: "+strings.Join(invalid, ", "))
		return nil
	}
	return teams
}

func (s *Server) handleAddCopilotSeatsForTeams(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	var req struct {
		SelectedTeams []string `json:"selected_teams"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	teams := s.resolveCopilotSeatTeams(w, org, req.SelectedTeams)
	if teams == nil {
		return
	}
	created := 0
	for _, team := range teams {
		members := s.store.ListTeamMembers(org.Login, team.Slug)
		ids := make([]int, 0, len(members))
		for _, m := range members {
			ids = append(ids, m.ID)
		}
		created += s.store.AddCopilotSeats(org.Login, ids, team.Slug)
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{"seats_created": created})
}

func (s *Server) handleCancelCopilotSeatsForTeams(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	var req struct {
		SelectedTeams []string `json:"selected_teams"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	teams := s.resolveCopilotSeatTeams(w, org, req.SelectedTeams)
	if teams == nil {
		return
	}
	cancelled := 0
	for _, team := range teams {
		cancelled += s.store.CancelCopilotSeatsForTeam(org.Login, team.Slug)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"seats_cancelled": cancelled})
}

func (s *Server) handleGetCopilotSeatDetailsForUser(w http.ResponseWriter, r *http.Request) {
	caller := ghUserFromContext(r.Context())
	if caller == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// The self-lookup arm is intersected with the credential's reach of the org:
	// "asking about myself" is a fact about the bearer, not the app speaking for
	// them.
	username := r.PathValue("username")
	self := strings.EqualFold(caller.Login, username) && s.viewerReachesOrg(r.Context(), org.Login)
	if !self && !s.viewerCanAdminOrg(r.Context(), org.Login) {
		writeGHError(w, http.StatusForbidden, "Must be an organization owner.")
		return
	}
	user := s.store.LookupUserByLogin(username)
	if user == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	m := s.store.GetMembership(org.Login, user.ID)
	if m == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if m.State == store.MembershipStatePending {
		writeGHError(w, http.StatusUnprocessableEntity, "User has a pending organization invitation.")
		return
	}
	seat := s.store.GetCopilotSeat(org.Login, user.ID)
	if seat == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.copilotSeatJSON(seat, org, s.baseURL(r)))
}

// copilotMetricsWindow validates the optional since/until query parameters,
// writing a 422 and returning false on a malformed value.
func copilotMetricsWindow(w http.ResponseWriter, r *http.Request) bool {
	for _, name := range []string{"since", "until"} {
		if v := r.URL.Query().Get(name); v != "" {
			if _, err := time.Parse(time.RFC3339, v); err != nil {
				writeGHError(w, http.StatusUnprocessableEntity,
					fmt.Sprintf("Invalid %s parameter. Expected an ISO 8601 timestamp.", name))
				return false
			}
		}
	}
	return true
}

func (s *Server) handleCopilotMetricsForOrganization(w http.ResponseWriter, r *http.Request) {
	if githubAPIVersionFromContext(r.Context()) == "2026-03-10" {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	if !copilotMetricsWindow(w, r) {
		return
	}
	// An empty array is the documented no-activity response, not fabricated data.
	since, until := copilotMetricsWindowBounds(r)
	writeJSON(w, http.StatusOK, s.CopilotMetricsForOrg(org.Login, "", since, until))
}

func (s *Server) handleCopilotMetricsForTeam(w http.ResponseWriter, r *http.Request) {
	if githubAPIVersionFromContext(r.Context()) == "2026-03-10" {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	teamSlug := r.PathValue("team_slug")
	if s.store.GetTeam(org.Login, teamSlug) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !copilotMetricsWindow(w, r) {
		return
	}
	since, until := copilotMetricsWindowBounds(r)
	writeJSON(w, http.StatusOK, s.CopilotMetricsForOrg(org.Login, teamSlug, since, until))
}

// handleCopilotOneDayReport serves the three org single-day report endpoints.
// The day parameter is required; a day with no recorded activity has no report,
// so a valid request gets the documented 204.
func (s *Server) handleCopilotOneDayReport(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	day := r.URL.Query().Get("day")
	if day == "" {
		store.WriteGHValidationError(w, "CopilotMetricsReport", "day", "missing_field")
		return
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		store.WriteGHValidationError(w, "CopilotMetricsReport", "day", "invalid")
		return
	}
	metrics := s.CopilotMetricsForOrg(org.Login, "", day, day)
	if len(metrics) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"download_links": []string{s.baseURL(r) + "/ui-data/orgs/" + org.Login + "/copilot/usage?since=" + day + "&until=" + day},
		"report_day":     day,
	})
}

// handleCopilotLatest28DayReport serves the two latest-28-day report endpoints.
// The period is the latest complete 28-day window (ending yesterday, UTC); with
// no recorded activity download_links is empty.
func (s *Server) handleCopilotLatest28DayReport(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	end := s.currentTime().AddDate(0, 0, -1)
	start := end.AddDate(0, 0, -27)
	startDay, endDay := start.Format("2006-01-02"), end.Format("2006-01-02")
	links := []string{}
	if len(s.CopilotMetricsForOrg(org.Login, "", startDay, endDay)) > 0 {
		links = append(links, s.baseURL(r)+"/ui-data/orgs/"+org.Login+"/copilot/usage?since="+startDay+"&until="+endDay)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"download_links":   links,
		"report_start_day": startDay,
		"report_end_day":   endDay,
	})
}

func (s *Server) handleGetCopilotContentExclusion(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	writeJSON(w, http.StatusOK, s.store.GetCopilotContentExclusion(org.Login))
}

func (s *Server) handleSetCopilotContentExclusion(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
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
					fmt.Sprintf("Invalid content exclusion rule for %q: each rule must be a path string or an object with exactly one of ifAnyMatch / ifNoneMatch.", scope))
				return
			}
		}
	}
	s.store.SetCopilotContentExclusion(org.Login, rules)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "Content exclusion settings updated.",
	})
}

// validContentExclusionRule accepts the documented rule forms: a path string,
// or an object with exactly one of ifAnyMatch / ifNoneMatch holding strings.
func validContentExclusionRule(entry interface{}) bool {
	switch v := entry.(type) {
	case string:
		return true
	case map[string]interface{}:
		if len(v) != 1 {
			return false
		}
		for key, val := range v {
			if key != "ifAnyMatch" && key != "ifNoneMatch" {
				return false
			}
			items, ok := val.([]interface{})
			if !ok {
				return false
			}
			for _, item := range items {
				if _, ok := item.(string); !ok {
					return false
				}
			}
		}
		return true
	default:
		return false
	}
}

func (s *Server) handleGetCopilotCodingAgentPermissions(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	p := s.store.GetCopilotCodingAgentPermissions(org.Login)
	out := map[string]interface{}{"enabled_repositories": p.EnabledRepositories}
	if p.EnabledRepositories == "selected" {
		out["selected_repositories_url"] = s.baseURL(r) + "/api/v3/orgs/" + org.Login + "/copilot/coding-agent/permissions/repositories"
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSetCopilotCodingAgentPermissions(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	var req struct {
		EnabledRepositories string `json:"enabled_repositories"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	switch req.EnabledRepositories {
	case "all", "selected", "none":
	case "":
		store.WriteGHValidationError(w, "CopilotCodingAgentPermissions", "enabled_repositories", "missing_field")
		return
	default:
		store.WriteGHValidationError(w, "CopilotCodingAgentPermissions", "enabled_repositories", "invalid")
		return
	}
	s.store.SetCopilotCodingAgentPolicy(org.Login, req.EnabledRepositories)
	w.WriteHeader(http.StatusNoContent)
}

// copilotCodingAgentSelectedGate enforces the 409 the selected-repository
// sub-resource returns when the org policy is not "selected".
func (s *Server) copilotCodingAgentSelectedGate(w http.ResponseWriter, org *store.Org) bool {
	p := s.store.GetCopilotCodingAgentPermissions(org.Login)
	if p.EnabledRepositories != "selected" {
		writeGHError(w, http.StatusConflict,
			"The organization's Copilot coding agent policy is not set to selected repositories.")
		return false
	}
	return true
}

func (s *Server) handleListCopilotCodingAgentRepos(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	if !s.copilotCodingAgentSelectedGate(w, org) {
		return
	}
	p := s.store.GetCopilotCodingAgentPermissions(org.Login)
	ids := make([]int, len(p.SelectedRepositoryIDs))
	copy(ids, p.SelectedRepositoryIDs)
	sort.Ints(ids)
	base := s.baseURL(r)
	repos := make([]map[string]interface{}, 0, len(ids))
	for _, id := range ids {
		s.store.Mu.RLock()
		repo := s.store.Repos[id]
		s.store.Mu.RUnlock()
		if repo != nil {
			repos = append(repos, store.RepoToJSON(repo, s.store, base))
		}
	}
	total := len(repos)
	repos = paginateAndLink(w, r, repos)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count":  total,
		"repositories": repos,
	})
}

// copilotOrgRepoIDs validates every id references an existing repository owned
// by the org, writing a 422 and returning false otherwise.
func (s *Server) copilotOrgRepoIDs(w http.ResponseWriter, org *store.Org, ids []int) bool {
	var invalid []string
	for _, id := range ids {
		s.store.Mu.RLock()
		repo := s.store.Repos[id]
		s.store.Mu.RUnlock()
		if repo == nil || !strings.HasPrefix(repo.FullName, org.Login+"/") {
			invalid = append(invalid, strconv.Itoa(id))
		}
	}
	if len(invalid) > 0 {
		writeGHError(w, http.StatusUnprocessableEntity,
			"The following repository IDs do not belong to this organization: "+strings.Join(invalid, ", "))
		return false
	}
	return true
}

func (s *Server) handleSetCopilotCodingAgentRepos(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	if !s.copilotCodingAgentSelectedGate(w, org) {
		return
	}
	var req struct {
		SelectedRepositoryIDs *[]int `json:"selected_repository_ids"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.SelectedRepositoryIDs == nil {
		store.WriteGHValidationError(w, "CopilotCodingAgentPermissions", "selected_repository_ids", "missing_field")
		return
	}
	if !s.copilotOrgRepoIDs(w, org, *req.SelectedRepositoryIDs) {
		return
	}
	s.store.SetCopilotCodingAgentSelectedRepos(org.Login, *req.SelectedRepositoryIDs)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEnableCopilotCodingAgentRepo(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	if !s.copilotCodingAgentSelectedGate(w, org) {
		return
	}
	id, err := strconv.Atoi(r.PathValue("repository_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.RLock()
	repo := s.store.Repos[id]
	s.store.Mu.RUnlock()
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.copilotOrgRepoIDs(w, org, []int{id}) {
		return
	}
	s.store.AddCopilotCodingAgentSelectedRepo(org.Login, id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDisableCopilotCodingAgentRepo(w http.ResponseWriter, r *http.Request) {
	org := s.copilotOrgAdmin(w, r)
	if org == nil {
		return
	}
	if !s.copilotCodingAgentSelectedGate(w, org) {
		return
	}
	id, err := strconv.Atoi(r.PathValue("repository_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.RLock()
	repo := s.store.Repos[id]
	s.store.Mu.RUnlock()
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.RemoveCopilotCodingAgentSelectedRepo(org.Login, id)
	w.WriteHeader(http.StatusNoContent)
}

// handleGetCopilotCloudAgentConfiguration serves the repository's Copilot cloud
// agent configuration. GitHub manages this through the settings UI only, so the
// REST surface is read-only and every repository reports GitHub's defaults.
func (s *Server) handleGetCopilotCloudAgentConfiguration(w http.ResponseWriter, r *http.Request) {
	if ghUserFromContext(r.Context()) == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"mcp_configuration": nil,
		"enabled_tools": map[string]interface{}{
			"codeql":                          true,
			"copilot_code_review":             true,
			"secret_scanning":                 true,
			"dependency_vulnerability_checks": true,
		},
		"require_actions_workflow_approval":            true,
		"is_firewall_enabled":                          true,
		"is_firewall_recommended_allowlist_enabled":    true,
		"custom_allowlist":                             []string{},
		"is_automations_enabled":                       true,
		"require_write_access_for_automation_triggers": true,
	})
}

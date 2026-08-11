package bleephub

import (
	"net/http"
	"strings"
)

// registerGHESAdminStatsRoutes closes the classic-token-only GHES
// installation administration surface. These paths intentionally have no
// enterprise slug: they address the appliance itself.
func (s *Server) registerGHESAdminStatsRoutes() {
	admin := s.requireGHESSiteAdmin
	s.registerGHESPreReceiveRoutes(admin)
	s.registerGHESDirectoryRoutes(admin)
	s.route("GET /api/v3/admin/hooks", admin(s.handleListGHESGlobalHooks))
	s.route("POST /api/v3/admin/hooks", admin(s.handleCreateGHESGlobalHook))
	s.route("GET /api/v3/admin/hooks/{hook_id}", admin(s.handleGetGHESGlobalHook))
	s.route("PATCH /api/v3/admin/hooks/{hook_id}", admin(s.handleUpdateGHESGlobalHook))
	s.route("DELETE /api/v3/admin/hooks/{hook_id}", admin(s.handleDeleteGHESGlobalHook))
	s.route("POST /api/v3/admin/hooks/{hook_id}/pings", admin(s.handlePingGHESGlobalHook))
	s.route("GET /api/v3/admin/keys", admin(s.handleListGHESPublicKeys))
	s.route("DELETE /api/v3/admin/keys/{key_ids}", admin(s.handleDeleteGHESPublicKeys))
	s.route("GET /api/v3/admin/tokens", admin(s.handleListGHESPersonalAccessTokens))
	s.route("DELETE /api/v3/admin/tokens/{token_id}", admin(s.handleDeleteGHESPersonalAccessToken))
	s.route("POST /api/v3/admin/users/{username}/authorizations", admin(s.handleCreateGHESImpersonationToken))
	s.route("DELETE /api/v3/admin/users/{username}/authorizations", admin(s.handleDeleteGHESImpersonationToken))
	s.route("GET /api/v3/enterprise/announcement", admin(s.handleGetEnterpriseAnnouncement))
	s.route("PATCH /api/v3/enterprise/announcement", admin(s.handleSetEnterpriseAnnouncement))
	s.route("DELETE /api/v3/enterprise/announcement", admin(s.handleDeleteEnterpriseAnnouncement))
	s.route("GET /api/v3/enterprise/settings/license", admin(s.handleGHESLicense))
	for _, name := range []string{
		"all", "comments", "gists", "hooks", "issues", "milestones", "orgs",
		"pages", "pulls", "repos", "security-products", "users",
	} {
		s.route("GET /api/v3/enterprise/stats/"+name, admin(s.handleGHESAdminStats))
	}
}

func (s *Server) requireGHESSiteAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := ghUserFromContext(r.Context())
		if user == nil {
			writeGHError(w, http.StatusUnauthorized, "Requires authentication")
			return
		}
		// GHES deliberately hides this surface from authenticated non-admins,
		// and from a site admin's narrow/delegated credentials (fine-grained
		// PAT, app or installation token) — those must not administer the box.
		if !user.SiteAdmin || !credentialConveysSiteAdmin(r.Context()) {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleGHESLicense(w http.ResponseWriter, _ *http.Request) {
	s.store.Mu.RLock()
	used := 0
	for _, user := range s.store.Users {
		if user.Type != "Bot" {
			used++
		}
	}
	s.store.Mu.RUnlock()
	// A development GHES appliance has an effectively unmetered local
	// license. Keep every numeric relationship internally consistent.
	seats := used
	if seats < 1 {
		seats = 1
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"seats": seats, "seats_used": used, "seats_available": seats - used,
		"kind": "standard", "days_until_expiration": 0, "expire_at": "",
	})
}

func (s *Server) ghesAdminStats() map[string]map[string]interface{} {
	s.store.Mu.RLock()
	repos := map[string]interface{}{
		"total_repos": len(s.store.Repos), "root_repos": 0, "fork_repos": 0,
		"org_repos": 0, "total_pushes": len(s.store.RepoActivities), "total_wikis": 0,
	}
	users := map[string]interface{}{"total_users": 0, "admin_users": 0, "suspended_users": 0}
	orgs := map[string]interface{}{
		"total_orgs": len(s.store.Orgs), "disabled_orgs": 0,
		"total_teams": len(s.store.Teams), "total_team_members": 0,
	}
	issues := map[string]interface{}{"total_issues": len(s.store.Issues), "open_issues": 0, "closed_issues": 0}
	milestones := map[string]interface{}{"total_milestones": len(s.store.Milestones), "open_milestones": 0, "closed_milestones": 0}
	pulls := map[string]interface{}{
		"total_pulls": len(s.store.PullRequests), "merged_pulls": 0,
		"mergeable_pulls": 0, "unmergeable_pulls": 0,
	}
	gists := map[string]interface{}{"total_gists": len(s.store.Gists), "private_gists": 0, "public_gists": 0}
	hooks := map[string]interface{}{"total_hooks": 0, "active_hooks": 0, "inactive_hooks": 0}
	comments := map[string]interface{}{
		"total_commit_comments": 0, "total_gist_comments": len(s.store.GistComments),
		"total_issue_comments": 0, "total_pull_request_comments": 0,
	}
	for _, repo := range s.store.Repos {
		if repo.Fork {
			repos["fork_repos"] = repos["fork_repos"].(int) + 1
		} else {
			repos["root_repos"] = repos["root_repos"].(int) + 1
		}
		if repo.OwnerType == "Organization" {
			repos["org_repos"] = repos["org_repos"].(int) + 1
		}
	}
	for _, user := range s.store.Users {
		if user.Type == "Bot" {
			continue
		}
		users["total_users"] = users["total_users"].(int) + 1
		if user.SiteAdmin {
			users["admin_users"] = users["admin_users"].(int) + 1
		}
		if user.Suspended {
			users["suspended_users"] = users["suspended_users"].(int) + 1
		}
	}
	for _, membership := range s.store.Memberships {
		if membership.State == MembershipStateActive {
			orgs["total_team_members"] = orgs["total_team_members"].(int) + 1
		}
	}
	for _, issue := range s.store.Issues {
		key := "open_issues"
		if issue.State == "CLOSED" {
			key = "closed_issues"
		}
		issues[key] = issues[key].(int) + 1
	}
	for _, milestone := range s.store.Milestones {
		key := "open_milestones"
		if milestone.State == "closed" {
			key = "closed_milestones"
		}
		milestones[key] = milestones[key].(int) + 1
	}
	for _, pull := range s.store.PullRequests {
		switch pull.State {
		case "MERGED":
			pulls["merged_pulls"] = pulls["merged_pulls"].(int) + 1
		default:
			if pull.Mergeable == "CONFLICTING" {
				pulls["unmergeable_pulls"] = pulls["unmergeable_pulls"].(int) + 1
			} else {
				pulls["mergeable_pulls"] = pulls["mergeable_pulls"].(int) + 1
			}
		}
	}
	for _, gist := range s.store.Gists {
		key := "private_gists"
		if gist.Public {
			key = "public_gists"
		}
		gists[key] = gists[key].(int) + 1
	}
	for _, comment := range s.store.Comments {
		key := "total_issue_comments"
		if comment.ParentType == "pull_request" {
			key = "total_pull_request_comments"
		}
		comments[key] = comments[key].(int) + 1
	}
	for _, list := range s.store.Hooks {
		for _, hook := range list {
			hooks["total_hooks"] = hooks["total_hooks"].(int) + 1
			key := "inactive_hooks"
			if hook.Active {
				key = "active_hooks"
			}
			hooks[key] = hooks[key].(int) + 1
		}
	}
	for _, list := range s.store.OrgHooks {
		for _, hook := range list {
			hooks["total_hooks"] = hooks["total_hooks"].(int) + 1
			key := "inactive_hooks"
			if hook.Active {
				key = "active_hooks"
			}
			hooks[key] = hooks[key].(int) + 1
		}
	}
	totalRepos, nonarchived := len(s.store.Repos), 0
	secretScanning, dependabot, codeScanning := 0, 0, 0
	for _, repo := range s.store.Repos {
		if !repo.Archived {
			nonarchived++
		}
		if repo.VulnerabilityAlertsEnabled {
			dependabot++
		}
		if s.store.CodeScanningDefaultSetups[repo.FullName] != nil {
			codeScanning++
		}
		if configID := s.store.EnterpriseCodeSecurityRepoConfigs[repo.ID]; configID != 0 {
			if config := s.store.EnterpriseCodeSecurityConfigs[configID]; config != nil &&
				(config.SecretScanning == "enabled" || config.AdvancedSecurity == "enabled") {
				secretScanning++
			}
		}
	}
	security := map[string]interface{}{
		"total_repos": totalRepos, "nonarchived_repos": nonarchived,
		"secret_scanning_enabled_repos":                 secretScanning,
		"secret_scanning_push_protection_enabled_repos": secretScanning,
		"code_scanning_enabled_repos":                   codeScanning, "code_scanning_pr_reviews_enabled_repos": 0,
		"code_scanning_default_setup_enabled_repos":  codeScanning,
		"code_scanning_default_setup_eligible_repos": nonarchived,
		"dependabot_alerts_enabled_repos":            dependabot,
		"dependabot_security_updates_enabled_repos":  0, "dependabot_version_updates_enabled_repos": 0,
		"advanced_security_enabled_repos": secretScanning, "active_committers": 0,
		"purchased_committers": 0, "maximum_committers": 0,
		"secret_protection_licenses": 0, "secret_protection_active_committers": 0,
		"code_security_licenses": 0, "code_security_active_committers": 0,
	}
	s.store.Mu.RUnlock()

	s.store.CommitComments.Mu.RLock()
	comments["total_commit_comments"] = len(s.store.CommitComments.ByID)
	s.store.CommitComments.Mu.RUnlock()
	s.store.Misc.Mu.RLock()
	pages := map[string]interface{}{"total_pages": len(s.store.Misc.PagesByRepo)}
	s.store.Misc.Mu.RUnlock()
	return map[string]map[string]interface{}{
		"repos": repos, "hooks": hooks, "pages": pages, "orgs": orgs, "users": users,
		"pulls": pulls, "issues": issues, "milestones": milestones, "gists": gists,
		"comments": comments, "security-products": security,
	}
}

func (s *Server) handleGHESAdminStats(w http.ResponseWriter, r *http.Request) {
	stats := s.ghesAdminStats()
	name := strings.TrimPrefix(r.URL.Path, "/api/v3/enterprise/stats/")
	if name == "all" {
		delete(stats, "security-products")
		writeJSON(w, http.StatusOK, stats)
		return
	}
	if value := stats[name]; value != nil {
		writeJSON(w, http.StatusOK, value)
		return
	}
	writeGHError(w, http.StatusNotFound, "Not Found")
}

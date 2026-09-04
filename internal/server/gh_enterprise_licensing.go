package bleephub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/google/uuid"
)

func (s *Server) registerGHEnterpriseLicensingRoutes() {
	s.route("GET /api/v3/enterprise-installation/{enterprise_or_org}/server-statistics", s.handleEnterpriseServerStatistics)
	s.route("GET /api/v3/enterprises/{enterprise}/consumed-licenses", s.requireEnterpriseOwner(s.handleEnterpriseConsumedLicenses))
	s.route("GET /api/v3/enterprises/{enterprise}/license-sync-status", s.requireEnterpriseOwner(s.handleEnterpriseLicenseSyncStatus))
	s.route("GET /api/v3/enterprises/{enterprise}/visual-studio-subscriptions", s.requireEnterpriseOwner(s.handleListVisualStudioSubscriptions))
	s.route("PUT /api/v3/enterprises/{enterprise}/visual-studio-subscriptions/{visual_studio_subscription_id}", s.requireEnterpriseOwner(s.handlePutVisualStudioSubscription))
	s.route("DELETE /api/v3/enterprises/{enterprise}/visual-studio-subscriptions/{visual_studio_subscription_id}", s.requireEnterpriseOwner(s.handleDeleteVisualStudioSubscription))
	s.route("GET /api/v3/enterprises/{enterprise}/installation", s.handleGetEnterpriseInstallation)

	innerSource := func(next http.HandlerFunc) http.HandlerFunc {
		return s.requireEnterpriseInstallationPermission("enterprise_innersource_vulnerabilities", "write", next)
	}
	s.route("POST /api/v3/enterprises/{enterprise}/innersource-vulnerabilities/sync", innerSource(s.handleSyncEnterpriseInnerSourceVulnerabilities))
	s.route("GET /api/v3/enterprises/{enterprise}/innersource-vulnerabilities/sync/status/{job_id}", innerSource(s.handleGetEnterpriseInnerSourceSync))
}

func (s *Server) requireEnterpriseInstallationPermission(permission, level string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.resolveEnterprise(w, r) {
			return
		}
		token := ghInstallationTokenFromContext(r.Context())
		if token == nil {
			writeGHError(w, http.StatusForbidden, "Resource not accessible by integration")
			return
		}
		actual := token.Permissions[permission]
		if actual != "write" && !(level == "read" && actual == "read") {
			writeGHError(w, http.StatusForbidden, "Resource not accessible by integration")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleGetEnterpriseInstallation(w http.ResponseWriter, r *http.Request) {
	if !s.resolveEnterprise(w, r) {
		return
	}
	app := ghAppFromContext(r.Context())
	if app == nil {
		writeGHError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	for _, installation := range s.store.ListAppInstallations(app.ID) {
		if installation.TargetType == "Enterprise" && installation.TargetLogin == s.enterpriseSlug() {
			writeJSON(w, http.StatusOK, installationToJSON(installation, s.baseURL(r)))
			return
		}
	}
	writeGHError(w, http.StatusNotFound, "Not Found")
}

func nonEmptyStrings(value string) []string {
	if value == "" {
		return []string{}
	}
	return []string{value}
}

func (s *Server) handleEnterpriseConsumedLicenses(w http.ResponseWriter, r *http.Request) {
	users := s.store.ListUsers()
	rows := make([]map[string]interface{}, 0, len(users))
	for _, user := range users {
		if user.Type == "Bot" {
			continue
		}
		rows = append(rows, map[string]interface{}{
			"github_com_login": user.Login, "github_com_name": user.Name,
			"enterprise_server_user_ids": []string{"bleephub:" + fmt.Sprint(user.ID)},
			"github_com_user":            true, "enterprise_server_user": true,
			"visual_studio_subscription_user": false, "license_type": "enterprise",
			"github_com_profile":      s.baseURL(r) + "/" + user.Login,
			"github_com_member_roles": []string{}, "github_com_enterprise_roles": []string{},
			"github_com_verified_domain_emails": nonEmptyStrings(user.Email),
			"github_com_saml_name_id":           user.Login, "github_com_orgs_with_pending_invites": []string{},
			"github_com_two_factor_auth": true, "enterprise_server_emails": nonEmptyStrings(user.Email),
			"visual_studio_license_status": "", "visual_studio_subscription_email": "",
			"total_user_accounts": 1,
		})
	}
	total := len(rows)
	rows = paginateAndLink(w, r, rows)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_seats_consumed": total, "total_seats_purchased": total, "users": rows,
	})
}

func (s *Server) handleEnterpriseLicenseSyncStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"server_instances": []interface{}{}})
}

func visualStudioSubscriptionListJSON(subscription *store.VisualStudioSubscription) map[string]interface{} {
	var username interface{}
	if subscription.Username != "" {
		username = subscription.Username
	}
	return map[string]interface{}{
		"email": subscription.Email, "subscriptionId": subscription.SubscriptionID,
		"username": username, "manual_match": subscription.ManualMatch,
	}
}

func visualStudioSubscriptionJSON(subscription *store.VisualStudioSubscription) map[string]interface{} {
	var username interface{}
	if subscription.Username != "" {
		username = subscription.Username
	}
	return map[string]interface{}{
		"visual_studio_subscription_email": subscription.Email,
		"subscription_id":                  subscription.SubscriptionID,
		"username":                         username, "manual_match": subscription.ManualMatch,
	}
}

func (s *Server) handleListVisualStudioSubscriptions(w http.ResponseWriter, r *http.Request) {
	unmatchedOnly := r.URL.Query().Get("is_unmatched_only") == "true"
	s.store.Mu.RLock()
	subscriptions := make([]*store.VisualStudioSubscription, 0, len(s.store.EnterpriseSettings.VisualStudioSubscriptions))
	for _, subscription := range s.store.EnterpriseSettings.VisualStudioSubscriptions {
		// "Unmatched" means the subscription is not linked to a GitHub user, i.e.
		// it has no username — not whether the match was manual.
		if unmatchedOnly && subscription.Username != "" {
			continue
		}
		copy := *subscription
		subscriptions = append(subscriptions, &copy)
	}
	s.store.Mu.RUnlock()
	sort.Slice(subscriptions, func(i, j int) bool {
		return subscriptions[i].SubscriptionID < subscriptions[j].SubscriptionID
	})
	total := len(subscriptions)
	page := paginateAndLink(w, r, subscriptions)
	rows := make([]map[string]interface{}, len(page))
	for i, subscription := range page {
		rows[i] = visualStudioSubscriptionListJSON(subscription)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count": total, "visual_studio_subscription_assignments": rows,
	})
}

func (s *Server) findVisualStudioUser(identifier string) *store.User {
	if user := s.store.LookupUserByLogin(identifier); user != nil {
		return user
	}
	for _, user := range s.store.ListUsers() {
		if user.Email != "" && strings.EqualFold(user.Email, identifier) {
			return user
		}
	}
	return nil
}

func (s *Server) handlePutVisualStudioSubscription(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("visual_studio_subscription_id")
	if _, err := uuid.Parse(id); err != nil {
		store.WriteGHValidationError(w, "VisualStudioSubscription", "visual_studio_subscription_id", "invalid")
		return
	}
	var req struct {
		UserIdentifier string `json:"user_identifier"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	user := s.findVisualStudioUser(req.UserIdentifier)
	if user == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	subscription := &store.VisualStudioSubscription{
		SubscriptionID: id, Email: user.Email, Username: user.Login, ManualMatch: true,
	}
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.VisualStudioSubscriptions[id] = subscription
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, visualStudioSubscriptionJSON(subscription))
}

func (s *Server) handleDeleteVisualStudioSubscription(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("visual_studio_subscription_id")
	s.store.Mu.Lock()
	subscription := s.store.EnterpriseSettings.VisualStudioSubscriptions[id]
	if subscription == nil {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	subscription.Username = ""
	subscription.ManualMatch = false
	s.store.PersistEnterpriseSettings()
	copy := *subscription
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, visualStudioSubscriptionJSON(&copy))
}

func (s *Server) handleSyncEnterpriseInnerSourceVulnerabilities(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Vulnerabilities []struct {
			ID        string           `json:"id"`
			Withdrawn *json.RawMessage `json:"withdrawn"`
		} `json:"vulnerabilities"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if len(req.Vulnerabilities) == 0 || len(req.Vulnerabilities) > 100 {
		store.WriteGHValidationError(w, "InnerSourceVulnerability", "vulnerabilities", "invalid")
		return
	}
	seen := map[string]bool{}
	now := s.store.CurrentTime()
	job := &store.EnterpriseInnerSourceSyncJob{
		ID: "external-vulnerability-sync-" + uuid.NewString(), Status: "queued",
		Results:   make([]store.EnterpriseInnerSourceSyncResult, 0, len(req.Vulnerabilities)),
		CreatedAt: now, UpdatedAt: now,
	}
	for i, vulnerability := range req.Vulnerabilities {
		if vulnerability.ID == "" || seen[vulnerability.ID] {
			store.WriteGHValidationError(w, "InnerSourceVulnerability", "id", "invalid")
			return
		}
		seen[vulnerability.ID] = true
		result := store.EnterpriseInnerSourceSyncResult{ExternalID: vulnerability.ID, Status: "created"}
		if vulnerability.Withdrawn != nil {
			result.Status = "withdrawn"
			job.Withdrawn++
		} else {
			job.Created++
			result.GHSAID = fmt.Sprintf("GHIS-0000-0000-%04d", i+1)
		}
		job.Results = append(job.Results, result)
	}
	job.Processed = len(job.Results)
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.InnerSourceSyncJobs[job.ID] = job
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	url := s.baseURL(r) + "/api/v3/enterprises/" + s.enterpriseSlug() +
		"/innersource-vulnerabilities/sync/status/" + job.ID
	w.Header().Set("Location", url)
	writeJSON(w, http.StatusAccepted, map[string]interface{}{"id": job.ID, "url": url, "status": "queued"})
}

func (s *Server) handleGetEnterpriseInnerSourceSync(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("job_id")
	s.store.Mu.Lock()
	job := s.store.EnterpriseSettings.InnerSourceSyncJobs[id]
	if job == nil {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if job.Status == "queued" {
		job.Status = "completed"
		job.UpdatedAt = s.store.CurrentTime()
		s.store.PersistEnterpriseSettings()
		s.store.Mu.Unlock()
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusAccepted, map[string]interface{}{"id": job.ID, "status": "queued"})
		return
	}
	copy := *job
	copy.Results = append([]store.EnterpriseInnerSourceSyncResult(nil), job.Results...)
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"processed": copy.Processed, "created": copy.Created, "updated": copy.Updated,
		"withdrawn": copy.Withdrawn, "errors": copy.Errors, "results": copy.Results,
	})
}

func (s *Server) handleEnterpriseServerStatistics(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	// Hide this instance-wide surface from non-admins, and from a site admin's
	// delegated credentials (fine-grained PAT, app or installation token) — matching
	// the sibling /enterprise/stats/* gate (requireGHESSiteAdmin).
	if user == nil || !user.SiteAdmin || !credentialConveysSiteAdmin(r.Context()) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	scope := r.PathValue("enterprise_or_org")
	if scope != s.enterpriseSlug() && s.store.GetOrg(scope) == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Mu.RLock()
	// total_teams counts organization teams only, matching the sibling admin-stats
	// endpoint; enterprise teams are a distinct concept and not part of this metric.
	totalRepos, totalTeams := len(s.store.Repos), len(s.store.Teams)
	totalOrgs := len(s.store.Orgs)
	users := make([]*store.User, 0, len(s.store.Users))
	for _, candidate := range s.store.Users {
		users = append(users, candidate)
	}
	s.store.Mu.RUnlock()
	admins, suspended := 0, 0
	for _, candidate := range users {
		if candidate.SiteAdmin {
			admins++
		}
		if candidate.Suspended {
			suspended++
		}
	}
	writeJSON(w, http.StatusOK, []map[string]interface{}{{
		"server_id": "bleephub", "collection_date": s.store.CurrentTime().Format(time.RFC3339),
		"schema_version": "20230306", "ghes_version": s.build.Version, "host_name": r.Host,
		"github_connect": map[string]interface{}{"features_enabled": []string{"license_usage_sync"}},
		"ghe_stats": map[string]interface{}{
			"orgs":  map[string]interface{}{"total_orgs": totalOrgs, "disabled_orgs": 0, "total_teams": totalTeams},
			"repos": map[string]interface{}{"total_repos": totalRepos},
			"users": map[string]interface{}{"total_users": len(users), "admin_users": admins, "suspended_users": suspended},
		},
	}})
}

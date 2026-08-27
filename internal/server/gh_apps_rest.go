package bleephub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/google/uuid"
)

func (s *Server) registerGHAppsRoutes() {
	s.registerGHAppSettingsRoutes()
	s.route("POST /api/v3/app-manifests/{code}/conversions", s.handleManifestConversion)
	s.route("GET /api/v3/app", s.handleGetAuthenticatedApp)
	s.route("GET /api/v3/apps/{app_slug}", s.handleGetAppBySlug)
	s.route("GET /api/v3/app/installations", s.handleListAppInstallations)
	s.route("GET /api/v3/app/installation-requests", s.handleListAppInstallationRequests)
	s.route("GET /api/v3/app/installations/{id}", s.handleGetAppInstallation)
	s.route("POST /api/v3/app/installations/{id}/access_tokens", s.handleCreateInstallationToken)
	s.route("DELETE /api/v3/app/installations/{id}", s.handleDeleteAppInstallation)
	s.route("PUT /api/v3/app/installations/{id}/suspended", s.handleSuspendInstallation)
	s.route("DELETE /api/v3/app/installations/{id}/suspended", s.handleUnsuspendInstallation)
	s.route("GET /api/v3/repos/{owner}/{repo}/installation", s.handleGetRepoInstallation)
	s.route("GET /api/v3/orgs/{org}/installation", s.handleGetOrgInstallation)
	s.route("GET /api/v3/orgs/{org}/installations", s.handleListOrgInstallations)
	s.route("GET /api/v3/users/{username}/installation", s.handleGetUserInstallation)

	s.route("POST /settings/apps/new", s.handleManifestSubmission)
	s.route("GET /settings/apps", s.handleListBrowserGitHubApps)
	s.route("POST /apps/{app_slug}/installations/new", s.handleBrowserInstallApp)
	s.route("POST /settings/apps/{app_slug}/installations/new", s.handleBrowserInstallApp)
	s.route("POST /settings/installations/{id}/suspend", s.handleBrowserSuspendInstallation)
	s.route("POST /settings/installations/{id}/unsuspend", s.handleBrowserUnsuspendInstallation)
	s.route("DELETE /settings/installations/{id}", s.handleBrowserDeleteInstallation)

	s.registerGHAppsUserAndOperatorRoutes()
}

// registerGHAppsUserAndOperatorRoutes mounts the authenticated-user
// installation views and the installation-token-scoped repository list.
func (s *Server) registerGHAppsUserAndOperatorRoutes() {
	s.route("GET /api/v3/user/installations", s.handleListUserInstallations)
	s.route("GET /api/v3/user/installations/{id}/repositories", s.handleListUserInstallationRepos)
	s.route("PUT /api/v3/user/installations/{id}/repositories/{repo_id}", s.handleAddUserInstallationRepo)
	s.route("DELETE /api/v3/user/installations/{id}/repositories/{repo_id}", s.handleRemoveUserInstallationRepo)
	s.route("DELETE /api/v3/installation/token", s.handleRevokeInstallationToken)

	s.route("GET /api/v3/installation/repositories", s.handleListInstallationRepositories)
}

// handleManifestSubmission is the browser half of the GitHub App Manifest flow:
// register the app from the posted `manifest` JSON and 302 to its redirect_url
// with a one-time `code` (echoing any `state`) that the conversion endpoint
// redeems for credentials.
func (s *Server) handleManifestSubmission(w http.ResponseWriter, r *http.Request) {
	r, user := s.authenticatedBrowserRequest(r)
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing form")
		return
	}
	var manifest struct {
		Name           string `json:"name"`
		URL            string `json:"url"`
		Description    string `json:"description"`
		RedirectURL    string `json:"redirect_url"`
		HookAttributes struct {
			URL    string `json:"url"`
			Active *bool  `json:"active"`
		} `json:"hook_attributes"`
		CallbackURLs       []string          `json:"callback_urls"`
		DefaultEvents      []string          `json:"default_events"`
		DefaultPermissions map[string]string `json:"default_permissions"`
	}
	raw := r.PostFormValue("manifest")
	if raw == "" {
		store.WriteGHValidationError(w, "AppManifest", "manifest", "missing_field")
		return
	}
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	if manifest.Name == "" {
		store.WriteGHValidationError(w, "AppManifest", "name", "missing_field")
		return
	}
	if manifest.URL == "" {
		store.WriteGHValidationError(w, "AppManifest", "url", "missing_field")
		return
	}
	if err := store.ValidateClientCallbackURL(strings.TrimSpace(manifest.URL)); err != nil {
		store.WriteGHValidationError(w, "AppManifest", "url", "invalid")
		return
	}
	if manifest.RedirectURL == "" {
		store.WriteGHValidationError(w, "AppManifest", "redirect_url", "missing_field")
		return
	}
	redirect, err := url.Parse(manifest.RedirectURL)
	if err != nil {
		store.WriteGHValidationError(w, "AppManifest", "redirect_url", "invalid")
		return
	}
	for scope, level := range manifest.DefaultPermissions {
		if !validPermLevelString(level) {
			store.WriteGHValidationError(w, "AppManifest", "default_permissions."+scope, "invalid")
			return
		}
	}
	// One callback per client (as OAuth Apps carry); a manifest offering
	// several is refused, not silently reduced to its first entry.
	callbackURL := ""
	if len(manifest.CallbackURLs) > 1 {
		store.WriteGHValidationError(w, "AppManifest", "callback_urls", "invalid")
		return
	}
	if len(manifest.CallbackURLs) == 1 {
		callbackURL = strings.TrimSpace(manifest.CallbackURLs[0])
		if err := store.ValidateClientCallbackURL(callbackURL); err != nil {
			store.WriteGHValidationError(w, "AppManifest", "callback_urls", "invalid")
			return
		}
	}

	app, err := s.store.CreateAppE(user.ID, manifest.Name, manifest.Description, manifest.DefaultPermissions, manifest.DefaultEvents)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.store.UpdateApp(app.ID, func(a *store.App) {
		if manifest.URL != "" {
			// The manifest's `url` is the app homepage (external_url).
			a.ExternalURL = manifest.URL
		}
		a.CallbackURL = callbackURL
		if manifest.HookAttributes.URL != "" {
			a.WebhookURL = manifest.HookAttributes.URL
			if manifest.HookAttributes.Active != nil {
				a.WebhookActive = *manifest.HookAttributes.Active
			}
			a.WebhookEvents = manifest.DefaultEvents
		}
	})

	q := redirect.Query()
	q.Set("code", s.store.RegisterManifestCode(app.ID))
	if state := r.URL.Query().Get("state"); state != "" {
		q.Set("state", state)
	}
	redirect.RawQuery = q.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

// handleListBrowserGitHubApps lists the signed-in user's GitHub Apps.
func (s *Server) handleListBrowserGitHubApps(w http.ResponseWriter, r *http.Request) {
	_, user := s.authenticatedBrowserRequest(r)
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	apps := s.snapshotGitHubApps()
	out := make([]map[string]interface{}, 0, len(apps))
	for _, app := range apps {
		if app.OwnerID == user.ID {
			out = append(out, appSettingsJSON(s.store, app, s.baseURL(r)))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleBrowserInstallApp is the browser "Install App" step. The app's
// registered default permissions/events form the grant; the form only chooses
// the target account and all-vs-selected repository access.
func (s *Server) handleBrowserInstallApp(w http.ResponseWriter, r *http.Request) {
	r, user := s.authenticatedBrowserRequest(r)
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	app := s.store.GetAppBySlug(r.PathValue("app_slug"))
	if app == nil {
		writeGHError(w, http.StatusNotFound, "App not found")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing form")
		return
	}
	targetLogin := r.PostFormValue("target_login")
	if targetLogin == "" {
		targetLogin = user.Login
	}
	targetType, targetID, ok := s.resolveInstallTarget(r.Context(), user, targetLogin)
	if !ok {
		writeGHError(w, http.StatusForbidden, "Must be able to install GitHub Apps on this account")
		return
	}
	for _, existing := range s.store.ListAppInstallations(app.ID) {
		if existing.TargetLogin == targetLogin {
			store.WriteGHValidationError(w, "Installation", "target_login", "already_exists")
			return
		}
	}
	selection := r.PostFormValue("repository_selection")
	if selection == "" {
		selection = "all"
	}
	if selection != "all" && selection != "selected" {
		store.WriteGHValidationError(w, "Installation", "repository_selection", "invalid")
		return
	}
	repoIDs, valid := s.resolveInstallationRepositorySelection(targetLogin, selection, r.PostForm["repository_ids"])
	if !valid {
		store.WriteGHValidationError(w, "Installation", "repository_ids", "invalid")
		return
	}

	inst := s.store.CreateInstallation(app.ID, targetType, targetID, targetLogin, copyInstallationPermissions(app.Permissions), append([]string(nil), app.Events...))
	if inst == nil {
		writeGHError(w, http.StatusNotFound, "App not found")
		return
	}
	if selection == "selected" {
		s.store.SetInstallationRepositorySelection(inst.ID, "selected", repoIDs)
		inst = s.store.GetInstallation(inst.ID)
	}
	s.emitInstallationEvent(app, "created", inst)
	writeJSON(w, http.StatusCreated, installationToJSON(inst, s.baseURL(r)))
}

func (s *Server) handleBrowserSuspendInstallation(w http.ResponseWriter, r *http.Request) {
	s.handleBrowserInstallationState(w, r, true)
}

func (s *Server) handleBrowserUnsuspendInstallation(w http.ResponseWriter, r *http.Request) {
	s.handleBrowserInstallationState(w, r, false)
}

func (s *Server) handleBrowserInstallationState(w http.ResponseWriter, r *http.Request, suspend bool) {
	r, user := s.authenticatedBrowserRequest(r)
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	inst, ok := s.browserManageableInstallation(w, r, user)
	if !ok {
		return
	}
	var changed bool
	if suspend {
		changed = s.store.SuspendInstallation(inst.ID, user)
	} else {
		changed = s.store.UnsuspendInstallation(inst.ID)
	}
	if !changed {
		writeGHError(w, http.StatusConflict, "Installation state already matches request")
		return
	}
	if app := s.store.GetApp(inst.AppID); app != nil {
		action := "unsuspend"
		if suspend {
			action = "suspend"
		}
		s.emitInstallationEvent(app, action, s.store.GetInstallation(inst.ID))
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBrowserDeleteInstallation(w http.ResponseWriter, r *http.Request) {
	r, user := s.authenticatedBrowserRequest(r)
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	inst, ok := s.browserManageableInstallation(w, r, user)
	if !ok {
		return
	}
	s.store.DeleteInstallation(inst.ID)
	if app := s.store.GetApp(inst.AppID); app != nil {
		s.emitInstallationEvent(app, "deleted", inst)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) browserManageableInstallation(w http.ResponseWriter, r *http.Request, user *store.User) (*store.Installation, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "Invalid installation ID")
		return nil, false
	}
	inst := s.store.GetInstallation(id)
	if inst == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil, false
	}
	if inst.TargetLogin == user.Login {
		return inst, true
	}
	if inst.TargetType == "Organization" && s.viewerCanAdminOrg(r.Context(), inst.TargetLogin) {
		return inst, true
	}
	writeGHError(w, http.StatusForbidden, "Must be able to manage GitHub Apps on this account")
	return nil, false
}

func (s *Server) resolveInstallTarget(ctx context.Context, user *store.User, targetLogin string) (string, int, bool) {
	// Compare identities, not raw strings: "ADMIN" names the "admin" account.
	if u := s.store.LookupUserByLogin(targetLogin); u != nil && u.ID == user.ID {
		return "User", user.ID, true
	}
	if org := s.store.GetOrg(targetLogin); org != nil {
		return "Organization", org.ID, s.viewerCanAdminOrg(ctx, targetLogin)
	}
	return "", 0, false
}

func (s *Server) resolveInstallationRepositorySelection(targetLogin, selection string, rawIDs []string) ([]int, bool) {
	if selection == "all" {
		return nil, true
	}
	if len(rawIDs) == 0 {
		return nil, false
	}
	repoByID := map[int]bool{}
	for _, repo := range s.store.ListReposByOwner(targetLogin) {
		repoByID[repo.ID] = true
	}
	ids := make([]int, 0, len(rawIDs))
	seen := map[int]bool{}
	for _, raw := range rawIDs {
		id, err := strconv.Atoi(raw)
		if err != nil || !repoByID[id] {
			return nil, false
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, true
}

func copyInstallationPermissions(perms map[string]string) map[string]string {
	return store.NormalizeAppPermissions(perms)
}

func (s *Server) handleManifestConversion(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	appID, ok := s.store.ConsumeManifestCode(code)
	if !ok {
		writeGHError(w, http.StatusNotFound, "Manifest code not found or already used")
		return
	}
	app := s.store.GetApp(appID)
	if app == nil {
		writeGHError(w, http.StatusNotFound, "App not found")
		return
	}
	writeJSON(w, http.StatusCreated, appToJSON(s.store, app, true, s.baseURL(r)))
}

func (s *Server) handleGetAuthenticatedApp(w http.ResponseWriter, r *http.Request) {
	app := ghAppFromContext(r.Context())
	if app == nil {
		writeGHError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	writeJSON(w, http.StatusOK, appToJSON(s.store, app, false, s.baseURL(r)))
}

func (s *Server) handleListAppInstallations(w http.ResponseWriter, r *http.Request) {
	app := ghAppFromContext(r.Context())
	if app == nil {
		writeGHError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	installations := s.store.ListAppInstallations(app.ID)
	result := make([]map[string]interface{}, 0, len(installations))
	for _, inst := range installations {
		result = append(result, installationToJSON(inst, s.baseURL(r)))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, result))
}

func (s *Server) handleGetAppInstallation(w http.ResponseWriter, r *http.Request) {
	app := ghAppFromContext(r.Context())
	if app == nil {
		writeGHError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "Invalid installation ID")
		return
	}
	inst := s.store.GetInstallation(id)
	if inst == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if inst.AppID != app.ID {
		writeGHError(w, http.StatusForbidden, "Installation does not belong to this app")
		return
	}
	writeJSON(w, http.StatusOK, installationToJSON(inst, s.baseURL(r)))
}

func (s *Server) handleCreateInstallationToken(w http.ResponseWriter, r *http.Request) {
	app := ghAppFromContext(r.Context())
	if app == nil {
		writeGHError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "Invalid installation ID")
		return
	}
	inst := s.store.GetInstallation(id)
	if inst == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if inst.AppID != app.ID {
		writeGHError(w, http.StatusForbidden, "Installation does not belong to this app")
		return
	}

	perms := inst.Permissions
	var repoIDs []int
	var body struct {
		Permissions   map[string]string `json:"permissions"`
		RepositoryIDs flexIntSlice      `json:"repository_ids"`
		Repositories  []string          `json:"repositories"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
			writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
	}

	if inst.SuspendedAt != nil {
		writeGHError(w, http.StatusForbidden, "This installation has been suspended.")
		return
	}

	// Requested permissions must be a subset of the installation's grants;
	// escalation is a 422.
	if body.Permissions != nil {
		if scope, ok := validateRequestedPermissions(body.Permissions, inst.Permissions); !ok {
			writeGHError(w, http.StatusUnprocessableEntity,
				"The permissions requested are not granted to this installation (permission: "+scope+")")
			return
		}
		perms = body.Permissions
	}

	// Every requested repo must exist under the target and be accessible to
	// the installation; unknown/inaccessible repos are a 422.
	accessible := installationAccessibleRepoIDs(s.store, inst)
	for _, rid := range body.RepositoryIDs {
		if _, ok := accessible[rid]; !ok {
			writeGHError(w, http.StatusUnprocessableEntity,
				"There is at least one repository that does not exist or is not accessible to the integration")
			return
		}
		repoIDs = append(repoIDs, rid)
	}
	if len(repoIDs) == 0 {
		for _, name := range body.Repositories {
			repo := s.store.GetRepo(inst.TargetLogin, name)
			if repo == nil {
				writeGHError(w, http.StatusUnprocessableEntity,
					"There is at least one repository that does not exist or is not accessible to the integration")
				return
			}
			if _, ok := accessible[repo.ID]; !ok {
				writeGHError(w, http.StatusUnprocessableEntity,
					"There is at least one repository that does not exist or is not accessible to the integration")
				return
			}
			repoIDs = append(repoIDs, repo.ID)
		}
	}

	token, err := s.store.CreateInstallationTokenE(inst.ID, app.ID, perms, repoIDs)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// A token minted with a repository subset returns
	// repository_selection="selected" and a `repositories` array; resolve its
	// repo IDs against the installation's owned repos.
	var scopedRepos []*store.Repo
	if len(token.RepositoryIDs) > 0 {
		owned := s.store.ListReposByOwner(inst.TargetLogin)
		byID := make(map[int]*store.Repo, len(owned))
		for _, repo := range owned {
			byID[repo.ID] = repo
		}
		for _, rid := range token.RepositoryIDs {
			if repo := byID[rid]; repo != nil {
				scopedRepos = append(scopedRepos, repo)
			}
		}
	}
	writeJSON(w, http.StatusCreated, installationTokenToJSON(token, inst, scopedRepos, s.store, s.baseURL(r)))
}

func (s *Server) handleDeleteAppInstallation(w http.ResponseWriter, r *http.Request) {
	app := ghAppFromContext(r.Context())
	if app == nil {
		writeGHError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "Invalid installation ID")
		return
	}
	inst := s.store.GetInstallation(id)
	if inst == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if inst.AppID != app.ID {
		writeGHError(w, http.StatusForbidden, "Installation does not belong to this app")
		return
	}
	s.store.DeleteInstallation(id)
	s.emitInstallationEvent(app, "deleted", inst)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetRepoInstallation(w http.ResponseWriter, r *http.Request) {
	app := ghAppFromContext(r.Context())
	if app == nil {
		writeGHError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	owner := r.PathValue("owner")
	repo := s.store.GetRepo(owner, r.PathValue("repo"))
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	for _, inst := range s.snapshotInstallations() {
		if inst.AppID != app.ID ||
			!strings.EqualFold(inst.TargetLogin, owner) ||
			!strings.EqualFold(inst.TargetType, repo.OwnerType) {
			continue
		}
		// A "selected"-mode installation 404s for repos outside its allow-list.
		if _, ok := installationAccessibleRepoIDs(s.store, inst)[repo.ID]; ok {
			writeJSON(w, http.StatusOK, installationToJSON(inst, s.baseURL(r)))
			return
		}
	}
	writeGHError(w, http.StatusNotFound, "Not Found")
}

// JSON serializers

func appToJSON(st *store.Store, app *store.App, includePEM bool, baseURL string) map[string]interface{} {
	result := map[string]interface{}{
		"id":                  app.ID,
		"node_id":             app.NodeID,
		"slug":                app.Slug,
		"name":                app.Name,
		"client_id":           app.ClientID,
		"description":         app.Description,
		"external_url":        app.ExternalURL,
		"html_url":            "https://github.com/apps/" + app.Slug,
		"permissions":         app.Permissions,
		"events":              jsonArray(app.Events),
		"installations_count": st.CountAppInstallations(app.ID),
		"created_at":          app.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":          app.UpdatedAt.UTC().Format(time.RFC3339),
		"owner":               appOwnerJSON(st, app, baseURL),
	}
	if includePEM {
		result["pem"] = app.PEMPrivateKey
		result["client_secret"] = app.ClientSecret
		result["webhook_secret"] = app.WebhookSecret
	}
	return result
}

// appOwnerJSON serializes the app's owning account as a Simple User. OwnerID is
// validated at load and creation, so a missing owner is corrupt state and
// serializes as null rather than a fabricated user.
func appOwnerJSON(st *store.Store, app *store.App, baseURL string) map[string]interface{} {
	st.Mu.RLock()
	owner := st.Users[app.OwnerID]
	st.Mu.RUnlock()
	if owner == nil {
		return nil
	}
	return store.UserToJSON(owner, baseURL)
}

// appPermissionScopesWithAdminLevel are the app-permissions members whose
// documented enum admits "admin" (["read","write","admin"]); all other scopes
// stop at ["read","write"].
var appPermissionScopesWithAdminLevel = map[string]bool{
	"repository_projects":                            true,
	"organization_projects":                          true,
	"organization_custom_properties":                 true,
	"enterprise_custom_properties_for_organizations": true,
}

// appPermissionsJSON renders a stored permission map on the wire. bleephub's
// PermAdmin sits above write; for scopes whose enum lacks "admin" it serializes
// as "write". The stored map is untouched — only the representation narrows.
func appPermissionsJSON(perms map[string]string) map[string]string {
	if perms == nil {
		return nil
	}
	out := make(map[string]string, len(perms))
	for scope, level := range perms {
		if level == "admin" && !appPermissionScopesWithAdminLevel[scope] {
			level = "write"
		}
		out[scope] = level
	}
	return out
}

func installationToJSON(inst *store.Installation, baseURL string) map[string]interface{} {
	if inst == nil {
		return nil
	}
	// The account rides as a simple-user regardless of target type
	// (Organization targets use the same shape with type "Organization").
	// Node ID and avatar were snapshotted at installation time.
	accountAPI := baseURL + "/api/v3/users/" + inst.TargetLogin
	account := map[string]interface{}{
		"login":               inst.TargetLogin,
		"id":                  inst.TargetID,
		"node_id":             inst.TargetNodeID,
		"avatar_url":          store.AvatarURLFor(inst.TargetAvatarURL, inst.TargetID, baseURL),
		"gravatar_id":         "",
		"url":                 accountAPI,
		"html_url":            baseURL + "/" + inst.TargetLogin,
		"followers_url":       accountAPI + "/followers",
		"following_url":       accountAPI + "/following{/other_user}",
		"gists_url":           accountAPI + "/gists{/gist_id}",
		"starred_url":         accountAPI + "/starred{/owner}{/repo}",
		"subscriptions_url":   accountAPI + "/subscriptions",
		"organizations_url":   accountAPI + "/orgs",
		"repos_url":           accountAPI + "/repos",
		"events_url":          accountAPI + "/events{/privacy}",
		"received_events_url": accountAPI + "/received_events",
		"type":                inst.TargetType,
		"site_admin":          false,
		"user_view_type":      "public",
	}
	out := map[string]interface{}{
		"id":                        inst.ID,
		"app_id":                    inst.AppID,
		"app_slug":                  inst.AppSlug,
		"target_type":               inst.TargetType,
		"target_id":                 inst.TargetID,
		"permissions":               appPermissionsJSON(inst.Permissions),
		"events":                    jsonArray(inst.Events),
		"repository_selection":      inst.RepositorySelection,
		"single_file_name":          inst.SingleFileName,
		"has_multiple_single_files": false,
		"single_file_paths":         []string{},
		"created_at":                inst.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":                inst.UpdatedAt.UTC().Format(time.RFC3339),
		"account":                   account,
		"html_url":                  "/apps/" + inst.AppSlug + "/installations/" + strconv.Itoa(inst.ID),
		"access_tokens_url":         "/api/v3/app/installations/" + strconv.Itoa(inst.ID) + "/access_tokens",
		"repositories_url":          "/api/v3/installation/repositories",
		"suspended_at":              nil,
		"suspended_by":              nil,
	}
	if inst.SuspendedAt != nil {
		out["suspended_at"] = inst.SuspendedAt.UTC().Format(time.RFC3339)
		if inst.SuspendedBy != nil {
			out["suspended_by"] = store.UserToJSON(inst.SuspendedBy, baseURL)
		}
	}
	return out
}

func installationTokenToJSON(token *store.InstallationToken, inst *store.Installation, scopedRepos []*store.Repo, st *store.Store, baseURL string) map[string]interface{} {
	// "selected" when minted with a repository subset, otherwise the
	// installation's own selection.
	selection := ""
	if inst != nil {
		selection = inst.RepositorySelection
	}
	if len(token.RepositoryIDs) > 0 {
		selection = "selected"
	}
	out := map[string]interface{}{
		"token":                token.Token,
		"expires_at":           token.ExpiresAt.UTC().Format(time.RFC3339),
		"permissions":          appPermissionsJSON(token.Permissions),
		"repository_selection": selection,
	}
	if len(token.RepositoryIDs) > 0 {
		repoJSON := make([]map[string]interface{}, 0, len(scopedRepos))
		for _, repo := range scopedRepos {
			repoJSON = append(repoJSON, store.RepoToJSON(repo, st, baseURL))
		}
		out["repositories"] = repoJSON
	}
	return out
}

// handleListUserInstallations lists installations on the user's own account
// plus those on organizations where the user is an active member.
func (s *Server) handleListUserInstallations(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	var all []*store.Installation
	for _, inst := range s.snapshotInstallations() {
		if inst.TargetLogin == user.Login {
			all = append(all, inst)
			continue
		}
		if inst.TargetType == "Organization" && s.viewerIsOrgMember(r.Context(), inst.TargetLogin) {
			all = append(all, inst)
		}
	}

	page := paginateAndLink(w, r, all)
	installations := make([]map[string]interface{}, 0, len(page))
	for _, inst := range page {
		installations = append(installations, installationToJSON(inst, s.baseURL(r)))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count":   len(all),
		"installations": installations,
	})
}

// handleListUserInstallationRepos returns the repos accessible via this
// installation.
func (s *Server) handleListUserInstallationRepos(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "Invalid installation ID")
		return
	}
	inst, ok := s.userAccessibleInstallation(r.Context(), user, id, false)
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	repos := filterInstallationRepos(s.store.ListReposByOwner(inst.TargetLogin), inst)
	page := paginateAndLink(w, r, repos)
	base := s.baseURL(r)
	repoJSON := make([]map[string]interface{}, 0, len(page))
	for _, repo := range page {
		repoJSON = append(repoJSON, store.RepoToJSON(repo, s.store, base))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count":          len(repos),
		"repository_selection": inst.RepositorySelection,
		"repositories":         repoJSON,
	})
}

// userAccessibleInstallation binds a /user/installations/{id} request to the
// authenticated human and, for user-to-server credentials, to the app and
// installation subset that credential represents. Reads accept active org
// members; mutations (requireAdmin) require an org owner; a user-account
// installation is reachable only by that account's user.
func (s *Server) userAccessibleInstallation(ctx context.Context, user *store.User, id int, requireAdmin bool) (*store.Installation, bool) {
	inst := s.store.GetInstallation(id)
	if inst == nil || user == nil {
		return nil, false
	}
	if token := ghUserToServerTokenFromContext(ctx); token != nil && token.AppID > 0 {
		if token.AppID != inst.AppID {
			return nil, false
		}
		if len(token.InstallationIDs) > 0 && !slices.Contains(token.InstallationIDs, inst.ID) {
			return nil, false
		}
	}
	switch inst.TargetType {
	case "User":
		if inst.TargetID != user.ID || !strings.EqualFold(inst.TargetLogin, user.Login) {
			return nil, false
		}
	case "Organization":
		if requireAdmin {
			if !s.viewerCanAdminOrg(ctx, inst.TargetLogin) {
				return nil, false
			}
		} else if !s.viewerIsOrgMember(ctx, inst.TargetLogin) {
			return nil, false
		}
	default:
		return nil, false
	}
	return inst, true
}

// handleGetAppBySlug is the anonymous-readable public app lookup: public fields
// only (no PEM, no client_secret), 404 on an unknown slug.
func (s *Server) handleGetAppBySlug(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("app_slug")
	app := s.store.GetAppBySlug(slug)
	if app == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, appToJSON(s.store, app, false, s.baseURL(r)))
}

// handleSuspendInstallation suspends an installation: 204 on success, 409 if
// already suspended.
func (s *Server) handleSuspendInstallation(w http.ResponseWriter, r *http.Request) {
	app := ghAppFromContext(r.Context())
	if app == nil {
		writeGHError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "Invalid installation ID")
		return
	}
	inst := s.store.GetInstallation(id)
	if inst == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if inst.AppID != app.ID {
		writeGHError(w, http.StatusForbidden, "Installation does not belong to this app")
		return
	}
	if !s.store.SuspendInstallation(id, &store.User{Login: app.Slug + "[bot]", Type: "Bot", ID: -app.ID}) {
		writeGHError(w, http.StatusConflict, "Installation already suspended")
		return
	}
	s.emitInstallationEvent(app, "suspend", inst)
	w.WriteHeader(http.StatusNoContent)
}

// handleUnsuspendInstallation unsuspends an installation: 204 on success, 409
// if not suspended.
func (s *Server) handleUnsuspendInstallation(w http.ResponseWriter, r *http.Request) {
	app := ghAppFromContext(r.Context())
	if app == nil {
		writeGHError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "Invalid installation ID")
		return
	}
	inst := s.store.GetInstallation(id)
	if inst == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if inst.AppID != app.ID {
		writeGHError(w, http.StatusForbidden, "Installation does not belong to this app")
		return
	}
	if !s.store.UnsuspendInstallation(id) {
		writeGHError(w, http.StatusConflict, "Installation not suspended")
		return
	}
	s.emitInstallationEvent(app, "unsuspend", inst)
	w.WriteHeader(http.StatusNoContent)
}

// findAppInstallationByTarget returns the authenticated app's installation
// matching targetLogin + targetType, or writes 404 and returns false.
func (s *Server) findAppInstallationByTarget(w http.ResponseWriter, appID int, targetLogin, targetType string) bool {
	for _, inst := range s.snapshotInstallations() {
		if inst.AppID == appID &&
			strings.EqualFold(inst.TargetLogin, targetLogin) &&
			strings.EqualFold(inst.TargetType, targetType) {
			writeJSON(w, http.StatusOK, installationToJSON(inst, s.publicOrigin()))
			return true
		}
	}
	writeGHError(w, http.StatusNotFound, "Not Found")
	return false
}

// handleListOrgInstallations lists the app installations on an organization,
// gated on active org admin membership.
func (s *Server) handleListOrgInstallations(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	org := s.store.GetOrg(r.PathValue("org"))
	if org == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.viewerCanAdminOrg(r.Context(), org.Login) {
		writeGHError(w, http.StatusForbidden, "Must be an organization owner.")
		return
	}
	var all []*store.Installation
	for _, inst := range s.snapshotInstallations() {
		if inst.TargetType == "Organization" && inst.TargetLogin == org.Login {
			all = append(all, inst)
		}
	}
	page := paginateAndLink(w, r, all)
	installations := make([]map[string]interface{}, 0, len(page))
	for _, inst := range page {
		installations = append(installations, installationToJSON(inst, s.baseURL(r)))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count":   len(all),
		"installations": installations,
	})
}

func (s *Server) handleGetOrgInstallation(w http.ResponseWriter, r *http.Request) {
	app := ghAppFromContext(r.Context())
	if app == nil {
		writeGHError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	s.findAppInstallationByTarget(w, app.ID, r.PathValue("org"), "Organization")
}

func (s *Server) handleGetUserInstallation(w http.ResponseWriter, r *http.Request) {
	app := ghAppFromContext(r.Context())
	if app == nil {
		writeGHError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	s.findAppInstallationByTarget(w, app.ID, r.PathValue("username"), "User")
}

// handleAddUserInstallationRepo adds a repository owned by the installation
// target to a "selected"-mode installation's allow-list.
func (s *Server) handleAddUserInstallationRepo(w http.ResponseWriter, r *http.Request) {
	s.handleUserInstallationRepoMutation(w, r, true)
}

func (s *Server) handleRemoveUserInstallationRepo(w http.ResponseWriter, r *http.Request) {
	s.handleUserInstallationRepoMutation(w, r, false)
}

// handleUserInstallationRepoMutation applies the common authorization and
// ownership contract for adding or removing a selected installation repo.
func (s *Server) handleUserInstallationRepoMutation(w http.ResponseWriter, r *http.Request, add bool) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	instID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "Invalid installation ID")
		return
	}
	repoID, err := strconv.Atoi(r.PathValue("repo_id"))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "Invalid repository ID")
		return
	}
	inst, allowed := s.userAccessibleInstallation(r.Context(), user, instID, true)
	if !allowed {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if inst.RepositorySelection != "selected" {
		store.WriteGHValidationError(w, "Installation", "repository_selection", "not_selected")
		return
	}
	repo := s.store.GetRepoByID(repoID)
	if !store.InstallationOwnsRepo(inst, repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var changed, ok bool
	eventAction := "added"
	if add {
		changed, ok = s.store.AddInstallationRepo(instID, repoID)
	} else {
		changed, ok = s.store.RemoveInstallationRepo(instID, repoID)
		eventAction = "removed"
	}
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if current := s.store.GetInstallation(instID); current != nil && changed {
		if app := s.store.GetApp(current.AppID); app != nil {
			s.emitInstallationRepositoriesEvent(app, eventAction, current, []int{repoID})
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListInstallationRepositories is the ghs_-token-scoped repo list: the
// repos the installation reaches, narrowed to the token's repository_ids subset
// when it has one.
func (s *Server) handleListInstallationRepositories(w http.ResponseWriter, r *http.Request) {
	tok := ghInstallationTokenFromContext(r.Context())
	inst := ghInstallationFromContext(r.Context())
	if tok == nil || inst == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	allRepos := s.store.ListReposByOwner(inst.TargetLogin)
	filtered := filterReposBySelection(allRepos, inst, tok)
	page := paginateAndLink(w, r, filtered)
	base := s.baseURL(r)
	repoJSON := make([]map[string]interface{}, 0, len(page))
	for _, repo := range page {
		repoJSON = append(repoJSON, store.RepoToJSON(repo, s.store, base))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count":          len(filtered),
		"repository_selection": inst.RepositorySelection,
		"repositories":         repoJSON,
	})
}

// snapshotInstallations copies every installation under one RLock so handlers
// iterate without holding the store lock.
func (s *Server) snapshotInstallations() []*store.Installation {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	out := make([]*store.Installation, 0, len(s.store.Installations))
	for _, inst := range s.store.Installations {
		out = append(out, inst)
	}
	return out
}

func (s *Server) snapshotGitHubApps() []*store.App {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	out := make([]*store.App, 0, len(s.store.Apps))
	for _, app := range s.store.Apps {
		out = append(out, app)
	}
	return out
}

// installationAccessibleRepoIDs is the target's owned repos, narrowed to
// SelectedRepoIDs in "selected" mode.
func installationAccessibleRepoIDs(st *store.Store, inst *store.Installation) map[int]struct{} {
	owned := st.ListReposByOwner(inst.TargetLogin)
	out := make(map[int]struct{}, len(owned))
	if inst.RepositorySelection == "selected" {
		ownedSet := make(map[int]struct{}, len(owned))
		for _, repo := range owned {
			ownedSet[repo.ID] = struct{}{}
		}
		for _, id := range inst.SelectedRepoIDs {
			if _, ok := ownedSet[id]; ok {
				out[id] = struct{}{}
			}
		}
		return out
	}
	for _, repo := range owned {
		out[repo.ID] = struct{}{}
	}
	return out
}

// filterReposBySelection applies the installation's repository_selection mode
// then the token-scoped repository_ids subset.
func filterReposBySelection(all []*store.Repo, inst *store.Installation, tok *store.InstallationToken) []*store.Repo {
	out := filterInstallationRepos(all, inst)
	if len(tok.RepositoryIDs) == 0 {
		return out
	}
	allowed := make(map[int]struct{}, len(tok.RepositoryIDs))
	for _, id := range tok.RepositoryIDs {
		allowed[id] = struct{}{}
	}
	filtered := out[:0]
	for _, repo := range out {
		if _, ok := allowed[repo.ID]; ok {
			filtered = append(filtered, repo)
		}
	}
	return filtered
}

func filterInstallationRepos(all []*store.Repo, inst *store.Installation) []*store.Repo {
	allowed := map[int]struct{}{}
	if inst.RepositorySelection == "selected" {
		for _, id := range inst.SelectedRepoIDs {
			allowed[id] = struct{}{}
		}
	} else {
		for _, r := range all {
			allowed[r.ID] = struct{}{}
		}
	}
	out := make([]*store.Repo, 0, len(all))
	for _, r := range all {
		if _, ok := allowed[r.ID]; ok {
			out = append(out, r)
		}
	}
	return out
}

// emitInstallationEvent fires an `installation` webhook (created | deleted |
// suspend | unsuspend | new_permissions_accepted) to the app's webhook URL.
func (s *Server) emitInstallationEvent(app *store.App, action string, inst *store.Installation) {
	if app == nil || app.WebhookURL == "" || !app.WebhookActive {
		return
	}
	sender := s.store.LookupUserByLogin(inst.TargetLogin)
	payload := buildInstallationEventPayload(app, action, inst, sender, s.publicOrigin())
	s.enqueueWebhookJob(appWebhookQueueKey(app), func() {
		s.deliverAppWebhook(app, "installation", action, inst.ID, mustMarshal(payload))
	})
}

// emitInstallationRepositoriesEvent fires an `installation_repositories`
// webhook (added | removed).
func (s *Server) emitInstallationRepositoriesEvent(app *store.App, action string, inst *store.Installation, repoIDsChanged []int) {
	if app == nil || app.WebhookURL == "" || !app.WebhookActive {
		return
	}
	sender := s.store.LookupUserByLogin(inst.TargetLogin)
	payload := buildInstallationRepositoriesEventPayload(app, action, inst, repoIDsChanged, sender, s.publicOrigin())
	s.enqueueWebhookJob(appWebhookQueueKey(app), func() {
		s.deliverAppWebhook(app, "installation_repositories", action, inst.ID, mustMarshal(payload))
	})
}

// deliverAppWebhook is the app-level deliverWebhook: same retry shape, records
// to AppHookDeliveries.
func (s *Server) deliverAppWebhook(app *store.App, event, action string, installationID int, payloadBytes []byte) {
	hook := appWebhookPseudoHook(app)
	guid := uuid.New().String()
	backoffs := []time.Duration{0, 1 * time.Second, 5 * time.Second}
	for attempt, backoff := range backoffs {
		if attempt > 0 {
			time.Sleep(backoff)
		}
		delivery := s.doDeliverAttempt(hook, event, action, guid, payloadBytes, attempt > 0)
		delivery.AppID = app.ID
		delivery.InstallationID = installationID
		s.store.AddAppDelivery(app.ID, delivery)
		if delivery.StatusCode >= 200 && delivery.StatusCode < 300 {
			return
		}
	}
}

// handleRevokeInstallationToken revokes the ghs_* installation token in the
// request's Authorization header, dropping it from the InstallationTokens map.
func (s *Server) handleRevokeInstallationToken(w http.ResponseWriter, r *http.Request) {
	scheme, cred := authScheme(r.Header.Get("Authorization"))
	tokenStr := ""
	if scheme == "token" || scheme == "bearer" {
		tokenStr = cred
	}
	if !strings.HasPrefix(tokenStr, store.TokenPrefixInstallation) {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	if !s.store.RevokeInstallationToken(tokenStr) {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListAppInstallationRequests always returns empty: installation requests
// arise only when a non-admin asks an owner to install, and bleephub
// installations are created directly by their owners.
func (s *Server) handleListAppInstallationRequests(w http.ResponseWriter, r *http.Request) {
	app := ghAppFromContext(r.Context())
	if app == nil {
		writeGHError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, []map[string]interface{}{}))
}

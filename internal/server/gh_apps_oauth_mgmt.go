package bleephub

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// OAuth applications token management endpoints.
// Real GitHub exposes a parallel surface for OAuth Apps and GitHub Apps acting
// as OAuth clients. Authentication is HTTP Basic with client_id:client_secret.
// `client_id` resolves to either an OAuth App or a GitHub App (looked up in
// Store.OAuthApps then Store.AppsByClientID).
//
// Endpoints implemented:
//   POST   /applications/{client_id}/token         check token validity
//   PATCH  /applications/{client_id}/token         reset (rotate) token
//   DELETE /applications/{client_id}/token         revoke token
//   POST   /applications/{client_id}/token/scoped  scope user-to-server token
//   DELETE /applications/{client_id}/grant         revoke user grant

func (s *Server) registerGHAppsOAuthMgmtRoutes() {
	s.route("POST /api/v3/applications/{client_id}/token", s.handleCheckOAuthToken)
	s.route("PATCH /api/v3/applications/{client_id}/token", s.handleResetOAuthToken)
	s.route("DELETE /api/v3/applications/{client_id}/{operation}", s.handleDeleteApplicationDispatch)
	s.route("POST /api/v3/applications/{client_id}/token/scoped", s.handleScopeOAuthToken)
	s.route("GET /api/v3/applications/grants", s.handleListLegacyOAuthGrants)
	s.route("GET /api/v3/applications/grants/{grant_id}", s.handleGetLegacyOAuthGrant)
	s.route("GET /api/v3/authorizations", s.handleListLegacyAuthorizations)
	s.route("POST /api/v3/authorizations", s.handleCreateLegacyAuthorization)
	s.route("GET /api/v3/authorizations/{authorization_id}", s.handleGetLegacyAuthorization)
	s.route("PATCH /api/v3/authorizations/{authorization_id}", s.handleUpdateLegacyAuthorization)
	s.route("DELETE /api/v3/authorizations/{authorization_id}", s.handleDeleteLegacyAuthorization)
	s.route("PUT /api/v3/authorizations/clients/{client_id}", s.handleGetOrCreateLegacyAuthorization)
	s.route("PUT /api/v3/authorizations/clients/{client_id}/{fingerprint}", s.handleGetOrCreateLegacyAuthorization)
	s.route("GET /settings/oauth-apps", s.handleListBrowserOAuthApps)
	s.route("POST /settings/oauth-apps/new", s.handleCreateBrowserOAuthApp)

}

func (s *Server) handleDeleteApplicationDispatch(w http.ResponseWriter, r *http.Request) {
	clientID, operation := r.PathValue("client_id"), r.PathValue("operation")
	switch {
	case clientID == "grants":
		r.SetPathValue("grant_id", operation)
		s.handleDeleteLegacyOAuthGrant(w, r)
	case operation == "token":
		s.handleRevokeOAuthToken(w, r)
	case operation == "grant":
		s.handleRevokeOAuthGrant(w, r)
	default:
		writeGHError(w, http.StatusNotFound, "Not Found")
	}
}

// authenticateClientCreds reads + verifies HTTP Basic auth carrying
// client_id:client_secret against either OAuthApps or AppsByClientID.
// On match returns (clientID, isOAuthApp). On miss writes 401 + returns ("", false).
func (s *Server) authenticateClientCreds(w http.ResponseWriter, r *http.Request, pathClientID string) (string, bool) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Basic ") {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	if err != nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return "", false
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return "", false
	}
	cid, secret := parts[0], parts[1]
	if cid != pathClientID {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return "", false
	}
	if oa := s.store.VerifyOAuthAppSecret(cid, secret); oa != nil {
		return cid, true
	}
	if a := s.store.VerifyAppClientSecret(cid, secret); a != nil {
		return cid, false
	}
	writeGHError(w, http.StatusUnauthorized, "Bad credentials")
	return "", false
}

func (s *Server) handleCheckOAuthToken(w http.ResponseWriter, r *http.Request) {
	clientID, _ := s.authenticateClientCreds(w, r, r.PathValue("client_id"))
	if clientID == "" {
		return
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.AccessToken == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "access_token required")
		return
	}
	tok, user := s.store.LookupUserToServerToken(body.AccessToken)
	if tok == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// Token must belong to this client_id, either as an OAuth App token or as a GitHub App OAuth client token.
	if !tokenMatchesClient(tok, clientID, s.store) {
		writeGHError(w, http.StatusUnprocessableEntity, "token does not match client_id")
		return
	}
	writeJSON(w, http.StatusOK, oauthTokenInspectionJSON(s.store, tok, user))
}

func (s *Server) handleResetOAuthToken(w http.ResponseWriter, r *http.Request) {
	clientID, _ := s.authenticateClientCreds(w, r, r.PathValue("client_id"))
	if clientID == "" {
		return
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.AccessToken == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "access_token required")
		return
	}
	tok, _ := s.store.LookupUserToServerToken(body.AccessToken)
	if tok == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !tokenMatchesClient(tok, clientID, s.store) {
		writeGHError(w, http.StatusUnprocessableEntity, "token does not match client_id")
		return
	}
	// Mint the replacement before revoking the old token so an entropy or
	// persistence failure leaves the original credential intact.
	fresh, refresh, err := s.store.CreateUserToServerTokenE(tok.UserID, tok.AppID, tok.OAuthAppClientID, tok.Scopes, 8*time.Hour, tok.RefreshTokenValue != "")
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Carry the source token's installation/permission/repository restrictions
	// onto the replacement. Resetting a narrowed token must not silently widen
	// it back to the installation's full authority.
	s.store.ScopeUserToServerToken(fresh.Token, tok.InstallationIDs, tok.Permissions, tok.RepositoryIDs)
	s.store.RevokeUserToServerToken(tok.Token)
	resp := oauthTokenInspectionJSON(s.store, fresh, s.userByID(fresh.UserID))
	resp["token"] = fresh.Token
	if refresh != nil {
		resp["refresh_token"] = refresh.Token
		resp["refresh_token_expires_in"] = int(time.Until(refresh.ExpiresAt).Seconds())
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleRevokeOAuthToken(w http.ResponseWriter, r *http.Request) {
	clientID, _ := s.authenticateClientCreds(w, r, r.PathValue("client_id"))
	if clientID == "" {
		return
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.AccessToken == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "access_token required")
		return
	}
	tok, _ := s.store.LookupUserToServerToken(body.AccessToken)
	if tok == nil {
		w.WriteHeader(http.StatusNoContent) // idempotent
		return
	}
	if !tokenMatchesClient(tok, clientID, s.store) {
		writeGHError(w, http.StatusUnprocessableEntity, "token does not match client_id")
		return
	}
	s.store.RevokeUserToServerToken(tok.Token)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleScopeOAuthToken(w http.ResponseWriter, r *http.Request) {
	clientID, _ := s.authenticateClientCreds(w, r, r.PathValue("client_id"))
	if clientID == "" {
		return
	}
	var body struct {
		AccessToken   string            `json:"access_token"`
		Target        string            `json:"target"`
		TargetID      int               `json:"target_id"`
		Permissions   map[string]string `json:"permissions"`
		Repositories  *[]string         `json:"repositories"`
		RepositoryIDs *[]int            `json:"repository_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.AccessToken == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "access_token required")
		return
	}
	tok, _ := s.store.LookupUserToServerToken(body.AccessToken)
	if tok == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !tokenMatchesClient(tok, clientID, s.store) {
		writeGHError(w, http.StatusUnprocessableEntity, "token does not match client_id")
		return
	}
	if tok.AppID == 0 {
		writeGHError(w, http.StatusUnprocessableEntity, "access_token must belong to a GitHub App")
		return
	}
	if (body.Target == "" && body.TargetID == 0) || (body.Repositories != nil && body.RepositoryIDs != nil) {
		writeGHValidationError(w, "Authorization", "target", "invalid")
		return
	}
	inst := s.resolveScopeTargetInstallation(tok, body.Target, body.TargetID)
	if inst == nil ||
		(len(tok.InstallationIDs) > 0 && !slices.Contains(tok.InstallationIDs, inst.ID)) ||
		!s.scopeTargetAccessibleToUser(tok.UserID, inst) {
		writeGHError(w, http.StatusUnprocessableEntity, "The target is not accessible to the token owner")
		return
	}
	if scope, ok := validateRequestedPermissions(body.Permissions, inst.Permissions); !ok {
		writeGHError(w, http.StatusUnprocessableEntity,
			"The permissions requested are not granted to this authorization (permission: "+scope+")")
		return
	}
	if tok.Permissions != nil {
		if scope, ok := validateRequestedPermissions(body.Permissions, tok.Permissions); !ok {
			writeGHError(w, http.StatusUnprocessableEntity,
				"The permissions requested are not granted to this authorization (permission: "+scope+")")
			return
		}
	}
	var repositoryNames []string
	if body.Repositories != nil {
		repositoryNames = *body.Repositories
	}
	var requestedRepositoryIDs []int
	if body.RepositoryIDs != nil {
		requestedRepositoryIDs = *body.RepositoryIDs
	}
	repositoryIDs, ok := s.resolveScopedUserTokenRepositories(inst, tok, repositoryNames, requestedRepositoryIDs)
	if !ok {
		writeGHError(w, http.StatusUnprocessableEntity,
			"There is at least one repository that does not exist or is not accessible to the authorization")
		return
	}
	// Real GitHub mints a fresh user-to-server token narrowed to the requested
	// target / permissions / repositories and returns it (the original is not
	// revoked). Reflect that by creating a new token carrying the same user +
	// app, scoped to the requested installation when a target is supplied.
	ttl := time.Until(tok.ExpiresAt)
	if ttl <= 0 {
		ttl = 8 * time.Hour
	}
	scoped, _, err := s.store.CreateUserToServerTokenE(tok.UserID, tok.AppID, tok.OAuthAppClientID, tok.Scopes, ttl, false)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// When the request omits permissions, inherit the source token's own
	// restriction rather than defaulting to the installation's full permission
	// map — scoping must only ever narrow, never widen.
	effectivePermissions := body.Permissions
	if effectivePermissions == nil {
		effectivePermissions = tok.Permissions
	}
	if !s.store.ScopeUserToServerToken(scoped.Token, []int{inst.ID}, effectivePermissions, repositoryIDs) {
		s.store.RevokeUserToServerToken(scoped.Token)
		writeGHError(w, http.StatusInternalServerError, "failed to persist scoped token")
		return
	}
	scoped.InstallationIDs = []int{inst.ID}
	scoped.Permissions = cloneStringMap(effectivePermissions)
	scoped.RepositoryIDs = appendOptionalInts(repositoryIDs)

	writeJSON(w, http.StatusOK, oauthTokenInspectionJSON(s.store, scoped, s.userByID(scoped.UserID)))
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func appendOptionalInts(values []int) []int {
	if values == nil {
		return nil
	}
	return append([]int{}, values...)
}

func (s *Server) scopeTargetAccessibleToUser(userID int, inst *Installation) bool {
	user := s.store.GetUserByID(userID)
	if user == nil || inst == nil {
		return false
	}
	switch inst.TargetType {
	case "User":
		return inst.TargetID == user.ID && strings.EqualFold(inst.TargetLogin, user.Login)
	case "Organization":
		// This asks about the token owner named in the authorization, not the
		// Basic-authenticated App client carrying this management request.
		return namedUserIsActiveOrgMember(s.store, user, inst.TargetLogin)
	default:
		return false
	}
}

func (s *Server) resolveScopedUserTokenRepositories(inst *Installation, source *UserToServerToken, names []string, ids []int) ([]int, bool) {
	if names == nil && ids == nil {
		return nil, true
	}
	accessible := installationAccessibleRepoIDs(s.store, inst)
	allowedBySource := func(id int) bool {
		return source.RepositoryIDs == nil || slices.Contains(source.RepositoryIDs, id)
	}
	resolved := make([]int, 0, len(names)+len(ids))
	for _, id := range ids {
		if _, ok := accessible[id]; !ok || !allowedBySource(id) {
			return nil, false
		}
		resolved = append(resolved, id)
	}
	for _, name := range names {
		repo := s.store.GetRepo(inst.TargetLogin, name)
		if repo == nil {
			return nil, false
		}
		if _, ok := accessible[repo.ID]; !ok || !allowedBySource(repo.ID) {
			return nil, false
		}
		resolved = append(resolved, repo.ID)
	}
	slices.Sort(resolved)
	resolved = slices.Compact(resolved)
	return resolved, true
}

// resolveScopeTargetInstallation finds the installation a scoped-token request
// targets, by target login or target id, among the app's installations.
func (s *Server) resolveScopeTargetInstallation(tok *UserToServerToken, targetLogin string, targetID int) *Installation {
	if tok.AppID == 0 {
		return nil
	}
	for _, inst := range s.store.ListAppInstallations(tok.AppID) {
		loginMatches := targetLogin == "" || strings.EqualFold(inst.TargetLogin, targetLogin)
		idMatches := targetID == 0 || inst.TargetID == targetID
		if loginMatches && idMatches {
			return inst
		}
	}
	return nil
}

func (s *Server) handleRevokeOAuthGrant(w http.ResponseWriter, r *http.Request) {
	clientID, _ := s.authenticateClientCreds(w, r, r.PathValue("client_id"))
	if clientID == "" {
		return
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.AccessToken == "" {
		writeGHError(w, http.StatusUnprocessableEntity, "access_token required")
		return
	}
	tok, _ := s.store.LookupUserToServerToken(body.AccessToken)
	if tok == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !tokenMatchesClient(tok, clientID, s.store) {
		writeGHError(w, http.StatusUnprocessableEntity, "token does not match client_id")
		return
	}
	s.store.RevokeUserGrant(clientID, tok.UserID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCreateBrowserOAuthApp(w http.ResponseWriter, r *http.Request) {
	r, user := s.authenticatedBrowserRequest(r)
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	req, ok := decodeOAuthAppSettingsRequest(w, r)
	if !ok {
		return
	}
	app, err := s.store.CreateOAuthAppE(user.ID, req.Name, req.Description, req.URL, req.CallbackURL)
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, oauthAppToJSON(app, true))
}

func (s *Server) handleListBrowserOAuthApps(w http.ResponseWriter, r *http.Request) {
	_, user := s.authenticatedBrowserRequest(r)
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	apps := s.store.ListOAuthApps()
	out := make([]map[string]interface{}, 0, len(apps))
	for _, a := range apps {
		if a.OwnerID == user.ID {
			out = append(out, oauthAppToJSON(a, false))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type oauthAppSettingsRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url"`
	CallbackURL string `json:"callback_url"`
}

func decodeOAuthAppSettingsRequest(w http.ResponseWriter, r *http.Request) (oauthAppSettingsRequest, bool) {
	var req oauthAppSettingsRequest
	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
			return req, false
		}
	} else {
		if err := r.ParseForm(); err != nil {
			writeGHError(w, http.StatusBadRequest, "Problems parsing form")
			return req, false
		}
		req.Name = r.PostFormValue("name")
		req.Description = r.PostFormValue("description")
		req.URL = r.PostFormValue("url")
		req.CallbackURL = r.PostFormValue("callback_url")
		if req.CallbackURL == "" {
			req.CallbackURL = r.PostFormValue("callbackUrl")
		}
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeGHValidationError(w, "OAuthApp", "name", "missing_field")
		return req, false
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		writeGHValidationError(w, "OAuthApp", "url", "missing_field")
		return req, false
	}
	if err := validateClientCallbackURL(req.URL); err != nil {
		writeGHValidationError(w, "OAuthApp", "url", "invalid")
		return req, false
	}
	req.CallbackURL = strings.TrimSpace(req.CallbackURL)
	if req.CallbackURL == "" {
		writeGHValidationError(w, "OAuthApp", "callback_url", "missing_field")
		return req, false
	}
	if err := validateClientCallbackURL(req.CallbackURL); err != nil {
		writeGHValidationError(w, "OAuthApp", "callback_url", "invalid")
		return req, false
	}
	return req, true
}

func tokenMatchesClient(tok *UserToServerToken, clientID string, st *Store) bool {
	if tok.OAuthAppClientID != "" {
		return tok.OAuthAppClientID == clientID
	}
	if tok.AppID > 0 {
		if app := st.GetAppByClientID(clientID); app != nil && app.ID == tok.AppID {
			return true
		}
	}
	return false
}

func (s *Server) userByID(id int) *User {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return s.store.Users[id]
}

func oauthTokenInspectionJSON(st *Store, tok *UserToServerToken, user *User) map[string]interface{} {
	// app: the OAuth App / GitHub App the token was issued for, with the real
	// client_id, name and url.
	app := map[string]interface{}{
		"client_id": "",
		"name":      "",
		"url":       "",
	}
	if tok.OAuthAppClientID != "" {
		app["client_id"] = tok.OAuthAppClientID
		if oa := st.GetOAuthApp(tok.OAuthAppClientID); oa != nil {
			app["name"] = oa.Name
			app["url"] = oa.URL
		}
	} else if tok.AppID > 0 {
		if ghApp := st.GetApp(tok.AppID); ghApp != nil {
			app["client_id"] = ghApp.ClientID
			app["name"] = ghApp.Name
			app["url"] = "https://github.com/apps/" + ghApp.Slug
		}
	}

	// installation: null for OAuth-App tokens; the scoped installation object
	// for GitHub-App user-to-server tokens.
	var installation interface{}
	if tok.AppID > 0 {
		if inst := firstInstallationForToken(st, tok); inst != nil {
			rendered := installationToJSON(inst)
			if tok.Permissions != nil {
				rendered["permissions"] = cloneStringMap(tok.Permissions)
			}
			if tok.RepositoryIDs != nil {
				rendered["repository_selection"] = "selected"
			}
			installation = rendered
		}
	}

	// id/url identify the authorization. We derive a stable id from the token
	// value (we don't carry a separate authorization id) and the matching URL.
	authID := authorizationID(tok.Token)
	out := map[string]interface{}{
		"id":               authID,
		"url":              "/api/v3/authorizations/" + strconv.Itoa(authID),
		"scopes":           splitScopes(tok.Scopes),
		"token":            tok.Token,
		"token_last_eight": lastEight(tok.Token),
		"hashed_token":     hashedToken(tok.Token),
		"app":              app,
		"note":             nil,
		"note_url":         nil,
		"updated_at":       tok.CreatedAt.UTC().Format(time.RFC3339),
		"created_at":       tok.CreatedAt.UTC().Format(time.RFC3339),
		"fingerprint":      nil,
		"expires_at":       tok.ExpiresAt.UTC().Format(time.RFC3339),
		"installation":     installation,
	}
	if user != nil {
		out["user"] = userToJSON(user)
	}
	return out
}

// firstInstallationForToken resolves the installation a GitHub-App
// user-to-server token is scoped to. Prefers an explicit InstallationIDs
// entry, falling back to the (user, app) installation.
func firstInstallationForToken(st *Store, tok *UserToServerToken) *Installation {
	for _, id := range tok.InstallationIDs {
		if inst := st.GetInstallation(id); inst != nil {
			return inst
		}
	}
	for _, inst := range st.ListAppInstallations(tok.AppID) {
		if inst.TargetID == tok.UserID {
			return inst
		}
	}
	return nil
}

// lastEight returns the last 8 characters of the token, matching real
// GitHub's token_last_eight field.
func lastEight(token string) string {
	if len(token) <= 8 {
		return token
	}
	return token[len(token)-8:]
}

// hashedToken returns the hex-encoded SHA-256 of the token, matching real
// GitHub's hashed_token field.
func hashedToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// authorizationID derives a stable positive integer id for an authorization
// from its token value. GitHub exposes a separate int authorization id; the
// Bleephub does not store one, so derive it deterministically from the token.
func authorizationID(token string) int {
	sum := sha256.Sum256([]byte(token))
	id := int(binary.BigEndian.Uint32(sum[:4]) & 0x7fffffff)
	if id == 0 {
		id = 1
	}
	return id
}

func splitScopes(s string) []string {
	if s == "" {
		return []string{}
	}
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func oauthAppToJSON(a *OAuthApp, includeSecret bool) map[string]interface{} {
	out := map[string]interface{}{
		"client_id":    a.ClientID,
		"name":         a.Name,
		"description":  a.Description,
		"url":          a.URL,
		"callback_url": a.CallbackURL,
		"owner_id":     a.OwnerID,
		"created_at":   a.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":   a.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if includeSecret {
		out["client_secret"] = a.ClientSecret
	}
	return out
}

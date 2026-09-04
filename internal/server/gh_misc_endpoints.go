package bleephub

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Long-tail GitHub API surfaces gh CLI / octokit / probot hit: user keys /
// emails / follows, Actions OIDC, Pages, org audit log, and Marketplace.
func (s *Server) registerGHMiscEndpoints() {
	// Users keys + emails + follow
	s.route("GET /api/v3/user/keys", s.handleListUserKeys)
	s.route("POST /api/v3/user/keys", s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleCreateUserKey))
	s.route("GET /api/v3/user/keys/{key_id}", s.handleGetUserKey)
	s.route("DELETE /api/v3/user/keys/{key_id}", s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleDeleteUserKey))
	s.route("GET /api/v3/user/gpg_keys", s.handleListGPGKeys)
	s.route("POST /api/v3/user/gpg_keys", s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleCreateGPGKey))
	s.route("GET /api/v3/user/gpg_keys/{gpg_key_id}", s.handleGetGPGKey)
	s.route("DELETE /api/v3/user/gpg_keys/{gpg_key_id}", s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleDeleteGPGKey))
	s.route("GET /api/v3/user/emails", s.handleListUserEmails)
	s.route("GET /api/v3/users/{username}/keys", s.handleListUserKeysByLogin)
	s.route("GET /api/v3/users/{username}/gpg_keys", s.handleListGPGKeysByLogin)
	s.route("GET /api/v3/users/{username}/followers", s.handleListFollowers)
	s.route("GET /api/v3/users/{username}/following", s.handleListFollowing)
	s.route("GET /api/v3/user/followers", s.handleListMyFollowers)
	s.route("GET /api/v3/user/following", s.handleListMyFollowing)
	s.route("PUT /api/v3/user/following/{username}", s.handleFollowUser)
	s.route("DELETE /api/v3/user/following/{username}", s.handleUnfollowUser)

	// Users extras
	s.route("GET /api/v3/users", s.handleListUsers)
	s.route("POST /api/v3/admin/users", s.handleAdminCreateUser)
	s.route("PATCH /api/v3/admin/users/{username}", s.handleAdminRenameUser)
	s.route("DELETE /api/v3/admin/users/{username}", s.handleAdminDeleteUser)
	s.route("PUT /api/v3/users/{username}/site_admin", s.handleAdminPromoteUser)
	s.route("DELETE /api/v3/users/{username}/site_admin", s.handleAdminDemoteUser)
	s.route("PUT /api/v3/users/{username}/suspended", s.handleAdminSuspendUser)
	s.route("DELETE /api/v3/users/{username}/suspended", s.handleAdminUnsuspendUser)
	s.route("GET /api/v3/users/{username}/gists", s.handleListUserGists)
	s.route("GET /api/v3/users/{username}/events", s.handleListUserEvents)
	s.route("GET /api/v3/users/{username}/events/public", s.handleListUserEventsPublic)
	s.route("GET /api/v3/users/{username}/events/orgs/{org}", s.handleListUserEventsForOrg)
	s.route("GET /api/v3/users/{username}/received_events", s.handleListUserReceivedEvents)
	s.route("GET /api/v3/users/{username}/received_events/public", s.handleListUserReceivedEventsPublic)
	s.route("GET /api/v3/users/{username}/following/{target_user}", s.handleCheckUserFollowing)
	s.route("GET /api/v3/users/{username}/social_accounts", s.handleListUserSocialAccounts)
	s.route("GET /api/v3/users/{username}/ssh_signing_keys", s.handleListUserSSHSigningKeys)
	s.route("GET /api/v3/users/{username}/subscriptions", s.handleListUserSubscriptions)
	s.route("GET /api/v3/user/blocks", s.handleListUserBlocks)
	s.route("GET /api/v3/user/blocks/{username}", s.handleCheckUserBlocked)
	s.route("PUT /api/v3/user/blocks/{username}", s.handleBlockUser)
	s.route("DELETE /api/v3/user/blocks/{username}", s.handleUnblockUser)
	s.route("GET /api/v3/user/following/{username}", s.handleCheckMyFollowing)
	s.route("GET /api/v3/user/social_accounts", s.handleListMySocialAccounts)
	s.route("POST /api/v3/user/social_accounts", s.handleCreateMySocialAccounts)
	s.route("DELETE /api/v3/user/social_accounts", s.handleDeleteMySocialAccounts)
	s.route("GET /api/v3/user/ssh_signing_keys", s.handleListMySSHSigningKeys)
	s.route("POST /api/v3/user/ssh_signing_keys", s.handleCreateMySSHSigningKey)
	s.route("DELETE /api/v3/user/ssh_signing_keys/{ssh_signing_key_id}", s.handleDeleteMySSHSigningKey)
	s.route("GET /api/v3/user/starred/{owner}/{repo}", s.handleCheckMyStarredRepo)
	s.route("GET /api/v3/user/subscriptions", s.handleListMySubscriptions)

	// Actions OIDC — minted for the requesting job, so gate it on that job's runtime token.
	s.route("GET /token", s.requireJobToken(s.handleActionsOIDCToken))
	s.route("GET /.well-known/openid-configuration", s.handleOIDCDiscovery)
	s.route("GET /.well-known/jwks", s.handleJWKS)
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/oidc/customization/sub", s.handleOIDCCustomSubGet)
	s.route("PUT /api/v3/repos/{owner}/{repo}/actions/oidc/customization/sub",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleOIDCCustomSubPut))
	s.route("GET /api/v3/orgs/{org}/actions/oidc/customization/sub", s.handleOIDCCustomSubGet)
	s.route("PUT /api/v3/orgs/{org}/actions/oidc/customization/sub",
		s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleOIDCCustomSubPut))

	// Pages
	s.route("GET /api/v3/repos/{owner}/{repo}/pages", s.requirePagesRead(s.handlePagesGet))
	s.route("POST /api/v3/repos/{owner}/{repo}/pages",
		s.requirePerm(store.ScopePages, store.PermWrite, s.handlePagesCreate))
	s.route("PUT /api/v3/repos/{owner}/{repo}/pages",
		s.requirePerm(store.ScopePages, store.PermWrite, s.handlePagesUpdate))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/pages",
		s.requirePerm(store.ScopePages, store.PermWrite, s.handlePagesDelete))
	s.route("GET /api/v3/repos/{owner}/{repo}/pages/builds", s.requirePagesRead(s.handlePagesListBuilds))
	s.route("POST /api/v3/repos/{owner}/{repo}/pages/builds",
		s.requirePerm(store.ScopePages, store.PermWrite, s.handlePagesTriggerBuild))
	s.route("GET /api/v3/repos/{owner}/{repo}/pages/builds/latest", s.requirePagesRead(s.handlePagesLatestBuild))
	s.route("GET /api/v3/repos/{owner}/{repo}/pages/builds/{build_id}", s.requirePagesRead(s.handlePagesGetBuild))

	s.route("GET /api/v3/orgs/{org}/audit-log", s.handleOrgAuditLog)

	// Marketplace; the stubbed variants serve the same plan/purchase state as the production routes.
	s.route("GET /api/v3/marketplace_listing/plans", s.handleMarketplacePlans)
	s.route("GET /api/v3/marketplace_listing/accounts/{account_id}", s.handleMarketplaceAccount)
	s.route("GET /api/v3/marketplace_listing/plans/{plan_id}/accounts", s.handleMarketplacePlanAccounts)
	s.route("GET /api/v3/marketplace_listing/stubbed/plans", s.handleMarketplacePlans)
	s.route("GET /api/v3/marketplace_listing/stubbed/plans/{plan_id}/accounts", s.handleMarketplacePlanAccounts)
	s.route("GET /api/v3/marketplace_listing/stubbed/accounts/{account_id}", s.handleMarketplaceAccount)

	// Meta — gh resolves the host version from installed_version to gate its GHES feature detection.
	s.route("GET /api/v3/meta", s.handleMeta)
}

// handleMeta serves GET /api/v3/meta in GHES shape. bleephub presents as GHES
// 3.21.0 to steer gh's version-gated feature detection. installed_version is a
// GHES-only member absent from the vendored dotcom description (see
// openapi-violation-allowlist.txt); verifiable_password_authentication is
// genuinely false — the API is token-only.
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	// ssh_key_fingerprints/ssh_keys reflect this instance's configured SSH host
	// key (empty when none is configured), letting clients seed known_hosts.
	fingerprints, sshKeys := metaSSHHostKeys()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"verifiable_password_authentication": false,
		"installed_version":                  "3.21.0",
		"ssh_key_fingerprints":               fingerprints,
		"ssh_keys":                           sshKeys,
	})
}

// User keys

func (s *Server) handleListUserKeys(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	s.store.Misc.Mu.RLock()
	defer s.store.Misc.Mu.RUnlock()
	out := make([]map[string]interface{}, 0, len(s.store.Misc.KeysByUser[user.ID]))
	for _, k := range s.store.Misc.KeysByUser[user.ID] {
		out = append(out, userKeyToJSON(k, s.baseURL(r)))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleCreateUserKey(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	// Changing an authentication key is sensitive: honor proof-of-presence first.
	if s.requireProofOfPresence(w, r) {
		return
	}
	var req struct {
		Title string `json:"title"`
		Key   string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		store.WriteGHValidationError(w, "Key", "key", "missing_field")
		return
	}
	s.store.Misc.Mu.Lock()
	id := s.store.Misc.NextKeyID
	s.store.Misc.NextKeyID++
	k := &store.UserKey{ID: id, Title: req.Title, Key: req.Key, Verified: true, UserID: user.ID, CreatedAt: time.Now().UTC()}
	parseErr := store.CacheParsedKey(k)
	s.store.Misc.UserKeys[id] = k
	s.store.Misc.KeysByUser[user.ID] = append(s.store.Misc.KeysByUser[user.ID], k)
	if s.store.Misc.Persist != nil {
		s.store.Misc.Persist.MustPut("user_keys", strconv.Itoa(id), k)
	}
	s.store.Misc.Mu.Unlock()
	if parseErr != nil {
		s.logger.Warn().Err(parseErr).Str("user", user.Login).
			Msg("registered SSH key does not parse; it will never authenticate")
	}
	s.recordAuditEvent("ssh_key.create", user.Login, "", map[string]interface{}{"key_id": k.ID})
	keyJSON := userKeyToJSON(k, s.baseURL(r))
	writeJSONCreated(w, jsonStringField(keyJSON, "url"), keyJSON)
}

func (s *Server) handleGetUserKey(w http.ResponseWriter, r *http.Request) {
	// A key that is not the caller's is 404 — never disclose another account's key.
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	id, err := strconv.Atoi(r.PathValue("key_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Misc.Mu.RLock()
	k := s.store.Misc.UserKeys[id]
	s.store.Misc.Mu.RUnlock()
	if k == nil || k.UserID != user.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, userKeyToJSON(k, s.baseURL(r)))
}

func (s *Server) handleDeleteUserKey(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	// Changing an authentication key is sensitive: honor proof-of-presence first.
	if s.requireProofOfPresence(w, r) {
		return
	}
	id, err := strconv.Atoi(r.PathValue("key_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Misc.Mu.Lock()
	k := s.store.Misc.UserKeys[id]
	// A key that is not the caller's is 404, so this cannot revoke another
	// account's key or probe which ids exist.
	if k == nil || k.UserID != user.ID {
		s.store.Misc.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	delete(s.store.Misc.UserKeys, id)
	src := s.store.Misc.KeysByUser[k.UserID]
	for i, x := range src {
		if x.ID == id {
			s.store.Misc.KeysByUser[k.UserID] = append(src[:i], src[i+1:]...)
			break
		}
	}
	if s.store.Misc.Persist != nil {
		s.store.Misc.Persist.MustDelete("user_keys", strconv.Itoa(id))
	}
	s.store.Misc.Mu.Unlock()
	s.recordAuditEvent("ssh_key.delete", user.Login, "", map[string]interface{}{"key_id": id})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListGPGKeys(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	s.store.Misc.Mu.RLock()
	out := make([]map[string]interface{}, 0, len(s.store.Misc.GpgKeysByUser[user.ID]))
	for _, k := range s.store.Misc.GpgKeysByUser[user.ID] {
		out = append(out, gpgKeyToJSON(k))
	}
	s.store.Misc.Mu.RUnlock()
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleCreateGPGKey(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	// Changing an authentication key is sensitive: honor proof-of-presence first.
	if s.requireProofOfPresence(w, r) {
		return
	}
	var req struct {
		ArmoredPublicKey string `json:"armored_public_key"`
		Name             string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ArmoredPublicKey == "" {
		store.WriteGHValidationError(w, "ArmoredPublicKey", "armored_public_key", "missing_field")
		return
	}
	s.store.Misc.Mu.Lock()
	id := s.store.Misc.NextGPGKeyID
	s.store.Misc.NextGPGKeyID++
	email := ""
	if user.Email != "" {
		email = user.Email
	}
	k := &store.GPGKey{
		ID: id, PublicKey: req.ArmoredPublicKey, Name: req.Name, UserID: user.ID,
		CreatedAt: time.Now(), CanSign: true, CanEncryptComms: true, CanEncryptStorage: true, CanCertify: true,
		Emails: []store.GPGKeyEmail{{Email: email, Verified: true, Primary: true}},
	}
	s.store.Misc.GpgKeys[id] = k
	s.store.Misc.GpgKeysByUser[user.ID] = append(s.store.Misc.GpgKeysByUser[user.ID], k)
	if s.store.Misc.Persist != nil {
		s.store.Misc.Persist.MustPut("gpg_keys", strconv.Itoa(id), k)
	}
	s.store.Misc.Mu.Unlock()
	s.recordAuditEvent("gpg_key.create", user.Login, "", map[string]interface{}{"gpg_key_id": id})
	writeJSON(w, http.StatusCreated, gpgKeyToJSON(k))
}

func (s *Server) handleGetGPGKey(w http.ResponseWriter, r *http.Request) {
	// A key that is not the caller's is 404 — never disclose another account's key.
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	id, err := strconv.Atoi(r.PathValue("gpg_key_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Misc.Mu.RLock()
	k := s.store.Misc.GpgKeys[id]
	s.store.Misc.Mu.RUnlock()
	if k == nil || k.UserID != user.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, gpgKeyToJSON(k))
}

func (s *Server) handleDeleteGPGKey(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	// Changing an authentication key is sensitive: honor proof-of-presence first.
	if s.requireProofOfPresence(w, r) {
		return
	}
	id, err := strconv.Atoi(r.PathValue("gpg_key_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Misc.Mu.Lock()
	k := s.store.Misc.GpgKeys[id]
	if k == nil || k.UserID != user.ID {
		s.store.Misc.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	delete(s.store.Misc.GpgKeys, id)
	src := s.store.Misc.GpgKeysByUser[user.ID]
	for i, x := range src {
		if x.ID == id {
			s.store.Misc.GpgKeysByUser[user.ID] = append(src[:i], src[i+1:]...)
			break
		}
	}
	if s.store.Misc.Persist != nil {
		_ = s.store.Misc.Persist.Delete("gpg_keys", strconv.Itoa(id))
	}
	s.store.Misc.Mu.Unlock()
	s.recordAuditEvent("gpg_key.delete", user.Login, "", map[string]interface{}{"gpg_key_id": id})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListGPGKeysByLogin(w http.ResponseWriter, r *http.Request) {
	user := s.store.LookupUserByLogin(r.PathValue("username"))
	if user == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Misc.Mu.RLock()
	out := make([]map[string]interface{}, 0, len(s.store.Misc.GpgKeysByUser[user.ID]))
	for _, k := range s.store.Misc.GpgKeysByUser[user.ID] {
		out = append(out, gpgKeyToJSON(k))
	}
	s.store.Misc.Mu.RUnlock()
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func gpgKeyToJSON(k *store.GPGKey) map[string]interface{} {
	// GitHub's gpg-key emails carry only {email, verified}; drop the stored primary flag.
	emails := make([]map[string]interface{}, 0, len(k.Emails))
	for _, e := range k.Emails {
		emails = append(emails, map[string]interface{}{"email": e.Email, "verified": e.Verified})
	}
	m := map[string]interface{}{
		"id":                  k.ID,
		"primary_key_id":      nil,
		"key_id":              k.KeyID,
		"raw_key":             nullOrString(k.RawKey),
		"public_key":          k.PublicKey,
		"emails":              emails,
		"subkeys":             []interface{}{},
		"can_sign":            k.CanSign,
		"can_encrypt_comms":   k.CanEncryptComms,
		"can_encrypt_storage": k.CanEncryptStorage,
		"can_certify":         k.CanCertify,
		"created_at":          k.CreatedAt.UTC().Format(time.RFC3339),
		"expires_at":          nil,
		"revoked":             k.Revoked,
		"name":                nullOrString(k.Name),
	}
	if k.PrimaryKeyID != 0 {
		m["primary_key_id"] = k.PrimaryKeyID
	}
	if k.ExpiresAt != nil {
		m["expires_at"] = k.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return m
}

func (s *Server) handleListUserEmails(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	emails := s.store.ListUserEmails(user.ID)
	out := make([]map[string]interface{}, len(emails))
	for i, e := range emails {
		out[i] = userEmailJSON(e)
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleListUserKeysByLogin(w http.ResponseWriter, r *http.Request) {
	user := s.store.LookupUserByLogin(r.PathValue("username"))
	if user == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Misc.Mu.RLock()
	defer s.store.Misc.Mu.RUnlock()
	out := make([]map[string]interface{}, 0, len(s.store.Misc.KeysByUser[user.ID]))
	for _, k := range s.store.Misc.KeysByUser[user.ID] {
		out = append(out, map[string]interface{}{"id": k.ID, "key": k.Key})
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

// resolveLoginsJSON converts logins to user JSON, skipping unknown logins.
// Must not be called with Misc.mu held: LookupUserByLogin takes Store.mu, and
// the lock order is Store.mu before Misc.mu.
func (s *Server) resolveLoginsJSON(logins []string, baseURL string) []map[string]interface{} {
	out := []map[string]interface{}{}
	for _, login := range logins {
		if u := s.store.LookupUserByLogin(login); u != nil {
			out = append(out, store.UserToJSON(u, baseURL))
		}
	}
	return out
}

// followerLogins returns the logins that follow target. The caller resolves
// them to users after Misc.mu is released.
func (s *Server) followerLogins(target string) []string {
	s.store.Misc.Mu.RLock()
	defer s.store.Misc.Mu.RUnlock()
	var logins []string
	for user, follows := range s.store.Misc.Follows {
		if follows[target] {
			logins = append(logins, user)
		}
	}
	sort.Strings(logins)
	return logins
}

// followingLogins returns the logins that login follows. The caller resolves
// them to users after Misc.mu is released.
func (s *Server) followingLogins(login string) []string {
	s.store.Misc.Mu.RLock()
	defer s.store.Misc.Mu.RUnlock()
	var logins []string
	for target := range s.store.Misc.Follows[login] {
		logins = append(logins, target)
	}
	sort.Strings(logins)
	return logins
}

func (s *Server) handleListFollowers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, s.resolveLoginsJSON(s.followerLogins(r.PathValue("username")), s.baseURL(r))))
}
func (s *Server) handleListFollowing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, s.resolveLoginsJSON(s.followingLogins(r.PathValue("username")), s.baseURL(r))))
}
func (s *Server) handleListMyFollowers(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeJSON(w, http.StatusOK, []map[string]interface{}{})
		return
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, s.resolveLoginsJSON(s.followerLogins(user.Login), s.baseURL(r))))
}
func (s *Server) handleListMyFollowing(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeJSON(w, http.StatusOK, []map[string]interface{}{})
		return
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, s.resolveLoginsJSON(s.followingLogins(user.Login), s.baseURL(r))))
}

func (s *Server) handleFollowUser(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	target := s.store.LookupUserByLogin(r.PathValue("username"))
	if target == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	// A user blocked by the target cannot follow them (GitHub returns 403).
	if s.store.IsUserBlocked(target.ID, user.ID) {
		writeGHError(w, http.StatusForbidden, "You have been blocked from following this user.")
		return
	}
	s.store.SetFollow(user.Login, target.Login, true)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUnfollowUser(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	if user == nil {
		writeGHError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	s.store.SetFollow(user.Login, r.PathValue("username"), false)
	w.WriteHeader(http.StatusNoContent)
}

// Actions OIDC

func (s *Server) handleActionsOIDCToken(w http.ResponseWriter, r *http.Request) {
	audience := r.URL.Query().Get("audience")
	if audience == "" {
		audience = "https://github.com/" + r.URL.Query().Get("repo")
	}
	token, err := s.mintOIDCToken(r, audience)
	if err != nil {
		writeGHError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"value": token, "count": 1})
}

func (s *Server) handleOIDCDiscovery(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"issuer":   s.actionsOIDCIssuer(r),
		"jwks_uri": base + "/.well-known/jwks",
		// Advertise the OAuth2 authorize/token/userinfo endpoints so relying
		// parties that auto-configure from this document can use bleephub as an IdP.
		"authorization_endpoint":   base + "/login/oauth/authorize",
		"token_endpoint":           base + "/login/oauth/access_token",
		"userinfo_endpoint":        base + "/api/v3/user",
		"subject_types_supported":  []string{"public", "pairwise"},
		"response_types_supported": []string{"code", "id_token"},
		"response_modes_supported": []string{"query"},
		"grant_types_supported":    []string{"authorization_code"},
		"claims_supported": []string{
			"sub", "aud", "exp", "iat", "iss", "jti", "nbf",
			"ref", "repository", "repository_id", "repository_owner",
			"run_id", "run_number", "sha", "actor", "environment",
		},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid"},
	})
}

func (s *Server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	key, err := s.oidcKeyE()
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, err.Error())
		return
	}
	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"keys": []map[string]interface{}{
			{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "bleephub-oidc", "n": n, "e": e},
		},
	})
}

// oidcCustomSubScopeKey derives the OIDC subject-customization scope from the
// org or owner/repo path values, or "" when neither is present.
func oidcCustomSubScopeKey(r *http.Request) string {
	if org := r.PathValue("org"); org != "" {
		return "org:" + strings.ToLower(org)
	}
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	if owner != "" && repo != "" {
		return "repo:" + strings.ToLower(owner+"/"+repo)
	}
	return ""
}

func (s *Server) handleOIDCCustomSubGet(w http.ResponseWriter, r *http.Request) {
	if org := r.PathValue("org"); org != "" {
		// The org route has no repo for enforceRepoReadable to gate on; require
		// org membership instead.
		if !s.viewerIsOrgMember(r.Context(), org) {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
	} else if !s.enforceRepoReadable(w, r) {
		return
	}
	scope := oidcCustomSubScopeKey(r)
	s.store.Misc.Mu.RLock()
	keys := append([]string(nil), s.store.Misc.OidcClaimKeys[scope]...)
	s.store.Misc.Mu.RUnlock()
	body := map[string]interface{}{"include_claim_keys": keys}
	// Only the repo variant (oidc-custom-sub-repo) carries use_default: whether
	// the repo falls back to the org/enterprise default template.
	if r.PathValue("org") == "" {
		body["use_default"] = len(keys) == 0
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleOIDCCustomSubPut(w http.ResponseWriter, r *http.Request) {
	scope := oidcCustomSubScopeKey(r)
	if scope == "" {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		IncludeClaimKeys []string `json:"include_claim_keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	s.store.Misc.Mu.Lock()
	if s.store.Misc.OidcClaimKeys == nil {
		s.store.Misc.OidcClaimKeys = map[string][]string{}
	}
	s.store.Misc.OidcClaimKeys[scope] = req.IncludeClaimKeys
	if s.store.Persist != nil {
		s.store.Persist.MustPut("misc", "oidc_claim_keys", s.store.Misc.OidcClaimKeys)
	}
	s.store.Misc.Mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]interface{}{})
}

func (s *Server) oidcKeyE() (*rsa.PrivateKey, error) {
	s.store.Misc.Mu.Lock()
	defer s.store.Misc.Mu.Unlock()
	if s.store.Misc.OidcKey != nil {
		return s.store.Misc.OidcKey, nil
	}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate OpenID Connect signing key: %w", err)
	}
	s.store.Misc.OidcKey = k
	return k, nil
}

// oidcRunClaimsFromRun captures a run/job's authoritative OpenID Connect context
// at job-token mint time, so the OIDC-token endpoint mints the subject and
// claims from it rather than from runner-supplied query parameters — a job
// cannot then forge its ref/environment to assume a cloud role it isn't
// entitled to. Mirrors the run's github-context assembly (actor/head_ref/base_ref
// from the event payload).
func oidcRunClaimsFromRun(wf *store.Workflow, jd *store.JobDef) *oidcRunClaims {
	if wf == nil {
		return nil
	}
	attempt := wf.Attempt
	if attempt == 0 {
		attempt = 1
	}
	c := &oidcRunClaims{
		Ref:        wf.Ref,
		Sha:        wf.Sha,
		RunID:      strconv.Itoa(wf.RunID),
		RunNumber:  strconv.Itoa(wf.RunNumber),
		RunAttempt: strconv.Itoa(attempt),
		EventName:  wf.EventName,
		Workflow:   wf.Name,
	}
	if wf.WorkflowFilePath != "" {
		c.WorkflowFile = path.Base(wf.WorkflowFilePath)
	}
	if jd != nil {
		c.Environment = jd.EnvironmentName()
	}
	if wf.EventPayload != nil {
		if sender, _ := wf.EventPayload["sender"].(map[string]interface{}); sender != nil {
			c.Actor, _ = sender["login"].(string)
		}
		if pr, _ := wf.EventPayload["pull_request"].(map[string]interface{}); pr != nil {
			if head, _ := pr["head"].(map[string]interface{}); head != nil {
				c.HeadRef, _ = head["ref"].(string)
			}
			if base, _ := pr["base"].(map[string]interface{}); base != nil {
				c.BaseRef, _ = base["ref"].(string)
			}
		}
	}
	return c
}

func (s *Server) mintOIDCToken(r *http.Request, audience string) (string, error) {
	now := s.currentTime()
	q := r.URL.Query()
	// Every claim derives from a specific run: a missing field means no real run
	// to mint for, so fail rather than fabricate a claim that would defeat OIDC
	// trust policies. The caller must present that job's runtime token (not any
	// authenticated user) and the requested repo must be the one it is scoped
	// to, else any principal could forge a signed subject for another repo.
	principal := runnerFromContext(r.Context())
	if principal == nil || !principal.IsJobToken() {
		return "", fmt.Errorf("oidc: a job runtime token is required")
	}
	repoFull := q.Get("repo")
	if repoFull == "" {
		return "", fmt.Errorf("oidc: 'repo' (owner/name) is required")
	}
	if !principal.Scope.CoversRepo(repoFull) {
		return "", fmt.Errorf("oidc: job token is not scoped to %q", repoFull)
	}
	// Minting an OIDC token requires the workflow to declare `permissions:
	// id-token: write`; the job token carries the workflow's least-privilege set,
	// so gate on it (GitHub does not provision the token request otherwise).
	if principal.Claims == nil || principal.Claims.Perms["id_token"] != "write" {
		return "", fmt.Errorf("oidc: the workflow must set 'permissions: id-token: write' to request a token")
	}
	owner, repoName := splitRepoFull(repoFull)
	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		return "", fmt.Errorf("oidc: repository %q not found", repoFull)
	}
	// Every OIDC claim below is fixed by the workflow run at job-token mint time,
	// NOT read from this request: the runner cannot forge its ref/sha/environment
	// to obtain a token for a context it is not running in (subject forgery).
	oc := principal.Claims.OIDC
	if oc == nil {
		return "", fmt.Errorf("oidc: the job token carries no run context")
	}
	ref := oc.Ref
	if ref == "" {
		return "", fmt.Errorf("oidc: the run has no ref")
	}
	sha := oc.Sha
	runID := oc.RunID
	runNumber := oc.RunNumber
	workflowName := oc.Workflow
	workflowFile := oc.WorkflowFile
	eventName := oc.EventName
	// actor is informational; resolve its id best-effort.
	actor := oc.Actor
	actorID := 0
	if actor != "" {
		if u := s.store.LookupUserByLogin(actor); u != nil {
			actorID = u.ID
		}
	}

	repoID := repo.ID
	ownerID := repo.OwnerID
	visibility := repo.Visibility
	if visibility == "" {
		if repo.Private {
			visibility = "private"
		} else {
			visibility = "public"
		}
	}

	// "1" is the real value for a first attempt, not a placeholder.
	runAttempt := oc.RunAttempt
	if runAttempt == "" {
		runAttempt = "1"
	}
	headRef := oc.HeadRef
	baseRef := oc.BaseRef

	refType := "branch"
	switch {
	case strings.HasPrefix(ref, "refs/tags/"):
		refType = "tag"
	case strings.HasPrefix(ref, "refs/heads/"):
		refType = "branch"
	}

	env := oc.Environment

	// sub reflects the environment when supplied, else the ref form.
	var sub string
	if env != "" {
		sub = "repo:" + repoFull + ":environment:" + env
	} else if eventName == "pull_request" {
		sub = "repo:" + repoFull + ":pull_request"
	} else {
		sub = "repo:" + repoFull + ":ref:" + ref
	}

	workflowRef := repoFull + "/.github/workflows/" + workflowFile + "@" + ref
	jobWorkflowRef := workflowRef

	jtiBytes, err := store.RandomBytes(12)
	if err != nil {
		return "", fmt.Errorf("generate OpenID Connect token id: %w", err)
	}

	payload := map[string]interface{}{
		"iss":                   s.actionsOIDCIssuer(r),
		"aud":                   audience,
		"sub":                   sub,
		"iat":                   now.Unix(),
		"nbf":                   now.Unix(),
		"exp":                   now.Add(5 * time.Minute).Unix(),
		"jti":                   base64.RawURLEncoding.EncodeToString(jtiBytes),
		"ref":                   ref,
		"ref_type":              refType,
		"repository":            repoFull,
		"repository_id":         strconv.Itoa(repoID),
		"repository_owner":      owner,
		"repository_owner_id":   strconv.Itoa(ownerID),
		"repository_visibility": visibility,
		"run_id":                runID,
		"run_number":            runNumber,
		"run_attempt":           runAttempt,
		"sha":                   sha,
		"actor":                 actor,
		"actor_id":              strconv.Itoa(actorID),
		"workflow":              workflowName,
		"workflow_ref":          workflowRef,
		"workflow_sha":          sha,
		"job_workflow_ref":      jobWorkflowRef,
		"job_workflow_sha":      sha,
		"head_ref":              headRef,
		"base_ref":              baseRef,
		"event_name":            eventName,
		"runner_environment":    "github-hosted",
		"environment":           env,
	}
	// An org/repo OIDC subject customization (include_claim_keys) rewrites the
	// subject to key:value:key:value from the configured claims. The repo scope
	// wins over the org scope. Without this a configured (narrower) subject was
	// silently ignored and tokens kept the broad default subject.
	if custom := s.oidcCustomSubject(repoFull, owner, payload); custom != "" {
		payload["sub"] = custom
	}
	key, err := s.oidcKeyE()
	if err != nil {
		return "", err
	}
	return signRS256JWT(payload, key, "bleephub-oidc")
}

// oidcCustomSubject builds the customized subject from the configured
// include_claim_keys for repoFull (falling back to the owning org), joining
// "key:value" for each key present in the token payload. Returns "" when no
// customization is configured.
func (s *Server) oidcCustomSubject(repoFull, owner string, payload map[string]interface{}) string {
	s.store.Misc.Mu.RLock()
	keys := s.store.Misc.OidcClaimKeys["repo:"+strings.ToLower(repoFull)]
	if keys == nil {
		keys = s.store.Misc.OidcClaimKeys["org:"+strings.ToLower(owner)]
	}
	keys = append([]string(nil), keys...)
	s.store.Misc.Mu.RUnlock()
	if len(keys) == 0 {
		return ""
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		value, _ := payload[k].(string)
		parts = append(parts, k+":"+value)
	}
	return strings.Join(parts, ":")
}

func (s *Server) actionsOIDCIssuer(r *http.Request) string {
	issuer := s.baseURL(r)
	s.store.Mu.RLock()
	includeEnterpriseSlug := s.store.EnterpriseSettings.OIDCIncludeEnterpriseSlug
	s.store.Mu.RUnlock()
	if includeEnterpriseSlug {
		return strings.TrimRight(issuer, "/") + "/" + s.enterpriseSlug()
	}
	return issuer
}

// splitRepoFull splits "owner/repo"; a bare value becomes the repo with no owner.
func splitRepoFull(full string) (owner, repo string) {
	if i := strings.IndexByte(full, '/'); i >= 0 {
		return full[:i], full[i+1:]
	}
	return "", full
}

func signRS256JWT(payload map[string]interface{}, key *rsa.PrivateKey, kid string) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid}
	hb, _ := json.Marshal(header)
	pb, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Pages

func (s *Server) handlePagesGet(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Misc.Mu.RLock()
	pages := s.store.Misc.PagesByRepo[repo.ID]
	s.store.Misc.Mu.RUnlock()
	if pages == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, pages)
}

func (s *Server) handlePagesCreate(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Source struct {
			Branch string `json:"branch"`
			Path   string `json:"path"`
		} `json:"source"`
		CNAME     string `json:"cname"`
		BuildType string `json:"build_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	buildType := store.CoalesceStr(req.BuildType, "legacy")
	sourcePath := store.CoalesceStr(req.Source.Path, "/")
	if err := s.validatePagesConfiguration(repo, buildType, req.Source.Branch, sourcePath); err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// GitHub returns 409 when Pages is already enabled; PUT modifies an existing
	// site. Re-creating would silently drop https_enforced and the built status.
	s.store.Misc.Mu.RLock()
	exists := s.store.Misc.PagesByRepo[repo.ID] != nil
	s.store.Misc.Mu.RUnlock()
	if exists {
		writeGHError(w, http.StatusConflict, "GitHub Pages is already enabled.")
		return
	}
	ownerLogin := repo.Owner.Login
	pages := &store.PagesSite{
		CNAME:   req.CNAME,
		URL:     s.baseURL(r) + "/" + repo.FullName + "/pages",
		HTMLURL: s.baseURL(r) + "/pages/" + ownerLogin + "/" + repo.Name + "/",
		Status:  "building",
		Source: map[string]interface{}{
			// Empty branch is legitimate for a workflow build (not a fabricated default).
			"branch": req.Source.Branch,
			"path":   sourcePath,
		},
		Public:    !repo.Private,
		Custom404: false,
		BuildType: &buildType,
	}
	s.store.Misc.Mu.Lock()
	s.store.Misc.PagesByRepo[repo.ID] = pages
	if s.store.Misc.Persist != nil {
		s.store.Misc.Persist.MustPut("pages_sites", strconv.Itoa(repo.ID), pages)
	}
	s.store.Misc.Mu.Unlock()
	writeJSON(w, http.StatusCreated, pages)
}

// handlePagesUpdate returns 204 (GitHub's PUT /pages response), unlike create
// which returns 201+body.
func (s *Server) handlePagesUpdate(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		CNAME         *string `json:"cname"`
		HTTPSEnforced *bool   `json:"https_enforced"`
		BuildType     *string `json:"build_type"`
		Public        *bool   `json:"public"`
		Source        *struct {
			Branch string `json:"branch"`
			Path   string `json:"path"`
		} `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	s.store.Misc.Mu.Lock()
	pages := s.store.Misc.PagesByRepo[repo.ID]
	if pages == nil {
		s.store.Misc.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	buildType := "legacy"
	if pages.BuildType != nil {
		buildType = *pages.BuildType
	}
	if req.BuildType != nil {
		buildType = *req.BuildType
	}
	branch, _ := pages.Source["branch"].(string)
	sourcePath, _ := pages.Source["path"].(string)
	if req.Source != nil {
		branch = req.Source.Branch
		sourcePath = req.Source.Path
	}
	s.store.Misc.Mu.Unlock()
	if err := s.validatePagesConfiguration(repo, buildType, branch, sourcePath); err != nil {
		writeGHError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.store.Misc.Mu.Lock()
	pages = s.store.Misc.PagesByRepo[repo.ID]
	if pages == nil {
		s.store.Misc.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if req.CNAME != nil {
		pages.CNAME = *req.CNAME
	}
	if req.HTTPSEnforced != nil {
		pages.HTTPSEnforced = *req.HTTPSEnforced
	}
	if req.BuildType != nil {
		bt := *req.BuildType
		pages.BuildType = &bt
	}
	if req.Public != nil {
		pages.Public = *req.Public
	}
	if req.Source != nil {
		if pages.Source == nil {
			pages.Source = map[string]interface{}{}
		}
		if req.Source.Branch != "" {
			pages.Source["branch"] = req.Source.Branch
		}
		if req.Source.Path != "" {
			pages.Source["path"] = req.Source.Path
		}
	}
	if s.store.Misc.Persist != nil {
		s.store.Misc.Persist.MustPut("pages_sites", strconv.Itoa(repo.ID), pages)
	}
	s.store.Misc.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) validatePagesConfiguration(repo *store.Repo, buildType, branch, sourcePath string) error {
	if buildType != "legacy" && buildType != "workflow" {
		return fmt.Errorf("invalid request: build_type must be legacy or workflow")
	}
	if sourcePath == "" {
		sourcePath = "/"
	}
	if sourcePath != "/" && sourcePath != "/docs" {
		return fmt.Errorf("invalid request: source.path must be / or /docs")
	}
	if buildType == "legacy" {
		if branch == "" {
			return fmt.Errorf("invalid request: source.branch is required for legacy Pages builds")
		}
		if store.ResolveBranchSha(s.store.GetGitStorage(repo.Owner.Login, repo.Name), branch) == "" {
			return fmt.Errorf("invalid request: Pages source branch %q does not exist", branch)
		}
	}
	return nil
}

func (s *Server) handlePagesDelete(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if err := s.store.DeletePagesPublicationData(r.Context(), repo.ID); err != nil {
		writeGHError(w, http.StatusInternalServerError, "Pages deletion failed: "+err.Error())
		return
	}
	s.store.Misc.Mu.Lock()
	delete(s.store.Misc.PagesByRepo, repo.ID)
	if s.store.Misc.Persist != nil {
		s.store.Misc.Persist.MustDelete("pages_sites", strconv.Itoa(repo.ID))
	}
	s.store.Misc.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePagesListBuilds(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Misc.Mu.RLock()
	builds := s.store.Misc.PagesBuilds[repo.FullName]
	s.store.Misc.Mu.RUnlock()
	if builds == nil {
		builds = []*store.PagesBuild{}
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, builds))
}

func (s *Server) handlePagesTriggerBuild(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	actor := "bleephub-system"
	var pusher *store.PagesPusher
	if user := ghUserFromContext(r.Context()); user != nil {
		actor = user.Login
		pusher = &store.PagesPusher{Login: user.Login, ID: user.ID, Type: store.CoalesceStr(user.Type, "User")}
	}
	_, ok := s.runPagesBuild(r.Context(), repo, pusher, actor, s.baseURL(r))
	if !ok {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{"status": "queued", "url": s.baseURL(r) + "/api/v3/repos/" + repo.FullName + "/pages/builds/latest"})
}

func (s *Server) runPagesBuild(ctx context.Context, repo *store.Repo, pusher *store.PagesPusher, actor, baseURL string) (*store.PagesBuild, bool) {
	now := time.Now()
	s.store.Misc.Mu.Lock()
	pages := s.store.Misc.PagesByRepo[repo.ID]
	if pages == nil {
		s.store.Misc.Mu.Unlock()
		return nil, false
	}
	buildID := s.store.Misc.NextPagesBuildID
	s.store.Misc.NextPagesBuildID++
	buildURL := baseURL + "/api/v3/repos/" + repo.FullName + "/pages/builds/" + strconv.FormatInt(buildID, 10)
	build := &store.PagesBuild{
		ID:        buildID,
		URL:       buildURL,
		Status:    "queued",
		Pusher:    pusher,
		CreatedAt: now,
		UpdatedAt: now,
		Error:     &store.PagesBuildErr{},
	}
	s.store.Misc.PagesBuilds[repo.FullName] = append([]*store.PagesBuild{build}, s.store.Misc.PagesBuilds[repo.FullName]...)
	if s.store.Misc.Persist != nil {
		s.store.Misc.Persist.MustPut("pages_builds", repo.FullName, s.store.Misc.PagesBuilds[repo.FullName])
	}
	branch, sourcePath, sourceErr := pagesLegacySource(pages)
	s.store.Misc.Mu.Unlock()
	buildStarted := time.Now()
	commitSHA := ""
	custom404 := false
	buildErr := sourceErr
	if buildErr == nil {
		commitSHA, custom404, buildErr = s.buildPagesBranch(ctx, repo, branch, sourcePath)
	}
	finishedAt := time.Now()
	s.store.Misc.Mu.Lock()
	build.Commit = commitSHA
	build.UpdatedAt = finishedAt
	build.Duration = int(finishedAt.Sub(buildStarted).Milliseconds())
	if buildErr != nil {
		message := buildErr.Error()
		build.Status = "errored"
		build.Error.Message = &message
		pages.Status = "errored"
	} else {
		build.Status = "built"
		pages.Status = "built"
		pages.Custom404 = custom404
	}
	// Commit the build record and the site status in one transaction so a crash
	// can't leave them disagreeing (STORE-001/002); unlock before any panic so a
	// persist failure can't deadlock on the held lock.
	batch := store.NewPersistBatch(s.store.Misc.Persist)
	batch.Put("pages_builds", repo.FullName, s.store.Misc.PagesBuilds[repo.FullName])
	batch.Put("pages_sites", strconv.Itoa(repo.ID), pages)
	persistErr := batch.Commit()
	buildStatus, buildCommit, buildDuration := build.Status, build.Commit, build.Duration
	s.store.Misc.Mu.Unlock()
	if persistErr != nil {
		panic(&store.PersistenceFailure{Op: "batch", Bucket: "pages_sites", Key: strconv.Itoa(repo.ID), Err: persistErr})
	}
	s.recordAuditEvent("pages.build", actor, "", map[string]interface{}{"repo": repo.FullName, "build_id": buildID})
	// `page_build` fires when a Pages build finishes (ACT-026); fields were
	// snapshotted under the lock above.
	s.emitWebhookEvent(repo.FullName, "page_build", "", map[string]interface{}{
		"build": map[string]interface{}{
			"status":   buildStatus,
			"commit":   buildCommit,
			"duration": buildDuration,
		},
		"repository": repoPayload(repo, baseURL),
		"sender":     store.UserToJSON(s.store.LookupUserByLogin(actor), baseURL),
	})
	return build, true
}

func (s *Server) handlePagesLatestBuild(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.store.Misc.Mu.RLock()
	builds := s.store.Misc.PagesBuilds[repo.FullName]
	s.store.Misc.Mu.RUnlock()
	if len(builds) == 0 {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, builds[0])
}

func (s *Server) handlePagesGetBuild(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupRepoFromPath(r)
	if repo == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	buildID, _ := strconv.ParseInt(r.PathValue("build_id"), 10, 64)
	s.store.Misc.Mu.RLock()
	for _, b := range s.store.Misc.PagesBuilds[repo.FullName] {
		if b.ID == buildID {
			s.store.Misc.Mu.RUnlock()
			writeJSON(w, http.StatusOK, b)
			return
		}
	}
	s.store.Misc.Mu.RUnlock()
	writeGHError(w, http.StatusNotFound, "Not Found")
}

// Orgs depth

func (s *Server) handleOrgAuditLog(w http.ResponseWriter, r *http.Request) {
	orgName := r.PathValue("org")

	// Restricted to org owners (404 hides existence from anyone else). Honor
	// SiteAdmin too: on GHES the operator can audit any org without membership,
	// which is how the /ui audit-log page (listing every org) reaches it.
	user := ghUserFromContext(r.Context())
	org := s.store.GetOrg(orgName)
	if user == nil || org == nil || !(user.SiteAdmin || s.viewerCanAdminOrg(r.Context(), org.Login)) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}

	s.store.Misc.Mu.RLock()
	entries := make([]*store.AuditEntry, 0, len(s.store.Misc.AuditLog))
	order := r.URL.Query().Get("order")
	if order != "" && order != "desc" && order != "asc" {
		s.store.Misc.Mu.RUnlock()
		store.WriteGHValidationError(w, "AuditLog", "order", "invalid")
		return
	}
	for _, e := range s.store.Misc.AuditLog {
		if !store.AuditEntryVisibleInOrgLog(e, orgName) {
			continue
		}
		if phrase := r.URL.Query().Get("phrase"); phrase != "" {
			if !auditEntryMatchesPhrase(e, phrase) {
				continue
			}
		}
		if actorID := r.URL.Query().Get("actor_id"); actorID != "" {
			if e.Actor != actorID {
				continue
			}
		}
		entries = append(entries, e)
	}
	s.store.Misc.Mu.RUnlock()
	if order == "asc" {
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, entries))
}

func auditEntryMatchesPhrase(e *store.AuditEntry, phrase string) bool {
	terms := strings.Fields(strings.ToLower(phrase))
	if len(terms) == 0 {
		return true
	}
	text := strings.ToLower(strings.Join([]string{e.Action, e.Actor, e.Org}, " "))
	if len(e.Data) > 0 {
		if b, err := json.Marshal(e.Data); err == nil {
			text += " " + strings.ToLower(string(b))
		}
	}
	action := strings.ToLower(e.Action)
	for _, term := range terms {
		// GitHub's audit-log phrase supports key:value qualifiers; the plain
		// substring match treated e.g. "actor:octocat" as a literal it could never
		// find. Handle the common qualifiers against their own field (action:repo
		// matches every repo.* action), and fall back to free-text for a plain term
		// or an unhandled qualifier.
		if key, val, ok := strings.Cut(term, ":"); ok {
			switch key {
			case "actor":
				if !strings.EqualFold(e.Actor, val) {
					return false
				}
				continue
			case "org":
				if !strings.EqualFold(e.Org, val) {
					return false
				}
				continue
			case "action":
				if action != val && !strings.HasPrefix(action, val+".") {
					return false
				}
				continue
			}
		}
		if !strings.Contains(text, term) {
			return false
		}
	}
	return true
}

func (s *Server) recordAuditEvent(action, actor, org string, data map[string]interface{}) {
	// The append lives in the store so GraphQL resolvers write through the same path.
	s.store.RecordAuditEntry(action, actor, org, data)
}

// maxAuditLogEntries bounds the in-memory audit log, keeping the newest entries so the prepend-only slice cannot grow without limit.
const maxAuditLogEntries = 5000

func marketplacePlanToJSON(p *store.MarketplacePlan, baseURL string) map[string]interface{} {
	api := baseURL + "/api/v3/marketplace_listing/plans/" + strconv.Itoa(p.ID)
	return map[string]interface{}{
		"url":                    api,
		"accounts_url":           api + "/accounts",
		"id":                     p.ID,
		"number":                 p.Number,
		"name":                   p.Name,
		"description":            p.Description,
		"monthly_price_in_cents": p.MonthlyPriceInCents,
		"yearly_price_in_cents":  p.YearlyPriceInCents,
		"price_model":            p.PriceModel,
		"has_free_trial":         p.HasFreeTrial,
		"unit_name":              nullOrString(p.UnitName),
		"state":                  p.State,
		"bullets":                append([]string{}, p.Bullets...),
	}
}

// marketplaceAccountJSON renders the `marketplace-purchase` shape.
func (s *Server) marketplaceAccountJSON(purchase *store.MarketplacePurchase, plan *store.MarketplacePlan, baseURL string) map[string]interface{} {
	accountType := purchase.AccountType
	login := ""
	var email interface{}
	if accountType == "Organization" {
		if org := s.store.GetOrgByID(purchase.AccountID); org != nil {
			login = org.Login
			email = nullOrString(org.Email)
		}
	} else if u := s.store.GetUserByID(purchase.AccountID); u != nil {
		login = u.Login
		email = nullOrString(u.Email)
	}
	var freeTrialEnds interface{}
	if purchase.FreeTrialEnds != nil {
		freeTrialEnds = purchase.FreeTrialEnds.UTC().Format(time.RFC3339)
	}
	var nextBillingDate, updatedAt interface{}
	if purchase.NextBillingDate != nil {
		nextBillingDate = purchase.NextBillingDate.UTC().Format(time.RFC3339)
	}
	if purchase.UpdatedAt != nil {
		updatedAt = purchase.UpdatedAt.UTC().Format(time.RFC3339)
	}
	var pendingChange interface{}
	if purchase.PendingChange != nil {
		pendingRow := map[string]interface{}{
			"effective_date": purchase.PendingChange.EffectiveDate.UTC().Format(time.RFC3339),
			"billing_cycle":  nullOrString(purchase.PendingChange.BillingCycle),
			"unit_count":     purchase.PendingChange.UnitCount,
			"cancellation":   purchase.PendingChange.Cancellation,
		}
		if purchase.PendingChange.PlanID != 0 {
			if pendingPlan := s.store.GetMarketplacePlanForListing(purchase.ListingSlug, purchase.PendingChange.PlanID); pendingPlan != nil {
				pendingRow["plan"] = marketplacePlanToJSON(pendingPlan, baseURL)
			}
		}
		pendingChange = pendingRow
	}
	return map[string]interface{}{
		"url":                        baseURL + "/api/v3/users/" + login,
		"type":                       accountType,
		"id":                         purchase.AccountID,
		"login":                      login,
		"email":                      email,
		"marketplace_pending_change": pendingChange,
		"marketplace_purchase": map[string]interface{}{
			"billing_cycle":      purchase.BillingCycle,
			"next_billing_date":  nextBillingDate,
			"is_installed":       purchase.InstallationID != nil,
			"unit_count":         purchase.UnitCount,
			"on_free_trial":      purchase.OnFreeTrial,
			"free_trial_ends_on": freeTrialEnds,
			"updated_at":         updatedAt,
			"plan":               marketplacePlanToJSON(plan, baseURL),
		},
	}
}

func (s *Server) handleMarketplacePlans(w http.ResponseWriter, r *http.Request) {
	listing := s.marketplaceListingForPublisher(w, r)
	if listing == nil {
		return
	}
	base := s.baseURL(r)
	plans := s.store.ListMarketplacePlans(listing.Slug, false)
	out := make([]map[string]interface{}, 0, len(plans))
	for _, p := range plans {
		out = append(out, marketplacePlanToJSON(p, base))
	}
	writeJSON(w, http.StatusOK, paginateAndLink(w, r, out))
}

func (s *Server) handleMarketplaceAccount(w http.ResponseWriter, r *http.Request) {
	listing := s.marketplaceListingForPublisher(w, r)
	if listing == nil {
		return
	}
	s.reconcileMarketplacePurchases(listing.Slug)
	accountID, err := strconv.Atoi(r.PathValue("account_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var purchase *store.MarketplacePurchase
	for _, candidate := range s.store.ListMarketplacePurchasesForListing(listing.Slug) {
		if candidate.AccountID == accountID {
			if purchase != nil {
				writeGHError(w, http.StatusConflict, "Multiple account types share this identifier")
				return
			}
			purchase = candidate
		}
	}
	var plan *store.MarketplacePlan
	if purchase != nil {
		plan = s.store.GetMarketplacePlanForListing(listing.Slug, purchase.PlanID)
	}
	if purchase == nil || plan == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.marketplaceAccountJSON(purchase, plan, s.baseURL(r)))
}

// handleMarketplacePlanAccounts lists the accounts holding an active purchase
// of the plan.
func (s *Server) handleMarketplacePlanAccounts(w http.ResponseWriter, r *http.Request) {
	listing := s.marketplaceListingForPublisher(w, r)
	if listing == nil {
		return
	}
	s.reconcileMarketplacePurchases(listing.Slug)
	planID, err := strconv.Atoi(r.PathValue("plan_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	plan := s.store.GetMarketplacePlanForListing(listing.Slug, planID)
	purchases := make([]*store.MarketplacePurchase, 0)
	for _, pu := range s.store.ListMarketplacePurchasesForListing(listing.Slug) {
		if pu.PlanID == planID {
			purchases = append(purchases, pu)
		}
	}
	if plan == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	direction := r.URL.Query().Get("direction")
	sort.Slice(purchases, func(i, j int) bool {
		var ti, tj time.Time
		if purchases[i].UpdatedAt != nil {
			ti = *purchases[i].UpdatedAt
		}
		if purchases[j].UpdatedAt != nil {
			tj = *purchases[j].UpdatedAt
		}
		if direction == "desc" {
			return ti.After(tj)
		}
		return ti.Before(tj)
	})

	page := paginateAndLink(w, r, purchases)
	base := s.baseURL(r)
	out := make([]map[string]interface{}, 0, len(page))
	for _, pu := range page {
		out = append(out, s.marketplaceAccountJSON(pu, plan, base))
	}
	writeJSON(w, http.StatusOK, out)
}

// Helpers

func userKeyToJSON(k *store.UserKey, baseURL string) map[string]interface{} {
	return map[string]interface{}{
		"id":         k.ID,
		"url":        baseURL + "/api/v3/user/keys/" + strconv.Itoa(k.ID),
		"key":        k.Key,
		"title":      k.Title,
		"verified":   k.Verified,
		"created_at": k.CreatedAt.UTC().Format(time.RFC3339),
		"read_only":  false,
	}
}

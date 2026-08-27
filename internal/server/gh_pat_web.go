package bleephub

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) personalAccessTokenWebUser(w http.ResponseWriter, r *http.Request) (*store.User, *http.Request) {
	ctx := s.authenticateRequest(r)
	user := ghUserFromContext(ctx)
	// Accept a browser session so a signed-in user can create their first PAT;
	// requiring a pre-existing PAT would make that flow circular.
	if user == nil || (ghPersonalAccessTokenFromContext(ctx) == nil && s.sessionFromRequest(r) == nil) {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return nil, r
	}
	if suspended, _ := ctx.Value(ctxSuspendedUser).(bool); suspended {
		writeGHError(w, http.StatusForbidden, "This account has been suspended")
		return nil, r
	}
	return user, r.WithContext(ctx)
}

func (s *Server) fineGrainedPATStatus(token *store.Token) string {
	if token.ExpiresAt != nil && !token.ExpiresAt.After(time.Now()) {
		return "expired"
	}
	if s.store.GetOrg(token.ResourceOwner) == nil {
		return "active"
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, grant := range s.store.OrgPATGrants[token.ResourceOwner] {
		if grant.TokenID == token.FineGrainedID {
			return "active"
		}
	}
	for _, request := range s.store.OrgPATGrantRequests[token.ResourceOwner] {
		if request.TokenID == token.FineGrainedID {
			return "pending"
		}
	}
	return "revoked"
}

func personalAccessTokenWebJSON(token *store.Token, status string) map[string]interface{} {
	var expiry interface{}
	if token.ExpiresAt != nil {
		expiry = token.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return map[string]interface{}{
		"id": token.FineGrainedID, "name": token.Name, "resource_owner": token.ResourceOwner,
		"repository_selection": token.RepositorySelection, "repository_ids": token.RepositoryIDs,
		"permissions": token.Permissions, "created_at": token.CreatedAt.UTC().Format(time.RFC3339),
		"expires_at": expiry, "status": status,
	}
}

func (s *Server) handleListPersonalAccessTokensWeb(w http.ResponseWriter, r *http.Request) {
	user, _ := s.personalAccessTokenWebUser(w, r)
	if user == nil {
		return
	}
	tokens := make([]*store.Token, 0)
	s.store.Mu.RLock()
	for _, token := range s.store.Tokens {
		if token.UserID == user.ID && token.FineGrained {
			copy := *token
			copy.RepositoryIDs = append([]int(nil), token.RepositoryIDs...)
			tokens = append(tokens, &copy)
		}
	}
	s.store.Mu.RUnlock()
	sort.Slice(tokens, func(i, j int) bool { return tokens[i].FineGrainedID < tokens[j].FineGrainedID })
	rows := make([]map[string]interface{}, 0, len(tokens))
	for _, token := range tokens {
		rows = append(rows, personalAccessTokenWebJSON(token, s.fineGrainedPATStatus(token)))
	}

	owners := []map[string]interface{}{{"login": user.Login, "type": "User"}}
	repositories := map[string][]map[string]interface{}{user.Login: {}}
	pending := []map[string]interface{}{}
	for _, org := range s.store.ListOrgsByUser(user.ID) {
		owners = append(owners, map[string]interface{}{"login": org.Login, "type": "Organization"})
		repositories[org.Login] = s.personalAccessTokenRepositories(org.Login)
		if s.viewerCanAdminOrg(r.Context(), org.Login) {
			s.store.Mu.RLock()
			requests := make([]*store.OrgPATGrantRequest, 0, len(s.store.OrgPATGrantRequests[org.Login]))
			for _, request := range s.store.OrgPATGrantRequests[org.Login] {
				requests = append(requests, request)
			}
			s.store.Mu.RUnlock()
			sort.Slice(requests, func(i, j int) bool { return requests[i].ID < requests[j].ID })
			for _, request := range requests {
				row := s.patGrantRequestJSON(request, s.baseURL(r))
				row["organization"] = org.Login
				pending = append(pending, row)
			}
		}
	}
	repositories[user.Login] = s.personalAccessTokenRepositories(user.Login)
	writeJSON(w, http.StatusOK, map[string]interface{}{"tokens": rows, "resource_owners": owners, "repositories": repositories, "pending_requests": pending})
}

func (s *Server) personalAccessTokenRepositories(owner string) []map[string]interface{} {
	type repositoryRow struct {
		id      int
		name    string
		private bool
	}
	typed := []repositoryRow{}
	s.store.Mu.RLock()
	for _, repo := range s.store.ReposByName {
		if strings.HasPrefix(repo.FullName, owner+"/") {
			typed = append(typed, repositoryRow{id: repo.ID, name: repo.Name, private: repo.Private})
		}
	}
	s.store.Mu.RUnlock()
	sort.Slice(typed, func(i, j int) bool { return typed[i].name < typed[j].name })
	rows := make([]map[string]interface{}, 0, len(typed))
	for _, repo := range typed {
		rows = append(rows, map[string]interface{}{"id": repo.id, "name": repo.name, "private": repo.private})
	}
	return rows
}

func (s *Server) handleCreatePersonalAccessTokenWeb(w http.ResponseWriter, r *http.Request) {
	user, r := s.personalAccessTokenWebUser(w, r)
	if user == nil {
		return
	}
	// Minting or revoking a credential is a sudo-mode action; enforce proof of
	// presence when the enterprise demands it.
	if s.requireProofOfPresence(w, r) {
		return
	}
	var body store.CreatePersonalAccessTokenWebRequest
	if !decodeJSONBody(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.Name) > 40 {
		store.WriteGHValidationError(w, "FineGrainedPersonalAccessToken", "name", "invalid")
		return
	}
	if body.ResourceOwner == "" {
		body.ResourceOwner = user.Login
	}
	if body.RepositorySelection == "selected" {
		body.RepositorySelection = "subset"
	}
	if body.RepositorySelection != "all" && body.RepositorySelection != "subset" && body.RepositorySelection != "none" {
		store.WriteGHValidationError(w, "FineGrainedPersonalAccessToken", "repository_selection", "invalid")
		return
	}
	if body.ExpiresAt != nil && !body.ExpiresAt.After(time.Now()) {
		store.WriteGHValidationError(w, "FineGrainedPersonalAccessToken", "expires_at", "invalid")
		return
	}
	if !validPATPermissions(body.Permissions) {
		store.WriteGHValidationError(w, "FineGrainedPersonalAccessToken", "permissions", "invalid")
		return
	}
	org := s.store.GetOrg(body.ResourceOwner)
	if body.ResourceOwner != user.Login && (org == nil || !s.viewerIsOrgMember(r.Context(), org.Login)) {
		store.WriteGHValidationError(w, "FineGrainedPersonalAccessToken", "resource_owner", "invalid")
		return
	}
	if body.RepositorySelection != "subset" && len(body.RepositoryIDs) != 0 {
		store.WriteGHValidationError(w, "FineGrainedPersonalAccessToken", "repository_ids", "invalid")
		return
	}
	for _, id := range body.RepositoryIDs {
		repo := s.store.GetRepoByID(id)
		if repo == nil || !strings.HasPrefix(repo.FullName, body.ResourceOwner+"/") || !s.viewerCanReadRepo(r.Context(), repo) {
			store.WriteGHValidationError(w, "FineGrainedPersonalAccessToken", "repository_ids", "invalid")
			return
		}
	}
	if s.store.CountFineGrainedPATs(user.ID) >= 50 {
		writeGHError(w, http.StatusUnprocessableEntity, "Fine-grained personal access token limit reached")
		return
	}
	var token *store.Token
	var err error
	if org != nil {
		request, createErr := s.store.CreateOrgPATGrantRequest(org.Login, user.ID, body.Name, body.Reason, body.RepositorySelection, body.RepositoryIDs, body.Permissions, body.ExpiresAt)
		err = createErr
		if request != nil {
			token, _ = s.store.LookupToken(request.TokenValue)
		}
	} else {
		token, err = s.store.CreateUserFineGrainedPAT(user.ID, body)
	}
	if err != nil || token == nil {
		s.logger.Error().Err(err).Msg("create fine-grained personal access token")
		writeGHError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	out := personalAccessTokenWebJSON(token, s.fineGrainedPATStatus(token))
	out["token"] = token.Value
	writeJSON(w, http.StatusCreated, out)
}

func validPATPermissions(perms store.OrgPATPermissions) bool {
	for _, group := range []map[string]string{perms.Organization, perms.Repository, perms.Other} {
		for _, level := range group {
			if !validPermLevelString(level) {
				return false
			}
		}
	}
	return true
}

func (s *Server) handleDeletePersonalAccessTokenWeb(w http.ResponseWriter, r *http.Request) {
	user, _ := s.personalAccessTokenWebUser(w, r)
	if user == nil {
		return
	}
	// Minting or revoking a credential is a sudo-mode action; enforce proof of
	// presence when the enterprise demands it.
	if s.requireProofOfPresence(w, r) {
		return
	}
	id, err := strconv.Atoi(r.PathValue("token_id"))
	if err != nil || !s.store.DeleteFineGrainedPAT(user.ID, id) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleReviewPersonalAccessTokenWeb(w http.ResponseWriter, r *http.Request) {
	user, _ := s.personalAccessTokenWebUser(w, r)
	org := s.store.GetOrg(r.PathValue("org"))
	if user == nil {
		return
	}
	if org == nil || !s.viewerCanAdminOrg(r.Context(), org.Login) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	id, err := strconv.Atoi(r.PathValue("pat_request_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if !validPATReviewAction(body.Action) {
		store.WriteGHValidationError(w, "OrganizationProgrammaticAccessGrantRequest", "action", "invalid")
		return
	}
	if !s.store.ReviewOrgPATGrantRequest(org.Login, id, body.Action == "approve") {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

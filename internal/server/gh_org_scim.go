package bleephub

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/google/uuid"
)

func (s *Server) registerGHOrganizationSCIMRoutes() {
	read := s.requirePerm(store.ScopeMembers, store.PermRead, s.handleListOrganizationSCIMUsers)
	write := func(handler http.HandlerFunc) http.HandlerFunc {
		return s.requirePerm(store.ScopeMembers, store.PermWrite, handler)
	}
	s.route("GET /api/v3/scim/v2/organizations/{org}/Users", read)
	s.route("POST /api/v3/scim/v2/organizations/{org}/Users", write(s.handleCreateOrganizationSCIMUser))
	s.route("GET /api/v3/scim/v2/organizations/{org}/Users/{scim_user_id}", write(s.handleGetOrganizationSCIMUser))
	s.route("PUT /api/v3/scim/v2/organizations/{org}/Users/{scim_user_id}", write(s.handleReplaceOrganizationSCIMUser))
	s.route("PATCH /api/v3/scim/v2/organizations/{org}/Users/{scim_user_id}", write(s.handlePatchOrganizationSCIMUser))
	s.route("DELETE /api/v3/scim/v2/organizations/{org}/Users/{scim_user_id}", write(s.handleDeleteOrganizationSCIMUser))
}

func (s *Server) resolveOrganizationSCIMAdmin(w http.ResponseWriter, r *http.Request) *store.Org {
	org, _ := s.resolveOrgOwner(w, r)
	return org
}

func (s *Server) organizationSCIMUserJSON(r *http.Request, org *store.Org, user *store.EnterpriseSCIMUser) map[string]interface{} {
	return map[string]interface{}{
		"schemas": user.Schemas, "id": user.ID, "externalId": user.ExternalID,
		"userName": user.UserName, "name": user.Name, "displayName": user.DisplayName,
		"active": user.Active, "emails": user.Emails,
		"meta": map[string]interface{}{
			"resourceType": "User",
			"created":      user.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			"lastModified": user.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			"location":     s.baseURL(r) + "/api/v3/scim/v2/organizations/" + org.Login + "/Users/" + user.ID,
		},
	}
}

func copySCIMUser(user *store.EnterpriseSCIMUser) *store.EnterpriseSCIMUser {
	if user == nil {
		return nil
	}
	result := *user
	result.Schemas = append([]string(nil), user.Schemas...)
	result.Emails = append([]store.EnterpriseSCIMEmail(nil), user.Emails...)
	return &result
}

func (s *Server) handleListOrganizationSCIMUsers(w http.ResponseWriter, r *http.Request) {
	org := s.resolveOrganizationSCIMAdmin(w, r)
	if org == nil {
		return
	}
	filter := r.URL.Query().Get("filter")
	s.store.Mu.RLock()
	users := make([]*store.EnterpriseSCIMUser, 0, len(s.store.OrgSCIMUsers[org.Login]))
	for _, user := range s.store.OrgSCIMUsers[org.Login] {
		if filter != "" {
			value, ok := scimFilterValue(filter, "userName")
			if !ok {
				value, ok = scimFilterValue(filter, "externalId")
			}
			if !ok {
				s.store.Mu.RUnlock()
				writeSCIMError(w, http.StatusBadRequest, "Unsupported filter")
				return
			}
			if user.UserName != value && user.ExternalID != value {
				continue
			}
		}
		users = append(users, copySCIMUser(user))
	}
	s.store.Mu.RUnlock()
	sort.Slice(users, func(i, j int) bool { return users[i].ID < users[j].ID })
	page, start, count, ok := scimListPage(r, users)
	if !ok {
		writeSCIMError(w, http.StatusBadRequest, "Invalid pagination")
		return
	}
	resources := make([]map[string]interface{}, len(page))
	for i, user := range page {
		resources[i] = s.organizationSCIMUserJSON(r, org, user)
	}
	writeSCIM(w, http.StatusOK, map[string]interface{}{
		"schemas": []string{scimListSchema}, "totalResults": len(users),
		"startIndex": start, "itemsPerPage": count, "Resources": resources,
	})
}

func (s *Server) createOrResolveOrganizationSCIMBackingUser(w http.ResponseWriter, org *store.Org, req *scimUserRequest) (*store.User, bool) {
	login := normalizeGitHubLogin(req.UserName)
	if login == "" {
		writeSCIMError(w, http.StatusBadRequest, "userName is required")
		return nil, false
	}
	s.store.Mu.Lock()
	if existing := s.store.UserByLoginLocked(login); existing != nil {
		// Reuse only an account this same org's SCIM provisioned. Adopting any
		// other account would let an org owner bind a SCIM record to a victim's
		// global account and rewrite their login/email.
		if existing.SCIMManagedByOrg != org.Login {
			s.store.Mu.Unlock()
			writeSCIMError(w, http.StatusConflict, "userName already exists")
			return nil, false
		}
		copy := *existing
		s.store.Mu.Unlock()
		return &copy, true
	}
	now := s.store.CurrentTime()
	userID := s.store.ReserveGlobalID("next_user", &s.store.NextUser)
	user := &store.User{
		ID: userID, NodeID: fmt.Sprintf("U_kgDO%08d", userID),
		Login: login, Name: req.DisplayName, Email: primarySCIMEmail(req.Emails), Type: "User",
		SCIMManagedByOrg: org.Login,
		StarredRepos:     map[string]time.Time{}, CreatedAt: now, UpdatedAt: now,
	}
	s.store.Users[user.ID] = user
	s.store.UsersByLogin[user.Login] = user
	s.store.IndexUserLoginLocked(user.Login)
	if s.store.Persist != nil {
		s.store.Persist.MustPut("users", strconv.Itoa(user.ID), user)
	}
	copy := *user
	s.store.Mu.Unlock()
	return &copy, true
}

func (s *Server) setOrganizationSCIMMembershipLocked(org *store.Org, userID int, active bool) {
	key := store.MembershipKey(org.Login, userID)
	if !active {
		delete(s.store.Memberships, key)
		if s.store.Persist != nil {
			s.store.Persist.MustDelete("memberships", key)
		}
		return
	}
	membership := s.store.Memberships[key]
	if membership == nil {
		membership = &store.Membership{OrgID: org.ID, UserID: userID, Role: store.OrgRoleMember}
		s.store.Memberships[key] = membership
	}
	membership.State = store.MembershipStateActive
	if s.store.Persist != nil {
		s.store.Persist.MustPut("memberships", key, membership)
	}
}

func (s *Server) handleCreateOrganizationSCIMUser(w http.ResponseWriter, r *http.Request) {
	org := s.resolveOrganizationSCIMAdmin(w, r)
	if org == nil {
		return
	}
	var req scimUserRequest
	if !decodeSCIMBody(w, r, &req) {
		return
	}
	backing, ok := s.createOrResolveOrganizationSCIMBackingUser(w, org, &req)
	if !ok {
		return
	}
	now := s.currentTime()
	active := req.Active == nil || *req.Active
	user := &store.EnterpriseSCIMUser{
		Schemas: []string{scimUserSchema}, ID: uuid.NewString(), ExternalID: req.ExternalID,
		UserName: backing.Login, Name: req.Name, DisplayName: req.DisplayName,
		Active: active, Emails: append([]store.EnterpriseSCIMEmail(nil), req.Emails...),
		UserID: backing.ID, CreatedAt: now, UpdatedAt: now,
	}
	s.store.Mu.Lock()
	if s.store.OrgSCIMUsers[org.Login] == nil {
		s.store.OrgSCIMUsers[org.Login] = map[string]*store.EnterpriseSCIMUser{}
	}
	for _, existing := range s.store.OrgSCIMUsers[org.Login] {
		if strings.EqualFold(existing.UserName, user.UserName) ||
			(user.ExternalID != "" && existing.ExternalID == user.ExternalID) {
			s.store.Mu.Unlock()
			writeSCIMError(w, http.StatusConflict, "Identity already exists")
			return
		}
	}
	s.store.OrgSCIMUsers[org.Login][user.ID] = user
	s.setOrganizationSCIMMembershipLocked(org, user.UserID, active)
	if s.store.Persist != nil {
		s.store.Persist.MustPut("org_scim_users", org.Login, s.store.OrgSCIMUsers[org.Login])
	}
	s.store.Mu.Unlock()
	w.Header().Set("Location", s.organizationSCIMUserJSON(r, org, user)["meta"].(map[string]interface{})["location"].(string))
	writeSCIM(w, http.StatusCreated, s.organizationSCIMUserJSON(r, org, user))
}

func (s *Server) organizationSCIMUser(w http.ResponseWriter, r *http.Request, org *store.Org) *store.EnterpriseSCIMUser {
	s.store.Mu.RLock()
	user := copySCIMUser(s.store.OrgSCIMUsers[org.Login][r.PathValue("scim_user_id")])
	s.store.Mu.RUnlock()
	if user == nil {
		writeSCIMError(w, http.StatusNotFound, "Resource not found")
	}
	return user
}

func (s *Server) handleGetOrganizationSCIMUser(w http.ResponseWriter, r *http.Request) {
	org := s.resolveOrganizationSCIMAdmin(w, r)
	if org == nil {
		return
	}
	if user := s.organizationSCIMUser(w, r, org); user != nil {
		writeSCIM(w, http.StatusOK, s.organizationSCIMUserJSON(r, org, user))
	}
}

func (s *Server) replaceOrganizationSCIMUser(w http.ResponseWriter, r *http.Request, org *store.Org, req *scimUserRequest) *store.EnterpriseSCIMUser {
	id := r.PathValue("scim_user_id")
	login := normalizeGitHubLogin(req.UserName)
	if login == "" {
		writeSCIMError(w, http.StatusBadRequest, "userName is required")
		return nil
	}
	s.store.Mu.Lock()
	user := s.store.OrgSCIMUsers[org.Login][id]
	if user == nil {
		s.store.Mu.Unlock()
		writeSCIMError(w, http.StatusNotFound, "Resource not found")
		return nil
	}
	for otherID, existing := range s.store.OrgSCIMUsers[org.Login] {
		if otherID != id && (strings.EqualFold(existing.UserName, login) ||
			(req.ExternalID != "" && existing.ExternalID == req.ExternalID)) {
			s.store.Mu.Unlock()
			writeSCIMError(w, http.StatusConflict, "Identity already exists")
			return nil
		}
	}
	backing := s.store.Users[user.UserID]
	if backing == nil {
		s.store.Mu.Unlock()
		writeSCIMError(w, http.StatusInternalServerError, "Backing user is missing")
		return nil
	}
	// Never rewrite the global identity of an account this org's SCIM does not
	// own; an org owner must not rename or re-home an arbitrary account here.
	if backing.SCIMManagedByOrg != org.Login {
		s.store.Mu.Unlock()
		writeSCIMError(w, http.StatusConflict, "userName is not managed by this organization")
		return nil
	}
	if other := s.store.UserByLoginLocked(login); other != nil && other.ID != backing.ID {
		s.store.Mu.Unlock()
		writeSCIMError(w, http.StatusConflict, "userName already exists")
		return nil
	}
	delete(s.store.UsersByLogin, backing.Login)
	s.store.UnindexUserLoginLocked(backing.Login)
	backing.Login, backing.Name, backing.Email = login, req.DisplayName, primarySCIMEmail(req.Emails)
	backing.UpdatedAt = s.currentTime()
	s.store.UsersByLogin[backing.Login] = backing
	s.store.IndexUserLoginLocked(backing.Login)
	active := req.Active == nil || *req.Active
	user.ExternalID, user.UserName, user.Name, user.DisplayName = req.ExternalID, login, req.Name, req.DisplayName
	user.Active, user.Emails, user.UpdatedAt = active, append([]store.EnterpriseSCIMEmail(nil), req.Emails...), backing.UpdatedAt
	s.setOrganizationSCIMMembershipLocked(org, backing.ID, active)
	// One transaction: the renamed backing account and the org's SCIM-user record
	// commit together, so a crash cannot leave them disagreeing (STORE-001/002).
	batch := store.NewPersistBatch(s.store.Persist)
	batch.Put("users", strconv.Itoa(backing.ID), backing)
	batch.Put("org_scim_users", org.Login, s.store.OrgSCIMUsers[org.Login])
	commitErr := batch.Commit()
	result := copySCIMUser(user)
	s.store.Mu.Unlock()
	if commitErr != nil {
		panic(&store.PersistenceFailure{Op: "batch", Bucket: "users", Err: commitErr})
	}
	return result
}

func (s *Server) handleReplaceOrganizationSCIMUser(w http.ResponseWriter, r *http.Request) {
	org := s.resolveOrganizationSCIMAdmin(w, r)
	if org == nil {
		return
	}
	var req scimUserRequest
	if !decodeSCIMBody(w, r, &req) {
		return
	}
	if user := s.replaceOrganizationSCIMUser(w, r, org, &req); user != nil {
		writeSCIM(w, http.StatusOK, s.organizationSCIMUserJSON(r, org, user))
	}
}

func (s *Server) handlePatchOrganizationSCIMUser(w http.ResponseWriter, r *http.Request) {
	org := s.resolveOrganizationSCIMAdmin(w, r)
	if org == nil {
		return
	}
	current := s.organizationSCIMUser(w, r, org)
	if current == nil {
		return
	}
	req := scimUserRequest{
		ExternalID: current.ExternalID, UserName: current.UserName, Name: current.Name,
		DisplayName: current.DisplayName, Active: &current.Active, Emails: current.Emails,
	}
	var patch scimPatchRequest
	if !decodeSCIMBody(w, r, &patch) {
		return
	}
	for _, operation := range patch.Operations {
		op := strings.ToLower(operation.Op)
		if op != "add" && op != "replace" && op != "remove" {
			writeSCIMError(w, http.StatusBadRequest, "Unsupported patch operation")
			return
		}
		if operation.Path == "" {
			fields, ok := operation.Value.(map[string]interface{})
			if !ok || op == "remove" {
				writeSCIMError(w, http.StatusBadRequest, "Pathless patch value must be an object")
				return
			}
			for path, value := range fields {
				if !applySCIMUserField(w, &req, op, path, value) {
					return
				}
			}
		} else if !applySCIMUserField(w, &req, op, operation.Path, operation.Value) {
			return
		}
	}
	if user := s.replaceOrganizationSCIMUser(w, r, org, &req); user != nil {
		writeSCIM(w, http.StatusOK, s.organizationSCIMUserJSON(r, org, user))
	}
}

func (s *Server) handleDeleteOrganizationSCIMUser(w http.ResponseWriter, r *http.Request) {
	org := s.resolveOrganizationSCIMAdmin(w, r)
	if org == nil {
		return
	}
	id := r.PathValue("scim_user_id")
	s.store.Mu.Lock()
	user := s.store.OrgSCIMUsers[org.Login][id]
	if user == nil {
		s.store.Mu.Unlock()
		writeSCIMError(w, http.StatusNotFound, "Resource not found")
		return
	}
	delete(s.store.OrgSCIMUsers[org.Login], id)
	s.setOrganizationSCIMMembershipLocked(org, user.UserID, false)
	if s.store.Persist != nil {
		s.store.Persist.MustPut("org_scim_users", org.Login, s.store.OrgSCIMUsers[org.Login])
	}
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

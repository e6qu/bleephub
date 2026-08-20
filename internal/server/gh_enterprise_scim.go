package bleephub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/google/uuid"
)

const (
	scimUserSchema  = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimGroupSchema = "urn:ietf:params:scim:schemas:core:2.0:Group"
	scimListSchema  = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
)

func (s *Server) registerGHEnterpriseSCIMRoutes() {
	s.route("GET /api/v3/scim/v2/enterprises/{enterprise}/Users", s.requireEnterpriseOwner(s.handleListEnterpriseSCIMUsers))
	s.route("POST /api/v3/scim/v2/enterprises/{enterprise}/Users", s.requireEnterpriseOwner(s.handleCreateEnterpriseSCIMUser))
	s.route("GET /api/v3/scim/v2/enterprises/{enterprise}/Users/{scim_user_id}", s.requireEnterpriseOwner(s.handleGetEnterpriseSCIMUser))
	s.route("PUT /api/v3/scim/v2/enterprises/{enterprise}/Users/{scim_user_id}", s.requireEnterpriseOwner(s.handleReplaceEnterpriseSCIMUser))
	s.route("PATCH /api/v3/scim/v2/enterprises/{enterprise}/Users/{scim_user_id}", s.requireEnterpriseOwner(s.handlePatchEnterpriseSCIMUser))
	s.route("DELETE /api/v3/scim/v2/enterprises/{enterprise}/Users/{scim_user_id}", s.requireEnterpriseOwner(s.handleDeleteEnterpriseSCIMUser))

	s.route("GET /api/v3/scim/v2/enterprises/{enterprise}/Groups", s.requireEnterpriseOwner(s.handleListEnterpriseSCIMGroups))
	s.route("POST /api/v3/scim/v2/enterprises/{enterprise}/Groups", s.requireEnterpriseOwner(s.handleCreateEnterpriseSCIMGroup))
	s.route("GET /api/v3/scim/v2/enterprises/{enterprise}/Groups/{scim_group_id}", s.requireEnterpriseOwner(s.handleGetEnterpriseSCIMGroup))
	s.route("PUT /api/v3/scim/v2/enterprises/{enterprise}/Groups/{scim_group_id}", s.requireEnterpriseOwner(s.handleReplaceEnterpriseSCIMGroup))
	s.route("PATCH /api/v3/scim/v2/enterprises/{enterprise}/Groups/{scim_group_id}", s.requireEnterpriseOwner(s.handlePatchEnterpriseSCIMGroup))
	s.route("DELETE /api/v3/scim/v2/enterprises/{enterprise}/Groups/{scim_group_id}", s.requireEnterpriseOwner(s.handleDeleteEnterpriseSCIMGroup))
}

func (s *Server) scimMeta(r *http.Request, resourceType, id string, created, updated time.Time) map[string]interface{} {
	return map[string]interface{}{
		"resourceType": resourceType, "created": created.Format(time.RFC3339),
		"lastModified": updated.Format(time.RFC3339),
		"location": s.baseURL(r) + "/api/v3/scim/v2/enterprises/" + s.enterpriseSlug() +
			"/" + resourceType + "s/" + id,
	}
}

func (s *Server) scimUserJSON(r *http.Request, user *store.EnterpriseSCIMUser) map[string]interface{} {
	return map[string]interface{}{
		"schemas": user.Schemas, "id": user.ID, "externalId": user.ExternalID,
		"userName": user.UserName, "name": user.Name, "displayName": user.DisplayName,
		"active": user.Active, "emails": user.Emails,
		"meta": s.scimMeta(r, "User", user.ID, user.CreatedAt, user.UpdatedAt),
	}
}

func (s *Server) scimGroupJSON(r *http.Request, group *store.EnterpriseSCIMGroup) map[string]interface{} {
	return map[string]interface{}{
		"schemas": group.Schemas, "id": group.ID, "externalId": group.ExternalID,
		"displayName": group.DisplayName, "members": group.Members,
		"meta": s.scimMeta(r, "Group", group.ID, group.CreatedAt, group.UpdatedAt),
	}
}

func writeSCIMError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/scim+json")
	writeJSON(w, status, map[string]interface{}{
		"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:Error"},
		"status":  strconv.Itoa(status), "detail": detail,
	})
}

func writeSCIM(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/scim+json")
	writeJSON(w, status, value)
}

func decodeSCIMBody(w http.ResponseWriter, r *http.Request, value interface{}) bool {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := decoder.Decode(value); err != nil {
		writeSCIMError(w, http.StatusBadRequest, "Invalid JSON")
		return false
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeSCIMError(w, http.StatusBadRequest, "Invalid JSON")
		return false
	}
	return true
}

func scimListPage[T any](r *http.Request, rows []T) ([]T, int, int, bool) {
	start, count := 1, 100
	var err error
	if raw := r.URL.Query().Get("startIndex"); raw != "" {
		start, err = strconv.Atoi(raw)
		if err != nil || start < 1 {
			return nil, 0, 0, false
		}
	}
	if raw := r.URL.Query().Get("count"); raw != "" {
		count, err = strconv.Atoi(raw)
		if err != nil || count < 0 {
			return nil, 0, 0, false
		}
	}
	from := min(start-1, len(rows))
	to := min(from+count, len(rows))
	return rows[from:to], start, to - from, true
}

func scimFilterValue(raw, field string) (string, bool) {
	if raw == "" {
		return "", true
	}
	prefix := field + ` eq "`
	if !strings.HasPrefix(raw, prefix) || !strings.HasSuffix(raw, `"`) {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(raw, prefix), `"`), true
}

func (s *Server) handleListEnterpriseSCIMUsers(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("filter")
	s.store.Mu.RLock()
	users := make([]*store.EnterpriseSCIMUser, 0, len(s.store.EnterpriseSettings.SCIMUsers))
	for _, user := range s.store.EnterpriseSettings.SCIMUsers {
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
		copy := *user
		users = append(users, &copy)
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
		resources[i] = s.scimUserJSON(r, user)
	}
	writeSCIM(w, http.StatusOK, map[string]interface{}{
		"schemas": []string{scimListSchema}, "totalResults": len(users),
		"startIndex": start, "itemsPerPage": count, "Resources": resources,
	})
}

type scimUserRequest struct {
	Schemas     []string                    `json:"schemas"`
	ExternalID  string                      `json:"externalId"`
	UserName    string                      `json:"userName"`
	Name        store.EnterpriseSCIMName    `json:"name"`
	DisplayName string                      `json:"displayName"`
	Active      *bool                       `json:"active"`
	Emails      []store.EnterpriseSCIMEmail `json:"emails"`
}

func (s *Server) createSCIMBackingUser(w http.ResponseWriter, req *scimUserRequest) (*store.User, bool) {
	login := normalizeGitHubLogin(req.UserName)
	if login == "" {
		writeSCIMError(w, http.StatusBadRequest, "userName is required")
		return nil, false
	}
	email := ""
	for _, candidate := range req.Emails {
		if candidate.Primary || email == "" {
			email = candidate.Value
		}
	}
	s.store.Mu.Lock()
	if s.store.UserByLoginLocked(login) != nil {
		s.store.Mu.Unlock()
		writeSCIMError(w, http.StatusConflict, "userName already exists")
		return nil, false
	}
	now := s.store.CurrentTime()
	active := req.Active == nil || *req.Active
	userID := s.store.ReserveGlobalID("next_user", &s.store.NextUser)
	user := &store.User{
		ID: userID, NodeID: fmt.Sprintf("U_kgDO%08d", userID),
		Login: login, Name: req.DisplayName, Email: email, Type: "User",
		Suspended: !active, StarredRepos: map[string]bool{}, CreatedAt: now, UpdatedAt: now,
	}
	s.store.Users[user.ID] = user
	s.store.UsersByLogin[user.Login] = user
	s.store.IndexUserLoginLocked(user.Login)
	if s.store.Persist != nil {
		s.store.Persist.MustPut("users", strconv.Itoa(user.ID), user)
	}
	s.store.Mu.Unlock()
	return user, true
}

func (s *Server) handleCreateEnterpriseSCIMUser(w http.ResponseWriter, r *http.Request) {
	var req scimUserRequest
	if !decodeSCIMBody(w, r, &req) {
		return
	}
	backing, ok := s.createSCIMBackingUser(w, &req)
	if !ok {
		return
	}
	now := s.store.CurrentTime()
	active := req.Active == nil || *req.Active
	user := &store.EnterpriseSCIMUser{
		Schemas: []string{scimUserSchema}, ID: uuid.NewString(), ExternalID: req.ExternalID,
		UserName: backing.Login, Name: req.Name, DisplayName: req.DisplayName,
		Active: active, Emails: append([]store.EnterpriseSCIMEmail(nil), req.Emails...),
		UserID: backing.ID, CreatedAt: now, UpdatedAt: now,
	}
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.SCIMUsers[user.ID] = user
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	writeSCIM(w, http.StatusCreated, s.scimUserJSON(r, user))
}

func (s *Server) enterpriseSCIMUser(w http.ResponseWriter, r *http.Request) *store.EnterpriseSCIMUser {
	s.store.Mu.RLock()
	user := s.store.EnterpriseSettings.SCIMUsers[r.PathValue("scim_user_id")]
	if user != nil {
		copy := *user
		user = &copy
	}
	s.store.Mu.RUnlock()
	if user == nil {
		writeSCIMError(w, http.StatusNotFound, "Resource not found")
	}
	return user
}

func (s *Server) handleGetEnterpriseSCIMUser(w http.ResponseWriter, r *http.Request) {
	user := s.enterpriseSCIMUser(w, r)
	if user != nil {
		writeSCIM(w, http.StatusOK, s.scimUserJSON(r, user))
	}
}

func (s *Server) replaceSCIMUser(w http.ResponseWriter, r *http.Request, req *scimUserRequest) *store.EnterpriseSCIMUser {
	id := r.PathValue("scim_user_id")
	s.store.Mu.Lock()
	user := s.store.EnterpriseSettings.SCIMUsers[id]
	if user == nil {
		s.store.Mu.Unlock()
		writeSCIMError(w, http.StatusNotFound, "Resource not found")
		return nil
	}
	backing := s.store.Users[user.UserID]
	if backing == nil {
		s.store.Mu.Unlock()
		writeSCIMError(w, http.StatusInternalServerError, "Backing user is missing")
		return nil
	}
	login := normalizeGitHubLogin(req.UserName)
	if login == "" || (login != backing.Login && s.store.UserByLoginLocked(login) != nil) {
		s.store.Mu.Unlock()
		writeSCIMError(w, http.StatusConflict, "userName already exists")
		return nil
	}
	delete(s.store.UsersByLogin, backing.Login)
	s.store.UnindexUserLoginLocked(backing.Login)
	backing.Login = login
	backing.Name = req.DisplayName
	backing.Email = primarySCIMEmail(req.Emails)
	backing.Suspended = req.Active != nil && !*req.Active
	backing.UpdatedAt = s.store.CurrentTime()
	s.store.UsersByLogin[login] = backing
	s.store.IndexUserLoginLocked(login)
	user.ExternalID, user.UserName, user.Name, user.DisplayName = req.ExternalID, login, req.Name, req.DisplayName
	user.Active = req.Active == nil || *req.Active
	user.Emails = append([]store.EnterpriseSCIMEmail(nil), req.Emails...)
	user.UpdatedAt = backing.UpdatedAt
	if s.store.Persist != nil {
		s.store.Persist.MustPut("users", strconv.Itoa(backing.ID), backing)
	}
	s.store.PersistEnterpriseSettings()
	copy := *user
	s.store.Mu.Unlock()
	return &copy
}

func (s *Server) handleReplaceEnterpriseSCIMUser(w http.ResponseWriter, r *http.Request) {
	var req scimUserRequest
	if !decodeSCIMBody(w, r, &req) {
		return
	}
	if user := s.replaceSCIMUser(w, r, &req); user != nil {
		writeSCIM(w, http.StatusOK, s.scimUserJSON(r, user))
	}
}

type scimPatchRequest struct {
	Operations []struct {
		Op    string      `json:"op"`
		Path  string      `json:"path"`
		Value interface{} `json:"value"`
	} `json:"Operations"`
}

func primarySCIMEmail(emails []store.EnterpriseSCIMEmail) string {
	email := ""
	for _, candidate := range emails {
		if candidate.Primary || email == "" {
			email = candidate.Value
		}
	}
	return email
}

func scimString(value interface{}) (string, bool) {
	stringValue, ok := value.(string)
	return stringValue, ok
}

func decodeSCIMEmails(value interface{}) ([]store.EnterpriseSCIMEmail, bool) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var emails []store.EnterpriseSCIMEmail
	if err := json.Unmarshal(encoded, &emails); err != nil {
		return nil, false
	}
	for _, email := range emails {
		if strings.TrimSpace(email.Value) == "" {
			return nil, false
		}
	}
	return emails, true
}

func applySCIMUserField(w http.ResponseWriter, req *scimUserRequest, op, path string, value interface{}) bool {
	switch strings.ToLower(path) {
	case "username":
		if op == "remove" {
			writeSCIMError(w, http.StatusBadRequest, "userName cannot be removed")
			return false
		}
		stringValue, ok := scimString(value)
		if !ok {
			writeSCIMError(w, http.StatusBadRequest, "userName must be a string")
			return false
		}
		req.UserName = stringValue
	case "active":
		if op == "remove" {
			value = true
		}
		boolValue, ok := value.(bool)
		if !ok {
			writeSCIMError(w, http.StatusBadRequest, "active must be boolean")
			return false
		}
		req.Active = &boolValue
	case "displayname":
		req.DisplayName = ""
		if op != "remove" {
			stringValue, ok := scimString(value)
			if !ok {
				writeSCIMError(w, http.StatusBadRequest, "displayName must be a string")
				return false
			}
			req.DisplayName = stringValue
		}
	case "externalid":
		req.ExternalID = ""
		if op != "remove" {
			stringValue, ok := scimString(value)
			if !ok {
				writeSCIMError(w, http.StatusBadRequest, "externalId must be a string")
				return false
			}
			req.ExternalID = stringValue
		}
	case "name.givenname":
		req.Name.GivenName = ""
		if op != "remove" {
			stringValue, ok := scimString(value)
			if !ok {
				writeSCIMError(w, http.StatusBadRequest, "name.givenName must be a string")
				return false
			}
			req.Name.GivenName = stringValue
		}
	case "name.familyname":
		req.Name.FamilyName = ""
		if op != "remove" {
			stringValue, ok := scimString(value)
			if !ok {
				writeSCIMError(w, http.StatusBadRequest, "name.familyName must be a string")
				return false
			}
			req.Name.FamilyName = stringValue
		}
	case "emails":
		if op == "remove" {
			req.Emails = nil
			return true
		}
		emails, ok := decodeSCIMEmails(value)
		if !ok {
			writeSCIMError(w, http.StatusBadRequest, "emails must be an array")
			return false
		}
		if op == "add" {
			req.Emails = append(req.Emails, emails...)
		} else {
			req.Emails = emails
		}
	default:
		writeSCIMError(w, http.StatusBadRequest, "Unsupported patch path")
		return false
	}
	return true
}

func (s *Server) handlePatchEnterpriseSCIMUser(w http.ResponseWriter, r *http.Request) {
	current := s.enterpriseSCIMUser(w, r)
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
			continue
		}
		if !applySCIMUserField(w, &req, op, operation.Path, operation.Value) {
			return
		}
	}
	if user := s.replaceSCIMUser(w, r, &req); user != nil {
		writeSCIM(w, http.StatusOK, s.scimUserJSON(r, user))
	}
}

func (s *Server) handleDeleteEnterpriseSCIMUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("scim_user_id")
	s.store.Mu.Lock()
	user := s.store.EnterpriseSettings.SCIMUsers[id]
	if user == nil {
		s.store.Mu.Unlock()
		writeSCIMError(w, http.StatusNotFound, "Resource not found")
		return
	}
	if backing := s.store.Users[user.UserID]; backing != nil {
		backing.Suspended = true
		backing.UpdatedAt = s.store.CurrentTime()
		if s.store.Persist != nil {
			s.store.Persist.MustPut("users", strconv.Itoa(backing.ID), backing)
		}
	}
	delete(s.store.EnterpriseSettings.SCIMUsers, id)
	for _, group := range s.store.EnterpriseSettings.SCIMGroups {
		group.Members = removeSCIMMember(group.Members, id)
		group.UpdatedAt = s.store.CurrentTime()
		s.syncSCIMGroupTeamLocked(group)
	}
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func removeSCIMMember(members []store.EnterpriseSCIMMember, id string) []store.EnterpriseSCIMMember {
	out := members[:0]
	for _, member := range members {
		if member.Value != id {
			out = append(out, member)
		}
	}
	return out
}

type scimGroupRequest struct {
	ExternalID  string                       `json:"externalId"`
	DisplayName string                       `json:"displayName"`
	Members     []store.EnterpriseSCIMMember `json:"members"`
}

func (s *Server) validateSCIMMembers(w http.ResponseWriter, members []store.EnterpriseSCIMMember) bool {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	seen := map[string]bool{}
	for _, member := range members {
		if s.store.EnterpriseSettings.SCIMUsers[member.Value] == nil || seen[member.Value] {
			writeSCIMError(w, http.StatusBadRequest, "Invalid group member")
			return false
		}
		seen[member.Value] = true
	}
	return true
}

func (s *Server) syncSCIMGroupTeamLocked(group *store.EnterpriseSCIMGroup) {
	team := s.store.EnterpriseTeams[group.TeamID]
	if team == nil {
		return
	}
	oldSlug := team.Slug
	team.Name = group.DisplayName
	team.Slug = store.Slugify(group.DisplayName)
	if oldSlug != team.Slug {
		delete(s.store.EnterpriseTeamsBySlug, oldSlug)
		s.store.EnterpriseTeamsBySlug[team.Slug] = team
	}
	team.MemberIDs = team.MemberIDs[:0:0]
	for _, member := range group.Members {
		if user := s.store.EnterpriseSettings.SCIMUsers[member.Value]; user != nil {
			team.MemberIDs = append(team.MemberIDs, user.UserID)
		}
	}
	team.UpdatedAt = group.UpdatedAt
	s.store.PersistEnterpriseTeam(team)
}

func (s *Server) handleCreateEnterpriseSCIMGroup(w http.ResponseWriter, r *http.Request) {
	var req scimGroupRequest
	if !decodeSCIMBody(w, r, &req) {
		return
	}
	if req.DisplayName == "" || !s.validateSCIMMembers(w, req.Members) {
		if req.DisplayName == "" {
			writeSCIMError(w, http.StatusBadRequest, "displayName is required")
		}
		return
	}
	id := uuid.NewString()
	team := s.store.CreateEnterpriseTeam(req.DisplayName, "", "disabled", &id, "")
	if team == nil {
		writeSCIMError(w, http.StatusConflict, "displayName already exists")
		return
	}
	now := s.store.CurrentTime()
	group := &store.EnterpriseSCIMGroup{
		Schemas: []string{scimGroupSchema}, ID: id, ExternalID: req.ExternalID,
		DisplayName: req.DisplayName, Members: append([]store.EnterpriseSCIMMember(nil), req.Members...),
		TeamID: team.ID, CreatedAt: now, UpdatedAt: now,
	}
	s.store.Mu.Lock()
	s.store.EnterpriseSettings.SCIMGroups[id] = group
	s.syncSCIMGroupTeamLocked(group)
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	writeSCIM(w, http.StatusCreated, s.scimGroupJSON(r, group))
}

func (s *Server) enterpriseSCIMGroup(w http.ResponseWriter, r *http.Request) *store.EnterpriseSCIMGroup {
	s.store.Mu.RLock()
	group := s.store.EnterpriseSettings.SCIMGroups[r.PathValue("scim_group_id")]
	if group != nil {
		copy := *group
		copy.Members = append([]store.EnterpriseSCIMMember(nil), group.Members...)
		group = &copy
	}
	s.store.Mu.RUnlock()
	if group == nil {
		writeSCIMError(w, http.StatusNotFound, "Resource not found")
	}
	return group
}

func (s *Server) handleGetEnterpriseSCIMGroup(w http.ResponseWriter, r *http.Request) {
	if group := s.enterpriseSCIMGroup(w, r); group != nil {
		writeSCIM(w, http.StatusOK, s.scimGroupJSON(r, group))
	}
}

func (s *Server) handleListEnterpriseSCIMGroups(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("filter")
	filterValue := ""
	if filter != "" {
		var ok bool
		filterValue, ok = scimFilterValue(filter, "displayName")
		if !ok {
			writeSCIMError(w, http.StatusBadRequest, "Unsupported filter")
			return
		}
	}
	s.store.Mu.RLock()
	groups := make([]*store.EnterpriseSCIMGroup, 0, len(s.store.EnterpriseSettings.SCIMGroups))
	for _, group := range s.store.EnterpriseSettings.SCIMGroups {
		if filter != "" {
			if group.DisplayName != filterValue {
				continue
			}
		}
		copy := *group
		copy.Members = append([]store.EnterpriseSCIMMember(nil), group.Members...)
		groups = append(groups, &copy)
	}
	s.store.Mu.RUnlock()
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	page, start, count, ok := scimListPage(r, groups)
	if !ok {
		writeSCIMError(w, http.StatusBadRequest, "Invalid pagination")
		return
	}
	resources := make([]map[string]interface{}, len(page))
	for i, group := range page {
		resources[i] = s.scimGroupJSON(r, group)
	}
	writeSCIM(w, http.StatusOK, map[string]interface{}{
		"schemas": []string{scimListSchema}, "totalResults": len(groups),
		"startIndex": start, "itemsPerPage": count, "Resources": resources,
	})
}

func (s *Server) replaceSCIMGroup(w http.ResponseWriter, r *http.Request, req *scimGroupRequest) *store.EnterpriseSCIMGroup {
	if req.DisplayName == "" || !s.validateSCIMMembers(w, req.Members) {
		if req.DisplayName == "" {
			writeSCIMError(w, http.StatusBadRequest, "displayName is required")
		}
		return nil
	}
	s.store.Mu.Lock()
	group := s.store.EnterpriseSettings.SCIMGroups[r.PathValue("scim_group_id")]
	if group == nil {
		s.store.Mu.Unlock()
		writeSCIMError(w, http.StatusNotFound, "Resource not found")
		return nil
	}
	newSlug := store.Slugify(req.DisplayName)
	if other := s.store.EnterpriseTeamsBySlug[newSlug]; other != nil && other.ID != group.TeamID {
		s.store.Mu.Unlock()
		writeSCIMError(w, http.StatusConflict, "displayName already exists")
		return nil
	}
	group.ExternalID, group.DisplayName = req.ExternalID, req.DisplayName
	group.Members = append([]store.EnterpriseSCIMMember(nil), req.Members...)
	group.UpdatedAt = s.store.CurrentTime()
	s.syncSCIMGroupTeamLocked(group)
	s.store.PersistEnterpriseSettings()
	copy := *group
	copy.Members = append([]store.EnterpriseSCIMMember(nil), group.Members...)
	s.store.Mu.Unlock()
	return &copy
}

func (s *Server) handleReplaceEnterpriseSCIMGroup(w http.ResponseWriter, r *http.Request) {
	var req scimGroupRequest
	if !decodeSCIMBody(w, r, &req) {
		return
	}
	if group := s.replaceSCIMGroup(w, r, &req); group != nil {
		writeSCIM(w, http.StatusOK, s.scimGroupJSON(r, group))
	}
}

func decodeSCIMMembers(value interface{}) ([]store.EnterpriseSCIMMember, bool) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var members []store.EnterpriseSCIMMember
	if err := json.Unmarshal(encoded, &members); err != nil {
		return nil, false
	}
	for _, member := range members {
		if member.Value == "" {
			return nil, false
		}
	}
	return members, true
}

func addSCIMMembers(existing, added []store.EnterpriseSCIMMember) []store.EnterpriseSCIMMember {
	seen := make(map[string]bool, len(existing)+len(added))
	out := make([]store.EnterpriseSCIMMember, 0, len(existing)+len(added))
	for _, member := range append(existing, added...) {
		if !seen[member.Value] {
			seen[member.Value] = true
			out = append(out, member)
		}
	}
	return out
}

func scimMemberFilter(path string) (string, bool) {
	const prefix = `members[value eq "`
	if !strings.HasPrefix(strings.ToLower(path), strings.ToLower(prefix)) || !strings.HasSuffix(path, `"]`) {
		return "", false
	}
	return strings.TrimSuffix(path[len(prefix):], `"]`), true
}

func applySCIMGroupField(w http.ResponseWriter, req *scimGroupRequest, op, path string, value interface{}) bool {
	switch strings.ToLower(path) {
	case "displayname":
		if op == "remove" {
			writeSCIMError(w, http.StatusBadRequest, "displayName cannot be removed")
			return false
		}
		stringValue, ok := scimString(value)
		if !ok {
			writeSCIMError(w, http.StatusBadRequest, "displayName must be a string")
			return false
		}
		req.DisplayName = stringValue
	case "externalid":
		req.ExternalID = ""
		if op != "remove" {
			stringValue, ok := scimString(value)
			if !ok {
				writeSCIMError(w, http.StatusBadRequest, "externalId must be a string")
				return false
			}
			req.ExternalID = stringValue
		}
	case "members":
		if op == "remove" && value == nil {
			req.Members = nil
			return true
		}
		members, ok := decodeSCIMMembers(value)
		if !ok {
			writeSCIMError(w, http.StatusBadRequest, "members must be an array")
			return false
		}
		switch op {
		case "add":
			req.Members = addSCIMMembers(req.Members, members)
		case "remove":
			for _, member := range members {
				req.Members = removeSCIMMember(req.Members, member.Value)
			}
		default:
			req.Members = members
		}
	default:
		memberID, ok := scimMemberFilter(path)
		if !ok || op != "remove" {
			writeSCIMError(w, http.StatusBadRequest, "Unsupported patch path")
			return false
		}
		req.Members = removeSCIMMember(req.Members, memberID)
	}
	return true
}

func (s *Server) handlePatchEnterpriseSCIMGroup(w http.ResponseWriter, r *http.Request) {
	current := s.enterpriseSCIMGroup(w, r)
	if current == nil {
		return
	}
	req := scimGroupRequest{ExternalID: current.ExternalID, DisplayName: current.DisplayName, Members: current.Members}
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
				if !applySCIMGroupField(w, &req, op, path, value) {
					return
				}
			}
			continue
		}
		if !applySCIMGroupField(w, &req, op, operation.Path, operation.Value) {
			return
		}
	}
	if group := s.replaceSCIMGroup(w, r, &req); group != nil {
		writeSCIM(w, http.StatusOK, s.scimGroupJSON(r, group))
	}
}

func (s *Server) handleDeleteEnterpriseSCIMGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("scim_group_id")
	s.store.Mu.Lock()
	group := s.store.EnterpriseSettings.SCIMGroups[id]
	if group == nil {
		s.store.Mu.Unlock()
		writeSCIMError(w, http.StatusNotFound, "Resource not found")
		return
	}
	delete(s.store.EnterpriseSettings.SCIMGroups, id)
	if team := s.store.EnterpriseTeams[group.TeamID]; team != nil {
		delete(s.store.EnterpriseTeamsBySlug, team.Slug)
	}
	delete(s.store.EnterpriseTeams, group.TeamID)
	if s.store.Persist != nil {
		s.store.Persist.MustDelete("enterprise_teams", strconv.Itoa(group.TeamID))
	}
	s.store.PersistEnterpriseSettings()
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

package bleephub

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	reviewKindPushBypass      = "push-rules"
	reviewKindSecretBypass    = "secret-scanning-bypass"
	reviewKindCodeDismissal   = "code-scanning"
	reviewKindDependabot      = "dependabot"
	reviewKindSecretDismissal = "secret-scanning-dismissal"
)

func securityReviewScope(repoKey, kind string) string { return repoKey + "|" + kind }

func (s *Server) registerGHSecurityReviewRoutes() {
	read := func(handler http.HandlerFunc) http.HandlerFunc {
		return s.requirePerm(scopeSecurityEvents, permRead, handler)
	}
	write := func(handler http.HandlerFunc) http.HandlerFunc {
		return s.requirePerm(scopeSecurityEvents, permWrite, handler)
	}

	s.route("GET /api/v3/orgs/{org}/bypass-requests/push-rules", s.requireOrgAdmin(scopeSecurityEvents, permRead, s.orgSecurityReviewList(reviewKindPushBypass)))
	s.route("GET /api/v3/orgs/{org}/bypass-requests/secret-scanning", s.requireOrgAdmin(scopeSecurityEvents, permRead, s.orgSecurityReviewList(reviewKindSecretBypass)))
	s.route("GET /api/v3/orgs/{org}/dismissal-requests/code-scanning", s.requireOrgAdmin(scopeSecurityEvents, permRead, s.orgSecurityReviewList(reviewKindCodeDismissal)))
	s.route("GET /api/v3/orgs/{org}/dismissal-requests/dependabot", s.requireOrgAdmin(scopeSecurityEvents, permRead, s.orgSecurityReviewList(reviewKindDependabot)))
	s.route("GET /api/v3/orgs/{org}/dismissal-requests/secret-scanning", s.requireOrgAdmin(scopeSecurityEvents, permRead, s.orgSecurityReviewList(reviewKindSecretDismissal)))

	s.route("GET /api/v3/repos/{owner}/{repo}/bypass-requests/push-rules", read(s.repoSecurityReviewList(reviewKindPushBypass)))
	s.route("GET /api/v3/repos/{owner}/{repo}/bypass-requests/push-rules/{bypass_request_number}", read(s.repoSecurityReviewGet(reviewKindPushBypass, "bypass_request_number")))
	s.route("GET /api/v3/repos/{owner}/{repo}/bypass-requests/secret-scanning", read(s.repoSecurityReviewList(reviewKindSecretBypass)))
	s.route("GET /api/v3/repos/{owner}/{repo}/bypass-requests/secret-scanning/{bypass_request_number}", read(s.repoSecurityReviewGet(reviewKindSecretBypass, "bypass_request_number")))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/bypass-requests/secret-scanning/{bypass_request_number}", write(s.repoSecurityReviewDecision(reviewKindSecretBypass, "bypass_request_number", true)))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/bypass-responses/secret-scanning/{bypass_response_id}", write(s.handleDismissSecretScanningBypassResponse))

	s.route("GET /api/v3/repos/{owner}/{repo}/dismissal-requests/code-scanning", read(s.repoSecurityReviewList(reviewKindCodeDismissal)))
	s.route("GET /api/v3/repos/{owner}/{repo}/dismissal-requests/code-scanning/{alert_number}", read(s.repoSecurityReviewGet(reviewKindCodeDismissal, "alert_number")))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/dismissal-requests/code-scanning/{alert_number}", write(s.repoSecurityReviewDecision(reviewKindCodeDismissal, "alert_number", false)))

	s.route("GET /api/v3/repos/{owner}/{repo}/dismissal-requests/dependabot", read(s.repoSecurityReviewList(reviewKindDependabot)))
	s.route("GET /api/v3/repos/{owner}/{repo}/dismissal-requests/dependabot/{alert_number}", read(s.repoSecurityReviewGet(reviewKindDependabot, "alert_number")))
	s.route("POST /api/v3/repos/{owner}/{repo}/dismissal-requests/dependabot/{alert_number}", write(s.handleCreateDependabotDismissalRequest))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/dismissal-requests/dependabot/{alert_number}", write(s.repoSecurityReviewDecision(reviewKindDependabot, "alert_number", false)))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/dismissal-requests/dependabot/{alert_number}", write(s.handleCancelDependabotDismissalRequest))

	s.route("GET /api/v3/repos/{owner}/{repo}/dismissal-requests/secret-scanning", read(s.repoSecurityReviewList(reviewKindSecretDismissal)))
	s.route("GET /api/v3/repos/{owner}/{repo}/dismissal-requests/secret-scanning/{alert_number}", read(s.repoSecurityReviewGet(reviewKindSecretDismissal, "alert_number")))
	s.route("PATCH /api/v3/repos/{owner}/{repo}/dismissal-requests/secret-scanning/{alert_number}", write(s.repoSecurityReviewDecision(reviewKindSecretDismissal, "alert_number", false)))
}

func (s *Server) securityReviewJSON(r *http.Request, request *SecurityReviewRequest) map[string]interface{} {
	repo := s.store.GetRepoByFullName(request.RepoKey)
	if repo == nil {
		return nil
	}
	org := s.store.GetOrg(request.OrgLogin)
	requester := s.store.GetUserByID(request.RequesterID)
	requesterJSON := map[string]interface{}{"actor_id": request.RequesterID, "actor_name": ""}
	if requester != nil {
		requesterJSON["actor_name"] = requester.Login
	}
	responses := make([]map[string]interface{}, 0, len(request.Responses))
	for _, response := range request.Responses {
		reviewer := s.store.GetUserByID(response.ReviewerID)
		reviewerName := ""
		if reviewer != nil {
			reviewerName = reviewer.Login
		}
		responses = append(responses, map[string]interface{}{
			"id":       response.ID,
			"reviewer": map[string]interface{}{"actor_id": response.ReviewerID, "actor_name": reviewerName},
			"message":  response.Message, "status": response.Status,
			"created_at": response.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	orgID := 0
	if org != nil {
		orgID = org.ID
	}
	family := request.Kind
	if request.Kind == reviewKindSecretBypass {
		family = "secret-scanning"
	}
	prefix := "dismissal-requests"
	if request.Kind == reviewKindPushBypass || request.Kind == reviewKindSecretBypass {
		prefix = "bypass-requests"
	}
	return map[string]interface{}{
		"id": request.ID, "number": request.Number,
		"repository":   map[string]interface{}{"id": repo.ID, "name": repo.Name, "full_name": repo.FullName},
		"organization": map[string]interface{}{"id": orgID, "name": request.OrgLogin},
		"requester":    requesterJSON, "request_type": request.Kind,
		"data": request.Data, "resource_identifier": request.ResourceID,
		"status": request.Status, "requester_comment": request.RequesterComment,
		"expires_at": request.ExpiresAt.UTC().Format(time.RFC3339),
		"created_at": request.CreatedAt.UTC().Format(time.RFC3339),
		"responses":  responses,
		"url":        s.baseURL(r) + "/api/v3/repos/" + repo.FullName + "/" + prefix + "/" + family + "/" + strconv.Itoa(request.Number),
		"html_url":   s.baseURL(r) + "/" + repo.FullName + "/security/" + family + "/" + strconv.Itoa(request.Number),
	}
}

func (s *Server) writeSecurityReviewList(w http.ResponseWriter, r *http.Request, requests []*SecurityReviewRequest) {
	requests = paginateAndLink(w, r, requests)
	result := make([]map[string]interface{}, 0, len(requests))
	for _, request := range requests {
		if value := s.securityReviewJSON(r, request); value != nil {
			result = append(result, value)
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) orgSecurityReviewList(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		org := s.store.GetOrg(r.PathValue("org"))
		if org == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		s.writeSecurityReviewList(w, r, s.store.ListSecurityReviewRequests("", org.Login, kind))
	}
}

func (s *Server) repoSecurityReviewList(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := s.lookupReadableRepoFromPath(w, r)
		if repo == nil {
			return
		}
		s.writeSecurityReviewList(w, r, s.store.ListSecurityReviewRequests(repo.FullName, "", kind))
	}
}

func securityReviewNumber(w http.ResponseWriter, r *http.Request, parameter string) (int, bool) {
	number, err := strconv.Atoi(r.PathValue(parameter))
	if err != nil || number <= 0 {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return 0, false
	}
	return number, true
}

func (s *Server) repoSecurityReviewGet(kind, parameter string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := s.lookupReadableRepoFromPath(w, r)
		if repo == nil {
			return
		}
		number, ok := securityReviewNumber(w, r, parameter)
		if !ok {
			return
		}
		s.store.Mu.RLock()
		request := copySecurityReviewRequest(s.store.SecurityReviewRequests[securityReviewScope(repo.FullName, kind)][number])
		s.store.Mu.RUnlock()
		if request == nil {
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, s.securityReviewJSON(r, request))
	}
}

func (s *Server) repoSecurityReviewDecision(kind, parameter string, bypass bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := s.lookupReadableRepoFromPath(w, r)
		if repo == nil {
			return
		}
		number, ok := securityReviewNumber(w, r, parameter)
		if !ok {
			return
		}
		var req struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		}
		if !decodeJSONBody(w, r, &req) {
			return
		}
		status := req.Status
		if bypass && status == "reject" {
			status = "denied"
		}
		if status == "approve" {
			status = "approved"
		}
		if status != "approved" && status != "deny" && status != "denied" {
			writeGHValidationError(w, "SecurityReview", "status", "invalid")
			return
		}
		if status == "deny" {
			status = "denied"
		}
		if len(req.Message) > 2048 {
			writeGHValidationError(w, "SecurityReview", "message", "too_long")
			return
		}
		user := ghUserFromContext(r.Context())
		s.store.Mu.Lock()
		request := s.store.SecurityReviewRequests[securityReviewScope(repo.FullName, kind)][number]
		if request == nil {
			s.store.Mu.Unlock()
			writeGHError(w, http.StatusNotFound, "Not Found")
			return
		}
		responseID := s.store.NextSecurityReviewResponseID
		s.store.NextSecurityReviewResponseID++
		request.Status = status
		request.Responses = append(request.Responses, SecurityReviewResponse{
			ID: responseID, ReviewerID: user.ID, Message: req.Message, Status: status, CreatedAt: s.currentTime(),
		})
		if s.store.Persist != nil {
			scope := securityReviewScope(repo.FullName, kind)
			s.store.Persist.MustPut("security_review_requests", scope, s.store.SecurityReviewRequests[scope])
		}
		s.store.Mu.Unlock()
		key := "dismissal_review_id"
		if bypass {
			key = "bypass_review_id"
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{key: responseID})
	}
}

func (s *Server) handleCreateDependabotDismissalRequest(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	number, ok := securityReviewNumber(w, r, "alert_number")
	if !ok {
		return
	}
	alert := s.store.GetDependabotAlert(repo.FullName, number)
	if alert == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		DismissedReason  string  `json:"dismissed_reason"`
		DismissedComment *string `json:"dismissed_comment"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	switch req.DismissedReason {
	case "fix_started", "no_bandwidth", "tolerable_risk", "inaccurate", "not_used":
	default:
		writeGHValidationError(w, "DependabotDismissalRequest", "dismissed_reason", "invalid")
		return
	}
	user := ghUserFromContext(r.Context())
	now := s.currentTime()
	orgLogin := ""
	if repo.OwnerType == "Organization" {
		orgLogin = strings.SplitN(repo.FullName, "/", 2)[0]
	}
	request := &SecurityReviewRequest{
		Number: number, RepoKey: repo.FullName, OrgLogin: orgLogin, Kind: reviewKindDependabot,
		RequesterID: user.ID, ResourceID: strconv.Itoa(number), Status: "pending",
		RequesterComment: req.DismissedComment,
		Data: []map[string]interface{}{{
			"reason": req.DismissedReason, "alert_number": strconv.Itoa(number),
			"alert_title": alert.Summary,
		}},
		CreatedAt: now, ExpiresAt: now.Add(7 * 24 * time.Hour),
	}
	scope := securityReviewScope(repo.FullName, reviewKindDependabot)
	s.store.Mu.Lock()
	if s.store.SecurityReviewRequests[scope] == nil {
		s.store.SecurityReviewRequests[scope] = map[int]*SecurityReviewRequest{}
	}
	if existing := s.store.SecurityReviewRequests[scope][number]; existing != nil && existing.Status == "pending" {
		s.store.Mu.Unlock()
		writeGHValidationError(w, "DependabotDismissalRequest", "alert_number", "already_exists")
		return
	}
	request.ID = s.store.NextSecurityReviewRequestID
	s.store.NextSecurityReviewRequestID++
	s.store.SecurityReviewRequests[scope][number] = request
	if s.store.Persist != nil {
		s.store.Persist.MustPut("security_review_requests", scope, s.store.SecurityReviewRequests[scope])
	}
	s.store.Mu.Unlock()
	writeJSON(w, http.StatusCreated, s.securityReviewJSON(r, request))
}

func (s *Server) handleCancelDependabotDismissalRequest(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	number, ok := securityReviewNumber(w, r, "alert_number")
	if !ok {
		return
	}
	scope := securityReviewScope(repo.FullName, reviewKindDependabot)
	s.store.Mu.Lock()
	if s.store.SecurityReviewRequests[scope][number] == nil {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	delete(s.store.SecurityReviewRequests[scope], number)
	if s.store.Persist != nil {
		s.store.Persist.MustPut("security_review_requests", scope, s.store.SecurityReviewRequests[scope])
	}
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDismissSecretScanningBypassResponse(w http.ResponseWriter, r *http.Request) {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return
	}
	responseID, ok := securityReviewNumber(w, r, "bypass_response_id")
	if !ok {
		return
	}
	scope := securityReviewScope(repo.FullName, reviewKindSecretBypass)
	s.store.Mu.Lock()
	found := false
	for _, request := range s.store.SecurityReviewRequests[scope] {
		for index := range request.Responses {
			if request.Responses[index].ID == responseID {
				request.Responses[index].Status = "dismissed"
				found = true
				break
			}
		}
	}
	if !found {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	if s.store.Persist != nil {
		s.store.Persist.MustPut("security_review_requests", scope, s.store.SecurityReviewRequests[scope])
	}
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

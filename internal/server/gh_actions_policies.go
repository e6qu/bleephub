package bleephub

// Actions policies: rules restricting which actors may trigger Actions
// workflows and which events may start them, owned by a repository or by an
// organization. The CRUD surface is here; the rules take effect where runs are
// created (gh_actions_policy_enforcement.go).

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHActionsPolicyRoutes() {
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/policies", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleListRepoActionsPolicies))
	s.route("POST /api/v3/repos/{owner}/{repo}/actions/policies", s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleCreateRepoActionsPolicy))
	s.route("GET /api/v3/repos/{owner}/{repo}/actions/policies/{policy_id}", s.requirePerm(store.ScopeAdministration, store.PermRead, s.handleGetRepoActionsPolicy))
	s.route("PUT /api/v3/repos/{owner}/{repo}/actions/policies/{policy_id}", s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleUpdateRepoActionsPolicy))
	s.route("DELETE /api/v3/repos/{owner}/{repo}/actions/policies/{policy_id}", s.requirePerm(store.ScopeAdministration, store.PermWrite, s.handleDeleteRepoActionsPolicy))

	s.route("GET /api/v3/orgs/{org}/actions/policies", s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermRead, s.handleListOrgActionsPolicies))
	s.route("POST /api/v3/orgs/{org}/actions/policies", s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermWrite, s.handleCreateOrgActionsPolicy))
	s.route("GET /api/v3/orgs/{org}/actions/policies/{policy_id}", s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermRead, s.handleGetOrgActionsPolicy))
	s.route("PUT /api/v3/orgs/{org}/actions/policies/{policy_id}", s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermWrite, s.handleUpdateOrgActionsPolicy))
	s.route("DELETE /api/v3/orgs/{org}/actions/policies/{policy_id}", s.requireOrgAdmin(store.ScopeOrgAdministration, store.PermWrite, s.handleDeleteOrgActionsPolicy))
}

// actionsPolicyEvents are the events a restrict_action_events rule may list.
var actionsPolicyEvents = func() map[string]bool {
	events := map[string]bool{}
	for _, event := range []string{"branch_protection_rule", "check_run", "check_suite", "create", "delete", "deployment", "deployment_status", "discussion", "discussion_comment", "fork", "gollum", "image_version", "issue_comment", "issues", "label", "merge_group", "milestone", "page_build", "project", "project_card", "project_column", "public", "pull_request", "pull_request_review", "pull_request_review_comment", "pull_request_target", "push", "registry_package", "release", "repository_dispatch", "schedule", "status", "watch", "workflow_call", "workflow_dispatch", "workflow_run"} {
		events[event] = true
	}
	return events
}()

// actionsPolicyRequest is the create and update body. Pointers and nil slices
// distinguish a member that was sent from one that was left out, which an
// update preserves.
type actionsPolicyRequest struct {
	Name        *string                     `json:"name"`
	Enforcement *string                     `json:"enforcement"`
	Conditions  *actionsPolicyConditionsReq `json:"conditions"`
	Rules       *[]actionsPolicyRuleReq     `json:"rules"`
}

type actionsPolicyConditionsReq struct {
	RepositoryName     *store.ActionsPolicyNameCondition     `json:"repository_name"`
	RepositoryID       *store.ActionsPolicyIDCondition       `json:"repository_id"`
	RepositoryProperty *store.ActionsPolicyPropertyCondition `json:"repository_property"`
	WorkflowPath       *store.ActionsPolicyPathCondition     `json:"workflow_path"`
}

type actionsPolicyRuleReq struct {
	Type       string `json:"type"`
	Parameters *struct {
		AllowedActors *[]store.ActionsPolicyActor `json:"allowed_actors"`
		AllowedEvents *[]string                   `json:"allowed_events"`
	} `json:"parameters"`
}

// validActionsPolicyEnforcement is repository-rule-enforcement.
func validActionsPolicyEnforcement(value string) bool {
	return value == "disabled" || value == "active" || value == "evaluate"
}

// actionsPolicyRulesFromRequest validates the rules array. Each rule must be
// one of the two documented types and carry that type's parameter.
func actionsPolicyRulesFromRequest(w http.ResponseWriter, rules []actionsPolicyRuleReq) ([]store.ActionsPolicyRule, bool) {
	out := make([]store.ActionsPolicyRule, 0, len(rules))
	for _, rule := range rules {
		switch rule.Type {
		case store.ActionsPolicyRuleRestrictActors:
			if rule.Parameters == nil || rule.Parameters.AllowedActors == nil {
				store.WriteGHValidationError(w, "ActionsPolicy", "rules.parameters.allowed_actors", "missing_field")
				return nil, false
			}
			for _, actor := range *rule.Parameters.AllowedActors {
				if !store.ActionsPolicyActorTypes[actor.Type] || actor.ID <= 0 {
					store.WriteGHValidationError(w, "ActionsPolicy", "rules.parameters.allowed_actors", "invalid")
					return nil, false
				}
			}
			out = append(out, store.ActionsPolicyRule{Type: rule.Type, AllowedActors: *rule.Parameters.AllowedActors})
		case store.ActionsPolicyRuleRestrictEvents:
			if rule.Parameters == nil || rule.Parameters.AllowedEvents == nil {
				store.WriteGHValidationError(w, "ActionsPolicy", "rules.parameters.allowed_events", "missing_field")
				return nil, false
			}
			for _, event := range *rule.Parameters.AllowedEvents {
				if !actionsPolicyEvents[event] {
					store.WriteGHValidationError(w, "ActionsPolicy", "rules.parameters.allowed_events", "invalid")
					return nil, false
				}
			}
			out = append(out, store.ActionsPolicyRule{Type: rule.Type, AllowedEvents: *rule.Parameters.AllowedEvents})
		default:
			store.WriteGHValidationError(w, "ActionsPolicy", "rules.type", "invalid")
			return nil, false
		}
	}
	return out, true
}

// validActionsPolicyWorkflowPath applies the server-side rules the schema
// leaves to the API: a new or changed condition needs at least one pattern,
// `~ALL` stands alone among the included patterns, and is never excluded.
func validActionsPolicyWorkflowPath(condition *store.ActionsPolicyPathCondition) bool {
	if condition.Include == nil || condition.Exclude == nil {
		return false
	}
	if len(condition.Include) == 0 && len(condition.Exclude) == 0 {
		return false
	}
	for _, pattern := range condition.Include {
		if pattern == "" || (pattern == "~ALL" && len(condition.Include) > 1) {
			return false
		}
	}
	for _, pattern := range condition.Exclude {
		if pattern == "" || pattern == "~ALL" {
			return false
		}
	}
	return true
}

// actionsPolicyConditionsFromRequest validates conditions for the owner kind.
// A repository policy may carry only workflow_path. An organization policy
// must select repositories by exactly one of name, id or property, and may
// add workflow_path.
func (s *Server) actionsPolicyConditionsFromRequest(w http.ResponseWriter, req *actionsPolicyConditionsReq, org *store.Org) (*store.ActionsPolicyConditions, bool) {
	selectors := 0
	for _, present := range []bool{req.RepositoryName != nil, req.RepositoryID != nil, req.RepositoryProperty != nil} {
		if present {
			selectors++
		}
	}
	if (org == nil && selectors != 0) || (org != nil && selectors != 1) {
		store.WriteGHValidationError(w, "ActionsPolicy", "conditions", "invalid")
		return nil, false
	}
	if req.WorkflowPath != nil && !validActionsPolicyWorkflowPath(req.WorkflowPath) {
		store.WriteGHValidationError(w, "ActionsPolicy", "conditions.workflow_path", "invalid")
		return nil, false
	}
	switch {
	case req.RepositoryName != nil && (req.RepositoryName.Include == nil || req.RepositoryName.Exclude == nil):
		store.WriteGHValidationError(w, "ActionsPolicy", "conditions.repository_name", "invalid")
		return nil, false
	case req.RepositoryProperty != nil && (req.RepositoryProperty.Include == nil || req.RepositoryProperty.Exclude == nil):
		store.WriteGHValidationError(w, "ActionsPolicy", "conditions.repository_property", "invalid")
		return nil, false
	case req.RepositoryID != nil:
		if req.RepositoryID.RepositoryIDs == nil {
			store.WriteGHValidationError(w, "ActionsPolicy", "conditions.repository_id", "invalid")
			return nil, false
		}
		// An id naming another owner's repository would let an organization
		// learn, from a 201, that the repository exists.
		for _, id := range req.RepositoryID.RepositoryIDs {
			repo := s.store.GetRepoByID(id)
			if repo == nil || repo.OwnerType != "Organization" || repo.OwnerID != org.ID {
				store.WriteGHValidationError(w, "ActionsPolicy", "conditions.repository_id", "invalid")
				return nil, false
			}
		}
	}
	if selectors == 0 && req.WorkflowPath == nil {
		// The empty object: no stored condition, every workflow targeted.
		return nil, true
	}
	return &store.ActionsPolicyConditions{
		RepositoryName:     req.RepositoryName,
		RepositoryID:       req.RepositoryID,
		RepositoryProperty: req.RepositoryProperty,
		WorkflowPath:       req.WorkflowPath,
	}, true
}

// actionsPolicyJSON renders a policy. withBody adds conditions and rules, which
// the list leaves out. An omitted stored workflow condition is reported as the
// condition it means — every workflow — as the schema describes.
func (s *Server) actionsPolicyJSON(r *http.Request, p *store.ActionsPolicy, withBody bool) map[string]interface{} {
	sourceType, source := s.store.ActionsPolicySource(p)
	base := s.baseURL(r)
	self, html := base+"/api/v3/repos/"+source+"/actions/policies/", base+"/"+source+"/settings/actions/policies/"
	if sourceType == "Organization" {
		self, html = base+"/api/v3/orgs/"+source+"/actions/policies/", base+"/organizations/"+source+"/settings/actions/policies/"
	}
	id := strconv.Itoa(p.ID)
	out := map[string]interface{}{
		"id":          p.ID,
		"node_id":     p.NodeID,
		"name":        p.Name,
		"target":      "actions",
		"source_type": sourceType,
		"source":      source,
		"enforcement": p.Enforcement,
		"_links": map[string]interface{}{
			"self": map[string]interface{}{"href": self + id},
			"html": map[string]interface{}{"href": html + id},
		},
		"created_at": p.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": p.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if !withBody {
		return out
	}
	conditions := map[string]interface{}{}
	if c := p.Conditions; c != nil {
		if c.RepositoryName != nil {
			conditions["repository_name"] = c.RepositoryName
		}
		if c.RepositoryID != nil {
			conditions["repository_id"] = c.RepositoryID
		}
		if c.RepositoryProperty != nil {
			conditions["repository_property"] = c.RepositoryProperty
		}
	}
	path := &store.ActionsPolicyPathCondition{Include: []string{"~ALL"}, Exclude: []string{}}
	if p.Conditions != nil && p.Conditions.WorkflowPath != nil {
		path = p.Conditions.WorkflowPath
	}
	conditions["workflow_path"] = path
	out["conditions"] = conditions

	rules := make([]map[string]interface{}, 0, len(p.Rules))
	for _, rule := range p.Rules {
		parameters := map[string]interface{}{}
		if rule.Type == store.ActionsPolicyRuleRestrictActors {
			parameters["allowed_actors"] = jsonArray(rule.AllowedActors)
		} else {
			parameters["allowed_events"] = jsonArray(rule.AllowedEvents)
		}
		rules = append(rules, map[string]interface{}{"type": rule.Type, "parameters": parameters})
	}
	out["rules"] = rules
	return out
}

// lookupActionsPolicy resolves {policy_id} to a policy the given owner holds.
// A policy of another owner is not found, never forbidden: its id says nothing
// about whose it is.
func (s *Server) lookupActionsPolicy(w http.ResponseWriter, r *http.Request, repoID, orgID int) *store.ActionsPolicy {
	id, err := strconv.Atoi(r.PathValue("policy_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	policy := s.store.GetActionsPolicy(id)
	if policy == nil || policy.RepoID != repoID || policy.OrgID != orgID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return policy
}

func (s *Server) writeActionsPolicyList(w http.ResponseWriter, r *http.Request, policies []*store.ActionsPolicy) {
	if field := invalidRESTPaginationQuery(r); field != "" {
		store.WriteGHValidationError(w, "ActionsPolicy", field, "invalid")
		return
	}
	rendered := make([]map[string]interface{}, 0, len(policies))
	for _, policy := range policies {
		rendered = append(rendered, s.actionsPolicyJSON(r, policy, false))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_count": len(rendered),
		"policies":    paginateAndLink(w, r, rendered),
	})
}

// actionsPolicyHasParents reads has_parents, which defaults to true.
func actionsPolicyHasParents(w http.ResponseWriter, r *http.Request) (bool, bool) {
	switch strings.ToLower(r.URL.Query().Get("has_parents")) {
	case "", "true":
		return true, true
	case "false":
		return false, true
	}
	store.WriteGHValidationError(w, "ActionsPolicy", "has_parents", "invalid")
	return false, false
}

// createActionsPolicy is the shared create path; org is nil for a repository.
func (s *Server) createActionsPolicy(w http.ResponseWriter, r *http.Request, repo *store.Repo, org *store.Org) {
	var req actionsPolicyRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name == nil || strings.TrimSpace(*req.Name) == "" {
		store.WriteGHValidationError(w, "ActionsPolicy", "name", "missing_field")
		return
	}
	if req.Enforcement == nil {
		store.WriteGHValidationError(w, "ActionsPolicy", "enforcement", "missing_field")
		return
	}
	if !validActionsPolicyEnforcement(*req.Enforcement) {
		store.WriteGHValidationError(w, "ActionsPolicy", "enforcement", "invalid")
		return
	}
	policy := &store.ActionsPolicy{Name: strings.TrimSpace(*req.Name), Enforcement: *req.Enforcement}
	if org != nil {
		policy.OrgID = org.ID
		if req.Conditions == nil {
			// An organization policy has to say which repositories it governs.
			store.WriteGHValidationError(w, "ActionsPolicy", "conditions", "missing_field")
			return
		}
	} else {
		policy.RepoID = repo.ID
	}
	if req.Conditions != nil {
		conditions, ok := s.actionsPolicyConditionsFromRequest(w, req.Conditions, org)
		if !ok {
			return
		}
		policy.Conditions = conditions
	}
	if req.Rules != nil {
		rules, ok := actionsPolicyRulesFromRequest(w, *req.Rules)
		if !ok {
			return
		}
		policy.Rules = rules
	}
	created := s.store.CreateActionsPolicy(policy)
	body := s.actionsPolicyJSON(r, created, true)
	links := body["_links"].(map[string]interface{})["self"].(map[string]interface{})
	writeJSONCreated(w, links["href"].(string), body)
}

// updateActionsPolicy is the shared update path. A member left out of the body
// keeps its stored value; an omitted workflow_path keeps the stored targeting.
func (s *Server) updateActionsPolicy(w http.ResponseWriter, r *http.Request, policy *store.ActionsPolicy, org *store.Org) {
	var req actionsPolicyRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		store.WriteGHValidationError(w, "ActionsPolicy", "name", "invalid")
		return
	}
	if req.Enforcement != nil && !validActionsPolicyEnforcement(*req.Enforcement) {
		store.WriteGHValidationError(w, "ActionsPolicy", "enforcement", "invalid")
		return
	}
	var conditions *store.ActionsPolicyConditions
	if req.Conditions != nil {
		validated, ok := s.actionsPolicyConditionsFromRequest(w, req.Conditions, org)
		if !ok {
			return
		}
		conditions = validated
	}
	var rules []store.ActionsPolicyRule
	if req.Rules != nil {
		validated, ok := actionsPolicyRulesFromRequest(w, *req.Rules)
		if !ok {
			return
		}
		rules = validated
	}
	updated := s.store.UpdateActionsPolicy(policy.ID, func(stored *store.ActionsPolicy) {
		if req.Name != nil {
			stored.Name = strings.TrimSpace(*req.Name)
		}
		if req.Enforcement != nil {
			stored.Enforcement = *req.Enforcement
		}
		if req.Conditions != nil {
			var keptPath *store.ActionsPolicyPathCondition
			if stored.Conditions != nil {
				keptPath = stored.Conditions.WorkflowPath
			}
			if conditions == nil {
				conditions = &store.ActionsPolicyConditions{}
			}
			if conditions.WorkflowPath == nil {
				conditions.WorkflowPath = keptPath
			}
			if *conditions == (store.ActionsPolicyConditions{}) {
				conditions = nil
			}
			stored.Conditions = conditions
		}
		if req.Rules != nil {
			stored.Rules = rules
		}
	})
	if updated == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, s.actionsPolicyJSON(r, updated, true))
}

func (s *Server) handleListRepoActionsPolicies(w http.ResponseWriter, r *http.Request) {
	repo := s.resolveRepo(w, r)
	if repo == nil {
		return
	}
	hasParents, ok := actionsPolicyHasParents(w, r)
	if !ok {
		return
	}
	s.writeActionsPolicyList(w, r, s.store.ListRepoActionsPolicies(repo, hasParents))
}

func (s *Server) handleCreateRepoActionsPolicy(w http.ResponseWriter, r *http.Request) {
	if repo := s.resolveRepo(w, r); repo != nil {
		s.createActionsPolicy(w, r, repo, nil)
	}
}

func (s *Server) handleGetRepoActionsPolicy(w http.ResponseWriter, r *http.Request) {
	repo := s.resolveRepo(w, r)
	if repo == nil {
		return
	}
	if policy := s.lookupActionsPolicy(w, r, repo.ID, 0); policy != nil {
		writeJSON(w, http.StatusOK, s.actionsPolicyJSON(r, policy, true))
	}
}

func (s *Server) handleUpdateRepoActionsPolicy(w http.ResponseWriter, r *http.Request) {
	repo := s.resolveRepo(w, r)
	if repo == nil {
		return
	}
	if policy := s.lookupActionsPolicy(w, r, repo.ID, 0); policy != nil {
		s.updateActionsPolicy(w, r, policy, nil)
	}
}

func (s *Server) handleDeleteRepoActionsPolicy(w http.ResponseWriter, r *http.Request) {
	repo := s.resolveRepo(w, r)
	if repo == nil {
		return
	}
	if policy := s.lookupActionsPolicy(w, r, repo.ID, 0); policy != nil {
		s.store.DeleteActionsPolicy(policy.ID)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleListOrgActionsPolicies(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	// An organization is the top of the hierarchy bleephub models for these
	// policies, so has_parents selects nothing more; it is still validated.
	if _, ok := actionsPolicyHasParents(w, r); !ok {
		return
	}
	s.writeActionsPolicyList(w, r, s.store.ListOrgActionsPolicies(org.ID))
}

func (s *Server) handleCreateOrgActionsPolicy(w http.ResponseWriter, r *http.Request) {
	s.createActionsPolicy(w, r, nil, s.store.GetOrg(r.PathValue("org")))
}

func (s *Server) handleGetOrgActionsPolicy(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	if policy := s.lookupActionsPolicy(w, r, 0, org.ID); policy != nil {
		writeJSON(w, http.StatusOK, s.actionsPolicyJSON(r, policy, true))
	}
}

func (s *Server) handleUpdateOrgActionsPolicy(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	if policy := s.lookupActionsPolicy(w, r, 0, org.ID); policy != nil {
		s.updateActionsPolicy(w, r, policy, org)
	}
}

func (s *Server) handleDeleteOrgActionsPolicy(w http.ResponseWriter, r *http.Request) {
	org := s.store.GetOrg(r.PathValue("org"))
	if policy := s.lookupActionsPolicy(w, r, 0, org.ID); policy != nil {
		s.store.DeleteActionsPolicy(policy.ID)
		w.WriteHeader(http.StatusNoContent)
	}
}

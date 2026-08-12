package bleephub

import (
	"net/http"
	"strconv"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHEnterpriseRulesetRoutes() {
	s.route("POST /api/v3/enterprises/{enterprise}/rulesets", s.requireEnterpriseOwner(s.handleCreateEnterpriseRuleset))
	s.route("GET /api/v3/enterprises/{enterprise}/rulesets/{ruleset_id}", s.requireEnterpriseMember(s.handleGetEnterpriseRuleset))
	s.route("PUT /api/v3/enterprises/{enterprise}/rulesets/{ruleset_id}", s.requireEnterpriseOwner(s.handleUpdateEnterpriseRuleset))
	s.route("DELETE /api/v3/enterprises/{enterprise}/rulesets/{ruleset_id}", s.requireEnterpriseOwner(s.handleDeleteEnterpriseRuleset))
	s.route("GET /api/v3/enterprises/{enterprise}/rulesets/{ruleset_id}/history", s.requireEnterpriseMember(s.handleListEnterpriseRulesetHistory))
	s.route("GET /api/v3/enterprises/{enterprise}/rulesets/{ruleset_id}/history/{version_id}", s.requireEnterpriseMember(s.handleGetEnterpriseRulesetVersion))
}

func (s *Server) lookupEnterpriseRuleset(w http.ResponseWriter, r *http.Request) *store.Ruleset {
	id, err := strconv.Atoi(r.PathValue("ruleset_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	ruleset := s.store.GetEnterpriseRuleset(s.enterpriseSlug(), id)
	if ruleset == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
	}
	return ruleset
}

func (s *Server) handleCreateEnterpriseRuleset(w http.ResponseWriter, r *http.Request) {
	var body store.Ruleset
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if body.Name == "" {
		store.WriteGHValidationError(w, "ruleset", "name", "missing_field")
		return
	}
	ruleset := s.store.CreateEnterpriseRuleset(s.enterpriseSlug(), &body)
	writeJSON(w, http.StatusCreated, rulesetToJSON(ruleset, true))
}

func (s *Server) handleGetEnterpriseRuleset(w http.ResponseWriter, r *http.Request) {
	ruleset := s.lookupEnterpriseRuleset(w, r)
	if ruleset == nil {
		return
	}
	writeJSON(w, http.StatusOK, rulesetToJSON(ruleset, true))
}

func applyRulesetUpdate(ruleset *store.Ruleset, body *store.Ruleset) {
	if body.Name != "" {
		ruleset.Name = body.Name
	}
	if body.Target != "" {
		ruleset.Target = body.Target
	}
	if body.Enforcement != "" {
		ruleset.Enforcement = body.Enforcement
	}
	if body.BypassActors != nil {
		ruleset.BypassActors = body.BypassActors
	}
	if body.CurrentUserCanBypass != "" {
		ruleset.CurrentUserCanBypass = body.CurrentUserCanBypass
	}
	if len(body.Conditions.RefName.Include) > 0 || len(body.Conditions.RefName.Exclude) > 0 {
		ruleset.Conditions = body.Conditions
	}
	if body.Rules != nil {
		ruleset.Rules = body.Rules
	}
}

func (s *Server) handleUpdateEnterpriseRuleset(w http.ResponseWriter, r *http.Request) {
	user := ghUserFromContext(r.Context())
	ruleset := s.lookupEnterpriseRuleset(w, r)
	if ruleset == nil {
		return
	}
	var body store.Ruleset
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if !s.store.UpdateOrgRuleset(ruleset.ID, user.ID, func(candidate *store.Ruleset) {
		applyRulesetUpdate(candidate, &body)
	}) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, rulesetToJSON(s.store.GetEnterpriseRuleset(s.enterpriseSlug(), ruleset.ID), true))
}

func (s *Server) handleDeleteEnterpriseRuleset(w http.ResponseWriter, r *http.Request) {
	ruleset := s.lookupEnterpriseRuleset(w, r)
	if ruleset == nil {
		return
	}
	s.store.DeleteRuleset(ruleset.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListEnterpriseRulesetHistory(w http.ResponseWriter, r *http.Request) {
	ruleset := s.lookupEnterpriseRuleset(w, r)
	if ruleset == nil {
		return
	}
	versions := s.store.GetRulesetHistory(ruleset)
	out := make([]map[string]interface{}, len(versions))
	for i, version := range versions {
		out[i] = rulesetVersionJSON(version, false)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetEnterpriseRulesetVersion(w http.ResponseWriter, r *http.Request) {
	ruleset := s.lookupEnterpriseRuleset(w, r)
	if ruleset == nil {
		return
	}
	versionID, err := strconv.Atoi(r.PathValue("version_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	version := s.store.GetRulesetVersion(ruleset, versionID)
	if version == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, rulesetVersionJSON(*version, true))
}

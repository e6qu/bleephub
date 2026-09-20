package bleephub

// An enterprise's proof-of-presence requirement, the setting sudo mode enforces
// (gh_sudo_mode.go). Served under /ui-data, not /api/v3 or GraphQL: GitHub
// withdrew the updateEnterpriseProofOfPresenceRequiredSetting mutation from its
// public schema and offers the setting through web settings only, so any
// GitHub-shaped route for it would be invented.

import (
	"net/http"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHEnterpriseProofOfPresenceRoutes() {
	s.route("GET /ui-data/enterprises/{enterprise}/proof-of-presence", s.handleGetEnterpriseProofOfPresence)
	s.route("PUT /ui-data/enterprises/{enterprise}/proof-of-presence", s.handleSetEnterpriseProofOfPresence)
}

// enterpriseForProofOfPresence resolves the path's enterprise and checks access.
// A non-member gets 404, not 403, so a stranger cannot learn a slug names one.
func (s *Server) enterpriseForProofOfPresence(w http.ResponseWriter, r *http.Request, write bool) *store.Enterprise {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return nil
	}
	e := s.store.GetEnterprise(r.PathValue("enterprise"))
	if e == nil || !s.store.IsEnterpriseMember(e.ID, viewer) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	if write && !s.store.IsEnterpriseOwner(e.ID, viewer) {
		writeGHError(w, http.StatusForbidden, "Must be an enterprise owner.")
		return nil
	}
	return e
}

func proofOfPresenceJSON(e *store.Enterprise) map[string]interface{} {
	requirement := e.Policy.ProofOfPresenceRequired
	if requirement == "" {
		requirement = store.EnterprisePolicyNoPolicy
	}
	return map[string]interface{}{"requirement": requirement}
}

func (s *Server) handleGetEnterpriseProofOfPresence(w http.ResponseWriter, r *http.Request) {
	e := s.enterpriseForProofOfPresence(w, r, false)
	if e == nil {
		return
	}
	writeJSON(w, http.StatusOK, proofOfPresenceJSON(e))
}

func (s *Server) handleSetEnterpriseProofOfPresence(w http.ResponseWriter, r *http.Request) {
	e := s.enterpriseForProofOfPresence(w, r, true)
	if e == nil {
		return
	}
	if s.crossSiteBrowserPost(r) {
		writeGHError(w, http.StatusForbidden, "cross-origin request denied")
		return
	}
	// The requirement guards itself: were it changeable from a stale session,
	// whoever found an unattended browser could switch the gate off first.
	if s.requireProofOfPresence(w, r) {
		return
	}
	var req struct {
		Requirement string `json:"requirement"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	switch req.Requirement {
	case store.EnterprisePolicyNoPolicy, store.EnterpriseProofOfPresenceMFA, store.EnterpriseProofOfPresenceReauth:
	default:
		store.WriteGHValidationError(w, "Enterprise", "requirement", "invalid")
		return
	}
	updated := s.store.UpdateEnterprisePolicy(e.ID, func(policy *store.EnterprisePolicy) {
		policy.ProofOfPresenceRequired = req.Requirement
	})
	if updated == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.recordAuditEvent("business.proof_of_presence_required_policy_update", ghUserFromContext(r.Context()).Login, "",
		map[string]interface{}{"enterprise": updated.Slug, "requirement": req.Requirement})
	writeJSON(w, http.StatusOK, proofOfPresenceJSON(updated))
}

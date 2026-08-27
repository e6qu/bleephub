package bleephub

// An enterprise's verified domains, which the notification-delivery restriction
// names. Served under /ui-data, not /api/v3: GitHub exposes verifiable domains
// only through GraphQL and web settings, so a REST route would be invented.

import (
	"net/http"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) registerGHEnterpriseVerifiedDomainRoutes() {
	s.route("GET /ui-data/enterprises/{enterprise}/verified-domains", s.handleGetEnterpriseVerifiedDomains)
	s.route("PUT /ui-data/enterprises/{enterprise}/verified-domains", s.handleSetEnterpriseVerifiedDomains)
}

// enterpriseForVerifiedDomains resolves the path's enterprise and checks access.
// A non-member gets 404, not 403, so a stranger cannot learn a slug names one.
func (s *Server) enterpriseForVerifiedDomains(w http.ResponseWriter, r *http.Request, write bool) *store.Enterprise {
	viewer := ghUserFromContext(r.Context())
	if viewer == nil {
		writeGHError(w, http.StatusUnauthorized, "Requires authentication")
		return nil
	}
	e := s.store.GetEnterprise(r.PathValue("enterprise"))
	if e == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	if write && !s.store.IsEnterpriseOwner(e.ID, viewer) {
		writeGHError(w, http.StatusForbidden, "Must be an enterprise owner.")
		return nil
	}
	if !write && !s.store.IsEnterpriseMember(e.ID, viewer) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return e
}

// verifiedDomainsJSON reports the domains alongside the restriction that gives
// them effect.
func verifiedDomainsJSON(e *store.Enterprise) map[string]interface{} {
	domains := e.VerifiedDomains
	if domains == nil {
		domains = []string{}
	}
	return map[string]interface{}{
		"domains": domains,
		"notification_delivery_restriction_enabled": e.Policy.NotificationDeliveryRestrictionEnabled == store.EnterprisePolicyEnabled,
	}
}

func (s *Server) handleGetEnterpriseVerifiedDomains(w http.ResponseWriter, r *http.Request) {
	e := s.enterpriseForVerifiedDomains(w, r, false)
	if e == nil {
		return
	}
	writeJSON(w, http.StatusOK, verifiedDomainsJSON(e))
}

func (s *Server) handleSetEnterpriseVerifiedDomains(w http.ResponseWriter, r *http.Request) {
	e := s.enterpriseForVerifiedDomains(w, r, true)
	if e == nil {
		return
	}
	if s.crossSiteBrowserPost(r) {
		writeGHError(w, http.StatusForbidden, "cross-origin request denied")
		return
	}
	var req struct {
		Domains []string `json:"domains"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	for _, domain := range req.Domains {
		if store.NormalizeVerifiedDomain(domain) == "" {
			store.WriteGHValidationError(w, "VerifiableDomain", "domain", "invalid")
			return
		}
	}
	updated := s.store.SetEnterpriseVerifiedDomains(e.ID, req.Domains)
	if updated == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.recordAuditEvent("business.set_verified_domains", ghUserFromContext(r.Context()).Login, "",
		map[string]interface{}{"enterprise": updated.Slug, "domain_count": len(updated.VerifiedDomains)})
	writeJSON(w, http.StatusOK, verifiedDomainsJSON(updated))
}

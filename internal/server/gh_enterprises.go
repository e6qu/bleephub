package bleephub

import (
	"net/http"
	"os"
	"strings"
)

// bleephub is a single GHES instance: exactly one enterprise, its slug set by
// BLEEPHUB_ENTERPRISE_SLUG (default "bleephub"). Every authenticated user is a
// member; site admins are the enterprise owners.

const defaultEnterpriseSlug = "bleephub"

func (s *Server) enterpriseSlug() string {
	if v := os.Getenv("BLEEPHUB_ENTERPRISE_SLUG"); v != "" {
		return v
	}
	return defaultEnterpriseSlug
}

// resolveEnterprise 404s unless the {enterprise} path parameter names the
// configured enterprise.
func (s *Server) resolveEnterprise(w http.ResponseWriter, r *http.Request) bool {
	if r.PathValue("enterprise") != s.enterpriseSlug() {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return false
	}
	return true
}

// enterpriseFromRequest returns the {enterprise} path parameter as the tenant
// key; a slug other than the configured one is a 404.
func (s *Server) enterpriseFromRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	enterprise := r.PathValue("enterprise")
	if enterprise != s.enterpriseSlug() {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return "", false
	}
	return enterprise, true
}

// requireEnterpriseMember gates an endpoint on an authenticated user (every
// user is a member) and the {enterprise} path parameter matching.
func (s *Server) requireEnterpriseMember(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ghUserFromContext(r.Context()) == nil {
			writeGHError(w, http.StatusUnauthorized, "Requires authentication")
			return
		}
		if !s.resolveEnterprise(w, r) {
			return
		}
		next(w, r)
	}
}

// requireEnterpriseOwner additionally requires a site admin (the enterprise
// owner on GHES).
func (s *Server) requireEnterpriseOwner(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := ghUserFromContext(r.Context())
		if user == nil {
			writeGHError(w, http.StatusUnauthorized, "Requires authentication")
			return
		}
		if !s.resolveEnterprise(w, r) {
			return
		}
		if !user.SiteAdmin || !credentialConveysSiteAdmin(r.Context()) {
			writeGHError(w, http.StatusForbidden, "Must be an enterprise owner.")
			return
		}
		next(w, r)
	}
}

// splitCommaList splits a comma-separated filter into trimmed, non-empty parts.
func splitCommaList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) registerGHEnterpriseRoutes() {
	s.registerGHEnterpriseAdminRoutes()
	s.registerGHEnterpriseVerifiedDomainRoutes()
	s.registerGHEnterprisePropertyRoutes()
	s.registerGHEnterpriseRulesetRoutes()
	s.registerGHEnterpriseSecurityRoutes()
	s.registerGHEnterpriseAppRoutes()
	s.registerGHEnterpriseSCIMRoutes()
	s.registerGHEnterpriseRoleRoutes()
	s.registerGHEnterpriseLicensingRoutes()
	s.registerGHEnterpriseCopilotV2Routes()
	s.registerGHEnterpriseBillingRoutes()
	s.registerGHEnterpriseTeamRoutes()
	s.registerGHEnterpriseCodeSecurityRoutes()
	s.registerGHEnterpriseActionsRoutes()
	s.registerGHEnterpriseCopilotRoutes()
	s.registerGHEnterpriseDependabotRoutes()
}

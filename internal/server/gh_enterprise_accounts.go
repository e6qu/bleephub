package bleephub

// The enterprise account layer: the instance's own enterprise account and
// the role a principal holds in an enterprise.
//
// bleephub's pre-existing enterprise surface is the GHES *settings*
// singleton keyed on the configured slug. This file adds the account itself:
// a persisted Enterprise row with a profile, a membership roll, the
// organizations it owns and a policy set. The configured slug names the
// instance's own enterprise account, which is created at boot, so REST and
// GraphQL describe the same enterprise rather than two ideas of one.

import (
	"context"

	"github.com/e6qu/bleephub/internal/store"
)

// seedPrimaryEnterprise creates the instance's own enterprise account if it
// does not already exist. The slug is BLEEPHUB_ENTERPRISE_SLUG (the same
// configuration the REST enterprise routes key on), so the account GraphQL
// serves and the settings REST serves are one enterprise.
func (s *Server) seedPrimaryEnterprise() {
	slug := s.enterpriseSlug()
	s.store.SetPrimaryEnterpriseSlug(slug)
	s.store.EnsureEnterprise(slug, slug, "")
}

// primaryEnterprise returns the instance's own enterprise account. It is
// never nil after seedPrimaryEnterprise has run; the nil return covers a
// store constructed without the seed (a unit test that builds a bare store).
func (s *Server) primaryEnterprise() *store.Enterprise {
	return s.store.GetEnterprise(s.enterpriseSlug())
}

// enterpriseRoleOfUser reports the role a user holds in an enterprise. The
// derivation lives in the store (EffectiveEnterpriseRole) so REST, GraphQL and
// the policy predicates answer the question from one implementation.
func (s *Server) enterpriseRoleOfUser(e *store.Enterprise, user *store.User) store.EnterpriseRole {
	if e == nil {
		return ""
	}
	return s.store.EffectiveEnterpriseRole(e.ID, user)
}

// viewerEnterpriseRole reports the request principal's role in an enterprise.
// An enterprise-scoped read or write is authorized against the value this
// returns; "" means the principal is not a member and must not learn anything
// about the enterprise beyond its public profile.
func (s *Server) viewerEnterpriseRole(ctx context.Context, e *store.Enterprise) store.EnterpriseRole {
	return s.enterpriseRoleOfUser(e, ghUserFromContext(ctx))
}

// enterpriseOwnerRole reports whether role administers the enterprise.
func enterpriseOwnerRole(role store.EnterpriseRole) bool {
	return role == store.EnterpriseRoleOwner
}

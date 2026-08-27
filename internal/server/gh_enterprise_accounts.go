package bleephub

// The enterprise account layer: the instance's own enterprise account (a
// persisted Enterprise row) and the role a principal holds in an enterprise.
// The configured slug names the instance's own account, so REST and GraphQL
// describe one enterprise.

import (
	"context"

	"github.com/e6qu/bleephub/internal/store"
)

// seedPrimaryEnterprise creates the instance's own enterprise account if absent.
func (s *Server) seedPrimaryEnterprise() {
	slug := s.enterpriseSlug()
	s.store.SetPrimaryEnterpriseSlug(slug)
	s.store.EnsureEnterprise(slug, slug, "")
}

// primaryEnterprise returns the instance's own enterprise account (nil only
// for a bare store built without seedPrimaryEnterprise).
func (s *Server) primaryEnterprise() *store.Enterprise {
	return s.store.GetEnterprise(s.enterpriseSlug())
}

// enterpriseRoleOfUser reports the role a user holds in an enterprise.
func (s *Server) enterpriseRoleOfUser(e *store.Enterprise, user *store.User) store.EnterpriseRole {
	if e == nil {
		return ""
	}
	return s.store.EffectiveEnterpriseRole(e.ID, user)
}

// viewerEnterpriseRole reports the request principal's role in an enterprise;
// "" means a non-member, who may see nothing beyond the public profile.
func (s *Server) viewerEnterpriseRole(ctx context.Context, e *store.Enterprise) store.EnterpriseRole {
	return s.enterpriseRoleOfUser(e, ghUserFromContext(ctx))
}

func enterpriseOwnerRole(role store.EnterpriseRole) bool {
	return role == store.EnterpriseRoleOwner
}

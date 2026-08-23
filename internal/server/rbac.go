package bleephub

import (
	"context"
	"net/http"

	"github.com/e6qu/bleephub/internal/store"
)

// requireRepoOwns binds a sub-resource addressed by its own id — a check run, a
// deployment status, a comment — to the repository named in the URL path.
// ownerRepoID is the repo reached by walking the sub-resource's parent chain; a
// broken chain yields 0, which never matches a stored repo.
//
// The mismatch answer is 404 rather than 403 so the id cannot be probed for
// existence in another tenant.
func requireRepoOwns(w http.ResponseWriter, repo *store.Repo, ownerRepoID int) bool {
	if repo == nil || ownerRepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return false
	}
	return true
}

// canAdminOrgAsUser checks if a user is an active admin of the given organization.
// A pending (invited) admin has not accepted yet and holds no rights.
func canAdminOrgAsUser(st *store.Store, user *store.User, org *store.Org) bool {
	if user == nil || org == nil {
		return false
	}
	m := st.GetMembership(org.Login, user.ID)
	return m != nil && m.Role == store.OrgRoleAdmin && m.State == store.MembershipStateActive
}

// isActiveOrgMemberAsUser reports whether user holds an active membership in the
// org. Org teams (their names, members, and repo grants) are visible only to
// org members on real GitHub — a non-member authenticated caller gets 404, the
// same as an unknown org, so the org's internal structure never leaks.
func isActiveOrgMemberAsUser(st *store.Store, user *store.User, orgLogin string) bool {
	if user == nil {
		return false
	}
	m := st.GetMembership(orgLogin, user.ID)
	return m != nil && m.State == store.MembershipStateActive
}

// namedUserIsActiveOrgMember asks the membership question about somebody other
// than the caller: a login supplied in a request body, a collaborator being
// added to a resource. There is no credential to intersect for a third party,
// which is why this is not (*Server).viewerIsOrgMember and why call sites are
// not required to route through the credential-aware predicate.
func namedUserIsActiveOrgMember(st *store.Store, subject *store.User, orgLogin string) bool {
	return isActiveOrgMemberAsUser(st, subject, orgLogin)
}

// visibleRepos filters a repository list down to what the request's credential
// may see.
//
// Several list paths reach the repository index directly and return whatever
// the prefix match found. Public repositories are visible to everyone
// including an anonymous viewer; a private one requires read access. Doing
// this in one place means a new listing gets the rule by calling a function
// rather than by its author remembering the rule exists.
func (s *Server) visibleRepos(ctx context.Context, repos []*store.Repo) []*store.Repo {
	out := make([]*store.Repo, 0, len(repos))
	for _, repo := range repos {
		if repo == nil {
			continue
		}
		if repo.Private && !s.viewerCanReadRepo(ctx, repo) {
			continue
		}
		out = append(out, repo)
	}
	return out
}

// ARCH-001: the user-scoped RBAC predicates moved to internal/store with the
// data layer. These forwarders keep the declarations inside the RBAC layer so
// the authz chokepoint ratchet (authz_chokepoint_test.go) still guards every
// other call site; nothing outside rbac.go / gh_apps_perms.go may call them.
func canAdminRepo(st *store.Store, user *store.User, repo *store.Repo) bool {
	return store.CanAdminRepo(st, user, repo)
}

func canPushRepo(st *store.Store, user *store.User, repo *store.Repo) bool {
	return store.CanPushRepo(st, user, repo)
}

func canReadRepoAsUser(st *store.Store, user *store.User, repo *store.Repo) bool {
	return store.CanReadRepoAsUser(st, user, repo)
}

// namedUserCanReadRepo asks the read question about somebody other than the
// caller: a CODEOWNERS owner being considered for an automatic review request.
// As with namedUserIsActiveOrgMember there is no credential to intersect for a
// third party — the question is what that person can see, not what the
// request's bearer may do — which is why this is not (*Server).viewerCanReadRepo.
func namedUserCanReadRepo(st *store.Store, subject *store.User, repo *store.Repo) bool {
	if subject == nil || repo == nil {
		return false
	}
	return canReadRepoAsUser(st, subject, repo)
}

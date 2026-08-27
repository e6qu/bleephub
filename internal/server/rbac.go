package bleephub

import (
	"context"
	"net/http"

	"github.com/e6qu/bleephub/internal/store"
)

// requireRepoOwns binds a sub-resource addressed by its own id to the repo named
// in the URL. ownerRepoID is the repo reached via the sub-resource's parent chain
// (0 on a broken chain, matching no repo). Mismatch is 404, not 403, so the id
// cannot be probed for existence in another tenant.
func requireRepoOwns(w http.ResponseWriter, repo *store.Repo, ownerRepoID int) bool {
	if repo == nil || ownerRepoID != repo.ID {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return false
	}
	return true
}

// canAdminOrgAsUser reports whether user is an active admin of org. A pending
// (invited) admin holds no rights.
func canAdminOrgAsUser(st *store.Store, user *store.User, org *store.Org) bool {
	if user == nil || org == nil {
		return false
	}
	m := st.GetMembership(org.Login, user.ID)
	return m != nil && m.Role == store.OrgRoleAdmin && m.State == store.MembershipStateActive
}

// isActiveOrgMemberAsUser reports whether user holds an active membership. Org
// team structure is visible only to members; a non-member gets 404, same as an
// unknown org, so the org's internals never leak.
func isActiveOrgMemberAsUser(st *store.Store, user *store.User, orgLogin string) bool {
	if user == nil {
		return false
	}
	m := st.GetMembership(orgLogin, user.ID)
	return m != nil && m.State == store.MembershipStateActive
}

// namedUserIsActiveOrgMember asks the membership question about a third party (a
// login from a request body), not the caller — so it is not the credential-aware
// (*Server).viewerIsOrgMember.
func namedUserIsActiveOrgMember(st *store.Store, subject *store.User, orgLogin string) bool {
	return isActiveOrgMemberAsUser(st, subject, orgLogin)
}

// visibleRepos filters a repo list to what the request's credential may see:
// public repos are visible to anyone, a private one needs read access.
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

// ARCH-001: the user-scoped RBAC predicates live in internal/store; these
// forwarders keep the declarations in the RBAC layer so the authz chokepoint
// ratchet still guards every other call site. Only rbac.go / gh_apps_perms.go may
// call them.
func canAdminRepo(st *store.Store, user *store.User, repo *store.Repo) bool {
	return store.CanAdminRepo(st, user, repo)
}

func canPushRepo(st *store.Store, user *store.User, repo *store.Repo) bool {
	return store.CanPushRepo(st, user, repo)
}

func canReadRepoAsUser(st *store.Store, user *store.User, repo *store.Repo) bool {
	return store.CanReadRepoAsUser(st, user, repo)
}

// namedUserCanReadRepo asks the read question about a third party (a CODEOWNERS
// owner considered for an auto review request), not the caller — so it is not
// (*Server).viewerCanReadRepo.
func namedUserCanReadRepo(st *store.Store, subject *store.User, repo *store.Repo) bool {
	if subject == nil || repo == nil {
		return false
	}
	return canReadRepoAsUser(st, subject, repo)
}

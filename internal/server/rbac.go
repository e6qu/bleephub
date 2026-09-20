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

// GitHub's predefined repository-role ids, as rulesets and Actions policies
// name them; an organization's custom roles take ids of their own and stand on
// a base role.
var predefinedRepositoryRoleBase = map[int]string{1: "read", 2: "maintain", 3: "triage", 4: "write", 5: "admin"}

// userHoldsRepositoryRole reports whether a named user holds the repository
// role, or a higher one, on the repository. It asks about a third party — the
// actor behind a workflow trigger, on a path with no request credential — so it
// is not a viewer predicate. Standing is compared on the three levels the
// access model distinguishes: triage reads, maintain writes.
func userHoldsRepositoryRole(st *store.Store, user *store.User, repo *store.Repo, roleID int) bool {
	if user == nil || repo == nil {
		return false
	}
	base, ok := predefinedRepositoryRoleBase[roleID]
	if !ok {
		owner, _ := splitRepoFull(repo.FullName)
		st.Mu.RLock()
		role := st.OrgCustomRepoRoles[owner][roleID]
		if role != nil {
			base = role.BaseRole
		}
		st.Mu.RUnlock()
		if role == nil {
			return false
		}
	}
	switch base {
	case "admin":
		return canAdminRepo(st, user, repo)
	case "write", "maintain":
		return canPushRepo(st, user, repo)
	default:
		return canReadRepoAsUser(st, user, repo)
	}
}

// rulesetActorBypasses reports whether actor may bypass the ruleset on a direct
// write to a reference. A bypass actor is listed by kind: a user, a team, a
// repository role (held at that level or above), the organization's
// administrators, the enterprise's owners, or an app. Only the `always` and
// `exempt` modes cover a direct write; `pull_request` lets its holder bypass
// when merging a pull request and nowhere else. Everything but `User` used to be
// stored, returned by the API and ignored, so a team given a bypass was still
// refused. It lives in the RBAC layer because it asks what a named user holds.
func rulesetActorBypasses(st *store.Store, rs *store.Ruleset, repo *store.Repo, actor *store.User) bool {
	if actor == nil {
		return false
	}
	for _, bypass := range rs.BypassActors {
		if bypass.BypassMode != "always" && bypass.BypassMode != "exempt" {
			continue
		}
		switch bypass.ActorType {
		case "User":
			if bypass.ActorID == actor.ID {
				return true
			}
		case "Team":
			team := st.GetTeamByID(bypass.ActorID)
			if team == nil {
				continue
			}
			org := st.GetOrgByID(team.OrgID)
			if org == nil {
				continue
			}
			if _, member := st.GetTeamMembership(org.Login, team.Slug, actor.ID); member {
				return true
			}
		case "RepositoryRole":
			if userHoldsRepositoryRole(st, actor, repo, bypass.ActorID) {
				return true
			}
		case "OrganizationAdmin":
			if repo.OwnerType == "Organization" && canAdminOrgAsUser(st, actor, st.GetOrgByID(repo.OwnerID)) {
				return true
			}
		case "EnterpriseOwner":
			if _, enterprise := st.EnterprisePolicyForRepo(repo); enterprise != nil && st.IsEnterpriseOwner(enterprise.ID, actor) {
				return true
			}
		case "Integration":
			// An app acts as its bot, whose id is the app's, negated.
			if actor.Type == "Bot" && actor.ID == -bypass.ActorID {
				return true
			}
		}
	}
	return false
}

package store

import (
	"crypto/subtle"
	"strings"
)

// repoPermissionsJSON is a serialized repository's `permissions` block: the
// viewer's own capabilities on it.
func repoPermissionsJSON(st *Store, viewer *User, repo *Repo) map[string]bool {
	return map[string]bool{
		"admin": CanAdminRepo(st, viewer, repo),
		"push":  CanPushRepo(st, viewer, repo),
		"pull":  CanReadRepoAsUser(st, viewer, repo),
	}
}

// SecretEqual compares two credential strings in constant time. Use it for all
// secret material (client secrets, CSRF tokens, webhook signatures, download
// tokens): some comparisons are reachable unauthenticated and unthrottled,
// exactly the shape a byte-at-a-time timing oracle needs.
func SecretEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func CanAdminRepo(st *Store, user *User, repo *Repo) bool {
	if user == nil {
		return false
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return canAccessRepoLocked(st, user, repo, "admin")
}

// CanPushRepo reports whether user can push: ownership, sufficient org base
// permission, team push, or collaborator push/admin access.
func CanPushRepo(st *Store, user *User, repo *Repo) bool {
	if user == nil {
		return false
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return canAccessRepoLocked(st, user, repo, "push")
}

// CanReadRepoAsUser reports read access: public repos are readable by all,
// private repos require ownership, org membership, team access, or collaborator
// pull access. Must not be called with st.Mu held; it takes the read lock.
func CanReadRepoAsUser(st *Store, user *User, repo *Repo) bool {
	if !repo.Private {
		return true
	}
	if user != nil && user.SiteAdmin {
		return true
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return canAccessRepoLocked(st, user, repo, "pull")
}

// canAccessRepoLocked is the single human-principal repository capability
// calculation; callers hold st.Mu. Org base role, team role, and
// direct-collaborator role live in one lattice so read and write surfaces never
// assign different rights to the same principal.
func canAccessRepoLocked(st *Store, user *User, repo *Repo, required string) bool {
	if user == nil || repo == nil {
		return false
	}
	// A user-namespace grant admits an enterprise owner with the full capability
	// set, so an admin can intervene in a managed account's repository.
	if userNamespaceGrantAdmitsLocked(st, user, repo) {
		return true
	}
	if user.SiteAdmin || (repo.Owner != nil && repo.Owner.ID == user.ID) {
		return true
	}
	parts := strings.SplitN(repo.FullName, "/", 2)
	if len(parts) != 2 {
		return false
	}
	orgLogin := parts[0]
	if org := st.OrgsByLogin[orgLogin]; org != nil {
		if membership := st.Memberships[MembershipKey(orgLogin, user.ID)]; membership != nil &&
			membership.State == MembershipStateActive {
			// Org owners administer every repository regardless of the members' base permission.
			if membership.Role == OrgRoleAdmin ||
				repositoryPermissionAtLeast(st.enterpriseClampedBasePermissionLocked(org), required) {
				return true
			}
		}
		if hasTeamAccessLocked(st, orgLogin, user.ID, repo.FullName, TeamPermission(required)) {
			return true
		}
	}
	return RepoCollaboratorPermissionAtLeastLocked(st, repo.FullName, user.Login, required)
}

// hasTeamAccessLocked checks team grants while the caller holds st.Mu.
func hasTeamAccessLocked(st *Store, orgLogin string, userID int, repoFullName string, minPermission TeamPermission) bool {
	org := st.OrgsByLogin[orgLogin]
	if org == nil {
		return false
	}
	// Team access requires an ACTIVE organization membership. A user added to a
	// team while only invited (org membership state "pending") must not receive
	// the team's repo grants until they accept — otherwise a pending invitee can
	// read/push/admin the team's private repos, and may still decline the invite.
	if m := st.Memberships[MembershipKey(orgLogin, userID)]; m == nil || m.State != MembershipStateActive {
		return false
	}

	for _, team := range st.TeamsBySlug {
		if team.OrgID != org.ID {
			continue
		}
		repoFound := false
		for _, rn := range team.RepoNames {
			if rn == repoFullName {
				repoFound = true
				break
			}
		}
		if !repoFound {
			continue
		}
		// The team's permission on THIS repo is the per-repo override when set,
		// else the team default. Using the default alone would grant a member the
		// team's baseline even where the repo was explicitly downgraded (privilege
		// escalation) — or deny where it was upgraded.
		effective := team.Permission
		if override, ok := team.RepoPermissions[repoFullName]; ok {
			effective = override
		}
		if !PermissionAtLeast(effective, minPermission) {
			continue
		}
		for _, mid := range team.MemberIDs {
			if mid == userID {
				return true
			}
		}
	}
	return false
}

// RepoCollaboratorPermissionAtLeastLocked checks direct collaboration while
// the caller holds st.Mu.
func RepoCollaboratorPermissionAtLeastLocked(st *Store, repoFullName, login, minPerm string) bool {
	parts := strings.SplitN(repoFullName, "/", 2)
	if len(parts) != 2 {
		return false
	}
	perm := ""
	if collabs := st.RepoCollaborators[repoFullName]; collabs != nil {
		perm = collabs[login]
	}
	if perm == "" {
		return false
	}
	return repositoryPermissionAtLeast(perm, minPerm)
}

// repositoryPermissionAtLeast compares org base-permission values against
// capability names. An empty stored value is GitHub's default "read"; "pull"
// and "push" are the collaborator/team wire names for read/write.
func repositoryPermissionAtLeast(granted, required string) bool {
	levels := map[string]int{
		"none":  0,
		"read":  1,
		"pull":  1,
		"write": 2,
		"push":  2,
		"admin": 3,
	}
	if granted == "" {
		granted = "read"
	}
	return levels[granted] >= levels[required]
}

// PermissionAtLeast reports whether perm ranks at least minPerm (pull < push < admin).
func PermissionAtLeast(perm, minPerm TeamPermission) bool {
	levels := map[TeamPermission]int{TeamPermissionPull: 1, TeamPermissionPush: 2, TeamPermissionAdmin: 3}
	return levels[perm] >= levels[minPerm]
}

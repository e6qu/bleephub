package store

import (
	"crypto/subtle"
	"strings"
)

// repoPermissionsJSON is the `permissions` block of a serialized repository:
// the viewer's own capabilities on it. It lives beside the predicates it
// projects rather than in the serializer, so the RBAC vocabulary stays in one
// file.
func repoPermissionsJSON(st *Store, viewer *User, repo *Repo) map[string]bool {
	return map[string]bool{
		"admin": CanAdminRepo(st, viewer, repo),
		"push":  CanPushRepo(st, viewer, repo),
		"pull":  CanReadRepoAsUser(st, viewer, repo),
	}
}

// SecretEqual compares two credential strings in constant time.
//
// The comparison must not short-circuit on the first differing byte: several of
// these are reachable unauthenticated and unthrottled — the OAuth token
// endpoint accepts a client secret from any caller — which is exactly the shape
// a byte-at-a-time timing oracle needs. Use this for every comparison of secret
// material: client secrets, CSRF tokens, webhook signatures, download tokens.
func SecretEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// CanAdminRepo checks if a user has admin rights to a repository.
func CanAdminRepo(st *Store, user *User, repo *Repo) bool {
	if user == nil {
		return false
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return canAccessRepoLocked(st, user, repo, "admin")
}

// CanPushRepo checks if a user can push to a repository.
// Push requires ownership, a sufficient organization base permission, team
// push permission, or collaborator push/admin access.
func CanPushRepo(st *Store, user *User, repo *Repo) bool {
	if user == nil {
		return false
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return canAccessRepoLocked(st, user, repo, "push")
}

// CanReadRepoAsUser checks if a user can read a repository.
// Public repos are readable by all. Private repos require ownership, org
// membership, team access, or collaborator pull access.
// Must not be called with st.Mu held; it takes the read lock itself.
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
// calculation. Callers already hold st.Mu. Keeping the organization base role,
// team role, and direct-collaborator role in one lattice prevents read and
// write surfaces from assigning different rights to the same principal.
func canAccessRepoLocked(st *Store, user *User, repo *Repo, required string) bool {
	if user == nil || repo == nil {
		return false
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
			// Organization owners administer every repository regardless of
			// the base permission assigned to ordinary members.
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

	for _, team := range st.TeamsBySlug {
		if team.OrgID != org.ID {
			continue
		}
		if !PermissionAtLeast(team.Permission, minPermission) {
			continue
		}
		// Check if repo is in team's repo list
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
		// Check if user is a team member
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

// repositoryPermissionAtLeast compares GitHub's organization base-repository
// permission values with repository capability names. An empty stored value is
// GitHub's default "read"; "pull" and "push" are the collaborator/team wire
// names for the same read/write levels.
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

// PermissionAtLeast returns true if perm is at least minPerm.
// Permission hierarchy: pull < push < admin.
func PermissionAtLeast(perm, minPerm TeamPermission) bool {
	levels := map[TeamPermission]int{TeamPermissionPull: 1, TeamPermissionPush: 2, TeamPermissionAdmin: 3}
	return levels[perm] >= levels[minPerm]
}

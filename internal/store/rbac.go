package store

import (
	"crypto/subtle"
	"slices"
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
			// An organization-role assignment (the predefined all_repo_* roles or a
			// custom role with a base role) grants its base repository role on every
			// repository in the org. Gated on active membership, like team access.
			if userHasOrgRoleRepoGrantLocked(st, org, user.ID, required) {
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

	// teamGrantsRepo reports whether one team's own grant on repoFullName meets
	// minPermission. The team's permission on THIS repo is the per-repo override
	// when set, else the team default — using the default alone would grant a
	// member the team's baseline where the repo was explicitly downgraded
	// (privilege escalation) or deny where it was upgraded.
	teamGrantsRepo := func(team *Team) bool {
		for _, rn := range team.RepoNames {
			if rn != repoFullName {
				continue
			}
			effective := team.Permission
			if override, ok := team.RepoPermissions[repoFullName]; ok {
				effective = override
			}
			return PermissionAtLeast(effective, minPermission)
		}
		return false
	}

	for _, team := range st.Teams {
		if team.OrgID != org.ID {
			continue
		}
		member := false
		for _, mid := range team.MemberIDs {
			if mid == userID {
				member = true
				break
			}
		}
		if !member {
			continue
		}
		// A member of `team` inherits the repo grants of `team` AND all its
		// ancestor teams: GitHub cascades repository access down the hierarchy,
		// so a child team's members hold every grant the parent chain holds. Walk
		// up via ParentID, guarding against a cycle.
		visited := map[int]bool{}
		for t := team; t != nil && !visited[t.ID]; t = st.Teams[t.ParentID] {
			visited[t.ID] = true
			if teamGrantsRepo(t) {
				return true
			}
		}
	}
	return false
}

// PredefinedOrgRoleBaseRoles maps GitHub's predefined organization-role IDs to
// the repository base role each confers on every repository in the org. It
// mirrors the server's predefined-role catalog for the enforcement path in the
// store layer; TestPredefinedOrgRoleBaseRolesMatchCatalog guards against drift.
var PredefinedOrgRoleBaseRoles = map[int]string{
	138: "read",     // all_repo_read
	139: "triage",   // all_repo_triage
	140: "write",    // all_repo_write
	141: "maintain", // all_repo_maintain
	142: "admin",    // all_repo_admin
	143: "read",     // security_manager (read plus security-management capabilities)
}

// repoBaseRoleForOrgRoleLocked resolves an assigned organization-role ID to the
// repository base role it confers, whether the role is predefined or a custom
// org role. Returns "" for a role that grants no repository base access.
func repoBaseRoleForOrgRoleLocked(st *Store, orgLogin string, roleID int) string {
	if base, ok := PredefinedOrgRoleBaseRoles[roleID]; ok {
		return base
	}
	if roles := st.OrgCustomRoles[orgLogin]; roles != nil {
		if role := roles[roleID]; role != nil && role.BaseRole != nil {
			return *role.BaseRole
		}
	}
	return ""
}

// orgRoleBaseGrants reports whether a repository base role (read, triage, write,
// maintain or admin) confers the required pull/push/admin capability. triage
// grants no more than read for access purposes and maintain no more than write.
func orgRoleBaseGrants(baseRole, required string) bool {
	switch baseRole {
	case "read", "triage":
		return repositoryPermissionAtLeast("read", required)
	case "write", "maintain":
		return repositoryPermissionAtLeast("write", required)
	case "admin":
		return repositoryPermissionAtLeast("admin", required)
	}
	return false
}

// userHasOrgRoleRepoGrantLocked reports whether an organization-role assignment
// grants the user the required capability on the org's repositories — directly,
// or through a team the user belongs to (with a role assigned to that team or
// any of its ancestors, mirroring how team repository access cascades up the
// hierarchy). Caller holds st.Mu.
func userHasOrgRoleRepoGrantLocked(st *Store, org *Org, userID int, required string) bool {
	orgLogin := org.Login
	grants := func(roleID int) bool {
		return orgRoleBaseGrants(repoBaseRoleForOrgRoleLocked(st, orgLogin, roleID), required)
	}

	for roleID, userIDs := range st.OrgRoleUserAssignments[orgLogin] {
		if slices.Contains(userIDs, userID) && grants(roleID) {
			return true
		}
	}

	teamAssignments := st.OrgRoleTeamAssignments[orgLogin]
	if len(teamAssignments) == 0 {
		return false
	}
	for _, team := range st.Teams {
		if team.OrgID != org.ID || !slices.Contains(team.MemberIDs, userID) {
			continue
		}
		// Walk the team and its ancestors: a role assigned to an ancestor team
		// reaches the members of its descendants, as repository access does.
		visited := map[int]bool{}
		for t := team; t != nil && !visited[t.ID]; t = st.Teams[t.ParentID] {
			visited[t.ID] = true
			for roleID, teamIDs := range teamAssignments {
				if slices.Contains(teamIDs, t.ID) && grants(roleID) {
					return true
				}
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

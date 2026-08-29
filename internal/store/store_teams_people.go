package store

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Store state for the org people surfaces: invitations, user blocks,
// interaction limits, org-role assignments, and outside collaborators.

// orgInvitationTTL matches GitHub's 7-day invitation expiry.
const orgInvitationTTL = 7 * 24 * time.Hour

// OrgInvitation is a pending or failed invitation to join an org. Role holds the
// invitation-role wire value (direct_member | admin | billing_manager), distinct
// from the membership role enum.
type OrgInvitation struct {
	ID           int        `json:"id"`
	NodeID       string     `json:"node_id"`
	OrgID        int        `json:"org_id"`
	UserID       int        `json:"user_id"` // resolved invitee; 0 when the email matches no account
	Login        string     `json:"login"`   // "" for email-only invitations
	Email        string     `json:"email"`
	Role         string     `json:"role"`
	InviterID    int        `json:"inviter_id"`
	TeamIDs      []int      `json:"team_ids"`
	Source       string     `json:"source"` // "member" for API-created invitations
	CreatedAt    time.Time  `json:"created_at"`
	FailedAt     *time.Time `json:"failed_at,omitempty"`
	FailedReason string     `json:"failed_reason,omitempty"`
}

// OrgInteractionLimit is an org-wide interaction restriction that auto-expires
// at ExpiresAt.
type OrgInteractionLimit struct {
	Limit     string    `json:"limit"`
	ExpiresAt time.Time `json:"expires_at"`
}

// invitationMembershipRole maps an invitation role to the membership role held
// on acceptance. billing_manager confers ordinary membership (no separate class).
func invitationMembershipRole(role string) OrgRole {
	if role == "admin" {
		return OrgRoleAdmin
	}
	return OrgRoleMember
}

// CreateOrgInvitation creates an invitation and, when the invitee resolves to an
// account, the pending membership they later accept. Returns nil and a reason
// when the invitation is invalid (already a member or already invited).
func (st *Store) CreateOrgInvitation(org *Org, inviter *User, invitee *User, email, role string, teamIDs []int) (*OrgInvitation, string) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	if invitee != nil {
		if m := st.Memberships[MembershipKey(org.Login, invitee.ID)]; m != nil {
			if m.State == MembershipStateActive {
				return nil, "Invitee is already a member of this organization."
			}
			return nil, "A pending invitation already exists for this invitee."
		}
		for _, inv := range st.OrgInvitations {
			if inv.OrgID == org.ID && inv.FailedAt == nil && inv.UserID == invitee.ID {
				return nil, "A pending invitation already exists for this invitee."
			}
		}
	} else {
		for _, inv := range st.OrgInvitations {
			if inv.OrgID == org.ID && inv.FailedAt == nil && inv.Email != "" && strings.EqualFold(inv.Email, email) {
				return nil, "A pending invitation already exists for this invitee."
			}
		}
	}

	inv := &OrgInvitation{
		ID:        st.NextOrgInvitationID,
		NodeID:    fmt.Sprintf("OI_kgDO%08d", st.NextOrgInvitationID),
		OrgID:     org.ID,
		Email:     email,
		Role:      role,
		InviterID: inviter.ID,
		TeamIDs:   append([]int{}, teamIDs...),
		Source:    "member",
		CreatedAt: st.CurrentTime(),
	}
	st.NextOrgInvitationID++
	if invitee != nil {
		inv.UserID = invitee.ID
		inv.Login = invitee.Login
	}
	st.OrgInvitations[inv.ID] = inv

	// Pending membership and its invitation commit in one transaction so a crash
	// cannot leave one without the other.
	batch := NewPersistBatch(st.Persist)
	if invitee != nil {
		key := MembershipKey(org.Login, invitee.ID)
		m := &Membership{OrgID: org.ID, UserID: invitee.ID, Role: invitationMembershipRole(role), State: MembershipStatePending}
		st.Memberships[key] = m
		batch.Put("memberships", key, m)
	}
	batch.Put("org_invitations", strconv.Itoa(inv.ID), inv)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "org_invitations", Err: err})
	}
	return inv, ""
}

// reconcileOrgInvitationsLocked reconciles the org's invitations against
// membership state: active memberships consume their invitation (invitee joins
// the invited teams), vanished pending memberships cancel it, and invitations
// past the TTL fail as expired (dropping the pending membership). Callers hold
// st.Mu for writing.
func (st *Store) reconcileOrgInvitationsLocked(org *Org, now time.Time) {
	for id, inv := range st.OrgInvitations {
		if inv.OrgID != org.ID || inv.FailedAt != nil {
			continue
		}
		if inv.UserID != 0 {
			m := st.Memberships[MembershipKey(org.Login, inv.UserID)]
			switch {
			case m == nil:
				// Pending membership removed out-of-band; nothing left to accept.
				delete(st.OrgInvitations, id)
				if st.Persist != nil {
					st.Persist.MustDelete("org_invitations", strconv.Itoa(id))
				}
				continue
			case m.State == MembershipStateActive:
				st.consumeOrgInvitationLocked(inv)
				continue
			}
		}
		if now.Sub(inv.CreatedAt) > orgInvitationTTL {
			failedAt := inv.CreatedAt.Add(orgInvitationTTL)
			inv.FailedAt = &failedAt
			inv.FailedReason = "Invitation expired."
			// Dropping the pending membership and failing the invitation commit
			// in one transaction so a crash cannot strand one without the other
			// (STORE-001/002).
			batch := NewPersistBatch(st.Persist)
			if inv.UserID != 0 {
				key := MembershipKey(org.Login, inv.UserID)
				if m := st.Memberships[key]; m != nil && m.State == MembershipStatePending {
					delete(st.Memberships, key)
					batch.Delete("memberships", key)
				}
			}
			batch.Put("org_invitations", strconv.Itoa(inv.ID), inv)
			if err := batch.Commit(); err != nil {
				panic(&PersistenceFailure{Op: "batch", Bucket: "org_invitations", Err: err})
			}
		}
	}
}

// ReconcileAllOrgInvitations runs the invitation state machine across every org
// on the background dispatcher tick, so a GET never takes the write lock or does
// a durable delete on a read (STORE-034).
func (st *Store) ReconcileAllOrgInvitations(now time.Time) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	for _, org := range st.OrgsByLogin {
		st.reconcileOrgInvitationsLocked(org, now)
	}
}

// consumeOrgInvitationLocked completes an accepted invitation: the invitee joins
// every carried team and the invitation is removed. Callers hold st.Mu for writing.
func (st *Store) consumeOrgInvitationLocked(inv *OrgInvitation) {
	// Team joins and invitation removal commit in one transaction so a crash
	// cannot leave the invitee half-joined with the invitation still live (which
	// reload would re-consume) (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	for _, teamID := range inv.TeamIDs {
		team := st.Teams[teamID]
		if team == nil || team.OrgID != inv.OrgID {
			continue
		}
		if !slices.Contains(team.MemberIDs, inv.UserID) {
			team.MemberIDs = append(team.MemberIDs, inv.UserID)
			team.UpdatedAt = st.CurrentTime()
			batch.Put("teams", strconv.Itoa(team.ID), team)
		}
	}
	delete(st.OrgInvitations, inv.ID)
	batch.Delete("org_invitations", strconv.Itoa(inv.ID))
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "org_invitations", Err: err})
	}
}

// consumeOrgInvitationsForUserLocked consumes every live invitation the user
// holds in the org, invoked when a membership turns active. Callers hold st.Mu
// for writing.
func (st *Store) consumeOrgInvitationsForUserLocked(orgLogin string, userID int) {
	org := st.OrgByLoginLocked(orgLogin)
	if org == nil {
		return
	}
	for _, inv := range st.OrgInvitations {
		if inv.OrgID == org.ID && inv.UserID == userID && inv.FailedAt == nil {
			st.consumeOrgInvitationLocked(inv)
		}
	}
}

// ListPendingOrgInvitations returns the org's live invitations sorted by ID.
func (st *Store) ListPendingOrgInvitations(orgLogin string) []*OrgInvitation {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	org := st.OrgByLoginLocked(orgLogin)
	if org == nil {
		return nil
	}
	// Read stays pure: the state machine is applied durably by the background
	// reconciler, not on a GET (STORE-034).
	var out []*OrgInvitation
	for _, inv := range st.OrgInvitations {
		if inv.OrgID == org.ID && inv.FailedAt == nil {
			out = append(out, inv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotOrgInvitations(out)
}

// ListFailedOrgInvitations returns the org's failed invitations sorted by ID.
func (st *Store) ListFailedOrgInvitations(orgLogin string) []*OrgInvitation {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	org := st.OrgByLoginLocked(orgLogin)
	if org == nil {
		return nil
	}
	var out []*OrgInvitation
	for _, inv := range st.OrgInvitations {
		if inv.OrgID == org.ID && inv.FailedAt != nil {
			out = append(out, inv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotOrgInvitations(out)
}

// cloneOrgInvitation returns a copy safe to hand outside the store lock (STORE-021).
func cloneOrgInvitation(inv *OrgInvitation) *OrgInvitation {
	if inv == nil {
		return nil
	}
	clone := *inv
	if inv.TeamIDs != nil {
		clone.TeamIDs = append([]int(nil), inv.TeamIDs...)
	}
	if inv.FailedAt != nil {
		failed := *inv.FailedAt
		clone.FailedAt = &failed
	}
	return &clone
}

func (st *Store) GetOrgInvitation(orgLogin string, id int) *OrgInvitation {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	org := st.OrgByLoginLocked(orgLogin)
	if org == nil {
		return nil
	}
	inv := st.OrgInvitations[id]
	if inv == nil || inv.OrgID != org.ID || inv.FailedAt != nil {
		return nil
	}
	return cloneOrgInvitation(inv)
}

// CancelOrgInvitation removes a live invitation and its pending membership.
// Returns false when no such invitation exists.
func (st *Store) CancelOrgInvitation(orgLogin string, id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	org := st.OrgByLoginLocked(orgLogin)
	if org == nil {
		return false
	}
	inv := st.OrgInvitations[id]
	if inv == nil || inv.OrgID != org.ID || inv.FailedAt != nil {
		return false
	}
	// Cancel and pending-membership drop commit in one transaction so a stale
	// membership cannot outlive its invitation.
	batch := NewPersistBatch(st.Persist)
	if inv.UserID != 0 {
		key := MembershipKey(org.Login, inv.UserID)
		if m := st.Memberships[key]; m != nil && m.State == MembershipStatePending {
			delete(st.Memberships, key)
			batch.Delete("memberships", key)
		}
	}
	delete(st.OrgInvitations, id)
	batch.Delete("org_invitations", strconv.Itoa(id))
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "org_invitations", Err: err})
	}
	return true
}

// ListPendingOrgInvitationsForTeam returns the org's live invitations carrying
// the given team, sorted by ID.
func (st *Store) ListPendingOrgInvitationsForTeam(orgLogin string, teamID int) []*OrgInvitation {
	pending := st.ListPendingOrgInvitations(orgLogin)
	var out []*OrgInvitation
	for _, inv := range pending {
		if slices.Contains(inv.TeamIDs, teamID) {
			out = append(out, inv)
		}
	}
	return snapshotOrgInvitations(out)
}

// organization user blocks

// BlockUserForOrg records a block of the user by the organization.
// Idempotent.
func (st *Store) BlockUserForOrg(orgLogin string, userID int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	if st.OrgBlocks[orgLogin] == nil {
		st.OrgBlocks[orgLogin] = map[int]time.Time{}
	}
	if _, ok := st.OrgBlocks[orgLogin][userID]; !ok {
		st.OrgBlocks[orgLogin][userID] = st.CurrentTime()
	}
	if st.Persist != nil {
		st.Persist.MustPut("org_blocks", orgLogin, st.OrgBlocks[orgLogin])
	}
}

// UnblockUserForOrg removes an organization's block of the user.
// Idempotent.
func (st *Store) UnblockUserForOrg(orgLogin string, userID int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	if st.OrgBlocks[orgLogin] == nil {
		return
	}
	delete(st.OrgBlocks[orgLogin], userID)
	if st.Persist != nil {
		st.Persist.MustPut("org_blocks", orgLogin, st.OrgBlocks[orgLogin])
	}
}

// IsUserBlockedByOrg reports whether the organization blocks the user.
func (st *Store) IsUserBlockedByOrg(orgLogin string, userID int) bool {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	_, ok := st.OrgBlocks[orgLogin][userID]
	return ok
}

// ListOrgBlockedUsers returns the users the organization blocks, sorted
// by user ID.
func (st *Store) ListOrgBlockedUsers(orgLogin string) []*User {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	ids := make([]int, 0, len(st.OrgBlocks[orgLogin]))
	for id := range st.OrgBlocks[orgLogin] {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]*User, 0, len(ids))
	for _, id := range ids {
		if u := st.Users[id]; u != nil {
			out = append(out, u)
		}
	}
	return snapshotUsers(out)
}

// organization interaction limits

// GetOrgInteractionLimit returns the org's active interaction limit, or nil. An
// expired limit is removed on read, matching GitHub's automatic lapse.
func (st *Store) GetOrgInteractionLimit(orgLogin string) *OrgInteractionLimit {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	lim := st.OrgInteractionLimits[orgLogin]
	if lim == nil {
		return nil
	}
	if st.CurrentTime().After(lim.ExpiresAt) {
		delete(st.OrgInteractionLimits, orgLogin)
		if st.Persist != nil {
			st.Persist.MustDelete("org_interaction_limits", orgLogin)
		}
		return nil
	}
	// All-value struct, so a shallow copy detaches (STORE-021).
	clone := *lim
	return &clone
}

// SetOrgInteractionLimit stores the org's interaction limit.
func (st *Store) SetOrgInteractionLimit(orgLogin, limit string, expiresAt time.Time) *OrgInteractionLimit {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	lim := &OrgInteractionLimit{Limit: limit, ExpiresAt: expiresAt}
	st.OrgInteractionLimits[orgLogin] = lim
	if st.Persist != nil {
		st.Persist.MustPut("org_interaction_limits", orgLogin, lim)
	}
	return lim
}

// DeleteOrgInteractionLimit removes the org's interaction limit.
// Idempotent.
func (st *Store) DeleteOrgInteractionLimit(orgLogin string) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	delete(st.OrgInteractionLimits, orgLogin)
	if st.Persist != nil {
		st.Persist.MustDelete("org_interaction_limits", orgLogin)
	}
}

// organization role assignments

// AssignOrgRoleToTeam grants an organization role to a team. Idempotent.
func (st *Store) AssignOrgRoleToTeam(orgLogin string, roleID, teamID int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	if st.OrgRoleTeamAssignments[orgLogin] == nil {
		st.OrgRoleTeamAssignments[orgLogin] = map[int][]int{}
	}
	ids := st.OrgRoleTeamAssignments[orgLogin][roleID]
	if !slices.Contains(ids, teamID) {
		st.OrgRoleTeamAssignments[orgLogin][roleID] = append(ids, teamID)
	}
	if st.Persist != nil {
		st.Persist.MustPut("org_role_team_assignments", orgLogin, st.OrgRoleTeamAssignments[orgLogin])
	}
}

// UnassignOrgRoleFromTeam revokes one organization role from a team.
// Idempotent.
func (st *Store) UnassignOrgRoleFromTeam(orgLogin string, roleID, teamID int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	if st.OrgRoleTeamAssignments[orgLogin] == nil {
		return
	}
	st.OrgRoleTeamAssignments[orgLogin][roleID] = intSliceRemove(st.OrgRoleTeamAssignments[orgLogin][roleID], teamID)
	if st.Persist != nil {
		st.Persist.MustPut("org_role_team_assignments", orgLogin, st.OrgRoleTeamAssignments[orgLogin])
	}
}

// UnassignAllOrgRolesFromTeam revokes every organization role from a
// team. Idempotent.
func (st *Store) UnassignAllOrgRolesFromTeam(orgLogin string, teamID int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	if st.OrgRoleTeamAssignments[orgLogin] == nil {
		return
	}
	for roleID, ids := range st.OrgRoleTeamAssignments[orgLogin] {
		st.OrgRoleTeamAssignments[orgLogin][roleID] = intSliceRemove(ids, teamID)
	}
	if st.Persist != nil {
		st.Persist.MustPut("org_role_team_assignments", orgLogin, st.OrgRoleTeamAssignments[orgLogin])
	}
}

// AssignOrgRoleToUser grants an organization role to a user directly.
// Idempotent.
func (st *Store) AssignOrgRoleToUser(orgLogin string, roleID, userID int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	if st.OrgRoleUserAssignments[orgLogin] == nil {
		st.OrgRoleUserAssignments[orgLogin] = map[int][]int{}
	}
	ids := st.OrgRoleUserAssignments[orgLogin][roleID]
	if !slices.Contains(ids, userID) {
		st.OrgRoleUserAssignments[orgLogin][roleID] = append(ids, userID)
	}
	if st.Persist != nil {
		st.Persist.MustPut("org_role_user_assignments", orgLogin, st.OrgRoleUserAssignments[orgLogin])
	}
}

// UnassignOrgRoleFromUser revokes one organization role from a user.
// Idempotent.
func (st *Store) UnassignOrgRoleFromUser(orgLogin string, roleID, userID int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	if st.OrgRoleUserAssignments[orgLogin] == nil {
		return
	}
	st.OrgRoleUserAssignments[orgLogin][roleID] = intSliceRemove(st.OrgRoleUserAssignments[orgLogin][roleID], userID)
	if st.Persist != nil {
		st.Persist.MustPut("org_role_user_assignments", orgLogin, st.OrgRoleUserAssignments[orgLogin])
	}
}

// UnassignAllOrgRolesFromUser revokes every organization role from a
// user. Idempotent.
func (st *Store) UnassignAllOrgRolesFromUser(orgLogin string, userID int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	if st.OrgRoleUserAssignments[orgLogin] == nil {
		return
	}
	for roleID, ids := range st.OrgRoleUserAssignments[orgLogin] {
		st.OrgRoleUserAssignments[orgLogin][roleID] = intSliceRemove(ids, userID)
	}
	if st.Persist != nil {
		st.Persist.MustPut("org_role_user_assignments", orgLogin, st.OrgRoleUserAssignments[orgLogin])
	}
}

// ListTeamsWithOrgRole returns the org's existing teams holding the role, sorted
// by team ID. Assignments to since-deleted teams are skipped.
func (st *Store) ListTeamsWithOrgRole(orgLogin string, roleID int) []*Team {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	org := st.OrgByLoginLocked(orgLogin)
	if org == nil {
		return nil
	}
	orgLogin = org.Login
	ids := append([]int{}, st.OrgRoleTeamAssignments[orgLogin][roleID]...)
	sort.Ints(ids)
	out := make([]*Team, 0, len(ids))
	for _, id := range ids {
		if team := st.Teams[id]; team != nil && team.OrgID == org.ID {
			out = append(out, team)
		}
	}
	return snapshotTeams(out)
}

// ListUsersWithOrgRole maps each user holding the role to its assignment kind:
// "direct", "indirect" (via a team), or "mixed". Users without an active
// membership are skipped.
func (st *Store) ListUsersWithOrgRole(orgLogin string, roleID int) map[int]string {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	org := st.OrgByLoginLocked(orgLogin)
	if org == nil {
		return nil
	}
	orgLogin = org.Login
	activeMember := func(userID int) bool {
		m := st.Memberships[MembershipKey(orgLogin, userID)]
		return m != nil && m.State == MembershipStateActive
	}

	out := map[int]string{}
	for _, userID := range st.OrgRoleUserAssignments[orgLogin][roleID] {
		if st.Users[userID] != nil && activeMember(userID) {
			out[userID] = "direct"
		}
	}
	for _, teamID := range st.OrgRoleTeamAssignments[orgLogin][roleID] {
		team := st.Teams[teamID]
		if team == nil || team.OrgID != org.ID {
			continue
		}
		for _, userID := range team.MemberIDs {
			if st.Users[userID] == nil || !activeMember(userID) {
				continue
			}
			if out[userID] == "direct" {
				out[userID] = "mixed"
			} else if out[userID] == "" {
				out[userID] = "indirect"
			}
		}
	}
	return out
}

// outside collaborators

// ListOutsideCollaborators returns users who collaborate on at least one of the
// org's repositories without an active org membership, sorted by user ID.
func (st *Store) ListOutsideCollaborators(orgLogin string) []*User {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	org := st.OrgByLoginLocked(orgLogin)
	if org == nil {
		return nil
	}
	orgLogin = org.Login
	prefix := orgLogin + "/"
	seen := map[int]bool{}
	var out []*User
	for repoKey, collabs := range st.RepoCollaborators {
		if !strings.HasPrefix(repoKey, prefix) {
			continue
		}
		repo := st.ReposByName[repoKey]
		if repo == nil || repo.OwnerType != "Organization" {
			continue
		}
		for login := range collabs {
			u := st.UsersByLogin[login]
			if u == nil || seen[u.ID] {
				continue
			}
			if m := st.Memberships[MembershipKey(orgLogin, u.ID)]; m != nil && m.State == MembershipStateActive {
				continue
			}
			seen[u.ID] = true
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotUsers(out)
}

// GrantTeamRepoAccessAsCollaborator materializes a member's team-derived repo
// access as direct collaborator grants — the access a member keeps when
// converted to an outside collaborator. Stronger existing direct grants are kept.
func (st *Store) GrantTeamRepoAccessAsCollaborator(orgLogin string, user *User) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	org := st.OrgByLoginLocked(orgLogin)
	if org == nil {
		return
	}
	levels := map[string]int{"pull": 1, "push": 2, "admin": 3}
	permName := func(p TeamPermission) string {
		switch p {
		case TeamPermissionPush:
			return "push"
		case TeamPermissionAdmin:
			return "admin"
		default:
			return "pull"
		}
	}
	changed := map[string]bool{}
	for _, team := range st.TeamsBySlug {
		if team.OrgID != org.ID || !slices.Contains(team.MemberIDs, user.ID) {
			continue
		}
		for _, repoKey := range team.RepoNames {
			if st.ReposByName[repoKey] == nil {
				continue
			}
			perm := team.Permission
			if team.RepoPermissions != nil {
				if override, ok := team.RepoPermissions[repoKey]; ok {
					perm = override
				}
			}
			grant := permName(perm)
			if st.RepoCollaborators[repoKey] == nil {
				st.RepoCollaborators[repoKey] = map[string]string{}
			}
			if existing := st.RepoCollaborators[repoKey][user.Login]; levels[existing] >= levels[grant] {
				continue
			}
			st.RepoCollaborators[repoKey][user.Login] = grant
			changed[repoKey] = true
		}
	}
	if st.Persist != nil {
		for repoKey := range changed {
			st.Persist.MustPut("repo_collaborators", repoKey, st.RepoCollaborators[repoKey])
		}
	}
}

// RemoveOutsideCollaborator strips the user's collaborator grants and pending
// invitations across every repository of the org.
func (st *Store) RemoveOutsideCollaborator(orgLogin, login string) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	// Removal across every repo commits in one transaction so a crash cannot
	// leave them a collaborator on some repos but not others.
	batch := NewPersistBatch(st.Persist)
	prefix := orgLogin + "/"
	for repoKey, collabs := range st.RepoCollaborators {
		if !strings.HasPrefix(repoKey, prefix) {
			continue
		}
		if _, ok := collabs[login]; !ok {
			continue
		}
		delete(collabs, login)
		batch.Put("repo_collaborators", repoKey, collabs)
	}
	for repoKey, invs := range st.RepoInvitations {
		if !strings.HasPrefix(repoKey, prefix) {
			continue
		}
		removed := false
		for id, inv := range invs {
			if inv.Status == "pending" && strings.EqualFold(inv.InviteeLogin, login) {
				delete(invs, id)
				removed = true
			}
		}
		if removed {
			batch.Put("repo_invitations", repoKey, invs)
		}
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "repo_collaborators", Err: err})
	}
}

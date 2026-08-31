package store

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CreateRepoInvitation creates a pending collaborator invitation.
func (st *Store) CreateRepoInvitation(repoKey, inviteeLogin, inviteeEmail string, inviterID int, permission string) *RepoInvitation {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)

	perm := normalizeRepoPermission(permission)
	inv := &RepoInvitation{
		ID:           st.NextInvitationID,
		NodeID:       fmt.Sprintf("RI_kgDO%08d", st.NextInvitationID),
		RepoKey:      repoKey,
		InviteeLogin: inviteeLogin,
		InviteeEmail: inviteeEmail,
		InviterID:    inviterID,
		Permissions:  perm,
		CreatedAt:    time.Now().UTC(),
		Status:       "pending",
	}
	st.NextInvitationID++
	if st.RepoInvitations[repoKey] == nil {
		st.RepoInvitations[repoKey] = map[int]*RepoInvitation{}
	}
	st.RepoInvitations[repoKey][inv.ID] = inv
	if st.Persist != nil {
		st.Persist.MustPut("repo_invitations", repoKey, st.RepoInvitations[repoKey])
	}
	return inv
}

// ListPendingRepoInvitations returns a repository's pending invitations, sorted by ID.
func (st *Store) ListPendingRepoInvitations(repoKey string) []*RepoInvitation {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)

	m := st.RepoInvitations[repoKey]
	out := make([]*RepoInvitation, 0, len(m))
	for _, inv := range m {
		if inv.Status == "pending" {
			out = append(out, inv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotSlice(out)
}

// GetRepoInvitation returns an invitation by repository key and ID, or nil.
func (st *Store) GetRepoInvitation(repoKey string, id int) *RepoInvitation {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)
	if st.RepoInvitations[repoKey] == nil {
		return nil
	}
	inv := st.RepoInvitations[repoKey][id]
	if inv == nil {
		return nil
	}
	// All-value struct: shallow copy detaches it (STORE-021); UpdateRepoInvitation
	// rewrites Permissions in place.
	cp := *inv
	return &cp
}

// UpdateRepoInvitation changes the permission on a pending invitation, or
// returns nil if not found.
func (st *Store) UpdateRepoInvitation(repoKey string, id int, permission string) *RepoInvitation {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)

	if st.RepoInvitations[repoKey] == nil {
		return nil
	}
	inv, ok := st.RepoInvitations[repoKey][id]
	if !ok || inv.Status != "pending" {
		return nil
	}
	inv.Permissions = normalizeRepoPermission(permission)
	if st.Persist != nil {
		st.Persist.MustPut("repo_invitations", repoKey, st.RepoInvitations[repoKey])
	}
	return inv
}

// DeleteRepoInvitation removes an invitation, returning true if it existed.
func (st *Store) DeleteRepoInvitation(repoKey string, id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)

	if st.RepoInvitations[repoKey] == nil {
		return false
	}
	if _, ok := st.RepoInvitations[repoKey][id]; !ok {
		return false
	}
	delete(st.RepoInvitations[repoKey], id)
	if st.Persist != nil {
		st.Persist.MustPut("repo_invitations", repoKey, st.RepoInvitations[repoKey])
	}
	return true
}

// ListUserRepoInvitations returns pending invitations addressed to the user.
func (st *Store) ListUserRepoInvitations(user *User) []*RepoInvitation {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	out := []*RepoInvitation{}
	for _, m := range st.RepoInvitations {
		for _, inv := range m {
			if inv.Status != "pending" {
				continue
			}
			if strings.EqualFold(inv.InviteeLogin, user.Login) || (inv.InviteeEmail != "" && strings.EqualFold(inv.InviteeEmail, user.Email)) {
				out = append(out, inv)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotSlice(out)
}

// AcceptRepoInvitation consumes a pending invitation and adds the user to the
// repo's collaborators. It returns the repo's full name (for the `member`
// webhook) and whether an invitation was accepted.
func (st *Store) AcceptRepoInvitation(id int, user *User) (string, bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	var target *RepoInvitation
	for _, m := range st.RepoInvitations {
		if inv, ok := m[id]; ok {
			target = inv
			break
		}
	}
	if target == nil || target.Status != "pending" {
		return "", false
	}
	if !invitationMatchesUser(target, user) {
		return "", false
	}
	parts := strings.SplitN(target.RepoKey, "/", 2)
	if len(parts) != 2 {
		return "", false
	}
	repo, ok := st.ReposByName[target.RepoKey]
	if !ok {
		return "", false
	}
	if st.RepoCollaborators[target.RepoKey] == nil {
		st.RepoCollaborators[target.RepoKey] = map[string]string{}
	}
	st.RepoCollaborators[target.RepoKey][user.Login] = target.Permissions
	repo.UpdatedAt = time.Now().UTC()
	delete(st.RepoInvitations[target.RepoKey], id)
	// One transaction, so a crash cannot grant access while leaving the
	// invitation live, or consume it without granting access (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	batch.Put("repo_invitations", target.RepoKey, st.RepoInvitations[target.RepoKey])
	batch.Put("repo_collaborators", target.RepoKey, st.RepoCollaborators[target.RepoKey])
	batch.Put("repos", strconv.Itoa(repo.ID), repo)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "repo_invitations", Err: err})
	}
	return target.RepoKey, true
}

// DeclineRepoInvitation removes an invitation addressed to the user.
func (st *Store) DeclineRepoInvitation(id int, user *User) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	var target *RepoInvitation
	for _, m := range st.RepoInvitations {
		if inv, ok := m[id]; ok {
			target = inv
			break
		}
	}
	if target == nil || target.Status != "pending" {
		return false
	}
	if !invitationMatchesUser(target, user) {
		return false
	}
	delete(st.RepoInvitations[target.RepoKey], id)
	if st.Persist != nil {
		st.Persist.MustPut("repo_invitations", target.RepoKey, st.RepoInvitations[target.RepoKey])
	}
	return true
}

func invitationMatchesUser(inv *RepoInvitation, user *User) bool {
	if strings.EqualFold(inv.InviteeLogin, user.Login) {
		return true
	}
	if inv.InviteeEmail != "" && strings.EqualFold(inv.InviteeEmail, user.Email) {
		return true
	}
	return false
}

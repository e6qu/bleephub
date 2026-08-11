package store

import "strings"

func CloneCodespace(cs *Codespace) *Codespace {
	if cs == nil {
		return nil
	}
	view := *cs
	view.LatestExport = ClonePointer(cs.LatestExport)
	return &view
}

// OrgCodespacesAccess records which organization users can create codespaces
// billed to the organization.
type OrgCodespacesAccess struct {
	Visibility        string   `json:"visibility"` // disabled | selected_members | all_members | all_members_and_outside_collaborators
	SelectedUsernames []string `json:"selected_usernames,omitempty"`
}

// SetOrgCodespacesAccess replaces the org's codespaces access settings.
func (st *Store) SetOrgCodespacesAccess(orgLogin, visibility string, selected []string) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	st.OrgCodespacesAccess[orgLogin] = &OrgCodespacesAccess{
		Visibility:        visibility,
		SelectedUsernames: selected,
	}
	if st.Persist != nil {
		st.Persist.MustPut("org_codespaces_access", orgLogin, st.OrgCodespacesAccess[orgLogin])
	}
}

// ModifyOrgCodespacesAccessUsers adds or removes usernames from the org's
// selected-members codespaces access list. Returns false when the org's
// access visibility is not selected_members.
func (st *Store) ModifyOrgCodespacesAccessUsers(orgLogin string, add bool, usernames []string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	access := st.OrgCodespacesAccess[orgLogin]
	if access == nil || access.Visibility != "selected_members" {
		return false
	}
	if add {
		present := map[string]bool{}
		for _, u := range access.SelectedUsernames {
			present[strings.ToLower(u)] = true
		}
		for _, u := range usernames {
			if !present[strings.ToLower(u)] {
				access.SelectedUsernames = append(access.SelectedUsernames, u)
				present[strings.ToLower(u)] = true
			}
		}
	} else {
		remove := map[string]bool{}
		for _, u := range usernames {
			remove[strings.ToLower(u)] = true
		}
		kept := access.SelectedUsernames[:0:0]
		for _, u := range access.SelectedUsernames {
			if !remove[strings.ToLower(u)] {
				kept = append(kept, u)
			}
		}
		access.SelectedUsernames = kept
	}
	if st.Persist != nil {
		st.Persist.MustPut("org_codespaces_access", orgLogin, access)
	}
	return true
}

// orgCodespacesInvalidUsers returns the usernames that are neither active
// organization members nor collaborators on any of the org's repositories.
func (st *Store) OrgCodespacesInvalidUsers(org *Org, usernames []string) []string {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	collaborators := map[string]bool{}
	for repoKey, byLogin := range st.RepoCollaborators {
		repo := st.ReposByName[repoKey]
		if repo == nil || repo.OwnerType != "Organization" || repo.OwnerID != org.ID {
			continue
		}
		for login := range byLogin {
			collaborators[strings.ToLower(login)] = true
		}
	}

	var invalid []string
	for _, username := range usernames {
		u := st.UsersByLogin[username]
		if u != nil {
			m := st.Memberships[MembershipKey(org.Login, u.ID)]
			if m != nil && m.State == MembershipStateActive {
				continue
			}
		}
		if collaborators[strings.ToLower(username)] {
			continue
		}
		invalid = append(invalid, username)
	}
	return invalid
}

package store

import (
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Case-insensitive name resolution (GitHub parity). Entities are stored under
// canonical names in UsersByLogin/OrgsByLogin/ReposByName, with folded secondary
// indexes (folded key → canonical key) beside them so an exact miss retries in
// O(1). Creation-time collision checks fold too, so no two live entities differ
// only by case. Every mutation of the three primary maps must keep the folded
// index in step via the Index*/Unindex*Locked helpers below.

// FoldName canonicalizes a name for case-insensitive comparison: NFKC
// normalization then lower-casing, the same folding AUTH-028 applies to logins.
func FoldName(s string) string {
	return strings.ToLower(norm.NFKC.String(s))
}

// IndexUserLoginLocked records login in the folded login index. Caller holds
// st.Mu and has inserted the user into UsersByLogin under login.
func (st *Store) IndexUserLoginLocked(login string) {
	if st.foldedUserLogins == nil {
		st.foldedUserLogins = make(map[string]string)
	}
	st.foldedUserLogins[FoldName(login)] = login
}

// UnindexUserLoginLocked removes login from the folded login index, but only if
// the entry still points at this exact canonical login, so a case-only rename
// may add the new spelling and remove the old one in either order. Caller holds st.Mu.
func (st *Store) UnindexUserLoginLocked(login string) {
	folded := FoldName(login)
	if st.foldedUserLogins[folded] == login {
		delete(st.foldedUserLogins, folded)
	}
}

// IndexOrgLoginLocked records login in the folded org-login index. Caller holds
// st.Mu and has inserted the org into OrgsByLogin under login.
func (st *Store) IndexOrgLoginLocked(login string) {
	if st.foldedOrgLogins == nil {
		st.foldedOrgLogins = make(map[string]string)
	}
	st.foldedOrgLogins[FoldName(login)] = login
}

// UnindexOrgLoginLocked removes login from the folded org-login index; see
// UnindexUserLoginLocked for the case-only-rename guard. Caller holds st.Mu.
func (st *Store) UnindexOrgLoginLocked(login string) {
	folded := FoldName(login)
	if st.foldedOrgLogins[folded] == login {
		delete(st.foldedOrgLogins, folded)
	}
}

// IndexRepoNameLocked records the "owner/name" key in the folded repo index.
// Caller holds st.Mu and has inserted the repo into ReposByName under fullName.
func (st *Store) IndexRepoNameLocked(fullName string) {
	if st.foldedRepoNames == nil {
		st.foldedRepoNames = make(map[string]string)
	}
	st.foldedRepoNames[FoldName(fullName)] = fullName
}

// UnindexRepoNameLocked removes the "owner/name" key from the folded repo
// index; see UnindexUserLoginLocked for the case-only-rename guard. Caller holds st.Mu.
func (st *Store) UnindexRepoNameLocked(fullName string) {
	folded := FoldName(fullName)
	if st.foldedRepoNames[folded] == fullName {
		delete(st.foldedRepoNames, folded)
	}
}

// rebuildFoldedNameIndexesLocked recomputes the three folded case-insensitive
// name indexes from the primary maps. The replica-refresh reflect copy replaces
// the exported UsersByLogin/OrgsByLogin/ReposByName maps but skips these
// unexported derived indexes, so they must be rebuilt from the merged primaries
// (as the workflow indexes are) or case-insensitive lookups on a replica go
// stale after a refresh — resolving a peer-created org/user/repo (or a case-only
// rename) to a 404. Caller holds st.Mu.
func (st *Store) rebuildFoldedNameIndexesLocked() {
	st.foldedUserLogins = make(map[string]string, len(st.UsersByLogin))
	for login := range st.UsersByLogin {
		st.foldedUserLogins[FoldName(login)] = login
	}
	st.foldedOrgLogins = make(map[string]string, len(st.OrgsByLogin))
	for login := range st.OrgsByLogin {
		st.foldedOrgLogins[FoldName(login)] = login
	}
	st.foldedRepoNames = make(map[string]string, len(st.ReposByName))
	for fullName := range st.ReposByName {
		st.foldedRepoNames[FoldName(fullName)] = fullName
	}
}

// UserByLoginLocked resolves a login case-insensitively to the live user row,
// or nil. Caller holds st.Mu; the pointer is only valid under that lock.
func (st *Store) UserByLoginLocked(login string) *User {
	if u := st.UsersByLogin[login]; u != nil {
		return u
	}
	if canonical, ok := st.foldedUserLogins[FoldName(login)]; ok {
		return st.UsersByLogin[canonical]
	}
	return nil
}

// OrgByLoginLocked resolves an org login case-insensitively to the live org
// row, or nil. Caller holds st.Mu.
func (st *Store) OrgByLoginLocked(login string) *Org {
	if o := st.OrgsByLogin[login]; o != nil {
		return o
	}
	if canonical, ok := st.foldedOrgLogins[FoldName(login)]; ok {
		return st.OrgsByLogin[canonical]
	}
	return nil
}

// RepoByNameLocked resolves an "owner/name" key case-insensitively to the live
// repo row, or nil. Caller holds st.Mu.
func (st *Store) RepoByNameLocked(fullName string) *Repo {
	if r := st.ReposByName[fullName]; r != nil {
		return r
	}
	if canonical, ok := st.foldedRepoNames[FoldName(fullName)]; ok {
		return st.ReposByName[canonical]
	}
	return nil
}

// canonicalRepoKeyLocked maps a case-variant "owner/name" key to its canonical
// spelling, or returns the input unchanged when no repo matches. For accessors
// whose secondary maps are keyed by the canonical repo key. Caller holds st.Mu.
func (st *Store) canonicalRepoKeyLocked(fullName string) string {
	if repo := st.RepoByNameLocked(fullName); repo != nil {
		return repo.FullName
	}
	return fullName
}

// canonicalOrgLoginLocked maps a case-variant org login to its canonical
// spelling, or returns the input unchanged when no org matches. For accessors
// that build derived keys (memberships, team slugs). Caller holds st.Mu.
func (st *Store) canonicalOrgLoginLocked(login string) string {
	if org := st.OrgByLoginLocked(login); org != nil {
		return org.Login
	}
	return login
}

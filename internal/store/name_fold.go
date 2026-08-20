package store

import (
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Case-insensitive name resolution (GitHub parity).
//
// GitHub resolves owner logins, organization logins and repository names
// case-insensitively: GET /repos/ADMIN/Hello-App finds admin/hello-app and the
// payload renders the canonical casing. bleephub stores entities under their
// canonical names in UsersByLogin/OrgsByLogin/ReposByName and keeps folded
// secondary indexes (folded key → canonical key) beside them, so an exact miss
// can retry with a folded comparison in O(1) without scanning.
//
// Folding matches the login canonicalization the auth layer applies before a
// login is ever stored (AUTH-028): NFKC compatibility normalization followed
// by lower-casing. Creation-time collision checks fold too, so no two live
// entities ever differ only by case and a folded lookup is unambiguous.
//
// Every mutation of the three primary maps — create, rename, transfer,
// delete, load — must keep the folded index in step via the
// Index*/Unindex*Locked helpers below.

// FoldName canonicalizes a login, org login, repository name or "owner/name"
// key for case-insensitive comparison: NFKC normalization then lower-casing,
// the same folding AUTH-028 applies to logins ('/' is stable under both).
func FoldName(s string) string {
	return strings.ToLower(norm.NFKC.String(s))
}

// IndexUserLoginLocked records login in the folded login index. Caller must
// hold st.Mu and have inserted the user into UsersByLogin under login.
func (st *Store) IndexUserLoginLocked(login string) {
	if st.foldedUserLogins == nil {
		st.foldedUserLogins = make(map[string]string)
	}
	st.foldedUserLogins[FoldName(login)] = login
}

// UnindexUserLoginLocked removes login from the folded login index. The entry
// is dropped only if it still points at this exact canonical login, so a
// case-only rename may add the new spelling before removing the old one in
// either order. Caller must hold st.Mu.
func (st *Store) UnindexUserLoginLocked(login string) {
	folded := FoldName(login)
	if st.foldedUserLogins[folded] == login {
		delete(st.foldedUserLogins, folded)
	}
}

// IndexOrgLoginLocked records login in the folded org-login index. Caller
// must hold st.Mu and have inserted the org into OrgsByLogin under login.
func (st *Store) IndexOrgLoginLocked(login string) {
	if st.foldedOrgLogins == nil {
		st.foldedOrgLogins = make(map[string]string)
	}
	st.foldedOrgLogins[FoldName(login)] = login
}

// UnindexOrgLoginLocked removes login from the folded org-login index; see
// UnindexUserLoginLocked for the case-only-rename guard. Caller must hold
// st.Mu.
func (st *Store) UnindexOrgLoginLocked(login string) {
	folded := FoldName(login)
	if st.foldedOrgLogins[folded] == login {
		delete(st.foldedOrgLogins, folded)
	}
}

// IndexRepoNameLocked records the "owner/name" key in the folded repo index.
// Caller must hold st.Mu and have inserted the repo into ReposByName under
// fullName.
func (st *Store) IndexRepoNameLocked(fullName string) {
	if st.foldedRepoNames == nil {
		st.foldedRepoNames = make(map[string]string)
	}
	st.foldedRepoNames[FoldName(fullName)] = fullName
}

// UnindexRepoNameLocked removes the "owner/name" key from the folded repo
// index; see UnindexUserLoginLocked for the case-only-rename guard. Caller
// must hold st.Mu.
func (st *Store) UnindexRepoNameLocked(fullName string) {
	folded := FoldName(fullName)
	if st.foldedRepoNames[folded] == fullName {
		delete(st.foldedRepoNames, folded)
	}
}

// UserByLoginLocked resolves a login case-insensitively to the live user row,
// or nil. Exact matches win; a miss retries through the folded index. Caller
// must hold st.Mu (read or write); the pointer is only valid under that lock.
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
// row, or nil. Caller must hold st.Mu (read or write).
func (st *Store) OrgByLoginLocked(login string) *Org {
	if o := st.OrgsByLogin[login]; o != nil {
		return o
	}
	if canonical, ok := st.foldedOrgLogins[FoldName(login)]; ok {
		return st.OrgsByLogin[canonical]
	}
	return nil
}

// RepoByNameLocked resolves an "owner/name" key case-insensitively to the
// live repo row, or nil. Caller must hold st.Mu (read or write).
func (st *Store) RepoByNameLocked(fullName string) *Repo {
	if r := st.ReposByName[fullName]; r != nil {
		return r
	}
	if canonical, ok := st.foldedRepoNames[FoldName(fullName)]; ok {
		return st.ReposByName[canonical]
	}
	return nil
}

// canonicalRepoKeyLocked maps a possibly case-variant "owner/name" key to its
// canonical spelling, or returns the input unchanged when no repo matches.
// Used by accessors whose secondary maps (hooks, wiki pages, autolinks, …)
// are keyed by the canonical repo key. Caller must hold st.Mu.
func (st *Store) canonicalRepoKeyLocked(fullName string) string {
	if repo := st.RepoByNameLocked(fullName); repo != nil {
		return repo.FullName
	}
	return fullName
}

// canonicalOrgLoginLocked maps a possibly case-variant org login to its
// canonical spelling, or returns the input unchanged when no org matches.
// Used by accessors that build derived keys (memberships, team slugs) from a
// caller-supplied org login. Caller must hold st.Mu.
func (st *Store) canonicalOrgLoginLocked(login string) string {
	if org := st.OrgByLoginLocked(login); org != nil {
		return org.Login
	}
	return login
}

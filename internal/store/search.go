package store

// AuthorAssociation returns the author_association value for the author of
// an issue, pull request or comment within repo. Must not be called with
// st.Mu held; it takes the read lock itself.
func AuthorAssociation(st *Store, authorID int, repo *Repo) string {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return AuthorAssociationLocked(st, authorID, repo)
}

// AuthorAssociationLocked is AuthorAssociation for callers that already hold
// st.Mu; it never acquires the lock itself.
func AuthorAssociationLocked(st *Store, authorID int, repo *Repo) string {
	author := st.Users[authorID]
	if author == nil {
		return "NONE"
	}
	if repo.Owner != nil && repo.Owner.ID == author.ID {
		return "OWNER"
	}
	if repo.Owner != nil && repo.Owner.Type == "Organization" {
		if membership := st.Memberships[MembershipKey(repo.Owner.Login, author.ID)]; membership != nil &&
			membership.State == MembershipStateActive {
			return "MEMBER"
		}
	}
	if collaborators := st.RepoCollaborators[repo.FullName]; collaborators != nil {
		if collaborators[author.Login] != "" {
			return "COLLABORATOR"
		}
	}
	for _, pr := range st.PullRequests {
		if pr.RepoID == repo.ID && pr.AuthorID == author.ID && pr.State == "MERGED" {
			return "CONTRIBUTOR"
		}
	}
	return "NONE"
}

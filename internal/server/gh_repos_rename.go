package bleephub

import (
	"fmt"
)

// renameRepository renames owner/name to newName and carries the artifact
// metadata (which embeds the full name) with it. Both PATCH /repos and the
// GraphQL updateRepository mutation route through here so neither forgets the
// carry — a moved row without a moved artifact index orphans every cache entry
// and workflow artifact. A failed carry is rolled back: the caller sees both
// halves or neither.
func (s *Server) renameRepository(owner, name, newName string) error {
	if !s.store.RenameRepo(owner, name, newName) {
		//lint:ignore ST1005 GitHub API parity requires this exact upstream message.
		return fmt.Errorf("Repository rename failed")
	}
	oldFull := owner + "/" + name
	newFull := owner + "/" + newName
	if err := s.artifactStore.RenameRepository(oldFull, newFull); err != nil {
		if !s.store.RenameRepo(owner, newName, name) {
			return fmt.Errorf("repository artifact metadata rename failed and repository rename rollback failed: %w", err)
		}
		return fmt.Errorf("repository artifact metadata rename failed; repository rename rolled back: %w", err)
	}
	return nil
}

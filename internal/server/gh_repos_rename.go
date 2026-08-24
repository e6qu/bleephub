package bleephub

import (
	"fmt"
)

// renameRepository renames owner/name to newName and carries the artifact
// metadata that embeds the repository's full name across with it.
//
// It is one function rather than one per caller because the carry is the part
// that is easy to forget: a rename that moves the repository row but not the
// artifact index leaves every cache entry and workflow artifact addressed by a
// name nothing answers to. PATCH /repos/{owner}/{repo} and the GraphQL
// updateRepository mutation both rename through here, so neither can be the
// one that forgets.
//
// A failed carry is rolled back, so the caller sees either both halves or
// neither.
func (s *Server) renameRepository(owner, name, newName string) error {
	if !s.store.RenameRepo(owner, name, newName) {
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

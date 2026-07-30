package bleephub

import (
	"context"
	"fmt"
)

// setArtifactStore replaces both references that production construction
// deliberately keeps identical. Tests use an isolated byte store to exercise
// restart and object-storage behavior without exposing a production mutator.
func (s *Server) setArtifactStore(store *ArtifactStore) {
	s.artifactStore = store
	s.store.actionsArtifacts = store
}

// deleteRepositoryActionsData exercises ArtifactStore's standalone durable
// deletion contract. Repository deletion itself uses the crash-consistent
// prepare/commit/cleanup coordinator in Store.
func (s *Server) deleteRepositoryActionsData(ctx context.Context, repoFullName string) error {
	s.artifactStore.mu.RLock()
	artifactIDs := make([]int64, 0)
	cacheIDs := make([]int64, 0)
	for id, artifact := range s.artifactStore.artifacts {
		if artifact.RepoFullName == repoFullName {
			artifactIDs = append(artifactIDs, id)
		}
	}
	for id, entry := range s.artifactStore.caches {
		if entry.Repo == repoFullName {
			cacheIDs = append(cacheIDs, id)
		}
	}
	s.artifactStore.mu.RUnlock()

	for _, id := range artifactIDs {
		if _, err := s.artifactStore.deleteArtifact(ctx, id); err != nil {
			return fmt.Errorf("delete Actions artifact %d: %w", id, err)
		}
	}
	for _, id := range cacheIDs {
		if err := s.removeCacheBytes(ctx, id); err != nil {
			return fmt.Errorf("delete Actions cache %d: %w", id, err)
		}
		s.artifactStore.mu.Lock()
		entry := s.artifactStore.caches[id]
		if entry != nil {
			delete(s.artifactStore.cacheIndex, cacheLookupKey(entry.Repo, entry.Key, entry.Version))
			delete(s.artifactStore.caches, id)
		}
		s.artifactStore.mu.Unlock()
	}
	return nil
}

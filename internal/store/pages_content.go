package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

func (st *Store) LatestPublishedPagesDeployment(repoID int) *PagesDeploymentRecord {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var deployments []*PagesDeploymentRecord
	for _, deployment := range st.PagesDeployments[repoID] {
		if deployment.Status == "succeed" && deployment.ArtifactKey != "" {
			deployments = append(deployments, deployment)
		}
	}
	if len(deployments) == 0 {
		return nil
	}
	sort.Slice(deployments, func(i, j int) bool { return deployments[i].ID > deployments[j].ID })
	copy := *deployments[0]
	return &copy
}

func (st *Store) DeletePagesPublicationData(ctx context.Context, repoID int) error {
	st.Mu.RLock()
	keys, hasDeployments := st.pagesPublicationKeysLocked(repoID)
	st.Mu.RUnlock()
	return st.deletePagesPublicationKeys(ctx, keys, hasDeployments)
}

func (st *Store) pagesPublicationKeysLocked(repoID int) (map[string]struct{}, bool) {
	deployments := st.PagesDeployments[repoID]
	keys := map[string]struct{}{}
	for _, deployment := range deployments {
		if deployment.ArtifactKey != "" {
			keys[deployment.ArtifactKey] = struct{}{}
		}
	}
	return keys, len(deployments) > 0
}

func (st *Store) deletePagesPublicationKeys(ctx context.Context, keys map[string]struct{}, hasDeployments bool) error {
	if st.ObjectByteStore == nil {
		if !hasDeployments {
			return nil
		}
		return errors.New("pages publication deletion requires configured object storage")
	}
	for key := range keys {
		if err := st.ObjectByteStore.Delete(ctx, key); err != nil {
			return fmt.Errorf("delete Pages artifact %s: %w", key, err)
		}
	}
	return nil
}

package bleephub

import (
	"fmt"
	"reflect"
)

// replicaLocalStoreFields are the deliberately process-local parts of Store.
// Everything else exported by Store is durable metadata and is replaced from
// a fresh database snapshot when another dqlite replica advances the revision.
var replicaLocalStoreFields = map[string]struct{}{
	"Agents":           {},
	"AuthCodes":        {},
	"DeviceCodes":      {},
	"Jobs":             {},
	"LogFiles":         {},
	"LogLines":         {},
	"LogMasks":         {},
	"ManifestCodes":    {},
	"NextAgent":        {},
	"NextLog":          {},
	"NextMsg":          {},
	"NextReqID":        {},
	"OIDCLogoutClaims": {},
	"PendingMessages":  {},
	"Sessions":         {},
}

var replicaInfrastructureStoreFields = map[string]struct{}{
	"ObjectByteStore": {},
	"PackageDataDir":  {},
}

// RefreshFromPersistenceIfStale propagates writes made through another
// dqlite connection. Local SQLite is exclusively owned, so it has no peer to
// refresh from and avoids the revision query entirely.
func (st *Store) RefreshFromPersistenceIfStale() error {
	st.mu.RLock()
	recoveryRequired := st.persistenceRecoveryRequired
	st.mu.RUnlock()
	if recoveryRequired {
		return st.ReloadFromPersistence()
	}
	return st.refreshFromPersistence(false)
}

// ReloadFromPersistence discards durable in-memory mutations after a failed
// write. Process-local runner state is preserved just as it is for peer
// refreshes.
func (st *Store) ReloadFromPersistence() error {
	err := st.refreshFromPersistence(true)
	st.mu.Lock()
	st.persistenceRecoveryRequired = err != nil
	st.mu.Unlock()
	return err
}

func (st *Store) refreshFromPersistence(force bool) error {
	return st.refreshFromPersistenceBeforeApply(force, nil)
}

func (st *Store) refreshFromPersistenceBeforeApply(force bool, beforeApply func()) error {
	st.mu.RLock()
	persist := st.persist
	loadedRevision := st.persistenceRevision
	st.mu.RUnlock()
	if persist == nil || (!force && persist.OwnedExclusively()) {
		return nil
	}
	revision, err := persist.StateRevision()
	if err != nil {
		return fmt.Errorf("read replica state revision: %w", err)
	}
	if !force && revision <= loadedRevision {
		return nil
	}
	if !force && revision <= persist.LocalRevision() {
		// This process performed every change since its loaded snapshot; the
		// corresponding Store mutation already updated memory.
		st.mu.Lock()
		if st.persistenceRevision < revision {
			st.persistenceRevision = revision
		}
		st.mu.Unlock()
		return nil
	}

	st.replicaRefreshMu.Lock()
	defer st.replicaRefreshMu.Unlock()
	st.mu.RLock()
	loadedRevision = st.persistenceRevision
	objectByteStore := st.ObjectByteStore
	packageDataDir := st.PackageDataDir
	actionsArtifacts := st.actionsArtifacts
	st.mu.RUnlock()
	revision, err = persist.StateRevision()
	if err != nil {
		return fmt.Errorf("recheck replica state revision: %w", err)
	}
	if !force && revision <= loadedRevision {
		return nil
	}

	for attempt := 0; attempt < 3; attempt++ {
		before, err := persist.StateRevision()
		if err != nil {
			persist.localRevision.Store(loadedRevision)
			return fmt.Errorf("read replica revision before reload: %w", err)
		}
		candidate := NewStore()
		candidate.ObjectByteStore = objectByteStore
		candidate.PackageDataDir = packageDataDir
		candidate.Releases.byteStore = objectByteStore
		candidate.actionsArtifacts = actionsArtifacts
		// A candidate is not yet the live in-memory snapshot. Do not advance
		// Persistence.localRevision while loading it: concurrent request
		// middleware uses that value to decide whether this process has
		// already applied a revision.
		if err := candidate.setPersistence(persist, false); err != nil {
			persist.localRevision.Store(loadedRevision)
			return fmt.Errorf("reload replica state at revision %d: %w", before, err)
		}
		if candidate.persistenceRevision != before {
			continue
		}

		// Closing only the database-side before/after window is insufficient:
		// a local handler can commit after the candidate loads, then release
		// Store.mu immediately before this reconciler acquires it. Recheck the
		// revision while holding Store.mu so a successful local mutation can
		// never be overwritten by an older snapshot.
		if beforeApply != nil {
			beforeApply()
		}
		st.mu.Lock()
		latest, err := persist.StateRevision()
		if err != nil {
			st.mu.Unlock()
			persist.localRevision.Store(loadedRevision)
			return fmt.Errorf("verify replica revision before applying snapshot: %w", err)
		}
		if latest != candidate.persistenceRevision {
			st.mu.Unlock()
			continue
		}

		oldWorkflows := st.Workflows
		localJobs := st.Jobs
		destination := reflect.ValueOf(st).Elem()
		source := reflect.ValueOf(candidate).Elem()
		storeType := destination.Type()
		for index := 0; index < destination.NumField(); index++ {
			field := storeType.Field(index)
			if field.PkgPath != "" { // unexported fields are handled explicitly.
				continue
			}
			if _, local := replicaLocalStoreFields[field.Name]; local {
				continue
			}
			if _, infrastructure := replicaInfrastructureStoreFields[field.Name]; infrastructure {
				continue
			}
			destination.Field(index).Set(source.Field(index))
		}
		// A running local workflow is still owned by this process's runner job.
		// Keep its object identity so completion goroutines do not update a
		// detached pre-refresh pointer. Remote and completed workflows come
		// from the durable snapshot.
		for id, workflow := range oldWorkflows {
			if workflowHasLocalRuntimeJob(workflow, localJobs) {
				st.Workflows[id] = workflow
			}
		}
		// The Workflows map was just replaced (with local-runtime identities
		// re-attached above); the derived run-id and concurrency-group indexes
		// are unexported, so the reflect copy left the old ones in place —
		// recompute them from the merged map.
		st.rebuildWorkflowIndexesLocked()
		st.actionsKeyPair = candidate.actionsKeyPair
		st.persistenceRevision = candidate.persistenceRevision
		persist.localRevision.Store(candidate.persistenceRevision)
		st.mu.Unlock()
		return nil
	}
	persist.localRevision.Store(loadedRevision)
	return fmt.Errorf("shared state kept changing during three replica snapshot attempts")
}

func workflowHasLocalRuntimeJob(workflow *Workflow, jobs map[string]*Job) bool {
	if workflow == nil {
		return false
	}
	for _, job := range workflow.Jobs {
		if jobs[job.JobID] != nil {
			return true
		}
	}
	return false
}

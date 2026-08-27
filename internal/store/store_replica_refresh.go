package store

import (
	"fmt"
	"reflect"
)

// ReplicaLocalStoreFields are the process-local parts of Store. Every other
// exported field is durable metadata, replaced from a fresh snapshot when a
// peer replica advances the revision.
var ReplicaLocalStoreFields = map[string]struct{}{
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

var ReplicaInfrastructureStoreFields = map[string]struct{}{
	"ObjectByteStore": {},
	"PackageDataDir":  {},
}

// ReplicaServerAccessStoreFields are exported (for the server package, per
// ARCH-001) but never copied from a snapshot: locks, clock overrides, injected
// callbacks, derived runtime indexes and process-local caches. Copying them
// would replace a locked mutex, drop a test clock, or import a peer's indexes.
var ReplicaServerAccessStoreFields = map[string]struct{}{
	"ActionsArtifacts":            {},
	"ApiRequestRecordCap":         {},
	"ClockMu":                     {},
	"ClockNow":                    {},
	"CodespaceRuntimeDelete":      {},
	"CodespaceWorkspacePrepare":   {},
	"JobsByPlanID":                {},
	"Logger":                      {},
	"Mu":                          {},
	"PendingRepoCreations":        {},
	"Persist":                     {},
	"PersistenceRecoveryRequired": {},
	"PlanIDByScope":               {},
	"PlanScopes":                  {},
	"RepoPrefixCopy":              {},
	"RepoPrefixDelete":            {},
	"RepoStorageOpen":             {},
	"WikiGitStorages":             {},
	"WikiMu":                      {},
	"WikiProjections":             {},
	"WorkflowsByRunID":            {},
}

// RefreshFromPersistenceIfStale propagates writes made through another dqlite
// connection. Exclusively-owned local SQLite has no peer and skips the query.
func (st *Store) RefreshFromPersistenceIfStale() error {
	st.Mu.RLock()
	recoveryRequired := st.PersistenceRecoveryRequired
	st.Mu.RUnlock()
	if recoveryRequired {
		return st.ReloadFromPersistence()
	}
	return st.refreshFromPersistence(false)
}

// ReloadFromPersistence discards durable in-memory mutations after a failed
// write, preserving process-local runner state as peer refreshes do.
func (st *Store) ReloadFromPersistence() error {
	err := st.refreshFromPersistence(true)
	st.Mu.Lock()
	st.PersistenceRecoveryRequired = err != nil
	st.Mu.Unlock()
	return err
}

func (st *Store) refreshFromPersistence(force bool) error {
	return st.RefreshFromPersistenceBeforeApply(force, nil)
}

func (st *Store) RefreshFromPersistenceBeforeApply(force bool, beforeApply func()) error {
	st.Mu.RLock()
	persist := st.Persist
	loadedRevision := st.persistenceRevision
	st.Mu.RUnlock()
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
		// This process made every change since its snapshot; memory is current.
		st.Mu.Lock()
		if st.persistenceRevision < revision {
			st.persistenceRevision = revision
		}
		st.Mu.Unlock()
		return nil
	}

	st.replicaRefreshMu.Lock()
	defer st.replicaRefreshMu.Unlock()
	st.Mu.RLock()
	loadedRevision = st.persistenceRevision
	objectByteStore := st.ObjectByteStore
	packageDataDir := st.PackageDataDir
	actionsArtifacts := st.ActionsArtifacts
	st.Mu.RUnlock()
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
		candidate.Releases.ByteStore = objectByteStore
		candidate.ActionsArtifacts = actionsArtifacts
		// Don't advance localRevision while loading the candidate: request
		// middleware reads it to tell whether this process applied a revision,
		// and the candidate is not yet live.
		if err := candidate.setPersistence(persist, false); err != nil {
			persist.localRevision.Store(loadedRevision)
			return fmt.Errorf("reload replica state at revision %d: %w", before, err)
		}
		if candidate.persistenceRevision != before {
			continue
		}

		// Recheck the revision while holding Store.Mu: a local handler can commit
		// after the candidate loads and release Store.Mu just before this
		// reconciler acquires it, and an older snapshot must not overwrite it.
		if beforeApply != nil {
			beforeApply()
		}
		st.Mu.Lock()
		latest, err := persist.StateRevision()
		if err != nil {
			st.Mu.Unlock()
			persist.localRevision.Store(loadedRevision)
			return fmt.Errorf("verify replica revision before applying snapshot: %w", err)
		}
		if latest != candidate.persistenceRevision {
			st.Mu.Unlock()
			continue
		}

		oldWorkflows := st.Workflows
		localJobs := st.Jobs
		destination := reflect.ValueOf(st).Elem()
		source := reflect.ValueOf(candidate).Elem()
		storeType := destination.Type()
		for index := 0; index < destination.NumField(); index++ {
			field := storeType.Field(index)
			if field.PkgPath != "" { // unexported: handled explicitly below.
				continue
			}
			if _, local := ReplicaLocalStoreFields[field.Name]; local {
				continue
			}
			if _, infrastructure := ReplicaInfrastructureStoreFields[field.Name]; infrastructure {
				continue
			}
			if _, serverAccess := ReplicaServerAccessStoreFields[field.Name]; serverAccess {
				continue
			}
			destination.Field(index).Set(source.Field(index))
		}
		// Keep the identity of workflows owned by a local runner job so
		// completion goroutines don't update a detached pre-refresh pointer;
		// remote and completed workflows come from the snapshot.
		for id, workflow := range oldWorkflows {
			if workflowHasLocalRuntimeJob(workflow, localJobs) {
				st.Workflows[id] = workflow
			}
		}
		// The reflect copy skipped the unexported run-id and concurrency-group
		// indexes; recompute them from the merged Workflows map.
		st.rebuildWorkflowIndexesLocked()
		st.actionsKeyPair = candidate.actionsKeyPair
		st.persistenceRevision = candidate.persistenceRevision
		persist.localRevision.Store(candidate.persistenceRevision)
		st.Mu.Unlock()
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

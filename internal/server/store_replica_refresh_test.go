package bleephub

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestPersistenceRevisionAdvancesOncePerTransaction(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	p, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	before, err := p.StateRevision()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.PutBatch(
		persistencePut{bucket: "replica_test", key: "one", value: map[string]int{"value": 1}},
		persistencePut{bucket: "replica_test", key: "two", value: map[string]int{"value": 2}},
	); err != nil {
		t.Fatal(err)
	}
	afterBatch, err := p.StateRevision()
	if err != nil {
		t.Fatal(err)
	}
	if afterBatch != before+1 {
		t.Fatalf("batch revision = %d, want %d", afterBatch, before+1)
	}
	if err := p.DeleteBatch(
		persistencePut{bucket: "replica_test", key: "one"},
		persistencePut{bucket: "replica_test", key: "two"},
	); err != nil {
		t.Fatal(err)
	}
	afterDelete, err := p.StateRevision()
	if err != nil {
		t.Fatal(err)
	}
	if afterDelete != afterBatch+1 {
		t.Fatalf("delete batch revision = %d, want %d", afterDelete, afterBatch+1)
	}
}

func TestScheduleClaimIsDurableAndExclusiveAcrossReplicas(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	first, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	minute := time.Date(2026, 7, 29, 12, 30, 0, 0, time.UTC)
	type outcome struct {
		claimed bool
		err     error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	for _, persistence := range []*Persistence{first, second} {
		go func(persistence *Persistence) {
			ready.Done()
			<-start
			claimed, err := persistence.ClaimScheduleFiring("owner/repo\x00workflow.yml\x000 12 * * *", minute)
			results <- outcome{claimed: claimed, err: err}
		}(persistence)
	}
	ready.Wait()
	close(start)
	claims := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.claimed {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("successful schedule claims = %d, want exactly one", claims)
	}
	if claimed, err := first.ClaimScheduleFiring("owner/repo\x00workflow.yml\x000 12 * * *", minute); err != nil || claimed {
		t.Fatalf("persisted duplicate claim = %v, err=%v", claimed, err)
	}
	if claimed, err := second.ClaimScheduleFiring("owner/repo\x00workflow.yml\x000 12 * * *", minute.Add(time.Minute)); err != nil || !claimed {
		t.Fatalf("next-minute claim = %v, err=%v", claimed, err)
	}
}

func TestStoreRefreshPropagatesPeerWritesAndPreservesRuntimeState(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	firstPersistence, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer firstPersistence.Close()
	firstPersistence.dialect.name = "dqlite"
	first := NewStore()
	if err := first.SetPersistence(firstPersistence); err != nil {
		t.Fatal(err)
	}
	first.SeedDefaultUser()

	secondPersistence, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer secondPersistence.Close()
	secondPersistence.dialect.name = "dqlite"
	second := NewStore()
	if err := second.SetPersistence(secondPersistence); err != nil {
		t.Fatal(err)
	}
	second.Agents[77] = &Agent{ID: 77, Name: "local-runner"}

	admin := first.LookupUserByLogin("admin")
	org := first.CreateOrg(admin, "peer-org", "Peer Org", "created on another replica")
	classroom := first.CreateClassroom("Peer Classroom", org.ID, false)
	if second.OrgsByLogin[org.Login] != nil || second.Classrooms[classroom.ID] != nil {
		t.Fatal("second replica observed peer writes before refreshing")
	}
	if err := second.RefreshFromPersistenceIfStale(); err != nil {
		t.Fatal(err)
	}
	if got := second.OrgsByLogin[org.Login]; got == nil || got.Name != org.Name {
		t.Fatalf("organization did not propagate: %+v", got)
	}
	if got := second.Classrooms[classroom.ID]; got == nil || got.Name != classroom.Name {
		t.Fatalf("classroom did not propagate: %+v", got)
	}
	if got := second.Agents[77]; got == nil || got.Name != "local-runner" {
		t.Fatalf("replica refresh discarded process-local runner: %+v", got)
	}
}

func TestArtifactStoreRefreshPropagatesPeerMetadataAndPreservesUploads(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	firstPersistence, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer firstPersistence.Close()
	firstPersistence.dialect.name = "dqlite"
	first := NewArtifactStoreWithByteStore("", nil)
	if err := first.SetPersistence(firstPersistence); err != nil {
		t.Fatal(err)
	}

	secondPersistence, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer secondPersistence.Close()
	secondPersistence.dialect.name = "dqlite"
	second := NewArtifactStoreWithByteStore("", nil)
	if err := second.SetPersistence(secondPersistence); err != nil {
		t.Fatal(err)
	}
	second.artifacts[99] = &Artifact{ID: 99, Name: "uploading", Finalized: false}

	peer := &Artifact{
		ID: 1, Name: "peer", Finalized: true, RepoFullName: "admin/repo",
		CreatedAt: fixedTestTime,
	}
	if err := firstPersistence.Put(actionsArtifactsBucket, "1", peer); err != nil {
		t.Fatalf("persist peer artifact: %v", err)
	}
	if _, ok := second.artifactByID(peer.ID); ok {
		t.Fatal("second replica observed peer artifact before refreshing")
	}
	if err := second.RefreshFromPersistenceIfStale(); err != nil {
		t.Fatal(err)
	}
	if got, ok := second.artifactByID(peer.ID); !ok || got.Name != peer.Name {
		t.Fatalf("peer artifact did not propagate: %#v, %v", got, ok)
	}
	second.mu.RLock()
	inFlight := second.artifacts[99]
	second.mu.RUnlock()
	if inFlight == nil || inFlight.Finalized {
		t.Fatalf("replica refresh discarded in-flight upload: %#v", inFlight)
	}
}

func TestStoreReloadRollsBackUnpersistedMemoryMutation(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	persistence, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer persistence.Close()
	store := NewStore()
	if err := store.SetPersistence(persistence); err != nil {
		t.Fatal(err)
	}
	store.SeedDefaultUser()
	store.Agents[91] = &Agent{ID: 91, Name: "must-survive"}
	store.mu.Lock()
	store.UsersByLogin["admin"].Name = "half-applied mutation"
	store.mu.Unlock()

	if err := store.ReloadFromPersistence(); err != nil {
		t.Fatal(err)
	}
	if got := store.LookupUserByLogin("admin"); got == nil || got.Name != "Admin" {
		t.Fatalf("durable state was not restored: %+v", got)
	}
	if got := store.Agents[91]; got == nil || got.Name != "must-survive" {
		t.Fatalf("rollback discarded process-local state: %+v", got)
	}
}

func TestStoreRefreshCannotOverwriteCommitBetweenSnapshotAndApply(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	firstPersistence, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer firstPersistence.Close()
	firstPersistence.dialect.name = "dqlite"
	first := NewStore()
	if err := first.SetPersistence(firstPersistence); err != nil {
		t.Fatal(err)
	}
	first.SeedDefaultUser()

	secondPersistence, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer secondPersistence.Close()
	secondPersistence.dialect.name = "dqlite"
	second := NewStore()
	if err := second.SetPersistence(secondPersistence); err != nil {
		t.Fatal(err)
	}

	admin := second.LookupUserByLogin("admin")
	if admin == nil {
		t.Fatal("admin did not load on second replica")
	}
	observedBeforeRefresh := secondPersistence.LocalRevision()
	first.CreateOrg(first.LookupUserByLogin("admin"), "peer-before-refresh", "", "")
	var once sync.Once
	err = second.refreshFromPersistenceBeforeApply(false, func() {
		once.Do(func() {
			if got := secondPersistence.LocalRevision(); got != observedBeforeRefresh {
				t.Errorf("candidate snapshot advanced live local revision to %d, want %d", got, observedBeforeRefresh)
			}
			second.mu.Lock()
			admin.Name = "local transaction during refresh"
			second.persist.MustPut("users", "1", admin)
			second.mu.Unlock()
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := second.LookupUserByLogin("admin"); got == nil || got.Name != "local transaction during refresh" {
		t.Fatal("refresh overwrote a local transaction committed after its candidate snapshot")
	}
	if second.OrgsByLogin["peer-before-refresh"] == nil {
		t.Fatal("refresh did not include the peer transaction that triggered it")
	}
}

func TestFailedRollbackPoisonsRequestsUntilPersistenceRecovers(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	persistence, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	if err := store.SetPersistence(persistence); err != nil {
		t.Fatal(err)
	}
	store.SeedDefaultUser()
	store.mu.Lock()
	store.UsersByLogin["admin"].Name = "uncommitted mutation"
	store.mu.Unlock()
	if err := persistence.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.ReloadFromPersistence(); err == nil {
		t.Fatal("reload with a closed database unexpectedly succeeded")
	}
	store.mu.RLock()
	recoveryRequired := store.persistenceRecoveryRequired
	store.mu.RUnlock()
	if !recoveryRequired {
		t.Fatal("failed rollback did not mark the live store as requiring recovery")
	}
	if err := store.RefreshFromPersistenceIfStale(); err == nil {
		t.Fatal("request refresh served state after rollback failed")
	}
}

func TestReplicaRefreshFieldClassificationsAreValid(t *testing.T) {
	storeType := reflect.TypeOf(Store{})
	for category, fields := range map[string]map[string]struct{}{
		"local":          replicaLocalStoreFields,
		"infrastructure": replicaInfrastructureStoreFields,
	} {
		for name := range fields {
			field, ok := storeType.FieldByName(name)
			if !ok {
				t.Errorf("%s replica field %q does not exist", category, name)
				continue
			}
			if field.PkgPath != "" {
				t.Errorf("%s replica field %q is unexported and cannot be copied by the snapshot reconciler", category, name)
			}
			if _, duplicated := replicaLocalStoreFields[name]; duplicated {
				if _, alsoInfrastructure := replicaInfrastructureStoreFields[name]; alsoInfrastructure {
					t.Errorf("replica field %q is both local and infrastructure", name)
				}
			}
		}
	}
}

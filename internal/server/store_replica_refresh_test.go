package bleephub

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

func TestPersistenceRevisionAdvancesOncePerTransaction(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	p, err := store.NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	before, err := p.StateRevision()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.PutBatch(
		store.PersistencePut{Bucket: "replica_test", Key: "one", Value: map[string]int{"value": 1}},
		store.PersistencePut{Bucket: "replica_test", Key: "two", Value: map[string]int{"value": 2}},
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
		store.PersistencePut{Bucket: "replica_test", Key: "one"},
		store.PersistencePut{Bucket: "replica_test", Key: "two"},
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
	first, err := store.NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := store.NewPersistence()
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
	for _, persistence := range []*store.Persistence{first, second} {
		go func(persistence *store.Persistence) {
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

// A released claim (firing failed) can be re-taken, so a transient failure does
// not permanently drop the occurrence.
func TestReleasedScheduleClaimCanBeRetaken(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	p, err := store.NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	key := "owner/repo\x00workflow.yml\x000 12 * * *"
	minute := time.Date(2026, 7, 29, 12, 30, 0, 0, time.UTC)

	if claimed, err := p.ClaimScheduleFiring(key, minute); err != nil || !claimed {
		t.Fatalf("initial claim = %v, err=%v", claimed, err)
	}
	if claimed, err := p.ClaimScheduleFiring(key, minute); err != nil || claimed {
		t.Fatalf("duplicate claim before release = %v, err=%v (want false)", claimed, err)
	}
	if err := p.ReleaseScheduleFiring(key, minute); err != nil {
		t.Fatalf("release: %v", err)
	}
	if claimed, err := p.ClaimScheduleFiring(key, minute); err != nil || !claimed {
		t.Fatalf("re-claim after release = %v, err=%v (want true)", claimed, err)
	}
}

func TestStoreRefreshPropagatesPeerWritesAndPreservesRuntimeState(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	firstPersistence, err := store.NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer firstPersistence.Close()
	firstPersistence.Dialect.Name = "dqlite"
	first := store.NewStore()
	if err := first.SetPersistence(firstPersistence); err != nil {
		t.Fatal(err)
	}
	first.SeedDefaultUser()

	secondPersistence, err := store.NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer secondPersistence.Close()
	secondPersistence.Dialect.Name = "dqlite"
	second := store.NewStore()
	if err := second.SetPersistence(secondPersistence); err != nil {
		t.Fatal(err)
	}
	second.Agents[77] = &store.Agent{ID: 77, Name: "local-runner"}

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
	firstPersistence, err := store.NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer firstPersistence.Close()
	firstPersistence.Dialect.Name = "dqlite"
	first := store.NewArtifactStoreWithByteStore("", nil)
	if err := first.SetPersistence(firstPersistence); err != nil {
		t.Fatal(err)
	}

	secondPersistence, err := store.NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer secondPersistence.Close()
	secondPersistence.Dialect.Name = "dqlite"
	second := store.NewArtifactStoreWithByteStore("", nil)
	if err := second.SetPersistence(secondPersistence); err != nil {
		t.Fatal(err)
	}
	second.Artifacts[99] = &store.Artifact{ID: 99, Name: "uploading", Finalized: false}

	peer := &store.Artifact{
		ID: 1, Name: "peer", Finalized: true, RepoFullName: "admin/repo",
		CreatedAt: fixedTestTime,
	}
	if err := firstPersistence.Put(store.ActionsArtifactsBucket, "1", peer); err != nil {
		t.Fatalf("persist peer artifact: %v", err)
	}
	if _, ok := second.ArtifactByID(peer.ID); ok {
		t.Fatal("second replica observed peer artifact before refreshing")
	}
	if err := second.RefreshFromPersistenceIfStale(); err != nil {
		t.Fatal(err)
	}
	if got, ok := second.ArtifactByID(peer.ID); !ok || got.Name != peer.Name {
		t.Fatalf("peer artifact did not propagate: %#v, %v", got, ok)
	}
	second.Mu.RLock()
	inFlight := second.Artifacts[99]
	second.Mu.RUnlock()
	if inFlight == nil || inFlight.Finalized {
		t.Fatalf("replica refresh discarded in-flight upload: %#v", inFlight)
	}
}

func TestStoreReloadRollsBackUnpersistedMemoryMutation(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	persistence, err := store.NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer persistence.Close()
	st := store.NewStore()
	if err := st.SetPersistence(persistence); err != nil {
		t.Fatal(err)
	}
	st.SeedDefaultUser()
	st.Agents[91] = &store.Agent{ID: 91, Name: "must-survive"}
	st.Mu.Lock()
	st.UsersByLogin["admin"].Name = "half-applied mutation"
	st.Mu.Unlock()

	if err := st.ReloadFromPersistence(); err != nil {
		t.Fatal(err)
	}
	if got := st.LookupUserByLogin("admin"); got == nil || got.Name != "Admin" {
		t.Fatalf("durable state was not restored: %+v", got)
	}
	if got := st.Agents[91]; got == nil || got.Name != "must-survive" {
		t.Fatalf("rollback discarded process-local state: %+v", got)
	}
}

func TestStoreRefreshCannotOverwriteCommitBetweenSnapshotAndApply(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	firstPersistence, err := store.NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer firstPersistence.Close()
	firstPersistence.Dialect.Name = "dqlite"
	first := store.NewStore()
	if err := first.SetPersistence(firstPersistence); err != nil {
		t.Fatal(err)
	}
	first.SeedDefaultUser()

	secondPersistence, err := store.NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer secondPersistence.Close()
	secondPersistence.Dialect.Name = "dqlite"
	second := store.NewStore()
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
	err = second.RefreshFromPersistenceBeforeApply(false, func() {
		once.Do(func() {
			if got := secondPersistence.LocalRevision(); got != observedBeforeRefresh {
				t.Errorf("candidate snapshot advanced live local revision to %d, want %d", got, observedBeforeRefresh)
			}
			second.Mu.Lock()
			admin.Name = "local transaction during refresh"
			second.Persist.MustPut("users", "1", admin)
			second.Mu.Unlock()
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
	persistence, err := store.NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewStore()
	if err := st.SetPersistence(persistence); err != nil {
		t.Fatal(err)
	}
	st.SeedDefaultUser()
	st.Mu.Lock()
	st.UsersByLogin["admin"].Name = "uncommitted mutation"
	st.Mu.Unlock()
	if err := persistence.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.ReloadFromPersistence(); err == nil {
		t.Fatal("reload with a closed database unexpectedly succeeded")
	}
	st.Mu.RLock()
	recoveryRequired := st.PersistenceRecoveryRequired
	st.Mu.RUnlock()
	if !recoveryRequired {
		t.Fatal("failed rollback did not mark the live store as requiring recovery")
	}
	if err := st.RefreshFromPersistenceIfStale(); err == nil {
		t.Fatal("request refresh served state after rollback failed")
	}
}

func TestReplicaRefreshFieldClassificationsAreValid(t *testing.T) {
	storeType := reflect.TypeOf(store.Store{})
	for category, fields := range map[string]map[string]struct{}{
		"local":          store.ReplicaLocalStoreFields,
		"infrastructure": store.ReplicaInfrastructureStoreFields,
		"server-access":  store.ReplicaServerAccessStoreFields,
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
			if _, duplicated := store.ReplicaLocalStoreFields[name]; duplicated {
				if _, alsoInfrastructure := store.ReplicaInfrastructureStoreFields[name]; alsoInfrastructure {
					t.Errorf("replica field %q is both local and infrastructure", name)
				}
			}
		}
	}
}

// TestReplicaRefreshClassificationsCoverDangerousKinds is the ratchet for
// ARCH-004: the snapshot reconciler reflect-copies every exported Store field
// not named in a classification set, so an unclassified field of a dangerous
// kind (a mutex, an injected callback/channel, or the Persistence handle)
// would silently corrupt the live store. Plain durable data is what the
// reconciler exists to copy and is deliberately not required to be classified.
func TestReplicaRefreshClassificationsCoverDangerousKinds(t *testing.T) {
	storeType := reflect.TypeOf(store.Store{})
	for i := 0; i < storeType.NumField(); i++ {
		field := storeType.Field(i)
		if field.PkgPath != "" {
			// The reconciler's copy loop skips unexported fields; they are
			// handled explicitly and cannot be clobbered by the reflect copy.
			continue
		}
		if _, ok := store.ReplicaLocalStoreFields[field.Name]; ok {
			continue
		}
		if _, ok := store.ReplicaInfrastructureStoreFields[field.Name]; ok {
			continue
		}
		if _, ok := store.ReplicaServerAccessStoreFields[field.Name]; ok {
			continue
		}
		if reason := replicaDangerousKind(field.Type, map[reflect.Type]bool{}); reason != "" {
			t.Errorf("store.Store exported field %q is %s, which the replica snapshot reconciler must never copy, "+
				"but it is not named in any classification set. Add %q to the set matching its role in "+
				"internal/store/store_replica_refresh.go: ReplicaLocalStoreFields (process-local runner/session state), "+
				"ReplicaInfrastructureStoreFields (storage handles shared with the candidate), or "+
				"ReplicaServerAccessStoreFields (locks, clock overrides, injected callbacks, derived indexes, the Persistence handle). "+
				"Alternatively make the field unexported if only internal/store needs it — the reconciler skips unexported fields.",
				field.Name, reason, field.Name)
		}
	}
}

// replicaDangerousKind reports why copying a value of type t from a freshly
// loaded candidate snapshot over the live store would be unsafe, or "" when
// the type is plain data. It follows value containment only: struct fields,
// and map/slice/array elements are part of the copied value, while a plain
// pointer swap replaces the whole referent wholesale (the reconciler's
// intended design for sub-stores like *ReactionStore), so pointers are not
// followed — except *store.Persistence itself, which the live store must
// keep as its own handle.
func replicaDangerousKind(t reflect.Type, seen map[reflect.Type]bool) string {
	if seen[t] {
		return ""
	}
	seen[t] = true
	persistenceType := reflect.TypeOf(store.Persistence{})
	if t == persistenceType {
		return "the Persistence handle"
	}
	if t.Kind() == reflect.Ptr && t.Elem() == persistenceType {
		return "a reference to the Persistence handle"
	}
	switch t {
	case reflect.TypeOf(sync.Mutex{}):
		return "a sync.Mutex"
	case reflect.TypeOf(sync.RWMutex{}):
		return "a sync.RWMutex"
	}
	switch t.Kind() {
	case reflect.Func:
		return "a func"
	case reflect.Chan:
		return "a channel"
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if reason := replicaDangerousKind(t.Field(i).Type, seen); reason != "" {
				return fmt.Sprintf("a struct containing %s (via field %s.%s)", reason, t.Name(), t.Field(i).Name)
			}
		}
	case reflect.Map:
		if reason := replicaDangerousKind(t.Key(), seen); reason != "" {
			return fmt.Sprintf("a map keyed by %s", reason)
		}
		if reason := replicaDangerousKind(t.Elem(), seen); reason != "" {
			return fmt.Sprintf("a map of %s", reason)
		}
	case reflect.Slice, reflect.Array:
		if reason := replicaDangerousKind(t.Elem(), seen); reason != "" {
			return fmt.Sprintf("a sequence of %s", reason)
		}
	}
	return ""
}

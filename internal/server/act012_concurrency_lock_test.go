package bleephub

import (
	"context"
	"testing"
	"time"
)

// TestConcurrencyAdmissionSerializesAcrossReplicas pins the ACT-012 wiring:
// on a shared (non-exclusively-owned) database, workflow concurrency admission
// runs under the group's TTL'd database lock — a submission waits while a peer
// replica holds the lock and proceeds once it is released, and the lock is
// released again afterwards. On an exclusively-owned database the lock is
// never touched, so a held lock must not block anything.
func TestConcurrencyAdmissionSerializesAcrossReplicas(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)

	p, err := NewPersistence()
	if err != nil {
		t.Fatalf("open persistence: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	s := newTestServer()
	if err := s.store.SetPersistence(p); err != nil {
		t.Fatalf("SetPersistence: %v", err)
	}

	def := func(name string) *WorkflowDef {
		wd, err := ParseWorkflow([]byte("name: " + name + "\nconcurrency: deploy\njobs:\n  a:\n    runs-on: self-hosted\n    steps:\n      - run: echo hi\n"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return wd
	}
	lockName := actionsConcurrencyLockName("deploy")

	// Exclusively owned (sqlite): a held database lock is irrelevant — the
	// single-process path must skip the lock entirely.
	if ok, err := p.AcquireLock(lockName, "peer", time.Minute); err != nil || !ok {
		t.Fatalf("peer AcquireLock: ok=%v err=%v", ok, err)
	}
	if _, err := s.submitWorkflow(context.Background(), "http://localhost", def("solo"), ""); err != nil {
		t.Fatalf("submit on exclusive database: %v", err)
	}
	if err := p.ReleaseLock(lockName, "peer"); err != nil {
		t.Fatalf("peer ReleaseLock: %v", err)
	}

	// Shared database: make OwnedExclusively report false (the dqlite dialect
	// marker is what distinguishes a shared quorum; the SQL is identical).
	p.dialect.name = "dqlite"

	if ok, err := p.AcquireLock(lockName, "peer", 30*time.Second); err != nil || !ok {
		t.Fatalf("peer re-AcquireLock: ok=%v err=%v", ok, err)
	}
	submitted := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := s.submitWorkflow(context.Background(), "http://localhost", def("blocked"), "")
		submitted <- err
	}()

	select {
	case err := <-submitted:
		t.Fatalf("submission finished in %s with a peer holding the admission lock (err=%v)", time.Since(started), err)
	case <-time.After(250 * time.Millisecond):
		// Still waiting on the peer — expected.
	}

	if err := p.ReleaseLock(lockName, "peer"); err != nil {
		t.Fatalf("release peer lock: %v", err)
	}
	select {
	case err := <-submitted:
		if err != nil {
			t.Fatalf("submission after peer release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("submission never proceeded after the peer released the admission lock")
	}

	// The admission path must have released its own lock.
	if ok, err := p.AcquireLock(lockName, "verifier", time.Minute); err != nil || !ok {
		t.Fatalf("admission lock was not released after submit: ok=%v err=%v", ok, err)
	}
	if err := p.ReleaseLock(lockName, "verifier"); err != nil {
		t.Fatalf("verifier ReleaseLock: %v", err)
	}
}

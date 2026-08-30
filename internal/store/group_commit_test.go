package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
)

const testEncryptionKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

// newGroupCommitPersistence opens a persistence in dir with group commit on.
func newGroupCommitPersistence(t *testing.T, dir string) *Persistence {
	t.Helper()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)
	t.Setenv("BLEEPHUB_PERSISTENCE_ENCRYPTION_KEY", testEncryptionKey)
	t.Setenv("BLEEPHUB_GROUP_COMMIT", "true")
	p, err := NewPersistence()
	if err != nil {
		t.Fatalf("open persistence: %v", err)
	}
	return p
}

// reopenSynchronous reopens dir with group commit OFF, to read back durable state.
func reopenSynchronous(t *testing.T, dir string) *Persistence {
	t.Helper()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)
	t.Setenv("BLEEPHUB_PERSISTENCE_ENCRYPTION_KEY", testEncryptionKey)
	t.Setenv("BLEEPHUB_GROUP_COMMIT", "")
	p, err := NewPersistence()
	if err != nil {
		t.Fatalf("reopen persistence: %v", err)
	}
	return p
}

func TestGroupCommitEnablesOnlyForOwnedSQLite(t *testing.T) {
	p := newGroupCommitPersistence(t, t.TempDir())
	defer p.Close()
	if !p.GroupCommitActive() {
		t.Fatal("group commit should be active for exclusively-owned SQLite with the env set")
	}
}

// TestGroupCommitDurableAfterWait writes through the async path, waits for
// durability, then reopens the database synchronously and confirms every waited
// write survived.
func TestGroupCommitDurableAfterWait(t *testing.T) {
	dir := t.TempDir()
	p := newGroupCommitPersistence(t, dir)
	if !p.GroupCommitActive() {
		t.Fatal("expected group commit active")
	}
	const n = 200
	for i := 0; i < n; i++ {
		if err := p.PutBatch(PersistencePut{Bucket: "kv-test", Key: strconv.Itoa(i), Value: i}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if err := p.WaitDurable(context.Background(), p.EnqueuedSeq()); err != nil {
		t.Fatalf("wait durable: %v", err)
	}
	_ = p.Close()

	reopened := reopenSynchronous(t, dir)
	defer reopened.Close()
	for i := 0; i < n; i++ {
		raw, err := reopened.Get("kv-test", strconv.Itoa(i))
		if err != nil {
			t.Fatalf("get %d after reopen: %v", i, err)
		}
		var got int
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal %d: %v (raw %q)", i, err, raw)
		}
		if got != i {
			t.Fatalf("key %d: got %d, want %d", i, got, i)
		}
	}
}

// TestGroupCommitCounterMatchesSynchronous checks the in-memory allocator returns
// the exact sequence the synchronous SQL allocator would, and that the allocation
// is durable after a reopen.
func TestGroupCommitCounterMatchesSynchronous(t *testing.T) {
	// Reference sequence from the synchronous allocator.
	refDir := t.TempDir()
	ref := reopenSynchronous(t, refDir)
	var want []int64
	mins := []int64{1, 1, 1, 5, 0, 0, 100, 1, 1}
	for _, m := range mins {
		v, err := ref.AllocateCounterValue("seq", m)
		if err != nil {
			t.Fatalf("ref allocate: %v", err)
		}
		want = append(want, v)
	}
	ref.Close()

	dir := t.TempDir()
	p := newGroupCommitPersistence(t, dir)
	var got []int64
	for _, m := range mins {
		v, err := p.AllocateCounterValue("seq", m)
		if err != nil {
			t.Fatalf("gc allocate: %v", err)
		}
		got = append(got, v)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("allocation %d: group-commit gave %d, synchronous gave %d", i, got[i], want[i])
		}
	}
	if err := p.WaitDurable(context.Background(), p.EnqueuedSeq()); err != nil {
		t.Fatalf("wait durable: %v", err)
	}
	p.Close()

	// After reopen the durable counter must not hand back an already-used value.
	reopened := reopenSynchronous(t, dir)
	defer reopened.Close()
	next, err := reopened.AllocateCounterValue("seq", 0)
	if err != nil {
		t.Fatalf("reopen allocate: %v", err)
	}
	last := got[len(got)-1]
	if next <= last {
		t.Fatalf("counter reused after reopen: next=%d must exceed last allocated=%d", next, last)
	}
}

// TestGroupCommitSurvivesCrash re-executes this test binary as a child that
// writes a prefix of records, waits for that prefix to be durable, then hard
// exits (os.Exit) WITHOUT a graceful Close — simulating a crash mid-run. Every
// awaited write must survive; the un-awaited tail may or may not.
func TestGroupCommitSurvivesCrash(t *testing.T) {
	const durablePrefix = 100
	if os.Getenv("BLEEPHUB_GC_CRASH_CHILD") == "1" {
		dir := os.Getenv("BLEEPHUB_DATA_DIR")
		p, err := NewPersistence()
		if err != nil {
			fmt.Fprintln(os.Stderr, "child open:", err)
			os.Exit(2)
		}
		for i := 0; i < durablePrefix; i++ {
			_ = p.PutBatch(PersistencePut{Bucket: "kv-crash", Key: strconv.Itoa(i), Value: i})
		}
		if err := p.WaitDurable(context.Background(), p.EnqueuedSeq()); err != nil {
			fmt.Fprintln(os.Stderr, "child wait:", err)
			os.Exit(2)
		}
		// Enqueue an un-awaited tail, then crash without draining.
		for i := durablePrefix; i < durablePrefix+100; i++ {
			_ = p.PutBatch(PersistencePut{Bucket: "kv-crash", Key: strconv.Itoa(i), Value: i})
		}
		_ = dir
		os.Exit(1) // crash: no Close(), committer may not have flushed the tail
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestGroupCommitSurvivesCrash", "-test.v")
	cmd.Env = append(os.Environ(),
		"BLEEPHUB_GC_CRASH_CHILD=1",
		"BLEEPHUB_PERSIST=true",
		"BLEEPHUB_DATA_DIR="+dir,
		"BLEEPHUB_PERSISTENCE_ENCRYPTION_KEY="+testEncryptionKey,
		"BLEEPHUB_GROUP_COMMIT=true",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child was expected to crash with a non-zero exit; output:\n%s", out)
	}

	reopened := reopenSynchronous(t, dir)
	defer reopened.Close()
	for i := 0; i < durablePrefix; i++ {
		raw, err := reopened.Get("kv-crash", strconv.Itoa(i))
		if err != nil || raw == nil {
			t.Fatalf("awaited write %d lost after crash: raw=%v err=%v\nchild output:\n%s", i, raw, err, out)
		}
	}
}

// TestGroupCommitConcurrentWritersAllDurable hammers the queue from many
// goroutines; every write whose durability was awaited must survive a reopen.
func TestGroupCommitConcurrentWritersAllDurable(t *testing.T) {
	dir := t.TempDir()
	p := newGroupCommitPersistence(t, dir)
	const workers, each = 16, 50
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				key := fmt.Sprintf("%d-%d", w, i)
				if err := p.PutBatch(PersistencePut{Bucket: "kv-conc", Key: key, Value: key}); err != nil {
					t.Errorf("put %s: %v", key, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if err := p.WaitDurable(context.Background(), p.EnqueuedSeq()); err != nil {
		t.Fatalf("wait durable: %v", err)
	}
	p.Close()

	reopened := reopenSynchronous(t, dir)
	defer reopened.Close()
	for w := 0; w < workers; w++ {
		for i := 0; i < each; i++ {
			key := fmt.Sprintf("%d-%d", w, i)
			raw, err := reopened.Get("kv-conc", key)
			if err != nil {
				t.Fatalf("get %s after reopen: %v", key, err)
			}
			var got string
			if err := json.Unmarshal(raw, &got); err != nil || got != key {
				t.Fatalf("key %s lost after reopen: got %q err %v", key, got, err)
			}
		}
	}
}

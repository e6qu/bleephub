package store

import (
	"context"
	"sync"
)

// Group commit (single-node SQLite only, opt-in via BLEEPHUB_GROUP_COMMIT).
//
// Without it, every store write fsyncs synchronously inside the caller's
// Store.Mu critical section (synchronous(FULL)), so durable-write throughput is
// one write per fsync AND fully serialized by the global lock. Because Store.Mu
// is held across the commit, only one writer is ever inside apply() at a time —
// so the fix is not to batch at the fsync (nothing is ever concurrent there) but
// to move the durability WAIT off Store.Mu entirely: apply() enqueues its ops
// and returns immediately, the caller releases Store.Mu, and a single background
// committer fsyncs many writers' ops in one transaction. The HTTP durability
// barrier (server side) withholds each response until the writes it observed are
// durable, so an acknowledged write is exactly as durable as before — a crash
// can only lose writes no client was ever told about (textbook group commit).
//
// This is enabled only when the process owns the database exclusively
// (OwnedExclusively == local SQLite). The dqlite/Raft quorum path keeps the
// synchronous, consensus-ordered body verbatim.

// groupCommitter batches enqueued persistence operations and fsyncs them off the
// hot path. All fields are guarded by mu.
type groupCommitter struct {
	p *Persistence

	mu       sync.Mutex
	pending  []gcEntry
	enqueued int64 // last assigned sequence
	durable  int64 // highest sequence made durable
	failedAt int64 // first sequence whose commit failed (0 = none)
	failErr  error
	counters map[string]int64 // in-memory durable value per counter (async-persisted)
	wake     chan struct{}    // buffered(1): nudges the committer that work is pending
	notify   chan struct{}    // closed and replaced on every durable advance (broadcast)
	closing  bool
	done     chan struct{} // closed when the committer goroutine has exited
}

// gcEntry is one apply() call's worth of work, carrying a monotonic sequence so
// a waiter can block until its own writes are durable.
type gcEntry struct {
	seq      int64
	ops      []persistOp
	counters []gcCounter
}

type gcCounter struct {
	name  string
	value int64
}

func newGroupCommitter(p *Persistence) *groupCommitter {
	gc := &groupCommitter{
		p:        p,
		counters: map[string]int64{},
		wake:     make(chan struct{}, 1),
		notify:   make(chan struct{}),
		done:     make(chan struct{}),
	}
	go gc.run()
	return gc
}

// enqueue appends one batch of put/delete ops and returns its sequence without
// waiting for durability. Ops are already marshaled (PersistBatch.Put marshals
// synchronously, so marshal errors still surface at the call site); the committer
// seals and fsyncs them.
func (gc *groupCommitter) enqueue(ops []persistOp) int64 {
	gc.mu.Lock()
	gc.enqueued++
	seq := gc.enqueued
	gc.pending = append(gc.pending, gcEntry{seq: seq, ops: ops})
	gc.mu.Unlock()
	gc.signal()
	return seq
}

// allocateCounter hands out the next durable value (>= minimum) from an in-memory
// high-water seeded from the database, and enqueues a monotonic raise so the
// allocation survives a crash. It returns synchronously (the caller needs the id
// now); durability rides the next group commit and is gated by the barrier before
// the id reaches any client. Mirrors AllocateCounterValue's semantics: it returns
// max(storedValue, minimum) and advances the stored value to that + 1.
func (gc *groupCommitter) allocateCounter(name string, minimum int64) int64 {
	gc.mu.Lock()
	stored, seeded := gc.counters[name]
	if !seeded {
		// Seed from the durable value once. GetCounter takes p.Mu, so release
		// gc.mu around it (lock order is gc.mu → p.Mu) and re-check after.
		gc.mu.Unlock()
		durable, _ := gc.p.GetCounter(name)
		gc.mu.Lock()
		if s, ok := gc.counters[name]; ok {
			stored = s
		} else {
			stored = durable
			gc.counters[name] = stored
		}
	}
	allocated := stored
	if minimum > allocated {
		allocated = minimum
	}
	next := allocated + 1
	gc.counters[name] = next
	gc.enqueued++
	seq := gc.enqueued
	gc.pending = append(gc.pending, gcEntry{seq: seq, counters: []gcCounter{{name: name, value: next}}})
	gc.mu.Unlock()
	gc.signal()
	return allocated
}

func (gc *groupCommitter) signal() {
	select {
	case gc.wake <- struct{}{}:
	default:
	}
}

// enqueuedSeq reports the highest sequence handed out so far. The barrier reads
// it after a handler returns: it is >= the sequence of anything that handler
// enqueued (the handler only saw earlier writes by holding Store.Mu after they
// were enqueued), so waiting for it to become durable covers the request.
func (gc *groupCommitter) enqueuedSeq() int64 {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	return gc.enqueued
}

// waitDurable blocks until seq is durable, the request is cancelled, or the
// commit that would have covered seq failed (returning that error).
func (gc *groupCommitter) waitDurable(ctx context.Context, seq int64) error {
	for {
		gc.mu.Lock()
		if gc.failedAt != 0 && seq >= gc.failedAt {
			err := gc.failErr
			gc.mu.Unlock()
			return err
		}
		if gc.durable >= seq {
			gc.mu.Unlock()
			return nil
		}
		ch := gc.notify
		gc.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// run drains pending batches and fsyncs each drained group in one transaction.
func (gc *groupCommitter) run() {
	defer close(gc.done)
	for {
		<-gc.wake
		for {
			gc.mu.Lock()
			if len(gc.pending) == 0 {
				closing := gc.closing
				gc.mu.Unlock()
				if closing {
					return
				}
				break
			}
			batch := gc.pending
			gc.pending = nil
			maxSeq := gc.enqueued
			gc.mu.Unlock()

			err := gc.p.commitGroup(batch)

			gc.mu.Lock()
			if err != nil {
				if gc.failedAt == 0 {
					gc.failedAt = batch[0].seq
					gc.failErr = err
				}
			} else {
				gc.durable = maxSeq
			}
			close(gc.notify)
			gc.notify = make(chan struct{})
			gc.mu.Unlock()

			if err != nil {
				// A failed fsync means in-memory state has diverged from disk;
				// halt so no later batch persists on top of a lost write. The
				// barrier turns the pending waiters into 500s + reload.
				return
			}
		}
	}
}

// close drains any pending work, stops the committer, and waits for it to exit.
func (gc *groupCommitter) close() {
	gc.mu.Lock()
	gc.closing = true
	gc.mu.Unlock()
	gc.signal()
	<-gc.done
}

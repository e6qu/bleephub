package gitstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/e6qu/bleephub/gitstore/objstore"
)

// A commit is a compare-and-swap of the manifest. A change to a repository is a
// mutation — a function that edits a draft of the manifest, and may refuse — and
// committing it is: apply it to the manifest this handle holds, write the result
// on the condition that the store's manifest is still the one held, and if it is
// not (412), read the store's, apply the mutation to that, and try again. The
// 412 is the verification. Nothing is read before a write when a manifest is
// held, and nothing but the store arbitrates between writers, whether they are
// goroutines of one process or replicas: there is no lock, in process or out.
//
// A REFUSAL — a reference is not where the caller expected it, a pack to be
// retired is not live — is believed only of a manifest the store has vouched for
// since the commit began. A mutation that refuses on the manifest held is
// therefore applied again to a revalidated one before its caller is told.
//
// GROUP COMMIT. One object takes about one conditional overwrite a second on
// Google Cloud Storage, so commits to one repository that arrive while a swap is
// in flight do not each make their own: they wait, and the next swap applies
// them all, in arrival order, each to the draft the one before left. Each is
// validated on its own, and one that refuses is dropped from the draft and fails
// its own caller only. There is no background goroutine: the caller that finds
// no swap in flight leads, committing every mutation waiting with its own, and
// hands the lead to a waiter if more arrived meanwhile.

const (
	// commitAttempts bounds how many times one group is applied and swapped before
	// its callers are told the repository is too contended to write.
	commitAttempts = 16
	// The pause before another attempt grows from commitBackoffFloor, doubling to
	// commitBackoffCeiling, and is jittered over its whole length so that
	// replicas that collided once do not collide again in step.
	commitBackoffFloor   = 5 * time.Millisecond
	commitBackoffCeiling = time.Second
)

// ErrCommitContended reports that a commit lost the race for the manifest
// commitAttempts times running. Nothing was changed.
var ErrCommitContended = errors.New("gitstore: the repository's manifest is too contended to commit to")

// mutation edits a draft of the manifest, or refuses by returning an error and
// leaving the draft for the committer to discard. It may be run several times,
// once for each manifest the commit is attempted on, and must depend on nothing
// but the draft.
type mutation func(*draft) error

// commitRequest is one caller's mutation waiting to be committed.
type commitRequest struct {
	mutate mutation
	// done is closed once err and state are final.
	done chan struct{}
	// lead is closed to tell the waiting caller that it now leads.
	lead  chan struct{}
	err   error
	state *repoState
}

// committer serializes one repository's commits in this process and groups the
// ones that arrive together.
type committer struct {
	manifests *manifestStore

	mu      sync.Mutex
	leading bool
	waiting []*commitRequest
}

// commit applies mutate to the repository and returns the state that holds it.
func (c *committer) commit(mutate mutation) (*repoState, error) {
	request := &commitRequest{mutate: mutate, done: make(chan struct{}), lead: make(chan struct{})}
	c.mu.Lock()
	c.waiting = append(c.waiting, request)
	if c.leading {
		c.mu.Unlock()
		select {
		case <-request.done:
			return request.state, request.err
		case <-request.lead:
		}
		c.mu.Lock()
	}
	c.leading = true
	group := c.waiting
	c.waiting = nil
	c.mu.Unlock()

	c.swap(group)

	c.mu.Lock()
	if len(c.waiting) > 0 {
		close(c.waiting[0].lead)
	} else {
		c.leading = false
	}
	c.mu.Unlock()
	return request.state, request.err
}

// swap commits a group: every request in it is finished, one way or the other,
// when it returns.
func (c *committer) swap(group []*commitRequest) {
	finish := func(requests []*commitRequest, state *repoState, err error) {
		for _, request := range requests {
			request.state, request.err = state, err
			close(request.done)
		}
	}
	shared := c.manifests.shared
	ctx := shared.baseContext()

	held, err := c.manifests.held()
	if err != nil {
		finish(group, nil, err)
		return
	}
	// vouched says the store has confirmed held since this commit began, which is
	// what a refusal needs before it is believed.
	vouched := false
	// lost counts the swaps this group has lost, which is what it backs off for.
	lost := 0
	for range commitAttempts {
		if lost > 0 {
			if err := pause(ctx, lost); err != nil {
				finish(group, nil, err)
				return
			}
		}
		working := newDraft(held.manifest, held.base, shared.now())
		var accepted, refused []*commitRequest
		for _, request := range group {
			candidate := working.clone()
			if request.err = request.mutate(candidate); request.err != nil {
				refused = append(refused, request)
				continue
			}
			working = candidate
			accepted = append(accepted, request)
		}
		if len(refused) > 0 && !vouched {
			if held, err = c.manifests.read(held); err != nil {
				finish(group, nil, err)
				return
			}
			c.manifests.current.Store(held)
			vouched = true
			continue
		}
		for _, request := range refused {
			close(request.done)
		}
		group = accepted
		if len(group) == 0 {
			return
		}

		next, err := c.write(ctx, held, working)
		if err == nil {
			c.manifests.current.Store(next)
			finish(group, next, nil)
			return
		}
		if !errors.Is(err, objstore.ErrConditionNotMet) {
			finish(group, nil, err)
			return
		}
		if held, err = c.manifests.read(held); err != nil {
			finish(group, nil, err)
			return
		}
		c.manifests.current.Store(held)
		vouched = true
		lost++
	}
	finish(group, nil, fmt.Errorf("%w: %s, after %d attempts", ErrCommitContended, c.manifests.key(), commitAttempts))
}

// write folds the draft's references if they are due it, and swaps the draft in
// for the manifest held. The snapshot a fold writes goes first, under a key
// nothing has used, so that the manifest never names what is not there; if the
// swap is then lost the snapshot is an orphan for a sweep to find.
func (c *committer) write(ctx context.Context, held *repoState, working *draft) (*repoState, error) {
	shared := c.manifests.shared
	at := shared.now()
	base := working.base
	if len(working.manifest.Refs.Changes) > refChangeBound {
		folded := &refSnapshot{refs: working.references()}
		encoded, err := encodeRefSnapshot(folded.refs)
		if err != nil {
			return nil, err
		}
		nonce := make([]byte, 8)
		if _, err := rand.Read(nonce); err != nil {
			return nil, fmt.Errorf("name a reference snapshot: %w", err)
		}
		folded.key = fmt.Sprintf("%s%016x-%s", refSnapshotDirectory, held.manifest.Sequence+1, hex.EncodeToString(nonce))
		if _, err := shared.put(ctx, storeWriteTimeout, c.manifests.prefix+folded.key, bytes.NewReader(encoded), int64(len(encoded)), objstore.IfAbsent()); err != nil {
			return nil, fmt.Errorf("write reference snapshot: %w", err)
		}
		working.manifest.Refs = manifestRefs{Snapshot: folded.key}
		base = folded
	}

	next := working.manifest
	next.Format = manifestFormat
	next.Sequence = held.manifest.Sequence + 1
	encoded, err := next.encode()
	if err != nil {
		return nil, err
	}
	condition := objstore.IfAbsent()
	if held.exists() {
		condition = objstore.IfVersion(held.version)
	}
	version, err := shared.put(ctx, storeWriteTimeout, c.manifests.key(), bytes.NewReader(encoded), int64(len(encoded)), condition)
	if err != nil {
		return nil, err
	}
	return c.manifests.build(&next, version, at, base, held)
}

// pause waits out the backoff before another attempt, or returns early with the
// context's error.
func pause(ctx context.Context, lost int) error {
	ceiling := min(commitBackoffFloor<<min(lost, 16), commitBackoffCeiling)
	jittered, err := rand.Int(rand.Reader, big.NewInt(int64(ceiling)))
	if err != nil {
		return fmt.Errorf("jitter a commit's backoff: %w", err)
	}
	timer := time.NewTimer(time.Duration(jittered.Int64()))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

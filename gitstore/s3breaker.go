package gitstore

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/e6qu/bleephub/gitstore/objstore"
)

// ErrS3Unavailable is returned by a call the circuit breaker fast-fails while
// the object store is deemed down. It is a TRANSIENT error and deliberately not
// "not found": absence is proof a git object or reference does not exist, so
// mapping an outage to absence could let a push overwrite a live branch
// (STORE-037). Fast-failing instead makes a dead S3 return in
// microseconds rather than every goroutine blocking the full per-call timeout
// (and holding the per-repo lock while it waits).
var ErrS3Unavailable = errors.New("gitstore: object store temporarily unavailable (circuit open)")

// s3Breaker is a conservative circuit breaker shared across one Store (and its
// Subs). It trips only after several CONSECUTIVE hard
// failures — a normal 404 or a healthy call resets it — so steady-state traffic
// never trips it; only a genuine outage does. Tuned by Options.BreakerThreshold
// and Options.BreakerCooldown.
type s3Breaker struct {
	mu        sync.Mutex
	threshold int           // consecutive hard failures to trip; <=0 disables the breaker
	cooldown  time.Duration // how long to stay open before a half-open probe
	fails     int
	openUntil time.Time
	now       func() time.Time // injectable for tests
}

func newS3Breaker(threshold int, cooldown time.Duration) *s3Breaker {
	return &s3Breaker{
		threshold: threshold,
		cooldown:  cooldown,
		now:       time.Now,
	}
}

// check reports ErrS3Unavailable while the breaker is open. Once the cooldown
// elapses it lets a single half-open probe through (pushing the window forward
// so concurrent callers keep fast-failing until the probe's outcome is
// recorded). A caller that gets an error from check must NOT make the S3 call
// and must NOT call record.
func (b *s3Breaker) check() error {
	if b == nil || b.threshold <= 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return nil
	}
	if b.now().Before(b.openUntil) {
		return ErrS3Unavailable
	}
	b.openUntil = b.now().Add(b.cooldown)
	return nil
}

// record folds one completed call's outcome into the breaker. Success, and
// every answer that shows the store is there and working — absence, a refused
// condition, a range past the end — reset the failure run; a caller that went
// away says nothing either way; anything else (timeout, throttle, 5xx, network)
// advances the run.
func (b *s3Breaker) record(err error) {
	if b == nil || b.threshold <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err == nil || errors.Is(err, objstore.ErrNotFound) || errors.Is(err, objstore.ErrConditionNotMet) ||
		errors.Is(err, objstore.ErrRangeNotSatisfiable) {
		b.fails = 0
		b.openUntil = time.Time{}
		return
	}
	// A request abandoned by its caller — a client that hung up, a shutdown — is
	// no evidence about the store. A deadline that ran out is, and still counts.
	if errors.Is(err, context.Canceled) {
		return
	}
	b.fails++
	if b.fails >= b.threshold {
		b.openUntil = b.now().Add(b.cooldown)
	}
}

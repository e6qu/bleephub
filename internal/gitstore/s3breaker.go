package gitstore

import (
	"errors"
	"os"
	"strconv"
	"sync"
	"time"
)

// ErrS3Unavailable is returned by a call the circuit breaker fast-fails while
// the object store is deemed down. It is a TRANSIENT error and deliberately not
// os.ErrNotExist: go-git reads os.ErrNotExist as proof a git object/ref is
// absent, so mapping an outage to absence could let a push overwrite a live
// branch (STORE-037). Fast-failing instead makes a dead S3 return in
// microseconds rather than every goroutine blocking the full per-call timeout
// (and holding the per-repo lock while it waits).
var ErrS3Unavailable = errors.New("gitstore: object store temporarily unavailable (circuit open)")

// s3Breaker is a conservative circuit breaker shared across one process's S3
// filesystem (and its chroots). It trips only after several CONSECUTIVE hard
// failures — a normal 404 or a healthy call resets it — so steady-state traffic
// never trips it; only a genuine outage does. Tunable/disable-able via env.
type s3Breaker struct {
	mu        sync.Mutex
	threshold int           // consecutive hard failures to trip; <=0 disables the breaker
	cooldown  time.Duration // how long to stay open before a half-open probe
	fails     int
	openUntil time.Time
	now       func() time.Time // injectable for tests
}

func newS3Breaker() *s3Breaker {
	return &s3Breaker{
		threshold: envIntDefault("BLEEPHUB_S3_BREAKER_THRESHOLD", 5),
		cooldown:  time.Duration(envIntDefault("BLEEPHUB_S3_BREAKER_COOLDOWN_MS", 5000)) * time.Millisecond,
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

// record folds one completed call's outcome into the breaker. isFailure decides
// what counts: a definite 404 (a normal "absent" answer) and success both reset
// the failure run; anything else (timeout, throttle, 5xx, network) advances it.
func (b *s3Breaker) record(err error) {
	if b == nil || b.threshold <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// nil, a raw 404, or the os.ErrNotExist the filesystem maps a 404 to are all
	// normal "absent" answers, not outages — they reset the failure run.
	if err == nil || isNotFound(err) || errors.Is(err, os.ErrNotExist) {
		b.fails = 0
		b.openUntil = time.Time{}
		return
	}
	b.fails++
	if b.fails >= b.threshold {
		b.openUntil = b.now().Add(b.cooldown)
	}
}

func envIntDefault(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

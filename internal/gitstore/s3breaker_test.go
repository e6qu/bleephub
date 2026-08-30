package gitstore

import (
	"errors"
	"os"
	"testing"
	"time"
)

func newTestBreaker(threshold int, cooldown time.Duration, clock *time.Time) *s3Breaker {
	return &s3Breaker{
		threshold: threshold,
		cooldown:  cooldown,
		now:       func() time.Time { return *clock },
	}
}

func TestS3BreakerTripsAfterConsecutiveFailures(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTestBreaker(3, 5*time.Second, &now)
	hard := errors.New("s3 5xx")

	// Below threshold: still closed.
	b.record(hard)
	b.record(hard)
	if err := b.check(); err != nil {
		t.Fatalf("breaker tripped early: %v", err)
	}
	// Third consecutive failure trips it.
	b.record(hard)
	if err := b.check(); !errors.Is(err, ErrS3Unavailable) {
		t.Fatalf("breaker should be open, got %v", err)
	}
}

func TestS3BreakerSuccessAndNotFoundReset(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTestBreaker(3, 5*time.Second, &now)
	hard := errors.New("s3 5xx")

	b.record(hard)
	b.record(hard)
	// A normal absent answer (os.ErrNotExist, as the fs maps a 404) resets the run.
	b.record(os.ErrNotExist)
	b.record(hard)
	b.record(hard)
	if err := b.check(); err != nil {
		t.Fatalf("os.ErrNotExist should have reset the failure run; breaker open: %v", err)
	}
	// A success also resets.
	b.record(hard)
	b.record(nil)
	b.record(hard)
	b.record(hard)
	if err := b.check(); err != nil {
		t.Fatalf("success should have reset the failure run; breaker open: %v", err)
	}
}

func TestS3BreakerHalfOpensAfterCooldown(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTestBreaker(2, 5*time.Second, &now)
	hard := errors.New("s3 5xx")

	b.record(hard)
	b.record(hard)
	if err := b.check(); !errors.Is(err, ErrS3Unavailable) {
		t.Fatalf("breaker should be open")
	}
	// Still within cooldown: fast-fail.
	now = now.Add(3 * time.Second)
	if err := b.check(); !errors.Is(err, ErrS3Unavailable) {
		t.Fatalf("breaker should still be open within cooldown")
	}
	// Cooldown elapsed: a half-open probe is allowed through.
	now = now.Add(3 * time.Second)
	if err := b.check(); err != nil {
		t.Fatalf("breaker should allow a half-open probe: %v", err)
	}
	// A successful probe closes it.
	b.record(nil)
	if err := b.check(); err != nil {
		t.Fatalf("successful probe should close the breaker: %v", err)
	}
}

func TestS3BreakerDisabledWhenThresholdZero(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTestBreaker(0, 5*time.Second, &now)
	for i := 0; i < 100; i++ {
		b.record(errors.New("s3 5xx"))
	}
	if err := b.check(); err != nil {
		t.Fatalf("disabled breaker must never trip: %v", err)
	}
}

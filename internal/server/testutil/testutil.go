// Package testutil holds pure, server-independent test fixtures shared across
// the internal/server test suites. It deliberately imports nothing from the
// server package, so it can be imported by every package's _test.go without a
// cycle, and it is reachable only from tests, so the deadcode gate (which walks
// cmd/bleephub) never sees it. This is the Phase 0 prerequisite for splitting
// internal/server into subpackages (ARCH-001): each extracted package's tests
// need these leaf helpers, which previously lived in the flat package's tests.
package testutil

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// TestEventually polls condition until it returns true or timeout elapses,
// checking immediately and then every interval. It returns condition's final
// value.
func TestEventually(timeout, interval time.Duration, condition func() bool) bool {
	if condition() {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-timer.C:
			return condition()
		case <-ticker.C:
			if condition() {
				return true
			}
		}
	}
}

var testID atomic.Uint64

// NextTestID returns a process-unique monotonic id for naming test fixtures.
func NextTestID() uint64 {
	return testID.Add(1)
}

// FreeLocalAddr returns a currently-free loopback TCP address ("127.0.0.1:port").
func FreeLocalAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen free port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close free port listener: %v", err)
	}
	return addr
}

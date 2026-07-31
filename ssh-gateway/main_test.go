package main

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestSRVTargetsUseRegisteredHostWithSSHPort(t *testing.T) {
	got := sshTargetsFromRecords([]*net.SRV{
		{Target: "task-a.app.bleephub-dev.internal.", Port: 5555},
		{Target: "task-b.app.bleephub-dev.internal.", Port: 5555},
		{Target: "task-a.app.bleephub-dev.internal.", Port: 5555},
	})
	want := []string{
		"task-a.app.bleephub-dev.internal:2222",
		"task-b.app.bleephub-dev.internal:2222",
	}
	if len(got) != len(want) {
		t.Fatalf("target count = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("target[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSRVTargetsIgnoreEmptyTargets(t *testing.T) {
	if got := sshTargetsFromRecords([]*net.SRV{{Target: "."}}); len(got) != 0 {
		t.Fatalf("targets = %v, want none", got)
	}
}

func TestSourceRateLimiterEvictsExpiredSources(t *testing.T) {
	start := time.Date(2042, time.July, 15, 12, 0, 0, 0, time.UTC)
	limiter := &sourceRateLimiter{requests: make(map[string][]time.Time)}
	if !limiter.allow("192.0.2.1:1234", start) {
		t.Fatal("first source was unexpectedly refused")
	}
	if !limiter.allow("192.0.2.2:1234", start.Add(2*time.Minute)) {
		t.Fatal("second source was unexpectedly refused")
	}
	if _, exists := limiter.requests["192.0.2.1"]; exists {
		t.Fatal("expired source remains in the limiter map")
	}
}

func TestIdleDeadlineConnRefreshesDeadlines(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	conn := &idleDeadlineConn{Conn: left, timeout: time.Second}
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(conn, make([]byte, 1))
		readDone <- err
	}()
	if _, err := right.Write([]byte("x")); err != nil {
		t.Fatalf("write through pipe: %v", err)
	}
	if err := <-readDone; err != nil {
		t.Fatalf("read through idle-deadline connection: %v", err)
	}
}

package main

import (
	"net"
	"testing"
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

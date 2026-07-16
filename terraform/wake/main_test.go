package main

import (
	"testing"
	"time"
)

func TestIdleShutdownEnabled(t *testing.T) {
	t.Setenv("IDLE_SHUTDOWN_ENABLED", "true")
	if !idleShutdownEnabled() {
		t.Fatal("expected explicitly enabled idle shutdown")
	}
	t.Setenv("IDLE_SHUTDOWN_ENABLED", "false")
	if idleShutdownEnabled() {
		t.Fatal("expected disabled idle shutdown")
	}
}

func TestIdleShutdownDelay(t *testing.T) {
	t.Setenv("IDLE_SHUTDOWN_MINUTES", "15")
	delay, err := idleShutdownDelay()
	if err != nil {
		t.Fatalf("idleShutdownDelay() error = %v", err)
	}
	if delay != 15*time.Minute {
		t.Fatalf("idleShutdownDelay() = %v, want %v", delay, 15*time.Minute)
	}
}

func TestIdleShutdownDelayRejectsUnsafeValue(t *testing.T) {
	t.Setenv("IDLE_SHUTDOWN_MINUTES", "4")
	if _, err := idleShutdownDelay(); err == nil {
		t.Fatal("idleShutdownDelay() succeeded for less than five minutes")
	}
}

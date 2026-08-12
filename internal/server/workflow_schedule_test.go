package bleephub

import (
	"testing"
	"time"
)

// scheduleRunCounter counts schedule-triggered runs for one repo.
func (s *isolatedServer) scheduleRunCounter(repoKey string) func() int {
	return func() int {
		s.store.Mu.RLock()
		defer s.store.Mu.RUnlock()
		n := 0
		for _, w := range s.store.Workflows {
			if w.RepoFullName == repoKey && w.EventName == "schedule" {
				n++
			}
		}
		return n
	}
}

// A scan that overruns the minute boundary must not silently skip the minutes
// it jumped over: fireSchedulesThrough replays every minute in (lastFired, now].
func TestFireSchedulesThroughReplaysMissedMinutes(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "cronowner/catchup-repo"
	s.cancelRepoRunsCleanup(t, repoKey)
	commitWorkflowYAMLToStorage(t, s.Server, repoKey, ".github/workflows/every5.yml", `name: every5
on:
  schedule:
    - cron: '*/5 * * * *'
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo tick
`)
	countRuns := s.scheduleRunCounter(repoKey)

	// Simulate a tick at 12:12 whose previous processed minute was 12:03 — a
	// nine-minute overrun. Minutes 12:05 and 12:10 are due and must both fire.
	last := time.Date(2026, 6, 12, 12, 3, 0, 0, time.UTC)
	now := time.Date(2026, 6, 12, 12, 12, 30, 0, time.UTC)
	cursor := s.actions.FireSchedulesThrough(last, now)
	if got := countRuns(); got != 2 {
		t.Fatalf("catch-up fired %d runs, want 2 (12:05 and 12:10)", got)
	}
	if want := now.Truncate(time.Minute); !cursor.Equal(want) {
		t.Fatalf("cursor = %v, want %v", cursor, want)
	}
	// The advancing cursor must not reprocess already-fired minutes: a
	// following tick that spans no new due minute fires nothing more.
	s.actions.FireSchedulesThrough(cursor, now.Add(90*time.Second))
	if got := countRuns(); got != 2 {
		t.Fatalf("next tick reprocessed minutes and fired %d runs, want still 2", got)
	}
}

// On a DST fall-back the same wall-clock minute recurs at two UTC instants; a
// timezone'd schedule must fire once, not twice.
func TestFireDueSchedulesDeduplicatesDSTFallBack(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "cronowner/dst-repo"
	s.cancelRepoRunsCleanup(t, repoKey)
	commitWorkflowYAMLToStorage(t, s.Server, repoKey, ".github/workflows/nightly-tz.yml", `name: nightly-tz
on:
  schedule:
    - cron: '30 1 * * *'
      timezone: America/New_York
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo nightly
`)
	countRuns := s.scheduleRunCounter(repoKey)

	// 2026-11-01: US DST ends at 02:00 EDT -> 01:00 EST. Local 01:30 occurs at
	// both 05:30 UTC (EDT) and 06:30 UTC (EST).
	s.actions.FireDueSchedules(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC))
	if got := countRuns(); got != 1 {
		t.Fatalf("first local 01:30 (05:30 UTC) fired %d runs, want 1", got)
	}
	s.actions.FireDueSchedules(time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC))
	if got := countRuns(); got != 1 {
		t.Fatalf("repeated local 01:30 (06:30 UTC) fired %d runs total, want still 1 (no DST double-fire)", got)
	}
}

func TestFireDueSchedules(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repoKey := "cronowner/cron-repo"
	s.cancelRepoRunsCleanup(t, repoKey)
	commitWorkflowYAMLToStorage(t, s.Server, repoKey, ".github/workflows/nightly.yml", `name: nightly
on:
  schedule:
    - cron: '30 4 * * *'
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo nightly
`)

	countRuns := func() int {
		s.store.Mu.RLock()
		defer s.store.Mu.RUnlock()
		n := 0
		for _, w := range s.store.Workflows {
			if w.RepoFullName == repoKey && w.EventName == "schedule" {
				n++
			}
		}
		return n
	}

	// Non-matching minute: nothing fires.
	s.actions.FireDueSchedules(time.Date(2026, 6, 12, 4, 29, 0, 0, time.UTC))
	if got := countRuns(); got != 0 {
		t.Fatalf("4:29 fired %d runs, want 0", got)
	}

	// Matching minute fires exactly once.
	at := time.Date(2026, 6, 12, 4, 30, 0, 0, time.UTC)
	s.actions.FireDueSchedules(at)
	if got := countRuns(); got != 1 {
		t.Fatalf("4:30 fired %d runs, want 1", got)
	}

	// Same minute again: deduped.
	s.actions.FireDueSchedules(at.Add(10 * time.Second))
	if got := countRuns(); got != 1 {
		t.Fatalf("4:30 re-tick fired %d runs total, want still 1", got)
	}

	// Next day fires again.
	s.actions.FireDueSchedules(at.Add(24 * time.Hour))
	if got := countRuns(); got != 2 {
		t.Fatalf("next-day 4:30 fired %d runs total, want 2", got)
	}

	// The run carries schedule event metadata.
	s.store.Mu.RLock()
	var run *Workflow
	for _, w := range s.store.Workflows {
		if w.RepoFullName == repoKey && w.EventName == "schedule" {
			run = w
			break
		}
	}
	s.store.Mu.RUnlock()
	if run == nil {
		t.Fatal("no schedule run found")
	}
	if run.EventPayload["schedule"] != "30 4 * * *" {
		t.Errorf("payload schedule = %v", run.EventPayload["schedule"])
	}
	if run.Ref != "refs/heads/main" && run.Ref != "refs/heads/master" {
		t.Errorf("schedule run ref = %q, want default branch", run.Ref)
	}
}

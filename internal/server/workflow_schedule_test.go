package bleephub

import (
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// ACT-049: the per-minute dispatcher must not re-read and re-parse every
// workflow file every tick. It caches each repo's parsed schedule keyed by the
// default-branch tip commit, rebuilding only when that tip moves.
func TestScheduleIndexRebuildsOnlyOnTipChange(t *testing.T) {
	s := newTestServer()
	repoKey := "cronowner/idx-repo"
	commitWorkflowYAMLToStorage(t, s, repoKey, ".github/workflows/nightly.yml", `name: nightly
on:
  schedule:
    - cron: '30 4 * * *'
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`)

	before := s.scheduleIndex.rebuilds.Load()

	// Two ticks at non-matching minutes with an unchanged tip: the workflow is
	// parsed once and the second tick reuses the cached schedule.
	s.fireDueSchedules(time.Date(2026, 6, 12, 4, 29, 0, 0, time.UTC))
	s.fireDueSchedules(time.Date(2026, 6, 12, 4, 28, 0, 0, time.UTC))
	if got := s.scheduleIndex.rebuilds.Load() - before; got != 1 {
		t.Fatalf("index rebuilt %d times across two unchanged-tip ticks, want 1 (no per-minute reparse)", got)
	}

	// Advancing the default-branch tip invalidates the cache.
	advanceMainTip(t, s, repoKey)
	s.fireDueSchedules(time.Date(2026, 6, 12, 4, 27, 0, 0, time.UTC))
	if got := s.scheduleIndex.rebuilds.Load() - before; got != 2 {
		t.Fatalf("index rebuilt %d times after the branch tip advanced, want 2 (a moved tip must invalidate)", got)
	}
}

// advanceMainTip appends an empty commit (same tree as the current tip, new
// parent+timestamp) to the repo's main branch so its resolved SHA changes while
// the workflow content is preserved.
func advanceMainTip(t *testing.T, s *Server, repoKey string) {
	t.Helper()
	parts := splitRepoKeyParts(repoKey)
	stor := s.store.GetGitStorage(parts[0], parts[1])
	if stor == nil {
		t.Fatalf("no git storage for %s", repoKey)
	}
	mainRef := plumbing.NewBranchReferenceName("main")
	head, err := stor.Reference(mainRef)
	if err != nil {
		t.Fatalf("resolve main: %v", err)
	}
	parent, err := object.GetCommit(stor, head.Hash())
	if err != nil {
		t.Fatalf("load tip commit: %v", err)
	}
	sig := object.Signature{Name: "t", Email: "t@t", When: fixedTestTime.Add(time.Minute)}
	c := &object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      "empty advance",
		TreeHash:     parent.TreeHash,
		ParentHashes: []plumbing.Hash{head.Hash()},
	}
	obj := stor.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		t.Fatalf("encode commit: %v", err)
	}
	h, err := stor.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("store commit: %v", err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(mainRef, h)); err != nil {
		t.Fatalf("advance main: %v", err)
	}
}

func TestParseCron(t *testing.T) {
	cases := []struct {
		expr    string
		t       time.Time
		want    bool
		wantErr bool
	}{
		// every minute parses, but the dispatcher rejects its sub-five-minute interval
		{expr: "* * * * *", t: time.Date(2026, 6, 12, 10, 30, 0, 0, time.UTC), want: true},
		// specific minute/hour
		{expr: "30 10 * * *", t: time.Date(2026, 6, 12, 10, 30, 0, 0, time.UTC), want: true},
		{expr: "30 10 * * *", t: time.Date(2026, 6, 12, 10, 31, 0, 0, time.UTC), want: false},
		// steps
		{expr: "*/15 * * * *", t: time.Date(2026, 6, 12, 10, 45, 0, 0, time.UTC), want: true},
		{expr: "*/15 * * * *", t: time.Date(2026, 6, 12, 10, 40, 0, 0, time.UTC), want: false},
		// ranges with step
		{expr: "0 9-17/2 * * *", t: time.Date(2026, 6, 12, 13, 0, 0, 0, time.UTC), want: true},
		{expr: "0 9-17/2 * * *", t: time.Date(2026, 6, 12, 14, 0, 0, 0, time.UTC), want: false},
		// weekday range (2026-06-12 is a Friday)
		{expr: "0 4 * * 1-5", t: time.Date(2026, 6, 12, 4, 0, 0, 0, time.UTC), want: true},
		{expr: "0 4 * * 1-5", t: time.Date(2026, 6, 14, 4, 0, 0, 0, time.UTC), want: false}, // Sunday
		// names
		{expr: "0 0 * JUN FRI", t: time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC), want: true},
		{expr: "0 0 * JUL *", t: time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC), want: false},
		// dow 7 == Sunday
		{expr: "0 0 * * 7", t: time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC), want: true},
		// dom/dow OR rule: both restricted → either matches
		{expr: "0 0 1 * FRI", t: time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC), want: true},  // Friday, not the 1st
		{expr: "0 0 1 * FRI", t: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), want: true},   // the 1st (a Wednesday)
		{expr: "0 0 1 * FRI", t: time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC), want: false}, // Saturday the 13th
		// dom restricted, dow star → dom decides
		{expr: "0 0 13 * *", t: time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC), want: true},
		{expr: "0 0 13 * *", t: time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC), want: false},
		// lists
		{expr: "0,30 0 * * *", t: time.Date(2026, 6, 12, 0, 30, 0, 0, time.UTC), want: true},
		// errors
		{expr: "* * * *", wantErr: true},     // 4 fields
		{expr: "60 * * * *", wantErr: true},  // minute out of range
		{expr: "* 24 * * *", wantErr: true},  // hour out of range
		{expr: "* * 0 * *", wantErr: true},   // dom out of range
		{expr: "* * * 13 *", wantErr: true},  // month out of range
		{expr: "* * * * 8", wantErr: true},   // dow out of range
		{expr: "*/0 * * * *", wantErr: true}, // zero step
		{expr: "5-1 * * * *", wantErr: true}, // inverted range
		{expr: "x * * * *", wantErr: true},   // garbage
		{expr: "* * * BOB *", wantErr: true}, // bad name
	}
	for _, tc := range cases {
		cs, err := parseCron(tc.expr)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseCron(%q) expected error", tc.expr)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseCron(%q): %v", tc.expr, err)
			continue
		}
		if got := cs.matches(tc.t); got != tc.want {
			t.Errorf("cron %q at %s = %v, want %v", tc.expr, tc.t.Format("2006-01-02 15:04 Mon"), got, tc.want)
		}
	}
}

func TestCronMinimumInterval(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want time.Duration
	}{
		{"* * * * *", time.Minute},
		{"*/5 * * * *", 5 * time.Minute},
		{"0,30 * * * *", 30 * time.Minute},
		{"30 4 * * *", 24 * time.Hour},
	} {
		cs, err := parseCron(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		if got := cs.minimumInterval(); got != tc.want {
			t.Errorf("%s minimum interval = %s, want %s", tc.expr, got, tc.want)
		}
	}
}

// scheduleRunCounter counts schedule-triggered runs for one repo.
func (s *isolatedServer) scheduleRunCounter(repoKey string) func() int {
	return func() int {
		s.store.mu.RLock()
		defer s.store.mu.RUnlock()
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
	cursor := s.fireSchedulesThrough(last, now)
	if got := countRuns(); got != 2 {
		t.Fatalf("catch-up fired %d runs, want 2 (12:05 and 12:10)", got)
	}
	if want := now.Truncate(time.Minute); !cursor.Equal(want) {
		t.Fatalf("cursor = %v, want %v", cursor, want)
	}
	// The advancing cursor must not reprocess already-fired minutes: a
	// following tick that spans no new due minute fires nothing more.
	s.fireSchedulesThrough(cursor, now.Add(90*time.Second))
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
	s.fireDueSchedules(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC))
	if got := countRuns(); got != 1 {
		t.Fatalf("first local 01:30 (05:30 UTC) fired %d runs, want 1", got)
	}
	s.fireDueSchedules(time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC))
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
		s.store.mu.RLock()
		defer s.store.mu.RUnlock()
		n := 0
		for _, w := range s.store.Workflows {
			if w.RepoFullName == repoKey && w.EventName == "schedule" {
				n++
			}
		}
		return n
	}

	// Non-matching minute: nothing fires.
	s.fireDueSchedules(time.Date(2026, 6, 12, 4, 29, 0, 0, time.UTC))
	if got := countRuns(); got != 0 {
		t.Fatalf("4:29 fired %d runs, want 0", got)
	}

	// Matching minute fires exactly once.
	at := time.Date(2026, 6, 12, 4, 30, 0, 0, time.UTC)
	s.fireDueSchedules(at)
	if got := countRuns(); got != 1 {
		t.Fatalf("4:30 fired %d runs, want 1", got)
	}

	// Same minute again: deduped.
	s.fireDueSchedules(at.Add(10 * time.Second))
	if got := countRuns(); got != 1 {
		t.Fatalf("4:30 re-tick fired %d runs total, want still 1", got)
	}

	// Next day fires again.
	s.fireDueSchedules(at.Add(24 * time.Hour))
	if got := countRuns(); got != 2 {
		t.Fatalf("next-day 4:30 fired %d runs total, want 2", got)
	}

	// The run carries schedule event metadata.
	s.store.mu.RLock()
	var run *Workflow
	for _, w := range s.store.Workflows {
		if w.RepoFullName == repoKey && w.EventName == "schedule" {
			run = w
			break
		}
	}
	s.store.mu.RUnlock()
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

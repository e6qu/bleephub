package actions

import (
	"strings"
	"testing"
	"time"

	memfs "github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog"

	"github.com/e6qu/bleephub/internal/store"
)

// fixedEngineTestTime mirrors the server test fixture's frozen clock.
var fixedEngineTestTime = time.Date(2042, time.July, 15, 12, 0, 0, 0, time.UTC)

// nopSink drops engine lifecycle events; these tests assert scheduling
// behavior, not webhook rendering (which lives in the server).
type nopSink struct{}

func (nopSink) WorkflowRunEvent(*store.Workflow, string)                     {}
func (nopSink) WorkflowJobEvent(*store.Workflow, *store.WorkflowJob, string) {}
func (nopSink) CheckRunEvent(string, int64, string)                          {}
func (nopSink) CheckSuiteEvent(string, int64, string)                        {}

// newTestEngine builds an engine over a fresh in-memory store with stub
// seams, the direct analogue of the server package's newTestServer for
// engine-internal tests.
func newTestEngine() *Engine {
	st := store.NewStore()
	return NewEngine(Config{
		Store:  st,
		Logger: zerolog.Nop(),
		Addr:   "127.0.0.1:0",
		Events: nopSink{},
		MintJobToken: func(scopeID string, wf *store.Workflow, jd *store.JobDef) string {
			return "test-job-token"
		},
		RepoEventPayload: func(repo *store.Repo) map[string]interface{} {
			return map[string]interface{}{"full_name": repo.FullName}
		},
		Now:                   func() time.Time { return fixedEngineTestTime },
		Go:                    func(fn func()) { go fn() },
		CompletedJobRetention: 6 * time.Hour,
	})
}

// commitWorkflowYAMLToStore writes one workflow file into a fresh repo's git
// storage at the main branch tip (the engine-side port of the server test
// helper commitWorkflowYAMLToStorage).
func commitWorkflowYAMLToStore(t *testing.T, st *store.Store, repoFullName, path, body string) string {
	t.Helper()
	parts := strings.Split(repoFullName, "/")
	if len(parts) != 2 {
		t.Fatalf("expected owner/repo, got %q", repoFullName)
	}
	st.Mu.Lock()
	user := &store.User{ID: st.NextUser, Login: parts[0], Type: "User", CreatedAt: fixedEngineTestTime, UpdatedAt: fixedEngineTestTime}
	st.NextUser++
	st.Users[user.ID] = user
	st.UsersByLogin[user.Login] = user
	st.Mu.Unlock()
	st.CreateRepo(user, parts[1], "", false) // creates the GitStorage entry too
	storer := st.GetGitStorage(parts[0], parts[1])
	if storer == nil {
		t.Fatalf("no git storage for %s after CreateRepo", repoFullName)
	}
	fs := memfs.New()
	repo, err := git.Init(storer, fs)
	if err != nil {
		repo, err = git.Open(storer, fs)
		if err != nil {
			t.Fatalf("init/open repo: %v", err)
		}
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	f, err := fs.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	_, _ = f.Write([]byte(body))
	_ = f.Close()
	if _, err := wt.Add(path); err != nil {
		t.Fatalf("git add %s: %v", path, err)
	}
	commitHash, err := wt.Commit("add "+path, &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: fixedEngineTestTime},
	})
	if err != nil {
		t.Fatalf("git commit: %v", err)
	}
	mainRef := plumbing.NewBranchReferenceName("main")
	if err := storer.SetReference(plumbing.NewHashReference(mainRef, commitHash)); err != nil {
		t.Fatalf("set main ref: %v", err)
	}
	if err := storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, mainRef)); err != nil {
		t.Fatalf("set HEAD: %v", err)
	}
	return commitHash.String()
}

// ACT-049: the per-minute dispatcher must not re-read and re-parse every
// workflow file every tick. It caches each repo's parsed schedule keyed by the
// default-branch tip commit, rebuilding only when that tip moves.
func TestScheduleIndexRebuildsOnlyOnTipChange(t *testing.T) {
	e := newTestEngine()
	repoKey := "cronowner/idx-repo"
	commitWorkflowYAMLToStore(t, e.store, repoKey, ".github/workflows/nightly.yml", `name: nightly
on:
  schedule:
    - cron: '30 4 * * *'
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`)

	before := e.scheduleIndex.rebuilds.Load()

	// Two ticks at non-matching minutes with an unchanged tip: the workflow is
	// parsed once and the second tick reuses the cached schedule.
	e.FireDueSchedules(time.Date(2026, 6, 12, 4, 29, 0, 0, time.UTC))
	e.FireDueSchedules(time.Date(2026, 6, 12, 4, 28, 0, 0, time.UTC))
	if got := e.scheduleIndex.rebuilds.Load() - before; got != 1 {
		t.Fatalf("index rebuilt %d times across two unchanged-tip ticks, want 1 (no per-minute reparse)", got)
	}

	// Advancing the default-branch tip invalidates the cache.
	advanceMainTip(t, e.store, repoKey)
	e.FireDueSchedules(time.Date(2026, 6, 12, 4, 27, 0, 0, time.UTC))
	if got := e.scheduleIndex.rebuilds.Load() - before; got != 2 {
		t.Fatalf("index rebuilt %d times after the branch tip advanced, want 2 (a moved tip must invalidate)", got)
	}
}

// advanceMainTip appends an empty commit (same tree as the current tip, new
// parent+timestamp) to the repo's main branch so its resolved SHA changes while
// the workflow content is preserved.
func advanceMainTip(t *testing.T, st *store.Store, repoKey string) {
	t.Helper()
	parts := SplitRepoKeyParts(repoKey)
	stor := st.GetGitStorage(parts[0], parts[1])
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
	sig := object.Signature{Name: "t", Email: "t@t", When: fixedEngineTestTime.Add(time.Minute)}
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

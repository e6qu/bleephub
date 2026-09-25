package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
)

// gitScenarioOrder is the git level's run. It has no probe or maintenance
// scenario: what a remote does between requests is its own business, and its
// cost is charged to the request that caused it (see settle).
var gitScenarioOrder = []string{
	scenarioPushInitial, scenarioReplicaStart, scenarioCloneCold, scenarioCloneWarm, scenarioPushIncremental, scenarioFetch, scenarioCloneParallel,
}

// everyScenario is every scenario either level runs, the Storer level's first,
// each named once.
func everyScenario() []string {
	names := append([]string(nil), scenarioOrder...)
	for _, name := range gitScenarioOrder {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// clientRepository is the pusher's side of the workload: a bare repository on
// local disk holding every object, with one ref per push so each can be sent.
type clientRepository struct {
	dir string
}

func newClientRepository(ctx context.Context, env Env, workload *Workload) (*clientRepository, error) {
	dir, err := env.tempDir("client-*.git")
	if err != nil {
		return nil, err
	}
	if _, err := runGit(ctx, "", nil, nil, "init", "--quiet", "--bare", "--initial-branch=main", dir); err != nil {
		return nil, err
	}
	for _, push := range append([]Push{workload.Initial}, workload.Incremental...) {
		if _, err := runGit(ctx, dir, nil, push.Pack, "index-pack", "--stdin"); err != nil {
			return nil, fmt.Errorf("load the workload into the client repository: %w", err)
		}
	}
	return &clientRepository{dir: dir}, nil
}

// gitRunner drives one remote through the git-level scenarios for one run.
type gitRunner struct {
	driver   RemoteDriver
	meter    *Meter
	workload *Workload
	client   *clientRepository
	env      Env
	repo     string
	run      int
	parallel int
	selected map[string]bool
}

// settle waits for the object store to go quiet. A server may do work after it
// has answered — bleephub compacts in the background once a push returns — and
// that work is a cost of the request that caused it. Waiting here charges it to
// the right scenario instead of letting it leak into the next one's count.
func (r *gitRunner) settle() {
	const quiet, limit = 400 * time.Millisecond, 60 * time.Second
	deadline := time.Now().Add(limit)
	last, since := r.meter.Snapshot().Total(), time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if now := r.meter.Snapshot().Total(); now != last {
			last, since = now, time.Now()
			continue
		}
		if time.Since(since) >= quiet {
			return
		}
	}
}

func (r *gitRunner) measure(scenario string, ops int, body func(result *Result) error) Result {
	result := Result{Driver: r.driver.Name(), Scenario: scenario, Run: r.run, Ops: ops, Level: levelGit}
	before := r.meter.Snapshot()
	start := time.Now()
	err := body(&result)
	// Elapsed is what the git client waited; the requests include whatever the
	// remote went on to do afterwards.
	result.Elapsed = time.Since(start)
	r.settle()
	result.S3 = r.meter.Snapshot().Sub(before)
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

// measureCold measures a scenario against a replica that has served nothing: the
// remote is restarted, and has gone quiet, before the clock and the request count
// start. What a server does when it starts — opening its store, proving the
// store keeps its promises, fetching what it means to serve — is a cost of
// starting, paid once however many clones follow, and billing it to the first of
// them made a cold clone look like a cold boot. It is not hidden either: start
// is where it is reported, because designs differ in how much they do there. One
// that materializes its repositories as it starts answers its first clone from
// disk, and has paid for that here.
func (r *gitRunner) measureCold(ctx context.Context, scenario string, ops int, body func(result *Result) error) (start, measured Result) {
	start = r.measure(scenarioReplicaStart, 1, func(*Result) error { return r.driver.Restart(ctx) })
	if start.Error != "" {
		return start, Result{Driver: r.driver.Name(), Scenario: scenario, Run: r.run, Ops: ops, Level: levelGit, Error: start.Error}
	}
	return start, r.measure(scenario, ops, body)
}

func (r *gitRunner) execute(ctx context.Context) []Result {
	var results []Result
	keep := func(result Result) bool {
		if r.selected[result.Scenario] {
			results = append(results, result)
		}
		return result.Error == ""
	}
	remote, err := r.driver.Remote(ctx, r.repo)
	if err != nil {
		return []Result{{Driver: r.driver.Name(), Scenario: scenarioPushInitial, Run: r.run, Level: levelGit, Error: err.Error()}}
	}
	initial := r.workload.Initial
	final := initial
	if n := len(r.workload.Incremental); n > 0 {
		final = r.workload.Incremental[n-1]
	}
	gitEnv := r.driver.GitEnv()

	// A push is of a branch, as a developer's is: the client's main is moved to
	// the step's tip and pushed by name. Pushing a bare commit id would be
	// equivalent to a server, but remote helpers bundle the ref they are given
	// and cannot bundle an id.
	push := func(tip plumbing.Hash) error {
		if _, err := runGit(ctx, r.client.dir, nil, nil, "update-ref", "refs/heads/main", tip.String()); err != nil {
			return err
		}
		_, err := runGit(ctx, r.client.dir, gitEnv, nil, "push", "--quiet", remote, "refs/heads/main:refs/heads/main")
		return err
	}
	if !keep(r.measure(scenarioPushInitial, 1, func(*Result) error { return push(initial.Tip) })) {
		return results
	}

	var firstClone string
	started, cold := r.measureCold(ctx, scenarioCloneCold, 1, func(result *Result) error {
		dir, err := r.clone(ctx, remote, gitEnv, initial.Tip, initial.Objects, result)
		firstClone = dir
		return err
	})
	keep(started)
	keep(cold)
	keep(r.measure(scenarioCloneWarm, 1, func(result *Result) error {
		_, err := r.clone(ctx, remote, gitEnv, initial.Tip, initial.Objects, result)
		return err
	}))

	if len(r.workload.Incremental) > 0 {
		if !keep(r.measure(scenarioPushIncremental, len(r.workload.Incremental), func(*Result) error {
			for _, next := range r.workload.Incremental {
				if err := push(next.Tip); err != nil {
					return err
				}
			}
			return nil
		})) {
			return results
		}
		if firstClone != "" {
			keep(r.measure(scenarioFetch, 1, func(result *Result) error {
				if _, err := runGit(ctx, firstClone, gitEnv, nil, "fetch", "--quiet", remote, "+refs/heads/main:refs/heads/main"); err != nil {
					return err
				}
				return verifyClone(ctx, firstClone, final.Tip, r.workload.Objects, result)
			}))
		}
	}

	_, parallel := r.measureCold(ctx, scenarioCloneParallel, r.parallel, func(result *Result) error {
		var wg sync.WaitGroup
		errs := make([]error, r.parallel)
		for worker := range r.parallel {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var each Result
				_, errs[worker] = r.clone(ctx, remote, gitEnv, final.Tip, r.workload.Objects, &each)
			}()
		}
		wg.Wait()
		result.Objects = r.workload.Objects
		return errors.Join(errs...)
	})
	keep(parallel)
	return results
}

// clone makes a bare clone and checks it. Bare, because a checkout is the
// client's own disk work and the same for every remote.
func (r *gitRunner) clone(ctx context.Context, remote string, gitEnv []string, wantTip plumbing.Hash, wantObjects int, result *Result) (string, error) {
	parent, err := r.env.tempDir("clone-*")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(parent, "clone.git")
	if _, err := runGit(ctx, "", gitEnv, nil, "clone", "--quiet", "--bare", remote, dir); err != nil {
		return "", err
	}
	return dir, verifyClone(ctx, dir, wantTip, wantObjects, result)
}

// verifyClone is the git level's guard against a remote that wins by losing
// data: the branch must be where the pusher left it, every object reachable
// from it must be listed, and each of them must actually be there to read.
func verifyClone(ctx context.Context, dir string, wantTip plumbing.Hash, wantObjects int, result *Result) error {
	tip, err := runGit(ctx, dir, nil, nil, "rev-parse", "refs/heads/main")
	if err != nil {
		return err
	}
	if strings.TrimSpace(tip) != wantTip.String() {
		return fmt.Errorf("refs/heads/main is %s, want %s", strings.TrimSpace(tip), wantTip)
	}
	listed, err := runGit(ctx, dir, nil, nil, "rev-list", "--objects", "refs/heads/main")
	if err != nil {
		return err
	}
	objects := strings.Count(listed, "\n")
	if objects != wantObjects {
		return fmt.Errorf("clone holds %d objects reachable from the branch, want %d", objects, wantObjects)
	}
	// Every reachable object must be there to read. This asks git for each one
	// rather than running fsck, which also validates auxiliary files (a pack's
	// reverse index, say) that are git's to rebuild and no part of the
	// repository: a remote that deposits a pack without one has lost nothing.
	var ids strings.Builder
	for _, line := range strings.Split(listed, "\n") {
		if id, _, _ := strings.Cut(line, " "); id != "" {
			ids.WriteString(id + "\n")
		}
	}
	checked, err := runGit(ctx, dir, nil, []byte(ids.String()), "cat-file", "--batch-check")
	if err != nil {
		return err
	}
	if missing := strings.Count(checked, " missing"); missing != 0 {
		return fmt.Errorf("%d of the %d reachable objects are missing from the clone", missing, objects)
	}
	result.Objects = objects
	if size, err := directorySize(filepath.Join(dir, "objects")); err == nil {
		result.PackBytes = size
	}
	return nil
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err == nil {
			total += info.Size()
		}
		return err
	})
	return total, err
}

// customRemote parses a -remote flag value, name=template.
func customRemote(spec string) (*helperRemote, error) {
	name, template, ok := strings.Cut(spec, "=")
	if !ok || name == "" || !strings.Contains(template, "{repo}") {
		return nil, fmt.Errorf("-remote wants name=url-template containing {repo}, got %q", spec)
	}
	remote := &helperRemote{name: name, describe: "custom remote " + strconv.Quote(template), template: template}
	// A template naming the run's endpoint or bucket is a helper that keeps its
	// data there, configured as the S3 helpers are; any other is a server that
	// keeps its own.
	for _, placeholder := range []string{"{endpoint}", "{host}", "{bucket}", "{prefix}"} {
		if strings.Contains(template, placeholder) {
			remote.stores = []string{storeS3}
		}
	}
	return remote, nil
}

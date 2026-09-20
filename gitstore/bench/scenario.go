package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/e6qu/bleephub/gitstore/s3fake"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// Scenario names, in the order a run executes them. The order is part of the
// design: each scenario measures the repository the ones before it left behind,
// the way a server meets a repository over its life.
const (
	scenarioPushInitial     = "push-initial"
	scenarioCloneCold       = "clone-cold"
	scenarioCloneWarm       = "clone-warm"
	scenarioPushIncremental = "push-incremental"
	scenarioFetch           = "fetch-incremental"
	scenarioProbeAbsent     = "probe-absent"
	scenarioMaintain        = "maintain"
	scenarioCloneMaintained = "clone-cold-maintained"
	scenarioProbeMaintained = "probe-absent-maintained"
	scenarioCloneParallel   = "clone-parallel"
)

var scenarioOrder = []string{
	scenarioPushInitial, scenarioCloneCold, scenarioCloneWarm, scenarioPushIncremental, scenarioFetch,
	scenarioProbeAbsent, scenarioMaintain, scenarioCloneMaintained, scenarioProbeMaintained, scenarioCloneParallel,
}

var scenarioNotes = map[string]string{
	scenarioPushInitial:     "ingest the whole starting history as one packfile, as receive-pack does",
	scenarioCloneCold:       "full clone by a replica with no local cache: walk the history, encode the pack",
	scenarioCloneWarm:       "the same clone again on a replica that has served it once",
	scenarioPushIncremental: "single-commit pushes, each a compare-and-set of the branch",
	scenarioFetch:           "a client holding the initial tip fetches everything pushed since",
	scenarioProbeAbsent:     "fetch negotiation asking about objects the repository does not have",
	scenarioMaintain:        "the driver's own housekeeping (compaction); skipped where there is none",
	scenarioCloneMaintained: "cold full clone after housekeeping",
	scenarioProbeMaintained: "the same negotiation probes after housekeeping",
	scenarioCloneParallel:   "concurrent full clones from a cold start, to show how a replica scales",
}

// Result is one scenario measured once against one driver.
type Result struct {
	Driver   string        `json:"driver"`
	Scenario string        `json:"scenario"`
	Run      int           `json:"run"`
	Elapsed  time.Duration `json:"elapsed_ns"`
	S3       s3fake.Counts `json:"s3"`
	// Ops is how many logical operations the scenario performed (pushes,
	// probes, clones), for per-operation figures.
	Ops int `json:"ops"`
	// Objects and PackBytes describe what a clone or fetch produced.
	Objects   int    `json:"objects,omitempty"`
	PackBytes int64  `json:"pack_bytes,omitempty"`
	Skipped   string `json:"skipped,omitempty"`
	Error     string `json:"error,omitempty"`
}

// runner drives one driver through the scenarios for one run.
type runner struct {
	driver   StorerDriver
	meter    *Meter
	workload *Workload
	repo     string
	run      int
	parallel int
	probes   int
	selected map[string]bool
}

func (r *runner) measure(scenario string, ops int, body func(result *Result) error) Result {
	result := Result{Driver: r.driver.Name(), Scenario: scenario, Run: r.run, Ops: ops}
	before := r.meter.Snapshot()
	start := time.Now()
	err := body(&result)
	result.Elapsed = time.Since(start)
	result.S3 = r.meter.Snapshot().Sub(before)
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

// execute runs every scenario. Scenarios the user deselected still execute when
// a later one depends on the state they leave — a clone needs the push — but
// only selected ones are reported.
func (r *runner) execute(ctx context.Context) []Result {
	var results []Result
	keep := func(result Result) bool {
		if r.selected[result.Scenario] {
			results = append(results, result)
		}
		return result.Error == ""
	}
	initial := r.workload.Initial
	final := initial
	if n := len(r.workload.Incremental); n > 0 {
		final = r.workload.Incremental[n-1]
	}

	if !keep(r.measure(scenarioPushInitial, 1, func(*Result) error {
		return r.push(ctx, initial, plumbing.ZeroHash)
	})) {
		return results
	}
	keep(r.measure(scenarioCloneCold, 1, func(result *Result) error {
		return r.clone(ctx, true, initial.Tip, nil, initial.Objects, result)
	}))
	keep(r.measure(scenarioCloneWarm, 1, func(result *Result) error {
		return r.clone(ctx, false, initial.Tip, nil, initial.Objects, result)
	}))

	if len(r.workload.Incremental) > 0 {
		if !keep(r.measure(scenarioPushIncremental, len(r.workload.Incremental), func(*Result) error {
			previous := initial.Tip
			for _, push := range r.workload.Incremental {
				if err := r.push(ctx, push, previous); err != nil {
					return err
				}
				previous = push.Tip
			}
			return nil
		})) {
			return results
		}
		keep(r.measure(scenarioFetch, 1, func(result *Result) error {
			return r.clone(ctx, false, final.Tip, []plumbing.Hash{initial.Tip}, 0, result)
		}))
	}

	keep(r.measure(scenarioProbeAbsent, r.probes, func(*Result) error {
		return r.probeAbsent(ctx, 0)
	}))

	maintained := r.measure(scenarioMaintain, 1, func(result *Result) error {
		stor, err := r.driver.Open(ctx, r.repo, false)
		if err != nil {
			return err
		}
		ran, err := r.driver.Maintain(ctx, stor)
		if !ran {
			result.Skipped = "driver has no maintenance"
		}
		return err
	})
	keep(maintained)
	if maintained.Skipped == "" {
		keep(r.measure(scenarioCloneMaintained, 1, func(result *Result) error {
			return r.clone(ctx, true, final.Tip, nil, r.workload.Objects, result)
		}))
		keep(r.measure(scenarioProbeMaintained, r.probes, func(*Result) error {
			// Different objects from the first round, so no driver answers
			// from what it remembered of being asked before.
			return r.probeAbsent(ctx, r.probes)
		}))
	}

	keep(r.measure(scenarioCloneParallel, r.parallel, func(result *Result) error {
		// One cold open stands for the replica starting; the clones then share
		// whatever the driver shares between requests.
		if _, err := r.driver.Open(ctx, r.repo, true); err != nil {
			return err
		}
		var wg sync.WaitGroup
		errs := make([]error, r.parallel)
		packBytes := make([]int64, r.parallel)
		for worker := range r.parallel {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var each Result
				errs[worker] = r.clone(ctx, false, final.Tip, nil, r.workload.Objects, &each)
				packBytes[worker] = each.PackBytes
			}()
		}
		wg.Wait()
		result.Objects = r.workload.Objects
		for _, n := range packBytes {
			result.PackBytes += n
		}
		return errors.Join(errs...)
	}))
	return results
}

// probeAbsent asks about objects the repository does not hold, which is what a
// fetch negotiation does with every "have" line the server has never seen.
func (r *runner) probeAbsent(ctx context.Context, offset int) error {
	stor, err := r.driver.Open(ctx, r.repo, false)
	if err != nil {
		return err
	}
	for probe := range r.probes {
		if err := stor.HasEncodedObject(absentHash(offset + probe)); err == nil {
			return fmt.Errorf("absent object %d reported present", offset+probe)
		} else if !errors.Is(err, plumbing.ErrObjectNotFound) {
			return err
		}
	}
	return nil
}

// push ingests one packfile and moves the branch, which is what receive-pack
// does with a push. A zero previous tip is the first push to an empty repository.
func (r *runner) push(ctx context.Context, push Push, previous plumbing.Hash) error {
	stor, err := r.driver.Open(ctx, r.repo, false)
	if err != nil {
		return err
	}
	if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(push.Pack)); err != nil {
		return fmt.Errorf("ingest pack: %w", err)
	}
	next := plumbing.NewHashReference(benchBranch, push.Tip)
	if previous.IsZero() {
		return stor.SetReference(next)
	}
	return stor.CheckAndSetReference(next, plumbing.NewHashReference(benchBranch, previous))
}

// clone resolves the branch, walks what the client lacks and encodes it as a
// packfile, which is what upload-pack does for a clone (no haves) or a fetch.
func (r *runner) clone(ctx context.Context, cold bool, wantTip plumbing.Hash, haves []plumbing.Hash, wantObjects int, result *Result) error {
	stor, err := r.driver.Open(ctx, r.repo, cold)
	if err != nil {
		return err
	}
	ref, err := stor.Reference(benchBranch)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", benchBranch, err)
	}
	if ref.Hash() != wantTip {
		return fmt.Errorf("%s is %s, want %s", benchBranch, ref.Hash(), wantTip)
	}
	return encodeReachable(stor, wantTip, haves, wantObjects, result)
}

func encodeReachable(stor storer.Storer, tip plumbing.Hash, haves []plumbing.Hash, wantObjects int, result *Result) error {
	hashes, err := revlist.Objects(stor, []plumbing.Hash{tip}, haves)
	if err != nil {
		return fmt.Errorf("walk history: %w", err)
	}
	// A driver that silently drops objects would otherwise win every read.
	if wantObjects > 0 && len(hashes) != wantObjects {
		return fmt.Errorf("walk found %d objects, want %d", len(hashes), wantObjects)
	}
	var sink countingWriter
	if _, err := packfile.NewEncoder(&sink, stor, false).Encode(hashes, packWindow); err != nil {
		return fmt.Errorf("encode pack: %w", err)
	}
	result.Objects = len(hashes)
	result.PackBytes = sink.n
	return nil
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

var _ io.Writer = (*countingWriter)(nil)

// absentHash derives an object id no workload contains.
func absentHash(n int) plumbing.Hash {
	return plumbing.ComputeHash(plumbing.BlobObject, fmt.Appendf(nil, "gitstore-bench-absent-%d", n))
}

# Scaling and performance

Bleephub keeps its entire domain model in memory behind one process-global
`sync.RWMutex` (`store.Store.Mu`) and persists write-through. That makes it fast
and simple, and it means the scaling limits are concentrated and predictable.
This document records where those limits are, what the fuzz/benchmark/ramp suite
measures, and the knobs that move them.

## How to run the suite

- **Fuzzing** — `make fuzz` bursts every fuzz target across all packages
  (`FUZZTIME=60s make fuzz` for a longer campaign). A crash writes a reproducer
  under `<package>/testdata/fuzz/`.
- **Benchmarks** — `make bench` runs the scale/concurrency micro-benchmarks. Grow
  the fixture with `BLEEPHUB_BENCH_REPOS`, `_ISSUES`, `_PRS`, `_RUNS`; add
  `-cpu 1,2,4,8` and `-mutexprofile` to attribute lock contention.
- **Scaling ramp** — `make scale` runs the closed-loop ramp
  (`TestScalingRamp`, opt-in via `BLEEPHUB_SCALE=1`): it drives an increasing
  number of concurrent clients and prints, per step, p50/p95/p99 latency,
  throughput, error rate, heap, and goroutine count, then flags the **knee** —
  the first concurrency at which p99 crosses the SLO or errors appear. Tune with
  `BLEEPHUB_SCALE_STEPS`, `_SECONDS`, `_PROFILE` (`read`/`write`/`mixed`), and
  `_P99_MS`.
- **Concurrency correctness** — the full `-race` suite includes the CRUD storm
  (`stress_crud_test.go`), the sustained read/leak probe (`load_sustained_test.go`),
  and the number/merge-queue invariants (`concurrency_invariants_test.go`).

## The ceilings

1. **The global write lock.** Every mutation takes `Store.Mu.Lock()`, so writers
   fully serialize; concurrent creates on unrelated repositories still contend.
   `BenchmarkConcurrentIssueCreate` and the ramp's `write`/`mixed` profiles show
   throughput plateau as workers climb — that plateau is this lock. With
   persistence on and group commit **off**, the ceiling drops further to one fsync
   (or one Raft round-trip) per mutation. `BLEEPHUB_GROUP_COMMIT` (below) removes
   the per-mutation-fsync half of this on single-node SQLite by committing many
   writers' changes in one fsync; the in-memory lock itself remains the ultimate
   serialization point (in-memory writes already scale cleanly to dozens of
   concurrent clients, so it is not the acute limit).

2. **Resident memory.** All objects live in RAM; memory scales linearly with
   total repos × issues × PRs × runs × comments and every long-tail surface. There
   is no eviction or paging. Size the host for the working set.

3. **Cross-repo / search endpoints scan.** `GET /issues?filter=mentioned` and
   `/search/issues` scan every issue, PR, and (for mention/`in:comments`) comment
   — `BenchmarkGlobalIssuesMentionScan` is ~185 ms/op on the default corpus and
   grows with the corpus. These are inherent to the in-memory model and remain
   O(n); they are documented, not indexed.

4. **No HTTP backpressure by default.** With no in-flight cap, a flood admits
   unbounded goroutines that queue on the lock. See `BLEEPHUB_MAX_INFLIGHT` below.

## What was fixed

- **Comment-parent / lock resolution** used a full issue+PR scan; it now uses the
  existing `IssuesByRepo`/`PullsByRepo` O(1) index.
- **Workflow-run listing** scanned every run in the instance; a per-repo index
  (`workflowsByRepo`, maintained in the central `SyncWorkflowIndexesLocked` path)
  makes `GET .../actions/runs` independent of the instance-wide run total
  (`BenchmarkWorkflowRunListingScale`).
- **GraphQL node-cost limit.** The cost check bounded static fields and depth but
  not resolved nodes, so a small nested query (`repos(first:100){issues(first:100)
  {comments(first:100)}}`) could force ~10⁶ resolutions. It now enforces GitHub's
  500,000-node budget before execution.
- **Workflow-parser panic.** A null job body (`jobs:\n  build:`) nil-panicked the
  parser; fuzzing found it, and it now returns an error like GitHub.
- **Shorter global-lock hold on hot reads.** `GET /issues?filter=mentioned` re-scanned
  every comment per row (O(issues×comments)); it now precomputes the mentioned-parent
  set in one pass (the notifications pattern), so the lock is held far less. The
  GraphQL `search` `involves:` matcher memoizes its commenter set the same way, and
  the merge-queue scans, open-PR cap, `AuthorAssociation`, timeline-event recording,
  and issue-comment listings now use existing per-repo / per-parent indexes instead
  of scanning global maps.
- **Group commit (opt-in, single-node SQLite).** With `BLEEPHUB_GROUP_COMMIT=true`,
  durable writes no longer fsync one-at-a-time inside `Store.Mu`: `apply` and counter
  allocation enqueue and a background committer fsyncs many writers' ops in one
  transaction, while an HTTP durability barrier withholds each mutating response
  until its writes are durable. An acknowledged write is exactly as durable as
  before; a crash can only drop writes no client was told about. ~4.8× concurrent
  durable-write throughput at 32 writers on SSD (`BenchmarkDurableWriteThroughput`),
  more on slower disks. Gated to exclusively-owned SQLite (`OwnedExclusively`); the
  dqlite/Raft path is unchanged. Default **off** because it trades the synchronous
  memory-and-disk atomicity the store otherwise guarantees for throughput.

## Tuning knobs

| Variable | Effect |
|---|---|
| `BLEEPHUB_MAX_INFLIGHT` | Cap concurrent non-byte-transfer requests; excess gets 503 + Retry-After (git/artifact transfers exempt). Unset = unlimited (default). |
| `BLEEPHUB_MAX_WORKFLOWS` | Concurrent workflow-run admission cap (default 10). |
| `BLEEPHUB_PERSIST` | Write-through SQLite; each mutation fsyncs (throughput ≈ 1/fsync). Off = in-memory only. |
| `BLEEPHUB_GROUP_COMMIT` | Single-node SQLite only: batch many writers' fsyncs into one via a background committer + HTTP durability barrier (acked writes stay durable). ~4.8× concurrent durable-write throughput. Trades synchronous atomicity for throughput; default off. Ignored on a dqlite quorum. |
| `BLEEPHUB_GITSTORE_CHUNK_BYTES` | S3 pack-cache extent size (default 4 MiB). |
| `BLEEPHUB_GITSTORE_COMPACT_AFTER` | Loose-object count that triggers compaction (default 4096); lower to keep clone/fetch S3-request counts down under heavy push churn. |
| `BLEEPHUB_GITSTORE_INDEX_FRESHNESS` | Object-index negative-answer freshness window (default 250 ms). |

## Git over S3

Clone/fetch/push performance against object storage is characterized in
[docs/git-storage.md](git-storage.md) and measured by
`internal/gitstore/measure_test.go` (S3 requests and bytes per object). The main
ceilings are loose-object explosions between compactions and per-pack existence
checks when many packs accumulate.

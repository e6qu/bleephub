# gitstore/bench

Measures git-on-object-storage implementations against one another: the same
generated repository, the same go-git plumbing, the same metered path to the
same object store — so that what differs between two rows of the output is the
storage design and nothing else.

```sh
go run . -latency 5ms                          # everything, in-process fake S3
go run . -drivers gitstore,ogit -files 5000    # choose drivers and size
go run . -endpoint http://127.0.0.1:9000       # a real S3-compatible store (MinIO, …)
go run . -h                                    # drivers, scenarios, flags
```

From the repository root: `make bench-gitstore BENCH_FLAGS='-latency 5ms'`.

It is a module of its own so that the implementations it compares against — and
their dependency trees — never reach `gitstore`'s or bleephub's `go.mod`.

## How the comparison is kept fair

- **One workload.** The repository is generated once per invocation from a seed
  (`-seed`), by a hash-counter stream rather than `math/rand`, and each push is
  cut into a packfile once. Every driver ingests byte-identical packs, and the
  same seed yields the same bytes on any machine.
- **One set of operations.** A push is `packfile.UpdateObjectStorage` plus a
  reference compare-and-set, which is what a receive-pack does. A clone or fetch
  is a history walk plus `packfile.Encoder`, which is what an upload-pack does.
  The harness runs these against every driver; a driver only supplies storage.
- **One meter.** Every driver reaches the object store through a reverse proxy
  that counts requests by operation and bytes on the wire, and injects
  `-latency` into each request. A driver that brings its own S3 client, or is
  not written in Go, is measured by the same rule as one that is. The proxy
  forwards the `Host` header untouched, so SigV4 signatures verify against a
  real endpoint.
- **Verified reads.** Each clone checks the branch tip and the number of objects
  it walked, so a driver cannot win a read by losing data.

Request count is the figure to watch. At loopback speed a design that makes two
thousand requests looks merely slower than one that makes nine; add the round
trip to a real region with `-latency` and it is the whole difference.

## Drivers

| Driver | What it is |
|---|---|
| `gitstore` | This library, configured as bleephub runs it. |
| `ogit` | [Labbs/ogit](https://github.com/Labbs/ogit)'s S3 `Storer`: one S3 object per git object and per reference, no packs, no cache. The straightforward design, and so the baseline the optimisations are priced against. Its reference compare-and-set is not a conditional write, so it is measured single-writer only. |
| `gogit-disk` | go-git's dotgit layout on local disk: what the same code costs when a file operation is a syscall. Its "cold" is a fresh handle only — the OS page cache is not dropped. |
| `gogit-memory` | go-git with nothing under it. The ceiling: it prices the plumbing every driver pays. |

### Adding one

Implement `StorerDriver` (`driver.go`) — `Open` returns a go-git `storer.Storer`
for a repository, `cold` meaning "as a replica that has never served it" — and
register it in an `init`. `TestEveryDriverCompletesEveryScenario` then runs it
through every scenario at the smallest size.

### Implementations not driven yet

These keep git in object storage but are reached through the `git` CLI (as a
remote helper or a server) rather than as a go-git `Storer`, so they need a
second, CLI-level runner that pushes and clones with stock `git` through the same
meter. `gitstore` would join that comparison behind bleephub's smart-HTTP
endpoint.

Descriptions are from each project's own documentation as of September 2026.

| Candidate | Storage design |
|---|---|
| [tobi/walgit](https://github.com/tobi/walgit) (Rust server) | Write-ahead log of immutable packs with a compare-and-swapped manifest; geometric compaction. |
| [dekobon/git-remote-object-store](https://github.com/dekobon/git-remote-object-store) (Rust helper) | Bundle-per-push, or a pack chain with GC; per-ref locks by conditional write. |
| [awslabs/git-remote-s3](https://github.com/awslabs/git-remote-s3) (Python helper) | One full bundle per ref per push, so cost grows with the repository, not the delta. |
| [tigrisdata/objgit](https://github.com/tigrisdata/objgit) (Go server) | A custom columnar pack format read by exact-span range requests. Relies on Tigris's non-standard `RenameObject`, and its filesystem is under `internal/`. |
| [wzshiming/go-billy-s3fs](https://github.com/wzshiming/go-billy-s3fs) (Go, in-process) | The nearest like-for-like — a `billy.Filesystem` over S3 — but on go-billy v6 / go-git v6 alphas, which this go-git v5 harness cannot host. |

FUSE mounts were ruled out as comparisons: mountpoint-s3 and goofys do not
support the renames and non-sequential writes a bare repository needs.

## Scenarios

Run in this order, each against the repository the ones before it left, the way
a server meets a repository over its life. Scenarios a run needs but `-scenarios`
did not select still execute; they are just not reported.

| Scenario | |
|---|---|
| `push-initial` | Ingest the whole starting history as one packfile. |
| `clone-cold` | Full clone by a replica with no local cache. |
| `clone-warm` | The same clone again on a replica that has served it once. |
| `push-incremental` | `-pushes` single-commit pushes, each a compare-and-set of the branch. |
| `fetch-incremental` | A client holding the initial tip fetches everything pushed since. |
| `probe-absent` | `-probes` negotiation probes for objects the repository does not have. |
| `maintain` | The driver's own housekeeping; skipped where there is none. |
| `clone-cold-maintained`, `probe-absent-maintained` | The same reads after housekeeping. |
| `clone-parallel` | `-parallel` concurrent clones from a cold start. |

## Output

The table reports the median of `-runs`. `-json` writes every measurement;
`-benchfmt` writes Go benchmark lines, so two invocations — a branch against
`main`, MinIO against the fake, one latency against another — compare with
`benchstat old.txt new.txt`.

## Results

Median of three runs against the in-process fake with 2 ms injected into every
request — a same-region round trip. 1,000 files, 30 commits then 10 single-commit
pushes: 1,787 objects, a 928 KiB initial pack.
`go run . -files 1000 -commits 30 -pushes 10 -latency 2ms -runs 3`

| Scenario | gitstore | ogit | go-git on local disk |
|---|---|---|---|
| `push-initial` | **13** requests, 0.08 s | 3,187 requests, 9.7 s | 0.04 s |
| `clone-cold` | **9**, 0.68 s | 1,956, 6.4 s | 0.62 s |
| `clone-warm` | **7**, 0.68 s | 1,956, 6.7 s | 0.62 s |
| `push-incremental` (10 pushes) | **110**, 0.32 s | 408, 1.2 s | 0.03 s |
| `fetch-incremental` | **7**, 0.07 s | 671, 2.1 s | 0.07 s |
| `probe-absent` (1,000 probes) | **16**, 0.05 s | 1,000, 3.0 s | 0.01 s |
| `maintain` | 22, 0.11 s | — | — |
| `clone-cold-maintained` | 11, 0.73 s | — | — |
| `clone-parallel` (8 clones) | **55**, 0.90 s | 18,112, 7.5 s | 0.98 s |

A clone's time is dominated by the pack encode, which every driver pays (the
in-memory ceiling is 0.62 s), so the read rows say that `gitstore`'s object-store
overhead is a few tens of milliseconds while the one-object-per-key design's is
the whole clone, ten times over. The gap scales with the round trip, since it
is the request counts that differ: every extra millisecond of latency adds about
two seconds to the 1,956-request clone and about nine milliseconds to the
9-request one. Rerun with a larger `-latency` to measure a farther region.

### What building this harness found

The first run of this table did not look like that. `gitstore`'s pushes were
slower than the naive design's, and its clones no better until a compaction ran:

| Scenario | before | after |
|---|---|---|
| `push-initial` | 8,247 requests, 21.4 s | 13, 0.08 s |
| `clone-cold` | 2,231, 6.4 s | 9, 0.68 s |
| `push-incremental` | 1,010, 2.6 s | 110, 0.32 s |
| `fetch-incremental` | 674, 1.8 s | 7, 0.07 s |
| `probe-absent` | 695, 1.8 s | 16, 0.05 s |
| `maintain` | 1,803, 5.4 s | 22, 0.11 s |

Each was a separate defect, fixed in the library:

- **Pushes were exploded into loose objects.** The storer wrapper did not expose
  go-git's `PackfileWriter`, so go-git parsed every pushed pack and stored its
  objects one key at a time, at five requests each. Pushes now land as packs
  (`ingest.go`), including the thin packs a stock `git push` sends, which are
  completed first.
- **Every loose write cost a PUT, a COPY and a DELETE**, because git writes to a
  temporary name and renames. The temporary name no longer reaches the bucket.
- **Pack merging rewrote the whole repository** each time the pack count crossed
  its threshold, and then re-merged the same packs on every compaction for an
  hour, because packs it had already superseded still counted. Merging is now
  geometric and ignores superseded packs.
- **New readers adopted superseded packs**, loading an index and a filter for
  each one during its retention window.
- **Parallel cold clones each downloaded the same pack extents.** Fetches of one
  extent are now coalesced (`clone-parallel`: 188 requests and 8.8 MiB, to 55 and
  1.2 MiB).
- **Ranged pack reads bypassed the circuit breaker and the shutdown context.**

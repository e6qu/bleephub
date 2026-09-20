# gitstore/bench

Measures git-on-object-storage implementations against one another: the same
generated repository, the same go-git plumbing, the same metered path to the
same object store — so that what differs between two rows of the output is the
storage design and nothing else.

```sh
go run . -latency 5ms                          # Storer level, in-process fake S3
go run . -drivers gitstore,ogit -files 5000    # choose drivers and size
go run . -level git -latency 5ms               # stock git against the real server and remote helpers
go run . -level both -endpoint http://127.0.0.1:9000   # a real S3-compatible store (MinIO, …)
go run . -h                                    # drivers, scenarios, flags
```

From the repository root: `make bench-gitstore` and `make bench-gitstore-git`,
with flags in `BENCH_FLAGS`.

## Two levels

Implementations of git on object storage come in two shapes, and no one runner
reaches both, so the harness has two.

- **`-level storer`** drives an implementation in-process as a go-git
  `storer.Storer`. Only storage differs between drivers, so this level isolates
  the storage design — but only a Go library can take part.
- **`-level git`** drives the stock `git` client against a remote: a smart-HTTP
  server, or a remote helper that git runs for a URL scheme. Anything in any
  language takes part, and what is timed is what a developer waits for —
  negotiation, transfer and the client's own work included. `gitstore` takes
  part as **the real bleephub server binary** with its repositories in the
  object store, restarted mid-run for the cold scenarios.

Figures compare within a level, never across.

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

## Storer-level drivers (`-drivers`)

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

## Git-level drivers (`-git-drivers`)

A driver whose helper is not on `PATH` is reported and skipped, not failed: the
helpers are other people's programs.

| Driver | What it is | Status |
|---|---|---|
| `bleephub` | This repository's server on `gitstore`, over smart HTTP. Built from the checkout, or `-bleephub-bin`. | Runs in CI. |
| `git-local` | Stock git, bare repository on local disk. The ceiling. | Runs in CI. |
| `git-remote-s3` | [awslabs/git-remote-s3](https://github.com/awslabs/git-remote-s3) (Python): one full bundle per ref per push, so a push costs the repository, not the change. `pip install git-remote-s3`. | Verified against this harness, v0.4.2. |
| `git-remote-object-store` | [dekobon/git-remote-object-store](https://github.com/dekobon/git-remote-object-store) (Rust), `packchain` engine: a manifest of packs. `cargo xtask install`. | Verified against this harness, v0.2.5. |

Any other remote is driven without code:

```sh
go run . -level git -git-drivers bleephub -remote 'walgit=https://walgit.internal/{repo}.git'
```

`-remote name=url-template` takes the placeholders `{endpoint}` (the metered
endpoint), `{host}`, `{bucket}`, `{prefix}`, `{region}` and `{repo}`, and gives
the helper the standard `AWS_*` environment, `AWS_ENDPOINT_URL` included, which
is how every helper surveyed takes its endpoint. A server, such as
[tobi/walgit](https://github.com/tobi/walgit), is started by hand with its
store pointed at the meter, and named the same way.

Left out: [tigrisdata/objgit](https://github.com/tigrisdata/objgit) relies on
Tigris's non-standard `RenameObject`, and
[wzshiming/go-billy-s3fs](https://github.com/wzshiming/go-billy-s3fs) — the
nearest like-for-like, a `billy.Filesystem` over S3 — is on go-billy v6 / go-git
v6 alphas, which the go-git v5 Storer level cannot host. FUSE mounts
(mountpoint-s3, goofys) do not support the renames and non-sequential writes a
bare repository needs.

A remote may work after it has answered — bleephub compacts in the background
once a push returns. The git level waits for the object store to go quiet
before closing each scenario's count, so that work is charged to the request
that caused it and not to the next scenario.

### Pointing a tool at the fake by hand

```sh
go run ../s3fake/cmd/s3fake -addr 127.0.0.1:9000 -trace
```

serves the counting fake on a fixed address — any bucket exists, any
credentials sign — logs each request, and prints the totals when interrupted.
It is how the per-request breakdowns below were found.

## Scenarios

Run in this order, each against the repository the ones before it left, the way
a server meets a repository over its life. Scenarios a run needs but `-scenarios`
did not select still execute; they are just not reported.

The git level runs the push, clone and fetch scenarios; probes and maintenance
are a Storer's to expose, and a remote's own business.

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

### Git level

The same workload and latency, the stock git client, clones verified object by
object. `go run . -level git -files 1000 -commits 30 -pushes 10 -latency 2ms -runs 3`

| Scenario | bleephub | git-remote-object-store | git-remote-s3 | git, local disk |
|---|---|---|---|---|
| `push-initial` | 27 requests, **0.27 s**, 977 KiB up | 15, 0.35 s, 1.9 MiB up | **9**, 0.27 s, 929 KiB up | 0.07 s |
| `clone-cold` | 16, **0.23 s**, **977 KiB** down | 15, 0.31 s, 3.6 MiB down | **7**, 0.28 s, 1.8 MiB down | 0.04 s |
| `clone-warm` | 10, **0.11 s**, **2 KiB** down | 15, 0.34 s, 3.6 MiB down | **7**, 0.29 s, 1.8 MiB down | 0.04 s |
| `push-incremental` (10 pushes) | 246, **1.29 s**, **256 KiB** up | 150, 2.33 s, 854 KiB up | **90**, 2.75 s, 8.4 MiB up | 0.67 s |
| `fetch-incremental` | 12, **0.11 s**, **2 KiB** down | 26, 0.76 s, 146 KiB down | **5**, 0.31 s, 849 KiB down | 0.09 s |
| `clone-parallel` (8 clones) | 93, **0.30 s**, **1.2 MiB** down | 440, 1.24 s, 31 MiB down | **56**, 0.55 s, 13 MiB down | 0.61 s |

What it says:

- **A server that keeps state wins on bytes and time.** A remote helper starts
  from nothing on every invocation, so it downloads the repository to clone it
  and again to fetch into it; bleephub's warm clone and incremental fetch move
  two kilobytes, because the packs are already in its cache. Eight parallel
  clones cost the helpers eight downloads and bleephub one.
- **A bundle per push costs the repository, not the change.** `git-remote-s3`
  uploads 8.4 MiB over ten one-commit pushes to a repository whose whole history
  is under 1 MiB; bleephub uploads 256 KiB.
- **bleephub makes more requests per push than either helper** — about 25,
  against 15 and 9. Tracing one push (`s3fake -trace`) shows where they go: the
  server resolves the branch a dozen times in the course of a push (the
  advertisement, validation, the compare-and-set, hooks, workflow triggers),
  each time from the object store, and lists the pack directory after it. That
  is the next thing worth attacking, and it is a server question — how long a
  resolved reference may be reused across one request — rather than a storage
  one. At 2 ms it costs little; at a cross-region 50 ms it would be most of a
  push.

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

The git level then found three more, by driving the real server:

- **Concurrent clones of a freshly started replica failed with `not our ref`.**
  A server shares one storage handle per repository, and go-git builds that
  handle's pack index lazily, publishing the map before filling it — so of
  several first readers arriving together, some saw only part of the packs and
  reported objects the repository held as missing. The handle is now primed
  once, under its exclusive lock, before any shared read. Found by
  `clone-parallel` after a restart; invisible to any single-client test.
- **Every reference read cost two requests**, a HEAD and then a GET, because
  go-git stats a ref before opening it. The stat now fetches the bytes and hands
  them to the open that follows; a read made in order to write never takes that
  handoff, so the compare-and-set still compares against the store. About 30%
  fewer requests on every scenario through the server.
- **A compaction with nothing to do listed the pack directory twice**, on every
  push.

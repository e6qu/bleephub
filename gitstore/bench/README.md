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
go run . -level git -git-drivers bleephub -store azure -endpoint http://127.0.0.1:10000/devstoreaccount1   # Azure Blob Storage
go run . -h                                    # drivers, scenarios, flags
```

From the repository root: `make bench-gitstore` and `make bench-gitstore-git`,
with flags in `BENCH_FLAGS`.

## Two comparisons

The harness answers two separate questions, and a run should ask one of them.

- **Which object store to run bleephub on.** `bleephub` against each store, one
  invocation per endpoint, everything else fixed. Request counts are identical
  between stores — the design is the same — so what differs is the time a store
  takes to answer, and `-latency` is what makes a design difference visible at
  all.
- **How bleephub compares with other single-binary git servers.** Run
  `bleephub` on the fastest store measured above — bleephub keeps repositories
  in an object store, and it should be compared as it is deployed — against
  `gitea`, `forgejo`, `git-http-backend` and `git-local`, which keep theirs on
  a local filesystem. `bleephub-dir` belongs in this table only as a diagnostic
  of bleephub's own directory backend, not as its entry. Each of them is driven by `-remote`
  (below) with no code, and `git-local` is the ceiling under all of them.

Mixing the two in one table invites the wrong reading: a filesystem server
beating `bleephub` on an object store is a statement about disks and networks,
not about either server.

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
  real endpoint. The bleephub server has one endpoint for everything it keeps in
  the object store, so its byte store (artifacts, logs, packages) is behind the
  meter as well as its repositories. No scenario writes to the byte store; what
  it costs is the twelve requests of the conformance probe it runs when the
  server starts, which are in `replica-start` and in no other row. Until the
  server's configuration gave each driver a single endpoint, the harness
  pointed the byte store past the meter and `replica-start` read 13.
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
| `bleephub` | This repository's server on `gitstore`, repositories in the object store, over smart HTTP. Built from the checkout, or `-bleephub-bin`. | Runs in CI. |
| `bleephub-dir` | The same server with its repositories in a local directory (`BLEEPHUB_GIT_DIR`) — the row to put beside a filesystem git server. Its byte store is still the run's object store, because a persistent bleephub refuses to start without object-backed storage for artifacts, logs, release assets, packages and LFS; no git scenario writes any of those, so what it costs is the startup probe in `replica-start` and nothing per operation. | Runs in CI. |
| `git-http-backend` | Git's own smart-HTTP server: the `git-http-backend` CGI git ships, over a bare repository on local disk, with no forge, database or hooks on top. The protocol's own price, against which a forge's overhead is read. | Runs in CI. |
| `git-local` | Stock git, bare repository on local disk, reached by a `file://` URL. The ceiling. (By path, `git clone` hardlinks the repository's files instead of transferring a pack; an earlier version of the harness did that, and its clones looked five times faster than any transfer.) | Runs in CI. |
| `walgit` | [tobi/walgit](https://github.com/tobi/walgit) (Rust), a smart-HTTP server: packs plus a write-ahead log whose manifest it swaps by conditional write, repositories materialized to a local cache. The nearest design to `gitstore`. Needs git ≥ 2.46 and a store with conditional writes (the fake has them). `cargo build --release`, then `-walgit-bin` or `walgit` on `PATH`; the harness writes its configuration and restarts it with an empty cache for the cold scenarios. | Verified against this harness, commit `80e9a20`. |
| `git-remote-s3` | [awslabs/git-remote-s3](https://github.com/awslabs/git-remote-s3) (Python): one full bundle per ref per push, so a push costs the repository, not the change. `pip install git-remote-s3`. | Verified against this harness, v0.4.2. |
| `git-remote-object-store` | [dekobon/git-remote-object-store](https://github.com/dekobon/git-remote-object-store) (Rust), `packchain` engine: a manifest of packs. `cargo xtask install`. | Verified against this harness, v0.2.5. |

Any other remote is driven without code:

```sh
go run . -level git -git-drivers bleephub -remote 'mine=https://git.internal/{repo}.git'
```

`{repo}` expands to a name that already carries an owner segment
(`bench/repo-0`), so a forge whose URLs are `/<owner>/<repo>.git` takes
`-remote 'gitea=http://user:password@127.0.0.1:3100/{repo}.git'`, and needs
whatever setting creates a repository on first push (Gitea and Forgejo:
`ENABLE_PUSH_CREATE_USER`; Gogs: `ENABLE_PUSH_CREATE_USER` under
`[repository]`).

`-remote name=url-template` takes the placeholders `{endpoint}` (the metered
endpoint), `{host}`, `{bucket}`, `{prefix}`, `{region}` and `{repo}`, and gives
the helper the standard `AWS_*` environment, `AWS_ENDPOINT_URL` included, which
is how every helper surveyed takes its endpoint. A server is started by hand
with its store pointed at the meter, and named the same way.

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
credentials sign — logs each request with what tells it from another (a
listing's prefix, a read's extent, whether a write was conditional; never a
header value or a signature), and prints the totals when interrupted. It is how the per-request breakdowns below were found. The fake
gives objects content-derived ETags and honours `If-Match` / `If-None-Match` on
writes, which designs that lock or swap a manifest by conditional write depend
on.

### Larger workloads and a real store

```sh
docker run -d -p 127.0.0.1:19000:9000 -e MINIO_ROOT_USER=benchadmin -e MINIO_ROOT_PASSWORD=benchsecret123 \
    docker.io/pgsty/minio:RELEASE.2026-08-04T00-00-00Z server /data
AWS_ACCESS_KEY_ID=benchadmin AWS_SECRET_ACCESS_KEY=benchsecret123 \
    go run . -endpoint http://127.0.0.1:19000 -latency 5ms -files 4000 -file-lines 400 -commits 60 -pushes 20 -changes 25
```

### Azure Blob Storage and Cloud Storage

`-store azure` and `-store gcs` run against the other two kinds of store
bleephub has a driver for, and each without `-endpoint` against its in-process
fake. Azure's credentials are `AZURE_STORAGE_ACCOUNT` and `AZURE_STORAGE_KEY`,
and `-endpoint` is the blob service URL with the account in it where the
service is addressed path-style; Cloud Storage's are the service-account key
file `GOOGLE_APPLICATION_CREDENTIALS` names, and its bucket must exist, because
a bucket belongs to a project the harness is not told of. Only the drivers
that can keep their data in the chosen store run: `gitstore` and the two
bleephub servers can keep theirs in any; the S3 remote helpers, `walgit` and
`ogit` only in S3, and naming one with another store is refused rather than
skipped. The meter files each store's requests under the S3 operation they do
the work of — a block or a resumable chunk under the upload's parts, a batch
of deletes under the bulk delete — so the columns mean the same for every
store.

`-file-lines` scales each file, which is how a workload gets packs that span
several read extents (4 MiB by default; `-gitstore-chunk-bytes`) or cross the
multipart threshold (`-gitstore-multipart-bytes`). The meter forwards the `Host`
header untouched, so requests signed for it verify at MinIO or AWS.

## Scenarios

Run in this order, each against the repository the ones before it left, the way
a server meets a repository over its life. Scenarios a run needs but `-scenarios`
did not select still execute; they are just not reported.

The git level runs the push, clone and fetch scenarios; probes and maintenance
are a Storer's to expose, and a remote's own business.

| Scenario | |
|---|---|
| `push-initial` | Ingest the whole starting history as one packfile. |
| `replica-start` | Git level. A replica with an empty cache starts, and the object store goes quiet: what it spends before it can serve. Designs differ in how much they do here, so it is reported on its own and not folded into the clone after it. |
| `clone-cold` | Full clone by a replica with no local cache, measured once it has started. |
| `clone-warm` | The same clone again on a replica that has served it once. |
| `push-incremental` | `-pushes` single-commit pushes, each a compare-and-set of the branch. |
| `fetch-incremental` | A client holding the initial tip fetches everything pushed since. |
| `probe-absent` | `-probes` negotiation probes for objects the repository does not have. |
| `maintain` | The driver's own housekeeping; skipped where there is none. |
| `clone-cold-maintained`, `probe-absent-maintained` | The same reads after housekeeping. |
| `clone-parallel` | `-parallel` concurrent clones from a cold start. |
| `refs-create` | Storer level. `-refs` branches and tags (default 200; 0 skips both), one reference write each. |
| `refs-advertise` | Storer level. List every reference, as each fetch and push begins by doing: a cold replica, then three more clients 400 ms apart — longer than any driver reuses a read for, so each is a new arrival. The pauses are in the time column, equally for every driver. |

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
| `push-initial` | **7** requests, 0.07 s | 3,187 requests, 8.8 s | 0.04 s |
| `clone-cold` | **3**, 0.66 s | 1,956, 5.9 s | 0.62 s |
| `clone-warm` | **1**, 0.63 s | 1,956, 6.0 s | 0.62 s |
| `push-incremental` (10 pushes) | **50**, 0.10 s | 408, 1.1 s | 0.03 s |
| `fetch-incremental` | **0**, 0.05 s | 671, 1.9 s | 0.07 s |
| `probe-absent` (1,000 probes) | **1**, 0.004 s | 1,000, 2.7 s | 0.015 s |
| `maintain` | 6, 0.08 s | — | — |
| `clone-cold-maintained` | 5, 0.72 s | — | — |
| `clone-parallel` (8 clones) | **5**, 0.93 s | 18,112, 7.3 s | 0.94 s |
| `refs-create` (200 references) | 200, 0.55 s | 200, 0.55 s | 0.02 s |
| `refs-advertise` (4 clients, 201 references) | **4**, 1.22 s | 812, 3.44 s | 1.24 s |

`refs-advertise` spends 1.2 s of its time in the pauses between clients, which
every driver pays, so `gitstore`'s four advertisements cost nothing measurable
over the ceiling: a repository's references are its manifest and one immutable
snapshot object, so the cold replica reads two objects and each client after it
revalidates the manifest with a conditional read that moves no body — four
requests for four clients and 201 references, where it was 213 when every
reference was an object of its own, and 812 for the design that still keeps
them so. A reference write is one PUT in any design, which is why `refs-create`
is a draw; here the PUT is the manifest, swapped on the condition that nobody
else has moved it. At this level a push is five requests — the pack, its index,
its filter, a swap that adds the pack and a swap that moves the branch — because
the harness drives go-git's `Storer` a call at a time; through the server a push
is one transaction, and four.

A clone's time is dominated by the pack encode, which every driver pays (the
in-memory ceiling is 0.62 s), so the read rows say that `gitstore`'s object-store
overhead is a few tens of milliseconds while the one-object-per-key design's is
the whole clone, ten times over. The gap scales with the round trip, since it
is the request counts that differ: every extra millisecond of latency adds about
two seconds to the 1,956-request clone and about three milliseconds to the
3-request one. Rerun with a larger `-latency` to measure a farther region.

### Git level

The same workload and latency, the stock git client, clones verified object by
object. `go run . -level git -files 1000 -commits 30 -pushes 10 -latency 2ms -runs 3`

| Scenario | bleephub | walgit | git-remote-object-store | git-remote-s3 | git, local disk |
|---|---|---|---|---|---|
| `push-initial` | **3** requests, **0.11 s**, 978 KiB up | 9, **0.14 s**, 976 KiB up | 15, 0.27 s, 1.9 MiB up | 9, 0.32 s, 929 KiB up | 0.07 s |
| `replica-start` | **27**, **0.21 s**, **2 KiB** down | **27**, 2.31 s, 975 KiB down | — | — | — |
| `clone-cold` | **3**, **0.09 s**, 973 KiB down | 5, 0.12 s, **0 B** down | 15, 0.30 s, 3.6 MiB down | 7, 0.30 s, 1.8 MiB down | 0.08 s |
| `clone-warm` | **1**, **0.08 s**, **0 B** down | 4, 0.11 s, 0 B down | 15, 0.30 s, 3.6 MiB down | 7, 0.30 s, 1.8 MiB down | 0.07 s |
| `push-incremental` (10 pushes) | **38**, **0.50 s**, 310 KiB up | 70, 0.72 s, **186 KiB** up | 150, 2.24 s, 854 KiB up | 90, 2.81 s, 8.4 MiB up | 0.67 s |
| `fetch-incremental` | **1**, **0.08 s**, **0 B** down | 4, 0.09 s, 0 B down | 26, 0.72 s, 147 KiB down | 5, 0.30 s, 849 KiB down | 0.09 s |
| `clone-parallel` (8 clones) | **9**, **0.19 s**, 1.1 MiB down | 33, 0.24 s, **0 B** down | 440, 1.11 s, 31 MiB down | 56, 0.53 s, 13 MiB down | 0.21 s |

What it says:

- **A server that keeps state wins on bytes and time.** A remote helper starts
  from nothing on every invocation, so it downloads the repository to clone it
  and again to fetch into it; the two servers' warm clones and incremental
  fetches move nothing, because the packs are already in their caches. Eight
  parallel clones cost the helpers eight downloads and the servers one.
- **A bundle per push costs the repository, not the change.** `git-remote-s3`
  uploads 8.4 MiB over ten one-commit pushes to a repository whose whole history
  is under 1 MiB; bleephub uploads 268 KiB.
- **The two servers differ in when a replica pays.** walgit materializes its
  repositories on local disk as it starts — 2.3 s, 27 requests and the whole
  repository downloaded before it serves anything — and then answers its first
  clone from disk, downloading nothing. bleephub starts in 0.2 s, its 27
  requests being the proof, for each of its two stores, that the store keeps its
  promises, and leaves each pack in the store until a clone reads it: the first
  clone is the manifest, the pack's index and the extents it touches, 3 requests
  and 0.09 s. Which is better depends on how many repositories a replica hosts
  and how many of them anyone asks for: paying at start scales with the first
  number, paying at the first clone with the second. An earlier version of this
  table billed the start to the cold clone, which made walgit's cold clone look
  like 2.5 s; it was its start. Another ran a driver's three runs under one
  prefix, so walgit's second and third starts loaded the repositories of the
  runs before them, and its start and first push read 65 and 52 requests; each
  run now has a store of its own.
- **A push through bleephub is 3 requests, all of them writes**: the pack and
  its sidecar (index and filter in one object), uploaded together, and one
  conditional write of the repository's manifest that adds the pack and moves the branch in the same
  swap. Nothing is read — the lost condition is the verification — and nothing
  is listed. The ten pushes' 38 include the compaction their packs make due.
  walgit, whose manifest-and-log design this borrows from, takes 7. Ten pushes
  take bleephub 0.50 s and stock git, pushing to a bare repository on the same
  machine's disk, 0.67 s: at 2 ms a request the object store has stopped being
  what a push waits for. It was 25 requests when this table was first drawn;
  see below. A push uploads a little more than it did (310 KiB for the ten, from
  268): a thin pack is now stored as it was sent with the bases it left out
  appended whole, as git stores one, rather than re-encoded, and the next
  compaction deltifies them again.
- **A warm read is one request that moves no body.** A clone or a fetch on a
  replica that has served the repository revalidates the manifest with a
  conditional read and is answered "unchanged".

### A larger repository

The tables above are small enough that the object store dominates. At 20,000
files, 120 commits then 20 pushes — 35,378 objects, a 29 MiB first pack — it no
longer does, and what is left is each server's own work.
`go run . -level git -git-drivers bleephub,walgit,git-local -files 20000 -file-lines 120 -commits 120 -pushes 20 -changes 40 -latency 2ms -runs 3 -parallel 4`

| Scenario | bleephub | walgit | git, local disk |
|---|---|---|---|
| `push-initial` | 1.81 s, 6 requests | **0.80 s**, 12 | 0.61 s |
| `replica-start` | **0.22 s**, 27 | 2.31 s, 17 | — |
| `clone-cold` | 0.96 s, 10 | **0.68 s**, 14 | 0.66 s |
| `clone-warm` | **0.66 s**, 1 | 0.74 s, 5 | 0.68 s |
| `push-incremental` (20 pushes) | **1.59 s**, 89 | 1.92 s, 140 | 1.48 s |
| `fetch-incremental` | 0.37 s, 7 | **0.26 s**, 4 | 0.27 s |
| `clone-parallel` (4 clones) | 1.41 s, 22 | **1.09 s**, 26 | 1.25 s |

A clone here is mostly the client's: stock git cloning from the same machine's
disk takes as long as a warm clone from bleephub, since what both wait on is
`index-pack` on the client resolving 35,000 objects. (bleephub's fetch counts a
compaction the twenty pushes leave running as the fetch starts: 4 of its 7
requests are that compaction's writes, and the pushes' and the fetch's
together are 98 requests, as they were before the pushes got faster.)

The first run of this table found a push costing what the repository held rather
than what it changed: secret scanning read every blob in the new tip's tree after
every push, and push protection read every blob a push brought even with nothing
enabled that could block. Twenty pushes took 17.1 s and the first push 6.8 s;
both now scan only what a push adds, which is also what finds a secret a push
adds and removes again.

The second found a thin push parsed three times: indexed as it stood, which
failed at the first delta whose base it left out; parsed again against the
repository into loose objects on local disk; re-encoded whole, with a delta
search, and indexed once more. Twenty pushes took 3.2 s. A pack is now indexed
as git's `index-pack` indexes one (`gitstore/indexpack.go`): a first pass as the
push arrives records every entry and hashes the objects stored whole, a second
applies each delta to its base once, and the bases no object in the pack turns
out to be are read from the repository and appended to it (`--fix-thin`). The
pushed bytes are kept. On a 90 MB pack of this repository's history the two
passes take 1.9 s where go-git's parser took 3.4 s, and produce the same index,
object for object; a large object stored whole is never read a second time.

The third found the first push of a branch waiting on its webhook payload.
GitHub's push event lists the added, removed and modified paths of up to 2,048
commits, and they were computed with go-git's tree diff, which for the root
commit read every one of the 20,000 blobs to learn their names and for the rest
went through its generic merkle-trie comparison: 1.1 s of the push. They are now
read from trees alone, skipping every subtree whose id did not change, and
checked against go-git's diff path for path; the first push went from 3.4 s to
1.8 s, and twenty pushes to what stock git takes.

What is left, from a CPU profile of the server:

- **A first push scans the whole new tree for secrets**, reading every blob: the
  largest part of what the server does for it (0.37 s of CPU here), and real
  work — a new branch's content has not been scanned before.
- **A clone walks every tree in history** to learn what to send, about 0.17 s of
  server CPU per clone in the profile. It is not what a clone waits on: the
  client needs 0.6 s to index what it receives. Remembering the walk per tip was
  tried, and at git level measured no difference outside the run-to-run noise,
  so it is not kept.

The same comparison at a size where packs span read extents, against a real
MinIO with 5 ms injected into every request — 3,000 larger files, 50 commits
then 20 pushes, 6,265 objects, an 8 MiB initial pack:
`go run . -level git -endpoint http://127.0.0.1:19000 -latency 5ms -files 3000 -file-lines 300 -commits 50 -pushes 20 -changes 20 -runs 3`

| Scenario | bleephub | walgit | git-remote-object-store | git-remote-s3 | git, local disk |
|---|---|---|---|---|---|
| `push-initial` | 11, 1.15 s | 89, 0.54 s | 15, 0.69 s | **9**, **0.41 s** | 0.24 s |
| `clone-cold` | 11, **0.41 s**, **8.2 MiB** down | 87, 2.75 s, 18 MiB down | 15, 0.52 s, 32 MiB down | **7**, 0.48 s, 16 MiB down | 0.06 s |
| `clone-warm` | **4**, **0.24 s** | **4**, 0.30 s | 15, 0.51 s, 32 MiB down | 7, 0.48 s, 16 MiB down | 0.05 s |
| `push-incremental` (20 pushes) | 193, 4.50 s, 2.8 MiB up | **140**, **3.83 s**, **1.6 MiB** up | 300, 6.29 s, 5.4 MiB up | 180, 10.3 s, 154 MiB up | 2.30 s |
| `fetch-incremental` | **3**, **0.21 s**, **1 KiB** down | 4, 0.23 s | 46, 1.35 s, 1.3 MiB down | 5, 0.49 s, 7.6 MiB down | 0.16 s |
| `clone-parallel` (8 clones) | **44**, **0.72 s**, **9.8 MiB** down | 178, 5.48 s, 20 MiB down | 760, 2.75 s, 279 MiB down | 56, 0.98 s, 122 MiB down | 0.50 s |

(These rows were measured two engines ago — before the native engine and before
the manifest — and with the replica's start still billed to its cold clone, so
bleephub's and walgit's cold rows include a start, and bleephub's request counts
are several times what they are now. Stock git's column reached its repository
by path, so its clones are hardlinks, not transfers. They are kept for what they show about
size.) The shape holds, and the costs that grow with the repository show: a bundle per
push is now 154 MiB of upload for twenty one-commit pushes. bleephub's twenty
pushes include the compaction that more than eight packs make due, which
walgit's figure has no counterpart to. bleephub's initial push is its one slow row: it indexes
the 8 MiB pack and builds the membership filter before it answers, where a
helper uploads what the client already built. The Storer-level run at this size
(`-files 4000 -file-lines 400`, a 14 MiB pack) lands that pack in 15 requests
where the one-object-per-key design takes 15,055.

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

And the first run at a larger size found the one that mattered most:

- **Stock git could not push more than a mebibyte over HTTP.** A pack that
  outgrows `http.postBuffer` is streamed with chunked encoding, which git cannot
  replay if the server asks for credentials part way, so it first POSTs a lone
  flush packet to `git-receive-pack` to settle authentication. The server parsed
  that probe as a push with no commands and answered 400 — every push over
  1 MiB failed, and none under it, so no test and no small workload had noticed.
  It is now answered as `git-http-backend` answers it.

Then a second pass over the per-request traces, taking a push through the server
from 25 requests to 9 and an advertisement from a request per reference to one
listing:

| Scenario (git level, bleephub) | #543 | now |
|---|---|---|
| `push-initial` | 27 | 6 |
| `clone-cold` / `clone-warm` | 16 / 10 | 9 / 4 |
| `push-incremental` (10 pushes) | 246 | 89 |
| `fetch-incremental` | 12 | 3 |
| `clone-parallel` (8 clones) | 93 | 36 |

- **The server ran a compaction after every push**, which dates from when a push
  landed as loose objects. A compaction opens by listing the repository's whole
  object tree, and one run per push spent that finding nothing to do. The
  storage layer now asks for one when a write leaves the repository due it —
  and counts packs from the pack directory rather than from its own pushes, so a
  server restarted every few pushes no longer never compacts.
- **A pushed pack threw the membership index away**, and the next reader paid a
  listing of the loose tier and one of the pack directory to rebuild what had
  only gained a pack. The index is now told of the pack.
- **The replica downloaded the pack index it had just uploaded.**
- **A branch was read back from the store straight after it was written**, and
  resolved a dozen times in one push from the store each time. Within the
  freshness bound those are one read — none, after a local write.
- **References were listed the way a disk is walked**: a LIST per directory under
  `refs/` and a GET per branch and tag, for every client. One recursive listing
  carries every reference's ETag, so only references that have moved are read,
  and those sixteen at a time. In `refs-advertise` the cold replica's first look
  is 200 reads and each client after it is one listing, where every client used
  to cost the 200 reads again.
- **The fake compared entity tags as strings**, so a client that sends `If-Match`
  without the quotes — the AWS SDK for Rust does — lost every compare-and-swap.
  Found by walgit working against MinIO and not against the fake.

Then the engine itself was replaced. Every defect above was the undoing of a
cost of giving go-git a filesystem to work on — an object store made to
impersonate a disk for a library that then used the disk wastefully — and the
first of the git-level findings was go-git's storage not being safe for
concurrent first reads at all. `gitstore` now implements go-git's `Storer`
directly against the object store (see
[`docs/git_storage_research.md`](../../docs/git_storage_research.md) for why, and
what else was considered), with the bucket laid out exactly as before, so the
two engines compare request for request:

| Scenario | filesystem emulation | native engine |
|---|---|---|
| Storer level: `push-initial` | 12 | 10 |
| `clone-cold` / `clone-warm` | 6 / 4 | 5 / 2 |
| `push-incremental` (10 pushes) | 81 | 51 |
| `fetch-incremental` | 3 | 0 |
| `probe-absent` (1,000 probes) | 5 | 1 |
| `maintain` | 17 | 14 |
| `clone-parallel` (8 clones) | 29 | 7 |
| `refs-advertise` | 223 | 213 |
| Git level: `clone-cold` / `clone-warm` | 9 / 4 | 7 / 3 |
| `push-incremental` (10 pushes) | 89 | 78 |
| `clone-parallel` (8 clones) | 36 | 20 |

- **The engine knows what it holds.** The live packs and their parsed indexes
  are an immutable snapshot built from one listing, so a fetch no longer lists
  the pack directory, a pushed pack is added to the snapshot instead of being
  found by listing again, and eight clones arriving at a cold replica share one
  listing and one copy of each index.
- **The server asks the engine for its packs** to reuse them verbatim, where it
  used to open the pack directory a second time, list it and read the indexes
  again, on every fetch.
- **A large object is streamed from its pack.** Given no filesystem, go-git's
  decoder inflates every object it returns into memory; a large blob would cost
  its size in heap for every reader. Found on the way: go-git's
  `Scanner.Close()` drains its reader to the end, which is right for a pipe and
  over an object store downloads the rest of the pack.
- **The harness billed a replica's start to its first clone**, which is how it
  came to say walgit's cold clone took 2.5 s. Start is now a scenario of its own.

Then the layout changed. The native engine still laid a repository out in the
bucket like a bare git directory, so it still had to list to learn which packs
were live and which references existed, still read a branch in order to move it,
and still needed a lock service beside the store because nothing in the store
arbitrated between two writers. Each repository now has one object, its
manifest, that names its live packs and its references, and a change is visible
when — and only when — a conditional write of that object succeeds:

| Scenario | native engine, dotgit layout | manifest |
|---|---|---|
| Git level: one push | 8 requests | **4**, none of them a read |
| `push-incremental` (10 pushes) | 78 | 47 |
| `clone-cold` / `clone-warm` | 7 / 3 | 3 / 1 |
| `fetch-incremental` | 3 | 1 |
| `clone-parallel` (8 clones) | 20 | 9 |
| Storer level: `refs-advertise` | 213 | 4 |
| `maintain` | 14 | 6 |
| `clone-cold` / `clone-warm` | 5 / 2 | 3 / 1 |

- **The conditional write is the lock.** A commit applies its change to the
  manifest it holds and writes on the condition that the version has not moved;
  losing is how it learns that it must re-read. The durable SQL lock the server
  took around every reference update is gone, and with it the read before every
  write.
- **A push is one transaction.** Its pack is ingested into quarantine and joins
  the repository in the swap that moves its references, so a push whose
  references are refused leaves nothing visible — it used to leave its pack —
  and an atomic push is atomic.
- **Commits to one repository share a swap.** An object takes about one
  conditional overwrite a second on some stores, so mutations that arrive while
  a swap is in flight go together in the next, each validated on its own.
- **References are a snapshot and a few changes**, so a repository with a
  thousand branches advertises them in the same two reads as one with three.
- **Compaction lists once and swaps once**, retiring what it merged in the
  manifest rather than with a marker object per pack; what a manifest names is
  never deleted, and what none names is deleted only after a grace period.

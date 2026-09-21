# Git storage research: foundations for git on object storage

Research carried out on 2026-09-20 to answer one question: **what should a git
server whose only durable storage is an object store be built on?** It records
what was looked at, what was found, and why the decision went the way it did, so
that the question does not have to be researched again from nothing.

Statements are marked *unverified* where no source could be found for them.
Everything else was read from the cited source on the date above.

## The constraints

- **The object store is the only durable state.** Local disk is a cache that a
  replica may lose at any time, and several stateless replicas may serve one
  repository.
- **The store is a common denominator**, not one vendor: S3, Google Cloud
  Storage, Azure Blob Storage, MinIO, SeaweedFS and the like.
- **Stock git clients must work.** What lands in the bucket is ordinary,
  self-contained git packs.
- **Pure Go, no cgo.** The server builds with `CGO_ENABLED=0` for two
  architectures, and that is not up for trade.
- **AGPL-3.0**, so a foundation's licence must be compatible with it.

## Where we started, and what is wrong with it

bleephub used [go-git](https://github.com/go-git/go-git) v5 for three different
jobs:

| Layer | What it is | Verdict |
|---|---|---|
| Object model (`plumbing`, `plumbing/object`) | hashes, commits, trees, the iterators over them | Sound. About 200 imports across the server. |
| Format code (`plumbing/format/packfile`, `idxfile`) | parsing and writing packs and indexes | Sound. No I/O assumptions. |
| `storage/filesystem` and `dotgit` | about 2,250 lines that implement storage as a POSIX `.git` directory over a `billy.Filesystem` | The source of every storage defect we have fixed. |

go-git is a client library. Its storage was written for one process working on
one repository on a local disk, and it shows in two ways.

**It is not safe for concurrent reads of a handle nobody has read yet.**
`ObjectStorage.requireIndex` (v5.19.2, unchanged on `releases/v5.x`) assigns an
empty pack-index map to the storage and only then fills it, one pack at a time. A
second goroutine arriving in between sees a non-nil map, skips the build, and
looks its object up in a map holding some of the packs or none:
`plumbing.ErrObjectNotFound` for an object the repository holds, which a git
client is told as `not our ref`. `DotGit.hasIncomingObjects` and the pack list
are memoized from inside read calls in the same way. A standalone reproduction —
twelve single-object packs on local disk, twelve goroutines each asking a fresh
storage for an object that exists — failed 3,290 of 3,600 lookups, and
`go test -race` flags `requireIndex` on the first round. On a disk the window is
microseconds; over an object store each index load is a round trip, so a replica
that starts under load falls into it reliably. `Reindex()` reopens the window
after every push. go-git v6 (`main`) fixes this one with a mutex and a
singleflight; v6 is alpha.

**It expects a disk, so we had to build one.** Because the storage wants a
filesystem, `gitstore` implemented a `billy.Filesystem` over the object store:
about 1,700 lines and 74 methods. Every optimization made to it since was the
undoing of something the emulation costs — a HEAD and a GET for every reference
read because go-git stats before it opens; a PUT, a COPY and a DELETE for every
object written because git writes to a temporary name and renames; a LIST per
directory under `refs/` and a GET per reference for every advertisement; a probe
for the zero hash to force the lazy index; a lock taken around that probe. Each
is an object store made to impersonate a disk for a library that then uses the
disk wastefully.

## The decision

**Implement go-git's `storage.Storer` interface directly against the object
store, and delete the filesystem emulation.** The interface is about 25 methods.
Pack ingest, compaction, the membership index, the pack cache and ranged reads
were already ours; what is new is a pack-backed object reader that is
concurrency-safe by construction, and a reference store. The object model and the
format code stay, so the ninety-odd files that import go-git do not move, and it
stays pure Go.

The alternatives below were researched to check that decision and to collect
designs worth borrowing.

## Alternatives considered

### JGit's DFS storage and reftable

JGit's DFS layer is the closest prior art: Google runs Gerrit and googlesource on
it over a distributed blob store. Checked against JGit `master`, latest tag
v7.8.0 (2026-09-01).

**What it is.** Shawn Pearce replaced JGit's DHT storage with DFS in 2011 — "I
just can't find any DHT that can handle the read traffic… clone of linux-2.6"
([jgit-dev](https://www.eclipse.org/lists/jgit-dev/msg01189.html)). DFS stores
native pack and index files behind an S3-like API and assumes byte-range reads,
atomic file creation and append-only writes with no seek-back. **It has no loose
objects.**

**What a backend implements** (`DfsObjDatabase`): `newPack` (a unique name),
`writeFile` / `openFile(desc, PackExt)`, `commitPackImpl(desc, replaces)`,
`rollbackPack` and `listPacks`
([source](https://github.com/eclipse-jgit/jgit/tree/master/org.eclipse.jgit/src/org/eclipse/jgit/internal/storage/dfs)).

**What it assumes of the store.** JGit "always writes the pack, then the index.
This allows a simple commit process to do nothing if readers always look for both
files to exist and the DFS performs atomic creation" — so an append-only push
needs no atomic pack-list update. Garbage collection and compaction call
`commitPack(new, replaces)`, which swaps a *set* of packs, and the backend has to
make that swap atomic itself: in practice a metadata store or a compare-and-swap
manifest. The in-process pack list is an `AtomicReference` updated by
compare-and-swap.

**References.** `DfsRefDatabase` has abstract `compareAndPut` /
`compareAndRemove`, a per-reference compare-and-swap. `DfsReftableDatabase`
instead writes each transaction as a `REFTABLE` file inside a pack description
committed through `commitPack`, so the reftable stack *is* the pack list. It uses
only an in-process lock; arbitration between processes is left to the backend,
and the javadoc suggests using `getMaxUpdateIndex()` "as the primary key…
ensuring that when there are competing transactions one wins, and one will fail".

**Open-source backends.** `InMemoryRepository` "exists only for unit testing and
small experiments". `johnny0917/jgit-aws` (S3 packs, DynamoDB refs and pack
descriptions, self-described as "fairly naive") was last pushed in 2015-11;
`benhumphreys/jgit-cassandra` in 2015-02. Google's production backend is closed;
a 2016 thread mentions Megastore/Bigtable, with Pearce advising 1 MB pack chunks
([repo-discuss](https://groups.google.com/g/repo-discuss/c/IekVPmow0yE)). Gerrit
multi-site uses the file-based repository plus replication and a ZooKeeper global
ref database — not DFS. AWS documents CodeCommit's storage as S3 plus DynamoDB;
whether it uses JGit DFS is *unverified*. **No maintained open-source DFS backend
was found.**

**Server-side completeness on DFS** (verified in source): `UploadPack` and
`ReceivePack`; protocol v2 with `ls-refs`, `fetch`, `object-info`, ref-in-want,
sideband-all, wait-for-done; shallow clones; filters `blob:none`, `blob:limit`
and `tree:DEPTH` only. `DfsReader` exposes bitmap and commit-graph indexes and
`DfsGarbageCollector` writes commit-graph and object-size indexes. Multi-pack
index on DFS landed in 7.5.0 (2025-12) and is new.

**Cost of adopting it.** Licensing is fine (BSD-3-Clause / EDL). It would mean a
JVM sidecar with a heap-resident block cache, an RPC for every git operation,
auth and hooks duplicated across two languages, classes in
`internal.storage.dfs` with no API stability promise — and we would still write
and own the backend, portable across five stores.

**Verdict: do not adopt; copy the architecture.**

Ideas worth borrowing:

1. **Append-only commit with no coordination.** Write the pack, then the index,
   each an atomic PUT; a pack is visible when both exist. Compare-and-swap is
   needed only where packs are *replaced* (compaction, GC).
2. **No loose objects.** Everything written is a pack.
3. **Reftable-style references**
   ([spec](https://github.com/eclipse-jgit/jgit/blob/master/Documentation/technical/reftable.md)):
   one immutable object per transaction, named by an update index and created
   if-absent so exactly one competing writer wins; tombstones for deletions;
   compaction merge-joins tables and swaps a manifest; readers read the manifest
   once and get a lock-free snapshot. No Go reftable library is known
   (*unverified*); `hanwen/reftable` is C.
4. **Typed packs** (`INSERT`, `RECEIVE`, `COMPACT`, `GC`, `GC_REST`,
   `UNREACHABLE_GARBAGE`) drive search order and compaction eligibility.
5. **GC that never deletes what a reader may need.** Unreachable objects go into
   a garbage pack with a TTL (default one day) rather than being deleted, and
   `pack()` re-validates references and gives up if it detects a race.
6. **A process-wide block cache** keyed by (stream, offset), deliberately not a
   strict LRU, with indexes loaded through it.

### libgit2 and gitoxide

**Verdict: neither is a foundation for us.** Both lack the server side; libgit2
also needs cgo, and gitoxide's object store cannot yet be replaced.

**libgit2** (v1.9.7 / v1.8.7, 2026-08-13, security releases).

- *Pluggable storage is its design.* `git_odb_backend` has `read`,
  `read_prefix`, `read_header`, `write`, `writestream`, `readstream`, `exists`,
  `refresh` (called automatically when a lookup misses), `foreach`, `writepack`
  (whole-pack ingest), `writemidx`, `freshen`
  ([odb_backend.h](https://github.com/libgit2/libgit2/blob/main/include/git2/sys/odb_backend.h)).
  `git_refdb_backend` has `exists`, `lookup`, `iterator`, `write`, `rename`,
  `del`, `compress`, reflog functions and `lock` / `unlock`; `write` and `del`
  take the expected old value, so every update is a compare-and-swap
  ([refdb_backend.h](https://github.com/libgit2/libgit2/blob/main/include/git2/sys/refdb_backend.h)).
- *Threading.* "Unless otherwise specified, libgit2 objects cannot be safely
  accessed by multiple threads simultaneously"; `git_odb` "uses locking
  internally, and is thread-safe"; `git_repository` and the refdb are not listed
  ([threading.md](https://github.com/libgit2/libgit2/blob/main/docs/threading.md)).
  A maintainer suggests one repository handle per thread.
- *Known backends.* `libgit2-backends` (memcached, mysql, redis, sqlite) was last
  committed 2017-01 and stores an object per row — the wrong shape for an object
  store. No maintained object-store backend was found.
- *No server side.* No upload-pack or receive-pack (issues #1496, 2013, and
  #5605, 2020). The packbuilder "doesn't reuse deltas"
  ([PR #5170](https://github.com/libgit2/libgit2/pull/5170)).
- *Go bindings.* `git2go` supports libgit2 up to 1.5 (last substantive commit
  2022-10), four minor releases and their security fixes behind. Flux removed
  it; Gitaly removed it citing "build hacks" (*date unverified*).
- *Licence.* GPLv2-only with a linking exception: linking is permitted, copying
  source into AGPL code is not.

**gitoxide (gix).** Server upload-pack / receive-pack, push, delta compression,
bitmap use and commit-graph write are all still unchecked in
[crate-status.md](https://github.com/GitoxideLabs/gitoxide/blob/main/crate-status.md);
the upload-pack tracking issue was closed "not planned" on 2026-07-22. Read and
write traits exist but "the main implementation cannot yet be replaced… not on
the roadmap"
([discussion](https://github.com/GitoxideLabs/gitoxide/discussions/1281)). The
maintainer's advice to someone wanting a server: build your own from `gix-pack`
and `gix-revwalk`. MIT / Apache-2.0. No production git server on gix was found.

**Without cgo.** libgit2 compiles to WebAssembly for browsers (`wasm-git`); no
production use under a Go WebAssembly runtime was found. The pattern with
production precedent in Go is a sidecar running real `git`, which needs a local
filesystem — the opposite of our storage model.

Ideas worth borrowing:

1. **The refdb write contract:** every reference update carries its expected old
   value, and transactions are lock / unlock around deferred writes. It maps
   directly onto conditional writes.
2. **The ODB contract:** `refresh` on a miss (another replica may have published
   a pack), a whole-pack ingest hook, and `read_header` that answers type and
   size without inflating the object.
3. **gitoxide's concurrency model:** shared state is immutable and `Sync`; each
   request takes a cheap handle that owns its own caches. This is the exact fix
   for the go-git first-read race.
4. **Pack builders without delta reuse or bitmaps are slow** — both libraries
   confirm it. Keep serving a byte-range copy of stored packs.

### Staying on go-git: v6, a fork, and the other pure-Go libraries

**go-git v6.** Five alphas (2026-04-01 to 2026-07-29), no beta, no stated date
for a stable release; the
[migration guide](https://go-git.github.io/docs/tutorials/migrating-from-v5-to-v6/)
calls it "subject to change", with the transport API and `plumbing.Hash` still
moving, and it requires go-billy v6 and a mandatory `Repository.Close()`.

- The race is fixed there by
  [PR #2134](https://github.com/go-git/go-git/pull/2134) (merged 2026-05-29, in
  alpha.5): an `RWMutex` and a singleflight around the index, `Reindex` swapping
  in a fully built map, plus `hasIncomingObjects` (#2131) and the HTTP serve
  path (#2128).
- **It still promises no thread safety.** Tracking issue
  [#773](https://github.com/go-git/go-git/issues/773), "Concurrency Issues", is
  open and says "go-git is not thread-safe"; in it (2023-05) a maintainer named
  index and pack-map loading as the main cause of spurious "object not found".
- Gains for a server: redesigned transport with protocol v2 server and
  `ReceivePackHooks`, `.rev` reverse index, commit-graph generation v2, a file
  descriptor pool "for servers handling concurrent per-request repositories".
  Not present: multi-pack-index, bitmaps (#1971), reftable (PR #2098 open).

**`releases/v5.x`** is maintained (latest commit 2026-09-07) and takes community
backports, but the race is not fixed there: `ObjectStorage` has no mutex at all.
PR #2134 cannot be cherry-picked — it depends on v6-only packages — but a
v5-native patch is an estimated 50–100 lines. go-git published 2 security
advisories in 2025 and **10 in 2026 so far**, across eight v5 releases at roughly
one every three to four weeks, touching `idxfile`, `packfile` and `dotgit`. A
fork means rebasing a small patch about monthly.

**Other pure-Go libraries.** None is viable: `furgit` ("You should not use
furgit… unfinished research project"), `driusan/dgit` (dormant since 2022),
wrappers that shell out. **Tigris objgit is the same architecture we had** —
go-git plus a billy filesystem on S3 — and its
[blog](https://www.tigrisdata.com/blog/objgit/) describes the same disease:
pushes exploded into loose objects and excessive GET and LIST calls.

**What the Go forges do.** All of them shell out to the git binary. Gitea made
the native backend the default in 1.14 (measured about 99 MB with git against
about 240 MB and growing with go-git) and on 2026-09-16 disabled go-git builds
for stable releases, "we can completely remove gogit support in the future".
Forgejo removed go-git in v9.0 (2024-10), citing "less features and a history of
corrupting repositories"
([release notes](https://forgejo.org/2024-10-release-v9-0/)). soft-serve, Gitaly
and Sourcegraph's gitserver all spawn `git upload-pack`.

**The researcher's recommendation** was a small local fork of v5 now and v6 when
it stabilizes. **We chose differently**, and the same evidence is why: the fork
patches one race in a storage layer that promises no thread safety in any
version, that every Go forge has abandoned for serving, and that would still
force the filesystem emulation on us. Owning `storage.Storer` removes our
dependence on `storage/filesystem` and `dotgit` altogether — the code the
advisories keep landing in — while keeping the parts of go-git that are sound.

### How other systems put git on object storage

| System | Layout in the store | Atomic reference updates | Git engine | Local copy? | Numbers and limits |
|---|---|---|---|---|---|
| **tobi/walgit** (Rust) | `manifest.pb` (sequence, live pack set, checkpoint pointer); immutable `log/<seq>.pb`; content-addressed packs with `.idx/.rev/.bitmap/.commit-graph`; `checkpoints/<seq>/` with a folded ref snapshot; `leases/` | One compare-and-swap on the manifest, the only commit point; a 412 means re-validate and retry; concurrent pushes group-committed into one swap. No database, no leader. | Stock `git` for upload-pack and repack; its own receive-pack and WAL | Yes on placed hosts; also a remote-reader mode and a "history pack" mode (commits and trees local, blobs in the bucket) | Push is 5 requests, any read is 1 conditional GET. On GCS: a 304 in 15–18 ms, GET/PUT 60–80 ms, **compare-and-swap on one object about 1 write/s**. Clones offloaded through bundle-uri. |
| **Tigris objgit** (Go) | Bare-repo format through a billy filesystem; v2 re-makes packs as `.bin` (≤128 MiB, zstd) plus a `.cue` index of fixed 58-byte records | Not documented; relies on Tigris's non-standard `RenameObject` | go-git | Yes — downloads the whole pack while racing range reads | Push 231 → 18 requests. No compaction, no auth, 2 GiB file cap. |
| **Cloudflare Artifacts** | One Durable Object per repository; objects in its SQLite, chunked to 2 MB rows; snapshots to R2 | Single writer | Custom Zig → ~100 KB WebAssembly: pack, delta, smart HTTP v1/v2 from scratch | No | Stores the raw delta beside the resolved object so it never computes deltas. |
| **Mononoke** (Meta) | Immutable blobstore; git objects are derived data | Bookmarks in SQL | Custom Rust | No | Delta manifests precomputed asynchronously, off the write path. |
| **JGit DFS** | Packs plus a pack-description list; reftables as a pack extension | One transaction wins on `maxUpdateIndex` | JGit | No (block cache) | See above. |
| **Gitaly** (GitLab) | Block storage only; object storage for bundle-URI and backups | Per-partition WAL, single writer, Raft | Stock git | Yes | NFS dropped for latency; "store repos in object storage" (epic #479) still open, Raft pursued instead. |
| **GitHub Spokes** | Three local-disk replicas | Majority lock; identical result on a quorum or roll back | Stock git | Yes | 38M+ repositories. |
| **awslabs/git-remote-s3** | `<prefix>/<ref>/<sha>.bundle` | Per-ref lock created with `If-None-Match: *`, 60 s TTL | `git bundle` | Client | A full bundle per push. |
| **dekobon/git-remote-object-store** | `packs/<sha>.pack` + `.idx`; per-branch `chain.json`; a baseline bundle | `If-None-Match: *` plus TTL, on S3 and Azure | gitoxide | Client | Compaction advised at ~20 segments or 100 MiB; two-phase GC with a 24 h grace window. |
| **FUSE mounts** | — | — | Stock git | — | mountpoint-s3 has no rename, random writes or locks: stock git cannot run on it. JuiceFS: a clone in 2m12s against 2.8 s locally. |

**On engines:** none of the production-grade systems uses go-git. They use stock
git (walgit, Gitaly, Spokes), JGit, or a custom protocol and pack layer
(Mononoke, Cloudflare).

Ideas worth borrowing:

1. **One compare-and-swapped manifest as the only commit point** (walgit): the
   live pack set, the references or a pointer to them, a sequence number. It
   replaces an external lock, turns a reference listing into one conditional GET
   (a 304 in 15–18 ms) instead of a LIST, and takes a push from 9 requests to
   about 5 ([ROUNDTRIPS.md](https://github.com/tobi/walgit/blob/main/docs/ROUNDTRIPS.md)).
2. **Group-commit concurrent pushes into one swap.** A single object takes about
   one conditional overwrite per second, so batching is what makes a
   single-manifest design viable. walgit's other rules: make independent PUTs
   parallel ("depth before count"), and let the 412 be the verification rather
   than reading first.
3. **An append-only log with periodic checkpoints of folded references**
   (walgit, Gitaly, reftable), which bounds what a cold replica must read.
4. **Offload clones** with bundle-uri or packfile-uri served straight from the
   bucket or a CDN.
5. **Never compute deltas at serve time** (Mononoke, Cloudflare, objgit): keep
   pack serving a byte-range copy.

## What is portable across object stores

The storage layer spoke only the S3 wire protocol (through minio-go). That is not
the common interface it looks like.

**The S3 protocol cannot carry compare-and-swap to every store.** Google Cloud
Storage's S3-compatible XML API applies `If-Match` / `If-None-Match` to GET and
HEAD only: **a conditional PUT returns 200 and overwrites** (observed 2026-08,
[ClickHouse #115266](https://github.com/ClickHouse/ClickHouse/issues/115266);
[header reference](https://docs.cloud.google.com/storage/docs/xml-api/reference-headers)).
Its real precondition is `x-goog-if-generation-match` on the native API. Azure
Blob Storage has no S3 API at all, and no gateway in front of it is sound enough
for a durability-critical store (MinIO's gateway was removed in 2022; S3Proxy
supports a subset of conditional PUT and claims no production readiness).

| Store | Create if absent | Replace if unchanged | Through the S3 API? | Read / list after write | Version token of a small object |
|---|---|---|---|---|---|
| AWS S3 | `If-None-Match: *` (2024-08) | `If-Match` on PUT (2024-11-25) | native | strong since 2020-12 | MD5 for plaintext or SSE-S3; opaque for SSE-KMS, SSE-C, multipart |
| Google Cloud Storage | `x-goog-if-generation-match: 0` | generation match | **No — silently ignored** | strong, including list | use the generation; XML and JSON ETags differ |
| Azure Blob Storage | `If-None-Match: *` | `If-Match` ETag, and leases | no S3 API | strong | opaque version stamp |
| MinIO | yes | yes (2023-02) | yes | strong | MD5 |
| SeaweedFS | yes | yes | yes — atomic because a key's writes route to one owner filer, so it depends on stable routing (vendor blog, 2026-07) | *unverified*; depends on the filer store | *unverified* |
| Cloudflare R2 | yes | yes | yes | strong, including list | *unverified* |
| Ceph RGW | yes | yes; only `*` valid for `If-None-Match` | yes, but the Tentacle release still carries fixes for conditional PUT and DELETE | strong in one zone | MD5 |
| Backblaze B2 | *unverified* | *unverified* | *unverified* | *unverified* | *unverified* |
| Tigris | yes | yes | yes | strong per region, eventual across | *unverified* |

Alibaba OSS answers a conditional PUT with 501
([walgit #67](https://github.com/tobi/walgit/issues/67), 2026-09). MinIO's
community edition went into maintenance mode in 2025-12 and its repository was
archived on 2026-04-25: a frozen target, still useful to test against.

**Entity tags.** "Same tag, same content" holds for a small single-part object
on every store listed — so revalidating a small object by its tag is sound — but
the tag must be treated as an opaque version token, never as a hash: on
MD5-tagged stores a collision can be crafted and git content is
attacker-controlled; `If-Match` cannot see A→B→A; on Azure and native GCS the tag
also changes when only metadata does. Never revalidate multipart or SSE-KMS
objects by MD5.

**Go abstraction libraries.**

| Library | Providers | Conditional write | Version token | Presign | Copy / bulk delete | Notes |
|---|---|---|---|---|---|---|
| `gocloud.dev/blob` | S3, GCS, Azure, file, memory | `IfNotExist` only | ETag only | yes | copy, no bulk delete | v0.46.0 (2026-06), active; native preconditions reachable through `As` / `BeforeWrite` hooks |
| `thanos-io/objstore` | GCS, S3, Azure stable; six more in beta | `IfNotExists`, `IfMatch`, `IfNotMatch`, with capability discovery | typed `ObjectVersion` (generation or ETag) | no | neither | the right API shape |
| Apache OpenDAL (Go) | 40+ | in the Rust core; Go exposure *unverified* | *unverified* | yes | copy | no cgo, but loads a native Rust library and needs libffi |
| rclone `fs.Fs` | 70+ | none | none | public links only | varies | internal API |

**Recommendation.**

- **Define a small interface of our own, with three drivers:** generic S3 on
  minio-go (S3, R2, MinIO, SeaweedFS, Ceph, Tigris), native GCS using
  generations, native Azure using ETags. Borrow the *shape* of
  `thanos-io/objstore` — a version-typed token, capability discovery, one
  "condition not met" error — not the library, which lacks copy, presign and
  bulk delete.
- **The operation set:** GET, ranged GET, HEAD returning an opaque version
  token; PUT, PUT-if-absent, PUT-if-version-matches, streaming or multipart PUT,
  DELETE and bulk delete; strongly consistent prefix LIST, recursive and by
  directory; server-side COPY; presigned GET. (The researcher suggested putting
  bulk delete and presigning behind a capability flag with a client-side
  substitute; we require them of every driver instead.)
- **Never rely on:** an ETag being an MD5; conditional completion of a large
  upload being *documented* everywhere; conditional DELETE or COPY; list ordering
  beyond lexicographic. (On the second point, found when the drivers were
  written: Azure documents that a Put Block List may be conditional. GCS does not
  allow preconditions on its XML multipart uploads; its JSON resumable uploads
  accept `ifGenerationMatch` when the upload is initiated, and the emulator holds
  the upload to it at completion, but no documentation page says when it is
  enforced. S3 accepts `If-Match` on CompleteMultipartUpload. A caller that needs
  the condition to hold on every store states the object's size, so the write is
  one request.)
- **Probe at startup.** Create-if-absent twice must answer "condition not met";
  a stale version must too; a list after a write must show the key. On failure,
  fail closed. This is what catches GCS-over-HMAC, OSS, B2 and older Ceph or
  MinIO. (The researcher also offered "or keep the external lock for that
  store"; see *Decisions taken* for why there is no such second path.)

## Lessons from object-store-native data systems

Analytic and streaming systems paid to learn how an object store wants to be
used: few, large, immutable objects; a footer that says where everything is; a
manifest instead of a listing; metadata kept apart from bulk data. Each pattern
below is mapped onto git.

| Pattern, and where it comes from | Mapping to git | Benefit | Risk |
|---|---|---|---|
| **A manifest instead of LIST.** Mimir and Cortex keep a per-tenant `bucket-index.json.gz` (block list, deletion marks, `updated_at`); readers stop listing and fail if the index is staler than a bound. SlateDB and Iceberg swap a manifest pointer atomically. | One manifest object per repository: the live segments with size, object count, type mix and footer / filter / index offsets, the reference-table stack, the side indexes, and tombstones with timestamps. | A cold open is one GET and never a LIST. S3 prices LIST like PUT ($0.005 per 1,000 against $0.0004 for GET). | It is the contention point of a repository: needs a retry loop and must stay small. |
| **Compare-and-swap by conditional write.** S3 `If-None-Match: *` (2024-08) and `If-Match` (2024-11); GCS `ifGenerationMatch`; Azure ETag `If-Match`. | Commit a push by swapping the manifest, or by creating `manifest/000…N` if absent (the SlateDB / Delta numbered-log style). | One round trip gives a linearizable update across stateless replicas. | Stores without conditional writes have **no safe pure-object-store fallback**: SlateDB and Delta Lake both fall back to an external transactional store. Probe at startup; refuse multi-writer mode without it. |
| **A self-describing segment with a footer.** Parquet puts page indexes and Bloom filters by the footer; Lance opens in one or two reads, exactly one if the manifest carries the metadata size. A ClickHouse "compact" part is one file where a 109-column "wide" part is 227 — named in its docs as the main cause of slow inserts on S3. | `[pack bytes][index][filter][type/offset map][footer]` in one object, its tail offset recorded in the manifest. | One PUT per push instead of three; one ranged read of the tail opens it. | Such an object cannot be handed out verbatim as a packfile-uri, which must be a pure pack. Keep the pack bytes as a clean prefix (*whether git tolerates trailing bytes there is unverified*), or write separate `.pack` objects only for large compacted bases. |
| **A hotcache.** Quickwit stores a footer per split, under 0.1% of its size, holding exactly the bytes a cold searcher needs first. Thanos builds an index-header by ranged reads and keeps it on local disk. | The footer region holds the index fanout, sampled object offsets, the filter, the boundaries of each object-type run, and that segment's slice of the commit-graph. Cached per segment hash. | A cold replica serves an advertisement and the start of a history walk in about two GETs. | Must stay bounded, and must agree with the pack: it is rebuildable, not authoritative. |
| **Columnar separation.** Parquet column chunks; ClickHouse wide parts. Git's own [pack heuristics](https://git-scm.com/docs/pack-heuristics) already sort by type, then name and size, write in recency order and put each delta base before its delta. | At compaction — not at push — write type-clustered runs: commits, then trees, then blobs. For large repositories, split a "meta" segment from a "blob" segment. | History walks and blobless or treeless clones are contiguous range reads that never touch blob bytes. | Splitting doubles the object count of a small repository: split only above a size threshold (ClickHouse's `min_bytes_for_wide_part` defaults to 10 MB). Deltas across segments must be forbidden or tracked. |
| **An LSM of reference tables.** Reftable: sorted, prefix-compressed blocks with restarts and an index; tables stack, and git auto-compacts to keep each table at least twice the size of the next newer one. | References are a stack of immutable tables named in the manifest; a small push writes a delta table, or is inlined in the manifest below a few KiB. | Atomic multi-reference transactions; an advertisement is a merge of a few tables. | A deep stack is many GETs when cold: bound the depth and cache the tables. |
| **Derived side indexes.** Git's commit-graph (generation numbers, changed-path Bloom filters — which speed `log -- path` and `blame`) and reachability bitmaps. | Commit-graph chain, bitmaps, path→last-commit and tree-entry indexes, each an immutable object keyed by the segment set it covers and listed in the manifest. | Web log, blame and directory listings without inflating trees. | Staleness: always be able to fall back to a walk; rebuild in the compactor. |
| **Batching writes across tenants.** WarpStream flushes one file per agent every 250 ms or 4 MiB, mixing all topics (its figure for a file per partition every 100 ms: about $130 a month per partition in PUTs). AutoMQ gives a large stream its own object and bundles small ones, compacting them into per-stream objects later so deletion can be precise. | A group-commit log object spanning repositories, compacted into per-repository segments. | PUT count falls by the batch factor. | Adds push latency; breaks per-repository isolation, deletion and erasure; cannot serve packfile-uris; still needs the per-repository swap. |
| **Fencing writers.** SlateDB bumps a `writer_epoch` and writes a fencing file if-absent; Neon puts a generation in the object key, so a zombie writes where no reader looks (*secondary source*). | Fence the one compactor / GC role per repository with an epoch in the manifest. | No zombie compactor deletes live segments. | Pushers must stay multi-writer through compare-and-swap, not be fenced. |
| **Compaction and GC with grace.** RocksDB universal (size-tiered) compaction trades write amplification for read and space amplification. Iceberg's orphan cleanup defaults to **3 days**, because a shorter window can delete files of writes still in progress, and a writer that loses the commit race leaves orphans by design. Mimir uses deletion marks and a `deletion-delay`. | Compact geometrically; tombstone in the manifest with a timestamp; delete only after a grace period longer than the longest request; sweep orphans by LIST, rarely and offline. | Readers never meet a 404 in the middle of a clone. | Space amplification is billed storage: tier the largest packs and exempt them from routine merges. |
| **Caching immutable blocks.** JGit's `DfsBlockCache` is keyed by (stream, position), loads once per key and uses clock eviction. turbopuffer's first query reads the object store (p50 874 ms on a million documents), later ones the NVMe cache (p50 14 ms). S3's guidance is 8–16 MB ranged requests, with 100–200 ms first-byte latency. | Cache key (segment content hash, aligned block number): it never needs invalidating. Only the manifest is revalidated, by a conditional GET. | High hit rates; replicas can share a key space. | An aligned range over-reads a small pack: fetch anything under about 1 MiB whole. |

Recommendations, in order:

1. **A per-repository manifest swapped by compare-and-swap, and never a LIST on
   the hot path.** Put the reference stack and the segment list in one object so
   a push — new segment and reference update — is a single atomic swap. Fail
   closed on stores without conditional writes.
   ([Mimir bucket index](https://grafana.com/docs/mimir/latest/references/architecture/bucket-index/),
   [SlateDB manifest RFC](https://slatedb.io/rfcs/0001-manifest/))
2. **One single-object segment per push** (pack, index, filter, footer), its tail
   offsets in the manifest, so opening it is one ranged read and no probing.
   ([Lance file format](https://lance.org/format/file/))
3. **A hotcache footer**, contiguous and well under 1% of the segment, cached on
   local disk by segment hash.
   ([Quickwit](https://quickwit.io/blog/quickwit-101),
   [Thanos index-header](https://thanos.io/tip/operating/binary-index-header.md/))
4. **Cluster by object type at compaction**; split meta from blob segments only
   above a size threshold.
5. **References as a reftable-like stack** with geometric auto-compaction, small
   deltas inlined in the manifest. ([reftable](https://git-scm.com/docs/reftable))
6. **Commit-graph with changed-path Bloom filters as the first side index** — the
   largest win for the web UI — then bitmaps once there are large compacted
   bases, then path→last-commit.
   ([Bloom filters in the commit-graph](https://devblogs.microsoft.com/devops/super-charging-the-git-commit-graph-iv-bloom-filters/))
7. **GC by manifest tombstones and a grace period**, under an epoch-fenced
   compactor, with a rare orphan sweep.
   ([Iceberg maintenance](https://iceberg.apache.org/docs/latest/maintenance/))
8. **Defer batching across repositories.** Coalesce concurrent pushes to one
   repository first; if PUT cost forces more, follow AutoMQ: a shared log object
   compacted soon after into per-repository segments.
   ([AutoMQ S3 storage](https://docs.automq.com/automq/architecture/s3stream-shared-streaming-storage/s3-storage),
   [WarpStream architecture](https://docs.warpstream.com/warpstream/overview/architecture))

Read sizing: fetch segments under about 1 MiB whole, use 8–16 MB parallel ranges
for large ones, and spread key prefixes (S3 quotes 3,500 writes and 5,500 reads a
second per prefix;
[guidelines](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance-guidelines.html)).
S3 Express One Zone is a vendor extra: an optional cache tier at most.

## Lessons from canonical git, and from GitHub and GitLab operating it

Canonical git made most of these mistakes first and fixed them later, in public.
Items marked *recalled* were not checked against a source.

### The object database: what matters for serving

- **One global object-id → (pack, offset) index per repository, updated by
  appending layers.** Without a multi-pack-index, lookup is a linear search
  across packs; rewriting a whole one examines every object, so git 2.47
  (2024-10) added an incremental chain whose updates cost "proportional to the
  new objects" — which did not yet support multi-pack bitmaps.
  ([packed object store](https://github.blog/open-source/git/gits-database-internals-i-packed-object-store/),
  [git 2.47](https://github.blog/open-source/git/highlights-from-git-2-47/))
- **A reverse index (offset → position) and a CRC32 per object.** `.rev` cut
  fetch CPU by about 80% on tested repositories and 35% across GitHub's fleet.
  The CRC exists so compressed bytes can be copied pack to pack without
  undetected corruption — which verbatim reuse depends on.
  ([scaling monorepo maintenance](https://github.blog/open-source/git/scaling-monorepo-maintenance/),
  [pack format](https://git-scm.com/docs/gitformat-pack))
- **Reachability bitmaps are the largest single win for clones.** On the Linux
  kernel, "Counting objects" went from minutes of CPU to under 3 ms. Build them
  over the multi-pack order with the preferred pack first; add the name-hash
  cache and commit lookup table; add pseudo-merge bitmaps (2.46) for
  repositories with very many references.
  ([counting objects](https://github.blog/engineering/architecture-optimization/counting-objects/),
  [bitmap format](https://git-scm.com/docs/bitmap-format))
- **A commit-graph with generation numbers v2, as a split chain** that merges a
  layer when it is under twice the one below. `merge-base --is-ancestor` went
  from 2.64 s to 0.02 s, which speeds negotiation and reachability checks.
  ([commit-graph](https://git-scm.com/docs/commit-graph),
  [history queries](https://github.blog/open-source/git/gits-database-internals-ii-commit-history-queries/))
- *Recalled:* changed-path Bloom filters help path-limited log and blame, not
  fetch. Record type and inflated size in the index: in canonical git a
  deltified object's type costs a walk of its delta chain, and `object-info` and
  `blob:limit` want both cheaply.

### Staying correct while packs change underneath a reader

- **On a miss, reload the pack list and retry before answering "missing".** Git
  added `reprepare_packed_git` for exactly the race between a read and a repack
  ([Jeff King, 2016-06](https://ratatoskr.run/git/2016/06/7283708)).
- **A global index that names a deleted pack must fall back to the surviving
  copies.** Still being fixed in git in **2026-08**: a geometric repack deletes
  an object's "owning" pack, the stale multi-pack-index routes the lookup to it,
  and upload-pack's quick lookups skip the retry
  ([Elijah Newren](https://ratatoskr.run/git/2026/08/17431252/t)). For us: pack
  sets are immutable manifests, and a pack is deleted only after a grace period
  longer than any reader's lifetime.
- **Never prune unreachable objects at once; keep per-object mtimes in a cruft
  pack.** Git's docs admit the two-week grace period and mtime freshening "fall
  short of a complete solution". Cruft packs with an `.mtimes` file (2.37)
  replaced exploding unreachable objects into loose files. A write that reuses an
  existing unreachable object must freshen its mtime.
  ([git-gc](https://git-scm.com/docs/git-gc), [cruft packs](https://git-scm.com/docs/cruft-packs))
- **The geometric invariant:** each pack holds at least `factor` times as many
  objects as the next smaller; roll up only the smallest packs that violate it;
  the largest becomes the preferred pack for bitmaps. GitHub's average repack
  fell from about a minute to 15 seconds.
  ([git-repack](https://git-scm.com/docs/git-repack))

### Receiving a push

- **Quarantine incoming objects: visible only once the checks pass, and before
  the references move.** Without it a rejected pack "cannot just be deleted;
  other processes may be depending on it", and lingers for the whole prune
  window. Hooks must never point references at quarantined objects. Git is
  abstracting this into object-database transactions — the natural interface for
  our engine.
  ([the 2016 commit](https://github.com/git/git/commit/722ff7f876c8a2ad99c42434f58af098e61b96e8),
  [receive-pack](https://git-scm.com/docs/git-receive-pack))
- **The connectivity check must see the quarantine and the main store, packs
  first.** Git 2.54 changed the lookup order so each object paid a failed lookup
  in the quarantine directory, and pushes to Bitbucket slowed badly on NFS
  ([2026-07](https://ratatoskr.run/git/2026/07/17287379/t)). An object store has
  the same per-miss latency: probe membership filters before any remote lookup.
- **Check objects on ingest and cap the input size** (`receive.maxInputSize`);
  what fails simply stays in quarantine. `unpackLimit` exists only to avoid many
  tiny packs — geometric merging is our equivalent.
- **Hook order:** pre-receive once, all or nothing; update per reference;
  post-receive informational. Advertise `atomic`. Reference transactions go
  preparing → prepared → committed or aborted.

### References

- **Do not copy the files backend.** Deleting one reference rewrites all of
  `packed-refs`; multi-reference updates are not atomic; case-insensitive and
  Unicode-normalising filesystems collide names. Reftable becomes the default in
  Git 3.0
  ([BreakingChanges](https://raw.githubusercontent.com/git/git/master/Documentation/BreakingChanges.adoc)).
  The 2013 `packed-refs` race — a stale cached `packed-refs` plus pack-refs
  deleting the loose reference — cost GitHub real objects when combined with gc.
- **Model references as a reftable-style stack:** immutable sorted tables, a
  `tables.list` manifest swapped atomically (on an object store, a conditional
  PUT), monotonic update indexes, deletions as tombstones, geometric compaction,
  and a reader that finds a table missing re-reads the manifest. With 866k
  references a lookup went from 410 ms to 34 µs; deleting one of a million took
  2.0 ms against 230 ms. Sequential single-reference transactions are 30–48%
  *slower* than the files backend — batch them.
  ([reftable](https://git-scm.com/docs/reftable),
  [GitLab's guide](https://about.gitlab.com/blog/2024/05/30/a-beginners-guide-to-the-git-reftable-format/))

### Serving

- **Protocol v2:** no reference advertisement, `ls-refs` takes `ref-prefix`, and
  every command is one stateless round trip, so any replica can answer. Support
  `want-ref`, designed for load-balanced servers whose replicas may disagree.
- **A fetch is cheap when bitmaps and verbatim reuse both apply.** Single-pack
  reuse forced operators to keep one big pack; git 2.44 added reuse across packs
  ([git 2.44](https://github.blog/open-source/git/highlights-from-git-2-44/)).
- **Offload and deduplicate the rest.** packfile-uris and bundle-uri map directly
  onto presigned object-store URLs. Gitaly's pack-objects cache deduplicates
  concurrent identical CI fetches (entries live 5 minutes).

### Operations and forks

- **Shared fork storage needs delta islands.** GitHub's `network.git` made bitmap
  clones 40% slower until deltas were restricted to objects sent together.
- **Never garbage-collect a shared pool**: it cannot know what its members still
  need ([GitLab object deduplication](https://docs.gitlab.com/development/git_object_deduplication/)).
- **Sharing objects shares their contents.** Anyone who knows an object id can
  obtain it through have/delta tricks: "keep private data in a separate
  repository"
  ([transfer-data-leaks](https://raw.githubusercontent.com/git/git/master/Documentation/transfer-data-leaks.adoc)).
- **Serialize writes per repository through a log** (Gitaly's single-writer
  TransactionManager; Spokes commits on 2 of 3 replicas). GitHub rejected shared
  filesystems because "Git is very sensitive to latency" — for us an aggressive
  local cache is not optional.
- **Housekeeping is heuristic:** repack objects more often as a repository grows,
  references less often.

### Foot-guns

- **Hash everything ingested, with collision-detecting SHA-1** (GitHub deployed
  sha1dc on 2017-03-20).
- **Enforce these fsck checks on ingest:** `gitmodulesUrl`, `gitmodulesPath`,
  `gitmodulesName`, `gitmodulesSymlink`, `gitmodulesUpdate`, `hasDot`,
  `hasDotdot`, `hasDotgit`, `fullPathname`, `treeNotSorted`, `duplicateEntries`,
  `nullSha1`, `zeroPaddedFilemode`, `largePathname`, `gitattributes*`,
  `badRefName` ([git-fsck](https://git-scm.com/docs/git-fsck)).
- **All ten `check-ref-format` rules**, and a decided policy on reference names
  that collide by case or Unicode normalization: the store can hold both, clients
  on case-insensitive filesystems break.
- **Protocol details:** an empty repository advertises `capabilities^{}` with a
  zero id; HEAD first, then references in C-locale order; report the symref
  target of HEAD, unborn included (*recalled*).
- **`hideRefs` hides names, not objects.** `allowAnySHA1InWant` turns off
  reachability-based access control; `allowReachableSHA1InWant` is documented as
  expensive, and bitmaps make it cheap.

## What all of it agrees on

Seven lines of research, arrived at independently, converge on the same engine:

1. **Immutable state, swapped whole.** The pack set and the reference set are
   immutable snapshots; a change publishes a new one; a reader that misses
   re-reads the snapshot and retries. (JGit's pack list, git's
   `reprepare_packed_git`, reftable's `tables.list`, Iceberg, SlateDB, walgit,
   gitoxide's `Sync` state with per-request handles.) This is also the fix for
   the go-git race: there is nothing lazily built for two readers to race on.
2. **One commit point per repository, by compare-and-swap** — a manifest holding
   the live segments and the references or a pointer to them. It replaces the
   external lock and every hot-path LIST. It needs a store with conditional
   writes, reached through that store's *native* API, and a probe at startup that
   fails closed.
3. **Everything written is a pack; nothing is loose.** Append a pack and its
   index — visible when both exist — with no coordination; coordinate only where
   packs are *replaced*.
4. **Delete nothing a reader may still need.** Tombstone in the manifest, delete
   after a grace period longer than the longest request, keep unreachable objects
   in a cruft pack with mtimes, fence the one compactor.
5. **Quarantine a push:** objects first, checks, references last; a rejected pack
   leaves no debt.
6. **References as a reftable-style stack**, with atomic multi-reference
   transactions and batched single-reference writes.
7. **Never compute at serve time what can be copied:** verbatim pack reuse across
   packs, presigned pack and bundle URLs, and — in order of value — reachability
   bitmaps, a commit-graph with generation numbers, a global object index.
8. **Lay data out for range reads:** one self-describing object per segment with
   a footer hotcache; objects clustered by type at compaction so history walks
   never touch blob bytes; caches keyed by content hash and block, which never
   need invalidating.
9. **Validate everything that arrives:** hash it, check it, apply the reference
   name rules, cap its size, check connectivity with filters first.

## Decisions taken

**Each store gets a driver for its native API, behind one interface of ours
(`gitstore/objstore`).** Azure Blob Storage has no S3 endpoint — Microsoft's own
answer is "no official S3 support for Blob Storage", and what exists is
third-party gateways (S3Proxy, Flexify.IO)
([Microsoft Q&A](https://learn.microsoft.com/en-us/answers/questions/1183760/s3-api-support-over-azure-blob-storage)) —
and Google Cloud Storage's S3 endpoint is unsafe for compare-and-swap. A gateway
would put someone else's component, with partial conditional-write support, on
the write path of the only durable state we have. Azure's native API offers every
operation the interface names, conditional writes included (`If-Match` /
`If-None-Match` on the ETag), as does GCS's (generation preconditions); both
official Go SDKs are pure Go.

**That driver switch is the one branch the design tolerates**, and it is tolerated
because it is not a fallback: the operator states which store protocol the
deployment speaks, and nothing infers it. Everywhere else the rule is one explicit
code path — no capability flags with a lesser alternative, no "try this, else
that", no value chosen silently for something the operator should state.
Concretely: `objstore.Bucket` has no capabilities to ask about and no
"unsupported" error; every driver implements every operation with its whole
meaning; and `objstore.Conform` proves the guarantees against the live store at
startup and refuses to run on one that fails them. A store that cannot arbitrate a
write is not run on differently. It is not run on.

**The server's configuration says the same thing, in the operator's words.**
`BLEEPHUB_OBJECT_STORE` is `s3`, `gcs` or `azure`, required whenever a bucket is
named and an error when none is. What is stored where has one name whichever
driver — `BLEEPHUB_GIT_BUCKET` and `BLEEPHUB_GIT_PREFIX` for repositories,
`BLEEPHUB_OBJECT_BUCKET` and `BLEEPHUB_OBJECT_PREFIX` for the byte store — and a
prefix is required with its bucket, because the two may share one and nothing may
guess where each lives. How the store is reached is named after the driver
(`BLEEPHUB_S3_ENDPOINT`, `BLEEPHUB_S3_REGION`; `BLEEPHUB_AZURE_ENDPOINT`,
`_ACCOUNT`, `_KEY`; `BLEEPHUB_GCS_ENDPOINT`, `_CREDENTIALS_FILE`), one endpoint
per driver serves both stores, and only the chosen driver's settings may be set:
another's is an error, since a configuration must say one thing. There are no
defaults — not the byte store's old `objects` prefix, not the region's old
`AWS_REGION`-then-`us-east-1` chain; an unset `BLEEPHUB_S3_ENDPOINT` is the one
unset value with a meaning, AWS S3 itself in the stated region. The tunables
keep one meaning across drivers (the breaker's became
`BLEEPHUB_OBJECT_STORE_BREAKER_*`, and `BLEEPHUB_GITSTORE_MULTIPART_BYTES` is
the part, block or chunk size, held by each driver to its own rule). Everything
is validated together at startup and reported at once, the names the server read
before this are not read at all, and the switch itself is one function,
`openBucket` in `internal/gitbackend`.

## The order of work

1. **Swap the engine, keep the bucket layout.** Implement `storage.Storer`
   directly against the object store through a small object-store interface with
   per-provider drivers, with rule 1's concurrency model; delete the filesystem
   emulation. No data-format change, so the benchmarks in `gitstore/bench`
   compare request for request with what it replaces.
2. **Then change the layout**, a rule at a time, each measured: the manifest and
   compare-and-swap (rules 2 and 4), no loose objects (3), reftable-style
   references (6), single-object segments with a footer (8), quarantine (5), side
   indexes (7).

## Step 2, as built: the manifest and compare-and-swap (rules 1, 2, 4, 5, and the start of 6)

Built on 2026-09-21. `docs/git-storage.md` describes the result; this records
what was decided on the way and what was measured.

**What was built.** One JSON object per repository, `manifest`, is the only
commit point: `format`, `sequence`, the live `packs` (name, sizes of pack, index
and filter, object count, what wrote it, when), the `retired` packs with the
time of their retirement, and `refs` as a pointer to an immutable snapshot
object plus an inline list of changes (a value or a tombstone), folded into a
new snapshot past 512 changes. Every change is a mutation committed by
`Put` … `IfVersion` with re-read, re-apply and jittered backoff on 412 (rule 2);
commits to one repository that arrive during a swap share the next (walgit's
group commit); a push is one transaction — pack and reference updates in one
swap, decided on through a view that alone reads the quarantined pack (rule 5);
compaction commits by one swap and retired packs are deleted a grace period
later (rule 4). The per-reference objects, the `.superseded` markers, the
in-process reference locks of the object-store backend and the durable SQL lock
are gone. Pack, index, filter and loose-object keys did not move; loose objects
remain (rule 3 is the next step), discovered by a listing taken only on a miss.

**What the three stores' documentation says about a conditional read**, since
revalidation leans on it: S3's GetObject takes `If-None-Match` and returns 304
when the ETag matches
([API_GetObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html));
Azure's Get Blob does the same, and its table of response codes gives 304 for an
unmet `If-None-Match` on a read
([conditional headers](https://learn.microsoft.com/en-us/rest/api/storageservices/specifying-conditional-headers-for-blob-service-operations));
Google Cloud Storage's `ifGenerationNotMatch` "fails with a 304 Not Modified
response" when the generation matches
([request preconditions](https://docs.cloud.google.com/storage/docs/request-preconditions)).
GCS's XML download — which the client uses because it alone documents the
generation as a response header — has `If-None-Match` on the ETag but no
generation-not-match header
([XML headers](https://docs.cloud.google.com/storage/docs/xml-api/reference-headers)),
and the version token is a generation. `ifGenerationNotMatch` was tried and
dropped for three reasons: it is on the JSON API only, whose download documents
no headers to learn the new generation from, so a second request is needed
anyway; the JSON reference says of it that "if no live object exists, the
precondition fails" without naming the status
([objects.get](https://docs.cloud.google.com/storage/docs/json_api/v1/objects/get)),
and a deleted manifest answered 304 would read as "unchanged"; and
fake-gcs-server, the emulator the driver's suite runs against in CI, ignores the
parameter on `objects.get` (read in its source, `fakestorage/object.go`, on
2026-09-21). So the GCS driver asks for the object's description with a plain
`objects.get` and compares the generation itself: one small request when nothing
changed, two when something did, and nothing relied on but `objects.get` as
documented. What it costs over a 304 is a response body of a couple of hundred
bytes.

**One thing the research did not foresee.** Pack names are digests of their
contents, so a push that was refused and is made again uploads the same keys. A
sweep that deleted an old unnamed upload between that re-upload and its commit
would delete a live pack, and no store offers a conditional delete to prevent
it. So an orphan is not deleted when found: it is entered in the manifest's
`retired` list, and deleted a grace period later like any retired pack; a commit
that wants the name back in the first half of that period takes it back, and
after that is refused. Reference snapshots have keys no commit reuses and are
deleted directly.

**Measured** with the fakes, 2 ms injected latency, median of 3
(`gitstore/bench`; S3 requests, before → after):

| Storer level | before | after | | Git level (bleephub over smart HTTP) | before | after |
|---|---|---|---|---|---|---|
| push-initial | 10 | 7 | | push-initial | 8 | 4 |
| clone-cold | 5 | 3 | | replica-start | 25 | 27 |
| clone-warm | 2 | 1 | | clone-cold | 7 | 3 |
| push-incremental (10 pushes) | 51 | 50 | | clone-warm | 3 | 1 |
| fetch-incremental | 0 | 0 | | push-incremental (10 pushes) | 78 | 47 |
| probe-absent | 1 | 1 | | fetch-incremental | 3 | 1 |
| maintain | 14 | 6 | | clone-parallel | 20 | 9 |
| clone-parallel | 7 | 5 | | | | |
| refs-create (200) | 201 | 200 | | | | |
| refs-advertise | 213 | 4 | | | | |

The harness's Storer-level push is two calls — `packfile.UpdateObjectStorage`,
then `CheckAndSetReference` — and so two swaps: five requests a push, and seven
for the first (the read that finds no manifest, the swap that creates the
repository, three uploads, two swaps). The server's push is the transaction: a
pack, an index, a filter and one swap. `replica-start` rose by two because each
of the two stores' startup probes now proves the conditional read, one request
more apiece.

## Step 3, as built: nothing loose, a pack and one sidecar (rule 3, and rule 8 as far as rule 7 allows)

Built on 2026-09-21. `docs/git-storage.md` describes the result.

**Rule 8 as written collides with rule 7.** A single self-describing object per
segment — pack, index and filter together, with a footer — would make every
write one upload. But a stored pack is also what a packfile-URI and a bundle URI
hand to a client, as a presigned URL, and neither the URL nor the client can ask
for a byte range: the client downloads the object whole and gives it to
`index-pack`, which refuses anything after the pack's checksum. Verified with
git 2.55: a pack with twelve bytes appended fails `git index-pack --stdin` and
`git index-pack <file>` alike with "fatal: pack has junk at the end". So the
pack stays an object of its own, byte for byte what git reads, and everything
else about it goes in one **sidecar**: git's `.idx`, the membership filter, and
a 44-byte footer (magic, the two lengths, the pack's checksum). A write is two
uploads and a swap, where it was three and a swap; the manifest records the
lengths so that a reader addresses the index and the filter as ranges of the
sidecar without reading it first, and checks the footer — and the index's own
pack checksum — against the pack's name when it loads the index, so a sidecar
that is not the pack's is refused rather than trusted.

**Nothing is written loose.** An object written one at a time is pending: held
by the repository's handle, readable through it at once, and packed with the
next reference commit of the repository — in the same swap, so objects and the
reference naming them appear together — or on an explicit flush, or once 8 MiB
is pending. The explicit flush is the durability point of a write that moves no
reference, and the server calls it before it names such an object: the
git-database create endpoints, a copy of one repository's objects into another,
a pull request's test merge. The loose tier's listing on a miss, its per-
directory cuckoo filters and its sweep are gone; the only listing left is the
one a compaction takes to find orphans.

**What it cost and bought.** A single-blob API write went from one PUT to two
uploads and a swap, and in exchange every replica finds it by revalidating the
manifest rather than by listing, and nothing is ever deleted from under a
reader by a compaction's loose sweep. A multi-object write (a commit through
the API: blobs, trees, the commit, the ref) went from a PUT per object and a
swap to two uploads and one swap. At git level, a push went from 4 requests to
3 and ten pushes, with the compaction they make due, from 49 to 38; clones and
fetches are unchanged.

**Migration.** Manifest format 2. A format-1 manifest is refused with
`ErrManifestOutdated`, and `bleephub adopt` converts it — writing each live
pack's sidecar from its `.idx` and `.bfilter`, packing the loose objects, and
swapping the manifest on the condition that it is still the one read — as it
converts the layout before the manifest. `adopt -remove-old-layout` then deletes
the separate indexes, filters and loose objects.

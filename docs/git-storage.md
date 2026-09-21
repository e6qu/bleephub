# Git storage

Bleephub serves real git — `git clone`, `git fetch`, `git push` over smart-HTTP
and SSH, plus everything the API and the Actions runner read from a repository —
without shelling out to the `git` binary. It drives git entirely in-process with
[go-git](https://github.com/go-git/go-git), a pure-Go implementation, so the same
Go process that answers the REST/GraphQL API also resolves trees, walks history,
computes merges, and negotiates packs.

## The storage seam

go-git separates git's *plumbing* (objects, references, the pack protocol) from
*where the bytes live* behind one interface, `storage.Storer`: a store of git
objects (blobs, trees, commits, tags) plus references (branches, tags, `HEAD`).
Every git operation bleephub performs runs against a `Storer`; nothing above that
interface knows or cares how the bytes are persisted.

The storage layer is the [`gitstore`](../gitstore/README.md) library, a Go module
of its own in this repository so that it can be imported, benchmarked and scaled
apart from the server. The library takes its configuration as options and never
reads the environment; `internal/gitbackend` is the one place bleephub's
`BLEEPHUB_*` storage settings are parsed and handed to it.

Bleephub builds three kinds of `Storer`, chosen at startup, all behind that one
interface (`internal/gitbackend`, `OpenOrInitGitStorage`):

| Backend | When | Built from |
|---|---|---|
| In-memory | default (no config) | `gitstore.OpenMemory` — go-git's `memory.NewStorage()` |
| Local filesystem | `BLEEPHUB_GIT_DIR` set | `gitstore.OpenDir` — go-git's filesystem storage over `<dir>/<owner>/<repo>` |
| Object store | `BLEEPHUB_GIT_BUCKET` set | `gitstore.Store.Repository` — the storage engine (below) |

The first two are go-git's own storage behind a wrapper (`atomicRefStorer`) that
makes one handle safe to share between requests and makes a reference update a
compare-and-swap. The third is not go-git's storage at all.

## The storage engine

go-git's filesystem storage is a client's: written for one process, one
repository and a local disk. Bleephub used to run it over a bucket by handing it
a filesystem whose files were objects, and everything that made that fast was a
way of guessing, from the file calls go-git happened to make, what it was about
to ask for. The engine that replaced it implements `storage.Storer` directly
against the object store (`gitstore/repository.go`). What lands in the bucket is
still git's ordinary layout — `objects/`, `refs/`, `HEAD`, pack files, one
object-store object per git file — so a bucket can be read with `git` itself,
but nothing in between pretends to be a disk.
[`docs/git_storage_research.md`](git_storage_research.md) records what was
measured, what other systems do, and why this is the design that was chosen.

- **Immutable snapshots, swapped whole** (`objectindex.go`, `refs.go`) — there is
  one handle per repository for the life of the process (`Store.Repository`
  memoises it), shared by every request. What it knows of the repository — the
  live packs, the loose tier, the references — it holds as immutable snapshots.
  A reader takes a pointer and is done; a writer builds the next snapshot and
  swaps it in. There is nothing half-built for two readers to race on, and so no
  lock on the read path at all.
- **Per-call decoders over shared indexes** (`repository.go`, `packreader.go`) —
  go-git's packfile decoder is not safe to share, so one is made for each call
  that reads a pack. What the calls share is everything expensive: a pack index
  parsed once and only read after, and the extent cache underneath.
- **A shared extent cache** (`packcache.go`, `packreader.go`) — a pack is read
  with HTTP range requests in fixed extents, so resolving one object pulls a
  bounded window rather than a gigabyte pack. A pack's key is the hash of its
  contents, so a fetched extent can never be stale: extents are cached in memory
  and on local disk with no invalidation, survive a restart, and the extent size
  is folded into the cache key so a reconfigured replica never confuses them.
  Concurrent fetches of one extent are one request, so a replica that starts
  cold under load downloads each extent once rather than once per clone.
- **Refresh on a miss** (`objectindex.go`) — a snapshot comes from one recursive
  listing of `objects/`. "Does the repository have this?" — which a fetch
  negotiation mostly asks about objects it does not — is answered from a
  membership filter per pack and per loose directory, without reading an index
  or asking the store; the filters are negative-only, so a "maybe" always goes on
  to the exact lookup. An object missing from a snapshot older than
  `Options.IndexFreshness` is looked for again after a fresh listing before it is
  reported missing, because another replica may have published the pack that
  holds it. Evidence that a snapshot is out of date — it named a loose object
  since packed, or a pack since retired — lists again at once, whatever its age.
- **Streamed large objects** (`streamedobject.go`) — an object above 8 MiB that
  a pack stores whole is handed out as a stream inflated from the pack as it is
  read, not as bytes in memory: a server reads on behalf of anyone who asks, and
  an object returned in memory costs its size in heap for every reader at once.
  (A large object stored as a delta is still rebuilt in memory, as in every git
  implementation: applying a delta needs its base to hand.)
- **Reference reads** (`refs.go`) — resolving a reference is one GET, and within
  the freshness bound a reference already read — or just written through this
  handle — answers again without a request, so the dozen resolutions a server
  makes in one push are one read. Listing them, which opens every fetch and
  push, is one recursive LIST of `refs/` that carries each reference's version
  token: what was read is kept beside the version it was read at, so a listing
  that shows the same version is the store's own word that the reference has not
  moved. An advertisement is one LIST plus a GET for each reference that has
  moved since this replica last read it, and those GETs run together. A read
  made in order to compare never relies on any of this.
- **One request per object written** (`repository.go`) — git writes an object to
  a temporary name and renames it into place, which on a disk makes it appear
  atomically. A PUT already is atomic, and the key is the hash of the bytes, so
  an object written through the API is one PUT: no probe, no temporary key, no
  copy.
- **Pack ingest** (`ingest.go`) — a push arrives as a packfile and is published
  as one: index, membership filter, then the pack, three writes however many
  objects it carries. A stock git client sends incremental pushes as *thin*
  packs, whose deltas lean on objects the server already has; those cannot be
  stored as they stand, so they are completed — resolved against the repository
  and re-encoded self-contained — before they are published. What lands in the
  bucket is always an ordinary git pack. The pushing handle adds the pack to the
  snapshot it holds: nothing is listed, and nothing just uploaded is read back.
- **Compaction** (`compact.go`) — loose objects (the REST git-database endpoints
  and web edits still write objects one at a time) are rolled into pack files,
  and the small packs pushes leave are merged, the same housekeeping `git gc`
  does. Merging is geometric, as in `git repack --geometric`: a pack is left
  alone while it is at least twice the size of everything smaller than it, so a
  run of small pushes never causes the repository's large packs to be rewritten,
  and the pack count stays logarithmic in the repository's size. A merged-away
  pack is kept for an hour for requests that were already reading it, but is
  hidden from every new reader. The engine asks for a compaction when a write
  leaves the repository due one — more than eight live packs, a push landing
  behind a loose tier worth packing, or the loose-write trigger — and the server
  runs it in the background; the library owns no goroutines.

### What the server asks of a repository beside `Storer`

A repository handle says what else it can do through two interfaces, and the
server asks with a plain type assertion on the storer it already holds:

- **`gitstore.PackSource`** (`packsource.go`) — the stored packs, their parsed
  indexes and their bytes. A clone of a packed repository is answered by copying
  the stored packs' entry regions onto the wire rather than encoding the same
  objects again (`internal/server/git_packreuse.go`). The object-store engine
  answers from the snapshot and the indexes it already holds, so choosing the
  packs lists and reads nothing a previous request has not paid for, and the
  bytes come through the extent cache. `OpenDir` is a `PackSource` too, over the
  pack directory on disk. Memory-backed storage has no packs and is not one; its
  fetches take the encoder path.
- **`gitstore.Addressable`** (`presign.go`) — presigned URLs for stored packs and
  for auxiliary objects kept beside the git data, which is how packfile-uris and
  bundle-uri hand a client an address to fetch from the bucket directly, without
  bleephub proxying the bytes or lending its credentials. Only the object store
  has such addresses, so only there are the two features advertised.

## The object store interface

The engine reaches the bucket only through `objstore.Bucket`
(`gitstore/objstore`): the operations S3, Google Cloud Storage, Azure Blob
Storage and the S3-compatible stores all offer *with the same meaning* — atomic
PUT, read-after-write, list-after-write, ranged reads, conditional writes,
server-side copy, presigned GET — and nothing else. The S3 wire protocol is not
that common interface: Google Cloud Storage's S3-compatible endpoint answers a
conditional PUT with 200 and overwrites, and Azure has no S3 endpoint at all.
Each store gets a driver that speaks its native API; the S3 driver is built on
the vendor-neutral [minio-go](https://github.com/minio/minio-go) client and
serves AWS S3, MinIO, R2, SeaweedFS, Ceph and the like. Every driver implements
every operation with its whole meaning: there is no capability to ask about and
no lesser behaviour to settle for.

Which is why bleephub **probes the store at startup and refuses to start on one
that fails**. `objstore.Conform` writes one key under `<prefix>/.conformance/`,
proves against the live bucket that a create-if-absent of an existing object is
refused, that a replace-if-unchanged against a stale version is refused, that
neither refused write changed anything, that a ranged read, a listing after a
write, a presigned URL and a delete behave, and removes the key.
`gitbackend.GetStore` runs it when the process-wide store is first opened, and
`Server.ListenAndServe` returns its error. A store that accepts the requests and
does not honour them would otherwise run for weeks, until two replicas were each
told they had moved a branch; there is nothing to fall back to, so the answer to
a failed probe is not to serve. The service byte store
(`BLEEPHUB_OBJECT_BUCKET`) is held to the same probe.

Which object store that is, the operator states: `BLEEPHUB_OBJECT_STORE` is
`s3`, `gcs` or `azure`, and `internal/gitbackend` holds the one switch in the
server that turns the name into a driver (`openBucket`). Above it everything
holds an `objstore.Bucket`; both the git store and the byte store are
`gitstore.Open` over a bucket it built. Nothing is detected or defaulted — not
the driver, not a prefix, not a region — and a setting of a driver that was not
chosen is an error; the README's **Object store** section lists the variables.

## Concurrency

Reads need no coordination on the object store: they are pointer loads of
immutable snapshots. On the directory and memory backends, where the storage
underneath is go-git's own maps, `atomicRefStorer` keeps readers and writers
apart with a read-write lock.

References are the one part that needs arbitration: two pushes must not both
advance a branch from the same tip. On every backend a reference update is a
compare-and-swap — a reference moves only if it still holds the value the writer
observed — made under that reference's lock, and on the object store the
comparison always reads the store, never anything remembered. So concurrent
pushes and the merge queue's ref writes stay safe on all three backends.

## Consistency, durability, and resilience

Bleephub keeps **metadata** (issues, PRs, refs-as-rows, package/artifact records) in
SQLite/dqlite and **bytes** (git objects, artifacts, logs, packages, LFS, CodeQL
databases) in the object store. The two are reconciled by a strict ordering:

- **Bytes first, metadata second.** Every write path uploads the object, then
  commits the metadata row that points at it — so a metadata row never references
  bytes that are not already durable. A crash between the two can only orphan an
  object (storage waste), never dangle a pointer at missing bytes. Read paths that
  do hit a missing object surface an explicit error, not a silent success, and a
  stored SHA-256 is verified on read — buffered reads reject a mismatch, and
  streamed reads recompute the digest and fail the final read rather than serving
  corruption silently. The digest is kept beside the object, as object metadata,
  under a name every store keeps as written (`objstore.Metadata`): a stored
  object is exactly its content, so it can be handed to a client by URL; and the
  store's own version token is not a content hash that can be trusted. An object
  with no digest beside it is not one the byte store wrote, and reading it is an
  error. The startup probe proves the store keeps metadata, as it proves the rest.
- **The durability barrier gates metadata, not bytes.** Group commit's HTTP
  durability barrier withholds a mutating response until the metadata is fsynced;
  byte-transfer routes are exempt (they carry their own protocol). Because bytes
  are written first, an acknowledged write's bytes are already durable when its
  metadata becomes durable.
- **Orphan reclamation.** Objects a crash orphaned, or content-addressed blobs
  (LFS, container-registry) left behind when their last referrer is deleted, are
  reclaimed by the object reaper — a mark-and-sweep that only removes keys with no
  live metadata reference and only after a grace window, so an in-flight
  byte-first upload is never swept. It is opt-in and reports before it deletes.
- **Upload memory is bounded, not streamed everywhere.** Genuinely large or
  unbounded transfers (git packs, LFS objects, migration archives, package
  downloads) stream without ever residing whole in the heap. The remaining
  buffered paths that must hold the content to process it — CodeQL bundle
  validation, release-asset digesting, chunked artifact assembly — are each capped
  (2 GiB uploads, 10 GiB artifact assembly) rather than unbounded.

## Multi-replica coordination

The object store has no advisory locking and multiple bleephub replicas may share
one bucket, so cross-replica correctness borrows the durable metadata store that
already serializes shared state:

- **Reference CAS across replicas** — every ref mutation takes a durable
  `GitObjectLocker` lock (a single TTL'd upsert in the SQLite/dqlite `locks`
  table) in addition to the process-local lock, so two replicas cannot both
  advance a branch from the same tip. A replica that dies holding a lock frees it
  when the TTL expires.
- **Snapshot freshness** — a repository's snapshot answers "absent" only while it
  is no older than `BLEEPHUB_GITSTORE_INDEX_FRESHNESS` (the library's
  `Options.IndexFreshness`); past that a miss lists again before it is believed,
  which is how one replica comes to see the pack another has just published. It
  relies on the store's list-after-write consistency, which the startup probe
  checks.
- **Compaction is single-writer per repo** — it runs under a durable
  `git-compact:` lock and publishes the `.pack` last (its commit point), deleting
  loose objects only after the pack that holds them is visible, so a concurrent
  reader on another replica never sees an object in neither tier.
- **Outage resilience** — every object-store call goes through one chokepoint
  (`storeShared.call`): it derives its timeout from the server-lifetime context
  (cancelled on shutdown) and passes through a circuit breaker: after a run
  of hard failures the breaker fast-fails for a short cooldown so a dead store
  returns in microseconds rather than every goroutine blocking the full timeout
  while holding a repo lock. The breaker's open error is transient, never
  "object absent", so go-git can't mistake an outage for a deleted ref.

## Why it is arranged this way

Because everything sits on `storage.Storer`, the durable-storage choice is a
deployment detail, not a code path: smart-HTTP git, SSH git, the API's tree and
blame reads, the workflow engine's checkout, and the merge queue all run the same
code whether a repository lives in memory for a test, on a disk for local
development, or in an object store for a durable multi-node deployment. See the
**Persistence** options in the [README](../README.md#configuration) for the
environment variables that select a backend.

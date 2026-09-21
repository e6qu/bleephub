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
compare-and-swap within the process. The third is not go-git's storage at all.

## The storage engine

go-git's filesystem storage is a client's: written for one process, one
repository and a local disk. Bleephub used to run it over a bucket by handing it
a filesystem whose files were objects, and everything that made that fast was a
way of guessing, from the file calls go-git happened to make, what it was about
to ask for. The engine that replaced it implements `storage.Storer` directly
against the object store (`gitstore/repository.go`), and nothing in between
pretends to be a disk.
[`docs/git_storage_research.md`](git_storage_research.md) records what was
measured, what other systems do, and why this is the design that was chosen.

### What is in the bucket

```
<prefix>/<owner>/<repo>/
  manifest                          the only commit point (JSON)
  objects/pack/pack-<sha>.pack      ordinary, self-contained git packs
  objects/pack/pack-<sha>.idx       git's own pack index
  objects/pack/pack-<sha>.bfilter   the pack's membership filter
  objects/<xx>/<38 hex>             loose objects, as git writes them
  objects/refs/<sequence>-<nonce>   reference snapshots the manifests have named
  config, shallow, index            git's small files, where a caller wrote them
  objects/bundle/…, and the like    auxiliary objects the server keeps beside them
  modules/<name>/…                  a submodule: a repository of its own, laid out the same
```

Packs, indexes and loose objects are git's own files under git's own names, so a
bucket's bulk can be read with `git` itself. What says which of them are the
repository is the **manifest** (`manifest.go`):

```json
{
  "format": 1,
  "sequence": 42,
  "packs": [{"name": "pack-<sha>", "bytes": 950611, "index_bytes": 45676, "filter_bytes": 2072,
             "objects": 1593, "source": "push", "added": "2026-09-21T10:00:00Z"}],
  "retired": [{"name": "pack-<sha>", "retired": "2026-09-21T09:30:00Z", "orphan": false}],
  "refs": {
    "snapshot": "objects/refs/0000000000000029-9f2c4e6a1b3d5f70",
    "changes": [{"name": "HEAD", "value": "ref: refs/heads/main"},
                {"name": "refs/heads/main", "value": "<40 hex>"},
                {"name": "refs/heads/gone", "deleted": true}]
  }
}
```

- `format` is the only format the engine reads; any other number is an error,
  for reads and writes alike. There is no migration code in the engine (see
  *Upgrading a store written before the manifest*, below).
- `sequence` increases by one with every successful swap.
- `packs` are the live packs, with the sizes that let a reader address a pack,
  its index and its filter by extent without asking the store about them, the
  object count, and what wrote it: `push`, `write` (an import or the API, through
  `PackfileWriter`), `compaction` or `adoption`.
- `retired` are packs no new reader adopts. A compaction's merged-away packs are
  kept an hour — longer than any request runs — for the requests that were
  already reading them. An `orphan` is an upload no manifest ever named (a
  refused push, a crash between upload and swap), entered here by a sweep a
  grace period before it is deleted.
- `refs` is every reference — branches, tags, `HEAD`, anything else — as a
  pointer to an immutable **snapshot** object plus the changes since it, each a
  value or a tombstone. When the change list passes 512 entries a commit folds
  it into a new snapshot, written *before* the swap that names it under a key
  nothing has used, so the manifest stays under about 128 KiB however many
  references the repository has. An empty repository has no snapshot. References
  are not objects of their own: the engine neither reads nor writes `refs/**`,
  `HEAD` or `packed-refs`.

A change to a repository is visible when, and only when, a conditional write of
the manifest succeeds. Nothing is discovered by listing except the loose tier,
and nothing is arbitrated by a lock.

### How it is read and written

- **Immutable state, swapped whole** (`state.go`, `objectindex.go`) — there is
  one handle per repository for the life of the process (`Store.Repository`
  memoises it), shared by every request. What it knows of the repository — the
  manifest and the snapshot it names, the live packs, the membership of the
  loose tier — it holds as immutable values. A reader takes a pointer and is
  done; whatever learns of a newer manifest builds the next state and swaps it
  in. There is nothing half-built for two readers to race on, and so no lock on
  the read path at all.
- **Revalidation, not re-reading** (`state.go`) — a manifest held answers reads
  of references for `Options.IndexFreshness` (250 ms) after the store last
  vouched for it. Past that a reader asks again with a *conditional read*
  (`objstore.Bucket.GetIfChanged`): one small request that the store usually
  answers "not modified", with no body. `IndexFreshness < 0` revalidates on
  every read of a reference and every miss. A write never reads first.
- **Commits are compare-and-swap** (`commit.go`) — a change is a mutation: a
  function from a manifest to a manifest that may refuse (a reference is not at
  the value the caller expected → `storage.ErrReferenceHasChanged`; a pack to
  retire is not live). Committing applies it to the manifest held and writes the
  result with `objstore.IfVersion` (`IfAbsent` for a repository's first). On 412
  the manifest is read again, the mutation re-applied, and the write retried,
  with a jittered backoff, sixteen times at most. The 412 is the verification:
  nothing is read before a write. A refusal is believed only of a manifest the
  store has vouched for since the commit began.
- **Group commit** — a single object takes about one conditional overwrite a
  second on Google Cloud Storage, so commits to one repository that arrive while
  a swap is in flight share the next one. Each mutation of a group is validated
  on its own, so one caller's refusal fails only that caller. There is no
  background goroutine: the caller that finds no swap in flight leads, and hands
  the lead to a waiter if more arrived meanwhile.
- **A push is one transaction** (`push.go`, `internal/server/git_receivepack.go`)
  — `PushTransactor.BeginPush` gives the server a view of the repository that
  also reads the pushed pack, which is uploaded but named by no manifest. The
  server decides every reference update through that view — connectivity,
  force-push protection and secret scanning all need the pushed commits — and
  then `Commit` adds the pack *and* applies the updates, each against its
  expected old value, in one swap. So a pushed pack is never visible unless the
  push's reference updates were accepted (git's quarantine rule), an `atomic`
  push is atomic because it is one write, and a refused push leaves only an
  upload for a later sweep. Every other caller — the REST git-database
  endpoints, web edits, the wiki, imports — still uses `PackfileWriter` and
  `SetReference` / `CheckAndSetReference` one call at a time, each its own swap.
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
- **The loose tier, and refresh on a miss** (`objectindex.go`, `repository.go`)
  — loose objects (the API's single-object writes) are not in the manifest: one
  PUT each, no commit. They are discovered by one recursive listing of
  `objects/`, taken only when an object is in none of the packs and the listing
  held is older than the freshness bound — so a repository whose objects are all
  packed is never listed by a read that finds what it is looking for. "Does the
  repository have this?" — which a fetch negotiation mostly asks about objects it
  does not — is answered from a membership filter per pack and per loose
  directory, without reading an index or asking the store; the filters are
  negative-only, so a "maybe" always goes on to the exact lookup. An object that
  is found is returned whatever the age of what found it, since packs never
  change; one that is not is believed absent only of a manifest and a listing
  inside the bound. Evidence that what is held is out of date — a loose key
  since packed, or a pack the manifest names that is gone (404) because another
  replica retired and then deleted it — reads the manifest again and retries,
  once.
- **Streamed large objects** (`streamedobject.go`) — an object above 8 MiB that
  a pack stores whole is handed out as a stream inflated from the pack as it is
  read, not as bytes in memory: a server reads on behalf of anyone who asks, and
  an object returned in memory costs its size in heap for every reader at once.
  (A large object stored as a delta is still rebuilt in memory, as in every git
  implementation: applying a delta needs its base to hand.)
- **Reference reads** (`state.go`) — resolving a reference, and listing them all,
  which opens every fetch and push, are answered from the manifest and snapshot
  held: no request inside the freshness bound, one conditional read past it. A
  cold replica advertises a repository of any number of references for two
  requests, the manifest and the snapshot, where it used to list `refs/` and
  read every reference.
- **One request per object written** (`repository.go`) — git writes an object to
  a temporary name and renames it into place, which on a disk makes it appear
  atomically. A PUT already is atomic, and the key is the hash of the bytes, so
  an object written through the API is one PUT: no probe, no temporary key, no
  copy.
- **Pack ingest** (`ingest.go`, `indexpack.go`) — a push arrives as a packfile
  and is stored as one: index, membership filter and pack, three uploads however
  many objects it carries, and then the commit that names it. The pack is
  indexed as git's `index-pack` indexes one, in two passes, the first while the
  push is still arriving, and held to the checksum it is named by. A stock git
  client sends incremental pushes as *thin* packs, whose deltas lean on objects
  the server already has; those cannot be stored as they stand, so they are
  completed as `index-pack --fix-thin` completes them — the bases they left out
  are read from the repository and appended, the pushed bytes kept. What lands
  in the bucket is always an ordinary git pack. Nothing is listed, and nothing
  just uploaded is read back.
- **Compaction, retirement and the sweep** (`compact.go`) — loose objects are
  rolled into pack files, and the small packs pushes leave are merged, the same
  housekeeping `git gc` does. Merging is geometric, as in
  `git repack --geometric`: a pack is left alone while it is at least twice the
  size of everything smaller than it, so a run of small pushes never causes the
  repository's large packs to be rewritten, and the pack count stays logarithmic
  in the repository's size. A compaction commits by ONE swap that adds the new
  pack and retires the ones it merged; only after it are the loose keys it
  packed deleted. Two compactions racing need no lock: the loser's mutation finds
  the packs it merged no longer live, refuses, and the loser deletes the pack it
  uploaded (unless it is the winner's pack, byte for byte and so name for name).
  The same run sweeps: retired packs are deleted a grace period (an hour) after
  their retirement, keys first and manifest entry after; reference snapshots no
  manifest names are deleted once they have lain there a grace period; and
  uploaded packs no manifest names, as old, are entered in `retired` as orphans
  and deleted a grace period after *that*. The detour matters because pack names
  are digests of their contents — a push that was refused and is made again
  uploads the same name — so an orphan is deleted only once the manifest has
  said, for a whole grace period, that it is going to be; a commit that wants
  the name back in the first half of that period takes it back, and after that
  is refused. Nothing a manifest names, live or retired within grace, is ever
  deleted. The engine asks for a compaction when a write leaves the repository
  due one — more than eight live packs, a push landing behind a loose tier worth
  packing, or the loose-write trigger — and the server runs it in the
  background; the library owns no goroutines.
- **Copy, rename, delete** (`store.go`) — a fork or rename reads the manifest
  first, copies every other key, and writes the manifest last, so the copy names
  only what the listing showed and becomes a repository in one step. A delete
  removes the manifest first.

### What the server asks of a repository beside `Storer`

A repository handle says what else it can do through three interfaces, and the
server asks with a plain type assertion on the storer it already holds:

- **`gitstore.PushTransactor`** (`push.go`) — a push as one transaction, above.
  Only the object store is one; on the other backends a push's objects land
  first and its references after, one by one.
- **`gitstore.PackSource`** (`packsource.go`) — the stored packs, their parsed
  indexes and their bytes. A clone of a packed repository is answered by copying
  the stored packs' entry regions onto the wire rather than encoding the same
  objects again (`internal/server/git_packreuse.go`). The object-store engine
  answers from the manifest and the indexes it already holds, so choosing the
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
neither refused write changed anything, that a conditional read at the current
version is answered "not modified" and at a replaced version with the object,
that a ranged read, a listing after a write, a presigned URL and a delete behave,
and removes the key. `gitbackend.GetStore` runs it when the process-wide store is
first opened, and `Server.ListenAndServe` returns its error. A store that accepts
the requests and does not honour them would otherwise run for weeks, until two
replicas were each told they had moved a branch; there is nothing to fall back
to, so the answer to a failed probe is not to serve. The service byte store
(`BLEEPHUB_OBJECT_BUCKET`) is held to the same probe.

The conditional read is each store's own: `If-None-Match` answered 304 on S3
([GetObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html))
and on Azure
([conditional headers](https://learn.microsoft.com/en-us/rest/api/storageservices/specifying-conditional-headers-for-blob-service-operations));
on Google Cloud Storage, whose version token is a generation and whose download
(the XML API's) takes conditions only on ETags, an `objects.get` of the
object's description — one small request — and a download only when the
generation it reports is not the one held. (The service's own precondition for
the question, `ifGenerationNotMatch` answered 304
([request preconditions](https://docs.cloud.google.com/storage/docs/request-preconditions)),
is not used: see the research notes for why.)

Which object store that is, the operator states: `BLEEPHUB_OBJECT_STORE` is
`s3`, `gcs` or `azure`, and `internal/gitbackend` holds the one switch in the
server that turns the name into a driver (`openBucket`). Above it everything
holds an `objstore.Bucket`; both the git store and the byte store are
`gitstore.Open` over a bucket it built. Nothing is detected or defaulted — not
the driver, not a prefix, not a region — and a setting of a driver that was not
chosen is an error; the README's **Object store** section lists the variables.

## Concurrency

Reads need no coordination on the object store: they are pointer loads of
immutable state. On the directory and memory backends, where the storage
underneath is go-git's own maps, `atomicRefStorer` keeps readers and writers
apart with a read-write lock, and makes a reference update a compare-and-swap
under a process-local lock.

On the object store nothing takes a lock, in process or out. Two pushes must not
both advance a branch from the same tip, and what prevents it is the conditional
write of the manifest: a reference update is a mutation that refuses when the
manifest holds another value, and the store lets exactly one of two writers
replace a given version of the manifest. The loser re-reads and re-applies; if
the branch it compares has moved it is refused, and if another branch has, it
commits. So concurrent pushes and the merge queue's ref writes stay safe across
goroutines and replicas alike, with the object store as the only arbiter.

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

Several bleephub replicas may share one bucket, and the bucket is all they need
to share for git:

- **One commit point** — every change to a repository, from any replica, is a
  conditional write of its manifest. There is no lock service and no leader:
  the durable SQL lock the engine used to take around reference updates and
  compactions is gone. A replica that dies mid-change leaves, at worst, uploads
  no manifest names.
- **Freshness** — a replica serves references from the manifest it holds for
  `BLEEPHUB_GITSTORE_INDEX_FRESHNESS` (the library's `Options.IndexFreshness`)
  and then revalidates it with a conditional read, which is how it comes to see
  what another replica pushed; a miss on an object revalidates before it is
  believed. A replica holding a manifest whose pack has since been retired and
  deleted elsewhere gets a 404, reads the manifest again, and retries.
- **Compaction needs no single writer** — see above: the loser of a race refuses
  and cleans up after itself, and loose keys are deleted only after the commit
  of the pack that holds them, so a reader on another replica never sees an
  object in neither tier.
- **Outage resilience** — every object-store call goes through one chokepoint
  (`storeShared.call`): it derives its timeout from the server-lifetime context
  (cancelled on shutdown) and passes through a circuit breaker: after a run
  of hard failures the breaker fast-fails for a short cooldown so a dead store
  returns in microseconds rather than every goroutine blocking for the full
  timeout. The breaker's open error is transient, never "object absent", so go-git
  can't mistake an outage for a deleted ref.

## Upgrading a store written before the manifest

Before the manifest, a repository in the bucket kept every reference as an
object of its own (`refs/**`, `HEAD`, a read-only `packed-refs`) and said which
packs were live by which keys lay beside them (`.superseded` markers). The
engine does not read that layout, and a server started on such a bucket refuses
it: `gitbackend.OpenExistingGitStorage`, which a restart opens every known
repository with, answers `gitstore.ErrNoManifest` rather than take the repository
for an empty one and initialize over it.

`bleephub adopt` is the one-off operation that brings such a store up to date. It
reads the same `BLEEPHUB_*` storage settings as the server and, for every
repository under the git prefix (or the one named with `-repository owner/repo`),
reads the old layout — live packs are those with a `.pack` and an `.idx` and no
`.superseded` marker; superseded ones become `retired` at their marker's time;
references come from `refs/**`, `HEAD` and `packed-refs` with git's precedence —
and writes the manifest with `IfAbsent`. It refuses a repository that already has
a manifest, reports what it did for each, handles submodule repositories, and
leaves every old key in place. `bleephub adopt -remove-old-layout` is the second,
explicit step: it deletes the old reference objects and markers of repositories
that have a manifest. The library functions are `Store.Adopt`,
`Store.RemoveAdoptedLayout` and `Store.Repositories` (`gitstore/adopt.go`).

## Why it is arranged this way

Because everything sits on `storage.Storer`, the durable-storage choice is a
deployment detail, not a code path: smart-HTTP git, SSH git, the API's tree and
blame reads, the workflow engine's checkout, and the merge queue all run the same
code whether a repository lives in memory for a test, on a disk for local
development, or in an object store for a durable multi-node deployment. See the
**Persistence** options in the [README](../README.md#configuration) for the
environment variables that select a backend.

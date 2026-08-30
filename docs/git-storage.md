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

Bleephub builds three kinds of `Storer`, chosen at startup, all behind that one
interface (`internal/gitstore/git_storage.go`, `newGitStorage`):

| Backend | When | Built from |
|---|---|---|
| In-memory | default (no config) | `memory.NewStorage()` |
| Local filesystem | `BLEEPHUB_GIT_DIR` set | `osfs` over `<dir>/<owner>/<repo>` |
| Object store (S3) | `BLEEPHUB_S3_BUCKET` set | the `S3FS` filesystem (below) |

The filesystem and object-store backends share the *same* go-git code:
`filesystem.NewStorage(fs, cache)` turns any **filesystem** into a git store,
reading and writing git's ordinary on-disk layout (`objects/`, `refs/`,
`packed-refs`, pack files) as files. The only thing that changes between "local
disk" and "S3" is which filesystem it is handed.

## The filesystem abstraction

That filesystem is [go-billy](https://github.com/go-git/go-billy), go-git's
filesystem-interface library. `billy.Filesystem` is a small abstraction — open,
read, write, stat, rename, list — that stands in for `os`. go-git's filesystem
storage is written entirely against `billy.Filesystem`, so it runs unmodified on
anything that implements it:

- **Local disk** uses billy's `osfs`, a thin pass-through to the real filesystem.
- **Object storage** uses bleephub's own `S3FS` (`internal/gitstore/s3fs.go`), a
  `billy.Filesystem` whose files are objects in an S3-compatible bucket. Because
  `S3FS` satisfies the same interface, go-git writes git's on-disk layout into
  the bucket exactly as it would to a disk — one object per git file, keyed by
  its path — and no git code above it changes.

Any S3-compatible store works (AWS S3, MinIO, and others); the client is the
vendor-neutral [minio-go](https://github.com/minio/minio-go) S3 client, so there
is no cloud-specific SDK in the tree.

## Making git-over-S3 fast

A naive "each git file is an S3 object" filesystem would be correct but slow —
git reads packs at random offsets and probes for thousands of loose objects. The
`S3FS` layer adds what disk gets for free:

- **Range reads** (`s3rangefile.go`) — pack files are read with HTTP range
  requests, so resolving one object pulls a bounded window instead of the whole
  (potentially gigabyte) pack.
- **Pack cache** (`packcache.go`) — pack files are content-addressed and
  immutable, so their fetched extents are cached locally and reused; the chunk
  size is folded into each cache key so a reconfigured replica never confuses
  extents.
- **Object index** (`objectindex.go`) — the "is this object loose?" probes a
  clone makes are batched into a few bucket listings, relying on S3's
  strongly-consistent list-after-write; a process trusts its own writes
  immediately.
- **Compaction** (`compact.go`) — accumulated loose objects are rolled into pack
  files, the same housekeeping `git gc` does, keeping listings and lookups cheap.
- **Presigned reads** (`presign.go`) — a caller already entitled to a repo's
  bytes can be handed a short-lived presigned URL that fetches one object
  directly from the bucket, without bleephub proxying the bytes or lending its
  credentials.

## Concurrency

References are the one part that needs coordination: two pushes must not both
advance a branch from the same tip. Every `Storer` is wrapped
(`atomicRefStorer`, `WrapAtomicRefStorage`) to make reference updates a
compare-and-swap — a reference moves only if it still holds the value the writer
observed — so concurrent pushes and the merge queue's ref writes stay safe on all
three backends.

## Why it is arranged this way

Because everything sits on `storage.Storer`, the durable-storage choice is a
deployment detail, not a code path: smart-HTTP git, SSH git, the API's tree and
blame reads, the workflow engine's checkout, and the merge queue all run the same
code whether a repository lives in memory for a test, on a disk for local
development, or in an object store for a durable multi-node deployment. See the
**Persistence** options in the [README](../README.md#configuration) for the
environment variables that select a backend.

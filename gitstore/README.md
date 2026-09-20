# gitstore

Git repositories in S3-compatible object storage, as a Go library.

`gitstore` gives [go-git](https://github.com/go-git/go-git) a place to keep a
repository that is a bucket rather than a disk. It implements go-git's
filesystem interface ([go-billy](https://github.com/go-git/go-billy)) over S3, so
go-git's ordinary storage code writes git's ordinary layout — `objects/`,
`refs/`, `packed-refs`, pack files — into the bucket, and everything above
`storage.Storer` (clone, fetch, push, merges, blame) runs unchanged. Then it adds
what a disk gives for free and an object store does not:

| | |
|---|---|
| **Pack ingest** | A pushed packfile is published as a pack — three writes, however many objects — instead of being exploded into one key per object. Thin packs from stock git clients are completed first, so the bucket only ever holds ordinary, self-contained git packs. |
| **Ranged pack reads** | A pack is read through HTTP range requests, so resolving one object pulls a bounded extent, not a gigabyte pack. |
| **Pack cache** | Packs are content-addressed and immutable, so fetched extents are cached on local disk and in memory with no invalidation protocol. |
| **Membership index** | "Is this object loose?" is answered from cuckoo and binary-fuse filters built off bucket listings, so a clone of a packed repository does not pay a 404 per object. Answers are negative-only: a "maybe" always falls through to the real lookup. |
| **Compaction** | Loose objects are rolled into packs and small packs are merged geometrically (as `git repack --geometric` does), published pack-last so a concurrent reader on another replica never sees an object in neither tier. |
| **Reference reads** | git stats a reference file before it opens it; here the stat performs the GET and hands the bytes to the open that follows. Within `IndexFreshness` a branch already read — or just written — answers again without a request, so the dozen resolutions a server makes in one push are one read. A read made in order to write never takes either, so the compare-and-set still compares against the store. |
| **One listing per advertisement** | Listing references walks `refs/` a directory and a file at a time. Here it is one recursive LIST, whose ETags say which references have moved since this replica last read them; only those are fetched, together rather than one after another. An advertisement of a thousand unchanged branches is one request. |
| **A push costs what it uploads** | A pushed pack updates the membership index in place and seeds the pack cache with its own index, and a compaction is requested only when the repository is due one — by the pack count as last listed, so a restart does not reset it — rather than run after every push to find nothing to do. |
| **Safe first use** | go-git builds a handle's pack index lazily, from inside read calls. A handle is primed once under its exclusive lock, so the concurrent first reads a freshly started server gets never see a half-built index. |
| **Atomic references** | Every reference update is a compare-and-swap, serialized in-process and — with a `GitObjectLocker` installed — across replicas sharing the bucket. |
| **Outage behaviour** | S3 calls run under a circuit breaker whose open state is a transient error, never "object absent", so go-git cannot mistake an outage for a deleted ref. |

It was extracted from [bleephub](../README.md), which uses it for every
repository it serves; [`docs/git-storage.md`](../docs/git-storage.md) describes
the design and its consistency argument. It is a module of its own so that it
can be imported, benchmarked and scaled apart from the server — see
[`bench/`](bench/README.md) for how it compares with other designs.

## Use

```go
fs, err := gitstore.NewS3FS(ctx, "http://127.0.0.1:9000", "my-bucket", "git", gitstore.Options{
	CacheDir: "/var/cache/gitstore",
})
if err != nil { /* … */ }

stor, err := gitstore.OpenObjectStore(fs, "octocat/hello-world")
if err != nil { /* … */ }
if err := gitstore.Init(stor); err != nil { /* … */ }

repo, err := git.Open(stor, nil) // go-git from here on
```

An empty endpoint targets AWS S3 in `Options.Region`; anything else is addressed
path-style, which is what MinIO and other S3-compatible stores expect.
Credentials default to the AWS environment chain. `NewS3FSWithClient` accepts a
`minio.Core` you configured yourself.

`OpenDir` and `OpenMemory` return the same reference-atomic wrapper over a local
directory and over process memory, so an application can choose a backend at
startup and run one code path.

### Configuration

The library never reads the process environment. Everything is an
[`Options`](options.go) field, and the zero value selects every default:

| Option | Default | |
|---|---|---|
| `Region`, `Credentials`, `Transport` | `us-east-1`, AWS env chain, client default | The connection. A harness wraps `Transport` to count requests. |
| `ChunkBytes` | 4 MiB | Extent size of ranged pack reads. |
| `CacheDir`, `CacheBytes` | under `os.TempDir`, 8 GiB | On-disk pack cache and compaction staging. |
| `MemoryCacheBytes` | 256 MiB | In-memory tier of the pack cache. Negative disables it. |
| `IndexFreshness` | 250ms | How far a read may lag another replica's write: how long a membership snapshot may answer "absent", and how long a fetched reference or a listing of `refs/` may answer again. Negative re-lists every probe and reads every reference from the store each time. |
| `CompactionTrigger` | 4096 | Loose writes to one repository that request a compaction. A push requests one sooner: when it leaves more than 8 live packs, or lands behind 64 or more loose writes. Negative never requests one. |
| `MultipartBytes` | 64 MiB | Pack size above which compaction publishes by multipart upload. |
| `BreakerThreshold`, `BreakerCooldown` | 5, 5s | Circuit breaker. A negative threshold disables it. |

Where a tunable has a meaningful "off", zero still means "default" and a negative
value means off, so that a zero `Options{}` is always safe.

### Hooks an application installs

- `SetGitObjectLocker` — a durable lock manager (bleephub uses a row in its
  SQL store) that extends reference compare-and-swap and compaction's
  single-writer guarantee across replicas. Without one, the process-local lock
  is the whole lock, which is correct for a single process.
- `SetCompactionRequestHandler` — called when a repository's loose tier crosses
  `CompactionTrigger`. The library owns no goroutines; the application decides
  when and where `CompactRepository` runs.
- `(*S3FS).SetBaseContext` — a server-lifetime context, so in-flight S3 calls
  are cancelled on shutdown.

## Testing against it: `s3fake`

[`s3fake`](s3fake/s3fake.go) is an in-process S3 endpoint that speaks enough of
the REST API for real S3 clients and counts every request and byte by operation.
It can inject per-request latency, fail chosen requests, and run a hook at an
exact point in a sequence — which is how the suite reproduces a crash half way
through a compaction, or another replica's write landing mid-operation. It is
exported so that code outside this module can be measured by the same
instrument.

## Measuring it

```sh
go test -race ./...                        # the suite
go test -run '^$' -bench . -benchmem .     # in-package benchmarks: S3 requests per operation
cd bench && go run . -latency 5ms          # against other Storer implementations
cd bench && go run . -level git            # stock git against the real server and remote helpers
go run ./s3fake/cmd/s3fake -trace          # the counting fake on a fixed port, for any tool
```

The in-package benchmarks report `s3-requests/op` and `bytes/op` beside time,
because request count is the cost this design exists to reduce and it is
invisible at loopback speed. `GITSTORE_BENCH_OBJECTS` and
`GITSTORE_BENCH_LATENCY` size them.

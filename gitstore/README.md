# gitstore

Git repositories in S3-compatible object storage, as a Go library.

`gitstore` gives [go-git](https://github.com/go-git/go-git) a place to keep a
repository that is a bucket rather than a disk. It implements go-git's
`storage.Storer` directly against the object store: what lands in the bucket is
git's ordinary layout — `objects/`, `refs/`, `HEAD`, pack files — one object per
git file, and everything above `storage.Storer` (clone, fetch, push, merges,
blame) runs unchanged. There is no filesystem in between. go-git's own storage
is a client's: written for one process, one repository and a local disk, and not
safe for concurrent reads of a handle nobody has read yet. This one is a
server's:

| | |
|---|---|
| **Shared handles, no locks** | One handle per repository serves every request for the life of the process. What it knows of the repository — the live packs, the loose tier, the references — it holds as immutable snapshots swapped whole, so a reader takes a pointer and there is nothing half-built for two readers to race on. go-git's packfile decoder, which cannot be shared, is made afresh for each call over a pack index that is parsed once and only read after. |
| **Refresh on a miss** | An object missing from a snapshot older than `IndexFreshness` is looked for again after a new listing before it is reported missing, because another replica may have published the pack that holds it. Finding that the store no longer holds something the snapshot named — a loose object since packed, a pack since retired — lists again at once. |
| **Pack ingest** | A pushed packfile is published as a pack — three writes, however many objects — instead of being exploded into one key per object. Thin packs from stock git clients are completed first, so the bucket only ever holds ordinary, self-contained git packs. The pushing handle adds the pack to the snapshot it holds: nothing is listed, and nothing just uploaded is read back. |
| **Ranged pack reads** | A pack is read through HTTP range requests, so resolving one object pulls a bounded extent, not a gigabyte pack. Concurrent fetches of one extent are one request. |
| **Pack cache** | Packs are content-addressed and immutable, so fetched extents are cached on local disk and in memory with no invalidation protocol. |
| **Membership index** | A snapshot comes from one recursive listing of `objects/`. "Does the repository have this?" — which a fetch negotiation mostly asks about objects it does not — is answered from each pack's binary-fuse filter and a cuckoo filter per loose directory, without reading a pack index or asking the store. Answers are negative-only: a "maybe" always goes on to the exact lookup. A pack's index is read when a read first needs it. |
| **Compaction** | Loose objects are rolled into packs and small packs are merged geometrically (as `git repack --geometric` does), published pack-last so a concurrent reader on another replica never sees an object in neither tier. A merged-away pack is kept for an hour for whoever was reading it, and hidden from every new reader. |
| **Reference reads** | Resolving a reference is one GET. Within `IndexFreshness` a branch already read — or just written — answers again without a request, so the dozen resolutions a server makes in one push are one read. A read made in order to compare never relies on any of it: the compare-and-set compares against the store. |
| **One listing per advertisement** | Listing references is one recursive LIST, whose version tokens say which references have moved since this replica last read them; only those are fetched, together rather than one after another. An advertisement of a thousand unchanged branches is one request. |
| **One request per object written** | An object written through the API is one PUT: no probe, no temporary key, no copy. |
| **Atomic references** | Every reference update is a compare-and-swap, serialized in-process and — with a `GitObjectLocker` installed — across replicas sharing the bucket. |
| **Outage behaviour** | Every store call runs under a timeout derived from the server's context and a circuit breaker whose open state is a transient error, never "not found", so an outage cannot be mistaken for a deleted ref. |

The object store is reached only through [`objstore.Bucket`](objstore/objstore.go):
the operations S3, Google Cloud Storage, Azure Blob Storage and the
S3-compatible stores all offer with the same meaning, with a driver per store
and a conformance probe (`objstore.Conform`) to run at startup.

It was extracted from [bleephub](../README.md), which uses it for every
repository it serves; [`docs/git-storage.md`](../docs/git-storage.md) describes
the design and its consistency argument. It is a module of its own so that it
can be imported, benchmarked and scaled apart from the server — see
[`bench/`](bench/README.md) for how it compares with other designs.

## Use

```go
store, err := gitstore.OpenS3(ctx, "http://127.0.0.1:9000", "my-bucket", "git", gitstore.Options{
	CacheDir: "/var/cache/gitstore",
})
if err != nil { /* … */ }

stor, err := store.Repository("octocat/hello-world")
if err != nil { /* … */ }
if err := gitstore.Init(stor); err != nil { /* … */ }

repo, err := git.Open(stor, nil) // go-git from here on
```

An empty endpoint targets AWS S3 in `Options.Region`; anything else is addressed
path-style, which is what MinIO and other S3-compatible stores expect.
Credentials default to the AWS environment chain. `Open` accepts an
`objstore.Bucket` you built yourself, for another driver or a client of your own.

`Store.Repository` returns the same handle for a repository each time, safe for
concurrent use; keep it, or ask again. `CopyRepository`, `RenameRepository` and
`DeleteRepository` move and remove whole repositories, and `Sub` opens a sibling
prefix on the same connection for an application's other bytes.

`OpenDir` and `OpenMemory` return go-git's own storage over a local directory
and over process memory, behind a wrapper that makes it safe to share and its
reference updates atomic, so an application can choose a backend at startup and
run one code path.

Beside `storage.Storer`, a repository handle offers `PackSource` — its stored
packs, their indexes and their bytes, for a server that answers a clone by
copying a pack out (the object store and `OpenDir`) — and `Addressable` —
presigned URLs for packs and for auxiliary objects such as bundles (the object
store only).

### Drivers

A driver is what stands between `objstore.Bucket` and one store's native API.
Each implements every operation with its whole meaning — there is no capability
to ask about — and is admitted by passing
[`objstoretest.Run`](objstore/objstoretest/suite.go), the same suite for all of
them. Which one a deployment uses is its operator's to say; nothing infers it
from an endpoint.

| Driver | For | Needs |
|---|---|---|
| `objstore.NewS3` (what `OpenS3` builds) | S3 and the stores that honour its conditional writes: R2, MinIO, SeaweedFS, Ceph, Tigris. Not Google Cloud Storage's S3-compatible endpoint, which accepts a conditional PUT and ignores the condition. | A bucket that exists. Endpoint, region and credentials have the defaults above; `PartBytes` (16 MiB) is the size above which an upload goes in parts. |
| [`azure.New`](objstore/azure/azure.go) | Azure Blob Storage, which has no S3 endpoint, through its own API. | A container that exists — a missing one is reported as itself, never as an object not found — and, with no defaults, `Endpoint` (`https://<account>.blob.core.windows.net`, or Azurite's `http://127.0.0.1:10000/devstoreaccount1`), `AccountName` and `AccountKey`. The shared key is the one way in, because it is also what signs a URL without asking the service. `BlockBytes` (16 MiB) is the size above which an upload goes in blocks. |
| [`gcs.New`](objstore/gcs/gcs.go) | Google Cloud Storage, through its JSON API — never through its S3-compatible endpoint, for the reason above. The client is [`gcsclient`](../gcsclient), this repository's own: Google's brings gRPC, xDS and OpenTelemetry with it, some hundreds of packages, for nine operations. | A bucket that exists — a missing one is reported as itself, never as an object not found — and, with no defaults, `Endpoint` (`https://storage.googleapis.com`, or an emulator's URL) and `CredentialsJSON`, a service-account key file's bytes. The key is the one way in, because it is the one Google credential that can sign a URL without asking the service: workload identity, the metadata server and Application Default Credentials are not looked for. `ChunkBytes` (16 MiB, a multiple of 256 KiB) is the size above which an upload goes in chunks. |

```go
bucket, err := azure.New("repositories", azure.Options{
	Endpoint:    "https://myaccount.blob.core.windows.net",
	AccountName: "myaccount",
	AccountKey:  accountKey,
})
if err != nil { /* … */ }
if err := objstore.Conform(ctx, bucket, "git/"); err != nil { /* do not start */ }
```

```go
key, err := os.ReadFile("/run/secrets/gcs-service-account.json")
if err != nil { /* … */ }
bucket, err := gcs.New("repositories", gcs.Options{
	Endpoint:        "https://storage.googleapis.com",
	CredentialsJSON: key,
})
if err != nil { /* … */ }
if err := objstore.Conform(ctx, bucket, "git/"); err != nil { /* do not start */ }
```

They differ where the stores do, and only there. On Azure an upload of unknown
size is staged in blocks and committed under the write's condition, and on
Google Cloud Storage a resumable upload carries the precondition it was begun
with to the write that completes it, so on both a streamed write can be
conditional; S3 cannot complete a multipart upload conditionally. Azure copies
asynchronously, and Cloud Storage copies a large object over several calls, so
`Copy` waits for the copy to finish, for as long as its context allows. A
version is an ETag on S3 and Azure and a generation number on Cloud Storage;
Azure's and Cloud Storage's change with every write, even of the same bytes,
and S3's need not — which is why `objstore.Version` promises neither. Cloud
Storage answers 404 alike for a missing object and a missing bucket, so the
first 404 a bucket gives costs its driver one listing to tell which.

### Configuration

The library never reads the process environment. Everything is an
[`Options`](options.go) field, and the zero value selects every default:

| Option | Default | |
|---|---|---|
| `Region`, `Credentials`, `Transport` | `us-east-1`, AWS env chain, client default | The connection. A harness wraps `Transport` to count requests. |
| `ChunkBytes` | 4 MiB | Extent size of ranged pack reads. |
| `CacheDir`, `CacheBytes` | under `os.TempDir`, 8 GiB | On-disk pack cache and compaction staging. |
| `MemoryCacheBytes` | 256 MiB | In-memory tier of the pack cache. Negative disables it. |
| `IndexFreshness` | 250ms | How far a plain read may lag another replica's write: how long a snapshot may answer "absent" before a miss re-lists, and how long a fetched reference or a listing of `refs/` may answer again. Negative re-lists on every miss and reads every reference from the store each time. |
| `CompactionTrigger` | 4096 | Loose writes to one repository that request a compaction. A push requests one sooner: when it leaves more than 8 live packs, or lands behind 64 or more loose writes. Negative never requests one. |
| `MultipartBytes` | 64 MiB | Pack size above which a pack is published by multipart upload, and the size of its parts (`OpenS3`; a bucket handed to `Open` brings its own, and `Options.UploadPieceBytes` tells whoever builds one this value with its default applied). |
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
- `(*Store).SetBaseContext` — a server-lifetime context, so in-flight store
  calls are cancelled on shutdown.

## Testing against it: `s3fake`

[`s3fake`](s3fake/s3fake.go) is an in-process S3 endpoint that speaks enough of
the REST API for real S3 clients and counts every request and byte by operation.
It can inject per-request latency, fail chosen requests, and run a hook at an
exact point in a sequence — which is how the suite reproduces a crash half way
through a compaction, or another replica's write landing mid-operation. It is
exported so that code outside this module can be measured by the same
instrument.

[`azfake`](azfake/azfake.go) is the same for Azure Blob Storage: enough of the
Blob REST API for the official Go client as the Azure driver uses it. It refuses
what the service refuses — a lost condition, a metadata name that is no
identifier, block IDs of differing lengths, a shared access signature that does
not verify, has expired or lacks the permission — and pages its listings. It is
written from the service's documentation, so the driver's suite also runs
against a real endpoint when `OBJSTORE_AZURE_TEST_ENDPOINT`, `_ACCOUNT`, `_KEY`
and `_CONTAINER` are set: the Azurite emulator, or a storage account.

[`gcsfake`](../gcsclient/gcsfake/gcsfake.go) is the same for Google Cloud
Storage, and lives with the client it is the fake of, in the
[`gcsclient`](../gcsclient) module. It issues access tokens only for an
assertion the service account signed, and refuses a request without one, a lost
precondition, a chunk out of its place or not a multiple of 256 KiB, a batch of
more than a hundred calls, a signed URL that does not verify, and any parameter
it does not implement. The Cloud Storage driver's suite runs against an emulator
as well when `OBJSTORE_GCS_TEST_ENDPOINT` and `_BUCKET` are set. That is
fake-gcs-server, which checks neither credentials nor signatures — so the test
brings a throwaway key and a token endpoint of its own, and `gcsfake` remains
the only place short of the service where a signed URL is verified.

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

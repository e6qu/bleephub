# gitstore

Git repositories in S3-compatible object storage, as a Go library.

`gitstore` gives [go-git](https://github.com/go-git/go-git) a place to keep a
repository that is a bucket rather than a disk. It implements go-git's
`storage.Storer` directly against the object store, and everything above
`storage.Storer` (clone, fetch, push, merges, blame) runs unchanged. There is no
filesystem in between. go-git's own storage is a client's: written for one
process, one repository and a local disk, and not safe for concurrent reads of a
handle nobody has read yet. This one is a server's.

A repository is `<prefix>/<owner>/<repo>/`: ordinary, self-contained git packs
(`objects/pack/pack-<sha>.pack`), each beside one **sidecar**
(`pack-<sha>.sidecar`) holding git's own index for it, its membership filter
and a self-describing footer; and one small JSON object, **`manifest`**, that is
the repository's only commit point. Every object is in a pack: nothing is
written loose. The manifest names the live packs (with their
sizes and object counts), the packs a compaction retired and when, and every
reference — as a pointer to an immutable snapshot object
(`objects/refs/<sequence>-<nonce>`) plus the changes since it, folded into a new
snapshot past 512 changes, so the manifest stays small however many references
there are. A change to a repository is visible when, and only when, a
conditional write of the manifest succeeds. References are not objects of their
own, nothing a reader needs is discovered by listing, and nothing is
arbitrated by a lock. [`docs/git-storage.md`](../docs/git-storage.md) has the
format and the argument.

| | |
|---|---|
| **One commit point, by compare-and-swap** | A change is a function from a manifest to a manifest that may refuse. It is applied to the manifest held and written with `IfVersion` (`IfAbsent` for the first); on 412 the manifest is re-read, the change re-applied, and the write retried with jittered backoff, sixteen times at most. The 412 is the verification: nothing is read before a write. Goroutines and replicas are arbitrated alike, by the store and nothing else. |
| **Group commit** | One object takes about one conditional overwrite a second on Google Cloud Storage, so commits to one repository that arrive while a swap is in flight share the next one. Each is validated on its own: one caller's refusal fails only that caller. No goroutine is started; the caller that finds no swap in flight leads. |
| **A push is one transaction** | `PushTransactor.BeginPush` returns a view that also reads the pushed pack — uploaded, but named by no manifest — so a server can decide a push with its commits to hand; `Commit` then adds the pack and applies the reference updates, each against its expected old value, in one swap. A pushed pack is never visible unless its reference updates were accepted, an `atomic` push is atomic, and a refused push leaves only an upload to be swept. `PackfileWriter`, `SetReference` and `CheckAndSetReference` still work one call at a time, each its own swap. |
| **Copying a repository's objects** | `CopyObjects` — a fork, a merge from a fork — copies the source's packs and sidecars server-side and names them in one swap: nothing decoded, nothing uploaded, a pack already held not copied again. |
| **Everything written is a pack** | An object written through `SetEncodedObject` is pending: readable through the handle at once, and packed with the next reference commit of the repository — in the same swap — or on `FlushObjects`, which an application calls before it names an object no reference points at, or once 8 MiB is pending. A write of one blob is two uploads and a swap; no replica ever lists to find it. |
| **Shared handles, no locks** | One handle per repository serves every request for the life of the process. What it knows of the repository — the manifest, the live packs — it holds as immutable values swapped whole, so a reader takes a pointer and there is nothing half-built for two readers to race on. go-git's packfile decoder, which cannot be shared, is made afresh for each call over a pack index that is parsed once and only read after. |
| **Revalidation** | A manifest held answers reads of references for `IndexFreshness`; past that it is revalidated with a conditional read (`Bucket.GetIfChanged`), one small request usually answered "not modified". A cold advertisement of any number of references is two requests: the manifest and its snapshot. |
| **Refresh on a miss** | An object that is found is returned whatever the age of what found it. One that is not is believed absent only of a manifest inside `IndexFreshness`; otherwise the manifest is revalidated first, because another replica may have pushed the pack that holds it. A pack the manifest names that is gone (404) — retired and deleted elsewhere — reads the manifest again and retries once. Nothing is listed on the read path. |
| **Pack ingest** | A pushed packfile is stored as a pack — two uploads (pack and sidecar) and a commit, however many objects — instead of being exploded into one key per object. Packs are indexed as git's `index-pack` indexes them, the first pass while the push arrives; thin packs from stock git clients are completed as `--fix-thin` completes them, the bases they left out appended and the pushed bytes kept, so the bucket only ever holds ordinary, self-contained git packs. Nothing is listed, and nothing just uploaded is read back. |
| **Ranged pack reads** | A pack is read through HTTP range requests, so resolving one object pulls a bounded extent, not a gigabyte pack. Concurrent fetches of one extent are one request. |
| **Pack cache** | Packs are content-addressed and immutable, so fetched extents are cached on local disk and in memory with no invalidation protocol. |
| **Membership index** | "Does the repository have this?" — which a fetch negotiation mostly asks about objects it does not — is answered from each pack's binary-fuse filter, without reading a pack index or asking the store. Answers are negative-only: a "maybe" always goes on to the exact lookup. A pack's index is read when a read first needs it. |
| **Compaction, retirement, sweep** | Small packs are merged geometrically (as `git repack --geometric` does), committed by one swap that adds the new pack and retires the merged ones. Two compactions racing need no lock: the loser's change refuses and it deletes what it uploaded. A retired pack is deleted an hour after its retirement. Uploads and reference snapshots no manifest names are swept once they have lain there as long — packs by way of the retired list, because a pack's name is its content's digest and may be uploaded again. Nothing a manifest names is ever deleted. |
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
concurrent use; keep it, or ask again. A repository the store holds no manifest
for reads as empty, and its first commit — `Init`'s — creates it;
`Store.ExistingRepository` is for a repository the application already knows of,
and answers `ErrNoManifest` instead. `CopyRepository`, `RenameRepository` and
`DeleteRepository` move and remove whole repositories, manifest and snapshot
included, and `Sub` opens a sibling prefix on the same connection for an
application's other bytes.

`Store.Adopt` is for a store written before the manifest existed, when
references were objects under `refs/` and live packs were told from superseded
ones by marker objects. The engine does not read that layout; `Adopt` reads it
once and writes the manifest it implied (`IfAbsent` — a repository that has one
is refused), and `Store.RemoveAdoptedLayout` afterwards deletes the old keys.
bleephub exposes both as `bleephub adopt`.

`OpenDir` and `OpenMemory` return go-git's own storage over a local directory
and over process memory, behind a wrapper that makes it safe to share and its
reference updates atomic within the process, so an application can choose a
backend at startup and run one code path.

Beside `storage.Storer`, a repository handle offers `PushTransactor` — a push as
one commit (the object store only) — `PackSource` — its stored packs, their indexes and their bytes, for a server that answers a clone by
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

They differ where the stores do, and only there. The conditional read is
`If-None-Match`, answered 304, on S3 and Azure; on Cloud Storage, whose download
takes conditions only on ETags and whose version is a generation, it is an
`objects.get` of the object's description and a download only when the
generation it reports has moved. On Azure an upload of unknown
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
| `IndexFreshness` | 250ms | How far a read may lag another replica's write: how long the manifest held may answer reads of references, and "absent" for an object, before the store is asked again. Negative revalidates on every read of a reference and every miss. A write never relies on it. |
| `CompactAfterPacks` | 8 | Live packs above which a push, a flush or a reference commit carrying written objects requests a compaction. Negative never requests one. What a compaction merges is decided by the packs' sizes alone. |
| `MultipartBytes` | 64 MiB | Pack size above which a pack is published by multipart upload, and the size of its parts (`OpenS3`; a bucket handed to `Open` brings its own, and `Options.UploadPieceBytes` tells whoever builds one this value with its default applied). |
| `BreakerThreshold`, `BreakerCooldown` | 5, 5s | Circuit breaker. A negative threshold disables it. |

Where a tunable has a meaningful "off", zero still means "default" and a negative
value means off, so that a zero `Options{}` is always safe.

### Hooks an application installs

- `SetCompactionRequestHandler` — called when a write leaves a repository more
  than `CompactAfterPacks` live packs. The library owns no goroutines; the
  application decides when and where `CompactRepository` runs.
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

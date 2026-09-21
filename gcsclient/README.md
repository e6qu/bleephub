# gcsclient

A small Go client for Google Cloud Storage: the object operations of its JSON
API, and V4 signed URLs, over plain HTTP. It depends on the standard library and
`golang.org/x/oauth2`, and on nothing else.

It exists because Google's own Go client brings gRPC, xDS and OpenTelemetry with
it — 455 packages and some 35 MiB in the program that prompted this one — and a
program that reads, writes, lists, copies and deletes objects needs none of
them. Beside the standard library this client links four packages, all of
`golang.org/x/oauth2`: the OAuth2 transport and the JWT flow of a service
account. It knows nothing about what is kept in the objects, and nothing about
the rest of this repository: it is a library of its own that happens to live
here, as [`gitstore`](../gitstore) does, and gitstore's Cloud Storage driver is
its first user.

## What it does

| | |
|---|---|
| `Get`, `GetRange` | Read an object, or a range of one. Both report the object's generation, modification time, metadata and whole size with the bytes, in one request. A range that starts at or past the end is `ErrRangeNotSatisfiable`. |
| `Stat` | Describe an object without reading it. |
| `Insert` | Write an object, with metadata and a precondition. A body of known size that fits a chunk is one `multipart` request; anything else — larger, or of unknown size — is a resumable upload, streamed a chunk at a time with one chunk in memory. An upload that fails is cancelled, and leaves no object. A body that is not the size it was said to be is not written. |
| `Delete`, `DeleteBatch` | Delete one object; or many, a hundred to a request, through the batch endpoint, with the outcome of each call read and matched to its name. |
| `List` | Everything under a prefix, through every page, optionally folded by a delimiter — with the folded prefixes merged back among the objects in byte order, which the API sends as two lists. |
| `Copy` | `objects.rewrite`, followed until the service says it is done, so an object of any size is copied without its bytes passing through the caller. |
| `SignedGetURL` | A V4 signed URL (`GOOG4-RSA-SHA256`, path-style), signed locally with the service account's key. Nothing is sent. |

A precondition is `DoesNotExist()` or `GenerationMatch(n)` — `ifGenerationMatch`,
whose zero means absent. An object's **generation** is its version: every write
gives the object a new one, even a write of the same bytes. Refusals are a
`*gcsclient.Error` carrying the service's status, reason and message, and
`errors.Is` matches it to `ErrNotFound` (404), `ErrPreconditionFailed` (412) and
`ErrRangeNotSatisfiable` (416).

```go
client, err := gcsclient.New(gcsclient.Options{
	Endpoint:        "https://storage.googleapis.com",
	CredentialsJSON: keyFile, // a service-account key file's bytes
})
if err != nil { /* … */ }

written, err := client.Insert(ctx, "bucket", "refs/heads/main", body, gcsclient.InsertOptions{
	Size:         size, // or gcsclient.SizeUnknown
	Precondition: gcsclient.DoesNotExist(),
	Metadata:     map[string]string{"sha256": digest},
})
if errors.Is(err, gcsclient.ErrPreconditionFailed) { /* someone else created it */ }

// Compare-and-swap: replace it only if it is still what was written.
_, err = client.Insert(ctx, "bucket", "refs/heads/main", next, gcsclient.InsertOptions{
	Size:         nextSize,
	Precondition: gcsclient.GenerationMatch(written.Generation),
})

signed, err := client.SignedGetURL("bucket", "packs/pack-1.pack", 15*time.Minute)
```

## What it deliberately is not

**It does one thing one way.** There is one way to authenticate; the endpoint is
stated, never assumed; nothing is looked for in the environment; and nothing is
retried, so an error is the service's own answer and not the last of several. A
caller that wants retries knows better than this library which of its operations
are safe to repeat.

**It is not the whole API.** Buckets, ACLs and IAM, object versions other than
the live one, compose, notifications, customer-supplied encryption keys, HMAC
keys, uploads by signed URL: none of them. What is left out is left out
entirely, rather than half-supported.

**It reads an object as it is stored.** A download asks for `gzip`, which is how
the service is told not to decompress an object stored with
`Content-Encoding: gzip` — and not, in doing so, to ignore the range asked for.

## The one way to authenticate, and why

`CredentialsJSON` is a service-account key file, and it is required. The key is
exchanged for OAuth2 access tokens (scope `devstorage.read_write`) at the
`token_uri` the file names, through `golang.org/x/oauth2/jwt`; and the same
private key signs URLs, locally.

Workload identity, the metadata server and Application Default Credentials are
deliberately not supported. None of them holds a private key, so none can sign a
URL without asking the IAM API to sign it for them — a second way of doing one
thing, with its own permissions and its own failures. And a client that went
looking for whichever credential was lying about would authenticate as something
nobody chose. A deployment that will not hold a key file is one this client is
not for.

## One request that is not the JSON API's

Objects are downloaded through the XML API (`GET /bucket/object`), with the same
bearer token. The JSON API's download (`alt=media`) documents no response
headers, so an object's generation, modification time and metadata would each
cost a second request, and a race between the two; the XML API documents them as
headers of the download itself (`x-goog-generation`, `Last-Modified`,
`x-goog-meta-*`). It is also the URL a signed URL addresses. Everything else is
the JSON API.

## Where the documentation is silent

Two things this client relies on are not stated in Google's documentation, and
are marked so where the code relies on them:

- that a precondition given when a resumable upload is **initiated** is applied
  when the upload **completes** — which is what makes a conditional write larger
  than one request possible. `objects.insert` documents `ifGenerationMatch` for
  every upload type and says no more. It is what Google's own client libraries
  depend on, and what the fake-gcs-server emulator does.
- that `ifGenerationMatch=<n>`, for an object that does not exist, is answered
  412 rather than 404.

## Testing against it: `gcsfake`

[`gcsfake`](gcsfake/gcsfake.go) is an in-process Cloud Storage: `New()`,
`URL()`, `CredentialsJSON()`, `CreateBucket()`, and direct accessors for a test
to look behind the API with. It is exported so that code built on this client
can be tested against the same stand-in.

It refuses what the service refuses where a client's correctness turns on the
refusal. Its token endpoint issues a token only for an assertion signed with the
key it gave out, for its own audience; every request needs such a token, for a
scope that can write. A lost precondition is 412, on a multipart upload and at
the completion of a resumable one. A chunk that is not a multiple of 256 KiB, or
does not start where the last one ended, or names another total than the one
declared, is refused; a cancelled upload answers 499. A batch of more than a
hundred calls is refused, and a batch's answers come back in another order than
its calls, since only the `Content-ID` ties them. A V4 signed URL is verified in
full — canonical request, RSA signature, signer, dates — so one that names
another object, has expired, or was signed with another key does not read
anything. Listings page at a thousand, with `items` and `prefixes` apart. A
`fields` selection is honoured, so a client that reads a field it forgot to ask
for finds it missing. And what the fake does not implement — a parameter, a
route — it refuses rather than ignores.

It is written from Google's documentation, not from the service, so it is
evidence that the client speaks the protocol as documented, and no more than
that. gitstore's driver suite also runs over this client against the
fake-gcs-server emulator in CI; the emulator checks neither credentials nor
signatures, so for signed URLs `gcsfake` is the only verifier short of the
service itself.

```sh
go test -race ./...
```

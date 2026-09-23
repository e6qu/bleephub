// Package azure drives Azure Blob Storage through its native API, which is the
// only one it has: Azure offers no S3 endpoint, so the S3 driver cannot reach
// it. A container is the bucket, a block blob is the object, and the blob's
// ETag is its version.
package azure

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"

	"github.com/e6qu/bleephub/gitstore/objstore"
)

// Options configure the driver. Endpoint, AccountName and AccountKey have no
// default: which account a deployment writes to is its operator's to state.
type Options struct {
	// Endpoint is the blob service URL: https://<account>.blob.core.windows.net
	// for the service itself, http://127.0.0.1:10000/devstoreaccount1 for the
	// Azurite emulator.
	Endpoint string
	// AccountName and AccountKey are the storage account's shared key, which
	// authorizes every request and is also what signs a URL locally: no other
	// kind of Azure credential can sign one without asking the service first.
	AccountName string
	AccountKey  string
	// Transport carries every request. Nil selects http.DefaultTransport.
	Transport http.RoundTripper
	// BlockBytes is the block size of an upload too large for one request, which
	// is also the size above which an upload goes in blocks. Zero selects 16 MiB;
	// the service allows no block above 4000 MiB.
	BlockBytes uint64
}

const (
	defaultBlockBytes = 16 << 20
	maximumBlockBytes = 4000 << 20
	// maximumBlocks is the most blocks the service lets one blob be committed
	// from.
	maximumBlocks = 50000
	// deleteBatch is the most sub-requests one Blob Batch request may carry.
	deleteBatch = 256
	// copyPollInterval is how often a copy still in progress is asked after. A
	// copy within one account usually completes before the request that started
	// it returns, so this is paid only by blobs large enough to take a while.
	copyPollInterval = time.Second
)

// bucket is one container of one storage account.
type bucket struct {
	client     *container.Client
	credential *container.SharedKeyCredential
	name       string
	blockBytes uint64
	copyPoll   time.Duration
}

// New opens a container of a storage account, which must already exist: a
// missing container is a deployment's mistake to be told about, not an object
// that is not found.
func New(containerName string, opts Options) (objstore.Bucket, error) {
	if opts.AccountName == "" || opts.AccountKey == "" {
		return nil, errors.New("azure: an account name and an account key are required")
	}
	endpoint, err := url.Parse(opts.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("azure endpoint %q: %w", opts.Endpoint, err)
	}
	if (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return nil, fmt.Errorf("azure endpoint %q: want the blob service URL, https://<account>.blob.core.windows.net", opts.Endpoint)
	}
	credential, err := container.NewSharedKeyCredential(opts.AccountName, opts.AccountKey)
	if err != nil {
		return nil, fmt.Errorf("azure account key: %w", err)
	}
	client, err := container.NewClientWithSharedKeyCredential(endpoint.JoinPath(containerName).String(), credential, &container.ClientOptions{
		ClientOptions: azcore.ClientOptions{Transport: &http.Client{Transport: opts.Transport}},
	})
	if err != nil {
		return nil, fmt.Errorf("azure client: %w", err)
	}
	return NewWithClient(client, credential, containerName, opts.BlockBytes)
}

// NewWithClient opens a container through a client the caller configured, which
// must point at that container and be authorized with the shared key itself: a
// bulk delete signs each of its sub-requests with the client's own credential.
// The credential and the container's name are wanted here as well because a URL
// is signed locally, from the key and the names, and the name is not to be
// guessed back out of the client's URL, whose shape differs between the service
// and an emulator.
func NewWithClient(client *container.Client, credential *container.SharedKeyCredential, containerName string, blockBytes uint64) (objstore.Bucket, error) {
	if containerName == "" {
		return nil, errors.New("azure: no container named")
	}
	if blockBytes > maximumBlockBytes {
		return nil, fmt.Errorf("azure block size %d is above the service's maximum of %d", blockBytes, maximumBlockBytes)
	}
	if blockBytes == 0 {
		blockBytes = defaultBlockBytes
	}
	return &bucket{
		client:     client,
		credential: credential,
		name:       containerName,
		blockBytes: blockBytes,
		copyPoll:   copyPollInterval,
	}, nil
}

func (b *bucket) Name() string { return b.name }

func (b *bucket) Get(ctx context.Context, key string) (io.ReadCloser, objstore.Info, error) {
	response, err := b.client.NewBlobClient(key).DownloadStream(ctx, &blob.DownloadStreamOptions{})
	if err != nil {
		return nil, objstore.Info{}, translate("get", key, err)
	}
	info, err := describe(key, response.ContentLength, response.ETag, response.LastModified, response.Metadata)
	if err != nil {
		_ = response.Body.Close()
		return nil, objstore.Info{}, fmt.Errorf("azure get %s: %w", key, err)
	}
	return response.Body, info, nil
}

// GetIfChanged sends the version held as If-None-Match, which a read operation
// answers with 304 Not Modified when the blob's ETag is still that one.
// https://learn.microsoft.com/en-us/rest/api/storageservices/specifying-conditional-headers-for-blob-service-operations
func (b *bucket) GetIfChanged(ctx context.Context, key string, held objstore.Version) (io.ReadCloser, objstore.Info, error) {
	if held == "" {
		return nil, objstore.Info{}, fmt.Errorf("azure get %s: no version held to compare with", key)
	}
	// A version is an ETag without its quotes, and the header wants them back.
	tag := azcore.ETag(`"` + string(held) + `"`)
	response, err := b.client.NewBlobClient(key).DownloadStream(ctx, &blob.DownloadStreamOptions{
		AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: &tag}},
	})
	if err != nil {
		return nil, objstore.Info{}, translate("get", key, err)
	}
	// The client reports a 304 not as an error but as a response with no body,
	// carrying the code the service sent with it.
	if response.Body == nil {
		return nil, objstore.Info{}, fmt.Errorf("azure get %s: %w", key, objstore.ErrNotModified)
	}
	info, err := describe(key, response.ContentLength, response.ETag, response.LastModified, response.Metadata)
	if err != nil {
		_ = response.Body.Close()
		return nil, objstore.Info{}, fmt.Errorf("azure get %s: %w", key, err)
	}
	return response.Body, info, nil
}

func (b *bucket) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, objstore.Info, error) {
	if offset < 0 || length <= 0 {
		return nil, objstore.Info{}, fmt.Errorf("azure get %s: invalid range %d+%d", key, offset, length)
	}
	response, err := b.client.NewBlobClient(key).DownloadStream(ctx, &blob.DownloadStreamOptions{
		Range: blob.HTTPRange{Offset: offset, Count: length},
	})
	if err != nil {
		return nil, objstore.Info{}, translate("get", key, err)
	}
	// Content-Length is the length of the range; the blob's own is the total in
	// Content-Range.
	info, err := describe(key, totalFromContentRange(response.ContentRange), response.ETag, response.LastModified, response.Metadata)
	if err != nil {
		_ = response.Body.Close()
		return nil, objstore.Info{}, fmt.Errorf("azure get %s: %w", key, err)
	}
	// The interface says a range that starts at or past the end is not
	// satisfiable, and a reader walking a pack to its end depends on being told
	// so. The service says it with 416, translated above. Azurite, the emulator,
	// answers a range that starts exactly at the end with success and no bytes,
	// which CI found; the response carries the blob's whole size, so the rule is
	// held here by that fact rather than by which of them answered.
	if offset >= info.Size {
		_ = response.Body.Close()
		return nil, objstore.Info{}, fmt.Errorf("azure get %s: range starts at %d of %d bytes: %w", key, offset, info.Size, objstore.ErrRangeNotSatisfiable)
	}
	return response.Body, info, nil
}

func (b *bucket) Head(ctx context.Context, key string) (objstore.Info, error) {
	properties, err := b.client.NewBlobClient(key).GetProperties(ctx, &blob.GetPropertiesOptions{})
	if err != nil {
		return objstore.Info{}, translate("head", key, err)
	}
	info, err := describe(key, properties.ContentLength, properties.ETag, properties.LastModified, properties.Metadata)
	if err != nil {
		return objstore.Info{}, fmt.Errorf("azure head %s: %w", key, err)
	}
	return info, nil
}

// Put sends an object of a known size that fits a block as one Put Blob, and
// anything else as staged blocks committed by a Put Block List. Either way the
// object appears whole or not at all: staged blocks are no part of a blob until
// a commit names them, and the service discards those never committed after a
// week.
func (b *bucket) Put(ctx context.Context, key string, body io.Reader, size int64, condition objstore.Condition, metadata objstore.Metadata) (objstore.Version, error) {
	if err := metadata.Validate(); err != nil {
		return "", fmt.Errorf("azure put %s: %w", key, err)
	}
	if size < 0 || uint64(size) > b.blockBytes {
		return b.putBlocks(ctx, key, body, size, condition, metadata)
	}
	// The client wants a body it can rewind to send again, and an io.Reader is
	// not one, so the object is held in memory — which is why one request carries
	// no more than a block, though the service would take far more.
	content := make([]byte, size)
	if _, err := io.ReadFull(body, content); err != nil {
		return "", fmt.Errorf("azure put %s: reading %d bytes: %w", key, size, err)
	}
	uploaded, err := b.client.NewBlockBlobClient(key).Upload(ctx, streaming.NopCloser(bytes.NewReader(content)), &blockblob.UploadOptions{
		Metadata:         wireMetadata(metadata),
		AccessConditions: accessConditions(condition),
	})
	if err != nil {
		return "", translate("put", key, err)
	}
	return writtenVersion(key, uploaded.ETag)
}

// putBlocks stages the body a block at a time, holding one block in memory, and
// commits the list. The condition is applied at the commit, which Azure allows
// and S3 does not: an S3 multipart upload cannot be completed conditionally, so
// there a streamed write is never conditional, and here it may be.
func (b *bucket) putBlocks(ctx context.Context, key string, body io.Reader, size int64, condition objstore.Condition, metadata objstore.Metadata) (objstore.Version, error) {
	// Staged blocks belong to the blob's name, not to an upload, so two writers
	// streaming to one key at once share a namespace of block IDs. Numbering
	// alone would let one writer's commit pick up the other's block 0; a nonce
	// for each upload keeps a commit to the blocks its own writer staged.
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("azure put %s: %w", key, err)
	}
	client := b.client.NewBlockBlobClient(key)
	block := make([]byte, b.blockBytes)
	var blockIDs []string
	var sent int64
	for {
		filled, readErr := io.ReadFull(body, block)
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return "", fmt.Errorf("azure put %s: reading the body after %d bytes: %w", key, sent, readErr)
		}
		if filled > 0 {
			if len(blockIDs) == maximumBlocks {
				return "", fmt.Errorf("azure put %s: more than %d blocks of %d bytes; the service commits no more to one blob", key, maximumBlocks, b.blockBytes)
			}
			// Every block ID of one blob must be the same length, hence the fixed width.
			blockID := base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "%s-%05d", hex.EncodeToString(nonce), len(blockIDs)))
			if _, err := client.StageBlock(ctx, blockID, streaming.NopCloser(bytes.NewReader(block[:filled])), &blockblob.StageBlockOptions{}); err != nil {
				return "", translate("put", key, err)
			}
			blockIDs = append(blockIDs, blockID)
			sent += int64(filled)
		}
		if readErr != nil {
			break
		}
	}
	if size >= 0 && sent != size {
		return "", fmt.Errorf("azure put %s: the body was %d bytes, not the %d it was said to be", key, sent, size)
	}
	committed, err := client.CommitBlockList(ctx, blockIDs, &blockblob.CommitBlockListOptions{
		Metadata:         wireMetadata(metadata),
		AccessConditions: accessConditions(condition),
	})
	if err != nil {
		return "", translate("put", key, err)
	}
	return writtenVersion(key, committed.ETag)
}

func (b *bucket) Delete(ctx context.Context, key string) error {
	_, err := b.client.NewBlobClient(key).Delete(ctx, &blob.DeleteOptions{})
	if err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
		return translate("delete", key, err)
	}
	return nil
}

func (b *bucket) DeleteMany(ctx context.Context, keys []string) error {
	for start := 0; start < len(keys); start += deleteBatch {
		batch, err := b.client.NewBatchBuilder()
		if err != nil {
			return fmt.Errorf("azure delete: %w", err)
		}
		batched := keys[start:min(start+deleteBatch, len(keys))]
		for _, key := range batched {
			if err := batch.Delete(key, &container.BatchDeleteOptions{}); err != nil {
				return fmt.Errorf("azure delete %s: %w", key, err)
			}
		}
		submitted, err := b.client.SubmitBatch(ctx, batch, &container.SubmitBatchOptions{})
		if err != nil {
			return fmt.Errorf("azure delete: %w", err)
		}
		var failures []error
		for _, result := range submitted.Responses {
			if result.Error != nil && !bloberror.HasCode(result.Error, bloberror.BlobNotFound) {
				// A failed part always carries the Content-ID of its request, which
				// names the key more surely than the client's reading of the URL: that
				// takes the account for the container at an emulator's endpoint.
				failures = append(failures, fmt.Errorf("azure delete %s: %w", batched[*result.ContentID], result.Error))
			}
		}
		if len(failures) > 0 {
			return errors.Join(failures...)
		}
	}
	return nil
}

func (b *bucket) List(ctx context.Context, prefix string, visit func(objstore.Entry) error) error {
	// The format is named because the client's own choice of one is "whatever
	// this release prefers", which is not a thing to find out by upgrading.
	pager := b.client.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix:         &prefix,
		ResponseFormat: container.StorageResponseFormatXML,
	})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return translate("list", prefix, err)
		}
		if page.Segment == nil {
			return fmt.Errorf("azure list %s: the listing came without its <Blobs> element", prefix)
		}
		for _, item := range page.Segment.BlobItems {
			entry, err := listed(item)
			if err != nil {
				return fmt.Errorf("azure list %s: %w", prefix, err)
			}
			if err := visit(entry); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *bucket) ListDirectory(ctx context.Context, prefix string, visit func(objstore.Entry) error) error {
	pager := b.client.NewListBlobsHierarchyPager("/", &container.ListBlobsHierarchyOptions{
		Prefix:         &prefix,
		ResponseFormat: container.StorageResponseFormatXML,
	})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return translate("list", prefix, err)
		}
		if page.Segment == nil {
			return fmt.Errorf("azure list %s: the listing came without its <Blobs> element", prefix)
		}
		// The service sends blobs and prefixes as one ordered sequence, and the
		// client sorts them into two; merging by key puts them back in order.
		blobs, prefixes := page.Segment.BlobItems, page.Segment.BlobPrefixes
		for len(blobs) > 0 || len(prefixes) > 0 {
			var entry objstore.Entry
			if len(prefixes) == 0 || (len(blobs) > 0 && deref(blobs[0].Name) < deref(prefixes[0].Name)) {
				if entry, err = listed(blobs[0]); err != nil {
					return fmt.Errorf("azure list %s: %w", prefix, err)
				}
				blobs = blobs[1:]
			} else {
				entry = objstore.Entry{Info: objstore.Info{Key: deref(prefixes[0].Name)}, Prefix: true}
				prefixes = prefixes[1:]
			}
			if err := visit(entry); err != nil {
				return err
			}
		}
	}
	return nil
}

// Copy uses Copy Blob, the asynchronous copy, because it is the one with no
// limit on size: the synchronous Copy Blob From URL stops at 256 MiB, and a
// fork copies packs of many GiB. The service answers before the bytes have
// moved, so the destination is asked after until they have.
func (b *bucket) Copy(ctx context.Context, sourceKey, destinationKey string) error {
	destination := b.client.NewBlobClient(destinationKey)
	// A source in the destination's own account is authorized by the request's
	// shared key, so its bare URL is enough.
	started, err := destination.StartCopyFromURL(ctx, b.client.NewBlobClient(sourceKey).URL(), &blob.StartCopyFromURLOptions{})
	if err != nil {
		return translate("copy", sourceKey, err)
	}
	status, copyID := started.CopyStatus, deref(started.CopyID)
	for status != nil && *status == blob.CopyStatusTypePending {
		wait := time.NewTimer(b.copyPoll)
		select {
		case <-ctx.Done():
			wait.Stop()
			return fmt.Errorf("azure copy %s to %s: still in progress: %w", sourceKey, destinationKey, ctx.Err())
		case <-wait.C:
		}
		properties, err := destination.GetProperties(ctx, &blob.GetPropertiesOptions{})
		if err != nil {
			return translate("copy", destinationKey, err)
		}
		// The destination describes the last copy made to it, which is this one
		// only while nothing else has written there.
		if deref(properties.CopyID) != copyID {
			return fmt.Errorf("azure copy %s to %s: the destination was overwritten while the copy ran", sourceKey, destinationKey)
		}
		status = properties.CopyStatus
	}
	if status == nil || *status != blob.CopyStatusTypeSuccess {
		return fmt.Errorf("azure copy %s to %s: the copy ended %q", sourceKey, destinationKey, deref(status))
	}
	return nil
}

// PresignGet signs a service SAS: read permission on this one blob until the
// expiry, signed locally with the account key. It carries no start time, since
// one taken from this machine's clock would refuse a reader whose store's clock
// runs a little behind.
func (b *bucket) PresignGet(_ context.Context, key string, expiry time.Duration) (string, error) {
	if expiry <= 0 {
		return "", fmt.Errorf("azure presign %s: expiry %s is not in the future", key, expiry)
	}
	signed, err := sas.BlobSignatureValues{
		Version:       sas.Version,
		ExpiryTime:    time.Now().UTC().Add(expiry),
		Permissions:   (&sas.BlobPermissions{Read: true}).String(),
		ContainerName: b.name,
		BlobName:      key,
	}.SignWithSharedKey(b.credential)
	if err != nil {
		return "", fmt.Errorf("azure presign %s: %w", key, err)
	}
	return b.client.NewBlobClient(key).URL() + "?" + signed.Encode(), nil
}

// accessConditions states a write's condition as the headers the service
// arbitrates on.
func accessConditions(condition objstore.Condition) *blob.AccessConditions {
	modified := &blob.ModifiedAccessConditions{}
	switch {
	case condition.Absent():
		anything := azcore.ETagAny
		modified.IfNoneMatch = &anything
	case condition.Version() != "":
		// A version is an ETag without its quotes, and the header wants them back.
		tag := azcore.ETag(`"` + string(condition.Version()) + `"`)
		modified.IfMatch = &tag
	}
	return &blob.AccessConditions{ModifiedAccessConditions: modified}
}

func wireMetadata(metadata objstore.Metadata) map[string]*string {
	wire := make(map[string]*string, len(metadata))
	for name, value := range metadata {
		wire[name] = &value
	}
	return wire
}

// describe builds an Info from what a response carried, and refuses a response
// without the facts callers revalidate and expire by rather than report zeros.
func describe(key string, size *int64, tag *azcore.ETag, modified *time.Time, metadata map[string]*string) (objstore.Info, error) {
	if size == nil || tag == nil || modified == nil {
		return objstore.Info{}, errors.New("the response came without a size, an ETag or a modification time")
	}
	info := objstore.Info{Key: key, Size: *size, Version: versionOf(*tag), ModTime: *modified}
	// The client hands a name back in the case HTTP gave its header, not the case
	// it was written in; the names this package allows have one spelling.
	for name, value := range metadata {
		if info.Metadata == nil {
			info.Metadata = objstore.Metadata{}
		}
		info.Metadata[strings.ToLower(name)] = deref(value)
	}
	return info, nil
}

func listed(item *container.BlobItem) (objstore.Entry, error) {
	if item.Name == nil || item.Properties == nil {
		return objstore.Entry{}, errors.New("the listing held a blob without a name or properties")
	}
	info, err := describe(*item.Name, item.Properties.ContentLength, item.Properties.ETag, item.Properties.LastModified, nil)
	if err != nil {
		return objstore.Entry{}, fmt.Errorf("%s: %w", *item.Name, err)
	}
	return objstore.Entry{Info: info}, nil
}

// versionOf takes the quotes off an ETag: a header carries them and a listing
// does not, and one object's version must compare equal wherever it was learned.
func versionOf(tag azcore.ETag) objstore.Version {
	return objstore.Version(strings.Trim(string(tag), `"`))
}

func writtenVersion(key string, tag *azcore.ETag) (objstore.Version, error) {
	if tag == nil {
		return "", fmt.Errorf("azure put %s: the write was accepted without an ETag", key)
	}
	return versionOf(*tag), nil
}

// totalFromContentRange reads the blob's whole size out of a ranged response's
// "bytes first-last/total", or nil where there is none to read.
func totalFromContentRange(header *string) *int64 {
	_, total, found := strings.Cut(deref(header), "/")
	if !found {
		return nil
	}
	size, err := strconv.ParseInt(total, 10, 64)
	if err != nil {
		return nil
	}
	return &size
}

func deref[T any](pointer *T) T {
	var zero T
	if pointer == nil {
		return zero
	}
	return *pointer
}

// translate turns the service's answer into objstore's errors, leaving the rest
// wrapped with what was being done. It goes by the service's error code and not
// the HTTP status, because a container that does not exist is a 404 too, and
// that is a misconfiguration to surface, not an object to report absent.
func translate(operation, key string, err error) error {
	switch {
	case bloberror.HasCode(err, bloberror.BlobNotFound), copySourceMissing(err):
		return fmt.Errorf("azure %s %s: %w", operation, key, objstore.ErrNotFound)
	// A create-if-absent of a blob that exists is refused 409, not 412.
	case bloberror.HasCode(err, bloberror.ConditionNotMet, bloberror.BlobAlreadyExists):
		return fmt.Errorf("azure %s %s: %w", operation, key, objstore.ErrConditionNotMet)
	case bloberror.HasCode(err, bloberror.InvalidRange):
		return fmt.Errorf("azure %s %s: %w", operation, key, objstore.ErrRangeNotSatisfiable)
	}
	return fmt.Errorf("azure %s %s: %w", operation, key, err)
}

// copySourceMissing reports a copy refused because its source is not there. The
// service does not answer that with BlobNotFound but with CannotVerifyCopySource,
// the code of every failure to read a copy's source, and says which failure it
// was only in headers: the source's own status and error code.
// https://learn.microsoft.com/en-us/rest/api/storageservices/status-and-error-codes2#copy-api-error-response
func copySourceMissing(err error) bool {
	var refused *azcore.ResponseError
	if !errors.As(err, &refused) || refused.ErrorCode != string(bloberror.CannotVerifyCopySource) || refused.RawResponse == nil {
		return false
	}
	return refused.RawResponse.Header.Get("x-ms-copy-source-error-code") == string(bloberror.BlobNotFound)
}

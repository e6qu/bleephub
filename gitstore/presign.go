package gitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	minio "github.com/minio/minio-go/v7"
)

// PresignedGetURL returns a URL that fetches one object directly from the
// private object store without lending bleephub's credentials: the signature
// authorizes exactly one GET of one key for expiry. Call it only for a reader
// already entitled to the bytes; expiry bounds how long that entitlement lasts.
func (f *S3FS) PresignedGetURL(ctx context.Context, name string, expiry time.Duration) (string, error) {
	if f == nil || f.client == nil {
		return "", errors.New("presign: no object store client")
	}
	if expiry <= 0 {
		return "", fmt.Errorf("presign %s: expiry %s is not in the future", name, expiry)
	}
	key := f.key(name)
	signed, err := f.client.Client.PresignedGetObject(ctx, f.bucket, key, expiry, url.Values{})
	if err != nil {
		return "", fmt.Errorf("presign %s: %w", key, err)
	}
	return signed.String(), nil
}

// s3StreamPartSize is one multipart part's size; S3 requires every part but
// the last to be at least 5 MiB.
const s3StreamPartSize = 16 << 20

// PutStream writes a stream to one key without buffering all of it. A stream
// fitting in one part is sent as a single object; anything longer continues as
// a multipart upload, which publishes atomically so a reader never sees a
// partial artefact.
func (f *S3FS) PutStream(ctx context.Context, name string, source io.Reader) error {
	if f == nil || f.client == nil {
		return errors.New("put stream: no object store client")
	}
	key := f.key(name)
	buffer := make([]byte, s3StreamPartSize)
	read, err := io.ReadFull(source, buffer)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("read %s: %w", name, err)
	}
	if read < len(buffer) {
		if _, err := f.client.Client.PutObject(ctx, f.bucket, key, bytes.NewReader(buffer[:read]), int64(read), minio.PutObjectOptions{}); err != nil {
			return fmt.Errorf("s3 put %s: %w", key, err)
		}
		return nil
	}
	return f.putStreamMultipart(ctx, key, buffer, source)
}

// putStreamMultipart finishes a PutStream whose source outgrew one part.
func (f *S3FS) putStreamMultipart(ctx context.Context, key string, first []byte, source io.Reader) error {
	uploadID, err := f.client.NewMultipartUpload(ctx, f.bucket, key, minio.PutObjectOptions{})
	if err != nil {
		return fmt.Errorf("s3 multipart create %s: %w", key, err)
	}
	abort := func() {
		_ = f.client.AbortMultipartUpload(context.WithoutCancel(ctx), f.bucket, key, uploadID)
	}

	var parts []minio.CompletePart
	part := first
	for number := 1; ; number++ {
		uploaded, err := f.client.PutObjectPart(ctx, f.bucket, key, uploadID, number, bytes.NewReader(part), int64(len(part)), minio.PutObjectPartOptions{})
		if err != nil {
			abort()
			return fmt.Errorf("s3 multipart upload %s part %d: %w", key, number, err)
		}
		parts = append(parts, minio.CompletePart{ETag: uploaded.ETag, PartNumber: number})
		if len(part) < s3StreamPartSize {
			break
		}
		next := make([]byte, s3StreamPartSize)
		read, err := io.ReadFull(source, next)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			abort()
			return fmt.Errorf("read %s: %w", key, err)
		}
		if read == 0 {
			break
		}
		part = next[:read]
	}
	if _, err := f.client.CompleteMultipartUpload(ctx, f.bucket, key, uploadID, parts, minio.PutObjectOptions{}); err != nil {
		abort()
		return fmt.Errorf("s3 multipart complete %s: %w", key, err)
	}
	return nil
}

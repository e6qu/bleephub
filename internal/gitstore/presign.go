package gitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Handing a client a URL it fetches itself.
//
// The pack artefacts under a repository's prefix are immutable and
// content-addressed, so the bytes behind one key never change and a reader that
// has the key needs nothing else from this server. What it does need is
// permission, and the object store does not know bleephub's users: the bucket
// is private and only bleephub's own credentials open it.
//
// A presigned GET closes that gap without lending the credential. The signature
// is computed here, over one bucket, one key, the GET method and an expiry, and
// travels in the URL's query string; the object store recomputes it and serves
// exactly that one object until the expiry passes. Nothing else in the bucket is
// reachable with it, and no credential of this server's leaves the process.

// PresignedGetURL returns a URL that fetches one object of this filesystem
// directly from the object store, without bleephub's credentials.
//
// The signature authorizes precisely one GET of one key, so the caller's own
// authorization decision — made before this is called — is what the URL carries.
// It is therefore only ever built for a reader already entitled to the bytes,
// and the expiry is what bounds how long that entitlement outlives the request
// that established it.
func (f *S3FS) PresignedGetURL(ctx context.Context, name string, expiry time.Duration) (string, error) {
	if f == nil || f.client == nil {
		return "", errors.New("presign: no object store client")
	}
	if expiry <= 0 {
		return "", fmt.Errorf("presign %s: expiry %s is not in the future", name, expiry)
	}
	key := f.key(name)
	signed, err := s3.NewPresignClient(f.client).PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(f.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", fmt.Errorf("presign %s: %w", key, err)
	}
	return signed.URL, nil
}

// s3StreamPartSize is how much of a stream one multipart part carries. Amazon
// S3 requires every part but the last to be at least five mebibytes, and a
// larger part means fewer requests for the same bytes.
const s3StreamPartSize = 16 << 20

// PutStream writes a stream to one key without holding all of it in memory.
//
// A stream that fits in one part is sent as a single object: the reader is
// drained into the buffer that would have become the first part, and the length
// is then known. Anything longer continues into a multipart upload, which is
// what keeps a large artefact from having to be buffered whole, and which
// publishes atomically — the key appears only when the completion request
// succeeds, so a reader never sees a partial artefact.
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
		if _, err := f.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(f.bucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(buffer[:read]),
			ContentLength: aws.Int64(int64(read)),
		}); err != nil {
			return fmt.Errorf("s3 put %s: %w", key, err)
		}
		return nil
	}
	return f.putStreamMultipart(ctx, key, buffer, source)
}

// putStreamMultipart finishes a PutStream whose source outgrew one part.
func (f *S3FS) putStreamMultipart(ctx context.Context, key string, first []byte, source io.Reader) error {
	created, err := f.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(f.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 multipart create %s: %w", key, err)
	}
	uploadID := aws.ToString(created.UploadId)
	abort := func() {
		_, _ = f.client.AbortMultipartUpload(context.WithoutCancel(ctx), &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(f.bucket),
			Key:      aws.String(key),
			UploadId: aws.String(uploadID),
		})
	}

	var parts []s3types.CompletedPart
	part := first
	for number := int32(1); ; number++ {
		uploaded, err := f.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(f.bucket),
			Key:           aws.String(key),
			UploadId:      aws.String(uploadID),
			PartNumber:    aws.Int32(number),
			Body:          bytes.NewReader(part),
			ContentLength: aws.Int64(int64(len(part))),
		})
		if err != nil {
			abort()
			return fmt.Errorf("s3 multipart upload %s part %d: %w", key, number, err)
		}
		parts = append(parts, s3types.CompletedPart{ETag: uploaded.ETag, PartNumber: aws.Int32(number)})
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
	if _, err := f.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(f.bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	}); err != nil {
		abort()
		return fmt.Errorf("s3 multipart complete %s: %w", key, err)
	}
	return nil
}

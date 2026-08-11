package gitstore

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// transientS3Client returns an *s3.Client whose every request short-circuits
// with a retryable HTTP 500 before it is ever signed or sent, standing in for a
// transient S3 outage (throttle, 5xx, network blip).
func transientS3Client() *s3.Client {
	return s3.New(s3.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String("http://127.0.0.1:1"),
		Credentials:      credentials.NewStaticCredentialsProvider("x", "y", ""),
		RetryMaxAttempts: 1,
		APIOptions: []func(*middleware.Stack) error{
			func(stack *middleware.Stack) error {
				return stack.Finalize.Add(
					middleware.FinalizeMiddlewareFunc("STORE037InjectTransient",
						func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
							return middleware.FinalizeOutput{}, middleware.Metadata{}, &smithyhttp.ResponseError{
								Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 500, Body: http.NoBody}},
								Err:      errors.New("transient S3 failure"),
							}
						}),
					middleware.Before)
			},
		},
	})
}

// TestS3TransientErrorIsNotMistakenForNotExist covers STORE-037: a transient S3
// failure while reading a ref must surface as an error, never as os.ErrNotExist.
// go-git treats a missing ref as "branch absent" and a push would then overwrite
// a live branch — a silent history loss. Only a definite 404 (NoSuchKey /
// NotFound) may map to os.ErrNotExist; every other failure must propagate.
func TestS3TransientErrorIsNotMistakenForNotExist(t *testing.T) {
	fs := &S3FS{
		client: transientS3Client(),
		bucket: "bucket",
		prefix: "repo",
		active: &s3ActiveFiles{files: map[string]*s3FileState{}},
		locks:  newS3KeyLocks(),
	}

	if _, err := fs.Open("refs/heads/main"); err == nil {
		t.Fatal("Open returned nil error on a transient S3 failure")
	} else if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open mapped a transient S3 failure to os.ErrNotExist: %v", err)
	}

	if _, err := fs.Stat("refs/heads/main"); err == nil {
		t.Fatal("Stat returned nil error on a transient S3 failure")
	} else if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat mapped a transient S3 failure to os.ErrNotExist: %v", err)
	}
}

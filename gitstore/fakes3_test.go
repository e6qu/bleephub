package gitstore

import (
	"github.com/e6qu/bleephub/gitstore/s3fake"
	"testing"
)

// fakeS3 pairs the counting object store with the options the filesystems built
// over it run under, so a test tunes one value and every handle it opens agrees.
type fakeS3 struct {
	*s3fake.Server
	opts Options
}

// newFakeS3 starts a fake whose filesystems cache packs in a directory of the
// test's own, so no test reads extents another one cached.
func newFakeS3(tb testing.TB) *fakeS3 {
	tb.Helper()
	server := s3fake.New()
	tb.Cleanup(server.Close)
	return &fakeS3{Server: server, opts: Options{CacheDir: tb.TempDir()}}
}

func (f *fakeS3) fs(bucket, prefix string) *S3FS {
	return NewS3FSWithClient(f.Client(), bucket, prefix, f.opts)
}

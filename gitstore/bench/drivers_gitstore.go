package main

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/e6qu/bleephub/gitstore"
)

func init() {
	registerStorerDriver("gitstore", func() StorerDriver { return &gitstoreDriver{} })
}

// gitstoreTuning carries the library tunables the command line overrides, for
// measuring what a setting is worth. The zero value leaves every default.
var gitstoreTuning gitstore.Options

// gitstoreDriver is the library under test, configured as bleephub runs it.
type gitstoreDriver struct {
	env Env
	fs  *gitstore.S3FS
}

func (d *gitstoreDriver) Name() string { return "gitstore" }
func (d *gitstoreDriver) Describe() string {
	return "dotgit layout in the bucket; ranged pack reads, local pack cache, membership index, compaction"
}

func (d *gitstoreDriver) Setup(ctx context.Context, env Env) error {
	d.env = env
	fs, err := d.newFS(ctx)
	d.fs = fs
	return err
}

// newFS builds a filesystem with a pack cache of its own, which is what a
// replica that has never served a repository has.
func (d *gitstoreDriver) newFS(ctx context.Context) (*gitstore.S3FS, error) {
	cacheDir, err := d.env.tempDir("gitstore-cache-*")
	if err != nil {
		return nil, err
	}
	return gitstore.NewS3FS(ctx, d.env.Endpoint, d.env.Bucket, d.env.Prefix+"/gitstore", gitstore.Options{
		Region:      d.env.Region,
		Credentials: credentials.NewStaticV4(d.env.AccessKey, d.env.SecretKey, ""),
		CacheDir:    cacheDir,
		// The harness decides when maintenance runs, so that its cost lands in
		// the maintenance phase and not in whichever push crossed the trigger.
		CompactionTrigger: -1,
		ChunkBytes:        gitstoreTuning.ChunkBytes,
		MultipartBytes:    gitstoreTuning.MultipartBytes,
		MemoryCacheBytes:  gitstoreTuning.MemoryCacheBytes,
	})
}

func (d *gitstoreDriver) Open(ctx context.Context, repo string, cold bool) (storer.Storer, error) {
	if cold {
		fs, err := d.newFS(ctx)
		if err != nil {
			return nil, err
		}
		d.fs = fs
	}
	stor, err := gitstore.OpenObjectStore(d.fs, repo)
	if err != nil {
		return nil, err
	}
	if err := gitstore.Init(stor); err != nil {
		return nil, err
	}
	return stor, nil
}

func (d *gitstoreDriver) Maintain(ctx context.Context, stor storer.Storer) (bool, error) {
	full, ok := stor.(storage.Storer)
	if !ok {
		return false, fmt.Errorf("gitstore handed back a %T, not a storage.Storer", stor)
	}
	_, err := gitstore.CompactRepository(ctx, full)
	return true, err
}

func (d *gitstoreDriver) Close() error { return nil }

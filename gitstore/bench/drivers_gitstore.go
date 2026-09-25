package main

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage"

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
	env   Env
	store *gitstore.Store
}

func (d *gitstoreDriver) Name() string     { return "gitstore" }
func (d *gitstoreDriver) Stores() []string { return everyStore }
func (d *gitstoreDriver) Describe() string {
	return "git's layout in the bucket, read by a native storer; ranged pack reads, local pack cache, membership index, compaction"
}

func (d *gitstoreDriver) Setup(ctx context.Context, env Env) error {
	d.env = env
	store, err := d.newStore(ctx)
	d.store = store
	return err
}

// newStore opens a store with a pack cache of its own and nothing remembered,
// which is what a replica that has never served a repository has.
func (d *gitstoreDriver) newStore(ctx context.Context) (*gitstore.Store, error) {
	cacheDir, err := d.env.tempDir("gitstore-cache-*")
	if err != nil {
		return nil, err
	}
	opts := gitstore.Options{
		CacheDir: cacheDir,
		// The harness decides when maintenance runs, so that its cost lands in
		// the maintenance phase and not in whichever push crossed the trigger.
		CompactAfterPacks: -1,
		ChunkBytes:        gitstoreTuning.ChunkBytes,
		MultipartBytes:    gitstoreTuning.MultipartBytes,
		MemoryCacheBytes:  gitstoreTuning.MemoryCacheBytes,
	}
	bucket, err := d.env.openBucket(d.env.Endpoint, opts.UploadPieceBytes())
	if err != nil {
		return nil, err
	}
	return gitstore.Open(bucket, d.env.Prefix+"/gitstore", opts), nil
}

func (d *gitstoreDriver) Open(ctx context.Context, repo string, cold bool) (storer.Storer, error) {
	if cold {
		store, err := d.newStore(ctx)
		if err != nil {
			return nil, err
		}
		d.store = store
	}
	stor, err := d.store.Repository(repo)
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

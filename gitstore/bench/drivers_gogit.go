package main

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/memory"
)

func init() {
	registerStorerDriver("gogit-memory", func() StorerDriver { return &memoryDriver{} })
	registerStorerDriver("gogit-disk", func() StorerDriver { return &diskDriver{} })
}

// memoryDriver is the ceiling: go-git with nothing under it. No object-store
// design can beat it, so it prices the plumbing every other driver also pays.
type memoryDriver struct {
	mu    sync.Mutex
	repos map[string]*memory.Storage
}

func (d *memoryDriver) Name() string { return "gogit-memory" }
func (d *memoryDriver) Describe() string {
	return "go-git in-memory storage: no persistence, the ceiling"
}

func (d *memoryDriver) Setup(context.Context, Env) error {
	d.repos = map[string]*memory.Storage{}
	return nil
}

func (d *memoryDriver) Open(_ context.Context, repo string, _ bool) (storer.Storer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.repos[repo] == nil {
		d.repos[repo] = memory.NewStorage()
	}
	return d.repos[repo], nil
}

func (d *memoryDriver) Maintain(context.Context, storer.Storer) (bool, error) { return false, nil }
func (d *memoryDriver) Close() error                                          { return nil }

// diskDriver is go-git's dotgit layout on local disk: what the same code costs
// when a file operation is a syscall rather than a request. Its "cold" is only
// a fresh handle — the operating system's page cache is not dropped — so read
// it as the local-disk reference, not as a cold-start figure.
type diskDriver struct {
	root string
}

func (d *diskDriver) Name() string     { return "gogit-disk" }
func (d *diskDriver) Describe() string { return "go-git dotgit layout on local disk" }

func (d *diskDriver) Setup(_ context.Context, env Env) error {
	root, err := env.tempDir("gogit-disk-*")
	d.root = root
	return err
}

func (d *diskDriver) Open(_ context.Context, repo string, _ bool) (storer.Storer, error) {
	return filesystem.NewStorage(osfs.New(filepath.Join(d.root, filepath.FromSlash(repo))), cache.NewObjectLRUDefault()), nil
}

func (d *diskDriver) Maintain(context.Context, storer.Storer) (bool, error) { return false, nil }
func (d *diskDriver) Close() error                                          { return nil }

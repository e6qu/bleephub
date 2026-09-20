package main

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/go-git/go-git/v5/plumbing/storer"
)

// Env is what a driver is given to reach the object store. Endpoint is always
// the meter, never the store itself.
type Env struct {
	Endpoint string
	// DirectEndpoint reaches the object store past the meter, for traffic that
	// is not git storage and must not be billed as it.
	DirectEndpoint string
	Bucket         string
	Region         string
	AccessKey      string
	SecretKey      string
	// Prefix is unique to this run, so runs against a shared bucket do not
	// read each other's repositories.
	Prefix string
	// TempDir is scratch space the harness removes when the run ends.
	TempDir string
}

// tempDir makes a scratch directory under the run's own.
func (e Env) tempDir(pattern string) (string, error) {
	return os.MkdirTemp(e.TempDir, pattern)
}

// StorerDriver is an implementation that can be driven in-process as a go-git
// storer.Storer. The harness runs the same plumbing against each one — the
// packfile ingest a receive-pack does, the pack encode an upload-pack does — so
// what differs between drivers is only where and how the bytes are kept.
type StorerDriver interface {
	Name() string
	// Describe says, in a line, how the driver keeps git data.
	Describe() string
	// Setup prepares the driver for one run.
	Setup(ctx context.Context, env Env) error
	// Open returns a handle on repo as a request to a running server would get
	// one. With cold set it is the handle a replica that has never served the
	// repository would get: every local cache is dropped first.
	Open(ctx context.Context, repo string, cold bool) (storer.Storer, error)
	// Maintain runs the driver's housekeeping (compaction, gc) on repo,
	// reporting false if it has none.
	Maintain(ctx context.Context, stor storer.Storer) (bool, error)
	Close() error
}

var storerDrivers = map[string]func() StorerDriver{}

func registerStorerDriver(name string, build func() StorerDriver) {
	storerDrivers[name] = build
}

func storerDriverNames() []string {
	names := make([]string, 0, len(storerDrivers))
	for name := range storerDrivers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func newStorerDriver(name string) (StorerDriver, error) {
	build, ok := storerDrivers[name]
	if !ok {
		return nil, fmt.Errorf("unknown driver %q (have %v)", name, storerDriverNames())
	}
	return build(), nil
}

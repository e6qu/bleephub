package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"

	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/e6qu/bleephub/gitstore/objstore"
	"github.com/e6qu/bleephub/gitstore/objstore/azure"
	"github.com/e6qu/bleephub/gitstore/objstore/gcs"
)

// The kinds of object store a run can be made against, named as bleephub's
// BLEEPHUB_OBJECT_STORE names them.
const (
	storeS3    = "s3"
	storeAzure = "azure"
	storeGCS   = "gcs"
)

var everyStore = []string{storeS3, storeAzure, storeGCS}

// Env is what a driver is given to reach the object store. Endpoint is always
// the meter, never the store itself.
type Env struct {
	// Store is the kind of object store Endpoint speaks for.
	Store    string
	Endpoint string
	// Bucket is the bucket, or on Azure the container.
	Bucket string
	// Region, AccessKey and SecretKey reach S3.
	Region    string
	AccessKey string
	SecretKey string
	// AzureAccount and AzureKey are an Azure storage account's shared key.
	AzureAccount string
	AzureKey     string
	// GCSCredentialsFile is a Cloud Storage service-account key file.
	GCSCredentialsFile string
	// Prefix is unique to this run, so runs against a shared bucket do not
	// read each other's repositories: a design that loads what its store holds
	// when it starts would otherwise load every earlier run's too.
	Prefix string
	// TempDir is scratch space the harness removes when the run ends.
	TempDir string
}

// forRun is the environment of one of the runs of a driver, under a prefix of
// its own.
func (e Env) forRun(run int) Env {
	e.Prefix = fmt.Sprintf("%s/run-%d", e.Prefix, run)
	return e
}

// openBucket opens the run's bucket through the objstore driver for its store,
// at endpoint: the meter for a driver under measurement, the store itself for
// the harness's own housekeeping, which no driver is billed for. pieceBytes is
// the size of an upload's parts; zero selects the driver's default.
func (e Env) openBucket(endpoint string, pieceBytes uint64) (objstore.Bucket, error) {
	switch e.Store {
	case storeS3:
		return objstore.NewS3(e.Bucket, objstore.S3Options{
			Endpoint:    endpoint,
			Region:      e.Region,
			Credentials: credentials.NewStaticV4(e.AccessKey, e.SecretKey, ""),
			PartBytes:   pieceBytes,
		})
	case storeAzure:
		return azure.New(e.Bucket, azure.Options{
			Endpoint: endpoint, AccountName: e.AzureAccount, AccountKey: e.AzureKey, BlockBytes: pieceBytes,
		})
	case storeGCS:
		key, err := os.ReadFile(e.GCSCredentialsFile)
		if err != nil {
			return nil, fmt.Errorf("gcs credentials: %w", err)
		}
		// Cloud Storage counts a chunk in signed bytes; a part size from the
		// command line is far below where the conversion could wrap.
		return gcs.New(e.Bucket, gcs.Options{Endpoint: endpoint, CredentialsJSON: key, ChunkBytes: int64(pieceBytes)}) // #nosec G115
	}
	return nil, fmt.Errorf("no object store %q (have %v)", e.Store, everyStore)
}

// bleephubStoreSettings are the settings that point a bleephub server at the
// run's store, through the meter, as its operator would write them.
func (e Env) bleephubStoreSettings() []string {
	switch e.Store {
	case storeS3:
		return []string{
			"BLEEPHUB_OBJECT_STORE=s3",
			"BLEEPHUB_S3_ENDPOINT=" + e.Endpoint,
			"BLEEPHUB_S3_REGION=" + e.Region,
			"AWS_ACCESS_KEY_ID=" + e.AccessKey,
			"AWS_SECRET_ACCESS_KEY=" + e.SecretKey,
			"AWS_EC2_METADATA_DISABLED=true",
		}
	case storeAzure:
		return []string{
			"BLEEPHUB_OBJECT_STORE=azure",
			"BLEEPHUB_AZURE_ENDPOINT=" + e.Endpoint,
			"BLEEPHUB_AZURE_ACCOUNT=" + e.AzureAccount,
			"BLEEPHUB_AZURE_KEY=" + e.AzureKey,
		}
	case storeGCS:
		return []string{
			"BLEEPHUB_OBJECT_STORE=gcs",
			"BLEEPHUB_GCS_ENDPOINT=" + e.Endpoint,
			"BLEEPHUB_GCS_CREDENTIALS_FILE=" + e.GCSCredentialsFile,
		}
	}
	return nil
}

// reaches reports whether a driver keeping its data in the stores given can run
// against store. A driver that keeps nothing in an object store runs against
// any.
func reaches(stores []string, store string) bool {
	return stores == nil || slices.Contains(stores, store)
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
	// Stores lists the kinds of object store the driver can keep its data in,
	// or nil if it keeps none in one.
	Stores() []string
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

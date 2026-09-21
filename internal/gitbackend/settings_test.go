package gitbackend

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/server/testutil"
)

// deployment sets a whole storage configuration, starting from none, so each
// test states everything it relies on.
func deployment(t *testing.T, environment map[string]string) {
	t.Helper()
	clearTunables(t)
	testutil.ClearObjectStoreSettings(t)
	for name, value := range environment {
		t.Setenv(name, value)
	}
}

// refusal reads the configuration, which must be refused, and returns why.
func refusal(t *testing.T) string {
	t.Helper()
	settings, err := SettingsFromEnv()
	if err == nil {
		t.Fatalf("the configuration was accepted: %+v", settings)
	}
	return err.Error()
}

func wantNamed(t *testing.T, message string, names ...string) {
	t.Helper()
	for _, name := range names {
		if !strings.Contains(message, name) {
			t.Errorf("the refusal does not name %s: %s", name, message)
		}
	}
}

// TestADeploymentWithNoObjectStoreConfiguresNothing protects the development
// default: a server told nothing about an object store keeps nothing in one, and
// that is not an error.
func TestADeploymentWithNoObjectStoreConfiguresNothing(t *testing.T) {
	deployment(t, nil)
	settings, err := SettingsFromEnv()
	if err != nil {
		t.Fatalf("an environment that names no object store was refused: %v", err)
	}
	if settings.Driver != "" || settings.GitBucket != "" || settings.ObjectBucket != "" {
		t.Fatalf("settings = %+v, want none", settings)
	}
	if GitStorageIsObjectStore() {
		t.Fatal("git storage is reported to be in an object store nobody named")
	}
}

// TestABucketWithoutAStatedObjectStoreIsRefused protects the rule that the kind
// of store is stated: it is never S3 because it used to be, and never guessed
// from the shape of an endpoint.
func TestABucketWithoutAStatedObjectStoreIsRefused(t *testing.T) {
	for _, bucket := range []string{"BLEEPHUB_GIT_BUCKET", "BLEEPHUB_OBJECT_BUCKET"} {
		t.Run(bucket, func(t *testing.T) {
			deployment(t, map[string]string{bucket: "bleephub", strings.Replace(bucket, "BUCKET", "PREFIX", 1): "here"})
			wantNamed(t, refusal(t), "BLEEPHUB_OBJECT_STORE", "azure", "gcs", "s3")
		})
	}
}

// TestAnUnknownObjectStoreIsRefused protects an operator who misspells the
// driver from a server that starts on something else.
func TestAnUnknownObjectStoreIsRefused(t *testing.T) {
	deployment(t, map[string]string{
		"BLEEPHUB_OBJECT_STORE": "minio",
		"BLEEPHUB_GIT_BUCKET":   "bleephub", "BLEEPHUB_GIT_PREFIX": "git",
		"BLEEPHUB_S3_REGION": "us-east-1",
	})
	message := refusal(t)
	wantNamed(t, message, `BLEEPHUB_OBJECT_STORE="minio"`, "azure", "gcs", "s3")
	if strings.Contains(message, "BLEEPHUB_S3_REGION") {
		t.Errorf("a driver's setting is blamed when the problem is the driver's name: %s", message)
	}
}

// TestAnObjectStoreThatNothingIsKeptInIsRefused protects against a deployment
// that names a store and no bucket, and so believes its bytes are somewhere they
// are not.
func TestAnObjectStoreThatNothingIsKeptInIsRefused(t *testing.T) {
	deployment(t, map[string]string{"BLEEPHUB_OBJECT_STORE": "s3", "BLEEPHUB_S3_REGION": "us-east-1"})
	wantNamed(t, refusal(t), "BLEEPHUB_OBJECT_STORE=s3", "BLEEPHUB_GIT_BUCKET", "BLEEPHUB_OBJECT_BUCKET")
}

// TestABucketWithoutItsPrefixIsRefused protects the two stores from each other:
// they may share a bucket, so where each lives in it is stated and never
// defaulted, and they may not live inside one another.
func TestABucketWithoutItsPrefixIsRefused(t *testing.T) {
	deployment(t, map[string]string{
		"BLEEPHUB_OBJECT_STORE": "s3", "BLEEPHUB_S3_REGION": "us-east-1",
		"BLEEPHUB_GIT_BUCKET": "bleephub", "BLEEPHUB_OBJECT_BUCKET": "bleephub",
	})
	wantNamed(t, refusal(t), "BLEEPHUB_GIT_PREFIX", "BLEEPHUB_OBJECT_PREFIX")

	deployment(t, map[string]string{
		"BLEEPHUB_OBJECT_STORE": "s3", "BLEEPHUB_S3_REGION": "us-east-1",
		"BLEEPHUB_GIT_BUCKET": "bleephub", "BLEEPHUB_GIT_PREFIX": "data",
		"BLEEPHUB_OBJECT_BUCKET": "bleephub", "BLEEPHUB_OBJECT_PREFIX": "data/objects",
	})
	wantNamed(t, refusal(t), "overlap", "BLEEPHUB_GIT_PREFIX", "BLEEPHUB_OBJECT_PREFIX")

	deployment(t, map[string]string{"BLEEPHUB_GIT_PREFIX": "git"})
	wantNamed(t, refusal(t), "BLEEPHUB_GIT_PREFIX", "BLEEPHUB_GIT_BUCKET")
}

// TestEachDriverNamesTheSettingsItIsMissing protects the rule that how a store
// is reached is stated in full: no region is assumed for S3, no account for
// Azure, and no credentials are looked for on behalf of Cloud Storage.
func TestEachDriverNamesTheSettingsItIsMissing(t *testing.T) {
	missing := map[string][]string{
		"s3":    {"BLEEPHUB_S3_REGION"},
		"azure": {"BLEEPHUB_AZURE_ENDPOINT", "BLEEPHUB_AZURE_ACCOUNT", "BLEEPHUB_AZURE_KEY"},
		"gcs":   {"BLEEPHUB_GCS_ENDPOINT", "BLEEPHUB_GCS_CREDENTIALS_FILE"},
	}
	for driver, names := range missing {
		t.Run(driver, func(t *testing.T) {
			deployment(t, map[string]string{
				"BLEEPHUB_OBJECT_STORE": driver,
				"BLEEPHUB_GIT_BUCKET":   "bleephub", "BLEEPHUB_GIT_PREFIX": "git",
			})
			// The region the AWS SDKs read is not one this server reads.
			t.Setenv("AWS_REGION", "eu-west-1")
			wantNamed(t, refusal(t), names...)
		})
	}
}

// TestS3WithoutAnEndpointIsAWSItself protects what an unset S3 endpoint means:
// AWS S3 in the stated region. It is the one driver setting that may be unset.
func TestS3WithoutAnEndpointIsAWSItself(t *testing.T) {
	deployment(t, map[string]string{
		"BLEEPHUB_OBJECT_STORE": "s3", "BLEEPHUB_S3_REGION": "eu-west-1",
		"BLEEPHUB_GIT_BUCKET": "bleephub", "BLEEPHUB_GIT_PREFIX": "git",
	})
	settings, err := SettingsFromEnv()
	if err != nil {
		t.Fatalf("S3 with a region and no endpoint was refused: %v", err)
	}
	if settings.Endpoint != "" {
		t.Fatalf("endpoint = %q, want none", settings.Endpoint)
	}
}

// TestASettingOfADriverThatWasNotChosenIsRefused protects a configuration from
// saying two things: the operator who left an Azure key beside an S3 deployment
// believes it does something, and is told that it does not.
func TestASettingOfADriverThatWasNotChosenIsRefused(t *testing.T) {
	deployment(t, map[string]string{
		"BLEEPHUB_OBJECT_STORE": "s3", "BLEEPHUB_S3_REGION": "us-east-1",
		"BLEEPHUB_GIT_BUCKET": "bleephub", "BLEEPHUB_GIT_PREFIX": "git",
		"BLEEPHUB_AZURE_KEY":    "c2VjcmV0",
		"BLEEPHUB_GCS_ENDPOINT": "https://storage.googleapis.com",
	})
	message := refusal(t)
	wantNamed(t, message, "BLEEPHUB_AZURE_KEY", "BLEEPHUB_GCS_ENDPOINT", "BLEEPHUB_OBJECT_STORE=s3")
	if strings.Contains(message, "c2VjcmV0") {
		t.Errorf("the refusal repeats an account key: %s", message)
	}

	deployment(t, map[string]string{"BLEEPHUB_S3_ENDPOINT": "http://127.0.0.1:9000"})
	wantNamed(t, refusal(t), "BLEEPHUB_S3_ENDPOINT", "BLEEPHUB_OBJECT_STORE is not set")
}

// TestEveryProblemOfAConfigurationIsReportedAtOnce protects the operator from
// fixing a deployment one restart at a time: the store's problems and the
// tunables' arrive in one refusal.
func TestEveryProblemOfAConfigurationIsReportedAtOnce(t *testing.T) {
	deployment(t, map[string]string{
		"BLEEPHUB_OBJECT_STORE":         "azure",
		"BLEEPHUB_GIT_BUCKET":           "bleephub",
		"BLEEPHUB_OBJECT_PREFIX":        "objects",
		"BLEEPHUB_AZURE_ACCOUNT":        "account",
		"BLEEPHUB_S3_REGION":            "us-east-1",
		"BLEEPHUB_GITSTORE_CACHE_BYTES": "8G",
	})
	wantNamed(t, refusal(t),
		"BLEEPHUB_GIT_PREFIX", "BLEEPHUB_OBJECT_BUCKET", "BLEEPHUB_AZURE_ENDPOINT", "BLEEPHUB_AZURE_KEY",
		"BLEEPHUB_S3_REGION", "BLEEPHUB_GITSTORE_CACHE_BYTES")
}

// TestARefusedConfigurationOpensNoStore protects the two doors into the object
// store: neither the git store nor the byte store opens on a configuration that
// was refused.
func TestARefusedConfigurationOpensNoStore(t *testing.T) {
	deployment(t, map[string]string{"BLEEPHUB_GIT_BUCKET": "bleephub", "BLEEPHUB_GIT_PREFIX": "git"})
	forgetOpenedStore(t)
	if opened, err := GetStore(context.Background()); err == nil || opened != nil {
		t.Fatalf("GetStore = %v, %v on a refused configuration", opened, err)
	}
	if opened, err := OpenByteStore(context.Background()); err == nil || opened != nil {
		t.Fatalf("OpenByteStore = %v, %v on a refused configuration", opened, err)
	}
}

// TestEachDriverOpensBothStoresAndPassesConformance protects the switch itself:
// for every driver, the environment a deployment writes opens the git store and
// the byte store, in one bucket under their own prefixes, and each passes the
// conformance probe against that driver's store.
func TestEachDriverOpensBothStoresAndPassesConformance(t *testing.T) {
	for _, driver := range testutil.ObjectStoreDrivers {
		t.Run(driver, func(t *testing.T) {
			clearTunables(t)
			testutil.ConfigureFakeObjectStore(t, driver, "bleephub-test")
			t.Setenv("BLEEPHUB_GIT_BUCKET", "bleephub-test")
			t.Setenv("BLEEPHUB_GIT_PREFIX", "git")
			t.Setenv("BLEEPHUB_OBJECT_BUCKET", "bleephub-test")
			t.Setenv("BLEEPHUB_OBJECT_PREFIX", "objects")
			t.Setenv("BLEEPHUB_GITSTORE_CACHE_DIR", t.TempDir())
			forgetOpenedStore(t)

			repositories, err := GetStore(context.Background())
			if err != nil || repositories == nil {
				t.Fatalf("the git store did not open: %v", err)
			}
			if repositories.Prefix() != "git" || repositories.Bucket().Name() != "bleephub-test" {
				t.Fatalf("the git store is at %s/%s", repositories.Bucket().Name(), repositories.Prefix())
			}
			if _, err := OpenOrInitGitStorage(context.Background(), "owner/repo"); err != nil {
				t.Fatalf("a repository did not open: %v", err)
			}
			objects, err := OpenByteStore(context.Background())
			if err != nil || objects == nil {
				t.Fatalf("the byte store did not open: %v", err)
			}
			if objects.Prefix() != "objects" {
				t.Fatalf("the byte store's prefix = %q", objects.Prefix())
			}
		})
	}
}

// TestADriversOwnLimitOnTheUploadSizeReachesTheOperator protects the one tunable
// whose limits differ by driver: Cloud Storage takes chunks in multiples of
// 256 KiB, and a size it cannot use stops the server with the variable's name,
// its value and the reason, rather than being rounded to one it can.
func TestADriversOwnLimitOnTheUploadSizeReachesTheOperator(t *testing.T) {
	clearTunables(t)
	testutil.ConfigureFakeObjectStore(t, "gcs", "bleephub-test")
	t.Setenv("BLEEPHUB_GIT_BUCKET", "bleephub-test")
	t.Setenv("BLEEPHUB_GIT_PREFIX", "git")
	t.Setenv("BLEEPHUB_GITSTORE_MULTIPART_BYTES", "10000000")
	forgetOpenedStore(t)
	_, err := GetStore(context.Background())
	if err == nil || !strings.Contains(err.Error(), "BLEEPHUB_GITSTORE_MULTIPART_BYTES") ||
		!strings.Contains(err.Error(), "10000000") || !strings.Contains(err.Error(), "multiple of") {
		t.Fatalf("a chunk size Cloud Storage cannot take answered %v", err)
	}
}

// TestACredentialsFileThatCannotBeReadNamesItsSetting protects the operator from
// a bare "no such file": the refusal says which setting named the file.
func TestACredentialsFileThatCannotBeReadNamesItsSetting(t *testing.T) {
	clearTunables(t)
	testutil.ConfigureFakeObjectStore(t, "gcs", "bleephub-test")
	t.Setenv("BLEEPHUB_GIT_BUCKET", "bleephub-test")
	t.Setenv("BLEEPHUB_GIT_PREFIX", "git")
	t.Setenv("BLEEPHUB_GCS_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "absent.json"))
	forgetOpenedStore(t)
	_, err := GetStore(context.Background())
	if err == nil || !strings.Contains(err.Error(), "BLEEPHUB_GCS_CREDENTIALS_FILE") {
		t.Fatalf("an absent credentials file answered %v", err)
	}
}

// TestRepositoriesLiveInABucketOrADirectoryNotBoth pins that the server does not
// choose between two places an operator named for the same thing. With both set
// the bucket used to win silently, so a deployment could run for months on
// storage nobody had meant, with the directory it was meant to use sitting empty.
func TestRepositoriesLiveInABucketOrADirectoryNotBoth(t *testing.T) {
	testutil.ConfigureFakeObjectStore(t, "s3", "bleephub-test")
	t.Setenv("BLEEPHUB_GIT_BUCKET", "bleephub-test")
	t.Setenv("BLEEPHUB_GIT_PREFIX", "git")
	t.Setenv("BLEEPHUB_GIT_DIR", "")
	if _, err := SettingsFromEnv(); err != nil {
		t.Fatalf("premise: the configuration is valid without a git directory: %v", err)
	}
	t.Setenv("BLEEPHUB_GIT_DIR", t.TempDir())
	_, err := SettingsFromEnv()
	if err == nil || !strings.Contains(err.Error(), "BLEEPHUB_GIT_DIR") || !strings.Contains(err.Error(), "BLEEPHUB_GIT_BUCKET") {
		t.Fatalf("a git bucket and a git directory together answered %v, want a refusal naming both", err)
	}
}

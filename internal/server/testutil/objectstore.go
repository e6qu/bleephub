package testutil

import (
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/e6qu/bleephub/gcsclient/gcsfake"
	"github.com/e6qu/bleephub/gitstore/azfake"
	"github.com/e6qu/bleephub/gitstore/s3fake"
)

// ObjectStoreDrivers names every object-store driver a deployment can choose,
// each of which has an in-process fake ConfigureFakeObjectStore can start.
var ObjectStoreDrivers = []string{"s3", "azure", "gcs"}

// objectStoreSettings is every variable that says which object store a
// deployment uses, what it keeps there and how it is reached.
var objectStoreSettings = []string{
	"BLEEPHUB_OBJECT_STORE",
	"BLEEPHUB_GIT_BUCKET", "BLEEPHUB_GIT_PREFIX", "BLEEPHUB_OBJECT_BUCKET", "BLEEPHUB_OBJECT_PREFIX",
	"BLEEPHUB_S3_ENDPOINT", "BLEEPHUB_S3_REGION",
	"BLEEPHUB_AZURE_ENDPOINT", "BLEEPHUB_AZURE_ACCOUNT", "BLEEPHUB_AZURE_KEY",
	"BLEEPHUB_GCS_ENDPOINT", "BLEEPHUB_GCS_CREDENTIALS_FILE",
}

// ClearObjectStoreSettings pins every object-store setting to unset for the
// test, so a developer's shell cannot leak into what the test configures.
func ClearObjectStoreSettings(t *testing.T) {
	t.Helper()
	for _, name := range objectStoreSettings {
		t.Setenv(name, "")
	}
}

// ConfigureFakeObjectStore starts the in-process fake of one driver, makes the
// named bucket in it, and sets the environment as a deployment on that driver
// does: the driver and how it is reached, and nothing else. What is kept in the
// bucket — BLEEPHUB_GIT_BUCKET and the rest — is the caller's to say. It uses
// t.Setenv, so the test cannot be parallel.
func ConfigureFakeObjectStore(t *testing.T, driver, bucket string) {
	t.Helper()
	ClearObjectStoreSettings(t)
	t.Setenv("BLEEPHUB_OBJECT_STORE", driver)
	switch driver {
	case "s3":
		t.Setenv("BLEEPHUB_S3_ENDPOINT", FakeS3Endpoint(t))
		ConfigureS3ForTest(t)
	case "azure":
		fake := azfake.New()
		t.Cleanup(fake.Close)
		fake.CreateContainer(bucket)
		t.Setenv("BLEEPHUB_AZURE_ENDPOINT", fake.URL())
		t.Setenv("BLEEPHUB_AZURE_ACCOUNT", fake.AccountName())
		t.Setenv("BLEEPHUB_AZURE_KEY", fake.AccountKey())
	case "gcs":
		fake := gcsfake.New()
		t.Cleanup(fake.Close)
		fake.CreateBucket(bucket)
		credentials := filepath.Join(t.TempDir(), "service-account.json")
		if err := os.WriteFile(credentials, fake.CredentialsJSON(), 0o600); err != nil {
			t.Fatalf("write the fake service account's key file: %v", err)
		}
		t.Setenv("BLEEPHUB_GCS_ENDPOINT", fake.URL())
		t.Setenv("BLEEPHUB_GCS_CREDENTIALS_FILE", credentials)
	default:
		t.Fatalf("no in-process fake for the object-store driver %q", driver)
	}
}

// ConfigureS3ForTest states what the S3 driver needs beyond an endpoint: the
// region to sign for, and credentials, which the fake accepts whatever they are.
func ConfigureS3ForTest(t *testing.T) {
	t.Helper()
	t.Setenv("BLEEPHUB_S3_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "bleephub-test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "bleephub-test-secret")
}

// FakeS3Endpoint starts an in-process S3-compatible object store that honours
// conditional writes, and returns its URL. It accepts any bucket name and any
// credentials.
func FakeS3Endpoint(t *testing.T) string {
	t.Helper()
	fake := s3fake.New()
	t.Cleanup(fake.Close)
	return fake.URL()
}

// S3EndpointIgnoringConditions starts an object store that accepts a conditional
// write and ignores the condition, as Google Cloud Storage's S3-compatible
// endpoint does: every request succeeds, and two writers racing for one key are
// both told they won. It is the store a server must refuse to start on.
func S3EndpointIgnoringConditions(t *testing.T) string {
	t.Helper()
	target, err := url.Parse(FakeS3Endpoint(t))
	if err != nil {
		t.Fatalf("parse the fake object store's address: %v", err)
	}
	unconditional := httptest.NewServer(&httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			request.Out.Header.Del("If-Match")
			request.Out.Header.Del("If-None-Match")
		},
	})
	t.Cleanup(unconditional.Close)
	return unconditional.URL
}

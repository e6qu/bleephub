package testutil

import (
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"

	"github.com/e6qu/bleephub/gitstore/s3fake"
)

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

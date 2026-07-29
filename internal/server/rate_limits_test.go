package bleephub

import (
	"bytes"
	"net/http/httptest"
	"testing"
)

func TestPrimaryRateLimitsAreStatefulAndCredentialScoped(t *testing.T) {
	server := &Server{rateLimits: map[string]*apiRateWindow{}}
	request := httptest.NewRequest("GET", "/api/v3/user", nil)
	request.Header.Set("Authorization", "Bearer credential-one")

	first := server.rateLimitSnapshot(request, "core", true)
	second := server.rateLimitSnapshot(request, "core", true)
	if first.Used != 1 || first.Remaining != 4999 {
		t.Fatalf("first core snapshot = %+v", first)
	}
	if second.Used != 2 || second.Remaining != 4998 || second.Reset != first.Reset {
		t.Fatalf("second core snapshot = %+v; first = %+v", second, first)
	}

	other := httptest.NewRequest("GET", "/api/v3/user", nil)
	other.Header.Set("Authorization", "Bearer credential-two")
	separate := server.rateLimitSnapshot(other, "core", true)
	if separate.Used != 1 {
		t.Fatalf("a different credential shared rate state: %+v", separate)
	}
}

func TestPrimaryRateLimitResourceClassification(t *testing.T) {
	cases := map[string]string{
		"/api/graphql":                                         "graphql",
		"/api/v3/search/code?q=hello":                          "code_search",
		"/api/v3/search/repositories?q=hello":                  "search",
		"/api/v3/repos/o/r/actions/runners/registration-token": "actions_runner_registration",
		"/api/v3/repos/o/r/dependency-graph/snapshots":         "dependency_snapshots",
		"/api/v3/repos/o/r/code-scanning/sarifs":               "code_scanning_upload",
		"/api/v3/repos/o/r/code-scanning/alerts/1/autofix":     "code_scanning_autofix",
		"/api/v3/scim/v2/enterprises/e/Users":                  "scim",
		"/api/v3/app-manifests/code/conversions":               "integration_manifest",
		"/api/v3/user":                                         "core",
	}
	for path, want := range cases {
		if got := apiRateResource(path); got != want {
			t.Errorf("apiRateResource(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestRateLimitResponseContainsEveryDocumentedResource(t *testing.T) {
	server := &Server{rateLimits: map[string]*apiRateWindow{}}
	request := httptest.NewRequest("GET", "/api/v3/rate_limit", nil)
	recorder := httptest.NewRecorder()
	server.handleGHRateLimit(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	for _, resource := range apiRateResponseResources {
		if !bytes.Contains(recorder.Body.Bytes(), []byte(`"`+resource+`"`)) {
			t.Errorf("rate-limit response omitted %q", resource)
		}
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte(`"code_scanning_upload"`)) {
		t.Error("rate-limit response exposed a resource absent from GitHub's response schema")
	}
}

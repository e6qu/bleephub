package bleephub

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var testRateLimitReset = time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)

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

func TestPrimaryRateIdentityCannotBeBypassedByChangingAuthScheme(t *testing.T) {
	bearer := httptest.NewRequest("GET", "/api/v3/user", nil)
	bearer.Header.Set("Authorization", "Bearer shared-credential")
	classic := httptest.NewRequest("GET", "/api/v3/user", nil)
	classic.Header.Set("Authorization", "TOKEN shared-credential")
	if apiRateIdentity(bearer) != apiRateIdentity(classic) {
		t.Fatal("Bearer and token schemes produced separate budgets for one credential")
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

func TestPrimaryRateLimitRejectsOnlyRequestsBeyondTheBudget(t *testing.T) {
	server := &Server{rateLimits: map[string]*apiRateWindow{}}
	request := httptest.NewRequest("GET", "/api/v3/user", nil)
	request.Header.Set("Authorization", "Bearer exhausted-credential")
	key := apiRateIdentity(request) + "\x1fcore"
	server.rateLimits[key] = &apiRateWindow{
		Limit: 2,
		Used:  1,
		Reset: testRateLimitReset,
	}

	lastAllowed := server.rateLimitSnapshot(request, "core", true)
	if lastAllowed.Exceeded || lastAllowed.Used != 2 || lastAllowed.Remaining != 0 {
		t.Fatalf("last allowed request = %+v", lastAllowed)
	}
	rejected := server.rateLimitSnapshot(request, "core", true)
	if !rejected.Exceeded || rejected.Used != 2 || rejected.Remaining != 0 {
		t.Fatalf("request past the budget = %+v", rejected)
	}
}

func TestRateLimitMiddlewareReturnsGitHubShaped403(t *testing.T) {
	server := newTestServer()
	server.rateLimits = map[string]*apiRateWindow{}
	request := httptest.NewRequest("GET", "/api/v3/user", nil)
	request.Header.Set("Authorization", "Bearer "+defaultToken)
	key := apiRateIdentity(request) + "\x1fcore"
	server.rateLimits[key] = &apiRateWindow{
		Limit: 1,
		Used:  1,
		Reset: testRateLimitReset,
	}
	recorder := httptest.NewRecorder()
	server.ghHeadersMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("rate-limited request reached the handler")
	})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-RateLimit-Remaining") != "0" || recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("rate-limit headers = %v", recorder.Header())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"message":"API rate limit exceeded"`)) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestUnauthenticatedCoreRateLimitIsSixty(t *testing.T) {
	server := &Server{rateLimits: map[string]*apiRateWindow{}}
	request := httptest.NewRequest("GET", "/api/v3/users/octocat", nil)
	got := server.rateLimitSnapshot(request, "core", true)
	if got.Limit != 60 || got.Remaining != 59 {
		t.Fatalf("anonymous core snapshot = %+v", got)
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

package bleephub

import "testing"

// TestAPIRateResourceDoesNotMatchRepositoryNames pins the rate-limit resource
// classifier against repository names that merely contain a special-resource
// substring. Repository names are user-controlled, so a substring match on
// "/import" put every request to a repository called `important` on the
// 100-per-hour source_import budget — 403s and a wrong X-RateLimit-Resource on
// an ordinary contents read.
func TestAPIRateResourceDoesNotMatchRepositoryNames(t *testing.T) {
	cases := []struct{ path, want string }{
		// Repository names that collide with a special resource's substring.
		{"/api/v3/repos/octo/important/contents/README.md", "core"},
		{"/api/v3/repos/octo/imports/issues", "core"},
		{"/api/v3/repos/octo/scim/issues", "core"},
		{"/api/v3/repos/octo/app-manifests/pulls", "core"},

		// The real endpoints must keep their budgets.
		{"/api/v3/repos/octo/hello/import", "source_import"},
		{"/api/v3/repos/octo/hello/import/authors", "source_import"},
		{"/api/v3/repos/octo/hello/import/large_files", "source_import"},
		{"/api/v3/scim/v2/enterprises/acme/Users", "scim"},
		{"/api/v3/app-manifests/abc123/conversions", "integration_manifest"},
		{"/api/v3/search/code", "code_search"},
		{"/api/v3/search/issues", "search"},
		{"/api/graphql", "graphql"},
		{"/api/v3/repos/octo/hello/dependency-graph/snapshots", "dependency_snapshots"},
		{"/api/v3/repos/octo/hello/code-scanning/sarifs", "code_scanning_upload"},
		{"/api/v3/repos/octo/hello/code-scanning/alerts/3/autofix", "code_scanning_autofix"},
		{"/api/v3/repos/octo/hello/actions/runners/registration-token", "actions_runner_registration"},
		{"/api/v3/repos/octo/hello/issues", "core"},
	}
	for _, tc := range cases {
		if got := apiRateResource(tc.path); got != tc.want {
			t.Errorf("apiRateResource(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

package bleephub

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	vendoredSpecFile             = "testdata/github-openapi.json.gz"
	vendoredSpecVersion          = "testdata/github-openapi.VERSION"
	registeredRouteSnapshotFile  = "testdata/registered-api-v3-routes.txt"
	registeredManageSnapshotFile = "testdata/registered-ghes-manage-routes.txt"
)

// TestVendoredOpenAPIMatchesRecordedPin makes the provenance record
// load-bearing. Both fidelity gates measure against the vendored
// description, so if the bytes can be swapped without a matching pin
// update, a gate's verdict can change without anyone deciding it should.
// The digest covers the uncompressed description: gzip output is not
// portable across implementations, the content is.
func TestVendoredOpenAPIMatchesRecordedPin(t *testing.T) {
	meta, err := os.ReadFile(vendoredSpecVersion)
	if err != nil {
		t.Fatalf("read %s: %v", vendoredSpecVersion, err)
	}
	pins := map[string]string{}
	for _, line := range strings.Split(string(meta), "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		pins[key] = strings.TrimSpace(value)
	}
	commit, sha := pins["upstream commit"], pins["content sha256"]
	if len(commit) != 40 {
		t.Errorf("%s: 'upstream commit' must be a full commit SHA, got %q; refresh with scripts/update-github-openapi.sh", vendoredSpecVersion, commit)
	}
	if len(sha) != 64 {
		t.Fatalf("%s: 'content sha256' must be 64 hex characters, got %q", vendoredSpecVersion, sha)
	}
	if !strings.Contains(pins["source"], commit) {
		t.Errorf("%s: 'source' %q does not name the pinned commit %s", vendoredSpecVersion, pins["source"], commit)
	}

	f, err := os.Open(vendoredSpecFile)
	if err != nil {
		t.Fatalf("open %s: %v", vendoredSpecFile, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gunzip %s: %v", vendoredSpecFile, err)
	}
	defer gz.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, gz); err != nil {
		t.Fatalf("read %s: %v", vendoredSpecFile, err)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != sha {
		t.Errorf("%s content sha256 = %s, %s records %s\n"+
			"the description the fidelity gates measure against is not the one recorded as pinned",
			vendoredSpecFile, got, vendoredSpecVersion, sha)
	}
}

// This test enforces the core fidelity invariant the project cares about:
// every route bleephub serves under the GitHub-compatible /api/v3 surface must
// be a REAL GitHub API path — bleephub must not invent paths under the GitHub
// namespace. It validates the registered route table (Server.routePatterns,
// recorded by Server.route) against the official github/rest-api-description
// OpenAPI document, vendored (gzipped) at testdata/github-openapi.json.gz so
// the test is hermetic. Refresh the vendored copy with
// scripts/update-github-openapi.sh.

var paramSegment = regexp.MustCompile(`\{[^}]+\}`)

// normalizePath collapses every "{param}" path segment to "{}", so routes
// match GitHub's templates regardless of parameter naming (bleephub's
// {number} vs GitHub's {issue_number}, etc.).
func normalizePath(path string) string {
	return paramSegment.ReplaceAllString(path, "{}")
}

// loadGitHubOperations parses the vendored OpenAPI description and returns the
// set of normalized "METHOD /path" operations GitHub documents.
func loadGitHubOperations(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(vendoredSpecFile)
	if err != nil {
		t.Fatalf("open vendored OpenAPI: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gunzip OpenAPI: %v", err)
	}
	defer gz.Close()

	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.NewDecoder(gz).Decode(&doc); err != nil {
		t.Fatalf("decode OpenAPI: %v", err)
	}
	if len(doc.Paths) < 500 {
		t.Fatalf("vendored OpenAPI looks truncated: only %d paths", len(doc.Paths))
	}

	ops := make(map[string]bool, len(doc.Paths)*3)
	for path, methods := range doc.Paths {
		norm := normalizePath(path)
		for method := range methods {
			switch method {
			case "get", "post", "put", "patch", "delete", "head":
				ops[strings.ToUpper(method)+" "+norm] = true
			}
		}
	}
	return ops
}

// officialDescriptions are the non-dotcom GitHub descriptions whose route
// lists are vendored in testdata/github-openapi-routes.txt.gz, at the
// commit pinned in testdata/github-openapi.VERSION. A route bleephub
// serves that the dotcom description omits must be citable in one of
// them; otherwise nothing establishes that GitHub has it at all.
var officialDescriptions = []string{"ghec", "ghes-3.21", "ghes-3.13", "ghes-2.22", "api-2022"}

const routeIndexFile = "testdata/github-openapi-routes.txt.gz"

// loadOfficialRouteIndex returns description name -> normalized
// "METHOD /path" set.
func loadOfficialRouteIndex(t *testing.T) map[string]map[string]bool {
	t.Helper()
	f, err := os.Open(routeIndexFile)
	if err != nil {
		t.Fatalf("open %s: %v", routeIndexFile, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gunzip %s: %v", routeIndexFile, err)
	}
	defer gz.Close()
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read %s: %v", routeIndexFile, err)
	}
	index := map[string]map[string]bool{}
	for n, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		name, route, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("%s:%d: want description<TAB>route, got %q", routeIndexFile, n+1, line)
		}
		if index[name] == nil {
			index[name] = map[string]bool{}
		}
		index[name][route] = true
	}
	for _, name := range officialDescriptions {
		if len(index[name]) < 300 {
			t.Fatalf("%s: %s contributes only %d routes; regenerate with scripts/update-github-openapi.sh",
				routeIndexFile, name, len(index[name]))
		}
	}
	return index
}

// describedOutsideDotcom maps a route bleephub serves to the official
// GitHub description that documents it, for routes the dotcom description
// omits: the Enterprise Server admin/staff-tools surface, and Projects
// classic, which GitHub retired from the current descriptions but still
// describes in ghes-3.13 and ghes-2.22. The citation is checked, not
// trusted: TestRouteAllowlistCitationsHold fails if the named description
// does not carry the route.
var describedOutsideDotcom = map[string]string{
	"DELETE /admin/hooks/{}":                                                           "ghes-3.21",
	"DELETE /admin/keys/{}":                                                            "ghes-3.21",
	"DELETE /admin/tokens/{}":                                                          "ghes-3.21",
	"DELETE /admin/users/{}/authorizations":                                            "ghes-3.21",
	"GET /admin/hooks":                                                                 "ghes-3.21",
	"GET /admin/hooks/{}":                                                              "ghes-3.21",
	"GET /admin/keys":                                                                  "ghes-3.21",
	"GET /admin/tokens":                                                                "ghes-3.21",
	"PATCH /admin/hooks/{}":                                                            "ghes-3.21",
	"POST /admin/hooks":                                                                "ghes-3.21",
	"POST /admin/hooks/{}/pings":                                                       "ghes-3.21",
	"POST /admin/users/{}/authorizations":                                              "ghes-3.21",
	"DELETE /admin/pre-receive-environments/{}":                                        "ghes-3.21",
	"DELETE /admin/pre-receive-hooks/{}":                                               "ghes-3.21",
	"DELETE /orgs/{}/pre-receive-hooks/{}":                                             "ghes-3.21",
	"DELETE /repos/{}/{}/pre-receive-hooks/{}":                                         "ghes-3.21",
	"GET /admin/pre-receive-environments":                                              "ghes-3.21",
	"GET /admin/pre-receive-environments/{}":                                           "ghes-3.21",
	"GET /admin/pre-receive-environments/{}/downloads/latest":                          "ghes-3.21",
	"GET /admin/pre-receive-hooks":                                                     "ghes-3.21",
	"GET /admin/pre-receive-hooks/{}":                                                  "ghes-3.21",
	"GET /orgs/{}/pre-receive-hooks":                                                   "ghes-3.21",
	"GET /orgs/{}/pre-receive-hooks/{}":                                                "ghes-3.21",
	"GET /repos/{}/{}/pre-receive-hooks":                                               "ghes-3.21",
	"GET /repos/{}/{}/pre-receive-hooks/{}":                                            "ghes-3.21",
	"PATCH /admin/pre-receive-environments/{}":                                         "ghes-3.21",
	"PATCH /admin/pre-receive-hooks/{}":                                                "ghes-3.21",
	"PATCH /orgs/{}/pre-receive-hooks/{}":                                              "ghes-3.21",
	"PATCH /repos/{}/{}/pre-receive-hooks/{}":                                          "ghes-3.21",
	"POST /admin/pre-receive-environments":                                             "ghes-3.21",
	"POST /admin/pre-receive-environments/{}/downloads":                                "ghes-3.21",
	"POST /admin/pre-receive-hooks":                                                    "ghes-3.21",
	"GET /organizations/{}/custom_roles":                                               "ghec",
	"GET /organizations/{}/org-properties/values":                                      "ghec",
	"PATCH /organizations/{}/org-properties/values":                                    "ghec",
	"DELETE /orgs/{}/announcement":                                                     "ghec",
	"GET /orgs/{}/announcement":                                                        "ghec",
	"PATCH /orgs/{}/announcement":                                                      "ghec",
	"DELETE /orgs/{}/custom-repository-roles/{}":                                       "ghec",
	"GET /orgs/{}/custom-repository-roles":                                             "ghec",
	"GET /orgs/{}/custom-repository-roles/{}":                                          "ghec",
	"PATCH /orgs/{}/custom-repository-roles/{}":                                        "ghec",
	"POST /orgs/{}/custom-repository-roles":                                            "ghec",
	"DELETE /orgs/{}/custom_roles/{}":                                                  "ghec",
	"GET /orgs/{}/custom_roles/{}":                                                     "ghec",
	"PATCH /orgs/{}/custom_roles/{}":                                                   "ghec",
	"POST /orgs/{}/custom_roles":                                                       "ghec",
	"GET /orgs/{}/fine_grained_permissions":                                            "ghec",
	"GET /orgs/{}/organization-fine-grained-permissions":                               "ghec",
	"GET /orgs/{}/repository-fine-grained-permissions":                                 "ghec",
	"DELETE /orgs/{}/organization-roles/{}":                                            "ghec",
	"PATCH /orgs/{}/organization-roles/{}":                                             "ghec",
	"POST /orgs/{}/organization-roles":                                                 "ghec",
	"DELETE /orgs/{}/credential-authorizations/{}":                                     "ghec",
	"GET /orgs/{}/credential-authorizations":                                           "ghec",
	"GET /orgs/{}/settings/billing/advanced-security":                                  "ghec",
	"GET /orgs/{}/bypass-requests/push-rules":                                          "ghec",
	"GET /orgs/{}/bypass-requests/secret-scanning":                                     "ghec",
	"GET /orgs/{}/dismissal-requests/code-scanning":                                    "ghec",
	"GET /orgs/{}/dismissal-requests/dependabot":                                       "ghec",
	"GET /orgs/{}/dismissal-requests/secret-scanning":                                  "ghec",
	"GET /repos/{}/{}/bypass-requests/push-rules":                                      "ghec",
	"GET /repos/{}/{}/bypass-requests/push-rules/{}":                                   "ghec",
	"GET /repos/{}/{}/bypass-requests/secret-scanning":                                 "ghec",
	"GET /repos/{}/{}/bypass-requests/secret-scanning/{}":                              "ghec",
	"PATCH /repos/{}/{}/bypass-requests/secret-scanning/{}":                            "ghec",
	"DELETE /repos/{}/{}/bypass-responses/secret-scanning/{}":                          "ghec",
	"GET /repos/{}/{}/dismissal-requests/code-scanning":                                "ghec",
	"GET /repos/{}/{}/dismissal-requests/code-scanning/{}":                             "ghec",
	"PATCH /repos/{}/{}/dismissal-requests/code-scanning/{}":                           "ghec",
	"GET /repos/{}/{}/dismissal-requests/dependabot":                                   "ghec",
	"GET /repos/{}/{}/dismissal-requests/dependabot/{}":                                "ghec",
	"POST /repos/{}/{}/dismissal-requests/dependabot/{}":                               "ghec",
	"PATCH /repos/{}/{}/dismissal-requests/dependabot/{}":                              "ghec",
	"DELETE /repos/{}/{}/dismissal-requests/dependabot/{}":                             "ghec",
	"GET /repos/{}/{}/dismissal-requests/secret-scanning":                              "ghec",
	"GET /repos/{}/{}/dismissal-requests/secret-scanning/{}":                           "ghec",
	"PATCH /repos/{}/{}/dismissal-requests/secret-scanning/{}":                         "ghec",
	"GET /orgs/{}/external-group/{}":                                                   "ghec",
	"GET /orgs/{}/external-groups":                                                     "ghec",
	"GET /orgs/{}/team-sync/groups":                                                    "ghec",
	"DELETE /orgs/{}/teams/{}/external-groups":                                         "ghec",
	"GET /orgs/{}/teams/{}/external-groups":                                            "ghec",
	"PATCH /orgs/{}/teams/{}/external-groups":                                          "ghec",
	"GET /orgs/{}/teams/{}/team-sync/group-mappings":                                   "ghec",
	"PATCH /orgs/{}/teams/{}/team-sync/group-mappings":                                 "ghec",
	"GET /teams/{}/team-sync/group-mappings":                                           "ghec",
	"PATCH /teams/{}/team-sync/group-mappings":                                         "ghec",
	"DELETE /scim/v2/organizations/{}/Users/{}":                                        "ghec",
	"GET /scim/v2/organizations/{}/Users":                                              "ghec",
	"GET /scim/v2/organizations/{}/Users/{}":                                           "ghec",
	"PATCH /scim/v2/organizations/{}/Users/{}":                                         "ghec",
	"POST /scim/v2/organizations/{}/Users":                                             "ghec",
	"PUT /scim/v2/organizations/{}/Users/{}":                                           "ghec",
	"DELETE /repos/{}/{}/lfs":                                                          "ghec",
	"PUT /repos/{}/{}/lfs":                                                             "ghec",
	"POST /admin/organizations":                                                        "ghes-3.21",
	"POST /admin/users":                                                                "ghes-3.21",
	"PATCH /admin/users/{}":                                                            "ghes-3.21",
	"DELETE /admin/users/{}":                                                           "ghes-3.21",
	"PUT /users/{}/site_admin":                                                         "ghes-3.21",
	"DELETE /users/{}/site_admin":                                                      "ghes-3.21",
	"PUT /users/{}/suspended":                                                          "ghes-3.21",
	"DELETE /users/{}/suspended":                                                       "ghes-3.21",
	"GET /orgs/{}/audit-log":                                                           "ghes-3.21",
	"GET /orgs/{}/copilot/metrics":                                                     "api-2022",
	"GET /orgs/{}/team/{}/copilot/metrics":                                             "api-2022",
	"DELETE /enterprise/announcement":                                                  "ghes-3.21",
	"GET /enterprise/announcement":                                                     "ghes-3.21",
	"GET /enterprise/settings/license":                                                 "ghes-3.21",
	"GET /enterprise/stats/all":                                                        "ghes-3.21",
	"GET /enterprise/stats/comments":                                                   "ghes-3.21",
	"GET /enterprise/stats/gists":                                                      "ghes-3.21",
	"GET /enterprise/stats/hooks":                                                      "ghes-3.21",
	"GET /enterprise/stats/issues":                                                     "ghes-3.21",
	"GET /enterprise/stats/milestones":                                                 "ghes-3.21",
	"GET /enterprise/stats/orgs":                                                       "ghes-3.21",
	"GET /enterprise/stats/pages":                                                      "ghes-3.21",
	"GET /enterprise/stats/pulls":                                                      "ghes-3.21",
	"GET /enterprise/stats/repos":                                                      "ghes-3.21",
	"GET /enterprise/stats/security-products":                                          "ghes-3.21",
	"GET /enterprise/stats/users":                                                      "ghes-3.21",
	"PATCH /enterprise/announcement":                                                   "ghes-3.21",
	"DELETE /enterprises/{}/actions/hosted-runners/images/custom/{}":                   "ghec",
	"DELETE /enterprises/{}/actions/hosted-runners/images/custom/{}/versions/{}":       "ghec",
	"DELETE /enterprises/{}/actions/hosted-runners/{}":                                 "ghec",
	"GET /enterprises/{}/actions/hosted-runners":                                       "ghec",
	"GET /enterprises/{}/actions/hosted-runners/images/custom":                         "ghec",
	"GET /enterprises/{}/actions/hosted-runners/images/custom/{}":                      "ghec",
	"GET /enterprises/{}/actions/hosted-runners/images/custom/{}/versions":             "ghec",
	"GET /enterprises/{}/actions/hosted-runners/images/custom/{}/versions/{}":          "ghec",
	"GET /enterprises/{}/actions/hosted-runners/images/github-owned":                   "ghec",
	"GET /enterprises/{}/actions/hosted-runners/images/partner":                        "ghec",
	"GET /enterprises/{}/actions/hosted-runners/limits":                                "ghec",
	"GET /enterprises/{}/actions/hosted-runners/machine-sizes":                         "ghec",
	"GET /enterprises/{}/actions/hosted-runners/platforms":                             "ghec",
	"GET /enterprises/{}/actions/hosted-runners/{}":                                    "ghec",
	"PATCH /enterprises/{}/actions/hosted-runners/{}":                                  "ghec",
	"POST /enterprises/{}/actions/hosted-runners":                                      "ghec",
	"DELETE /enterprises/{}/actions/permissions/organizations/{}":                      "ghec",
	"GET /enterprises/{}/actions/cache/usage":                                          "ghec",
	"GET /enterprises/{}/actions/permissions":                                          "ghec",
	"GET /enterprises/{}/actions/permissions/artifact-and-log-retention":               "ghec",
	"GET /enterprises/{}/actions/permissions/fork-pr-contributor-approval":             "ghec",
	"GET /enterprises/{}/actions/permissions/fork-pr-workflows-private-repos":          "ghec",
	"GET /enterprises/{}/actions/permissions/organizations":                            "ghec",
	"GET /enterprises/{}/actions/permissions/selected-actions":                         "ghec",
	"GET /enterprises/{}/actions/permissions/self-hosted-runners":                      "ghec",
	"GET /enterprises/{}/actions/permissions/workflow":                                 "ghec",
	"PUT /enterprises/{}/actions/oidc/customization/issuer":                            "ghec",
	"PUT /enterprises/{}/actions/permissions":                                          "ghec",
	"PUT /enterprises/{}/actions/permissions/artifact-and-log-retention":               "ghec",
	"PUT /enterprises/{}/actions/permissions/fork-pr-contributor-approval":             "ghec",
	"PUT /enterprises/{}/actions/permissions/fork-pr-workflows-private-repos":          "ghec",
	"PUT /enterprises/{}/actions/permissions/organizations":                            "ghec",
	"PUT /enterprises/{}/actions/permissions/organizations/{}":                         "ghec",
	"PUT /enterprises/{}/actions/permissions/selected-actions":                         "ghec",
	"PUT /enterprises/{}/actions/permissions/self-hosted-runners":                      "ghec",
	"PUT /enterprises/{}/actions/permissions/workflow":                                 "ghec",
	"DELETE /enterprises/{}/actions/runner-groups/{}":                                  "ghec",
	"DELETE /enterprises/{}/actions/runner-groups/{}/organizations/{}":                 "ghec",
	"DELETE /enterprises/{}/actions/runner-groups/{}/runners/{}":                       "ghec",
	"GET /enterprises/{}/actions/runner-groups":                                        "ghec",
	"GET /enterprises/{}/actions/runner-groups/{}":                                     "ghec",
	"GET /enterprises/{}/actions/runner-groups/{}/organizations":                       "ghec",
	"GET /enterprises/{}/actions/runner-groups/{}/runners":                             "ghec",
	"PATCH /enterprises/{}/actions/runner-groups/{}":                                   "ghec",
	"POST /enterprises/{}/actions/runner-groups":                                       "ghec",
	"PUT /enterprises/{}/actions/runner-groups/{}/organizations":                       "ghec",
	"PUT /enterprises/{}/actions/runner-groups/{}/organizations/{}":                    "ghec",
	"PUT /enterprises/{}/actions/runner-groups/{}/runners":                             "ghec",
	"PUT /enterprises/{}/actions/runner-groups/{}/runners/{}":                          "ghec",
	"DELETE /enterprises/{}/actions/runners/{}":                                        "ghec",
	"DELETE /enterprises/{}/actions/runners/{}/labels":                                 "ghec",
	"DELETE /enterprises/{}/actions/runners/{}/labels/{}":                              "ghec",
	"GET /enterprises/{}/actions/runners":                                              "ghec",
	"GET /enterprises/{}/actions/runners/downloads":                                    "ghec",
	"GET /enterprises/{}/actions/runners/{}":                                           "ghec",
	"GET /enterprises/{}/actions/runners/{}/labels":                                    "ghec",
	"POST /enterprises/{}/actions/runners/generate-jitconfig":                          "ghec",
	"POST /enterprises/{}/actions/runners/registration-token":                          "ghec",
	"POST /enterprises/{}/actions/runners/remove-token":                                "ghec",
	"POST /enterprises/{}/actions/runners/{}/labels":                                   "ghec",
	"PUT /enterprises/{}/actions/runners/{}/labels":                                    "ghec",
	"DELETE /enterprises/{}/announcement":                                              "ghec",
	"DELETE /enterprises/{}/apps/organizations/{}/installations/{}":                    "ghec",
	"DELETE /enterprises/{}/audit-log/streams/{}":                                      "ghec",
	"DELETE /enterprises/{}/copilot/billing/selected_enterprise_teams":                 "ghec",
	"DELETE /enterprises/{}/copilot/billing/selected_users":                            "ghec",
	"DELETE /enterprises/{}/copilot/custom-agents/source":                              "ghec",
	"DELETE /enterprises/{}/enterprise-roles/teams/{}":                                 "ghec",
	"DELETE /enterprises/{}/enterprise-roles/teams/{}/{}":                              "ghec",
	"DELETE /enterprises/{}/enterprise-roles/users/{}":                                 "ghec",
	"DELETE /enterprises/{}/enterprise-roles/users/{}/{}":                              "ghec",
	"DELETE /enterprises/{}/visual-studio-subscriptions/{}":                            "ghec",
	"DELETE /enterprises/{}/settings/billing/budgets/{}":                               "ghec",
	"DELETE /enterprises/{}/settings/billing/cost-centers/{}":                          "ghec",
	"DELETE /enterprises/{}/settings/billing/cost-centers/{}/resource":                 "ghec",
	"DELETE /enterprises/{}/network-configurations/{}":                                 "ghec",
	"DELETE /enterprises/{}/org-properties/schema/{}":                                  "ghec",
	"DELETE /enterprises/{}/properties/schema/{}":                                      "ghec",
	"DELETE /enterprises/{}/rulesets/{}":                                               "ghec",
	"DELETE /enterprises/{}/secret-scanning/custom-patterns":                           "ghec",
	"GET /enterprises/{}/announcement":                                                 "ghec",
	"GET /enterprises/{}/apps/installable_organizations":                               "ghec",
	"GET /enterprises/{}/apps/installable_organizations/{}/accessible_repositories":    "ghec",
	"GET /enterprises/{}/apps/organizations/{}/installations":                          "ghec",
	"GET /enterprises/{}/apps/organizations/{}/installations/{}/repositories":          "ghec",
	"GET /enterprises/{}/audit-log":                                                    "ghec",
	"GET /enterprises/{}/audit-log/stream-key":                                         "ghec",
	"GET /enterprises/{}/audit-log/streams":                                            "ghec",
	"GET /enterprises/{}/audit-log/streams/{}":                                         "ghec",
	"GET /enterprises/{}/settings/billing/advanced-security":                           "ghec",
	"GET /enterprises/{}/settings/billing/ai_credit/usage":                             "ghec",
	"GET /enterprises/{}/settings/billing/budgets":                                     "ghec",
	"GET /enterprises/{}/settings/billing/budgets/{}":                                  "ghec",
	"GET /enterprises/{}/settings/billing/budgets/{}/user-states":                      "ghec",
	"GET /enterprises/{}/settings/billing/cost-centers":                                "ghec",
	"GET /enterprises/{}/settings/billing/cost-centers/{}":                             "ghec",
	"GET /enterprises/{}/settings/billing/premium_request/usage":                       "ghec",
	"GET /enterprises/{}/settings/billing/reports":                                     "ghec",
	"GET /enterprises/{}/settings/billing/reports/{}":                                  "ghec",
	"GET /enterprises/{}/settings/billing/usage":                                       "ghec",
	"GET /enterprises/{}/settings/billing/usage/summary":                               "ghec",
	"PATCH /enterprises/{}/settings/billing/budgets/{}":                                "ghec",
	"PATCH /enterprises/{}/settings/billing/cost-centers/{}":                           "ghec",
	"POST /enterprises/{}/settings/billing/budgets":                                    "ghec",
	"POST /enterprises/{}/settings/billing/cost-centers":                               "ghec",
	"POST /enterprises/{}/settings/billing/cost-centers/{}/resource":                   "ghec",
	"POST /enterprises/{}/settings/billing/reports":                                    "ghec",
	"GET /enterprises/{}/bypass-requests/push-rules":                                   "ghec",
	"GET /enterprises/{}/bypass-requests/secret-scanning":                              "ghec",
	"GET /enterprises/{}/code-scanning/alerts":                                         "ghec",
	"GET /enterprises/{}/code_security_and_analysis":                                   "ghec",
	"GET /enterprises/{}/copilot/billing/seats":                                        "ghec",
	"GET /enterprises/{}/copilot/content_exclusion":                                    "ghec",
	"GET /enterprises/{}/copilot/custom-agents":                                        "ghec",
	"GET /enterprises/{}/copilot/custom-agents/source":                                 "ghec",
	"GET /enterprises/{}/copilot/usage-records":                                        "ghec",
	"GET /enterprises/{}/dismissal-requests/secret-scanning":                           "ghec",
	"GET /enterprises/{}/enterprise-roles":                                             "ghec",
	"GET /enterprises/{}/enterprise-roles/{}":                                          "ghec",
	"GET /enterprises/{}/enterprise-roles/{}/teams":                                    "ghec",
	"GET /enterprises/{}/enterprise-roles/{}/users":                                    "ghec",
	"GET /enterprise-installation/{}/server-statistics":                                "ghec",
	"GET /enterprises/{}/consumed-licenses":                                            "ghec",
	"GET /enterprises/{}/innersource-vulnerabilities/sync/status/{}":                   "ghec",
	"GET /enterprises/{}/installation":                                                 "ghec",
	"GET /enterprises/{}/license-sync-status":                                          "ghec",
	"GET /enterprises/{}/members/{}/copilot":                                           "ghec",
	"GET /enterprises/{}/visual-studio-subscriptions":                                  "ghec",
	"GET /enterprises/{}/network-configurations":                                       "ghec",
	"GET /enterprises/{}/network-configurations/{}":                                    "ghec",
	"GET /enterprises/{}/network-settings/{}":                                          "ghec",
	"GET /enterprises/{}/org-properties/schema":                                        "ghec",
	"GET /enterprises/{}/org-properties/schema/{}":                                     "ghec",
	"GET /enterprises/{}/org-properties/values":                                        "ghec",
	"GET /enterprises/{}/properties/schema":                                            "ghec",
	"GET /enterprises/{}/properties/schema/{}":                                         "ghec",
	"GET /enterprises/{}/rulesets/{}":                                                  "ghec",
	"GET /enterprises/{}/rulesets/{}/history":                                          "ghec",
	"GET /enterprises/{}/rulesets/{}/history/{}":                                       "ghec",
	"GET /enterprises/{}/secret-scanning/alerts":                                       "ghec",
	"GET /enterprises/{}/secret-scanning/custom-patterns":                              "ghec",
	"GET /enterprises/{}/secret-scanning/pattern-configurations":                       "ghec",
	"PATCH /enterprises/{}/announcement":                                               "ghec",
	"PATCH /enterprises/{}/apps/organizations/{}/installations/{}/repositories":        "ghec",
	"PATCH /enterprises/{}/apps/organizations/{}/installations/{}/repositories/add":    "ghec",
	"PATCH /enterprises/{}/apps/organizations/{}/installations/{}/repositories/remove": "ghec",
	"PATCH /enterprises/{}/code_security_and_analysis":                                 "ghec",
	"PATCH /enterprises/{}/network-configurations/{}":                                  "ghec",
	"PATCH /enterprises/{}/org-properties/schema":                                      "ghec",
	"PATCH /enterprises/{}/org-properties/values":                                      "ghec",
	"PATCH /enterprises/{}/properties/schema":                                          "ghec",
	"PATCH /enterprises/{}/secret-scanning/custom-patterns/{}":                         "ghec",
	"PATCH /enterprises/{}/secret-scanning/pattern-configurations":                     "ghec",
	"POST /enterprises/{}/access-restrictions/disable":                                 "ghec",
	"POST /enterprises/{}/access-restrictions/enable":                                  "ghec",
	"POST /enterprises/{}/apps/organizations/{}/installations":                         "ghec",
	"POST /enterprises/{}/audit-log/streams":                                           "ghec",
	"POST /enterprises/{}/copilot/billing/selected_enterprise_teams":                   "ghec",
	"POST /enterprises/{}/copilot/billing/selected_users":                              "ghec",
	"POST /enterprises/{}/credential-authorizations/revoke-all":                        "ghec",
	"POST /enterprises/{}/credential-authorizations/revoke-credential-type":            "ghec",
	"POST /enterprises/{}/credential-authorizations/{}/revoke":                         "ghec",
	"POST /enterprises/{}/credential-authorizations/{}/revoke-credential-type":         "ghec",
	"POST /enterprises/{}/innersource-vulnerabilities/sync":                            "ghec",
	"POST /enterprises/{}/network-configurations":                                      "ghec",
	"POST /enterprises/{}/rulesets":                                                    "ghec",
	"POST /enterprises/{}/secret-scanning/custom-patterns":                             "ghec",
	"POST /enterprises/{}/{}/{}":                                                       "ghec",
	"PUT /enterprises/{}/audit-log/streams/{}":                                         "ghec",
	"PUT /enterprises/{}/copilot/content_exclusion":                                    "ghec",
	"PUT /enterprises/{}/copilot/custom-agents/source":                                 "ghec",
	"PUT /enterprises/{}/enterprise-roles/teams/{}/{}":                                 "ghec",
	"PUT /enterprises/{}/enterprise-roles/users/{}/{}":                                 "ghec",
	"PUT /enterprises/{}/visual-studio-subscriptions/{}":                               "ghec",
	"PUT /enterprises/{}/org-properties/schema/{}":                                     "ghec",
	"PUT /enterprises/{}/properties/schema/organizations/{}/{}/promote":                "ghec",
	"PUT /enterprises/{}/properties/schema/{}":                                         "ghec",
	"PUT /enterprises/{}/rulesets/{}":                                                  "ghec",
	"DELETE /scim/v2/enterprises/{}/Groups/{}":                                         "ghec",
	"DELETE /scim/v2/enterprises/{}/Users/{}":                                          "ghec",
	"GET /scim/v2/enterprises/{}/Groups":                                               "ghec",
	"GET /scim/v2/enterprises/{}/Groups/{}":                                            "ghec",
	"GET /scim/v2/enterprises/{}/Users":                                                "ghec",
	"GET /scim/v2/enterprises/{}/Users/{}":                                             "ghec",
	"PATCH /scim/v2/enterprises/{}/Groups/{}":                                          "ghec",
	"PATCH /scim/v2/enterprises/{}/Users/{}":                                           "ghec",
	"POST /scim/v2/enterprises/{}/Groups":                                              "ghec",
	"POST /scim/v2/enterprises/{}/Users":                                               "ghec",
	"PUT /scim/v2/enterprises/{}/Groups/{}":                                            "ghec",
	"PUT /scim/v2/enterprises/{}/Users/{}":                                             "ghec",
	"GET /repos/{}/{}/projects":                                                        "ghes-3.13",
	"POST /repos/{}/{}/projects":                                                       "ghes-3.13",
	"GET /projects/{}":                                                                 "ghes-3.13",
	"PATCH /projects/{}":                                                               "ghes-3.13",
	"DELETE /projects/{}":                                                              "ghes-3.13",
	"POST /projects/columns/{}/moves":                                                  "ghes-3.13",
	"POST /projects/columns/cards/{}/moves":                                            "ghes-3.13",
}

// uncitedRoutes must stay empty. A public /api/v3 operation without an
// official GitHub description is an invented API, not an implementation
// backlog item.
var uncitedRoutes = map[string]string{}

const maxUncitedRoutes = 0

// runnerProtocolRoutes are private Actions runner handshake operations. The
// official runner invokes these below the GHES API prefix, but they are not
// GitHub REST operations and therefore do not appear in the REST description.
// Keep this boundary exact and prove it with the official-runner handshake
// suite rather than weakening the REST coverage gate.
var runnerProtocolRoutes = map[string]string{
	"POST /actions/runner-registration": "official actions/runner registration handshake",
}

// TestRouteAllowlistCitationsHold checks the two ledgers against the
// vendored descriptions, so neither can drift into decoration.
func TestRouteAllowlistCitationsHold(t *testing.T) {
	dotcom := loadGitHubOperations(t)
	index := loadOfficialRouteIndex(t)

	for route, description := range describedOutsideDotcom {
		if dotcom[route] {
			t.Errorf("describedOutsideDotcom[%q]: the dotcom description already documents this route; drop the entry", route)
			continue
		}
		if index[description] == nil {
			t.Errorf("describedOutsideDotcom[%q]: cites %q, which is not a vendored description (%s)",
				route, description, strings.Join(officialDescriptions, ", "))
			continue
		}
		if !index[description][route] {
			t.Errorf("describedOutsideDotcom[%q]: %s does not document this route; the citation is wrong",
				route, description)
		}
	}

	for route := range uncitedRoutes {
		if _, both := describedOutsideDotcom[route]; both {
			t.Errorf("%q is in both describedOutsideDotcom and uncitedRoutes", route)
		}
		if dotcom[route] {
			t.Errorf("uncitedRoutes[%q]: the dotcom description documents this route; drop the entry", route)
			continue
		}
		var found []string
		for _, name := range officialDescriptions {
			if index[name][route] {
				found = append(found, name)
			}
		}
		if len(found) > 0 {
			sort.Strings(found)
			t.Errorf("uncitedRoutes[%q]: %s documents it; move it to describedOutsideDotcom",
				route, strings.Join(found, ", "))
		}
	}

	if len(uncitedRoutes) > maxUncitedRoutes {
		t.Errorf("uncitedRoutes has %d entries, ratchet allows %d; a route GitHub does not have is a defect, not an allowance",
			len(uncitedRoutes), maxUncitedRoutes)
	}
}

// dispatchRoutes are real GitHub sub-resource paths served through a single
// two-/three-segment wildcard handler because Go 1.22's ServeMux rejects
// registering a literal and a wildcard that overlap at the same position
// (e.g. /pulls/comments/{id} vs /pulls/{number}/comments). The wildcard fans
// out to the real GitHub paths listed; it is a routing implementation detail,
// not an invented path. Keyed by the normalized wildcard pattern.
var dispatchRoutes = map[string]string{
	"DELETE /repos/{}/{}/issues/{}/{}":         "→ DELETE /repos/{}/{}/issues/{}/labels/{} (remove a label)",
	"GET /repos/{}/{}/issues/{}/{}":            "→ GET /repos/{}/{}/issues/comments/{comment_id}, /issues/{number}/reactions, or /issues/events/{event_id}",
	"GET /repos/{}/{}/issues/{}/{}/{}":         "→ GET /repos/{}/{}/issues/comments/{comment_id}/reactions or /issues/{number}/dependencies/blocked_by",
	"DELETE /repos/{}/{}/issues/{}/{}/{}":      "→ DELETE /repos/{}/{}/issues/{number}/reactions/{reaction_id} (or /comments/{comment_id}/reactions/{reaction_id})",
	"GET /repos/{}/{}/git/refs/{}":             "→ GET /repos/{}/{}/git/refs/{} (single ref lookup)",
	"GET /repos/{}/{}/pulls/{}/{}":             "→ GET /repos/{}/{}/pulls/comments/{} (a review comment)",
	"PATCH /repos/{}/{}/pulls/{}/{}":           "→ PATCH /repos/{}/{}/pulls/comments/{} (edit a review comment)",
	"DELETE /repos/{}/{}/pulls/{}/{}":          "→ DELETE /repos/{}/{}/pulls/comments/{} (delete a review comment)",
	"GET /repos/{}/{}/pulls/{}/{}/{}":          "→ GET /repos/{}/{}/pulls/{number}/reviews/{review_id} or /pulls/comments/{comment_id}/reactions",
	"POST /repos/{}/{}/pulls/{}/{}/{}":         "→ POST /repos/{}/{}/pulls/comments/{comment_id}/reactions",
	"PUT /repos/{}/{}/pulls/{}/{}/{}":          "→ PUT /repos/{}/{}/pulls/{number}/reviews/{review_id}",
	"DELETE /repos/{}/{}/pulls/{}/{}/{}":       "→ DELETE /repos/{}/{}/pulls/{number}/reviews/{review_id}",
	"GET /repos/{}/{}/releases/{}/{}":          "→ GET /repos/{}/{}/releases/{}/assets (list release assets) or /releases/tags/{tag}",
	"POST /repos/{}/{}/releases/{}/{}":         "→ POST /repos/{}/{}/releases/{}/reactions (react to a release) or /releases/{release_id}/assets",
	"PATCH /repos/{}/{}/releases/{}/{}":        "→ PATCH /repos/{}/{}/releases/assets/{asset_id} (edit a release asset)",
	"DELETE /repos/{}/{}/releases/{}/{}":       "→ DELETE /repos/{}/{}/releases/assets/{asset_id} (delete a release asset)",
	"DELETE /repos/{}/{}/releases/{}/{}/{}":    "→ DELETE /repos/{}/{}/releases/{}/reactions/{} (remove a release reaction)",
	"GET /orgs/{}/rulesets/{}/{}":              "→ GET /orgs/{org}/rulesets/rule-suites/{rule_suite_id} or /orgs/{org}/rulesets/{ruleset_id}/history",
	"GET /repos/{}/{}/rulesets/{}/{}":          "→ GET /repos/{}/{}/rulesets/rule-suites/{rule_suite_id} or /repos/{}/{}/rulesets/{ruleset_id}/history",
	"GET /orgs/{}/rulesets/{}/{}/{}":           "→ GET /orgs/{org}/rulesets/{ruleset_id}/history/{version_id}",
	"GET /projects/{}/{}":                      "→ GET /projects/{project_id}/columns or GET /projects/columns/{column_id} (Projects classic dispatch)",
	"POST /projects/{}/{}":                     "→ POST /projects/{project_id}/columns (Projects classic dispatch)",
	"PATCH /projects/{}/{}":                    "→ PATCH /projects/columns/{column_id} (Projects classic dispatch)",
	"DELETE /projects/{}/{}":                   "→ DELETE /projects/columns/{column_id} (Projects classic dispatch)",
	"GET /projects/columns/{}/{}":              "→ GET /projects/columns/{column_id}/cards or GET /projects/columns/cards/{card_id} (Projects classic dispatch)",
	"POST /projects/columns/{}/{}":             "→ POST /projects/columns/{column_id}/cards (Projects classic dispatch)",
	"PATCH /projects/columns/{}/{}":            "→ PATCH /projects/columns/cards/{card_id} (Projects classic dispatch)",
	"DELETE /projects/columns/{}/{}":           "→ DELETE /projects/columns/cards/{card_id} (Projects classic dispatch)",
	"POST /repos/{}/{}/security-advisories/{}": "→ POST /repos/{}/{}/security-advisories/reports (report vulnerability)",
	"GET /user/codespaces/{}/{}":               "→ GET /user/codespaces/{codespace_name}/machines (conflicts with the literal /user/codespaces/secrets/{secret_name})",
	"GET /user/codespaces/{}/{}/{}":            "→ GET /user/codespaces/{codespace_name}/exports/{export_id} (conflicts with the literal /user/codespaces/secrets/{secret_name}/repositories)",
}

func TestRegisteredAPIv3RoutesExistInGitHubSpec(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	ghOps := loadGitHubOperations(t)

	var offenders []string
	for _, pat := range s.routePatterns {
		method, path, found := strings.Cut(pat, " ")
		if !found {
			continue
		}
		// Only the GitHub-compatible REST surface is validated here.
		// /api/graphql, /_apis (runner protocol), /internal (sim-control),
		// /login (OAuth), and /.well-known are out of scope for the REST spec.
		if !strings.HasPrefix(path, "/api/v3/") {
			continue
		}
		norm := method + " " + normalizePath(strings.TrimPrefix(path, "/api/v3"))
		if ghOps[norm] {
			continue
		}
		if _, ok := describedOutsideDotcom[norm]; ok {
			continue
		}
		if _, ok := uncitedRoutes[norm]; ok {
			continue
		}
		if _, ok := runnerProtocolRoutes[norm]; ok {
			continue
		}
		if _, ok := dispatchRoutes[norm]; ok {
			continue
		}
		offenders = append(offenders, pat+"  (normalized: "+norm+")")
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d /api/v3 route(s) are not real GitHub API paths (invented under the GitHub namespace "+
			"or a parameter/path-shape mismatch). Cite the official description that documents each in "+
			"describedOutsideDotcom, or delete the route:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// TestRegisteredRoutesHaveCompleteFuzzInventory makes route-level coverage a
// set equality rather than a comment above a hand-maintained slice. Every
// registered operation must enter the HTTP fuzz vocabulary, and stale entries
// must be removed. This is the 100% operation-level floor; semantic behavior
// that OpenAPI cannot describe is pinned by focused compatibility vectors.
func TestRegisteredRoutesHaveCompleteFuzzInventory(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	registered := make(map[string]int, len(s.routePatterns))
	for _, pattern := range s.routePatterns {
		_, path, found := strings.Cut(pattern, " ")
		if !found || !(strings.HasPrefix(path, "/api/v3/") || strings.HasPrefix(path, "/manage/v1/")) {
			continue
		}
		registered[pattern]++
	}
	inventory := make(map[string]int, len(fuzzRoutePatterns))
	for _, pattern := range fuzzRoutePatterns {
		_, path, found := strings.Cut(pattern, " ")
		if !found || !(strings.HasPrefix(path, "/api/v3/") || strings.HasPrefix(path, "/manage/v1/")) {
			continue
		}
		inventory[pattern]++
	}

	var missing, stale, duplicates []string
	for pattern, count := range registered {
		if count != 1 {
			duplicates = append(duplicates, fmt.Sprintf("registered %dx: %s", count, pattern))
		}
		if inventory[pattern] == 0 {
			missing = append(missing, pattern)
		}
	}
	for pattern, count := range inventory {
		if count != 1 {
			duplicates = append(duplicates, fmt.Sprintf("inventory %dx: %s", count, pattern))
		}
		if registered[pattern] == 0 {
			stale = append(stale, pattern)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	sort.Strings(duplicates)
	if len(missing) > 0 || len(stale) > 0 || len(duplicates) > 0 {
		t.Fatalf("registered route coverage is not 100%%\nmissing from fuzz inventory (%d):\n  %s\n"+
			"stale fuzz inventory entries (%d):\n  %s\nduplicates (%d):\n  %s",
			len(missing), strings.Join(missing, "\n  "),
			len(stale), strings.Join(stale, "\n  "),
			len(duplicates), strings.Join(duplicates, "\n  "))
	}
	t.Logf("GitHub REST route inventory coverage: %d/%d operations (100%%)", len(inventory), len(registered))
}

func TestFuzzRouteSelectorCanReachEveryRegisteredAPIOperation(t *testing.T) {
	total := len(fuzzRoutePatterns)
	if total <= 256 {
		t.Fatalf("route selector reachability test is no longer exercising the multi-byte case: %d routes", total)
	}
	for want := range fuzzRoutePatterns {
		data := []byte{byte(want >> 8), byte(want)}
		reader := &fuzzReader{b: data}
		if got := reader.pick(total); got != want {
			t.Fatalf("route %d/%d is unreachable: selector returned %d for %v", want, total, got, data)
		}
	}
}

var dispatchCoveredOperations = map[string]bool{
	"DELETE /repos/{}/{}/issues/comments/{}":              true,
	"DELETE /repos/{}/{}/issues/comments/{}/pin":          true,
	"DELETE /repos/{}/{}/issues/{}/issue-field-values/{}": true,
	"DELETE /repos/{}/{}/issues/{}/labels/{}":             true,
	"DELETE /repos/{}/{}/issues/{}/lock":                  true,
	"DELETE /repos/{}/{}/issues/{}/reactions/{}":          true,
	"DELETE /repos/{}/{}/issues/{}/sub_issue":             true,
	"DELETE /repos/{}/{}/pulls/comments/{}":               true,
	"DELETE /repos/{}/{}/pulls/{}/reviews/{}":             true,
	"DELETE /repos/{}/{}/releases/assets/{}":              true,
	"DELETE /repos/{}/{}/releases/{}/reactions/{}":        true,
	"GET /orgs/{}/rulesets/rule-suites/{}":                true,
	"GET /orgs/{}/rulesets/{}/history":                    true,
	"GET /orgs/{}/rulesets/{}/history/{}":                 true,
	"GET /repos/{}/{}/issues/comments/{}":                 true,
	"GET /repos/{}/{}/issues/comments/{}/reactions":       true,
	"GET /repos/{}/{}/issues/events/{}":                   true,
	"GET /repos/{}/{}/issues/{}/assignees/{}":             true,
	"GET /repos/{}/{}/issues/{}/comments":                 true,
	"GET /repos/{}/{}/issues/{}/dependencies/blocked_by":  true,
	"GET /repos/{}/{}/issues/{}/dependencies/blocking":    true,
	"GET /repos/{}/{}/issues/{}/events":                   true,
	"GET /repos/{}/{}/issues/{}/issue-field-values":       true,
	"GET /repos/{}/{}/issues/{}/labels":                   true,
	"GET /repos/{}/{}/issues/{}/parent":                   true,
	"GET /repos/{}/{}/issues/{}/reactions":                true,
	"GET /repos/{}/{}/issues/{}/sub_issues":               true,
	"GET /repos/{}/{}/issues/{}/timeline":                 true,
	"GET /repos/{}/{}/pulls/comments/{}":                  true,
	"GET /repos/{}/{}/pulls/comments/{}/reactions":        true,
	"GET /repos/{}/{}/pulls/{}/files":                     true,
	"GET /repos/{}/{}/pulls/{}/reviews/{}":                true,
	"GET /repos/{}/{}/releases/assets/{}":                 true,
	"GET /repos/{}/{}/releases/tags/{}":                   true,
	"GET /repos/{}/{}/releases/{}/assets":                 true,
	"GET /repos/{}/{}/releases/{}/reactions":              true,
	"GET /repos/{}/{}/rulesets/rule-suites/{}":            true,
	"GET /repos/{}/{}/rulesets/{}/history":                true,
	"GET /user/codespaces/{}/exports/{}":                  true,
	"GET /user/codespaces/{}/machines":                    true,
	"PATCH /repos/{}/{}/pulls/comments/{}":                true,
	"PATCH /repos/{}/{}/releases/assets/{}":               true,
	"POST /repos/{}/{}/pulls/comments/{}/reactions":       true,
	"POST /repos/{}/{}/releases/{}/assets":                true,
	"POST /repos/{}/{}/releases/{}/reactions":             true,
	"POST /repos/{}/{}/security-advisories/reports":       true,
	"PUT /repos/{}/{}/pulls/{}/reviews/{}":                true,
}

// TestEveryDocumentedGitHubRESTOperationIsRegistered is the reverse half of
// TestRegisteredAPIv3RoutesExistInGitHubSpec. Together they make API coverage a
// two-way set comparison: Bleephub neither invents GitHub routes nor silently
// omits operations from the pinned dotcom description.
func TestEveryDocumentedGitHubRESTOperationIsRegistered(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	registered := map[string]bool{}
	for _, pattern := range s.routePatterns {
		method, routePath, found := strings.Cut(pattern, " ")
		if !found || !strings.HasPrefix(routePath, "/api/v3/") {
			continue
		}
		operation := method + " " + normalizePath(strings.TrimPrefix(routePath, "/api/v3"))
		registered[operation] = true
	}
	documented := loadGitHubOperations(t)
	var missing []string
	for operation := range documented {
		if !registered[operation] && !dispatchCoveredOperations[operation] {
			missing = append(missing, operation)
		}
	}
	for operation := range dispatchCoveredOperations {
		if !documented[operation] {
			t.Errorf("dispatch coverage entry is not in the pinned GitHub definition: %s", operation)
		}
		if registered[operation] {
			t.Errorf("dispatch coverage entry now has a direct route and must be removed: %s", operation)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("GitHub REST definition coverage is not 100%%: %d documented operation(s) are not registered:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	t.Logf("GitHub REST definition coverage: %d/%d operations (100%%)", len(documented), len(documented))
}

// TestRegisteredAPIRouteSnapshot makes the reviewable parity inventory derive
// from the route table the server actually registers. Source-code regexes miss
// registrations assembled from loops or variables, which previously made the
// generated inventory claim that every Copilot Spaces operation was absent
// while the runtime route-set test above correctly reported 100% coverage.
func TestRegisteredAPIRouteSnapshot(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	var routes []string
	for _, pattern := range s.routePatterns {
		_, path, found := strings.Cut(pattern, " ")
		if found && strings.HasPrefix(path, "/api/v3/") {
			routes = append(routes, pattern)
		}
	}
	sort.Strings(routes)
	body := []byte(strings.Join(routes, "\n") + "\n")
	if os.Getenv("BLEEPHUB_UPDATE_REST_ROUTE_SNAPSHOT") == "1" {
		if err := os.WriteFile(registeredRouteSnapshotFile, body, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s with %d runtime routes", registeredRouteSnapshotFile, len(routes))
		return
	}
	assertFileBytes(t, registeredRouteSnapshotFile, body,
		"the runtime REST route table changed; review it and run BLEEPHUB_UPDATE_REST_ROUTE_SNAPSHOT=1 go test ./internal/server -run TestRegisteredAPIRouteSnapshot")
}

func TestRegisteredGHESManageRoutesMatchOfficialDescriptionAndSnapshot(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	official := loadOfficialRouteIndex(t)["ghes-3.21"]
	var routes []string
	for _, pattern := range s.routePatterns {
		method, path, found := strings.Cut(pattern, " ")
		if !found || !strings.HasPrefix(path, "/manage/v1/") {
			continue
		}
		if normalized := method + " " + normalizePath(path); !official[normalized] {
			t.Errorf("GHES Manage route is not in the pinned official description: %s", normalized)
		}
		routes = append(routes, pattern)
	}
	sort.Strings(routes)
	body := []byte(strings.Join(routes, "\n") + "\n")
	if os.Getenv("BLEEPHUB_UPDATE_REST_ROUTE_SNAPSHOT") == "1" {
		if err := os.WriteFile(registeredManageSnapshotFile, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	assertFileBytes(t, registeredManageSnapshotFile, body,
		"the GHES Manage route table changed; review it and update the REST route snapshots")
}

// TestUncitedRoutesAreStillRegistered keeps the defect ledger from
// outliving the defects. An entry for a route nobody serves any more is a
// stale excuse, and it would silently re-authorise the route if it came
// back.
func TestUncitedRoutesAreStillRegistered(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	registered := map[string]bool{}
	for _, pat := range s.routePatterns {
		method, path, found := strings.Cut(pat, " ")
		if !found || !strings.HasPrefix(path, "/api/v3/") {
			continue
		}
		registered[method+" "+normalizePath(strings.TrimPrefix(path, "/api/v3"))] = true
	}
	var stale []string
	for route := range uncitedRoutes {
		if !registered[route] {
			stale = append(stale, route)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("%d uncitedRoutes entr(ies) name a route that is no longer registered; delete them and lower maxUncitedRoutes:\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
	for route := range runnerProtocolRoutes {
		if !registered[route] {
			t.Errorf("runner protocol compatibility route is no longer registered: %s", route)
		}
	}
}

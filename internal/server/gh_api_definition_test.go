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
	vendoredSpecFile    = "testdata/github-openapi.json.gz"
	vendoredSpecVersion = "testdata/github-openapi.VERSION"
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
var officialDescriptions = []string{"ghec", "ghes-3.21", "ghes-3.13", "ghes-2.22"}

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
	"POST /admin/organizations":             "ghes-3.21",
	"POST /admin/users":                     "ghes-3.21",
	"PATCH /admin/users/{}":                 "ghes-3.21",
	"DELETE /admin/users/{}":                "ghes-3.21",
	"PUT /users/{}/site_admin":              "ghes-3.21",
	"DELETE /users/{}/site_admin":           "ghes-3.21",
	"PUT /users/{}/suspended":               "ghes-3.21",
	"DELETE /users/{}/suspended":            "ghes-3.21",
	"GET /orgs/{}/audit-log":                "ghes-3.21",
	"GET /repos/{}/{}/projects":             "ghes-3.13",
	"POST /repos/{}/{}/projects":            "ghes-3.13",
	"GET /projects/{}":                      "ghes-3.13",
	"PATCH /projects/{}":                    "ghes-3.13",
	"DELETE /projects/{}":                   "ghes-3.13",
	"POST /projects/columns/{}/moves":       "ghes-3.13",
	"POST /projects/columns/cards/{}/moves": "ghes-3.13",
}

// uncitedRoutes are routes bleephub registers under /api/v3 that NO
// official GitHub description carries — not the dotcom one, not
// Enterprise Cloud, not Enterprise Server 3.21, 3.13 or 2.22. They were
// previously asserted to be "real GitHub (GHES) endpoints"; nothing
// supports that, so they are recorded here as outstanding parity defects
// with the correction where one is known, and the value names the file
// that registers the route. This ledger may only shrink: fixing an entry
// means changing or deleting the route, never rewording the excuse.
// TestRouteAllowlistCitationsHold fails if an entry turns out to be
// described after all, so it has to be promoted rather than left here.
var uncitedRoutes = map[string]string{
	"POST /actions/runner-registration": "auth.go — used by actions/runner config.sh against GHES; not in any description",

	"GET /repos/{}/{}/git/refs": "gh_repos_git.go — GitHub lists refs at GET /repos/{}/{}/git/matching-refs/{ref}",

	"GET /repos/{}/{}/branches/{}/protection/allow_deletions":        "gh_branch_protection.go — allow_deletions is a field of the protection object, not an endpoint",
	"PUT /repos/{}/{}/branches/{}/protection/allow_deletions":        "gh_branch_protection.go — allow_deletions is a field of the protection object, not an endpoint",
	"DELETE /repos/{}/{}/branches/{}/protection/allow_deletions":     "gh_branch_protection.go — allow_deletions is a field of the protection object, not an endpoint",
	"GET /repos/{}/{}/branches/{}/protection/allow_force_pushes":     "gh_branch_protection.go — allow_force_pushes is a field of the protection object, not an endpoint",
	"PUT /repos/{}/{}/branches/{}/protection/allow_force_pushes":     "gh_branch_protection.go — allow_force_pushes is a field of the protection object, not an endpoint",
	"DELETE /repos/{}/{}/branches/{}/protection/allow_force_pushes":  "gh_branch_protection.go — allow_force_pushes is a field of the protection object, not an endpoint",
	"PUT /repos/{}/{}/branches/{}/protection/required_status_checks": "gh_branch_protection.go — GitHub updates status-check protection with PATCH, not PUT",
	"PUT /repos/{}/{}/branches/{}/protection/restrictions":           "gh_branch_protection.go — GitHub has GET and DELETE on /restrictions; PUT only on its apps/teams/users children",

	"PATCH /repos/{}/{}/secret-scanning/alerts": "gh_secret_scanning.go — GitHub updates one alert at a time: PATCH /repos/{}/{}/secret-scanning/alerts/{alert_number}",

	"GET /orgs/{}/migrations/{}/repos/{}/lock": "gh_migrations.go — GitHub only has DELETE on the migration repo lock",

	"GET /repos/{}/{}/pages/deployments/{}/status": "gh_pages_deployments.go — the deployment status is GET /repos/{}/{}/pages/deployments/{}, already registered alongside this alias",

	"GET /orgs/{}/actions/cache/retention-limit": "gh_actions_permissions.go — GitHub scopes the org cache limits under /organizations/{org_id}/actions/cache/retention-limit",
	"PUT /orgs/{}/actions/cache/retention-limit": "gh_actions_permissions.go — GitHub scopes the org cache limits under /organizations/{org_id}/actions/cache/retention-limit",
	"GET /orgs/{}/actions/cache/storage-limit":   "gh_actions_permissions.go — GitHub scopes the org cache limits under /organizations/{org_id}/actions/cache/storage-limit",
	"PUT /orgs/{}/actions/cache/storage-limit":   "gh_actions_permissions.go — GitHub scopes the org cache limits under /organizations/{org_id}/actions/cache/storage-limit",

	"GET /repos/{}/{}/codespaces/{}":        "gh_codespaces.go — GitHub addresses a codespace by name under /user/codespaces/{codespace_name}",
	"DELETE /repos/{}/{}/codespaces/{}":     "gh_codespaces.go — GitHub addresses a codespace by name under /user/codespaces/{codespace_name}",
	"POST /repos/{}/{}/codespaces/{}/start": "gh_codespaces.go — GitHub starts a codespace at POST /user/codespaces/{codespace_name}/start",
	"POST /repos/{}/{}/codespaces/{}/stop":  "gh_codespaces.go — GitHub stops a codespace at POST /user/codespaces/{codespace_name}/stop",

	"GET /repos/{}/{}/packages":                            "gh_packages.go — the Packages API is scoped to a user or an org, never a repo",
	"GET /repos/{}/{}/packages/{}/{}":                      "gh_packages.go — the Packages API is scoped to a user or an org, never a repo",
	"DELETE /repos/{}/{}/packages/{}/{}":                   "gh_packages.go — the Packages API is scoped to a user or an org, never a repo",
	"GET /repos/{}/{}/packages/{}/{}/versions":             "gh_packages.go — the Packages API is scoped to a user or an org, never a repo",
	"GET /repos/{}/{}/packages/{}/{}/versions/{}":          "gh_packages.go — the Packages API is scoped to a user or an org, never a repo",
	"DELETE /repos/{}/{}/packages/{}/{}/versions/{}":       "gh_packages.go — the Packages API is scoped to a user or an org, never a repo",
	"GET /repos/{}/{}/packages/{}/{}/versions/{}/files":    "gh_packages.go — no description declares a files sub-resource on a package version",
	"GET /repos/{}/{}/packages/{}/{}/versions/{}/files/{}": "gh_packages.go — no description declares a files sub-resource on a package version",
	"GET /users/{}/packages/{}/{}/versions/{}/files":       "gh_packages.go — no description declares a files sub-resource on a package version",
	"GET /users/{}/packages/{}/{}/versions/{}/files/{}":    "gh_packages.go — no description declares a files sub-resource on a package version",
	"GET /orgs/{}/packages/{}/{}/versions/{}/files":        "gh_packages.go — no description declares a files sub-resource on a package version",
	"GET /orgs/{}/packages/{}/{}/versions/{}/files/{}":     "gh_packages.go — no description declares a files sub-resource on a package version",
}

// maxUncitedRoutes ratchets the ledger above. It may be lowered, never
// raised: a new uncited route means a route that GitHub does not have.
const maxUncitedRoutes = 33

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
		if !found || !strings.HasPrefix(path, "/api/v3/") {
			continue
		}
		registered[pattern]++
	}
	inventory := make(map[string]int, len(fuzzRoutePatterns))
	for _, pattern := range fuzzRoutePatterns {
		_, path, found := strings.Cut(pattern, " ")
		if !found || !strings.HasPrefix(path, "/api/v3/") {
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
}

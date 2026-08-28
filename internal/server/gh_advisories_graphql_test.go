package bleephub

import (
	"net/http"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// End-to-end tests for the security-advisory vertical: the global advisory
// database, a repository's Dependabot alerts and manifests, dismissal, and the
// derivation lifecycle between them.

// advisoryFixture is a repository that has submitted a dependency snapshot
// and an advisory drafted against one of the packages in it.
type advisoryFixture struct {
	server        *isolatedServer
	owner         *store.User
	ownerToken    string
	stranger      *store.User
	strangerToken string
	repo          *store.Repo
	ghsaID        string
}

// newAdvisoryFixture provisions the repository, its snapshot and accounts; the
// advisory is drafted but NOT published so a test can watch publication produce
// the alert.
func newAdvisoryFixture(t *testing.T, tag string, private bool) *advisoryFixture {
	t.Helper()
	s := newIsolatedServer(t)
	f := &advisoryFixture{server: s}

	f.owner = s.createTestUser(t, "adv-owner-"+tag)
	f.stranger = s.createTestUser(t, "adv-stranger-"+tag)
	ownerToken := s.store.CreateToken(f.owner.ID, "repo")
	strangerToken := s.store.CreateToken(f.stranger.ID, "repo")
	if ownerToken == nil || strangerToken == nil {
		t.Fatalf("fixture %s: could not mint tokens", tag)
	}
	f.ownerToken, f.strangerToken = ownerToken.Value, strangerToken.Value

	f.repo = s.store.CreateRepo(f.owner, "adv-repo-"+tag, "", private)
	if f.repo == nil {
		t.Fatalf("fixture %s: could not create the repository", tag)
	}
	f.submitSnapshot(t, "4.17.20")
	return f
}

// submitSnapshot submits a lodash-at-version snapshot through the real endpoint
// so the whole ingest path runs.
func (f *advisoryFixture) submitSnapshot(t *testing.T, lodashVersion string) {
	t.Helper()
	resp := f.server.post(t, "/api/v3/repos/"+f.repo.FullName+"/dependency-graph/snapshots", f.ownerToken,
		map[string]interface{}{
			"version": 0,
			"sha":     "0123456789abcdef0123456789abcdef01234567",
			"ref":     "refs/heads/" + f.repo.DefaultBranch,
			"job":     map[string]interface{}{"id": "job-" + lodashVersion, "correlator": "test-detector"},
			"detector": map[string]interface{}{
				"name": "test-detector", "version": "1.0.0", "url": "https://example.com/detector",
			},
			"scanned": "2026-01-01T00:00:00Z",
			"manifests": map[string]interface{}{
				"package-lock.json": map[string]interface{}{
					"name": "package-lock.json",
					"file": map[string]interface{}{"source_location": "package-lock.json"},
					"resolved": map[string]interface{}{
						"lodash": map[string]interface{}{
							"package_url":  "pkg:npm/lodash@" + lodashVersion,
							"relationship": "direct",
							"scope":        "runtime",
						},
						"chalk": map[string]interface{}{
							"package_url":  "pkg:npm/chalk@5.3.0",
							"relationship": "indirect",
							"scope":        "development",
						},
					},
				},
			},
		})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("dependency snapshot submission = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()
}

// draftAdvisory creates a repository advisory against lodash, still private.
func (f *advisoryFixture) draftAdvisory(t *testing.T) {
	t.Helper()
	resp := f.server.post(t, "/api/v3/repos/"+f.repo.FullName+"/security-advisories", f.ownerToken,
		map[string]interface{}{
			"summary":            "Prototype pollution in lodash",
			"description":        "A crafted object reaches Object.prototype.",
			"severity":           "high",
			"cve_id":             "CVE-2026-0001",
			"cwe_ids":            []string{"CWE-1321"},
			"cvss_vector_string": "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:H/A:N",
			"vulnerabilities": []map[string]interface{}{{
				"package":                  map[string]interface{}{"ecosystem": "npm", "name": "lodash"},
				"vulnerable_version_range": "< 4.17.21",
				"patched_versions":         "4.17.21",
			}},
		})
	body := decodeJSONWithStatus(t, resp, http.StatusCreated)
	f.ghsaID, _ = body["ghsa_id"].(string)
	if f.ghsaID == "" {
		t.Fatalf("advisory create returned no ghsa_id: %v", body)
	}
}

// publishAdvisory moves the drafted advisory to published, which is what
// makes it public and derives alerts against every repository.
func (f *advisoryFixture) publishAdvisory(t *testing.T) {
	t.Helper()
	resp := f.server.patch(t, "/api/v3/repos/"+f.repo.FullName+"/security-advisories/"+f.ghsaID, f.ownerToken,
		map[string]interface{}{"state": "published"})
	decodeJSONWithStatus(t, resp, http.StatusOK)
}

func TestSecurityAdvisoryQueriesReadTheGlobalDatabase(t *testing.T) {
	t.Parallel()
	f := newAdvisoryFixture(t, "globaldb", false)
	f.draftAdvisory(t)
	f.publishAdvisory(t)

	data := f.server.gqlData(t, `query($ghsa:String!){
		securityAdvisory(ghsaId:$ghsa){
			id databaseId ghsaId summary description severity classification origin
			publishedAt updatedAt withdrawnAt permalink notificationsPermalink
			cvss{ score vectorString }
			cvssSeverities{ cvssV3{ score } cvssV4{ score } }
			epss{ percentage }
			identifiers{ type value }
			references{ url }
			cwes(first:10){ totalCount nodes{ id cweId name } }
			vulnerabilities(first:10){
				totalCount
				nodes{
					vulnerableVersionRange
					severity
					package{ ecosystem name }
					firstPatchedVersion{ identifier }
					advisory{ ghsaId }
				}
			}
		}
	}`, map[string]interface{}{"ghsa": f.ghsaID})

	advisory, _ := data["securityAdvisory"].(map[string]interface{})
	if advisory == nil {
		t.Fatalf("securityAdvisory returned null for a published advisory: %v", data)
	}
	if advisory["ghsaId"] != f.ghsaID {
		t.Errorf("ghsaId = %v, want %q", advisory["ghsaId"], f.ghsaID)
	}
	if advisory["severity"] != "HIGH" {
		t.Errorf("severity = %v, want HIGH", advisory["severity"])
	}
	if advisory["classification"] != "GENERAL" {
		t.Errorf("classification = %v, want GENERAL", advisory["classification"])
	}
	// An advisory that stands has genuinely not been withdrawn.
	if advisory["withdrawnAt"] != nil {
		t.Errorf("withdrawnAt = %v, want null for a standing advisory", advisory["withdrawnAt"])
	}
	// EPSS is a score this instance has no source for, so null is the honest
	// answer rather than a fabricated probability.
	if advisory["epss"] != nil {
		t.Errorf("epss = %v, want null", advisory["epss"])
	}
	permalink, _ := advisory["permalink"].(string)
	if !strings.HasSuffix(permalink, "/advisories/"+f.ghsaID) {
		t.Errorf("permalink = %q, want it to address the advisory", permalink)
	}
	if notifications, _ := advisory["notificationsPermalink"].(string); !strings.HasSuffix(notifications, "/dependabot") {
		t.Errorf("notificationsPermalink = %q, want the dependabot alerts page", notifications)
	}

	cvss, _ := advisory["cvss"].(map[string]interface{})
	if cvss == nil || cvss["vectorString"] == nil {
		t.Errorf("cvss = %v, want the drafted vector", cvss)
	}

	identifiers := advisoryList(t, advisory["identifiers"])
	if len(identifiers) != 2 {
		t.Fatalf("identifiers = %v, want GHSA and CVE", identifiers)
	}
	if identifiers[0]["type"] != "GHSA" || identifiers[1]["type"] != "CVE" {
		t.Errorf("identifiers = %v, want GHSA then CVE", identifiers)
	}

	cwes, _ := advisory["cwes"].(map[string]interface{})
	if cwes == nil || cwes["totalCount"].(float64) != 1 {
		t.Fatalf("cwes = %v, want the one drafted CWE", cwes)
	}
	cweNodes := advisoryList(t, cwes["nodes"])
	if cweNodes[0]["cweId"] != "CWE-1321" {
		t.Errorf("cweId = %v, want CWE-1321", cweNodes[0]["cweId"])
	}

	vulnerabilities, _ := advisory["vulnerabilities"].(map[string]interface{})
	vulnNodes := advisoryList(t, vulnerabilities["nodes"])
	if len(vulnNodes) != 1 {
		t.Fatalf("vulnerabilities = %v, want one", vulnNodes)
	}
	pkg, _ := vulnNodes[0]["package"].(map[string]interface{})
	if pkg["ecosystem"] != "NPM" || pkg["name"] != "lodash" {
		t.Errorf("package = %v, want NPM/lodash", pkg)
	}
	if vulnNodes[0]["vulnerableVersionRange"] != "< 4.17.21" {
		t.Errorf("vulnerableVersionRange = %v", vulnNodes[0]["vulnerableVersionRange"])
	}
	patched, _ := vulnNodes[0]["firstPatchedVersion"].(map[string]interface{})
	if patched == nil || patched["identifier"] != "4.17.21" {
		t.Errorf("firstPatchedVersion = %v, want 4.17.21", patched)
	}
	// The cycle back to the parent must close on the same advisory.
	back, _ := vulnNodes[0]["advisory"].(map[string]interface{})
	if back == nil || back["ghsaId"] != f.ghsaID {
		t.Errorf("vulnerability.advisory = %v, want the parent advisory", back)
	}

	listData := f.server.gqlData(t, `{
		securityAdvisories(first:10){ totalCount nodes{ ghsaId } }
		securityVulnerabilities(first:10,ecosystem:NPM,package:"lodash"){
			totalCount nodes{ package{ name } advisory{ ghsaId } }
		}
	}`, nil)
	advisories, _ := listData["securityAdvisories"].(map[string]interface{})
	if advisories["totalCount"].(float64) != 1 {
		t.Errorf("securityAdvisories totalCount = %v, want 1", advisories["totalCount"])
	}
	vulns, _ := listData["securityVulnerabilities"].(map[string]interface{})
	if vulns["totalCount"].(float64) != 1 {
		t.Errorf("securityVulnerabilities totalCount = %v, want 1", vulns["totalCount"])
	}
}

// TestGlobalAdvisoryDatabaseIsPublicButDraftsAreNot pins the visibility rule:
// the published database answers any account, but a draft is absent from it
// even for the owner because a draft is not in the database at all.
func TestGlobalAdvisoryDatabaseIsPublicButDraftsAreNot(t *testing.T) {
	t.Parallel()
	f := newAdvisoryFixture(t, "publicdb", true)
	f.draftAdvisory(t)

	// While the advisory is a draft, no root field can see it — not even the
	// owner's, since these fields read the public database rather than the
	// repository.
	for _, token := range []string{f.ownerToken, f.strangerToken} {
		env := f.server.gqlAuthzPost(t, token,
			`query($ghsa:String!){ securityAdvisory(ghsaId:$ghsa){ ghsaId } }`,
			map[string]interface{}{"ghsa": f.ghsaID})
		data, _ := env["data"].(map[string]interface{})
		if data == nil || data["securityAdvisory"] != nil {
			t.Fatalf("a drafted advisory was readable from the global database: %v", env)
		}
	}
	// The owner can still read it through the repository advisory API, which
	// is where a draft lives.
	resp := f.server.get(t, "/api/v3/repos/"+f.repo.FullName+"/security-advisories/"+f.ghsaID, f.ownerToken)
	decodeJSONWithStatus(t, resp, http.StatusOK)

	// Publication makes it public — the stranger, who cannot even read the
	// private repository that drafted it, now reads the advisory.
	f.publishAdvisory(t)
	env := f.server.gqlAuthzPost(t, f.strangerToken,
		`query($ghsa:String!){ securityAdvisory(ghsaId:$ghsa){ ghsaId severity } }`,
		map[string]interface{}{"ghsa": f.ghsaID})
	data, _ := env["data"].(map[string]interface{})
	advisory, _ := data["securityAdvisory"].(map[string]interface{})
	if advisory == nil || advisory["ghsaId"] != f.ghsaID {
		t.Fatalf("a published advisory was not public: %v", env)
	}
	// Reading the advisory must not have leaked the private repository that
	// drafted it.
	resp = f.server.get(t, "/api/v3/repos/"+f.repo.FullName, f.strangerToken)
	if resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Errorf("the drafting private repository = %d for a stranger, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestVulnerabilityAlertsAreDerivedFromManifestsAndAdvisories is the core
// claim: an alert exists because a declared dependency falls inside a published
// advisory's vulnerable range, and clears when it moves out of that range.
func TestVulnerabilityAlertsAreDerivedFromManifestsAndAdvisories(t *testing.T) {
	t.Parallel()
	f := newAdvisoryFixture(t, "derived", false)

	// Before any advisory is published, the repository has no alerts. The
	// dependencies are there; nothing says they are vulnerable.
	if alerts := f.alertNodes(t, f.ownerToken); len(alerts) != 0 {
		t.Fatalf("alerts before any advisory was published: %v", alerts)
	}

	f.draftAdvisory(t)
	// A drafted advisory derives nothing either: an embargoed advisory has not
	// told anybody their dependency is vulnerable.
	if alerts := f.alertNodes(t, f.ownerToken); len(alerts) != 0 {
		t.Fatalf("alerts from a drafted advisory: %v", alerts)
	}

	f.publishAdvisory(t)
	alerts := f.alertNodes(t, f.ownerToken)
	if len(alerts) != 1 {
		t.Fatalf("alerts after publication = %d, want exactly one (lodash, not chalk)", len(alerts))
	}
	alert := alerts[0]
	if alert["state"] != "OPEN" {
		t.Errorf("state = %v, want OPEN", alert["state"])
	}
	if alert["vulnerableManifestPath"] != "package-lock.json" {
		t.Errorf("vulnerableManifestPath = %v", alert["vulnerableManifestPath"])
	}
	if alert["vulnerableManifestFilename"] != "package-lock.json" {
		t.Errorf("vulnerableManifestFilename = %v", alert["vulnerableManifestFilename"])
	}
	// Scope and relationship come from the manifest entry, not from the alert.
	if alert["dependencyScope"] != "RUNTIME" {
		t.Errorf("dependencyScope = %v, want RUNTIME", alert["dependencyScope"])
	}
	if alert["dependencyRelationship"] != "DIRECT" {
		t.Errorf("dependencyRelationship = %v, want DIRECT", alert["dependencyRelationship"])
	}
	if alert["vulnerableRequirements"] != "= 4.17.20" {
		t.Errorf("vulnerableRequirements = %v, want the resolved version", alert["vulnerableRequirements"])
	}
	advisory, _ := alert["securityAdvisory"].(map[string]interface{})
	if advisory == nil || advisory["ghsaId"] != f.ghsaID {
		t.Errorf("securityAdvisory = %v, want the published advisory", advisory)
	}
	vulnerability, _ := alert["securityVulnerability"].(map[string]interface{})
	if vulnerability == nil || vulnerability["vulnerableVersionRange"] != "< 4.17.21" {
		t.Errorf("securityVulnerability = %v", vulnerability)
	}
	// This instance raises alerts but opens no update pull requests, so the
	// nullable field reports none rather than an empty update.
	if alert["dependabotUpdate"] != nil {
		t.Errorf("dependabotUpdate = %v, want null", alert["dependabotUpdate"])
	}

	// Upgrading past the vulnerable range fixes the alert. This is the half
	// derivation cannot do: it only ever adds.
	f.submitSnapshot(t, "4.17.21")
	alerts = f.alertNodes(t, f.ownerToken)
	if len(alerts) != 1 || alerts[0]["state"] != "FIXED" {
		t.Fatalf("after upgrading past the range, alerts = %v, want one FIXED", alerts)
	}
	if alerts[0]["fixedAt"] == nil {
		t.Errorf("a fixed alert has no fixedAt: %v", alerts[0])
	}

	// Downgrading back into it reintroduces the vulnerability.
	f.submitSnapshot(t, "4.17.19")
	alerts = f.alertNodes(t, f.ownerToken)
	if len(alerts) != 1 || alerts[0]["state"] != "OPEN" {
		t.Fatalf("after downgrading back into the range, alerts = %v, want one OPEN", alerts)
	}
	if alerts[0]["fixedAt"] != nil {
		t.Errorf("a reintroduced alert still reports fixedAt: %v", alerts[0])
	}
}

// TestVulnerabilityAlertsRespectEcosystemVersionOrdering pins that the match
// uses the advisory's ecosystem ordering: a Python post-release is newer than
// its release, so a range that excludes it must not raise an alert while the
// same strings under npm would.
func TestVulnerabilityAlertsRespectEcosystemVersionOrdering(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	owner := s.createTestUser(t, "adv-pep440-owner")
	token := s.store.CreateToken(owner.ID, "repo")
	if token == nil {
		t.Fatal("could not mint a token")
	}
	repo := s.store.CreateRepo(owner, "adv-pep440", "", false)

	// requests 2.0.post1 is NEWER than 2.0 under PEP 440, so an advisory
	// covering "<= 2.0" does not cover it.
	resp := s.post(t, "/api/v3/repos/"+repo.FullName+"/dependency-graph/snapshots", token.Value,
		map[string]interface{}{
			"version":  0,
			"sha":      "0123456789abcdef0123456789abcdef01234567",
			"ref":      "refs/heads/" + repo.DefaultBranch,
			"job":      map[string]interface{}{"id": "job-pep440", "correlator": "pep440"},
			"detector": map[string]interface{}{"name": "pep440", "version": "1.0.0", "url": "https://example.com/d"},
			"scanned":  "2026-01-01T00:00:00Z",
			"manifests": map[string]interface{}{
				"requirements.txt": map[string]interface{}{
					"name": "requirements.txt",
					"resolved": map[string]interface{}{
						"requests": map[string]interface{}{"package_url": "pkg:pypi/requests@2.0.post1"},
					},
				},
			},
		})
	decodeJSONWithStatus(t, resp, http.StatusCreated)

	resp = s.post(t, "/api/v3/repos/"+repo.FullName+"/security-advisories", token.Value,
		map[string]interface{}{
			"summary":     "Something in requests",
			"description": "d",
			"severity":    "medium",
			"vulnerabilities": []map[string]interface{}{{
				"package":                  map[string]interface{}{"ecosystem": "pip", "name": "requests"},
				"vulnerable_version_range": "<= 2.0",
			}},
		})
	created := decodeJSONWithStatus(t, resp, http.StatusCreated)
	ghsaID, _ := created["ghsa_id"].(string)
	resp = s.patch(t, "/api/v3/repos/"+repo.FullName+"/security-advisories/"+ghsaID, token.Value,
		map[string]interface{}{"state": "published"})
	decodeJSONWithStatus(t, resp, http.StatusOK)

	alerts := s.store.ListDependabotAlerts(repo.FullName, "", "", "", "", "", "created", "asc")
	if len(alerts) != 0 {
		t.Fatalf("a PEP 440 post-release was treated as inside \"<= 2.0\": %+v", alerts[0])
	}
}

// TestVulnerabilityAlertsAreNotVisibleWithoutSecurityAccess is the
// cross-tenant isolation case. The field is nullable precisely so a refusal
// cannot be read as "this repository has no vulnerabilities".
func TestVulnerabilityAlertsAreNotVisibleWithoutSecurityAccess(t *testing.T) {
	t.Parallel()
	for _, private := range []bool{true, false} {
		private := private
		name := "public"
		if private {
			name = "private"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newAdvisoryFixture(t, "isolation-"+name, private)
			f.draftAdvisory(t)
			f.publishAdvisory(t)

			if alerts := f.alertNodes(t, f.ownerToken); len(alerts) != 1 {
				t.Fatalf("the repository owner sees %d alerts, want 1", len(alerts))
			}

			// An unrelated account gets null — not an empty connection —
			// whether or not it can read the repository's code.
			env := f.server.gqlAuthzPost(t, f.strangerToken, `query($owner:String!,$name:String!){
				repository(owner:$owner,name:$name){ vulnerabilityAlerts(first:10){ totalCount } }
			}`, map[string]interface{}{"owner": f.repo.Owner.Login, "name": f.repo.Name})
			data, _ := env["data"].(map[string]interface{})
			repository, _ := data["repository"].(map[string]interface{})
			if private {
				if repository != nil {
					t.Fatalf("a stranger read a private repository: %v", env)
				}
				return
			}
			if repository == nil {
				t.Fatalf("a stranger could not read a public repository: %v", env)
			}
			if repository["vulnerabilityAlerts"] != nil {
				t.Errorf("vulnerabilityAlerts = %v for a caller without security access, want null",
					repository["vulnerabilityAlerts"])
			}
		})
	}
}

func TestDismissRepositoryVulnerabilityAlertMutation(t *testing.T) {
	t.Parallel()
	f := newAdvisoryFixture(t, "dismiss", false)
	f.draftAdvisory(t)
	f.publishAdvisory(t)

	alerts := f.alertNodes(t, f.ownerToken)
	if len(alerts) != 1 {
		t.Fatalf("want one alert to dismiss, got %d", len(alerts))
	}
	alertID, _ := alerts[0]["id"].(string)

	env := f.server.gqlAuthzPost(t, f.ownerToken, `mutation($input:DismissRepositoryVulnerabilityAlertInput!){
		dismissRepositoryVulnerabilityAlert(input:$input){
			clientMutationId
			repositoryVulnerabilityAlert{ id state dismissReason dismissedAt dismisser{ login } }
		}
	}`, map[string]interface{}{"input": map[string]interface{}{
		"repositoryVulnerabilityAlertId": alertID,
		"dismissReason":                  "TOLERABLE_RISK",
		"clientMutationId":               "dismiss-1",
	}})
	if errs := gqlAuthzErrors(env); len(errs) != 0 {
		t.Fatalf("the repository owner was refused: %v", errs)
	}
	data, _ := env["data"].(map[string]interface{})
	payload, _ := data["dismissRepositoryVulnerabilityAlert"].(map[string]interface{})
	if payload["clientMutationId"] != "dismiss-1" {
		t.Errorf("clientMutationId = %v", payload["clientMutationId"])
	}
	dismissed, _ := payload["repositoryVulnerabilityAlert"].(map[string]interface{})
	if dismissed["state"] != "DISMISSED" {
		t.Fatalf("state = %v, want DISMISSED", dismissed["state"])
	}
	if dismissed["dismissedAt"] == nil {
		t.Errorf("dismissedAt is null on a dismissed alert")
	}
	if reason, _ := dismissed["dismissReason"].(string); reason != "Risk is tolerable to this project" {
		t.Errorf("dismissReason = %q", reason)
	}
	dismisser, _ := dismissed["dismisser"].(map[string]interface{})
	if dismisser == nil || dismisser["login"] != f.owner.Login {
		t.Errorf("dismisser = %v, want the acting user", dismisser)
	}

	stored := f.server.store.ListDependabotAlerts(f.repo.FullName, "", "", "", "", "", "created", "asc")
	if len(stored) != 1 || stored[0].State != store.DependabotStateDismissed {
		t.Fatalf("store state after the mutation = %+v", stored)
	}
	if stored[0].DismissedReason != "tolerable_risk" {
		t.Errorf("stored dismissed_reason = %q, want tolerable_risk", stored[0].DismissedReason)
	}

	// A dismissed alert is not resurrected by re-derivation: the dependency is
	// still vulnerable, but a person already ruled on it.
	f.submitSnapshot(t, "4.17.20")
	stored = f.server.store.ListDependabotAlerts(f.repo.FullName, "", "", "", "", "", "created", "asc")
	if stored[0].State != store.DependabotStateDismissed {
		t.Errorf("re-derivation overturned a dismissal: %q", stored[0].State)
	}
}

func TestDependencyGraphManifestsOverGraphQL(t *testing.T) {
	t.Parallel()
	f := newAdvisoryFixture(t, "manifests", false)

	data := f.server.gqlData(t, `query($owner:String!,$name:String!){
		repository(owner:$owner,name:$name){
			hasVulnerabilityAlertsEnabled
			dependencyGraphManifests(first:10){
				totalCount
				nodes{
					id filename blobPath parseable exceedsMaxSize dependenciesCount
					repository{ nameWithOwner }
					dependencies(first:10){
						totalCount
						nodes{ packageName packageManager packageUrl relationship requirements hasDependencies }
					}
				}
			}
		}
	}`, map[string]interface{}{"owner": f.repo.Owner.Login, "name": f.repo.Name})

	repository, _ := data["repository"].(map[string]interface{})
	manifests, _ := repository["dependencyGraphManifests"].(map[string]interface{})
	if manifests == nil || manifests["totalCount"].(float64) != 1 {
		t.Fatalf("dependencyGraphManifests = %v, want the one submitted manifest", manifests)
	}
	nodes := advisoryList(t, manifests["nodes"])
	manifest := nodes[0]
	if manifest["filename"] != "package-lock.json" {
		t.Errorf("filename = %v", manifest["filename"])
	}
	if manifest["parseable"] != true || manifest["exceedsMaxSize"] != false {
		t.Errorf("parseable/exceedsMaxSize = %v/%v", manifest["parseable"], manifest["exceedsMaxSize"])
	}
	if manifest["dependenciesCount"].(float64) != 2 {
		t.Errorf("dependenciesCount = %v, want 2", manifest["dependenciesCount"])
	}
	blobPath, _ := manifest["blobPath"].(string)
	if !strings.HasSuffix(blobPath, "/package-lock.json") {
		t.Errorf("blobPath = %q", blobPath)
	}
	owningRepo, _ := manifest["repository"].(map[string]interface{})
	if owningRepo == nil || owningRepo["nameWithOwner"] != f.repo.FullName {
		t.Errorf("manifest.repository = %v", owningRepo)
	}

	dependencies, _ := manifest["dependencies"].(map[string]interface{})
	depNodes := advisoryList(t, dependencies["nodes"])
	if len(depNodes) != 2 {
		t.Fatalf("dependencies = %v, want two", depNodes)
	}
	byName := map[string]map[string]interface{}{}
	for _, dependency := range depNodes {
		name, _ := dependency["packageName"].(string)
		byName[name] = dependency
	}
	lodash := byName["lodash"]
	if lodash == nil {
		t.Fatalf("lodash missing from %v", byName)
	}
	if lodash["packageManager"] != "NPM" {
		t.Errorf("packageManager = %v, want NPM", lodash["packageManager"])
	}
	if lodash["relationship"] != "direct" {
		t.Errorf("relationship = %v, want direct", lodash["relationship"])
	}
	if lodash["requirements"] != "= 4.17.20" {
		t.Errorf("requirements = %v", lodash["requirements"])
	}
	if byName["chalk"]["relationship"] != "transitive" {
		t.Errorf("an indirect dependency's relationship = %v, want transitive", byName["chalk"]["relationship"])
	}

	// hasVulnerabilityAlertsEnabled tracks the repository setting the REST
	// vulnerability-alerts routes flip.
	if repository["hasVulnerabilityAlertsEnabled"] != f.repo.VulnerabilityAlertsEnabled {
		t.Errorf("hasVulnerabilityAlertsEnabled = %v, want %v",
			repository["hasVulnerabilityAlertsEnabled"], f.repo.VulnerabilityAlertsEnabled)
	}
	resp := f.server.put(t, "/api/v3/repos/"+f.repo.FullName+"/vulnerability-alerts", f.ownerToken, nil)
	if resp.StatusCode != http.StatusNoContent {
		resp.Body.Close()
		t.Fatalf("enable vulnerability alerts = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
	data = f.server.gqlData(t, `query($owner:String!,$name:String!){
		repository(owner:$owner,name:$name){ hasVulnerabilityAlertsEnabled }
	}`, map[string]interface{}{"owner": f.repo.Owner.Login, "name": f.repo.Name})
	repository, _ = data["repository"].(map[string]interface{})
	if repository["hasVulnerabilityAlertsEnabled"] != true {
		t.Errorf("hasVulnerabilityAlertsEnabled after enabling = %v, want true",
			repository["hasVulnerabilityAlertsEnabled"])
	}
}

// TestAdvisoryNodesResolveThroughQueryNode pins that every Node this family
// adds is reachable by global id, and the two repository-scoped ones stay
// behind their access check.
func TestAdvisoryNodesResolveThroughQueryNode(t *testing.T) {
	t.Parallel()
	f := newAdvisoryFixture(t, "nodes", false)
	f.draftAdvisory(t)
	f.publishAdvisory(t)

	advisoryData := f.server.gqlData(t, `query($ghsa:String!){ securityAdvisory(ghsaId:$ghsa){ id } }`,
		map[string]interface{}{"ghsa": f.ghsaID})
	advisory, _ := advisoryData["securityAdvisory"].(map[string]interface{})
	advisoryID, _ := advisory["id"].(string)

	alerts := f.alertNodes(t, f.ownerToken)
	alertID, _ := alerts[0]["id"].(string)

	manifestData := f.server.gqlData(t, `query($owner:String!,$name:String!){
		repository(owner:$owner,name:$name){ dependencyGraphManifests(first:1){ nodes{ id } } }
	}`, map[string]interface{}{"owner": f.repo.Owner.Login, "name": f.repo.Name})
	repository, _ := manifestData["repository"].(map[string]interface{})
	manifests, _ := repository["dependencyGraphManifests"].(map[string]interface{})
	manifestID, _ := advisoryList(t, manifests["nodes"])[0]["id"].(string)

	cases := []struct {
		nodeID   string
		typename string
	}{
		{advisoryID, "SecurityAdvisory"},
		{alertID, "RepositoryVulnerabilityAlert"},
		{manifestID, "DependencyGraphManifest"},
	}
	for _, testCase := range cases {
		data := f.server.gqlData(t, `query($id:ID!){ node(id:$id){ __typename id } }`,
			map[string]interface{}{"id": testCase.nodeID})
		node, _ := data["node"].(map[string]interface{})
		if node == nil || node["__typename"] != testCase.typename {
			t.Errorf("node(%q) = %v, want a %s", testCase.nodeID, node, testCase.typename)
		}
	}

	// The alert is repository-scoped, so a stranger resolves it to null even
	// though the advisory behind it is public.
	env := f.server.gqlAuthzPost(t, f.strangerToken, `query($id:ID!){ node(id:$id){ __typename } }`,
		map[string]interface{}{"id": alertID})
	data, _ := env["data"].(map[string]interface{})
	if data["node"] != nil {
		t.Errorf("a stranger resolved a vulnerability alert node: %v", env)
	}
	env = f.server.gqlAuthzPost(t, f.strangerToken, `query($id:ID!){ node(id:$id){ __typename } }`,
		map[string]interface{}{"id": advisoryID})
	data, _ = env["data"].(map[string]interface{})
	node, _ := data["node"].(map[string]interface{})
	if node == nil || node["__typename"] != "SecurityAdvisory" {
		t.Errorf("a published advisory node was not public: %v", env)
	}
}

func TestAdvisoryConnectionFiltersAndOrdering(t *testing.T) {
	t.Parallel()
	f := newAdvisoryFixture(t, "filters", false)
	f.draftAdvisory(t)
	f.publishAdvisory(t)

	// A second published advisory, against a different ecosystem, so the
	// filters have something to exclude.
	resp := f.server.post(t, "/api/v3/repos/"+f.repo.FullName+"/security-advisories", f.ownerToken,
		map[string]interface{}{
			"summary": "Something in requests", "description": "d", "severity": "low",
			"vulnerabilities": []map[string]interface{}{{
				"package":                  map[string]interface{}{"ecosystem": "pip", "name": "requests"},
				"vulnerable_version_range": "< 2.0",
			}},
		})
	second := decodeJSONWithStatus(t, resp, http.StatusCreated)
	secondGHSA, _ := second["ghsa_id"].(string)
	resp = f.server.patch(t, "/api/v3/repos/"+f.repo.FullName+"/security-advisories/"+secondGHSA,
		f.ownerToken, map[string]interface{}{"state": "published"})
	decodeJSONWithStatus(t, resp, http.StatusOK)

	data := f.server.gqlData(t, `query($ghsa:String!){
		securityAdvisories(first:10,identifier:{type:GHSA,value:$ghsa}){ totalCount nodes{ ghsaId } }
	}`, map[string]interface{}{"ghsa": f.ghsaID})
	advisories, _ := data["securityAdvisories"].(map[string]interface{})
	if advisories["totalCount"].(float64) != 1 {
		t.Errorf("identifier filter returned %v advisories, want 1", advisories["totalCount"])
	}

	data = f.server.gqlData(t, `{
		npm: securityVulnerabilities(first:10,ecosystem:NPM){ totalCount }
		pip: securityVulnerabilities(first:10,ecosystem:PIP){ totalCount }
		byPackage: securityVulnerabilities(first:10,package:"lodash"){ totalCount }
		lowOnly: securityVulnerabilities(first:10,severities:[LOW]){ totalCount nodes{ severity } }
	}`, nil)
	for _, expectation := range []struct {
		alias string
		want  float64
	}{{"npm", 1}, {"pip", 1}, {"byPackage", 1}, {"lowOnly", 1}} {
		connection, _ := data[expectation.alias].(map[string]interface{})
		if connection["totalCount"].(float64) != expectation.want {
			t.Errorf("%s totalCount = %v, want %v", expectation.alias, connection["totalCount"], expectation.want)
		}
	}
	lowOnly, _ := data["lowOnly"].(map[string]interface{})
	if node := advisoryList(t, lowOnly["nodes"])[0]; node["severity"] != "LOW" {
		t.Errorf("severities filter returned a %v vulnerability", node["severity"])
	}

	ascending := f.server.gqlData(t, `{
		securityAdvisories(first:10,orderBy:{field:PUBLISHED_AT,direction:ASC}){ nodes{ ghsaId } }
	}`, nil)
	descending := f.server.gqlData(t, `{
		securityAdvisories(first:10,orderBy:{field:PUBLISHED_AT,direction:DESC}){ nodes{ ghsaId } }
	}`, nil)
	ascendingIDs := advisoryGHSAIDs(t, ascending)
	descendingIDs := advisoryGHSAIDs(t, descending)
	if len(ascendingIDs) != 2 || len(descendingIDs) != 2 {
		t.Fatalf("expected two advisories in both orderings: %v / %v", ascendingIDs, descendingIDs)
	}
	if ascendingIDs[0] != descendingIDs[1] || ascendingIDs[1] != descendingIDs[0] {
		t.Errorf("ASC %v and DESC %v are not reverses of one another", ascendingIDs, descendingIDs)
	}

	page := f.server.gqlData(t, `{
		securityAdvisories(first:1){ nodes{ ghsaId } pageInfo{ hasNextPage endCursor } }
	}`, nil)
	connection, _ := page["securityAdvisories"].(map[string]interface{})
	pageInfo, _ := connection["pageInfo"].(map[string]interface{})
	if pageInfo["hasNextPage"] != true {
		t.Fatalf("first:1 over two advisories reports no next page: %v", pageInfo)
	}
	cursor, _ := pageInfo["endCursor"].(string)
	next := f.server.gqlData(t, `query($after:String!){
		securityAdvisories(first:1,after:$after){ nodes{ ghsaId } pageInfo{ hasNextPage } }
	}`, map[string]interface{}{"after": cursor})
	firstIDs := advisoryGHSAIDs(t, page)
	nextIDs := advisoryGHSAIDs(t, next)
	if len(nextIDs) != 1 || nextIDs[0] == firstIDs[0] {
		t.Errorf("the second page repeated the first: %v then %v", firstIDs, nextIDs)
	}
}

// TestWithdrawnAdvisoryLeavesTheListingButStaysAddressable pins that a
// withdrawn advisory drops out of browsing but is still reported as withdrawn
// (not absent) when asked for by identifier.
func TestWithdrawnAdvisoryLeavesTheListingButStaysAddressable(t *testing.T) {
	t.Parallel()
	f := newAdvisoryFixture(t, "withdrawn", false)
	f.draftAdvisory(t)
	f.publishAdvisory(t)

	resp := f.server.patch(t, "/api/v3/repos/"+f.repo.FullName+"/security-advisories/"+f.ghsaID,
		f.ownerToken, map[string]interface{}{"state": "withdrawn"})
	decodeJSONWithStatus(t, resp, http.StatusOK)

	data := f.server.gqlData(t, `query($ghsa:String!){
		byId: securityAdvisory(ghsaId:$ghsa){ ghsaId withdrawnAt }
		browse: securityAdvisories(first:10){ totalCount }
		byIdentifier: securityAdvisories(first:10,identifier:{type:GHSA,value:$ghsa}){ totalCount }
	}`, map[string]interface{}{"ghsa": f.ghsaID})

	byID, _ := data["byId"].(map[string]interface{})
	if byID == nil || byID["withdrawnAt"] == nil {
		t.Fatalf("a withdrawn advisory did not report its withdrawal: %v", byID)
	}
	browse, _ := data["browse"].(map[string]interface{})
	if browse["totalCount"].(float64) != 0 {
		t.Errorf("a withdrawn advisory is still offered for browsing: %v", browse)
	}
	byIdentifier, _ := data["byIdentifier"].(map[string]interface{})
	if byIdentifier["totalCount"].(float64) != 1 {
		t.Errorf("a withdrawn advisory is not addressable by identifier: %v", byIdentifier)
	}
}

// --- helpers ---------------------------------------------------------------

func (f *advisoryFixture) alertNodes(t *testing.T, token string) []map[string]interface{} {
	t.Helper()
	env := f.server.gqlAuthzPost(t, token, `query($owner:String!,$name:String!){
		repository(owner:$owner,name:$name){
			vulnerabilityAlerts(first:50){
				totalCount
				nodes{
					id number state createdAt fixedAt dismissedAt autoDismissedAt
					dismissReason dismissComment dismisser{ login }
					vulnerableManifestPath vulnerableManifestFilename vulnerableRequirements
					dependencyScope dependencyRelationship dependabotUpdate{ error{ title } }
					repository{ nameWithOwner }
					securityAdvisory{ ghsaId severity }
					securityVulnerability{ vulnerableVersionRange package{ name ecosystem } }
				}
			}
		}
	}`, map[string]interface{}{"owner": f.repo.Owner.Login, "name": f.repo.Name})
	if errs := gqlAuthzErrors(env); len(errs) != 0 {
		t.Fatalf("vulnerabilityAlerts query failed: %v", errs)
	}
	data, _ := env["data"].(map[string]interface{})
	repository, _ := data["repository"].(map[string]interface{})
	if repository == nil {
		t.Fatalf("repository was not readable: %v", env)
	}
	connection, _ := repository["vulnerabilityAlerts"].(map[string]interface{})
	if connection == nil {
		t.Fatalf("vulnerabilityAlerts was null for a caller that should see it: %v", env)
	}
	return advisoryList(t, connection["nodes"])
}

func advisoryList(t *testing.T, value interface{}) []map[string]interface{} {
	t.Helper()
	raw, ok := value.([]interface{})
	if !ok {
		t.Fatalf("expected a list, got %T (%v)", value, value)
	}
	out := make([]map[string]interface{}, 0, len(raw))
	for i, member := range raw {
		entry, ok := member.(map[string]interface{})
		if !ok {
			t.Fatalf("list member %d is %T, want an object", i, member)
		}
		out = append(out, entry)
	}
	return out
}

func advisoryGHSAIDs(t *testing.T, data map[string]interface{}) []string {
	t.Helper()
	connection, _ := data["securityAdvisories"].(map[string]interface{})
	if connection == nil {
		t.Fatalf("no securityAdvisories connection in %v", data)
	}
	var ids []string
	for _, node := range advisoryList(t, connection["nodes"]) {
		id, _ := node["ghsaId"].(string)
		ids = append(ids, id)
	}
	return ids
}

// TestGraphQLRepositoryVulnerabilityAlertByNumber pins GQL-097: the
// Repository.vulnerabilityAlert(number:) lookup returns that alert, and null
// (not an error) for an unknown number.
func TestGraphQLRepositoryVulnerabilityAlertByNumber(t *testing.T) {
	t.Parallel()
	f := newAdvisoryFixture(t, "alert-by-number", false)
	f.draftAdvisory(t)
	f.publishAdvisory(t)

	alerts := f.alertNodes(t, f.ownerToken)
	if len(alerts) != 1 {
		t.Fatalf("want exactly one alert after publication, got %d", len(alerts))
	}
	number, ok := alerts[0]["number"].(float64)
	if !ok {
		t.Fatalf("alert has no numeric number: %v", alerts[0])
	}

	env := f.server.gqlAuthzPost(t, f.ownerToken,
		`query($o:String!,$n:String!,$num:Int!){repository(owner:$o,name:$n){vulnerabilityAlert(number:$num){number state securityAdvisory{ghsaId}}}}`,
		map[string]interface{}{"o": f.repo.Owner.Login, "n": f.repo.Name, "num": int(number)})
	if errs := gqlAuthzErrors(env); len(errs) != 0 {
		t.Fatalf("vulnerabilityAlert query: %v", errs)
	}
	alert := starTestObj(t, env, "data", "repository", "vulnerabilityAlert")
	if got, _ := alert["number"].(float64); got != number {
		t.Errorf("vulnerabilityAlert.number = %v, want %v", alert["number"], number)
	}
	if alert["state"] != "OPEN" {
		t.Errorf("vulnerabilityAlert.state = %v, want OPEN", alert["state"])
	}

	// An unknown number resolves to null, not an error.
	env = f.server.gqlAuthzPost(t, f.ownerToken,
		`query($o:String!,$n:String!){repository(owner:$o,name:$n){vulnerabilityAlert(number:987654){number}}}`,
		map[string]interface{}{"o": f.repo.Owner.Login, "n": f.repo.Name})
	if errs := gqlAuthzErrors(env); len(errs) != 0 {
		t.Fatalf("unknown-number query errored: %v", errs)
	}
	if repoMap := starTestObj(t, env, "data", "repository"); repoMap["vulnerabilityAlert"] != nil {
		t.Errorf("unknown alert number should resolve null, got %v", repoMap["vulnerabilityAlert"])
	}
}

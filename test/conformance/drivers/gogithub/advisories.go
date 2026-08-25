package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	github "github.com/google/go-github/v88/github"
)

// runSecurityAdvisories drives the security-advisory, Dependabot-alert and
// dependency-graph surfaces with the unmodified go-github client.
//
// The domain is one story rather than a list of endpoints, because that is how
// it is actually used: a maintainer drafts an advisory against a package, a
// build submits the repository's dependency snapshot, publishing the advisory
// turns the two into an alert, and the alert is then read, dismissed and
// listed. Every step's assertion is on what the client decoded, so a response
// that omits a key the client's type expects fails here even though the HTTP
// status was fine.
func runSecurityAdvisories(client *github.Client, rec *recorder, set *fixtureSet) {
	const domain = "security-advisories"
	operations := []string{
		"securityAdvisories.createRepositoryAdvisory",
		"securityAdvisories.listRepositoryAdvisories",
		"securityAdvisories.getRepositoryAdvisory",
		"dependencyGraph.createSnapshot",
		"dependencyGraph.getSBOM",
		"repos.enableVulnerabilityAlerts",
		"repos.getVulnerabilityAlerts",
		"securityAdvisories.updateRepositoryAdvisory (publish)",
		"securityAdvisories.listGlobalSecurityAdvisories",
		"securityAdvisories.getGlobalSecurityAdvisory",
		"securityAdvisories.listGlobalSecurityAdvisories (ecosystem filter)",
		"securityAdvisories.listGlobalSecurityAdvisories (affects filter)",
		"dependabot.listRepoAlerts",
		"dependabot.getRepoAlert",
		"dependabot.updateAlert (dismiss)",
		"dependabot.listRepoAlerts (state filter)",
		"securityAdvisories.requestCVE",
		"securityAdvisories.createTemporaryPrivateFork",
		"repos.disableVulnerabilityAlerts",
	}

	sc := newScratch(client, set.owner, "conformance-advisories")
	if !sc.ok() {
		skipAll(rec, domain, "POST /user/repos",
			"the security-advisory repository fixture could not be provisioned", operations...)
		return
	}

	// The package the whole story is about: a version inside the advisory's
	// vulnerable range, so publishing the advisory must produce exactly one
	// alert, and a second package outside any advisory, so it must not.
	const vulnerablePackage = "lodash"
	const vulnerableVersion = "4.17.20"
	const patchedVersion = "4.17.21"

	var ghsaID string
	rec.check(domain, "securityAdvisories.createRepositoryAdvisory",
		"POST /repos/{owner}/{repo}/security-advisories", func() error {
			advisory, resp, err := createRepositoryAdvisory(client, sc.owner, sc.repo, map[string]any{
				"summary":            "Prototype pollution in " + vulnerablePackage,
				"description":        "A crafted object reaches Object.prototype.",
				"severity":           "high",
				"cve_id":             "CVE-2026-11001",
				"cvss_vector_string": "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
				"cwe_ids":            []string{"CWE-1321"},
				"vulnerabilities": []map[string]any{{
					"package":                  map[string]string{"ecosystem": "npm", "name": vulnerablePackage},
					"vulnerable_version_range": "< " + patchedVersion,
					"patched_versions":         patchedVersion,
					"vulnerable_functions":     []string{"merge"},
				}},
			})
			if err != nil {
				return err
			}
			if err := wantStatus(resp, http.StatusCreated, "repository advisory creation"); err != nil {
				return err
			}
			ghsaID = advisory.GetGHSAID()
			if err := wantField("ghsa_id", ghsaID); err != nil {
				return err
			}
			if got := advisory.GetState(); got != "draft" {
				return deviate("draft", got, "a new advisory is not in the draft state")
			}
			// The CVE identifier and the CVSS vector are members of the
			// documented request; a response that dropped either means the
			// client's write silently did not happen.
			if got := advisory.GetCVEID(); got != "CVE-2026-11001" {
				return deviate("CVE-2026-11001", got, "cve_id did not survive the create")
			}
			if got := advisory.GetCVSS().GetVectorString(); !strings.HasPrefix(got, "CVSS:3.1/") {
				return deviate("the submitted CVSS vector", got, "cvss_vector_string did not survive the create")
			}
			// GitHub derives the score from the vector, so a client reading
			// severity numerically must not be handed a zero.
			if score := advisory.GetCVSS().GetScore(); score <= 0 {
				return deviate("a score derived from the vector", fmt.Sprintf("%v", score),
					"cvss.score is not derived from the submitted vector")
			}
			if len(advisory.Vulnerabilities) != 1 {
				return deviate("1 vulnerability", fmt.Sprintf("%d", len(advisory.Vulnerabilities)),
					"the advisory's vulnerabilities did not round-trip")
			}
			return nil
		})

	if ghsaID == "" {
		skipAll(rec, domain, "POST /repos/{owner}/{repo}/security-advisories",
			"no advisory was created, so the rest of the story has no subject", operations[1:]...)
		return
	}

	rec.check(domain, "securityAdvisories.listRepositoryAdvisories",
		"GET /repos/{owner}/{repo}/security-advisories", func() error {
			advisories, resp, err := client.SecurityAdvisories.ListRepositorySecurityAdvisories(
				ctx, sc.owner, sc.repo, &github.ListRepositorySecurityAdvisoriesOptions{})
			if err != nil {
				return err
			}
			if err := wantStatus(resp, http.StatusOK, "repository advisory listing"); err != nil {
				return err
			}
			for _, advisory := range advisories {
				if advisory.GetGHSAID() == ghsaID {
					return nil
				}
			}
			return deviate("the drafted advisory", fmt.Sprintf("%d advisories", len(advisories)),
				"the advisory just created is absent from the repository's listing")
		})

	rec.check(domain, "securityAdvisories.getRepositoryAdvisory",
		"GET /repos/{owner}/{repo}/security-advisories/{ghsa_id}", func() error {
			advisory, resp, err := getRepositoryAdvisory(client, sc.owner, sc.repo, ghsaID)
			if err != nil {
				return err
			}
			if err := wantStatus(resp, http.StatusOK, "repository advisory read"); err != nil {
				return err
			}
			if err := wantField("summary", advisory.GetSummary()); err != nil {
				return err
			}
			if advisory.GetAuthor().GetLogin() == "" {
				return deviate("an author", "none", "the advisory carries no author")
			}
			// credits_detailed is a separate member from credits, and a client
			// rendering the credit list reads the detailed one.
			if len(advisory.CWEs) != 1 || advisory.CWEs[0].GetCWEID() != "CWE-1321" {
				return deviate("CWE-1321", fmt.Sprintf("%d cwes", len(advisory.CWEs)),
					"the advisory's CWE list did not round-trip")
			}
			return nil
		})

	rec.check(domain, "dependencyGraph.createSnapshot",
		"POST /repos/{owner}/{repo}/dependency-graph/snapshots", func() error {
			if sc.sha == "" {
				return deviate("a head commit", "none", "the fixture repository has no head commit to attach a snapshot to")
			}
			created, resp, err := client.DependencyGraph.CreateSnapshot(ctx, sc.owner, sc.repo,
				dependencySnapshot(sc.branch, sc.sha, vulnerableVersion))
			if err != nil {
				return err
			}
			if err := wantStatus(resp, http.StatusCreated, "dependency snapshot submission"); err != nil {
				return err
			}
			// A snapshot on the default branch updates the repository's
			// dependencies; anything else is merely accepted. The client
			// branches on exactly this value.
			if got := created.GetResult(); got != "SUCCESS" {
				return deviate("SUCCESS", got, "a default-branch snapshot did not update the dependency graph")
			}
			return nil
		})

	rec.check(domain, "dependencyGraph.getSBOM", "GET /repos/{owner}/{repo}/dependency-graph/sbom", func() error {
		sbom, resp, err := client.DependencyGraph.GetSBOM(ctx, sc.owner, sc.repo)
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusOK, "SBOM export"); err != nil {
			return err
		}
		if err := wantField("SPDXID", sbom.GetSBOM().GetSPDXID()); err != nil {
			return err
		}
		for _, pkg := range sbom.GetSBOM().Packages {
			if strings.Contains(pkg.GetName(), vulnerablePackage) {
				return nil
			}
		}
		return deviate("the submitted dependency", "absent",
			"the SBOM does not describe the dependency the snapshot submitted")
	})

	rec.check(domain, "repos.enableVulnerabilityAlerts", "PUT /repos/{owner}/{repo}/vulnerability-alerts", func() error {
		resp, err := client.Repositories.EnableVulnerabilityAlerts(ctx, sc.owner, sc.repo)
		if err != nil {
			return err
		}
		return wantStatus(resp, http.StatusNoContent, "enabling vulnerability alerts")
	})

	rec.check(domain, "repos.getVulnerabilityAlerts", "GET /repos/{owner}/{repo}/vulnerability-alerts", func() error {
		enabled, _, err := client.Repositories.GetVulnerabilityAlerts(ctx, sc.owner, sc.repo)
		if err != nil {
			return err
		}
		if !enabled {
			return deviate("true", "false", "vulnerability alerts read back disabled after being enabled")
		}
		return nil
	})

	rec.check(domain, "securityAdvisories.updateRepositoryAdvisory (publish)",
		"PATCH /repos/{owner}/{repo}/security-advisories/{ghsa_id}", func() error {
			advisory, resp, err := updateRepositoryAdvisory(client, sc.owner, sc.repo, ghsaID,
				map[string]any{"state": "published"})
			if err != nil {
				return err
			}
			if err := wantStatus(resp, http.StatusOK, "advisory publication"); err != nil {
				return err
			}
			if got := advisory.GetState(); got != "published" {
				return deviate("published", got, "the advisory did not reach the published state")
			}
			if advisory.PublishedAt == nil {
				return deviate("a publication timestamp", "null", "a published advisory has no published_at")
			}
			return nil
		})

	rec.check(domain, "securityAdvisories.listGlobalSecurityAdvisories", "GET /advisories", func() error {
		advisories, resp, err := client.SecurityAdvisories.ListGlobalSecurityAdvisories(ctx,
			&github.ListGlobalSecurityAdvisoriesOptions{})
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusOK, "global advisory listing"); err != nil {
			return err
		}
		for _, advisory := range advisories {
			if advisory.GetGHSAID() == ghsaID {
				return nil
			}
		}
		return deviate("the published advisory", fmt.Sprintf("%d advisories", len(advisories)),
			"a published advisory is absent from the global database")
	})

	rec.check(domain, "securityAdvisories.getGlobalSecurityAdvisory", "GET /advisories/{ghsa_id}", func() error {
		advisory, resp, err := client.SecurityAdvisories.GetGlobalSecurityAdvisories(ctx, ghsaID)
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusOK, "global advisory read"); err != nil {
			return err
		}
		if got := advisory.GetType(); got != "reviewed" {
			return deviate("reviewed", got, "the global advisory has the wrong type")
		}
		if len(advisory.Identifiers) < 2 {
			return deviate("GHSA and CVE identifiers", fmt.Sprintf("%d", len(advisory.Identifiers)),
				"the global advisory does not carry both of its identifiers")
		}
		if len(advisory.Vulnerabilities) != 1 {
			return deviate("1 vulnerability", fmt.Sprintf("%d", len(advisory.Vulnerabilities)),
				"the global advisory carries no package coordinates")
		}
		vulnerability := advisory.Vulnerabilities[0]
		if got := vulnerability.GetPackage().GetName(); got != vulnerablePackage {
			return deviate(vulnerablePackage, got, "the vulnerability names the wrong package")
		}
		if got := vulnerability.GetFirstPatchedVersion(); got != patchedVersion {
			return deviate(patchedVersion, got, "the vulnerability does not report its first patched version")
		}
		if len(vulnerability.VulnerableFunctions) != 1 {
			return deviate("1 vulnerable function", fmt.Sprintf("%d", len(vulnerability.VulnerableFunctions)),
				"the submitted vulnerable_functions did not round-trip")
		}
		return nil
	})

	rec.check(domain, "securityAdvisories.listGlobalSecurityAdvisories (ecosystem filter)",
		"GET /advisories?ecosystem", func() error {
			matching, _, err := client.SecurityAdvisories.ListGlobalSecurityAdvisories(ctx,
				&github.ListGlobalSecurityAdvisoriesOptions{Ecosystem: github.Ptr("npm")})
			if err != nil {
				return err
			}
			if !advisoryListed(matching, ghsaID) {
				return deviate("the npm advisory", "absent", "the ecosystem filter excluded a matching advisory")
			}
			other, _, err := client.SecurityAdvisories.ListGlobalSecurityAdvisories(ctx,
				&github.ListGlobalSecurityAdvisoriesOptions{Ecosystem: github.Ptr("rubygems")})
			if err != nil {
				return err
			}
			if advisoryListed(other, ghsaID) {
				return deviate("no advisory", "the npm advisory", "the ecosystem filter did not exclude a non-matching advisory")
			}
			return nil
		})

	rec.check(domain, "securityAdvisories.listGlobalSecurityAdvisories (affects filter)",
		"GET /advisories?affects", func() error {
			matching, _, err := client.SecurityAdvisories.ListGlobalSecurityAdvisories(ctx,
				&github.ListGlobalSecurityAdvisoriesOptions{
					Affects: github.Ptr(vulnerablePackage + "@" + vulnerableVersion),
				})
			if err != nil {
				return err
			}
			if !advisoryListed(matching, ghsaID) {
				return deviate("the advisory affecting the version", "absent",
					"the affects filter excluded an advisory whose range covers the named version")
			}
			// The patched version is outside the vulnerable range, so the same
			// package at that version must NOT match.
			safe, _, err := client.SecurityAdvisories.ListGlobalSecurityAdvisories(ctx,
				&github.ListGlobalSecurityAdvisoriesOptions{
					Affects: github.Ptr(vulnerablePackage + "@" + patchedVersion),
				})
			if err != nil {
				return err
			}
			if advisoryListed(safe, ghsaID) {
				return deviate("no advisory", "the advisory",
					"the affects filter matched a version outside the vulnerable range")
			}
			return nil
		})

	var alertNumber int
	rec.check(domain, "dependabot.listRepoAlerts", "GET /repos/{owner}/{repo}/dependabot/alerts", func() error {
		alerts, resp, err := client.Dependabot.ListRepoAlerts(ctx, sc.owner, sc.repo, &github.ListAlertsOptions{})
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusOK, "Dependabot alert listing"); err != nil {
			return err
		}
		// Exactly one: the vulnerable dependency. The snapshot also declares a
		// package no advisory covers, so a second alert would mean the match
		// is not actually evaluating the version range.
		if len(alerts) != 1 {
			return deviate("1 alert", fmt.Sprintf("%d", len(alerts)),
				"publishing an advisory against a declared dependency did not produce exactly one alert")
		}
		alert := alerts[0]
		alertNumber = alert.GetNumber()
		if got := alert.GetState(); got != "open" {
			return deviate("open", got, "a newly derived alert is not open")
		}
		if got := alert.GetDependency().GetPackage().GetName(); got != vulnerablePackage {
			return deviate(vulnerablePackage, got, "the alert names the wrong package")
		}
		if got := alert.GetDependency().GetManifestPath(); got == "" {
			return deviate("the manifest path", "empty", "the alert does not say which manifest declares the dependency")
		}
		if got := alert.GetSecurityAdvisory().GetGHSAID(); got != ghsaID {
			return deviate(ghsaID, got, "the alert does not point at the advisory that produced it")
		}
		if alert.GetSecurityVulnerability().GetVulnerableVersionRange() == "" {
			return deviate("the vulnerable range", "empty", "the alert carries no vulnerable version range")
		}
		return nil
	})

	if alertNumber == 0 {
		skipAll(rec, domain, "GET /repos/{owner}/{repo}/dependabot/alerts",
			"no alert was derived, so the alert operations have no subject",
			"dependabot.getRepoAlert", "dependabot.updateAlert (dismiss)",
			"dependabot.listRepoAlerts (state filter)")
	} else {
		rec.check(domain, "dependabot.getRepoAlert",
			"GET /repos/{owner}/{repo}/dependabot/alerts/{alert_number}", func() error {
				alert, resp, err := client.Dependabot.GetRepoAlert(ctx, sc.owner, sc.repo, alertNumber)
				if err != nil {
					return err
				}
				if err := wantStatus(resp, http.StatusOK, "Dependabot alert read"); err != nil {
					return err
				}
				if alert.GetNumber() != alertNumber {
					return deviate(fmt.Sprintf("%d", alertNumber), fmt.Sprintf("%d", alert.GetNumber()),
						"the alert read back under a different number")
				}
				if err := wantField("html_url", alert.GetHTMLURL()); err != nil {
					return err
				}
				return nil
			})

		rec.check(domain, "dependabot.updateAlert (dismiss)",
			"PATCH /repos/{owner}/{repo}/dependabot/alerts/{alert_number}", func() error {
				alert, resp, err := client.Dependabot.UpdateAlert(ctx, sc.owner, sc.repo, alertNumber,
					&github.DependabotAlertState{
						State:            "dismissed",
						DismissedReason:  github.Ptr("tolerable_risk"),
						DismissedComment: github.Ptr("accepted by the conformance harness"),
					})
				if err != nil {
					return err
				}
				if err := wantStatus(resp, http.StatusOK, "Dependabot alert dismissal"); err != nil {
					return err
				}
				if got := alert.GetState(); got != "dismissed" {
					return deviate("dismissed", got, "the alert did not reach the dismissed state")
				}
				if got := alert.GetDismissedReason(); got != "tolerable_risk" {
					return deviate("tolerable_risk", got, "the dismissal reason did not round-trip")
				}
				if alert.GetDismissedBy().GetLogin() == "" {
					return deviate("the dismissing account", "none", "the dismissal records no dismissed_by")
				}
				if alert.DismissedAt == nil {
					return deviate("a dismissal timestamp", "null", "the dismissal records no dismissed_at")
				}
				return nil
			})

		rec.check(domain, "dependabot.listRepoAlerts (state filter)",
			"GET /repos/{owner}/{repo}/dependabot/alerts?state", func() error {
				dismissed, _, err := client.Dependabot.ListRepoAlerts(ctx, sc.owner, sc.repo,
					&github.ListAlertsOptions{State: github.Ptr("dismissed")})
				if err != nil {
					return err
				}
				if len(dismissed) != 1 {
					return deviate("1 dismissed alert", fmt.Sprintf("%d", len(dismissed)),
						"the state filter does not select the dismissed alert")
				}
				open, _, err := client.Dependabot.ListRepoAlerts(ctx, sc.owner, sc.repo,
					&github.ListAlertsOptions{State: github.Ptr("open")})
				if err != nil {
					return err
				}
				if len(open) != 0 {
					return deviate("0 open alerts", fmt.Sprintf("%d", len(open)),
						"the state filter still reports the dismissed alert as open")
				}
				return nil
			})
	}

	rec.check(domain, "securityAdvisories.requestCVE",
		"POST /repos/{owner}/{repo}/security-advisories/{ghsa_id}/cve", func() error {
			// A CVE can only be requested for an advisory that does not
			// already carry one, so this needs its own advisory: the story's
			// main one was created with a CVE identifier of its own, and
			// asking for a second is the documented 422.
			pending, _, err := createRepositoryAdvisory(client, sc.owner, sc.repo, map[string]any{
				"summary":     "Awaiting a CVE identifier",
				"description": "Filed without a CVE so one can be requested.",
				"severity":    "low",
			})
			if err != nil {
				return err
			}
			pendingGHSA := pending.GetGHSAID()
			if pending.GetCVEID() != "" {
				return deviate("no CVE", pending.GetCVEID(), "an advisory filed without a CVE came back carrying one")
			}
			resp, err := client.SecurityAdvisories.RequestCVE(ctx, sc.owner, sc.repo, pendingGHSA)
			if err != nil {
				return err
			}
			if err := wantStatus(resp, http.StatusAccepted, "CVE request"); err != nil {
				return err
			}
			// The request must actually assign one: a 202 that changed
			// nothing would leave the client polling forever.
			assigned, _, err := getRepositoryAdvisory(client, sc.owner, sc.repo, pendingGHSA)
			if err != nil {
				return err
			}
			if err := wantField("cve_id", assigned.GetCVEID()); err != nil {
				return err
			}
			// Requesting a second CVE for the same advisory is refused.
			if _, err := client.SecurityAdvisories.RequestCVE(ctx, sc.owner, sc.repo, pendingGHSA); err == nil {
				return deviate("a refusal", "success", "a second CVE request for the same advisory was accepted")
			}
			return nil
		})

	rec.check(domain, "securityAdvisories.createTemporaryPrivateFork",
		"POST /repos/{owner}/{repo}/security-advisories/{ghsa_id}/forks", func() error {
			fork, resp, err := client.SecurityAdvisories.CreateTemporaryPrivateFork(ctx, sc.owner, sc.repo, ghsaID)
			if err != nil {
				return err
			}
			if err := wantStatus(resp, http.StatusAccepted, "temporary private fork creation"); err != nil {
				return err
			}
			if err := wantField("full_name", fork.GetFullName()); err != nil {
				return err
			}
			if !fork.GetPrivate() {
				return deviate("a private fork", "public", "the advisory's temporary fork is not private")
			}
			return nil
		})

	rec.check(domain, "repos.disableVulnerabilityAlerts", "DELETE /repos/{owner}/{repo}/vulnerability-alerts", func() error {
		resp, err := client.Repositories.DisableVulnerabilityAlerts(ctx, sc.owner, sc.repo)
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusNoContent, "disabling vulnerability alerts"); err != nil {
			return err
		}
		enabled, _, err := client.Repositories.GetVulnerabilityAlerts(ctx, sc.owner, sc.repo)
		if err != nil {
			return err
		}
		if enabled {
			return deviate("false", "true", "vulnerability alerts read back enabled after being disabled")
		}
		return nil
	})

	runOrgSecurityAdvisories(client, rec, set)
}

// runOrgSecurityAdvisories covers the two organization-scoped listings, which
// exist only for a repository owned by an organization.
func runOrgSecurityAdvisories(client *github.Client, rec *recorder, set *fixtureSet) {
	const domain = "security-advisories"
	operations := []string{
		"securityAdvisories.listRepositorySecurityAdvisoriesForOrg",
		"dependabot.listOrgAlerts",
	}
	if set.org == "" {
		skipAll(rec, domain, "POST /admin/organizations",
			"no organization fixture, and both listings are organization-scoped", operations...)
		return
	}
	sc := newScratchInOrg(client, set.org, "conformance-org-advisories")
	if !sc.ok() {
		skipAll(rec, domain, "POST /orgs/{org}/repos",
			"the organization advisory repository fixture could not be provisioned", operations...)
		return
	}

	var ghsaID string
	if advisory, _, err := createRepositoryAdvisory(client, sc.owner, sc.repo, map[string]any{
		"summary":     "Organization-scoped advisory",
		"description": "Filed against a repository the organization owns.",
		"severity":    "medium",
		"vulnerabilities": []map[string]any{{
			"package":                  map[string]string{"ecosystem": "npm", "name": "left-pad"},
			"vulnerable_version_range": "< 1.3.0",
		}},
	}); err == nil {
		ghsaID = advisory.GetGHSAID()
	}

	rec.check(domain, "securityAdvisories.listRepositorySecurityAdvisoriesForOrg",
		"GET /orgs/{org}/security-advisories", func() error {
			advisories, resp, err := client.SecurityAdvisories.ListRepositorySecurityAdvisoriesForOrg(
				ctx, set.org, &github.ListRepositorySecurityAdvisoriesOptions{})
			if err != nil {
				return err
			}
			if err := wantStatus(resp, http.StatusOK, "organization advisory listing"); err != nil {
				return err
			}
			if ghsaID == "" {
				return deviate("an advisory to find", "none",
					"the organization advisory fixture could not be created")
			}
			for _, advisory := range advisories {
				if advisory.GetGHSAID() == ghsaID {
					return nil
				}
			}
			return deviate("the organization's advisory", fmt.Sprintf("%d advisories", len(advisories)),
				"an advisory filed on an organization repository is absent from the organization listing")
		})

	rec.check(domain, "dependabot.listOrgAlerts", "GET /orgs/{org}/dependabot/alerts", func() error {
		_, resp, err := client.Dependabot.ListOrgAlerts(ctx, set.org, &github.ListAlertsOptions{})
		if err != nil {
			return err
		}
		// The organization has no vulnerable dependency declared, so the
		// honest answer is an empty list served with a 200 — not a 404, which
		// is what a client would read as "this organization does not exist".
		return wantStatus(resp, http.StatusOK, "organization Dependabot alert listing")
	})
}

// advisoryListed reports whether a GHSA id appears in a global listing.
func advisoryListed(advisories []*github.GlobalSecurityAdvisory, ghsaID string) bool {
	for _, advisory := range advisories {
		if advisory.GetGHSAID() == ghsaID {
			return true
		}
	}
	return false
}

// dependencySnapshot builds the submission the story's alert is derived from:
// one package inside the advisory's vulnerable range and one outside every
// advisory, so the derivation has something it must NOT alert on.
func dependencySnapshot(branch, sha, lodashVersion string) *github.DependencyGraphSnapshot {
	return &github.DependencyGraphSnapshot{
		Version: 0,
		Sha:     github.Ptr(sha),
		Ref:     github.Ptr("refs/heads/" + branch),
		Job: &github.DependencyGraphSnapshotJob{
			ID:         github.Ptr("conformance-advisories"),
			Correlator: github.Ptr("conformance-advisories"),
		},
		Detector: &github.DependencyGraphSnapshotDetector{
			Name:    github.Ptr("conformance-detector"),
			Version: github.Ptr("1.0.0"),
			URL:     github.Ptr("https://example.invalid/detector"),
		},
		Scanned: &github.Timestamp{Time: time.Now().UTC()},
		Manifests: map[string]*github.DependencyGraphSnapshotManifest{
			"package-lock.json": {
				Name: github.Ptr("package-lock.json"),
				File: &github.DependencyGraphSnapshotManifestFile{
					SourceLocation: github.Ptr("package-lock.json"),
				},
				Resolved: map[string]*github.DependencyGraphSnapshotResolvedDependency{
					"lodash": {
						PackageURL:   github.Ptr("pkg:npm/lodash@" + lodashVersion),
						Relationship: github.Ptr("direct"),
						Scope:        github.Ptr("runtime"),
					},
					"chalk": {
						PackageURL:   github.Ptr("pkg:npm/chalk@5.3.0"),
						Relationship: github.Ptr("indirect"),
						Scope:        github.Ptr("development"),
					},
				},
			},
		},
	}
}

// createRepositoryAdvisory and updateRepositoryAdvisory issue the two
// repository-advisory writes go-github has no typed method for, through the
// client itself so the response is decoded by the client's own machinery into
// the client's own type — which is the whole point of the assertion.
func createRepositoryAdvisory(client *github.Client, owner, repo string, body map[string]any) (*github.SecurityAdvisory, *github.Response, error) {
	return advisoryRequest(client, http.MethodPost,
		fmt.Sprintf("repos/%v/%v/security-advisories", owner, repo), body)
}

func updateRepositoryAdvisory(client *github.Client, owner, repo, ghsaID string, body map[string]any) (*github.SecurityAdvisory, *github.Response, error) {
	return advisoryRequest(client, http.MethodPatch,
		fmt.Sprintf("repos/%v/%v/security-advisories/%v", owner, repo, ghsaID), body)
}

func getRepositoryAdvisory(client *github.Client, owner, repo, ghsaID string) (*github.SecurityAdvisory, *github.Response, error) {
	return advisoryRequest(client, http.MethodGet,
		fmt.Sprintf("repos/%v/%v/security-advisories/%v", owner, repo, ghsaID), nil)
}

func advisoryRequest(client *github.Client, method, url string, body map[string]any) (*github.SecurityAdvisory, *github.Response, error) {
	req, err := client.NewRequest(ctx, method, url, body)
	if err != nil {
		return nil, nil, err
	}
	advisory := new(github.SecurityAdvisory)
	resp, err := client.Do(req, advisory)
	if err != nil {
		return nil, resp, err
	}
	return advisory, resp, nil
}

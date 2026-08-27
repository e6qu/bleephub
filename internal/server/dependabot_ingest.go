package bleephub

import (
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Server) deriveDependabotAlertsForRepository(repo *store.Repo) {
	if repo == nil {
		return
	}
	deps := s.currentDependencies(repo.ID, "refs/heads/"+repo.DefaultBranch, "")
	if len(deps) == 0 {
		return
	}
	for _, advisory := range s.store.ListGlobalAdvisories() {
		s.deriveDependabotAlertsForRepositoryAdvisory(repo, deps, advisory)
	}
}

func (s *Server) deriveDependabotAlertsForPublishedAdvisory(advisory *store.SecurityAdvisory) {
	if advisory == nil || advisory.PublishedAt == nil || advisory.State != "published" {
		return
	}
	s.store.Mu.RLock()
	repos := make([]*store.Repo, 0, len(s.store.Repos))
	for _, repo := range s.store.Repos {
		repos = append(repos, repo)
	}
	s.store.Mu.RUnlock()

	for _, repo := range repos {
		deps := s.currentDependencies(repo.ID, "refs/heads/"+repo.DefaultBranch, "")
		if len(deps) == 0 {
			continue
		}
		s.deriveDependabotAlertsForRepositoryAdvisory(repo, deps, advisory)
	}
}

func (s *Server) deriveDependabotAlertsForRepositoryAdvisory(repo *store.Repo, deps map[string]*dependencyEntry, advisory *store.SecurityAdvisory) {
	if advisory == nil || advisory.PublishedAt == nil || advisory.State != "published" {
		return
	}
	for _, vulnerability := range advisory.Vulnerabilities {
		for purl, dep := range deps {
			ecosystem, packageName, version := parsePurl(purl)
			if !dependabotPackageMatches(vulnerability, ecosystem, packageName) {
				continue
			}
			// Compare against the advisory's ecosystem, not the purl's: it states which version algebra its range was written in.
			if !store.VersionInVulnerableRange(vulnerability.PackageEcosystem, version, vulnerability.VulnerableVersionRange) {
				continue
			}
			alert, created := s.store.CreateDependabotAlertIfNewReported(repo.FullName, vulnerability.PackageName, store.NormalizeAdvisoryEcosystem(vulnerability.PackageEcosystem), dep.Manifest,
				advisory.GHSAID, advisory.CVEID, advisory.Severity, advisory.Summary, advisory.Description,
				vulnerability.VulnerableVersionRange, vulnerability.FirstPatchedVersion)
			if created {
				s.announceDependabotAlertCreated(repo, alert)
			}
		}
	}
}

func dependabotPackageMatches(v store.SecurityAdvisoryVulnerability, ecosystem, packageName string) bool {
	return store.NormalizeAdvisoryEcosystem(v.PackageEcosystem) == store.NormalizeAdvisoryEcosystem(ecosystem) &&
		strings.EqualFold(v.PackageName, packageName)
}

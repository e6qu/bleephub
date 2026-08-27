package bleephub

import (
	"net/http"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// The event half of the security-advisory vertical: the dependabot_alert,
// repository_advisory and security_advisory webhook families, plus the
// reconciliation that fixes and reopens alerts as dependencies change.

// announceDependabotAlertCreated delivers the dependabot_alert "created" event.
//
// The sender is Dependabot, not the snapshot submitter: attributing the finding
// to the submitting account would name a CI service account as its discoverer.
func (s *Server) announceDependabotAlertCreated(repo *store.Repo, alert *store.DependabotAlert) {
	s.emitDependabotAlertEvent(repo, alert, nil, "created")
}

func (s *Server) emitDependabotAlertEvent(repo *store.Repo, alert *store.DependabotAlert, sender *store.User, action string) {
	if repo == nil || alert == nil {
		return
	}
	var senderJSON map[string]interface{}
	if sender != nil {
		senderJSON = store.UserToJSON(sender, s.publicOrigin())
	}
	var dismisser map[string]interface{}
	if alert.DismissedByLogin != "" {
		if user := s.store.LookupUserByLogin(alert.DismissedByLogin); user != nil {
			dismisser = store.UserToJSON(user, s.publicOrigin())
		}
	}
	payload := store.DependabotAlertEventPayload(alert, repoPayload(repo, s.publicOrigin()), senderJSON, dismisser, action)
	s.emitWebhookEvent(repo.FullName, "dependabot_alert", action, payload)
}

// dependabotAlertActionFor names the dependabot_alert action a state change produces.
func dependabotAlertActionFor(previous, current store.DependabotAlertState) string {
	switch current {
	case store.DependabotStateDismissed:
		return "dismissed"
	case store.DependabotStateAutoDismissed:
		return "auto_dismissed"
	case store.DependabotStateFixed:
		return "fixed"
	case store.DependabotStateOpen:
		switch previous {
		case store.DependabotStateFixed:
			// GitHub calls a vulnerability returning after a code-fix "reintroduced".
			return "reintroduced"
		case store.DependabotStateAutoDismissed:
			return "auto_reopened"
		default:
			return "reopened"
		}
	}
	return ""
}

// reconcileDependabotAlerts re-evaluates open and fixed alerts against the current
// dependency set, fixing the no-longer-vulnerable and reopening the vulnerable.
// Dismissed alerts are left alone: dismissal is a human judgement re-derivation may not overturn.
func (s *Server) reconcileDependabotAlerts(repo *store.Repo) {
	if repo == nil {
		return
	}
	vulnerable := s.vulnerableDependencyKeys(repo)
	for _, alert := range s.store.ListDependabotAlerts(repo.FullName, "", "", "", "", "", "created", "asc") {
		switch alert.State {
		case store.DependabotStateOpen:
			if vulnerable[dependabotAlertKey(alert)] {
				continue
			}
			if updated, ok := s.store.ResolveDependabotAlert(repo.FullName, alert.Number, store.DependabotStateFixed); ok {
				s.emitDependabotAlertEvent(repo, updated,
					nil, dependabotAlertActionFor(alert.State, updated.State))
			}
		case store.DependabotStateFixed:
			if !vulnerable[dependabotAlertKey(alert)] {
				continue
			}
			if updated, ok := s.store.ResolveDependabotAlert(repo.FullName, alert.Number, store.DependabotStateOpen); ok {
				s.emitDependabotAlertEvent(repo, updated,
					nil, dependabotAlertActionFor(alert.State, updated.State))
			}
		}
	}
}

// vulnerableDependencyKeys is the set of (manifest, ecosystem, package, advisory)
// tuples the repository is currently vulnerable through.
func (s *Server) vulnerableDependencyKeys(repo *store.Repo) map[string]bool {
	keys := map[string]bool{}
	manifests := s.store.ResolvedDependencyManifests(repo.ID, "refs/heads/"+repo.DefaultBranch, "")
	if len(manifests) == 0 {
		return keys
	}
	advisories := s.store.ListGlobalAdvisories()
	for _, advisory := range advisories {
		if advisory.PublishedAt == nil || advisory.State != "published" {
			continue
		}
		for _, vulnerability := range advisory.Vulnerabilities {
			for _, manifest := range manifests {
				for _, dependency := range manifest.Dependencies {
					if !dependabotPackageMatches(vulnerability, dependency.Ecosystem, dependency.Name) {
						continue
					}
					if !store.VersionInVulnerableRange(vulnerability.PackageEcosystem,
						dependency.Version, vulnerability.VulnerableVersionRange) {
						continue
					}
					keys[alertKey(manifest.Name, vulnerability.PackageEcosystem,
						vulnerability.PackageName, advisory.GHSAID)] = true
				}
			}
		}
	}
	return keys
}

// dependabotAlertKey is an existing alert's coordinate in the vulnerable set.
func dependabotAlertKey(alert *store.DependabotAlert) string {
	return alertKey(alert.ManifestPath, alert.PackageEcosystem, alert.PackageName, alert.VulnerabilityID)
}

// alertKey folds both sides of the comparison into one coordinate so differing
// ecosystem spellings cannot make one vulnerability look like two.
func alertKey(manifestPath, ecosystem, packageName, ghsaID string) string {
	return strings.Join([]string{
		manifestPath,
		store.NormalizeAdvisoryEcosystem(ecosystem),
		strings.ToLower(packageName),
		strings.ToUpper(ghsaID),
	}, "\x1f")
}

// --- security-access gating -------------------------------------------------

// viewerHasRepoSecurityAccess reports whether the request may read a repository's
// security findings (alerts and embargoed advisories).
//
// Readability is NOT the test: gating on read alone exposes a public repo's alerts —
// its exact vulnerable dependency versions — to any token holder. GitHub gates on
// write standing (or org owner / security manager), on public repos as on private.
func (s *Server) viewerHasRepoSecurityAccess(r *http.Request, repo *store.Repo) bool {
	return s.viewerMayActOnRepo(r.Context(), repo, store.ScopeSecurityEvents, store.PermRead, store.PermWrite)
}

// lookupSecurityReadableRepo resolves the path's repository, refusing with 404 unless
// the caller holds security access — a 403 would confirm the repo has findings to hide.
func (s *Server) lookupSecurityReadableRepo(w http.ResponseWriter, r *http.Request) *store.Repo {
	repo := s.lookupReadableRepoFromPath(w, r)
	if repo == nil {
		return nil
	}
	if !s.viewerHasRepoSecurityAccess(r, repo) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return repo
}

// --- repository_advisory and security_advisory ------------------------------

// announceAdvisoryPublication decides, from the advisory's state and whether it was
// already public, which transition happened and emits its events. A still-draft
// advisory emits nothing: that is the embargo — a webhook would broadcast it.
func (s *Server) announceAdvisoryPublication(repo *store.Repo, advisory *store.SecurityAdvisory, sender *store.User, publishedBefore bool) {
	if advisory == nil || advisory.PublishedAt == nil {
		return
	}
	switch {
	case !publishedBefore && advisory.State == "published":
		// Derive before emitting so a subscriber sees the alerts the advisory caused.
		s.deriveDependabotAlertsForPublishedAdvisory(advisory)
		s.emitRepositoryAdvisoryEvent(repo, advisory, sender, "published")
		s.emitSecurityAdvisoryEvent(advisory, "published")
	case publishedBefore && advisory.State == "withdrawn":
		s.emitSecurityAdvisoryEvent(advisory, "withdrawn")
	case publishedBefore && advisory.State == "published":
		s.emitSecurityAdvisoryEvent(advisory, "updated")
	}
}

// emitRepositoryAdvisoryEvent delivers a repository_advisory webhook. A draft under
// embargo produces none; only "reported" and "published" leave the repository.
func (s *Server) emitRepositoryAdvisoryEvent(repo *store.Repo, advisory *store.SecurityAdvisory, sender *store.User, action string) {
	if repo == nil || advisory == nil {
		return
	}
	payload := map[string]interface{}{
		"action":              action,
		"repository_advisory": securityAdvisoryToJSON(advisory, repo, s.baseURLFromConfig(), s.store),
		"repository":          repoPayload(repo, s.publicOrigin()),
	}
	if sender != nil {
		payload["sender"] = store.UserToJSON(sender, s.publicOrigin())
	}
	s.emitWebhookEvent(repo.FullName, "repository_advisory", action, payload)
}

// emitSecurityAdvisoryEvent delivers a security_advisory webhook. GitHub delivers it
// to app installations; bleephub keys delivery by repository, fanning out to the repos
// carrying an alert against the advisory.
func (s *Server) emitSecurityAdvisoryEvent(advisory *store.SecurityAdvisory, action string) {
	if advisory == nil {
		return
	}
	payload := map[string]interface{}{
		"action":            action,
		"security_advisory": s.globalAdvisoryToJSON(advisory, s.baseURLFromConfig()),
	}
	for _, repoKey := range s.repositoriesAffectedByAdvisory(advisory) {
		s.emitWebhookEvent(repoKey, "security_advisory", action, payload)
	}
}

// repositoriesAffectedByAdvisory names the repositories carrying an alert against
// an advisory, in a stable order.
func (s *Server) repositoriesAffectedByAdvisory(advisory *store.SecurityAdvisory) []string {
	seen := map[string]bool{}
	var repoKeys []string
	for _, repo := range s.store.SnapshotAllRepos() {
		for _, alert := range s.store.ListDependabotAlerts(repo.FullName, "", "", "", "", "", "created", "asc") {
			if !strings.EqualFold(alert.VulnerabilityID, advisory.GHSAID) || seen[repo.FullName] {
				continue
			}
			seen[repo.FullName] = true
			repoKeys = append(repoKeys, repo.FullName)
		}
	}
	return repoKeys
}

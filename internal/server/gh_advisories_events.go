package bleephub

import (
	"net/http"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// The event half of the security-advisory vertical: the dependabot_alert,
// repository_advisory and security_advisory webhook families, and the
// reconciliation that decides which of their actions a change produces.
//
// # Why reconciliation lives here rather than in the derivation
//
// Derivation answers one question — "does any declared dependency fall inside
// any published advisory's vulnerable range" — and it can only ever add. An
// alert's later life is the other half: a dependency upgraded past the
// vulnerable range fixes its alert, and a downgrade back into the range
// reintroduces it. Without that half, a repository that fixed everything
// still reports every alert it ever had as open, and no subscriber is ever
// told a vulnerability went away.
//
// Both halves run on the same trigger — a dependency submission, or an
// advisory reaching publication — because both read the same input: the
// repository's current dependency set against the published advisories.

// announceDependabotAlertCreated delivers the dependabot_alert "created"
// event for an alert derivation has just minted.
//
// The sender is Dependabot itself rather than whoever submitted the snapshot
// that revealed the vulnerability: the alert is the platform's finding, and
// attributing it to the submitting account would report a CI service account
// as the discoverer of every vulnerability in the instance.
func (s *Server) announceDependabotAlertCreated(repo *store.Repo, alert *store.DependabotAlert) {
	s.emitDependabotAlertEvent(repo, alert, nil, "created")
}

// emitDependabotAlertEvent delivers one dependabot_alert webhook.
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

// dependabotAlertActionFor names the dependabot_alert action a state change
// produces. GitHub distinguishes the four ways an alert can move, and a
// subscriber keys on the action rather than diffing the state itself.
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
			// The vulnerable version came back after the alert was closed by
			// a code change, which is GitHub's "reintroduced", not "reopened".
			return "reintroduced"
		case store.DependabotStateAutoDismissed:
			return "auto_reopened"
		default:
			return "reopened"
		}
	}
	return ""
}

// reconcileDependabotAlerts re-evaluates every open and fixed alert of a
// repository against its current dependency set and the published advisories,
// closing the ones whose dependency is no longer vulnerable and reopening the
// ones whose vulnerability came back.
//
// A dismissed alert is left alone: dismissal is a person's judgement about a
// vulnerability, and re-derivation is not entitled to overturn it.
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

// vulnerableDependencyKeys is the set of (manifest, ecosystem, package,
// advisory) tuples the repository is currently vulnerable through — the same
// match derivation performs, collected rather than turned into alerts.
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

// alertKey builds the coordinate both sides of the comparison are folded
// into, so a purl's ecosystem spelling and an advisory's cannot make the same
// vulnerability look like two.
func alertKey(manifestPath, ecosystem, packageName, ghsaID string) string {
	return strings.Join([]string{
		manifestPath,
		store.NormalizeAdvisoryEcosystem(ecosystem),
		strings.ToLower(packageName),
		strings.ToUpper(ghsaID),
	}, "\x1f")
}

// --- security-access gating -------------------------------------------------

// viewerHasRepoSecurityAccess reports whether the request may read a
// repository's security findings: the alerts, and the advisories still under
// embargo.
//
// Readability of the repository is NOT the test, and that distinction is the
// whole point of this predicate. A public repository is readable by everybody,
// so gating on read alone published every one of its Dependabot alerts — a
// list of the exact vulnerable dependency versions it is running — and every
// embargoed draft advisory in it to any account holding a token. GitHub gates
// both on write standing (or organization owner / security manager), on public
// repositories exactly as on private ones, because the finding is sensitive
// even when the code is not.
//
// The credential half is asked as security_events at read, which is the grant
// an app is given to read findings; the standing half is push, which is what
// resourceCapabilityFor already resolves that scope's writes to.
func (s *Server) viewerHasRepoSecurityAccess(r *http.Request, repo *store.Repo) bool {
	return s.viewerMayActOnRepo(r.Context(), repo, store.ScopeSecurityEvents, store.PermRead, store.PermWrite)
}

// lookupSecurityReadableRepo resolves the path's repository and refuses the
// request unless the caller holds security access to it.
//
// The refusal is 404 rather than 403 for the same reason the repository
// lookup masks a private repository: a 403 here would confirm that a
// repository has security findings to hide, which is itself information the
// caller is not entitled to.
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

// announceAdvisoryPublication is the one entry point the advisory write paths
// call after a create or an update. It decides, from the advisory's state and
// whether it was already public, which of the three transitions happened and
// which events each produces:
//
//	draft → published    repository_advisory "published" + security_advisory
//	                     "published", and derivation against every repository
//	published → updated  security_advisory "updated"
//	published → withdrawn security_advisory "withdrawn"
//
// An advisory that is still a draft produces nothing at all. That is the
// embargo: until it is published, the advisory is the security team's, and a
// webhook would hand it to everyone subscribed to the repository.
func (s *Server) announceAdvisoryPublication(repo *store.Repo, advisory *store.SecurityAdvisory, sender *store.User, publishedBefore bool) {
	if advisory == nil || advisory.PublishedAt == nil {
		return
	}
	switch {
	case !publishedBefore && advisory.State == "published":
		// Derivation runs before the events so a subscriber receiving
		// security_advisory "published" can already see the alerts it caused.
		s.deriveDependabotAlertsForPublishedAdvisory(advisory)
		s.emitRepositoryAdvisoryEvent(repo, advisory, sender, "published")
		s.emitSecurityAdvisoryEvent(advisory, "published")
	case publishedBefore && advisory.State == "withdrawn":
		s.emitSecurityAdvisoryEvent(advisory, "withdrawn")
	case publishedBefore && advisory.State == "published":
		s.emitSecurityAdvisoryEvent(advisory, "updated")
	}
}

// emitRepositoryAdvisoryEvent delivers a repository_advisory webhook, the
// event a repository's subscribers receive about advisories drafted in it.
//
// The private drafting workflow deliberately produces no event: an advisory
// under embargo is visible only to the people working on it, and a webhook is
// a broadcast. Only "reported" (somebody filed a private vulnerability
// report) and "published" (the embargo ended) leave the repository.
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

// emitSecurityAdvisoryEvent delivers a security_advisory webhook — the global
// advisory database's event, which reaches every repository whose
// subscribers asked for it rather than only the drafting repository's.
//
// GitHub delivers this one to app installations rather than to repository
// hooks. bleephub's delivery is keyed by repository, so it fans out to the
// repositories the advisory actually concerns: the ones with an alert against
// it. An advisory nobody is exposed to is still published; it simply has no
// repository to tell.
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

// repositoriesAffectedByAdvisory names the repositories carrying an alert
// against an advisory, in a stable order.
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

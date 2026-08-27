package store

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

// Node-id lookup and event-payload rendering for Dependabot alerts, shared by
// the REST handlers and the GraphQL resolvers.

// LookupDependabotAlertByNodeID returns a detached snapshot of the alert.
//
// Not spelt Find*ByNodeID (which signals a LIVE row) because the dismissal
// mutation reads pre-change state to decide the webhook action; a live row
// would report the post-change state instead.
func (st *Store) LookupDependabotAlertByNodeID(nodeID string) *DependabotAlert {
	if nodeID == "" {
		return nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, byNumber := range st.DependabotAlertsByRepo {
		for _, alert := range byNumber {
			if alert != nil && alert.NodeID == nodeID {
				return cloneDependabotAlert(alert)
			}
		}
	}
	return nil
}

// CreateDependabotAlertIfNewReported is CreateDependabotAlertIfNew that also
// reports whether this call minted the alert. Derivation runs on every
// dependency submission and advisory publication, so the flag lets the caller
// deliver a "created" webhook only for genuinely new alerts, not on every
// re-derivation.
func (st *Store) CreateDependabotAlertIfNewReported(repoKey, pkgName, ecosystem, manifest, vulnID, cveID, severity, summary, description, vulnRange, patched string) (*DependabotAlert, bool) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	for _, alert := range st.DependabotAlertsByRepo[repoKey] {
		if strings.EqualFold(alert.PackageName, pkgName) &&
			strings.EqualFold(alert.PackageEcosystem, ecosystem) &&
			alert.ManifestPath == manifest &&
			alert.VulnerabilityID == vulnID {
			return cloneDependabotAlert(alert), false
		}
	}
	created := st.CreateDependabotAlertLocked(repoKey, pkgName, ecosystem, manifest, vulnID, cveID,
		severity, "open", summary, description, vulnRange, patched)
	return cloneDependabotAlert(created), true
}

// ResolveDependabotAlert applies platform-driven transitions ("fixed" when the
// dependency is no longer vulnerable, "open" on reintroduction). Separate from
// the user-driven UpdateDependabotAlert (open ⇄ dismissed), which must keep
// refusing a client's own "fixed". Leaves a dismissed alert alone: a human
// decision outranks a re-derivation.
func (st *Store) ResolveDependabotAlert(repoKey string, number int, state DependabotAlertState) (*DependabotAlert, bool) {
	if state != DependabotStateFixed && state != DependabotStateOpen {
		return nil, false
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()

	live := st.DependabotAlertsByRepo[repoKey][number]
	if live == nil || live.State == state {
		return nil, false
	}
	if live.State == DependabotStateDismissed || live.State == DependabotStateAutoDismissed {
		return nil, false
	}

	now := st.CurrentTime()
	switch state {
	case DependabotStateFixed:
		live.State = DependabotStateFixed
		live.FixedAt = &now
	case DependabotStateOpen:
		live.State = DependabotStateOpen
		live.FixedAt = nil
	}
	live.UpdatedAt = now
	st.persistDependabotAlert(live)
	return cloneDependabotAlert(live), true
}

// DependabotAlertGraphQLState renders an alert's state as the
// RepositoryVulnerabilityAlertState enum member GraphQL names.
func DependabotAlertGraphQLState(state DependabotAlertState) string {
	switch state {
	case DependabotStateDismissed:
		return "DISMISSED"
	case DependabotStateFixed:
		return "FIXED"
	case DependabotStateAutoDismissed:
		return "AUTO_DISMISSED"
	default:
		return "OPEN"
	}
}

// dependabotDismissReasons maps GraphQL's DismissReason enum onto the REST
// dismissed_reason spelling the store persists.
var dependabotDismissReasons = map[string]string{
	"FIX_STARTED":    "fix_started",
	"INACCURATE":     "inaccurate",
	"NOT_USED":       "not_used",
	"NO_BANDWIDTH":   "no_bandwidth",
	"TOLERABLE_RISK": "tolerable_risk",
}

// DependabotDismissReasonFromGraphQL translates a DismissReason enum member to
// its stored spelling, reporting false for a value outside the enum.
func DependabotDismissReasonFromGraphQL(reason string) (string, bool) {
	stored, ok := dependabotDismissReasons[strings.ToUpper(strings.TrimSpace(reason))]
	return stored, ok
}

// dependabotDismissReasonText is the prose GitHub shows on
// RepositoryVulnerabilityAlert.dismissReason.
var dependabotDismissReasonText = map[string]string{
	"fix_started":    "A fix has already been started",
	"inaccurate":     "This alert is inaccurate or incorrect",
	"not_used":       "Vulnerable code is not actually used",
	"no_bandwidth":   "No bandwidth to fix this",
	"tolerable_risk": "Risk is tolerable to this project",
}

// DependabotDismissReasonText renders a stored dismissal reason as prose, or ""
// when the alert was never dismissed.
func DependabotDismissReasonText(reason string) string {
	return dependabotDismissReasonText[reason]
}

// DependabotAlertManifestFilename is the manifest's base name.
func DependabotAlertManifestFilename(manifestPath string) string {
	if manifestPath == "" {
		return ""
	}
	return path.Base(manifestPath)
}

// DependencyGraphManifestNodeID derives a manifest's node id from (repo, path).
// Manifests have no row of their own; they are a view over the latest
// submitted snapshots.
func DependencyGraphManifestNodeID(repoID int, filename string) string {
	return fmt.Sprintf("DGM_%d_%s", repoID, filename)
}

// ParseDependencyGraphManifestNodeID reverses DependencyGraphManifestNodeID.
func ParseDependencyGraphManifestNodeID(nodeID string) (repoID int, filename string, ok bool) {
	rest, found := strings.CutPrefix(nodeID, "DGM_")
	if !found {
		return 0, "", false
	}
	separator := strings.IndexByte(rest, '_')
	if separator <= 0 {
		return 0, "", false
	}
	id, err := strconv.Atoi(rest[:separator])
	if err != nil {
		return 0, "", false
	}
	return id, rest[separator+1:], true
}

// CWENodeID is the node id a CWE is addressed by.
func CWENodeID(cweID string) string { return "CWE_" + NormalizeCWEID(cweID) }

// NormalizeCWEID renders a CWE identifier in canonical "CWE-79" form, accepting
// a bare number.
func NormalizeCWEID(cweID string) string {
	cweID = strings.TrimSpace(cweID)
	if cweID == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToUpper(cweID), "CWE-") {
		return "CWE-" + cweID[4:]
	}
	return "CWE-" + cweID
}

// DependabotAlertEventPayload renders a dependabot_alert webhook body. Shared
// by the REST routes and the GraphQL dismissal mutation so a subscriber can't
// tell which surface moved the alert. repository, sender and dismisser arrive
// already rendered (their hypermedia is the HTTP layer's); dismisser is nil
// for an alert that stands.
func DependabotAlertEventPayload(alert *DependabotAlert, repository, sender, dismisser map[string]interface{}, action string) map[string]interface{} {
	if alert == nil {
		return nil
	}
	firstPatched := interface{}(nil)
	if alert.FirstPatchedVersion != "" {
		firstPatched = map[string]interface{}{"identifier": alert.FirstPatchedVersion}
	}
	identifiers := []map[string]interface{}{{"type": "GHSA", "value": alert.VulnerabilityID}}
	if alert.CVEID != "" {
		identifiers = append(identifiers, map[string]interface{}{"type": "CVE", "value": alert.CVEID})
	}
	vulnerability := map[string]interface{}{
		"package": map[string]interface{}{
			"ecosystem": NormalizeAdvisoryEcosystem(alert.PackageEcosystem),
			"name":      alert.PackageName,
		},
		"severity":                 alert.Severity,
		"vulnerable_version_range": alert.VulnerableVersionRange,
		"first_patched_version":    firstPatched,
	}
	payload := map[string]interface{}{
		"action": action,
		"alert": map[string]interface{}{
			"number":                 alert.Number,
			"state":                  string(alert.State),
			"dependency":             dependabotAlertDependencyPayload(alert),
			"security_advisory":      dependabotAlertAdvisoryPayload(alert, identifiers, vulnerability),
			"security_vulnerability": vulnerability,
			"url":                    fmt.Sprintf("/repos/%s/dependabot/alerts/%d", alert.RepoKey, alert.Number),
			"html_url":               fmt.Sprintf("/%s/security/dependabot/%d", alert.RepoKey, alert.Number),
			"created_at":             alert.CreatedAt.UTC().Format(time.RFC3339),
			"updated_at":             alert.UpdatedAt.UTC().Format(time.RFC3339),
			"dismissed_at":           nullableInstant(alert.DismissedAt),
			"dismissed_by":           nullOrMap(dismisser),
			"dismissed_reason":       nullOrStringValue(alert.DismissedReason),
			"dismissed_comment":      nullOrStringValue(alert.DismissedComment),
			"fixed_at":               nullableInstant(alert.FixedAt),
			"auto_dismissed_at":      nullableInstant(alert.AutoDismissedAt),
		},
	}
	if repository != nil {
		payload["repository"] = repository
	}
	if sender != nil {
		payload["sender"] = sender
	}
	return payload
}

func dependabotAlertDependencyPayload(alert *DependabotAlert) map[string]interface{} {
	return map[string]interface{}{
		"package": map[string]interface{}{
			"ecosystem": NormalizeAdvisoryEcosystem(alert.PackageEcosystem),
			"name":      alert.PackageName,
		},
		"manifest_path": alert.ManifestPath,
		"scope":         nil,
	}
}

func dependabotAlertAdvisoryPayload(alert *DependabotAlert, identifiers []map[string]interface{}, vulnerability map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"ghsa_id":         alert.VulnerabilityID,
		"cve_id":          nullOrStringValue(alert.CVEID),
		"summary":         alert.Summary,
		"description":     alert.Description,
		"severity":        alert.Severity,
		"identifiers":     identifiers,
		"references":      []interface{}{},
		"published_at":    alert.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":      alert.UpdatedAt.UTC().Format(time.RFC3339),
		"withdrawn_at":    nil,
		"vulnerabilities": []map[string]interface{}{vulnerability},
	}
}

func nullableInstant(at *time.Time) interface{} {
	if at == nil {
		return nil
	}
	return at.UTC().Format(time.RFC3339)
}

// nullOrMap renders an absent nested object as JSON null, not an empty object,
// which a typed SDK decodes differently.
func nullOrMap(value map[string]interface{}) interface{} {
	if value == nil {
		return nil
	}
	return value
}

// nullOrStringValue renders "" as JSON null, distinguishing "not set" from
// "set to empty".
func nullOrStringValue(value string) interface{} {
	if value == "" {
		return nil
	}
	return value
}

package store

import (
	"sort"
	"strings"
	"time"
)

// The global advisory database: the published advisories every account on the
// instance can read, whatever repository drafted them.
//
// A repository advisory is private while it is drafted and belongs to the
// repository's security team; publishing it moves it into this database,
// where it is public. That is the whole visibility rule, and it is why these
// lookups take no viewer: the caller has already been told the answer is
// public. Anything that has not been published is unreachable from here.

// GlobalAdvisoryFilter narrows a global-advisory listing. A zero filter
// matches every published advisory.
type GlobalAdvisoryFilter struct {
	// GHSAID and CVEID select one advisory by identifier.
	GHSAID string
	CVEID  string
	// Ecosystem and Package narrow to advisories with a vulnerability in
	// that ecosystem and/or against that package.
	Ecosystem string
	Package   string
	// Severities, when non-empty, keeps only advisories at one of them.
	Severities []string
	// PublishedSince and UpdatedSince keep only advisories at or after the
	// given instant.
	PublishedSince *time.Time
	UpdatedSince   *time.Time
	// IncludeWithdrawn keeps withdrawn advisories in the result. They stay
	// addressable by GHSA ID either way — a client that asks for a specific
	// withdrawn advisory is told it was withdrawn rather than that it never
	// existed — but they leave the browse listing.
	IncludeWithdrawn bool
}

// ListGlobalAdvisoriesFiltered returns the published advisories matching the
// filter as detached snapshots (STORE-021), newest publication first.
func (st *Store) ListGlobalAdvisoriesFiltered(filter GlobalAdvisoryFilter) []*SecurityAdvisory {
	st.Mu.RLock()
	advisories := make([]*SecurityAdvisory, 0, len(st.SecurityAdvisories))
	for _, advisory := range st.SecurityAdvisories {
		if !advisoryIsGlobal(advisory) {
			continue
		}
		advisories = append(advisories, cloneSecurityAdvisory(advisory))
	}
	st.Mu.RUnlock()

	matched := advisories[:0]
	for _, advisory := range advisories {
		if advisoryMatchesGlobalFilter(advisory, filter) {
			matched = append(matched, advisory)
		}
	}
	sortAdvisoriesByPublication(matched)
	return snapshotSlice(matched)
}

// GetGlobalAdvisoryByGHSA returns one published advisory by its GHSA ID, as a
// detached snapshot, or nil when the instance has published none with that
// ID. A drafted advisory is deliberately invisible here: the global database
// is public, and a draft is not.
func (st *Store) GetGlobalAdvisoryByGHSA(ghsaID string) *SecurityAdvisory {
	if ghsaID == "" {
		return nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, advisory := range st.SecurityAdvisories {
		if advisoryIsGlobal(advisory) && strings.EqualFold(advisory.GHSAID, ghsaID) {
			return cloneSecurityAdvisory(advisory)
		}
	}
	return nil
}

// SnapshotAllRepos returns every repository on the instance as detached
// snapshots (STORE-021).
//
// It exists for the two instance-wide sweeps this vertical performs — deriving
// alerts when an advisory is published, and finding the repositories an
// advisory's event concerns. Both walk every repository and then do
// substantial work per repository, which must not happen under the store
// lock; taking a snapshot first is what lets the lock be released before any
// of it runs.
func (st *Store) SnapshotAllRepos() []*Repo {
	st.Mu.RLock()
	repos := make([]*Repo, 0, len(st.Repos))
	for _, repo := range st.Repos {
		repos = append(repos, repo)
	}
	st.Mu.RUnlock()
	sort.Slice(repos, func(i, j int) bool { return repos[i].ID < repos[j].ID })
	return snapshotSlice(repos)
}

// GlobalSecurityVulnerability pairs one vulnerability with the advisory that
// declares it. Query.securityVulnerabilities enumerates the pairs rather than
// the advisories, because one advisory can name several vulnerable packages
// and a client filtering by package wants the matching entry, not the whole
// advisory.
type GlobalSecurityVulnerability struct {
	Advisory      *SecurityAdvisory
	Vulnerability SecurityAdvisoryVulnerability
}

// ListGlobalVulnerabilities flattens the published advisories into their
// individual package vulnerabilities, filtered the same way.
func (st *Store) ListGlobalVulnerabilities(filter GlobalAdvisoryFilter) []GlobalSecurityVulnerability {
	wantEcosystem := ""
	if filter.Ecosystem != "" {
		wantEcosystem = NormalizeAdvisoryEcosystem(filter.Ecosystem)
	}
	var out []GlobalSecurityVulnerability
	for _, advisory := range st.ListGlobalAdvisoriesFiltered(filter) {
		for _, vulnerability := range advisory.Vulnerabilities {
			if wantEcosystem != "" && NormalizeAdvisoryEcosystem(vulnerability.PackageEcosystem) != wantEcosystem {
				continue
			}
			if filter.Package != "" && !strings.EqualFold(vulnerability.PackageName, filter.Package) {
				continue
			}
			out = append(out, GlobalSecurityVulnerability{Advisory: advisory, Vulnerability: vulnerability})
		}
	}
	return out
}

// AdvisoryWithdrawnAt reports when an advisory was withdrawn, or nil when it
// stands. Withdrawal is recorded as a state change rather than its own
// timestamp, so the update that made the change is when it happened.
func AdvisoryWithdrawnAt(advisory *SecurityAdvisory) *time.Time {
	if advisory == nil || advisory.State != "withdrawn" {
		return nil
	}
	withdrawn := advisory.UpdatedAt
	return &withdrawn
}

// advisoryIsGlobal reports whether an advisory has reached the public
// database: published (or published and later withdrawn), with a publication
// date to be ordered by.
func advisoryIsGlobal(advisory *SecurityAdvisory) bool {
	if advisory == nil || advisory.PublishedAt == nil {
		return false
	}
	return advisory.State == "published" || advisory.State == "withdrawn"
}

// advisoryMatchesGlobalFilter applies every narrowing the filter declares.
func advisoryMatchesGlobalFilter(advisory *SecurityAdvisory, filter GlobalAdvisoryFilter) bool {
	if !filter.IncludeWithdrawn && advisory.State == "withdrawn" {
		return false
	}
	if filter.GHSAID != "" && !strings.EqualFold(advisory.GHSAID, filter.GHSAID) {
		return false
	}
	if filter.CVEID != "" && !strings.EqualFold(advisory.CVEID, filter.CVEID) {
		return false
	}
	if len(filter.Severities) != 0 && !advisorySeverityIn(advisory.Severity, filter.Severities) {
		return false
	}
	if filter.PublishedSince != nil && advisory.PublishedAt.Before(*filter.PublishedSince) {
		return false
	}
	if filter.UpdatedSince != nil && advisory.UpdatedAt.Before(*filter.UpdatedSince) {
		return false
	}
	if filter.Ecosystem == "" && filter.Package == "" {
		return true
	}
	wantEcosystem := ""
	if filter.Ecosystem != "" {
		wantEcosystem = NormalizeAdvisoryEcosystem(filter.Ecosystem)
	}
	for _, vulnerability := range advisory.Vulnerabilities {
		if wantEcosystem != "" && NormalizeAdvisoryEcosystem(vulnerability.PackageEcosystem) != wantEcosystem {
			continue
		}
		if filter.Package != "" && !strings.EqualFold(vulnerability.PackageName, filter.Package) {
			continue
		}
		return true
	}
	return false
}

// advisorySeverityIn reports whether severity is one of the wanted ones,
// comparing case-insensitively so a GraphQL CRITICAL and a REST "critical"
// select the same advisories.
func advisorySeverityIn(severity string, wanted []string) bool {
	for _, candidate := range wanted {
		if strings.EqualFold(severity, candidate) {
			return true
		}
	}
	return false
}

// sortAdvisoriesByPublication orders advisories newest publication first,
// with the database id breaking ties so the order — and therefore every
// connection cursor derived from it — is stable across requests.
func sortAdvisoriesByPublication(advisories []*SecurityAdvisory) {
	sort.Slice(advisories, func(i, j int) bool {
		left, right := advisories[i], advisories[j]
		if !left.PublishedAt.Equal(*right.PublishedAt) {
			return left.PublishedAt.After(*right.PublishedAt)
		}
		return left.ID > right.ID
	})
}

// SortAdvisoriesByUpdate orders advisories by update time, newest first when
// descending. It is the ordering SecurityAdvisoryOrder's UPDATED_AT field
// names, kept beside its PUBLISHED_AT sibling so the two cannot drift on
// their tiebreak.
func SortAdvisoriesByUpdate(advisories []*SecurityAdvisory, ascending bool) {
	sort.SliceStable(advisories, func(i, j int) bool {
		left, right := advisories[i], advisories[j]
		if !left.UpdatedAt.Equal(right.UpdatedAt) {
			if ascending {
				return left.UpdatedAt.Before(right.UpdatedAt)
			}
			return left.UpdatedAt.After(right.UpdatedAt)
		}
		if ascending {
			return left.ID < right.ID
		}
		return left.ID > right.ID
	})
}

// SortAdvisoriesByPublicationOrder orders advisories by publication time in
// the requested direction, the PUBLISHED_AT field of SecurityAdvisoryOrder.
func SortAdvisoriesByPublicationOrder(advisories []*SecurityAdvisory, ascending bool) {
	sortAdvisoriesByPublication(advisories)
	if !ascending {
		return
	}
	for i, j := 0, len(advisories)-1; i < j; i, j = i+1, j-1 {
		advisories[i], advisories[j] = advisories[j], advisories[i]
	}
}

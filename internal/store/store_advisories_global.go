package store

import (
	"sort"
	"strings"
	"time"
)

// The global advisory database: the published advisories every account can
// read. Publishing a drafted repository advisory moves it here; these lookups
// take no viewer because unpublished advisories are unreachable from here.

// GlobalAdvisoryFilter narrows a global-advisory listing. A zero filter
// matches every published advisory.
type GlobalAdvisoryFilter struct {
	GHSAID string
	CVEID  string
	// Ecosystem and Package narrow to advisories with a matching vulnerability.
	Ecosystem string
	Package   string
	// Severities, when non-empty, keeps only advisories at one of them.
	Severities     []string
	PublishedSince *time.Time
	UpdatedSince   *time.Time
	// IncludeWithdrawn keeps withdrawn advisories in the browse listing. They
	// stay addressable by GHSA ID regardless.
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

// GetGlobalAdvisoryByGHSA returns one published advisory by its GHSA ID as a
// detached snapshot, or nil. Drafted advisories are invisible here.
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

// SnapshotAllRepos returns every repository as detached snapshots (STORE-021).
// The two instance-wide advisory sweeps do substantial per-repo work that must
// not run under the store lock, so they snapshot first.
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

// GlobalSecurityVulnerability pairs one vulnerability with its advisory.
// Query.securityVulnerabilities enumerates pairs, not advisories, so a client
// filtering by package gets the matching entry rather than the whole advisory.
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
// stands. Withdrawal has no timestamp of its own, so UpdatedAt stands in.
func AdvisoryWithdrawnAt(advisory *SecurityAdvisory) *time.Time {
	if advisory == nil || advisory.State != "withdrawn" {
		return nil
	}
	withdrawn := advisory.UpdatedAt
	return &withdrawn
}

// advisoryIsGlobal reports whether an advisory has reached the public
// database: published or withdrawn, with a publication date.
func advisoryIsGlobal(advisory *SecurityAdvisory) bool {
	if advisory == nil || advisory.PublishedAt == nil {
		return false
	}
	return advisory.State == "published" || advisory.State == "withdrawn"
}

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

// advisorySeverityIn compares case-insensitively so a GraphQL CRITICAL and a
// REST "critical" select the same advisories.
func advisorySeverityIn(severity string, wanted []string) bool {
	for _, candidate := range wanted {
		if strings.EqualFold(severity, candidate) {
			return true
		}
	}
	return false
}

// sortAdvisoriesByPublication orders advisories newest publication first, id
// breaking ties so connection cursors are stable across requests.
func sortAdvisoriesByPublication(advisories []*SecurityAdvisory) {
	sort.Slice(advisories, func(i, j int) bool {
		left, right := advisories[i], advisories[j]
		if !left.PublishedAt.Equal(*right.PublishedAt) {
			return left.PublishedAt.After(*right.PublishedAt)
		}
		return left.ID > right.ID
	})
}

// SortAdvisoriesByUpdate orders advisories by update time, the UPDATED_AT
// field of SecurityAdvisoryOrder.
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

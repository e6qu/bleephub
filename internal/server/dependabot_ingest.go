package bleephub

import (
	"strconv"
	"strings"
	"unicode"

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
			if !dependabotVersionInRange(version, vulnerability.VulnerableVersionRange) {
				continue
			}
			s.store.CreateDependabotAlertIfNew(repo.FullName, vulnerability.PackageName, normalizeDependabotEcosystem(vulnerability.PackageEcosystem), dep.Manifest,
				advisory.GHSAID, advisory.CVEID, advisory.Severity, advisory.Summary, advisory.Description,
				vulnerability.VulnerableVersionRange, vulnerability.FirstPatchedVersion)
		}
	}
}

func dependabotPackageMatches(v store.SecurityAdvisoryVulnerability, ecosystem, packageName string) bool {
	return normalizeDependabotEcosystem(v.PackageEcosystem) == normalizeDependabotEcosystem(ecosystem) &&
		strings.EqualFold(v.PackageName, packageName)
}

func normalizeDependabotEcosystem(ecosystem string) string {
	switch strings.ToLower(ecosystem) {
	case "pypi":
		return "pip"
	default:
		return strings.ToLower(ecosystem)
	}
}

func dependabotVersionInRange(version, rangeExpr string) bool {
	version = strings.TrimSpace(version)
	if version == "" || strings.TrimSpace(rangeExpr) == "" {
		return false
	}
	for _, part := range strings.Split(rangeExpr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !dependabotVersionMatchesConstraint(version, part) {
			return false
		}
	}
	return true
}

func dependabotVersionMatchesConstraint(version, constraint string) bool {
	for _, op := range []string{"<=", ">=", "<", ">", "="} {
		if strings.HasPrefix(constraint, op) {
			want := strings.TrimSpace(strings.TrimPrefix(constraint, op))
			cmp, ok := compareDependencyVersions(version, want)
			if !ok {
				// A package-version dialect we do not understand must not
				// silently turn a published vulnerability into "safe".
				// Dependabot can later reconcile a conservative alert, while
				// suppressing it here hides the advisory entirely.
				return true
			}
			switch op {
			case "<":
				return cmp < 0
			case "<=":
				return cmp <= 0
			case ">":
				return cmp > 0
			case ">=":
				return cmp >= 0
			case "=":
				return cmp == 0
			}
		}
	}
	cmp, ok := compareDependencyVersions(version, constraint)
	return !ok || cmp == 0
}

func compareDependencyVersions(left, right string) (int, bool) {
	leftParts, leftPrerelease, ok := dependencyVersionParts(left)
	if !ok {
		return 0, false
	}
	rightParts, rightPrerelease, ok := dependencyVersionParts(right)
	if !ok {
		return 0, false
	}
	max := len(leftParts)
	if len(rightParts) > max {
		max = len(rightParts)
	}
	for len(leftParts) < max {
		leftParts = append(leftParts, dependencyVersionPart{numeric: true})
	}
	for len(rightParts) < max {
		rightParts = append(rightParts, dependencyVersionPart{numeric: true})
	}
	for i := 0; i < max; i++ {
		leftPart, rightPart := leftParts[i], rightParts[i]
		if leftPart.numeric && rightPart.numeric {
			switch {
			case leftPart.number < rightPart.number:
				return -1, true
			case leftPart.number > rightPart.number:
				return 1, true
			}
			continue
		}
		if leftPart.numeric != rightPart.numeric {
			if leftPart.numeric {
				return -1, true
			}
			return 1, true
		}
		switch {
		case leftPart.text < rightPart.text:
			return -1, true
		case leftPart.text > rightPart.text:
			return 1, true
		}
	}
	// SemVer and the package ecosystems Dependabot supports all order a
	// prerelease below the corresponding final release.
	if len(leftPrerelease) != 0 || len(rightPrerelease) != 0 {
		if len(leftPrerelease) == 0 {
			return 1, true
		}
		if len(rightPrerelease) == 0 {
			return -1, true
		}
		max = len(leftPrerelease)
		if len(rightPrerelease) > max {
			max = len(rightPrerelease)
		}
		for i := 0; i < max; i++ {
			if i >= len(leftPrerelease) {
				return -1, true
			}
			if i >= len(rightPrerelease) {
				return 1, true
			}
			leftPart, rightPart := leftPrerelease[i], rightPrerelease[i]
			if leftPart.numeric && rightPart.numeric {
				switch {
				case leftPart.number < rightPart.number:
					return -1, true
				case leftPart.number > rightPart.number:
					return 1, true
				}
				continue
			}
			// SemVer numeric prerelease identifiers have lower precedence
			// than non-numeric identifiers.
			if leftPart.numeric != rightPart.numeric {
				if leftPart.numeric {
					return -1, true
				}
				return 1, true
			}
			switch {
			case leftPart.text < rightPart.text:
				return -1, true
			case leftPart.text > rightPart.text:
				return 1, true
			}
		}
	}
	return 0, true
}

type dependencyVersionPart struct {
	numeric bool
	number  int
	text    string
}

func dependencyVersionParts(version string) ([]dependencyVersionPart, []dependencyVersionPart, bool) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if i := strings.IndexByte(version, '+'); i >= 0 {
		version = version[:i]
	}
	preAt := strings.IndexAny(version, "-~")
	for i, r := range version {
		if unicode.IsLetter(r) && (preAt < 0 || i < preAt) {
			preAt = i
			break
		}
	}
	core, prerelease := version, ""
	if preAt >= 0 {
		core, prerelease = version[:preAt], version[preAt:]
	}
	parse := func(value string) ([]dependencyVersionPart, bool) {
		var parts []dependencyVersionPart
		for i := 0; i < len(value); {
			r := rune(value[i])
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				i++
				continue
			}
			start := i
			isNumber := unicode.IsDigit(r)
			for i < len(value) {
				current := rune(value[i])
				if unicode.IsDigit(current) != isNumber ||
					(!unicode.IsLetter(current) && !unicode.IsDigit(current)) {
					break
				}
				i++
			}
			token := strings.ToLower(value[start:i])
			if isNumber {
				number, err := strconv.Atoi(token)
				if err != nil {
					return nil, false
				}
				parts = append(parts, dependencyVersionPart{numeric: true, number: number})
			} else {
				parts = append(parts, dependencyVersionPart{text: token})
			}
		}
		return parts, len(parts) != 0
	}
	coreParts, ok := parse(core)
	if !ok {
		return nil, nil, false
	}
	var prereleaseParts []dependencyVersionPart
	if prerelease != "" {
		prereleaseParts, ok = parse(prerelease)
		if !ok {
			return nil, nil, false
		}
	}
	return coreParts, prereleaseParts, true
}

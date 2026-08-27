package store

import (
	"net/url"
	"sort"
	"strings"
)

// The repository's current dependency set, derived from dependency submission
// API snapshots (nothing parses manifests out of git). Per (job.correlator,
// detector.name) only the newest snapshot counts — the replacement semantics
// the submission API documents. Both the Dependabot alert derivation and
// GraphQL's dependencyGraphManifests read this one view.

// ResolvedDependency is one package a manifest resolves to.
type ResolvedDependency struct {
	// PackageURL is the dependency's purl, as submitted.
	PackageURL string
	// Ecosystem is the purl's type folded onto a canonical ecosystem key.
	Ecosystem string
	// Name and Version are the purl's remaining coordinates.
	Name    string
	Version string
	// Relationship is "direct", "indirect" or "" when unsaid; GraphQL renders
	// the unsaid case as "unknown".
	Relationship string
	// Scope is "runtime", "development" or "" when unsaid.
	Scope string
	// DependsOn names the purls this dependency itself pulls in.
	DependsOn []string
}

// ResolvedManifest is one manifest of the repository's current dependency set.
type ResolvedManifest struct {
	Name           string
	SourceLocation string
	Dependencies   []ResolvedDependency
}

// ResolvedDependencyManifests returns the repository's current dependency
// manifests for a ref (or exact commit when sha is given), newest snapshot per
// detector, manifests ordered by name and dependencies by purl. The ordering
// is load-bearing: the underlying maps would otherwise yield GraphQL cursors
// that move between requests.
func (st *Store) ResolvedDependencyManifests(repoID int, ref, sha string) []ResolvedManifest {
	latest := map[string]*DependencySnapshot{}
	for _, snapshot := range st.ListDependencySnapshots(repoID) {
		// INVALID submissions are stored to serve their response but never
		// contribute to the dependency set.
		if snapshot.Result == "INVALID" {
			continue
		}
		if sha != "" && snapshot.Sha != sha {
			continue
		}
		if sha == "" && snapshot.Ref != ref {
			continue
		}
		key := snapshot.Job.Correlator + "\x1f" + snapshot.Detector.Name
		if current, seen := latest[key]; !seen || snapshot.ID > current.ID {
			latest[key] = snapshot
		}
	}

	byName := map[string]*ResolvedManifest{}
	for _, snapshot := range latest {
		for _, manifest := range snapshot.Manifests {
			if manifest == nil {
				continue
			}
			name := manifest.Name
			if name == "" {
				continue
			}
			merged := byName[name]
			if merged == nil {
				merged = &ResolvedManifest{Name: name}
				byName[name] = merged
			}
			if manifest.File != nil && manifest.File.SourceLocation != "" {
				merged.SourceLocation = manifest.File.SourceLocation
			}
			for _, dependency := range manifest.Resolved {
				if dependency == nil || dependency.PackageURL == "" {
					continue
				}
				ecosystem, packageName, version := ParsePackageURL(dependency.PackageURL)
				merged.Dependencies = append(merged.Dependencies, ResolvedDependency{
					PackageURL:   dependency.PackageURL,
					Ecosystem:    ecosystem,
					Name:         packageName,
					Version:      version,
					Relationship: dependency.Relationship,
					Scope:        dependency.Scope,
					DependsOn:    append([]string(nil), dependency.Dependencies...),
				})
			}
		}
	}

	manifests := make([]ResolvedManifest, 0, len(byName))
	for _, manifest := range byName {
		sort.Slice(manifest.Dependencies, func(i, j int) bool {
			return manifest.Dependencies[i].PackageURL < manifest.Dependencies[j].PackageURL
		})
		manifests = append(manifests, *manifest)
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].Name < manifests[j].Name })
	return manifests
}

// LookupResolvedDependency finds one dependency by manifest path, ecosystem and
// package name, reporting false when the repository no longer declares it. The
// false is load-bearing: a Dependabot alert outlives the dependency it was
// raised on, and a zero value would misread as "runtime, direct".
func (st *Store) LookupResolvedDependency(repoID int, ref, manifestPath, ecosystem, packageName string) (ResolvedDependency, bool) {
	wantEcosystem := NormalizeAdvisoryEcosystem(ecosystem)
	for _, manifest := range st.ResolvedDependencyManifests(repoID, ref, "") {
		if manifestPath != "" && manifest.Name != manifestPath {
			continue
		}
		for _, dependency := range manifest.Dependencies {
			if NormalizeAdvisoryEcosystem(dependency.Ecosystem) != wantEcosystem {
				continue
			}
			if strings.EqualFold(dependency.Name, packageName) {
				return dependency, true
			}
		}
	}
	return ResolvedDependency{}, false
}

// ParsePackageURL splits a package-url into ecosystem type, name and version.
// The name keeps its namespace ("@babel/core"), the coordinate advisories are
// written against.
func ParsePackageURL(purl string) (ecosystem, name, version string) {
	rest := strings.TrimPrefix(purl, "pkg:")
	rest = strings.TrimPrefix(rest, "/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		ecosystem, rest = rest[:i], rest[i+1:]
	} else {
		ecosystem, rest = rest, ""
	}
	// Qualifiers and subpath follow the version and are not coordinates.
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndexByte(rest, '@'); i >= 0 {
		version, rest = rest[i+1:], rest[:i]
	}
	if decoded, err := url.PathUnescape(rest); err == nil {
		rest = decoded
	}
	if decoded, err := url.PathUnescape(version); err == nil {
		version = decoded
	}
	return ecosystem, rest, version
}

// DependencyPackageManager renders a canonical ecosystem key as GitHub's
// DependencyGraphDependency.packageManager name.
func DependencyPackageManager(ecosystem string) string {
	switch NormalizeAdvisoryEcosystem(ecosystem) {
	case EcosystemNPM:
		return "NPM"
	case EcosystemPip:
		return "PIP"
	case EcosystemMaven:
		return "MAVEN"
	case EcosystemNuGet:
		return "NUGET"
	case EcosystemRubyGems:
		return "RUBYGEMS"
	case EcosystemGo:
		return "GO"
	case EcosystemComposer:
		return "COMPOSER"
	case EcosystemRust:
		return "RUST"
	case EcosystemActions:
		return "ACTIONS"
	case EcosystemPub:
		return "PUB"
	case EcosystemErlang:
		return "HEX"
	case EcosystemSwift:
		return "SWIFT"
	}
	return ""
}

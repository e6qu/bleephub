package store

import (
	"math"
	"testing"
)

// TestCompareEcosystemVersionsPerEcosystem pins the orderings that differ
// between ecosystems for the same pair of strings. Each case names the pair
// and the ecosystem, because the point of the table is that a generic
// comparator gets several of these wrong in both directions.
func TestCompareEcosystemVersionsPerEcosystem(t *testing.T) {
	cases := []struct {
		name      string
		ecosystem string
		left      string
		right     string
		want      int
	}{
		// SemVer 2.0.0 (npm and friends).
		{"npm patch", "npm", "1.2.3", "1.2.4", -1},
		{"npm minor beats patch", "npm", "1.3.0", "1.2.99", 1},
		{"npm missing segments are zero", "npm", "1.2", "1.2.0", 0},
		{"npm prerelease below release", "npm", "1.0.0-rc.1", "1.0.0", -1},
		{"npm prerelease ordering", "npm", "1.0.0-alpha.1", "1.0.0-alpha.2", -1},
		{"npm numeric identifier below alphanumeric", "npm", "1.0.0-1", "1.0.0-alpha", -1},
		{"npm longer prerelease set wins", "npm", "1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"npm build metadata ignored", "npm", "1.0.0+build.9", "1.0.0+build.1", 0},
		{"go module v prefix", "go", "v1.4.0", "1.5.0", -1},
		{"rust crate", "rust", "0.8.10", "0.8.9", 1},
		{"actions tag", "actions", "v3", "v4", -1},
		{"swift package", "swift", "5.9.1", "5.10.0", -1},
		{"erlang hex", "erlang", "2.0.0-rc.3", "2.0.0", -1},
		{"pub package", "pub", "3.1.0", "3.1.0", 0},

		// PEP 440. A post-release is NEWER than its release and a dev
		// release is older than everything; SemVer reads both backwards.
		{"pip post above release", "pip", "1.0.post1", "1.0", 1},
		{"pip implicit post spelling", "pip", "1.0-1", "1.0", 1},
		{"pip rev spelling", "pip", "1.0rev2", "1.0.post1", 1},
		{"pip dev below release", "pip", "1.0.dev1", "1.0", -1},
		{"pip dev below prerelease", "pip", "1.0a1.dev1", "1.0a1", -1},
		{"pip rc below release", "pip", "1.0rc1", "1.0", -1},
		{"pip alpha below beta", "pip", "1.0a2", "1.0b1", -1},
		{"pip c is rc", "pip", "1.0c1", "1.0rc1", 0},
		{"pip separator insensitivity", "pip", "1.0-alpha-1", "1.0a1", 0},
		{"pip epoch dominates", "pip", "1!1.0", "9.9.9", 1},
		{"pip local label above bare", "pip", "1.0+ubuntu1", "1.0", 1},
		{"pip release segments", "pip", "2.31.0", "2.30.0", 1},

		// Maven. "sp" is the one qualifier newer than the release.
		{"maven service pack above release", "maven", "1.0-sp", "1.0", 1},
		{"maven rc below release", "maven", "1.0-rc1", "1.0", -1},
		{"maven snapshot below release", "maven", "1.0-SNAPSHOT", "1.0", -1},
		{"maven snapshot above rc", "maven", "1.0-SNAPSHOT", "1.0-rc1", 1},
		{"maven alpha alias", "maven", "1.0-a1", "1.0-alpha1", 0},
		{"maven milestone below rc", "maven", "1.0-m1", "1.0-rc1", -1},
		{"maven trailing zero is equal", "maven", "1.0", "1.0.0", 0},
		{"maven numeric beats qualifier", "maven", "1.0.1", "1.0-sp", 1},
		{"maven unknown qualifier above release", "maven", "1.0-zeta", "1.0", 1},
		{"maven numeric segments", "maven", "2.17.1", "2.17.2", -1},

		// RubyGems. A letter segment makes a prerelease.
		{"rubygems prerelease letter", "rubygems", "1.0.a", "1.0", -1},
		{"rubygems prerelease ordering", "rubygems", "1.0.0.beta1", "1.0.0.beta2", -1},
		{"rubygems trailing zero equal", "rubygems", "1.0", "1.0.0", 0},
		{"rubygems numeric segments", "rubygems", "6.1.7", "6.1.10", -1},
		{"rubygems letter split from digits", "rubygems", "1.0.0.rc1", "1.0.0", -1},

		// NuGet pads to four parts and compares prerelease case-insensitively.
		{"nuget four part padding", "nuget", "1.0", "1.0.0.0", 0},
		{"nuget revision", "nuget", "1.0.0.1", "1.0.0.0", 1},
		{"nuget prerelease case insensitive", "nuget", "1.0.0-Beta", "1.0.0-beta", 0},
		{"nuget prerelease below release", "nuget", "1.0.0-beta", "1.0.0", -1},

		// Composer / PHP version_compare.
		{"composer dev lowest", "composer", "1.0.0-dev", "1.0.0-alpha1", -1},
		{"composer patch level above release", "composer", "1.0.0-pl1", "1.0.0", 1},
		{"composer rc below release", "composer", "1.0.0-RC1", "1.0.0", -1},
		{"composer beta above alpha", "composer", "1.0.0-beta", "1.0.0-alpha", 1},
		{"composer numeric", "composer", "8.1.2", "8.1.3", -1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := CompareEcosystemVersions(testCase.ecosystem, testCase.left, testCase.right)
			if !ok {
				t.Fatalf("CompareEcosystemVersions(%q, %q, %q) could not parse the pair",
					testCase.ecosystem, testCase.left, testCase.right)
			}
			if got != testCase.want {
				t.Errorf("CompareEcosystemVersions(%q, %q, %q) = %d, want %d",
					testCase.ecosystem, testCase.left, testCase.right, got, testCase.want)
			}
			// Every comparator must be antisymmetric, or a range check
			// answers differently depending on which side the advisory
			// happens to write the bound on.
			reversed, ok := CompareEcosystemVersions(testCase.ecosystem, testCase.right, testCase.left)
			if !ok || reversed != -testCase.want {
				t.Errorf("CompareEcosystemVersions(%q, %q, %q) = %d, want %d (antisymmetry)",
					testCase.ecosystem, testCase.right, testCase.left, reversed, -testCase.want)
			}
		})
	}
}

// TestEcosystemsDisagreeOnTheSameStrings is the direct statement of why the
// comparison is ecosystem-aware at all: three version pairs that a single
// comparator cannot order correctly for every ecosystem at once.
func TestEcosystemsDisagreeOnTheSameStrings(t *testing.T) {
	cases := []struct {
		left, right string
		orderings   map[string]int
	}{
		{
			left:  "1.0.post1",
			right: "1.0",
			orderings: map[string]int{
				// PEP 440 makes a post-release newer than its release.
				"pip": 1,
				// RubyGems reads "post" as a prerelease-style word segment.
				"rubygems": -1,
			},
		},
		{
			left:  "1.0-sp1",
			right: "1.0",
			orderings: map[string]int{
				// Maven's service pack is the one qualifier above release.
				"maven": 1,
				// SemVer reads any hyphen suffix as a prerelease.
				"npm": -1,
			},
		},
		{
			left:  "1.0.0-dev",
			right: "1.0.0-alpha",
			orderings: map[string]int{
				// PHP ranks dev below alpha.
				"composer": -1,
				// SemVer compares the identifiers lexically: "dev" > "alpha".
				"npm": 1,
			},
		},
	}
	for _, testCase := range cases {
		for ecosystem, want := range testCase.orderings {
			got, ok := CompareEcosystemVersions(ecosystem, testCase.left, testCase.right)
			if !ok {
				t.Fatalf("%s could not compare %q and %q", ecosystem, testCase.left, testCase.right)
			}
			if got != want {
				t.Errorf("%s: compare(%q, %q) = %d, want %d",
					ecosystem, testCase.left, testCase.right, got, want)
			}
		}
	}
}

// TestVersionInVulnerableRange exercises the range grammar GitHub documents
// on SecurityVulnerability.vulnerableVersionRange.
func TestVersionInVulnerableRange(t *testing.T) {
	cases := []struct {
		name      string
		ecosystem string
		version   string
		rangeExpr string
		want      bool
	}{
		{"single vulnerable version hit", "npm", "0.2.0", "= 0.2.0", true},
		{"single vulnerable version miss", "npm", "0.2.1", "= 0.2.0", false},
		{"bare version is equality", "npm", "0.2.0", "0.2.0", true},
		{"upper bound inclusive", "npm", "1.0.8", "<= 1.0.8", true},
		{"upper bound exclusive", "npm", "0.1.11", "< 0.1.11", false},
		{"upper bound exclusive below", "npm", "0.1.10", "< 0.1.11", true},
		{"closed interval inside", "npm", "4.3.2", ">= 4.3.0, < 4.3.5", true},
		{"closed interval below", "npm", "4.2.9", ">= 4.3.0, < 4.3.5", false},
		{"closed interval at open end", "npm", "4.3.5", ">= 4.3.0, < 4.3.5", false},
		{"open-ended minimum", "npm", "9.9.9", ">= 0.0.1", true},
		{"prerelease inside interval", "npm", "4.3.1-rc.1", ">= 4.3.0, < 4.3.5", true},

		// The ecosystem changes the answer for byte-identical inputs. Each
		// pair below is one version string and one range that the two
		// ecosystems place on opposite sides of.
		{"pip post release is inside", "pip", "1.0.post1", "> 1.0, < 1.1", true},
		{"pip implicit post release is inside", "pip", "1.0-1", "> 1.0, < 1.1", true},
		{"npm reads the same string as a prerelease", "npm", "1.0-1", "> 1.0, < 1.1", false},
		{"maven service pack is inside", "maven", "1.0-sp1", "> 1.0, < 1.1", true},
		{"npm reads sp as a prerelease", "npm", "1.0-sp1", "> 1.0, < 1.1", false},

		{"empty range never matches", "npm", "1.0.0", "", false},
		{"empty version never matches", "npm", "", "< 2.0.0", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := VersionInVulnerableRange(testCase.ecosystem, testCase.version, testCase.rangeExpr)
			if got != testCase.want {
				t.Errorf("VersionInVulnerableRange(%q, %q, %q) = %v, want %v",
					testCase.ecosystem, testCase.version, testCase.rangeExpr, got, testCase.want)
			}
		})
	}
}

// TestUnparseableVersionStillRaisesTheAlert pins the deliberate direction of
// the unknown-dialect fallback: a version string no comparator can read must
// not silently rule the dependency safe.
func TestUnparseableVersionStillRaisesTheAlert(t *testing.T) {
	if _, ok := CompareEcosystemVersions("npm", "not-a-version", "1.0.0"); ok {
		t.Fatal("CompareEcosystemVersions claimed to order an unparseable version")
	}
	if !VersionInVulnerableRange("npm", "not-a-version", "< 1.0.0") {
		t.Error("an unreadable version suppressed the advisory instead of surfacing it")
	}
}

// TestNormalizeAdvisoryEcosystem pins the folding that lets a purl's
// spelling, the REST spelling and the GraphQL enum name one ecosystem.
func TestNormalizeAdvisoryEcosystem(t *testing.T) {
	cases := map[string]string{
		"pypi": EcosystemPip, "PIP": EcosystemPip, "Python": EcosystemPip,
		"golang": EcosystemGo, "GO": EcosystemGo,
		"cargo": EcosystemRust, "RUST": EcosystemRust,
		"gem": EcosystemRubyGems, "RUBYGEMS": EcosystemRubyGems,
		"hex": EcosystemErlang, "ERLANG": EcosystemErlang,
		"packagist": EcosystemComposer, "COMPOSER": EcosystemComposer,
		"npm": EcosystemNPM, "NPM": EcosystemNPM,
		"maven": EcosystemMaven, "MAVEN": EcosystemMaven,
		"nuget": EcosystemNuGet, "NUGET": EcosystemNuGet,
		"actions": EcosystemActions, "ACTIONS": EcosystemActions,
		"pub": EcosystemPub, "PUB": EcosystemPub,
		"swift": EcosystemSwift, "SWIFT": EcosystemSwift,
	}
	for input, want := range cases {
		if got := NormalizeAdvisoryEcosystem(input); got != want {
			t.Errorf("NormalizeAdvisoryEcosystem(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestAdvisoryEcosystemGraphQLRoundTrip pins that every canonical key the
// SecurityAdvisoryEcosystem enum covers renders as a member and folds back,
// and that a key the enum has no member for reports "" rather than
// inventing one.
func TestAdvisoryEcosystemGraphQLRoundTrip(t *testing.T) {
	for _, ecosystem := range []string{
		EcosystemNPM, EcosystemPip, EcosystemMaven, EcosystemNuGet,
		EcosystemRubyGems, EcosystemGo, EcosystemComposer, EcosystemRust,
		EcosystemActions, EcosystemPub, EcosystemErlang, EcosystemSwift,
	} {
		enum := AdvisoryEcosystemGraphQL(ecosystem)
		if enum == "" {
			t.Errorf("ecosystem %q has no SecurityAdvisoryEcosystem member", ecosystem)
			continue
		}
		if back := AdvisoryEcosystemFromGraphQL(enum); back != ecosystem {
			t.Errorf("AdvisoryEcosystemFromGraphQL(%q) = %q, want %q", enum, back, ecosystem)
		}
	}
	if got := AdvisoryEcosystemGraphQL("conda"); got != "" {
		t.Errorf("AdvisoryEcosystemGraphQL(\"conda\") = %q, want \"\"", got)
	}
}

// TestCVSSBaseScore pins the v3.1 base-score arithmetic against vectors whose
// published scores are well known, including the two rounding edges and the
// scope-changed formula.
func TestCVSSBaseScore(t *testing.T) {
	cases := []struct {
		vector string
		want   float64
	}{
		// CVSS v3.1 specification examples.
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", 9.8},
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:H/A:N", 7.5},
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:N/A:N", 7.5},
		{"CVSS:3.1/AV:L/AC:H/PR:H/UI:R/S:U/C:N/I:N/A:N", 0.0},
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:R/S:C/C:L/I:L/A:N", 6.1},
		{"CVSS:3.1/AV:P/AC:H/PR:H/UI:R/S:U/C:L/I:L/A:L", 3.5},
		{"CVSS:3.1/AV:L/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:H", 7.8},
		{"CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:U/C:H/I:H/A:H", 8.1},
		// v3.0 shares the arithmetic.
		{"CVSS:3.0/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", 9.8},
	}
	for _, testCase := range cases {
		got, ok := CVSSBaseScore(testCase.vector)
		if !ok {
			t.Errorf("CVSSBaseScore(%q) could not score a complete v3 vector", testCase.vector)
			continue
		}
		if math.Abs(got-testCase.want) > 0.001 {
			t.Errorf("CVSSBaseScore(%q) = %v, want %v", testCase.vector, got, testCase.want)
		}
	}
}

// TestCVSSBaseScoreRefusesWhatItCannotScore pins the deliberate refusals: a
// v4 vector's score comes from a lookup table rather than this formula, and a
// vector missing a base metric has no defined score at all.
func TestCVSSBaseScoreRefusesWhatItCannotScore(t *testing.T) {
	for _, vector := range []string{
		"",
		"not a vector",
		"CVSS:2.0/AV:N/AC:L/Au:N/C:P/I:P/A:P",
		"CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:H/VI:H/VA:H/SC:N/SI:N/SA:N",
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H", // no A metric
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/C:H/I:H/A:H", // no S metric
		"CVSS:3.1/AV:Z/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
	} {
		if score, ok := CVSSBaseScore(vector); ok {
			t.Errorf("CVSSBaseScore(%q) = %v, want a refusal", vector, score)
		}
	}
}

// TestAdvisoryCVSSScorePrefersTheAuthoredScore pins that a score the author
// supplied outright wins over the one derived from the vector, and that an
// advisory with neither reports none.
func TestAdvisoryCVSSScorePrefersTheAuthoredScore(t *testing.T) {
	authored := &SecurityAdvisory{CVSSScore: 4.2, CVSSVector: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}
	if score, ok := AdvisoryCVSSScore(authored); !ok || score != 4.2 {
		t.Errorf("AdvisoryCVSSScore(authored) = %v/%v, want 4.2", score, ok)
	}
	derived := &SecurityAdvisory{CVSSVector: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}
	if score, ok := AdvisoryCVSSScore(derived); !ok || math.Abs(score-9.8) > 0.001 {
		t.Errorf("AdvisoryCVSSScore(derived) = %v/%v, want 9.8", score, ok)
	}
	if score, ok := AdvisoryCVSSScore(&SecurityAdvisory{}); ok {
		t.Errorf("AdvisoryCVSSScore(unscored) = %v, want none", score)
	}
	if _, ok := AdvisoryCVSSScore(nil); ok {
		t.Error("AdvisoryCVSSScore(nil) claimed a score")
	}
}

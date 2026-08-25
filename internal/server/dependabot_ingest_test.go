package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestDependabotVersionRangesHandlePackageVersionDialects checks the range
// evaluation alert derivation performs, through the ecosystem-aware
// comparator it delegates to. The ecosystem is part of the question: the
// same version string and the same range put a package inside the vulnerable
// window under one ecosystem's ordering and outside it under another's.
func TestDependabotVersionRangesHandlePackageVersionDialects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ecosystem string
		version   string
		rule      string
		want      bool
	}{
		{"npm", "1.2.3", ">= 1.0.0, < 2.0.0", true},
		{"npm", "2.0.0", ">= 1.0.0, < 2.0.0", false},
		{"npm", "1.0.0-rc.1", "< 1.0.0", true},
		{"pip", "2.4.0rc1", "< 2.4.0", true},
		{"npm", "1.0.0+build.9", "= 1.0.0", true},
		{"npm", "release-candidate", "< 2.0.0", true}, // unknown dialect fails safe

		// A post-release is inside ">= 1.0" for pip and outside it for npm,
		// which is the whole reason the ecosystem is threaded through.
		{"pip", "1.0-1", "> 1.0, < 1.1", true},
		{"npm", "1.0-1", "> 1.0, < 1.1", false},
		// Maven's service pack is likewise newer than its release.
		{"maven", "1.0-sp1", "> 1.0, < 1.1", true},
		{"npm", "1.0-sp1", "> 1.0, < 1.1", false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.ecosystem+"/"+test.version+"/"+test.rule, func(t *testing.T) {
			t.Parallel()
			got := store.VersionInVulnerableRange(test.ecosystem, test.version, test.rule)
			if got != test.want {
				t.Fatalf("VersionInVulnerableRange(%q, %q, %q) = %v, want %v",
					test.ecosystem, test.version, test.rule, got, test.want)
			}
		})
	}
}

// TestDependabotPackageMatchesFoldsEcosystemSpellings pins that a purl's
// spelling of an ecosystem and an advisory's spelling of the same one match.
// A snapshot submits "pkg:pypi/…" and "pkg:cargo/…" while advisories are
// written against "pip" and "rust"; without the fold, those dependencies
// would never match any advisory and the repository would look clean.
func TestDependabotPackageMatchesFoldsEcosystemSpellings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		advisoryEcosystem string
		purlEcosystem     string
		packageName       string
		want              bool
	}{
		{"pip", "pypi", "requests", true},
		{"rust", "cargo", "openssl", true},
		{"go", "golang", "github.com/example/lib", true},
		{"rubygems", "gem", "rails", true},
		{"composer", "packagist", "symfony/http-kernel", true},
		{"erlang", "hex", "plug", true},
		{"npm", "npm", "LODASH", true}, // package names match case-insensitively
		{"npm", "pypi", "requests", false},
	}
	for _, testCase := range cases {
		vulnerability := store.SecurityAdvisoryVulnerability{
			PackageEcosystem: testCase.advisoryEcosystem,
			PackageName:      testCase.packageName,
		}
		got := dependabotPackageMatches(vulnerability, testCase.purlEcosystem, testCase.packageName)
		if got != testCase.want {
			t.Errorf("dependabotPackageMatches(%q advisory, %q purl) = %v, want %v",
				testCase.advisoryEcosystem, testCase.purlEcosystem, got, testCase.want)
		}
	}
}

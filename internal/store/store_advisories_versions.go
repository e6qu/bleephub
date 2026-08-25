package store

import (
	"strconv"
	"strings"
	"unicode"
)

// This file is the ecosystem-aware version algebra Dependabot alert
// derivation runs on: given a dependency's resolved version, an advisory's
// vulnerable version range and the ecosystem both belong to, decide whether
// the dependency is affected.
//
// Every package ecosystem GitHub publishes advisories for orders versions by
// its own rules, and the rules genuinely disagree about the same string.
// "1.0.post1" is *newer* than "1.0" under PEP 440 but a prerelease of it under
// SemVer; "1.0-sp1" is newer than "1.0" under Maven but older under SemVer;
// "1.0.a" is a prerelease under RubyGems but an ordinary segment under Maven.
// A single generic comparator therefore cannot answer "is this version inside
// the vulnerable range" correctly for more than one ecosystem — it silently
// produces both missed alerts and false ones. Each ecosystem below gets the
// comparator its package manager actually uses.
//
// Implemented comparators, by SecurityAdvisoryEcosystem value:
//
//	NPM, GO, RUST, ACTIONS, PUB, ERLANG, SWIFT → SemVer 2.0.0 precedence
//	PIP                                        → PEP 440
//	MAVEN                                      → Maven ComparableVersion
//	RUBYGEMS                                   → Gem::Version
//	NUGET                                      → NuGetVersion (4-part SemVer 2.0)
//	COMPOSER                                   → PHP version_compare stability order

// Canonical ecosystem keys. These are the REST spelling (the
// dependabot-alert-package `ecosystem` field), which is also what the store
// records on an alert; GraphQL's SecurityAdvisoryEcosystem enum is the
// uppercased form of the same set with PIP spelt PIP for pip and RUST for
// cargo.
const (
	EcosystemNPM      = "npm"
	EcosystemPip      = "pip"
	EcosystemMaven    = "maven"
	EcosystemNuGet    = "nuget"
	EcosystemRubyGems = "rubygems"
	EcosystemGo       = "go"
	EcosystemComposer = "composer"
	EcosystemRust     = "rust"
	EcosystemActions  = "actions"
	EcosystemPub      = "pub"
	EcosystemErlang   = "erlang"
	EcosystemSwift    = "swift"
)

// NormalizeAdvisoryEcosystem folds the several spellings of one ecosystem
// onto the canonical key. Advisories, dependency snapshots and package URLs
// all name the same ecosystem differently — a purl says "pypi" and
// "cargo" and "golang", GitHub's REST advisory says "pip" and "rust" and
// "go", and the GraphQL enum shouts "PIP". Matching a dependency against an
// advisory compares the folded forms, so the three spellings cannot become
// three ecosystems that never match each other.
func NormalizeAdvisoryEcosystem(ecosystem string) string {
	switch strings.ToLower(strings.TrimSpace(ecosystem)) {
	case "npm", "node", "nodejs", "javascript":
		return EcosystemNPM
	case "pip", "pypi", "python":
		return EcosystemPip
	case "maven", "java", "gradle":
		return EcosystemMaven
	case "nuget", "dotnet", ".net":
		return EcosystemNuGet
	case "rubygems", "gem", "ruby", "bundler":
		return EcosystemRubyGems
	case "go", "golang", "gomod":
		return EcosystemGo
	case "composer", "packagist", "php":
		return EcosystemComposer
	case "rust", "cargo", "crates":
		return EcosystemRust
	case "actions", "githubactions", "github-actions":
		return EcosystemActions
	case "pub", "dart", "flutter":
		return EcosystemPub
	case "erlang", "hex", "elixir":
		return EcosystemErlang
	case "swift", "swifturl":
		return EcosystemSwift
	default:
		return strings.ToLower(strings.TrimSpace(ecosystem))
	}
}

// AdvisoryEcosystemGraphQL renders a canonical ecosystem key as its
// SecurityAdvisoryEcosystem enum value, or "" when the key is not one of the
// twelve ecosystems the enum names — a manifest may legitimately declare a
// package from an ecosystem GitHub's advisory database has no enum for, and
// the field is nullable precisely so that case reports honestly rather than
// inventing a member.
func AdvisoryEcosystemGraphQL(ecosystem string) string {
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
		return "ERLANG"
	case EcosystemSwift:
		return "SWIFT"
	}
	return ""
}

// AdvisoryEcosystemFromGraphQL is the inverse of AdvisoryEcosystemGraphQL:
// it turns a SecurityAdvisoryEcosystem enum value back into the canonical
// key so a GraphQL ecosystem: filter compares against the same folded form
// the store records.
func AdvisoryEcosystemFromGraphQL(enum string) string {
	return NormalizeAdvisoryEcosystem(enum)
}

// CompareEcosystemVersions orders two version strings under the given
// ecosystem's own rules, reporting false when either string is not a version
// that ecosystem's comparator can parse.
//
// The boolean is not decoration: callers must not treat an unparseable
// version as equal, less or greater. Alert derivation reads it to decide
// whether it is entitled to rule a dependency *out* of a vulnerable range.
func CompareEcosystemVersions(ecosystem, left, right string) (int, bool) {
	switch NormalizeAdvisoryEcosystem(ecosystem) {
	case EcosystemPip:
		return comparePEP440(left, right)
	case EcosystemMaven:
		return compareMaven(left, right)
	case EcosystemRubyGems:
		return compareRubyGems(left, right)
	case EcosystemNuGet:
		return compareNuGet(left, right)
	case EcosystemComposer:
		return compareComposer(left, right)
	default:
		// SemVer 2.0.0 precedence, which npm, Go modules, Cargo, Hex, pub,
		// SwiftPM and Actions tags all use. It is also the least surprising
		// answer for an ecosystem this build does not know, and the honest
		// one: every remaining GitHub advisory ecosystem is semver-ordered.
		return compareSemVer(left, right)
	}
}

// VersionInVulnerableRange reports whether version falls inside an advisory's
// vulnerableVersionRange under the ecosystem's ordering.
//
// The range grammar is GitHub's, documented on SecurityVulnerability:
// comma-separated constraints, all of which must hold — "= 0.2.0",
// "<= 1.0.8", "< 0.1.11", ">= 4.3.0, < 4.3.5", ">= 0.0.1".
func VersionInVulnerableRange(ecosystem, version, rangeExpr string) bool {
	version = strings.TrimSpace(version)
	rangeExpr = strings.TrimSpace(rangeExpr)
	if version == "" || rangeExpr == "" {
		return false
	}
	matched := false
	for _, constraint := range strings.Split(rangeExpr, ",") {
		constraint = strings.TrimSpace(constraint)
		if constraint == "" {
			continue
		}
		if !versionSatisfiesConstraint(ecosystem, version, constraint) {
			return false
		}
		matched = true
	}
	return matched
}

// versionSatisfiesConstraint evaluates one comparator of a vulnerable range.
func versionSatisfiesConstraint(ecosystem, version, constraint string) bool {
	operator, operand := splitVersionConstraint(constraint)
	cmp, ok := CompareEcosystemVersions(ecosystem, version, operand)
	if !ok {
		// A version dialect this comparator cannot read must not silently
		// turn a published vulnerability into "safe": suppressing the alert
		// hides the advisory outright, while raising it leaves a human (or a
		// later Dependabot reconciliation) able to dismiss it. The
		// conservative direction is the one that still tells somebody.
		return true
	}
	switch operator {
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	default:
		return cmp == 0
	}
}

// splitVersionConstraint separates a constraint's comparison operator from
// its operand, defaulting to equality for a bare version. The candidates are
// ordered longest-first so "<=" is never read as "<" followed by a version
// that begins with "=".
func splitVersionConstraint(constraint string) (operator, operand string) {
	for _, candidate := range []string{"<=", ">=", "==", "<", ">", "="} {
		if !strings.HasPrefix(constraint, candidate) {
			continue
		}
		operator = candidate
		if candidate == "==" {
			operator = "="
		}
		return operator, strings.TrimSpace(constraint[len(candidate):])
	}
	return "=", strings.TrimSpace(constraint)
}

// ---------------------------------------------------------------------------
// SemVer 2.0.0 — npm, Go, Cargo, Hex, pub, SwiftPM, Actions
// ---------------------------------------------------------------------------

// compareSemVer implements SemVer 2.0.0 §11 precedence: the release triple is
// compared numerically, build metadata is ignored, a version with a
// prerelease sorts below the same version without one, and prerelease
// identifiers are compared dot-part by dot-part with numeric parts below
// alphanumeric ones.
func compareSemVer(left, right string) (int, bool) {
	leftRelease, leftPre, ok := parseSemVer(left)
	if !ok {
		return 0, false
	}
	rightRelease, rightPre, ok := parseSemVer(right)
	if !ok {
		return 0, false
	}
	if cmp := compareNumericSegments(leftRelease, rightRelease); cmp != 0 {
		return cmp, true
	}
	return comparePrereleaseIdentifiers(leftPre, rightPre), true
}

// parseSemVer splits a version into its numeric release segments and its
// prerelease identifiers. A leading "v" is tolerated because Go module and
// Actions tags carry one, and a missing minor or patch is tolerated because
// advisories are routinely written "< 2" rather than "< 2.0.0".
func parseSemVer(version string) (release []int, prerelease []string, ok bool) {
	version = strings.TrimSpace(version)
	version = strings.TrimPrefix(version, "v")
	version = strings.TrimPrefix(version, "V")
	if version == "" {
		return nil, nil, false
	}
	// Build metadata never participates in precedence.
	if i := strings.IndexByte(version, '+'); i >= 0 {
		version = version[:i]
	}
	core := version
	if i := strings.IndexByte(version, '-'); i >= 0 {
		core, prerelease = version[:i], strings.Split(version[i+1:], ".")
	}
	for _, segment := range strings.Split(core, ".") {
		if segment == "" {
			return nil, nil, false
		}
		number, err := strconv.Atoi(segment)
		if err != nil || number < 0 {
			return nil, nil, false
		}
		release = append(release, number)
	}
	if len(release) == 0 {
		return nil, nil, false
	}
	return release, prerelease, true
}

// comparePrereleaseIdentifiers applies SemVer §11.4 to two prerelease
// identifier lists, where an empty list (a final release) outranks any
// non-empty one.
func comparePrereleaseIdentifiers(left, right []string) int {
	switch {
	case len(left) == 0 && len(right) == 0:
		return 0
	case len(left) == 0:
		return 1
	case len(right) == 0:
		return -1
	}
	for i := 0; i < len(left) && i < len(right); i++ {
		leftNumber, leftNotNumeric := strconv.Atoi(left[i])
		rightNumber, rightNotNumeric := strconv.Atoi(right[i])
		switch {
		case leftNotNumeric == nil && rightNotNumeric == nil:
			if leftNumber != rightNumber {
				return signOf(leftNumber - rightNumber)
			}
		case leftNotNumeric == nil:
			// Numeric identifiers always have lower precedence than
			// alphanumeric ones (SemVer §11.4.3).
			return -1
		case rightNotNumeric == nil:
			return 1
		default:
			if cmp := strings.Compare(left[i], right[i]); cmp != 0 {
				return cmp
			}
		}
	}
	// A larger set of prerelease identifiers outranks a smaller set when all
	// of the preceding identifiers are equal (SemVer §11.4.4).
	return signOf(len(left) - len(right))
}

// ---------------------------------------------------------------------------
// PEP 440 — pip
// ---------------------------------------------------------------------------

// pep440Version is a parsed PEP 440 version. The five ordered components are
// exactly the ones §"Summary of permitted suffixes and relative ordering"
// names, and the sentinel values are chosen so a plain slice comparison
// reproduces that ordering.
type pep440Version struct {
	epoch   int
	release []int
	// preKind is "" for a final release, otherwise "a", "b" or "rc"; a
	// pre-release sorts below the same release, which preRank encodes.
	preRank   int
	preNumber int
	// postNumber is -1 when there is no post-release, so a post-release
	// sorts above the same release.
	postNumber int
	// hasDev / devNumber describe a .devN suffix. A dev release sorts BELOW
	// everything else carrying the same release segment and stage, so the
	// absence of one cannot be spelt as a number: "1.0a1" must outrank
	// "1.0a1.dev1", which any sentinel value compared numerically against a
	// real dev number gets backwards in one direction or the other.
	hasDev    bool
	devNumber int
	local     []string
}

// comparePEP440 orders two Python versions.
func comparePEP440(left, right string) (int, bool) {
	leftVersion, ok := parsePEP440(left)
	if !ok {
		return 0, false
	}
	rightVersion, ok := parsePEP440(right)
	if !ok {
		return 0, false
	}
	if leftVersion.epoch != rightVersion.epoch {
		return signOf(leftVersion.epoch - rightVersion.epoch), true
	}
	if cmp := compareNumericSegments(leftVersion.release, rightVersion.release); cmp != 0 {
		return cmp, true
	}
	// dev < pre < final < post, with each rank's own number breaking ties.
	if cmp := signOf(pep440StageRank(leftVersion) - pep440StageRank(rightVersion)); cmp != 0 {
		return cmp, true
	}
	if leftVersion.preRank != rightVersion.preRank {
		return signOf(leftVersion.preRank - rightVersion.preRank), true
	}
	if leftVersion.preNumber != rightVersion.preNumber {
		return signOf(leftVersion.preNumber - rightVersion.preNumber), true
	}
	if leftVersion.postNumber != rightVersion.postNumber {
		return signOf(leftVersion.postNumber - rightVersion.postNumber), true
	}
	if leftVersion.hasDev != rightVersion.hasDev {
		// The one carrying a dev suffix is the earlier of the two.
		if leftVersion.hasDev {
			return -1, true
		}
		return 1, true
	}
	if leftVersion.hasDev && leftVersion.devNumber != rightVersion.devNumber {
		return signOf(leftVersion.devNumber - rightVersion.devNumber), true
	}
	return comparePEP440Local(leftVersion.local, rightVersion.local), true
}

// pep440StageRank collapses the dev/pre/final/post distinction into the
// coarse ordering PEP 440 gives them within one release segment.
func pep440StageRank(version pep440Version) int {
	switch {
	case version.postNumber >= 0:
		return 3
	case version.preRank > 0:
		// A pre-release with a .devN suffix is still below a bare
		// pre-release, which the hasDev tiebreak settles.
		return 1
	case version.hasDev:
		return 0
	default:
		return 2
	}
}

// comparePEP440Local orders local version labels: a version with a local
// label sorts above the same version without one, numeric segments outrank
// alphanumeric ones, and equal-kind segments compare in their own domain.
func comparePEP440Local(left, right []string) int {
	switch {
	case len(left) == 0 && len(right) == 0:
		return 0
	case len(left) == 0:
		return -1
	case len(right) == 0:
		return 1
	}
	for i := 0; i < len(left) && i < len(right); i++ {
		leftNumber, leftErr := strconv.Atoi(left[i])
		rightNumber, rightErr := strconv.Atoi(right[i])
		switch {
		case leftErr == nil && rightErr == nil:
			if leftNumber != rightNumber {
				return signOf(leftNumber - rightNumber)
			}
		case leftErr == nil:
			return 1
		case rightErr == nil:
			return -1
		default:
			if cmp := strings.Compare(left[i], right[i]); cmp != 0 {
				return cmp
			}
		}
	}
	return signOf(len(left) - len(right))
}

// parsePEP440 reads the normalized and the common non-normalized spellings
// PEP 440 declares equivalent: "1.0alpha1" and "1.0.a1" and "1.0-a-1" are one
// version, "1.0-1" and "1.0.post1" and "1.0rev1" are another.
func parsePEP440(version string) (pep440Version, bool) {
	parsed := pep440Version{preNumber: -1, postNumber: -1}
	version = strings.ToLower(strings.TrimSpace(version))
	version = strings.TrimPrefix(version, "v")
	if version == "" {
		return parsed, false
	}
	if i := strings.IndexByte(version, '+'); i >= 0 {
		local := version[i+1:]
		version = version[:i]
		parsed.local = append(parsed.local, strings.FieldsFunc(local, func(r rune) bool {
			return r == '.' || r == '-' || r == '_'
		})...)
	}
	if i := strings.IndexByte(version, '!'); i >= 0 {
		epoch, err := strconv.Atoi(version[:i])
		if err != nil || epoch < 0 {
			return parsed, false
		}
		parsed.epoch = epoch
		version = version[i+1:]
	}
	// Split the leading dotted-numeric release segment off the suffixes.
	end := 0
	for end < len(version) && (version[end] == '.' || (version[end] >= '0' && version[end] <= '9')) {
		end++
	}
	release, suffix := version[:end], version[end:]
	release = strings.Trim(release, ".")
	if release == "" {
		return parsed, false
	}
	for _, segment := range strings.Split(release, ".") {
		number, err := strconv.Atoi(segment)
		if err != nil || number < 0 {
			return parsed, false
		}
		parsed.release = append(parsed.release, number)
	}
	if !parsePEP440Suffix(&parsed, suffix) {
		return parsed, false
	}
	return parsed, true
}

// pep440PreRanks maps every spelling of a pre-release marker to its rank.
// The ranks start at 1 so that zero can mean "not a pre-release".
var pep440PreRanks = map[string]int{
	"a": 1, "alpha": 1,
	"b": 2, "beta": 2,
	"c": 3, "rc": 3, "pre": 3, "preview": 3,
}

// parsePEP440Suffix consumes the pre/post/dev suffix chain that follows a
// PEP 440 release segment, tolerating the "." "-" "_" separators the spec
// declares interchangeable.
func parsePEP440Suffix(parsed *pep440Version, suffix string) bool {
	for suffix != "" {
		suffix = strings.TrimLeft(suffix, ".-_")
		if suffix == "" {
			break
		}
		// The implicit post-release spelling "1.0-1": a separator followed
		// only by digits. The separator is already trimmed, so a leading
		// digit here means the number stood alone.
		if suffix[0] >= '0' && suffix[0] <= '9' {
			word, rest := splitLeadingDigits(suffix)
			number, err := strconv.Atoi(word)
			if err != nil {
				return false
			}
			parsed.postNumber = number
			suffix = rest
			continue
		}
		word, rest := splitLeadingLetters(suffix)
		rest = strings.TrimLeft(rest, ".-_")
		digits, remainder := splitLeadingDigits(rest)
		number := 0
		if digits != "" {
			parsedNumber, err := strconv.Atoi(digits)
			if err != nil {
				return false
			}
			number = parsedNumber
		}
		switch {
		case pep440PreRanks[word] != 0:
			parsed.preRank = pep440PreRanks[word]
			parsed.preNumber = number
		case word == "post" || word == "rev" || word == "r":
			parsed.postNumber = number
		case word == "dev":
			parsed.hasDev, parsed.devNumber = true, number
		default:
			return false
		}
		suffix = remainder
	}
	if parsed.preRank != 0 && parsed.preNumber < 0 {
		parsed.preNumber = 0
	}
	return true
}

// ---------------------------------------------------------------------------
// Maven ComparableVersion
// ---------------------------------------------------------------------------

// mavenQualifierOrder is Maven's ComparableVersion qualifier ranking. The
// empty string is the release itself, so anything below it is a prerelease
// and "sp" (service pack) is the one qualifier that outranks the release.
var mavenQualifierOrder = map[string]int{
	"alpha": 0, "a": 0,
	"beta": 1, "b": 1,
	"milestone": 2, "m": 2,
	"rc": 3, "cr": 3,
	"snapshot": 4,
	"":         5, "ga": 5, "final": 5, "release": 5,
	"sp": 6,
}

// compareMaven orders two Maven coordinates' versions the way Maven's own
// ComparableVersion does: tokens split on separators and on digit/letter
// transitions, numeric tokens compared as integers, qualifier tokens ranked
// against the release, and a shorter version zero-padded.
func compareMaven(left, right string) (int, bool) {
	leftTokens, ok := mavenTokens(left)
	if !ok {
		return 0, false
	}
	rightTokens, ok := mavenTokens(right)
	if !ok {
		return 0, false
	}
	for i := 0; i < len(leftTokens) || i < len(rightTokens); i++ {
		leftToken := mavenNullToken()
		if i < len(leftTokens) {
			leftToken = leftTokens[i]
		}
		rightToken := mavenNullToken()
		if i < len(rightTokens) {
			rightToken = rightTokens[i]
		}
		if cmp := compareMavenToken(leftToken, rightToken); cmp != 0 {
			return cmp, true
		}
	}
	return 0, true
}

// mavenTokenKind distinguishes the three things a ComparableVersion item can
// be. The null kind is not an absence to be skipped: Maven compares a
// present token against the padding explicitly, and the answer differs by
// kind — a trailing ".0" leaves a version unchanged while a trailing "-rc"
// makes it older and a trailing "-sp" makes it newer.
type mavenTokenKind int

const (
	mavenTokenNull mavenTokenKind = iota
	mavenTokenNumeric
	mavenTokenQualifier
)

// mavenToken is one item of a tokenized Maven version.
type mavenToken struct {
	kind      mavenTokenKind
	number    int
	qualifier string
}

// mavenNullToken is the padding a shorter version is compared against.
func mavenNullToken() mavenToken { return mavenToken{kind: mavenTokenNull} }

// mavenQualifierRank ranks a qualifier against the release marker. Maven
// sorts an unrecognized qualifier above every recognized one and above the
// release itself, so a rank one past the table stands in for "unknown".
func mavenQualifierRank(qualifier string) int {
	if rank, known := mavenQualifierOrder[qualifier]; known {
		return rank
	}
	return mavenQualifierOrder["sp"] + 1
}

// mavenTokens splits a version into ComparableVersion's token sequence.
func mavenTokens(version string) ([]mavenToken, bool) {
	version = strings.ToLower(strings.TrimSpace(version))
	if version == "" {
		return nil, false
	}
	var tokens []mavenToken
	for _, chunk := range strings.FieldsFunc(version, func(r rune) bool {
		return r == '.' || r == '-' || r == '_' || r == '+'
	}) {
		for _, run := range splitDigitLetterRuns(chunk) {
			if run == "" {
				continue
			}
			if number, err := strconv.Atoi(run); err == nil {
				tokens = append(tokens, mavenToken{kind: mavenTokenNumeric, number: number})
				continue
			}
			tokens = append(tokens, mavenToken{kind: mavenTokenQualifier, qualifier: run})
		}
	}
	if len(tokens) == 0 {
		return nil, false
	}
	return tokens, true
}

// compareMavenToken orders two ComparableVersion items, reproducing
// IntItem.compareTo and StringItem.compareTo including their treatment of
// the null padding.
func compareMavenToken(left, right mavenToken) int {
	switch {
	case left.kind == mavenTokenNumeric && right.kind == mavenTokenNumeric:
		return signOf(left.number - right.number)

	// A number is always newer than a qualifier: "1.1" beats "1.0-rc" on the
	// third item, and "1.0.1" beats "1.0-sp" on the same one.
	case left.kind == mavenTokenNumeric && right.kind == mavenTokenQualifier:
		return 1
	case left.kind == mavenTokenQualifier && right.kind == mavenTokenNumeric:
		return -1

	// A trailing zero is not a version change, so "1.0" and "1.0.0" are the
	// same version while "1.0.1" is newer than "1.0".
	case left.kind == mavenTokenNumeric && right.kind == mavenTokenNull:
		return signOf(left.number)
	case left.kind == mavenTokenNull && right.kind == mavenTokenNumeric:
		return -signOf(right.number)

	// A trailing qualifier is compared against the release marker, which is
	// what puts "1.0-rc" below "1.0" and "1.0-sp" above it.
	case left.kind == mavenTokenQualifier && right.kind == mavenTokenNull:
		return signOf(mavenQualifierRank(left.qualifier) - mavenQualifierOrder[""])
	case left.kind == mavenTokenNull && right.kind == mavenTokenQualifier:
		return -signOf(mavenQualifierRank(right.qualifier) - mavenQualifierOrder[""])

	case left.kind == mavenTokenQualifier && right.kind == mavenTokenQualifier:
		leftRank, rightRank := mavenQualifierRank(left.qualifier), mavenQualifierRank(right.qualifier)
		if leftRank != rightRank {
			return signOf(leftRank - rightRank)
		}
		// Two qualifiers of the same rank are either aliases of one another
		// ("a" and "alpha") or two unrecognized words, which Maven orders
		// lexically against each other.
		if _, leftKnown := mavenQualifierOrder[left.qualifier]; leftKnown {
			return 0
		}
		return strings.Compare(left.qualifier, right.qualifier)

	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// RubyGems Gem::Version
// ---------------------------------------------------------------------------

// compareRubyGems orders two gem versions. Gem::Version splits on "." and on
// digit/letter transitions, compares numeric segments as integers and string
// segments lexically, and ranks a string segment below a numeric one — which
// is what makes "1.0.a" a prerelease of "1.0" and "1.0.0" equal to "1.0".
func compareRubyGems(left, right string) (int, bool) {
	leftSegments, ok := rubyGemsSegments(left)
	if !ok {
		return 0, false
	}
	rightSegments, ok := rubyGemsSegments(right)
	if !ok {
		return 0, false
	}
	for i := 0; i < len(leftSegments) || i < len(rightSegments); i++ {
		// Gem::Version pads the shorter version with numeric zeroes, so
		// "1.0" and "1.0.0" compare equal while "1.0" outranks "1.0.a".
		leftSegment := "0"
		if i < len(leftSegments) {
			leftSegment = leftSegments[i]
		}
		rightSegment := "0"
		if i < len(rightSegments) {
			rightSegment = rightSegments[i]
		}
		leftNumber, leftErr := strconv.Atoi(leftSegment)
		rightNumber, rightErr := strconv.Atoi(rightSegment)
		switch {
		case leftErr == nil && rightErr == nil:
			if leftNumber != rightNumber {
				return signOf(leftNumber - rightNumber), true
			}
		case leftErr == nil:
			return 1, true
		case rightErr == nil:
			return -1, true
		default:
			if cmp := strings.Compare(leftSegment, rightSegment); cmp != 0 {
				return cmp, true
			}
		}
	}
	return 0, true
}

// rubyGemsSegments tokenizes a gem version.
func rubyGemsSegments(version string) ([]string, bool) {
	version = strings.ToLower(strings.TrimSpace(version))
	version = strings.TrimPrefix(version, "v")
	if version == "" {
		return nil, false
	}
	var segments []string
	for _, chunk := range strings.FieldsFunc(version, func(r rune) bool {
		return r == '.' || r == '-' || r == '_' || r == '+'
	}) {
		for _, run := range splitDigitLetterRuns(chunk) {
			if run != "" {
				segments = append(segments, run)
			}
		}
	}
	if len(segments) == 0 {
		return nil, false
	}
	return segments, true
}

// ---------------------------------------------------------------------------
// NuGet
// ---------------------------------------------------------------------------

// compareNuGet orders two NuGet package versions: a four-part numeric
// version (the legacy revision field) followed by SemVer 2.0 prerelease
// identifiers compared case-insensitively, with build metadata ignored.
func compareNuGet(left, right string) (int, bool) {
	leftRelease, leftPre, ok := parseNuGet(left)
	if !ok {
		return 0, false
	}
	rightRelease, rightPre, ok := parseNuGet(right)
	if !ok {
		return 0, false
	}
	// NuGet pads to four parts, so 1.0 and 1.0.0.0 are the same version.
	for len(leftRelease) < 4 {
		leftRelease = append(leftRelease, 0)
	}
	for len(rightRelease) < 4 {
		rightRelease = append(rightRelease, 0)
	}
	if cmp := compareNumericSegments(leftRelease, rightRelease); cmp != 0 {
		return cmp, true
	}
	return comparePrereleaseIdentifiers(leftPre, rightPre), true
}

// parseNuGet splits a NuGet version into its numeric parts and its
// lowercased prerelease identifiers.
func parseNuGet(version string) (release []int, prerelease []string, ok bool) {
	version = strings.ToLower(strings.TrimSpace(version))
	release, prerelease, ok = parseSemVer(version)
	if !ok || len(release) > 4 {
		return nil, nil, false
	}
	return release, prerelease, true
}

// ---------------------------------------------------------------------------
// Composer / PHP version_compare
// ---------------------------------------------------------------------------

// composerStabilityRanks is PHP's version_compare ordering of the stability
// words, where anything unrecognized sorts below "dev" exactly as PHP does.
var composerStabilityRanks = map[string]int{
	"dev":   0,
	"alpha": 1, "a": 1,
	"beta": 2, "b": 2,
	"rc": 3, "c": 3,
	"#":  4, // the release itself
	"pl": 5, "p": 5,
}

// compareComposer orders two Composer package versions with PHP's
// version_compare semantics: the version is canonicalized into
// period-separated parts, and each part is either a number or a stability
// word ranked against the implicit release marker.
func compareComposer(left, right string) (int, bool) {
	leftParts, ok := composerParts(left)
	if !ok {
		return 0, false
	}
	rightParts, ok := composerParts(right)
	if !ok {
		return 0, false
	}
	for i := 0; i < len(leftParts) || i < len(rightParts); i++ {
		leftPart, rightPart := "#", "#"
		if i < len(leftParts) {
			leftPart = leftParts[i]
		}
		if i < len(rightParts) {
			rightPart = rightParts[i]
		}
		if cmp := compareComposerPart(leftPart, rightPart); cmp != 0 {
			return cmp, true
		}
	}
	return 0, true
}

// compareComposerPart orders one canonicalized part against another.
func compareComposerPart(left, right string) int {
	leftNumber, leftErr := strconv.Atoi(left)
	rightNumber, rightErr := strconv.Atoi(right)
	if leftErr == nil && rightErr == nil {
		return signOf(leftNumber - rightNumber)
	}
	// A numeric part is the release marker's peer: "1.0.1" outranks
	// "1.0.rc1" for the same reason "1.0" does.
	rank := func(part string, numeric bool) int {
		if numeric {
			return composerStabilityRanks["#"]
		}
		if known, ok := composerStabilityRanks[part]; ok {
			return known
		}
		return -1
	}
	leftRank := rank(left, leftErr == nil)
	rightRank := rank(right, rightErr == nil)
	if leftRank != rightRank {
		return signOf(leftRank - rightRank)
	}
	if leftErr == nil && rightErr != nil {
		return 1
	}
	if rightErr == nil && leftErr != nil {
		return -1
	}
	return strings.Compare(left, right)
}

// composerParts canonicalizes a Composer version the way PHP's
// version_compare does before comparing: separators become boundaries, and
// every digit/letter transition becomes one too.
func composerParts(version string) ([]string, bool) {
	version = strings.ToLower(strings.TrimSpace(version))
	version = strings.TrimPrefix(version, "v")
	if version == "" {
		return nil, false
	}
	if i := strings.IndexByte(version, '+'); i >= 0 {
		version = version[:i]
	}
	var parts []string
	for _, chunk := range strings.FieldsFunc(version, func(r rune) bool {
		return r == '.' || r == '-' || r == '_'
	}) {
		for _, run := range splitDigitLetterRuns(chunk) {
			if run != "" {
				parts = append(parts, run)
			}
		}
	}
	if len(parts) == 0 {
		return nil, false
	}
	return parts, true
}

// ---------------------------------------------------------------------------
// Shared primitives
// ---------------------------------------------------------------------------

// compareNumericSegments orders two release-segment slices, treating a
// missing trailing segment as zero so "1.2" and "1.2.0" compare equal.
func compareNumericSegments(left, right []int) int {
	for i := 0; i < len(left) || i < len(right); i++ {
		leftSegment, rightSegment := 0, 0
		if i < len(left) {
			leftSegment = left[i]
		}
		if i < len(right) {
			rightSegment = right[i]
		}
		if leftSegment != rightSegment {
			return signOf(leftSegment - rightSegment)
		}
	}
	return 0
}

// splitDigitLetterRuns breaks a chunk at every digit-to-letter and
// letter-to-digit transition, which is the tokenization Maven, RubyGems and
// Composer all perform before comparing.
func splitDigitLetterRuns(chunk string) []string {
	var runs []string
	start := 0
	for i := 1; i <= len(chunk); i++ {
		if i == len(chunk) || isASCIIDigit(chunk[i]) != isASCIIDigit(chunk[start]) {
			runs = append(runs, chunk[start:i])
			start = i
		}
	}
	return runs
}

// splitLeadingDigits peels the leading run of digits off a string.
func splitLeadingDigits(value string) (digits, rest string) {
	i := 0
	for i < len(value) && isASCIIDigit(value[i]) {
		i++
	}
	return value[:i], value[i:]
}

// splitLeadingLetters peels the leading run of letters off a string.
func splitLeadingLetters(value string) (letters, rest string) {
	i := 0
	for i < len(value) && unicode.IsLetter(rune(value[i])) {
		i++
	}
	return value[:i], value[i:]
}

// isASCIIDigit reports whether b is one of '0'..'9'.
func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// signOf collapses a difference to -1, 0 or 1.
func signOf(difference int) int {
	switch {
	case difference < 0:
		return -1
	case difference > 0:
		return 1
	default:
		return 0
	}
}

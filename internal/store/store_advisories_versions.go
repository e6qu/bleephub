package store

import (
	"strconv"
	"strings"
	"unicode"
)

// Ecosystem-aware version algebra for Dependabot alert derivation. Each
// ecosystem orders versions by incompatible rules ("1.0-sp1" is newer than
// "1.0" under Maven but older under SemVer), so a single generic comparator
// would produce both missed and false alerts; each gets its package manager's
// own comparator:
//
//	NPM, GO, RUST, ACTIONS, PUB, ERLANG, SWIFT → SemVer 2.0.0 precedence
//	PIP                                        → PEP 440
//	MAVEN                                      → Maven ComparableVersion
//	RUBYGEMS                                   → Gem::Version
//	NUGET                                      → NuGetVersion (4-part SemVer 2.0)
//	COMPOSER                                   → PHP version_compare stability order

// Canonical ecosystem keys: the REST spelling the store records on an alert.
// GraphQL's SecurityAdvisoryEcosystem enum is the uppercased form.
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
// (purl "pypi"/"cargo", REST "pip"/"rust", GraphQL "PIP") onto the canonical
// key, so matching compares folded forms rather than three names that never
// meet.
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

// AdvisoryEcosystemGraphQL renders a canonical key as its
// SecurityAdvisoryEcosystem enum value, or "" for an ecosystem the enum does
// not name (the field is nullable so this reports honestly).
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

// AdvisoryEcosystemFromGraphQL turns a SecurityAdvisoryEcosystem enum value
// back into the canonical key.
func AdvisoryEcosystemFromGraphQL(enum string) string {
	return NormalizeAdvisoryEcosystem(enum)
}

// CompareEcosystemVersions orders two versions under the ecosystem's own
// rules. The bool is false when either string is unparseable; callers must not
// then treat the versions as ordered.
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
		// SemVer 2.0.0: what npm/Go/Cargo/Hex/pub/SwiftPM/Actions use, and the
		// right default for any unknown ecosystem (all remaining ones are
		// semver-ordered).
		return compareSemVer(left, right)
	}
}

// VersionInVulnerableRange reports whether version falls inside an advisory's
// vulnerableVersionRange. The grammar is GitHub's: comma-separated
// constraints, all of which must hold (e.g. ">= 4.3.0, < 4.3.5").
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
		// Unparseable version: fail toward alerting. Suppressing hides the
		// advisory; a false alert can still be dismissed.
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

// splitVersionConstraint separates a constraint's operator from its operand,
// defaulting to equality. Candidates are longest-first so "<=" is not read as
// "<" plus "=...".
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

// compareSemVer implements SemVer 2.0.0 §11 precedence: numeric release
// triple, build metadata ignored, a prerelease below the same release, and
// dot-parts compared with numeric below alphanumeric.
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

// parseSemVer splits a version into numeric release segments and prerelease
// identifiers. A leading "v" (Go/Actions tags) and a missing minor/patch
// (advisories write "< 2") are tolerated.
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

// comparePrereleaseIdentifiers applies SemVer §11.4; an empty list (final
// release) outranks any non-empty one.
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

// pep440Version is a parsed PEP 440 version, with sentinel values chosen so a
// plain field-by-field comparison reproduces PEP 440 ordering.
type pep440Version struct {
	epoch   int
	release []int
	// preRank is 0 for a final release, else the pre-release stage; a
	// pre-release sorts below the same release.
	preRank   int
	preNumber int
	// postNumber is -1 when absent, so a post-release sorts above the release.
	postNumber int
	// hasDev is a separate flag, not a sentinel devNumber: a dev release sorts
	// below everything at the same stage ("1.0a1" outranks "1.0a1.dev1"), which
	// no numeric sentinel gets right in both directions.
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

// pep440StageRank collapses the dev/pre/final/post distinction into PEP 440's
// coarse ordering within one release segment.
func pep440StageRank(version pep440Version) int {
	switch {
	case version.postNumber >= 0:
		return 3
	case version.preRank > 0:
		return 1
	case version.hasDev:
		return 0
	default:
		return 2
	}
}

// comparePEP440Local orders local version labels: a label outranks none,
// numeric segments outrank alphanumeric, equal kinds compare in their domain.
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

// parsePEP440 reads the normalized and non-normalized spellings PEP 440 treats
// as equivalent ("1.0alpha1" == "1.0.a1", "1.0-1" == "1.0.post1").
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

// pep440PreRanks maps each pre-release marker to its rank; ranks start at 1 so
// zero means "not a pre-release".
var pep440PreRanks = map[string]int{
	"a": 1, "alpha": 1,
	"b": 2, "beta": 2,
	"c": 3, "rc": 3, "pre": 3, "preview": 3,
}

// parsePEP440Suffix consumes the pre/post/dev suffix chain after a release
// segment, tolerating the interchangeable "." "-" "_" separators.
func parsePEP440Suffix(parsed *pep440Version, suffix string) bool {
	for suffix != "" {
		suffix = strings.TrimLeft(suffix, ".-_")
		if suffix == "" {
			break
		}
		// Implicit post-release "1.0-1": a leading digit (separator already
		// trimmed) means the number stood alone.
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

// mavenQualifierOrder is Maven's ComparableVersion qualifier ranking. "" is
// the release; anything below it is a prerelease, and "sp" alone outranks it.
var mavenQualifierOrder = map[string]int{
	"alpha": 0, "a": 0,
	"beta": 1, "b": 1,
	"milestone": 2, "m": 2,
	"rc": 3, "cr": 3,
	"snapshot": 4,
	"":         5, "ga": 5, "final": 5, "release": 5,
	"sp": 6,
}

// compareMaven orders two versions the way Maven's ComparableVersion does:
// tokens split on separators and digit/letter transitions, numerics compared
// as integers, qualifiers ranked against the release, shorter version padded.
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

// mavenTokenKind distinguishes a ComparableVersion item. The null kind is
// compared against explicitly, not skipped: trailing ".0" leaves a version
// unchanged, "-rc" makes it older, "-sp" newer.
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

// mavenQualifierRank ranks a qualifier against the release. An unrecognized
// qualifier sorts above every known one, so it gets a rank one past the table.
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
// IntItem/StringItem.compareTo including their null-padding treatment.
func compareMavenToken(left, right mavenToken) int {
	switch {
	case left.kind == mavenTokenNumeric && right.kind == mavenTokenNumeric:
		return signOf(left.number - right.number)

	// A number is always newer than a qualifier.
	case left.kind == mavenTokenNumeric && right.kind == mavenTokenQualifier:
		return 1
	case left.kind == mavenTokenQualifier && right.kind == mavenTokenNumeric:
		return -1

	// Trailing zero is not a version change: "1.0" == "1.0.0".
	case left.kind == mavenTokenNumeric && right.kind == mavenTokenNull:
		return signOf(left.number)
	case left.kind == mavenTokenNull && right.kind == mavenTokenNumeric:
		return -signOf(right.number)

	// Trailing qualifier compared against the release marker: "-rc" below,
	// "-sp" above.
	case left.kind == mavenTokenQualifier && right.kind == mavenTokenNull:
		return signOf(mavenQualifierRank(left.qualifier) - mavenQualifierOrder[""])
	case left.kind == mavenTokenNull && right.kind == mavenTokenQualifier:
		return -signOf(mavenQualifierRank(right.qualifier) - mavenQualifierOrder[""])

	case left.kind == mavenTokenQualifier && right.kind == mavenTokenQualifier:
		leftRank, rightRank := mavenQualifierRank(left.qualifier), mavenQualifierRank(right.qualifier)
		if leftRank != rightRank {
			return signOf(leftRank - rightRank)
		}
		// Same rank: aliases ("a"/"alpha") are equal; unrecognized words order
		// lexically.
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

// compareRubyGems orders two gem versions the way Gem::Version does: split on
// "." and digit/letter transitions, numerics as integers, strings lexically
// and below numerics ("1.0.a" is a prerelease of "1.0", "1.0.0" == "1.0").
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
		// Gem::Version pads the shorter version with numeric zeroes.
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

// compareNuGet orders two NuGet versions: a four-part numeric version plus
// case-insensitive SemVer 2.0 prerelease identifiers, build metadata ignored.
func compareNuGet(left, right string) (int, bool) {
	leftRelease, leftPre, ok := parseNuGet(left)
	if !ok {
		return 0, false
	}
	rightRelease, rightPre, ok := parseNuGet(right)
	if !ok {
		return 0, false
	}
	// NuGet pads to four parts: 1.0 == 1.0.0.0.
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

// composerStabilityRanks is PHP version_compare's stability-word ordering;
// anything unrecognized sorts below "dev", as PHP does.
var composerStabilityRanks = map[string]int{
	"dev":   0,
	"alpha": 1, "a": 1,
	"beta": 2, "b": 2,
	"rc": 3, "c": 3,
	"#":  4, // the release itself
	"pl": 5, "p": 5,
}

// compareComposer orders two versions with PHP version_compare semantics:
// canonicalized into parts, each a number or a stability word ranked against
// the release marker.
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
	// A numeric part ranks as the release marker.
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

// composerParts canonicalizes a version the way PHP version_compare does:
// separators and digit/letter transitions both become boundaries.
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

// compareNumericSegments orders two release-segment slices, a missing trailing
// segment counting as zero ("1.2" == "1.2.0").
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

// splitDigitLetterRuns breaks a chunk at every digit/letter transition, the
// tokenization Maven, RubyGems and Composer share.
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

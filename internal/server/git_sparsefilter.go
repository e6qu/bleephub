package bleephub

import (
	"bytes"
	"strings"
)

// A "sparse:oid=<blob-ish>" filter-spec names a blob holding sparse-checkout
// patterns, and the pack it produces carries only the blobs whose path those
// patterns select. This file is the pattern language that decision is made in.
//
// git matches such a filter with the ordinary, non-cone pattern engine:
// filter_sparse_oid__init in list-objects-filter.c zero-initialises the
// pattern_list it loads the blob into, and a zeroed pattern_list has
// use_cone_patterns unset, so path_matches_pattern_list takes its non-cone
// branch. The cone-mode files `git sparse-checkout set` writes ("/*", "!/*/",
// "/dir/", "!/dir/*/", …) are deliberately written in that same language, so
// one faithful non-cone matcher answers for both modes and no separate cone
// path is needed. What is implemented here is therefore the exact pair of
// functions the server side of git runs: parse_path_pattern and
// last_matching_pattern_from_list from dir.c, over the wildmatch of
// wildmatch.c.

// gitPatternMatch is what a pattern list says about one path. Undecided is the
// answer when no pattern names the path at all, and it means the path inherits
// whatever its parent directory was decided to be.
type gitPatternMatch int

const (
	gitPatternUndecided gitPatternMatch = iota
	gitPatternNotMatched
	gitPatternMatched
)

// gitSparsePattern is one parsed line of a sparse-checkout pattern file.
type gitSparsePattern struct {
	// pattern is the line with the leading "!" and the trailing "/" removed.
	pattern []byte
	// negative is a leading "!": the path it names is excluded again.
	negative bool
	// mustBeDir is a trailing "/": the pattern names directories only.
	mustBeDir bool
	// noDir is a pattern with no "/" left in it, which matches a basename at
	// any depth rather than a path anchored at the root.
	noDir bool
	// endsWith is the "*literal" shape, which a basename comparison answers
	// with a suffix test rather than a glob walk.
	endsWith bool
	// nowildcardLen is how much of the pattern is literal text before the
	// first glob metacharacter, which is the prefix a match can compare
	// outright.
	nowildcardLen int
}

// gitUTF8BOM is the byte order mark git skips before reading patterns, so a
// pattern file written by an editor that emits one still has a usable first
// line.
var gitUTF8BOM = []byte("\xef\xbb\xbf")

// parseGitSparsePatterns reads a sparse-checkout pattern file.
//
// Lines are taken in file order and matched in reverse, so the last line that
// names a path decides it. A blank line and a line opening with "#" carry no
// pattern; trailing spaces are dropped unless a backslash quotes them; and a
// file whose last line has no newline still contributes that line, because
// git's blob reader appends the missing newline before parsing.
func parseGitSparsePatterns(blob []byte) []gitSparsePattern {
	blob = bytes.TrimPrefix(blob, gitUTF8BOM)
	if len(blob) == 0 {
		return nil
	}
	if blob[len(blob)-1] != '\n' {
		blob = append(append([]byte{}, blob...), '\n')
	}
	var patterns []gitSparsePattern
	for _, line := range bytes.Split(blob[:len(blob)-1], []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		patterns = append(patterns, parseGitSparsePattern(trimGitPatternTrailingSpaces(line)))
	}
	return patterns
}

// trimGitPatternTrailingSpaces drops the run of spaces a pattern ends with. A
// backslash anywhere in the line quotes the character after it, so "a\ " keeps
// its space while "a  " does not.
func trimGitPatternTrailingSpaces(line []byte) []byte {
	lastSpace := -1
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ' ':
			if lastSpace < 0 {
				lastSpace = i
			}
		case '\\':
			i++
			if i >= len(line) {
				return line
			}
			lastSpace = -1
		default:
			lastSpace = -1
		}
	}
	if lastSpace >= 0 {
		return line[:lastSpace]
	}
	return line
}

// parseGitSparsePattern decodes one pattern line into the flags matching reads.
func parseGitSparsePattern(line []byte) gitSparsePattern {
	pattern := gitSparsePattern{pattern: line}
	if len(pattern.pattern) > 0 && pattern.pattern[0] == '!' {
		pattern.negative = true
		pattern.pattern = pattern.pattern[1:]
	}
	if len(pattern.pattern) > 0 && pattern.pattern[len(pattern.pattern)-1] == '/' {
		pattern.mustBeDir = true
		pattern.pattern = pattern.pattern[:len(pattern.pattern)-1]
	}
	pattern.noDir = bytes.IndexByte(pattern.pattern, '/') < 0
	pattern.nowildcardLen = gitSimpleLength(pattern.pattern)
	if len(pattern.pattern) > 0 && pattern.pattern[0] == '*' && gitHasNoWildcard(pattern.pattern[1:]) {
		pattern.endsWith = true
	}
	return pattern
}

// gitSimpleLength reports how many leading bytes of a pattern are literal.
func gitSimpleLength(pattern []byte) int {
	for i := 0; i < len(pattern); i++ {
		if isGitGlobSpecial(pattern[i]) {
			return i
		}
	}
	return len(pattern)
}

// gitHasNoWildcard reports whether a pattern is entirely literal text.
func gitHasNoWildcard(pattern []byte) bool {
	return gitSimpleLength(pattern) == len(pattern)
}

// isGitGlobSpecial reports whether a byte begins a glob construct.
func isGitGlobSpecial(b byte) bool {
	return b == '*' || b == '?' || b == '[' || b == '\\'
}

// matchGitSparsePath decides one path against a pattern list.
//
// The list is walked from its last line backwards and the first line that
// names the path wins, which is what makes a later "!" line re-include what an
// earlier line excluded. A pattern that ends in "/" is only consulted for a
// directory. A path no pattern names is undecided, and the caller resolves that
// from the directory the path sits in.
func matchGitSparsePath(patterns []gitSparsePattern, path string, isDir bool) gitPatternMatch {
	basename := path
	if slash := strings.LastIndexByte(path, '/'); slash >= 0 {
		basename = path[slash+1:]
	}
	for i := len(patterns) - 1; i >= 0; i-- {
		pattern := &patterns[i]
		if pattern.mustBeDir && !isDir {
			continue
		}
		if pattern.noDir {
			if matchGitPatternBasename(basename, pattern) {
				return matchedGitPattern(pattern)
			}
			continue
		}
		if matchGitPatternPathname(path, pattern) {
			return matchedGitPattern(pattern)
		}
	}
	return gitPatternUndecided
}

func matchedGitPattern(pattern *gitSparsePattern) gitPatternMatch {
	if pattern.negative {
		return gitPatternNotMatched
	}
	return gitPatternMatched
}

// matchGitPatternBasename matches a pattern that carries no "/" against the
// last component of a path, so "*.md" selects every markdown file at every
// depth. The glob runs without the pathname flag, matching git, because a
// basename holds no separator for the flag to protect.
func matchGitPatternBasename(basename string, pattern *gitSparsePattern) bool {
	text := []byte(basename)
	switch {
	case pattern.nowildcardLen == len(pattern.pattern):
		return len(pattern.pattern) == len(text) && bytes.Equal(pattern.pattern, text)
	case pattern.endsWith:
		suffix := pattern.pattern[1:]
		return len(suffix) <= len(text) && bytes.Equal(suffix, text[len(text)-len(suffix):])
	default:
		return gitWildmatch(pattern.pattern, text, 0)
	}
}

// matchGitPatternPathname matches a pattern that carries a "/" against the whole
// path from the repository root.
//
// A leading "/" is dropped, which is what anchors "/dir" at the root while
// leaving "a/b" anchored there too — any pattern with an interior separator is
// already root-relative. The literal head of the pattern is compared outright
// before the glob runs, so a long directory prefix costs a comparison rather
// than a walk.
func matchGitPatternPathname(path string, pattern *gitSparsePattern) bool {
	if len(path) < 1 {
		return false
	}
	text := []byte(path)
	glob := pattern.pattern
	prefix := pattern.nowildcardLen
	if len(glob) > 0 && glob[0] == '/' {
		glob = glob[1:]
		prefix--
	}
	if prefix > 0 {
		if prefix > len(text) {
			return false
		}
		if !bytes.Equal(glob[:prefix], text[:prefix]) {
			return false
		}
		glob = glob[prefix:]
		text = text[prefix:]
		if len(glob) == 0 && len(text) == 0 {
			return true
		}
	}
	return gitWildmatch(glob, text, gitWildmatchPathname)
}

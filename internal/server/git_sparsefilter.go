package bleephub

import (
	"bytes"
	"strings"
)

// A "sparse:oid=<blob-ish>" filter-spec names a blob of sparse-checkout patterns;
// the pack it produces carries only the blobs those patterns select. This is the
// pattern language that decision is made in.
//
// git matches such a filter with its ordinary non-cone pattern engine, and the
// cone-mode files `git sparse-checkout set` writes are deliberately in that same
// language, so one faithful non-cone matcher answers for both modes. This mirrors
// git's parse_path_pattern and last_matching_pattern_from_list (dir.c) over the
// wildmatch of wildmatch.c.

// gitPatternMatch is what a pattern list says about one path. Undecided means no
// pattern names the path, so it inherits its parent directory's decision.
type gitPatternMatch int

const (
	gitPatternUndecided gitPatternMatch = iota
	gitPatternNotMatched
	gitPatternMatched
)

// gitSparsePattern is one parsed line of a sparse-checkout pattern file.
type gitSparsePattern struct {
	// pattern is the line with the leading "!" and trailing "/" removed.
	pattern []byte
	// negative: a leading "!" re-excludes the path.
	negative bool
	// mustBeDir: a trailing "/", the pattern names directories only.
	mustBeDir bool
	// noDir: no "/" remains, so it matches a basename at any depth rather than a
	// root-anchored path.
	noDir bool
	// endsWith: the "*literal" shape, answered by a suffix test rather than a glob walk.
	endsWith bool
	// nowildcardLen is the length of literal text before the first glob metacharacter.
	nowildcardLen int
}

// gitUTF8BOM is the byte order mark git skips before reading patterns.
var gitUTF8BOM = []byte("\xef\xbb\xbf")

// parseGitSparsePatterns reads a sparse-checkout pattern file. Lines are matched
// in reverse (last match wins); blank and "#" lines are skipped; trailing spaces
// are dropped unless backslash-quoted; and a final line with no newline still
// contributes, since git appends the missing newline before parsing.
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

// trimGitPatternTrailingSpaces drops trailing spaces; a backslash quotes the next
// character, so "a\ " keeps its space while "a  " does not.
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

// matchGitSparsePath decides one path against a pattern list, walking from the
// last line backwards so a later "!" re-includes what an earlier line excluded.
// A trailing-"/" pattern is consulted only for a directory; a path no pattern
// names is undecided, resolved by the caller from the path's directory.
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

// matchGitPatternBasename matches a "/"-free pattern against the last path
// component, so "*.md" selects markdown at any depth. The glob runs without the
// pathname flag, matching git, since a basename holds no separator to protect.
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

// matchGitPatternPathname matches a pattern carrying a "/" against the whole path
// from the repository root. A leading "/" is dropped (any interior separator is
// already root-relative), and the literal head is compared outright before the
// glob runs, so a long directory prefix costs a comparison rather than a walk.
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

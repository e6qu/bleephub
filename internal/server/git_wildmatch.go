package bleephub

import "bytes"

// wildmatch is the glob git matches every pattern with, and a sparse-checkout
// pattern file is a list of them. It is not fnmatch and it is not Go's
// path.Match: it understands "**", it decides on its own whether "*" may cross
// a directory separator, and it reports an abort distinct from a plain failure
// so that a "**" higher up the pattern knows a "*" below it can never succeed.
// Getting any of that wrong silently ships the wrong subset of a tree, so this
// is a transcription of dowild in wildmatch.c rather than an approximation.

// gitWildmatchPathname is git's WM_PATHNAME: "*" and "?" stop at a directory
// separator, and only "**" crosses one.
const gitWildmatchPathname = 1

// The three answers dowild distinguishes, plus the abort a nested "*" reports
// upward when it ran into a separator it may not cross.
const (
	gitWildMatch           = 0
	gitWildNoMatch         = 1
	gitWildAbortAll        = -1
	gitWildAbortToStarStar = -2
)

// gitWildmatch reports whether a pattern matches a whole text.
func gitWildmatch(pattern, text []byte, flags int) bool {
	return gitDoWild(pattern, 0, text, 0, flags) == gitWildMatch
}

// byteAtOrNUL reads one byte, reporting NUL past the end. git walks
// NUL-terminated strings and its control flow reads that terminator as an
// ordinary value; a path or a pattern never contains a NUL, so the same
// sentinel is exact here.
func byteAtOrNUL(s []byte, i int) byte {
	if i < 0 || i >= len(s) {
		return 0
	}
	return s[i]
}

// gitDoWild matches pattern[pi:] against text[ti:].
func gitDoWild(pattern []byte, pi int, text []byte, ti int, flags int) int {
	for {
		patternByte := byteAtOrNUL(pattern, pi)
		if patternByte == 0 {
			break
		}
		textByte := byteAtOrNUL(text, ti)
		if textByte == 0 && patternByte != '*' {
			return gitWildAbortAll
		}

		switch patternByte {
		case '\\':
			// The byte after a backslash is matched literally. A pattern
			// ending in a backslash compares against the NUL past the text
			// and therefore fails, which is what git does with it.
			pi++
			if textByte != byteAtOrNUL(pattern, pi) {
				return gitWildNoMatch
			}
		case '?':
			if flags&gitWildmatchPathname != 0 && textByte == '/' {
				return gitWildNoMatch
			}
		case '[':
			result, next := gitWildBracket(pattern, pi, textByte, flags)
			if result != gitWildMatch {
				return result
			}
			pi = next
		case '*':
			pi++
			matchSlash := flags&gitWildmatchPathname == 0
			if byteAtOrNUL(pattern, pi) == '*' {
				// The byte before the run of stars decides whether this is a
				// "**" that may cross separators: git only grants that to a
				// run that stands alone as a whole path component.
				beforeStars := pi - 2
				for {
					pi++
					if byteAtOrNUL(pattern, pi) != '*' {
						break
					}
				}
				if flags&gitWildmatchPathname != 0 {
					after := byteAtOrNUL(pattern, pi)
					standalone := beforeStars < 0 || pattern[beforeStars] == '/'
					componentEnd := after == 0 || after == '/' ||
						(after == '\\' && byteAtOrNUL(pattern, pi+1) == '/')
					if standalone && componentEnd {
						// "a/**/b" also matches "a/b": try the remainder with
						// the separator consumed by the stars.
						if after == '/' && gitDoWild(pattern, pi+1, text, ti, flags) == gitWildMatch {
							return gitWildMatch
						}
						matchSlash = true
					} else {
						matchSlash = false
					}
				} else {
					matchSlash = true
				}
			}
			if byteAtOrNUL(pattern, pi) == 0 {
				// Trailing "**" takes the rest of the text; trailing "*" takes
				// it only while it holds no separator.
				if !matchSlash && ti < len(text) && bytes.IndexByte(text[ti:], '/') >= 0 {
					return gitWildNoMatch
				}
				return gitWildMatch
			}
			if !matchSlash && byteAtOrNUL(pattern, pi) == '/' {
				// One star followed by a separator: the star takes the rest of
				// this component and the separator is matched by the pattern.
				if ti >= len(text) {
					return gitWildNoMatch
				}
				slash := bytes.IndexByte(text[ti:], '/')
				if slash < 0 {
					return gitWildNoMatch
				}
				ti += slash
				break
			}
			for textByte != 0 {
				if !isGitGlobSpecial(byteAtOrNUL(pattern, pi)) {
					// The star is followed by a literal, so everything before
					// the next occurrence of that literal belongs to the star.
					literal := byteAtOrNUL(pattern, pi)
					for {
						textByte = byteAtOrNUL(text, ti)
						if textByte == 0 || (!matchSlash && textByte == '/') || textByte == literal {
							break
						}
						ti++
					}
					if textByte != literal {
						return gitWildNoMatch
					}
				}
				matched := gitDoWild(pattern, pi, text, ti, flags)
				if matched != gitWildNoMatch {
					if !matchSlash || matched != gitWildAbortToStarStar {
						return matched
					}
				} else if !matchSlash && textByte == '/' {
					return gitWildAbortToStarStar
				}
				ti++
				textByte = byteAtOrNUL(text, ti)
			}
			return gitWildAbortAll
		default:
			if textByte != patternByte {
				return gitWildNoMatch
			}
		}
		ti++
		pi++
	}
	if ti < len(text) {
		return gitWildNoMatch
	}
	return gitWildMatch
}

// gitWildBracket matches one bracket expression against a single byte and
// reports where the pattern continues.
//
// It accepts the two negation spellings git accepts ("!" and "^"), ranges,
// backslash escapes and POSIX character classes, and it refuses to match a
// directory separator whenever the pathname flag is set, so "[a-z]" cannot
// swallow a "/" the way a literal range otherwise would.
func gitWildBracket(pattern []byte, pi int, textByte byte, flags int) (result, next int) {
	pi++
	patternByte := byteAtOrNUL(pattern, pi)
	if patternByte == '^' {
		patternByte = '!'
	}
	negated := patternByte == '!'
	if negated {
		pi++
		patternByte = byteAtOrNUL(pattern, pi)
	}
	previous := byte(0)
	matched := false
	for {
		if patternByte == 0 {
			return gitWildAbortAll, pi
		}
		switch {
		case patternByte == '\\':
			pi++
			patternByte = byteAtOrNUL(pattern, pi)
			if patternByte == 0 {
				return gitWildAbortAll, pi
			}
			if textByte == patternByte {
				matched = true
			}
		case patternByte == '-' && previous != 0 && byteAtOrNUL(pattern, pi+1) != 0 && byteAtOrNUL(pattern, pi+1) != ']':
			pi++
			patternByte = byteAtOrNUL(pattern, pi)
			if patternByte == '\\' {
				pi++
				patternByte = byteAtOrNUL(pattern, pi)
				if patternByte == 0 {
					return gitWildAbortAll, pi
				}
			}
			if textByte <= patternByte && textByte >= previous {
				matched = true
			}
			// A consumed range leaves no endpoint behind, so the byte after it
			// cannot open a second range.
			patternByte = 0
		case patternByte == '[' && byteAtOrNUL(pattern, pi+1) == ':':
			classStart := pi + 2
			end := classStart
			for byteAtOrNUL(pattern, end) != 0 && byteAtOrNUL(pattern, end) != ']' {
				end++
			}
			if byteAtOrNUL(pattern, end) == 0 {
				return gitWildAbortAll, end
			}
			if end-1 < classStart || pattern[end-1] != ':' {
				// No ":]" closed it, so the "[" was an ordinary member.
				if textByte == '[' {
					matched = true
				}
				patternByte = '['
				break
			}
			class := string(pattern[classStart : end-1])
			inClass, known := gitCharacterClassMatches(class, textByte)
			if !known {
				return gitWildAbortAll, end
			}
			if inClass {
				matched = true
			}
			pi = end
			patternByte = 0
		case textByte == patternByte:
			matched = true
		}
		previous = patternByte
		pi++
		patternByte = byteAtOrNUL(pattern, pi)
		if patternByte == ']' {
			break
		}
	}
	if matched == negated || (flags&gitWildmatchPathname != 0 && textByte == '/') {
		return gitWildNoMatch, pi
	}
	return gitWildMatch, pi
}

// gitCharacterClassMatches answers a POSIX character class, and reports whether
// the class is one git defines at all — an unknown one is a malformed pattern
// rather than a class that matches nothing.
func gitCharacterClassMatches(class string, b byte) (matches, known bool) {
	digit := b >= '0' && b <= '9'
	lower := b >= 'a' && b <= 'z'
	upper := b >= 'A' && b <= 'Z'
	alpha := lower || upper
	space := b == ' ' || b == '\t' || b == '\n' || b == '\v' || b == '\f' || b == '\r'
	printable := b >= 0x20 && b < 0x7f
	switch class {
	case "alnum":
		return alpha || digit, true
	case "alpha":
		return alpha, true
	case "blank":
		return b == ' ' || b == '\t', true
	case "cntrl":
		return b < 0x20 || b == 0x7f, true
	case "digit":
		return digit, true
	case "graph":
		return printable && b != ' ', true
	case "lower":
		return lower, true
	case "print":
		return printable, true
	case "punct":
		return printable && b != ' ' && !alpha && !digit, true
	case "space":
		return space, true
	case "upper":
		return upper, true
	case "xdigit":
		return digit || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F'), true
	}
	return false, false
}

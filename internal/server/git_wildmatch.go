package bleephub

import "bytes"

// git's wildmatch glob (used for sparse-checkout patterns). Not fnmatch and not
// path.Match: it handles "**", decides whether "*" may cross a separator, and
// distinguishes an abort from a plain failure so a "**" above knows a "*" below
// can never succeed. A wrong answer silently ships the wrong subset of a tree,
// so this is a direct transcription of dowild in git's wildmatch.c.

// gitWildmatchPathname is git's WM_PATHNAME: "*" and "?" stop at a directory
// separator, and only "**" crosses one.
const gitWildmatchPathname = 1

// The three dowild answers, plus the abort a nested "*" reports upward when it
// hits a separator it may not cross.
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

// byteAtOrNUL reads one byte, reporting NUL past the end — matching git's
// NUL-terminated walk, exact because a path or pattern never contains a NUL.
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
			// Match the next byte literally; a trailing backslash compares against
			// the NUL past the text and fails, as git does.
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
				// git treats a "**" as separator-crossing only when the run stands
				// alone as a whole path component; the byte before it decides.
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
						// "a/**/b" also matches "a/b": try the remainder with the
						// separator consumed by the stars.
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
				// Trailing "**" takes the rest; trailing "*" takes it only while it
				// holds no separator.
				if !matchSlash && ti < len(text) && bytes.IndexByte(text[ti:], '/') >= 0 {
					return gitWildNoMatch
				}
				return gitWildMatch
			}
			if !matchSlash && byteAtOrNUL(pattern, pi) == '/' {
				// "*/": the star takes the rest of this component, the pattern
				// matches the separator.
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
					// Star followed by a literal: everything before the next
					// occurrence of that literal belongs to the star.
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
// reports where the pattern continues. It accepts both negation spellings ("!"
// and "^"), ranges, backslash escapes and POSIX classes, and never matches a
// separator when the pathname flag is set.
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
			// A consumed range leaves no endpoint to open a second range.
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
				// No ":]" closed it, so "[" was an ordinary member.
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

// gitCharacterClassMatches answers a POSIX character class and reports whether
// git defines it at all; an unknown class is a malformed pattern, not an empty one.
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

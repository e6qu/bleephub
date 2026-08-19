package store

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// BranchProtectionPatternRule is a web-only (github.com settings UI) branch
// protection rule addressed by an fnmatch pattern instead of an exact branch
// name. GitHub's REST branch protection API forbids wildcards, so these rules
// live under /ui-data, never /api/v3. Protection carries the same shape the
// REST PUT body persists, and the enforcement chokepoint consults these rules
// only when no exact-name rule exists for the branch.
type BranchProtectionPatternRule struct {
	Pattern    string            `json:"pattern"`
	Protection *BranchProtection `json:"protection"`
}

// cloneBranchProtectionPatternRules deep-copies a rule list via a JSON
// round-trip so no caller holds a pointer into the stored table (STORE-021).
// The types are plain data that already round-trip through persistence.
func cloneBranchProtectionPatternRules(rules []*BranchProtectionPatternRule) []*BranchProtectionPatternRule {
	if len(rules) == 0 {
		return nil
	}
	raw, err := json.Marshal(rules)
	if err != nil {
		return nil
	}
	var out []*BranchProtectionPatternRule
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// ListBranchProtectionPatterns returns the repository's ordered pattern rules
// as a detached snapshot.
func (st *Store) ListBranchProtectionPatterns(repoID int) []*BranchProtectionPatternRule {
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	return cloneBranchProtectionPatternRules(st.Misc.BranchProtectionPatterns[repoID])
}

// SetBranchProtectionPatterns replaces the repository's pattern rules. An
// empty list clears them. The stored slice is a private copy of the caller's.
func (st *Store) SetBranchProtectionPatterns(repoID int, rules []*BranchProtectionPatternRule) {
	stored := cloneBranchProtectionPatternRules(rules)
	key := strconv.Itoa(repoID)
	st.Misc.Mu.Lock()
	defer st.Misc.Mu.Unlock()
	if len(stored) == 0 {
		delete(st.Misc.BranchProtectionPatterns, repoID)
		if st.Misc.Persist != nil {
			st.Misc.Persist.MustDelete("branch_protection_patterns", key)
		}
		return
	}
	st.Misc.BranchProtectionPatterns[repoID] = stored
	if st.Misc.Persist != nil {
		st.Misc.Persist.MustPut("branch_protection_patterns", key, stored)
	}
}

// MatchBranchPattern reports whether a branch name matches an fnmatch-style
// pattern with GitHub's branch-protection semantics: `*` matches any run of
// characters within one path segment (it does not cross `/`), `**` matches
// across segments, and `?` matches a single non-`/` character. Everything
// else is literal.
func MatchBranchPattern(pattern, branch string) bool {
	if pattern == "" {
		return false
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*")
				i += 2
			} else {
				b.WriteString("[^/]*")
				i++
			}
		case '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
			i++
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(branch)
}

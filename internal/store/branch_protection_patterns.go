package store

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// BranchProtectionPatternRule is a web-only branch protection rule addressed by
// an fnmatch pattern. GitHub's REST API forbids wildcards, so these live under
// /ui-data, and the enforcement chokepoint consults them only when no exact-name
// rule matches the branch.
type BranchProtectionPatternRule struct {
	Pattern    string            `json:"pattern"`
	Protection *BranchProtection `json:"protection"`
}

// cloneBranchProtectionPatternRules deep-copies via JSON so no caller holds a
// pointer into the stored table (STORE-021).
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

// ListBranchProtectionPatterns returns the repo's ordered pattern rules as a detached snapshot.
func (st *Store) ListBranchProtectionPatterns(repoID int) []*BranchProtectionPatternRule {
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	return cloneBranchProtectionPatternRules(st.Misc.BranchProtectionPatterns[repoID])
}

// SetBranchProtectionPatterns replaces the repo's pattern rules; an empty list clears them.
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

// MatchBranchPattern reports whether a branch matches an fnmatch pattern with
// GitHub's branch-protection semantics: `*` spans one path segment (not `/`),
// `**` spans segments, `?` matches one non-`/` char, everything else literal.
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

package bleephub

import (
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"
)

// acceptsTextMatch reports whether the request's Accept header opts into
// GitHub's text-match media type (application/vnd.github.text-match+json,
// historically also application/vnd.github.v3.text-match+json), which adds a
// text_matches array to each search result item.
func acceptsTextMatch(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), ".text-match+json")
}

// searchTextMatchProperty is one candidate (property name, property value)
// pair a search endpoint offers for text-match highlighting, e.g. an issue's
// "title" and "body".
type searchTextMatchProperty struct {
	name  string
	value string
}

// searchTextMatchMaxOccurrences bounds the matches listed inside one fragment.
const searchTextMatchMaxOccurrences = 20

// searchTextMatches builds the spec `search-result-text-matches` payload for
// one result item: for each property that actually contains a search term, a
// fragment of roughly ±100 characters around the first hit, with every term
// occurrence inside the fragment as [start, end) indices relative to the
// fragment. Always returns a non-nil array so items marshal `"text_matches":
// []` rather than null when nothing matched.
func searchTextMatches(objectURL, objectType string, properties []searchTextMatchProperty, terms []string) []map[string]interface{} {
	out := []map[string]interface{}{}
	for _, property := range properties {
		fragment, matches, ok := searchTextMatchFragment(property.value, terms)
		if !ok {
			continue
		}
		out = append(out, map[string]interface{}{
			"object_url":  objectURL,
			"object_type": objectType,
			"property":    property.name,
			"fragment":    fragment,
			"matches":     matches,
		})
	}
	return out
}

// searchTextMatchFragment locates the first term hit in value, cuts a fragment
// of about 100 characters of context either side (on rune boundaries), and
// lists each term occurrence inside the fragment with fragment-relative byte
// indices. Search terms are already lower-cased by the query parser, so the
// haystack is matched lower-cased; when lower-casing changes the byte length
// (rare non-ASCII case folds would desynchronize indices) matching falls back
// to the original bytes.
func searchTextMatchFragment(value string, terms []string) (string, []map[string]interface{}, bool) {
	if value == "" || len(terms) == 0 {
		return "", nil, false
	}
	haystack := strings.ToLower(value)
	if len(haystack) != len(value) {
		haystack = value
	}

	// The fragment window anchors on the earliest hit of any term.
	first, firstEnd := -1, 0
	for _, term := range terms {
		if term == "" {
			continue
		}
		idx := strings.Index(haystack, term)
		if idx >= 0 && (first == -1 || idx < first) {
			first, firstEnd = idx, idx+len(term)
		}
	}
	if first < 0 {
		return "", nil, false
	}

	const context = 100
	start := first - context
	if start < 0 {
		start = 0
	}
	end := firstEnd + context
	if end > len(value) {
		end = len(value)
	}
	for start > 0 && !utf8.RuneStart(value[start]) {
		start--
	}
	for end < len(value) && !utf8.RuneStart(value[end]) {
		end++
	}

	fragment := value[start:end]
	fragmentHaystack := haystack[start:end]
	matches := []map[string]interface{}{}
	for _, term := range terms {
		if term == "" {
			continue
		}
		for from := 0; len(matches) < searchTextMatchMaxOccurrences; {
			idx := strings.Index(fragmentHaystack[from:], term)
			if idx < 0 {
				break
			}
			pos := from + idx
			matches = append(matches, map[string]interface{}{
				"text":    fragment[pos : pos+len(term)],
				"indices": []int{pos, pos + len(term)},
			})
			from = pos + len(term)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i]["indices"].([]int)[0] < matches[j]["indices"].([]int)[0]
	})
	return fragment, matches, true
}

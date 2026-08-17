package bleephub

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type tokenizedSearchTerm struct {
	Value  string
	Quoted bool
}

// tokenizeSearchQuery splits on whitespace outside quotes. Quotes work both
// around a complete term ("pull request") and around a qualifier value
// (language:"Objective C"), matching GitHub's documented query grammar.
func tokenizeSearchQuery(raw string) []tokenizedSearchTerm {
	var out []tokenizedSearchTerm
	var current strings.Builder
	quoted := false
	tokenQuoted := false
	escaped := false
	flush := func() {
		if current.Len() == 0 {
			tokenQuoted = false
			return
		}
		out = append(out, tokenizedSearchTerm{Value: current.String(), Quoted: tokenQuoted})
		current.Reset()
		tokenQuoted = false
	}
	for _, char := range raw {
		if escaped {
			current.WriteRune(char)
			escaped = false
			continue
		}
		if char == '\\' && quoted {
			escaped = true
			continue
		}
		if char == '"' {
			if current.Len() == 0 && !quoted {
				tokenQuoted = true
			}
			quoted = !quoted
			continue
		}
		if (char == ' ' || char == '\t' || char == '\n' || char == '\r') && !quoted {
			flush()
			continue
		}
		current.WriteRune(char)
	}
	if escaped {
		current.WriteRune('\\')
	}
	flush()
	return out
}

var searchQualifierScopes = map[string]map[string]bool{
	"repositories": qualifierSet(
		"repo", "user", "org", "language", "topic", "is", "in",
		"archived", "fork", "template", "mirror", "visibility", "license",
		"size", "followers", "forks", "stars", "topics", "created", "pushed",
		"good-first-issues", "help-wanted-issues", "has", "deployable", "deployed",
	),
	"issues": qualifierSet(
		"repo", "user", "org", "label", "state", "is", "in",
		"author", "assignee", "milestone", "mentions", "no", "type",
	),
	"code": qualifierSet(
		"repo", "user", "org", "language", "path", "extension", "ext", "filename", "file",
	),
	"users":   qualifierSet("type", "in", "repos", "followers", "location", "created", "language"),
	"commits": qualifierSet("repo", "user", "org", "author", "hash"),
	"labels":  qualifierSet(),
	"topics":  qualifierSet(),
}

func qualifierSet(values ...string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}

func (q searchQuery) validateFor(searchType string) error {
	allowed := searchQualifierScopes[searchType]
	for _, qualifier := range q.Qualifiers {
		if !allowed[qualifier.Key] &&
			!(searchType == "repositories" && strings.HasPrefix(qualifier.Key, "props.")) {
			return unsupportedQualifierError{key: qualifier.Key}
		}
		if qualifier.Negated && qualifier.Key == "in" {
			return unsupportedQualifierError{key: qualifier.Key, value: qualifier.Value}
		}
		if qualifier.Key == "is" {
			value := strings.ToLower(qualifier.Value)
			valid := false
			switch searchType {
			case "repositories":
				valid = value == "public" || value == "private" || value == "internal" || value == "sponsorable"
			case "issues":
				valid = value == "issue" || value == "pr" || value == "pull-request" ||
					value == "open" || value == "closed" || value == "public" ||
					value == "private" || value == "internal"
			}
			if !valid {
				return unsupportedQualifierError{key: qualifier.Key, value: qualifier.Value}
			}
		}
		if qualifier.Key == "in" {
			for _, value := range strings.Split(strings.ToLower(qualifier.Value), ",") {
				valid := searchType == "repositories" &&
					(value == "name" || value == "description" || value == "topics" || value == "readme") ||
					searchType == "issues" && (value == "title" || value == "body") ||
					searchType == "users" && (value == "login" || value == "name" || value == "email" || value == "fullname")
				if !valid {
					return unsupportedQualifierError{key: qualifier.Key, value: qualifier.Value}
				}
			}
		}
	}
	return nil
}

func (q searchQuery) excludes(key, candidate string) bool {
	for _, qualifier := range q.Qualifiers {
		if qualifier.Negated && qualifier.Key == key && strings.EqualFold(qualifier.Value, candidate) {
			return true
		}
	}
	return false
}

func (q searchQuery) includes(key, candidate string) bool {
	for _, qualifier := range q.Qualifiers {
		if !qualifier.Negated && qualifier.Key == key && strings.EqualFold(qualifier.Value, candidate) {
			return true
		}
	}
	return false
}

type numericSearchConstraint struct {
	min          int64
	max          int64
	hasMin       bool
	hasMax       bool
	includeMin   bool
	includeMax   bool
	exact        bool
	exactValue   int64
	originalText string
}

func parseNumericSearchConstraint(value string) (numericSearchConstraint, error) {
	out := numericSearchConstraint{originalText: value}
	if left, right, found := strings.Cut(value, ".."); found {
		min, err := strconv.ParseInt(left, 10, 64)
		if err != nil || min < 0 {
			return out, fmt.Errorf("invalid numeric range")
		}
		max, err := strconv.ParseInt(right, 10, 64)
		if err != nil || max < min {
			return out, fmt.Errorf("invalid numeric range")
		}
		out.min, out.max = min, max
		out.hasMin, out.hasMax = true, true
		out.includeMin, out.includeMax = true, true
		return out, nil
	}
	for _, prefix := range []string{">=", "<=", ">", "<"} {
		if strings.HasPrefix(value, prefix) {
			number, err := strconv.ParseInt(strings.TrimPrefix(value, prefix), 10, 64)
			if err != nil || number < 0 {
				return out, fmt.Errorf("invalid numeric comparison")
			}
			switch prefix {
			case ">=":
				out.min, out.hasMin, out.includeMin = number, true, true
			case ">":
				out.min, out.hasMin = number, true
			case "<=":
				out.max, out.hasMax, out.includeMax = number, true, true
			case "<":
				out.max, out.hasMax = number, true
			}
			return out, nil
		}
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number < 0 {
		return out, fmt.Errorf("invalid number")
	}
	out.exact, out.exactValue = true, number
	return out, nil
}

func (c numericSearchConstraint) matches(value int64) bool {
	if c.exact {
		return value == c.exactValue
	}
	if c.hasMin && (value < c.min || value == c.min && !c.includeMin) {
		return false
	}
	if c.hasMax && (value > c.max || value == c.max && !c.includeMax) {
		return false
	}
	return true
}

type dateSearchConstraint struct {
	min        time.Time
	max        time.Time
	hasMin     bool
	hasMax     bool
	includeMin bool
	includeMax bool
}

func parseDateSearchConstraint(value string) (dateSearchConstraint, error) {
	var out dateSearchConstraint
	if left, right, found := strings.Cut(value, ".."); found {
		min, minDateOnly, err := parseSearchDate(left)
		if err != nil {
			return out, err
		}
		max, maxDateOnly, err := parseSearchDate(right)
		if err != nil || max.Before(min) {
			return out, fmt.Errorf("invalid date range")
		}
		if maxDateOnly {
			max = max.Add(24*time.Hour - time.Nanosecond)
		}
		_ = minDateOnly
		out.min, out.max = min, max
		out.hasMin, out.hasMax = true, true
		out.includeMin, out.includeMax = true, true
		return out, nil
	}
	for _, prefix := range []string{">=", "<=", ">", "<"} {
		if strings.HasPrefix(value, prefix) {
			date, dateOnly, err := parseSearchDate(strings.TrimPrefix(value, prefix))
			if err != nil {
				return out, err
			}
			switch prefix {
			case ">=":
				out.min, out.hasMin, out.includeMin = date, true, true
			case ">":
				if dateOnly {
					date = date.Add(24*time.Hour - time.Nanosecond)
				}
				out.min, out.hasMin = date, true
			case "<=":
				if dateOnly {
					date = date.Add(24*time.Hour - time.Nanosecond)
				}
				out.max, out.hasMax, out.includeMax = date, true, true
			case "<":
				out.max, out.hasMax = date, true
			}
			return out, nil
		}
	}
	date, dateOnly, err := parseSearchDate(value)
	if err != nil {
		return out, err
	}
	out.min, out.max = date, date
	out.hasMin, out.hasMax = true, true
	out.includeMin, out.includeMax = true, true
	if dateOnly {
		out.max = date.Add(24*time.Hour - time.Nanosecond)
	}
	return out, nil
}

func parseSearchDate(value string) (time.Time, bool, error) {
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return parsed.UTC(), true, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false, err
	}
	return parsed.UTC(), false, nil
}

func (c dateSearchConstraint) matches(value time.Time) bool {
	value = value.UTC()
	if c.hasMin && (value.Before(c.min) || value.Equal(c.min) && !c.includeMin) {
		return false
	}
	if c.hasMax && (value.After(c.max) || value.Equal(c.max) && !c.includeMax) {
		return false
	}
	return true
}

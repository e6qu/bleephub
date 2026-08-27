package store

import "strings"

// Pure label predicates shared by the REST handlers and the GraphQL resolvers.

// IssueHasAllLabels reports whether an issue carries every named label.
func IssueHasAllLabels(st *Store, issue *Issue, labelNames []string, repoID int) bool {
	return LabelIDsCoverNames(st, issue.LabelIDs, labelNames)
}

// LabelIDsCoverNames asks about label ids rather than issues, so pull requests
// can be filtered by the same predicate.
func LabelIDsCoverNames(st *Store, labelIDs []int, labelNames []string) bool {
	for _, name := range labelNames {
		found := false
		for _, lid := range labelIDs {
			l := st.GetLabel(lid)
			if l != nil && l.Name == strings.TrimSpace(name) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

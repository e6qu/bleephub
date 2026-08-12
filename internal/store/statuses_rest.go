package store

import "strings"

// CommitStatusState is a commit status's state. normalizeStatusState collapses
// any client input to exactly one of these four, so typing the field (and the
// normalizer's return) propagates that guarantee instead of discarding it at
// the boundary. A typed string marshals to JSON identically to a plain string.
type CommitStatusState string

func computeCombinedState(statuses []*CommitStatus) string {
	for _, st := range statuses {
		if st.State == "error" {
			return "error"
		}
	}
	for _, st := range statuses {
		if st.State == "failure" {
			return "failure"
		}
	}
	for _, st := range statuses {
		if st.State == "pending" {
			return "pending"
		}
	}
	return "success"
}

// maxCommitStatusesPerRef bounds the number of statuses retained per repo+sha.
// Real GitHub rejects more than 1000 statuses on a single sha; matching that
// keeps a spammed ref from growing the store without limit.
const maxCommitStatusesPerRef = 1000

func normalizeStatusState(state string) CommitStatusState {
	switch strings.ToLower(state) {
	case "success":
		return CommitStatusSuccess
	case "failure":
		return CommitStatusFailure
	case "pending":
		return CommitStatusPending
	case "error":
		return CommitStatusError
	default:
		return CommitStatusPending
	}
}

func statusKey(repoKey, ref string) string {
	return repoKey + ":" + ref
}

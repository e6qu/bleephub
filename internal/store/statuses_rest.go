package store

import "strings"

// CommitStatusState is a commit status's state; normalizeStatusState collapses
// any client input to one of the four values.
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

// maxCommitStatusesPerRef bounds statuses per repo+sha, matching GitHub's 1000
// cap on a single sha.
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

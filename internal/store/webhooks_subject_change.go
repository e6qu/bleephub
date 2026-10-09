package store

// SubjectChange records what one mutation changed on an issue or PR, supplying
// the before/after pairs the webhook layer diffs to fan a single API call out
// into per-field actions (`edited`, `labeled`, `closed`, ...). Both REST and
// GraphQL feed it. A nil pointer or empty state means the field was untouched;
// *From scalars are set only on a real change, so a no-op delivers nothing.
type SubjectChange struct {
	// Pre-edit values behind an `edited` payload's `changes` member.
	TitleFrom   *string
	BodyFrom    *string
	BaseRefFrom *string

	// Full sets; the emitter diffs them into one action per entry that entered or left.
	LabelsFrom    []int
	LabelsTo      *[]int
	AssigneesFrom []int
	AssigneesTo   *[]int

	// Previous / requested milestone id (0 = none/cleared); To nil when untouched.
	MilestoneFrom int
	MilestoneTo   *int

	// Store states ("OPEN", "CLOSED", "MERGED"); only a real transition acts.
	StateFrom string
	StateTo   string

	// Requested reviewers and teams before and after (To nil when untouched),
	// diffed into review_requested and review_request_removed.
	ReviewersFrom   []int
	ReviewersTo     *[]int
	ReviewTeamsFrom []int
	ReviewTeamsTo   *[]int
	// Reviewers and teams asked again: one review_requested each, whether or
	// not they were already requested.
	ReviewersRerequested   []int
	ReviewTeamsRerequested []int
}

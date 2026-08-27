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
}

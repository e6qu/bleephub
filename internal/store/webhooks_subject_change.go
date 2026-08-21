package store

// SubjectChange records what a single mutation changed on an issue or pull
// request. GitHub does not deliver one webhook per API call: a PATCH that
// retitles an issue, swaps a label, and closes it fans out to `edited`,
// `labeled`, `unlabeled`, and `closed`, each carrying its own extra payload
// members. The webhook layer needs the before/after pair for every field to
// derive that fan-out, and both the REST handlers and the GraphQL resolvers
// feed it, so the value type lives here where both packages already look.
//
// A nil pointer (or an empty state string) means the mutation did not touch
// that field. The *From scalars are set only when the value actually changed,
// so a no-op write delivers nothing.
type SubjectChange struct {
	// TitleFrom / BodyFrom / BaseRefFrom are the pre-edit values behind the
	// `changes` member of an `edited` payload.
	TitleFrom   *string
	BodyFrom    *string
	BaseRefFrom *string

	// LabelsFrom / LabelsTo and AssigneesFrom / AssigneesTo are full sets; the
	// emitter diffs them into one labeled/unlabeled (assigned/unassigned)
	// action per entry that entered or left.
	LabelsFrom    []int
	LabelsTo      *[]int
	AssigneesFrom []int
	AssigneesTo   *[]int

	// MilestoneFrom is the previous milestone id (0 = none); MilestoneTo is
	// the requested one (0 = cleared), nil when untouched.
	MilestoneFrom int
	MilestoneTo   *int

	// StateFrom / StateTo are store states ("OPEN", "CLOSED", "MERGED");
	// only a real transition yields a closed/reopened action.
	StateFrom string
	StateTo   string
}

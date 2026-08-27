package store

// ProjectV2Event describes one delivery of the projects_v2 webhook family,
// built by both the GraphQL and REST write paths so the two surfaces agree.
// GitHub splits the family three ways, carrying a different object each:
//
//	projects_v2               the project itself
//	projects_v2_item          the item, plus the project it belongs to
//	projects_v2_status_update the status update, plus its project
//
// None carry a repository, so delivery is to the owning account's hooks.
type ProjectV2Event struct {
	Event   string
	Action  string
	Project *ProjectV2 // always set
	// Item and StatusUpdate are set for the events whose subject they are.
	Item         *ProjectV2Item
	StatusUpdate *ProjectV2StatusUpdate
	Sender       *User
	// Changes carries the before/after diff for `edited` actions, keyed by
	// field name; nil when the action has no diff.
	Changes map[string]ProjectV2Change
}

type ProjectV2Change struct {
	From interface{}
	To   interface{}
}

const (
	ProjectV2EventProject      = "projects_v2"
	ProjectV2EventItem         = "projects_v2_item"
	ProjectV2EventStatusUpdate = "projects_v2_status_update"
)

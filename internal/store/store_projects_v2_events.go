package store

// ProjectV2Event describes one delivery of the projects_v2 webhook family.
// The GraphQL and REST write paths both build one of these and hand it to the
// server's emitter, so the two surfaces cannot disagree about which action a
// mutation produces or which objects ride along with it.
//
// GitHub splits the family three ways, and the object each carries differs:
//
//	projects_v2               the project itself
//	projects_v2_item          the item, plus the project it belongs to
//	projects_v2_status_update the status update, plus its project
//
// None of them carry a repository — a project belongs to a user or an
// organization — so delivery is to the owning account's hooks.
type ProjectV2Event struct {
	// Event is the X-GitHub-Event name: projects_v2, projects_v2_item or
	// projects_v2_status_update.
	Event string
	// Action is the event's action key (created, edited, closed, archived, …).
	Action string
	// Project is the project the event concerns. Always set: every event in
	// the family names one.
	Project *ProjectV2
	// Item and StatusUpdate are set for the events whose subject they are.
	Item         *ProjectV2Item
	StatusUpdate *ProjectV2StatusUpdate
	// Sender is the account that performed the change.
	Sender *User
	// Changes carries GitHub's before/after diff for the `edited` actions,
	// keyed by field name. A nil map means the action has no diff.
	Changes map[string]ProjectV2Change
}

// ProjectV2Change is one field's before/after pair in an edited payload.
type ProjectV2Change struct {
	From interface{}
	To   interface{}
}

// Projects v2 webhook event names.
const (
	ProjectV2EventProject      = "projects_v2"
	ProjectV2EventItem         = "projects_v2_item"
	ProjectV2EventStatusUpdate = "projects_v2_status_update"
)

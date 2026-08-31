package store

import (
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

// ProjectsV2 store — covers what `gh project create`, `gh project item-add`,
// and `gh issue view --json projectItems` exercise, not GitHub's full schema.

// ProjectV2Owner is a project's resolved owner (org or user). Both the REST
// and GraphQL layers resolve into this before the shared access predicates.
type ProjectV2Owner struct {
	ID        int
	OwnerType string // "Organization" or "User"
	Login     string
	Org       *Org
	User      *User
}

// ProjectV2 is a Projects v2 project owned by a user or organization, with a
// stable per-owner Number and a globally unique NodeID.
type ProjectV2 struct {
	ID        int
	NodeID    string
	Number    int    // per-owner sequential
	OwnerID   int    // user/org ID
	OwnerType string // "User" or "Organization"
	CreatorID int    // user who created the project
	Title     string
	Closed    bool
	ClosedAt  *time.Time
	Public    bool
	URL       string
	CreatedAt time.Time
	UpdatedAt time.Time

	// ShortDescription is the one-line blurb; Readme is the markdown body.
	ShortDescription string
	Readme           string
	// Template marks the project as one new projects may be copied from.
	Template bool
	// LinkedRepoIDs / LinkedTeamIDs are the linked repositories and teams.
	LinkedRepoIDs []int
	LinkedTeamIDs []int
	// Collaborators are per-account grants layered on the owner's access.
	Collaborators []*ProjectV2Collaborator
}

// ProjectV2Collaborator is one account's explicit permission on a project.
// Role is GitHub's ProjectV2Roles enum: READER, WRITER, ADMIN or NONE.
type ProjectV2Collaborator struct {
	UserID int
	TeamID int // set instead of UserID when the collaborator is a team
	Role   string
}

// ProjectV2StatusUpdate is a dated progress note posted on a project.
type ProjectV2StatusUpdate struct {
	ID         int
	NodeID     string
	ProjectID  int
	CreatorID  int
	Body       string
	Status     string // ProjectV2StatusUpdateStatus enum, "" when unset
	StartDate  string // YYYY-MM-DD, "" when unset
	TargetDate string // YYYY-MM-DD, "" when unset
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ProjectV2Workflow is one automation rule on a project.
type ProjectV2Workflow struct {
	ID        int
	NodeID    string
	ProjectID int
	Number    int // per-project sequential
	Name      string
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ProjectV2Item links an issue or PR (or a draft issue) to a project.
// ContentType is "Issue", "PullRequest", or "DraftIssue".
type ProjectV2Item struct {
	ID          int
	NodeID      string
	ProjectID   int
	ContentType string
	ContentID   int // 0 for DraftIssue
	CreatorID   int
	DraftTitle  string
	DraftBody   string
	FieldValues map[int]*ProjectV2ItemFieldValue // fieldID → value
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ArchivedAt  *time.Time
	// Position orders the item within its project. Items are handed out in
	// ascending Position, and updateProjectV2ItemPosition rewrites it.
	Position int
}

// ProjectV2FieldDataType is the custom-field data type, spelled uppercase to
// match the GraphQL enum; REST handlers lowercase it on the wire.
type ProjectV2FieldDataType string

const (
	ProjectV2FieldSingleSelect ProjectV2FieldDataType = "SINGLE_SELECT"
	ProjectV2FieldMultiSelect  ProjectV2FieldDataType = "MULTI_SELECT"
	ProjectV2FieldText         ProjectV2FieldDataType = "TEXT"
	ProjectV2FieldNumber       ProjectV2FieldDataType = "NUMBER"
	ProjectV2FieldDate         ProjectV2FieldDataType = "DATE"
	ProjectV2FieldIteration    ProjectV2FieldDataType = "ITERATION"
)

// SelectsOptions reports whether the data type carries a list of selectable
// options, which SINGLE_SELECT and MULTI_SELECT both do.
func (t ProjectV2FieldDataType) SelectsOptions() bool {
	return t == ProjectV2FieldSingleSelect || t == ProjectV2FieldMultiSelect
}

// ProjectV2Field is a column on a project. SINGLE_SELECT carries
// per-option metadata in Options; ITERATION carries its schedule in
// Iteration.
type ProjectV2Field struct {
	ID        int
	NodeID    string
	ProjectID int
	Name      string
	DataType  ProjectV2FieldDataType
	Options   []*ProjectV2SingleSelectOption
	Iteration *ProjectV2IterationConfiguration
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ProjectV2SingleSelectOption is one selectable value on a
// SINGLE_SELECT field (e.g. Status: Todo / In Progress / Done).
type ProjectV2SingleSelectOption struct {
	ID          string // GitHub uses 8-char alnum IDs ("47fc9ee4"); we generate similar
	Name        string
	Color       string // GitHub's option color enum (BLUE, GRAY, GREEN, ...)
	Description string
}

// ProjectV2IterationConfiguration is the schedule of an ITERATION
// field: a default duration plus the concrete iterations.
type ProjectV2IterationConfiguration struct {
	StartDate  string // date of the first iteration, YYYY-MM-DD
	Duration   int    // default iteration length in days
	Iterations []*ProjectV2Iteration
}

// ProjectV2Iteration is one concrete iteration on an ITERATION field.
type ProjectV2Iteration struct {
	ID        string // same 8-char ID space as single-select options
	Title     string
	StartDate string // YYYY-MM-DD
	Duration  int    // days
}

// ProjectV2ItemFieldValue is an item's value for one field; which member is
// set depends on the field's data type.
type ProjectV2ItemFieldValue struct {
	FieldID     int
	OptionID    string   // SINGLE_SELECT
	OptionName  string   // denormalised so reads don't chase the field
	TextValue   string   // TEXT
	NumberValue float64  // NUMBER
	DateValue   string   // DATE, YYYY-MM-DD
	IterationID string   // ITERATION
	OptionIDs   []string // MULTI_SELECT, ordered set
	OptionNames []string
}

// ProjectV2View is a board/table/roadmap view inside a project.
type ProjectV2View struct {
	ID            int
	NodeID        string
	ProjectID     int
	Number        int // per-project sequential
	Name          string
	Layout        string // "table", "board", or "roadmap"
	CreatorID     int
	Filter        *string // the view's filter query, nil when unset
	VisibleFields []int   // field IDs shown in the view
	CreatedAt     time.Time
	UpdatedAt     time.Time

	// GroupBy / VerticalGroupBy are field IDs the view groups rows and
	// columns by; SortBy is the ordered sort specification.
	GroupBy         []int
	VerticalGroupBy []int
	SortBy          []*ProjectV2ViewSort
}

// ProjectV2ViewSort is one entry of a view's sort specification.
type ProjectV2ViewSort struct {
	FieldID   int
	Direction string // "ASC" or "DESC"
}

// ProjectV2Store is the in-memory store. Concurrency-safe via mu.
type ProjectV2Store struct {
	Mu              sync.RWMutex     `json:"-"`
	ClockMu         sync.RWMutex     `json:"-"`
	ClockNow        func() time.Time `json:"-"`
	projects        map[int]*ProjectV2
	items           map[int]*ProjectV2Item
	itemsByOwner    map[int][]*ProjectV2Item // contentID → items it appears in
	fields          map[int]*ProjectV2Field
	FieldsByProj    map[int][]*ProjectV2Field `json:"-"`
	views           map[int]*ProjectV2View
	viewsByProj     map[int][]*ProjectV2View
	statusUpdates   map[int]*ProjectV2StatusUpdate
	statusByProj    map[int][]*ProjectV2StatusUpdate
	workflows       map[int]*ProjectV2Workflow
	workflowsByProj map[int][]*ProjectV2Workflow
	nextProjectID   int
	nextItemID      int
	nextFieldID     int
	nextOptionSeed  int
	nextViewID      int
	nextStatusID    int
	nextWorkflowID  int
	Persist         *Persistence `json:"-"`
}

func (s *ProjectV2Store) CurrentTime() time.Time {
	if s != nil {
		s.ClockMu.RLock()
		clockNow := s.ClockNow
		s.ClockMu.RUnlock()
		if clockNow != nil {
			return clockNow().UTC()
		}
	}
	return time.Now().UTC()
}

func NewProjectV2Store(p *Persistence) *ProjectV2Store {
	return &ProjectV2Store{
		projects:        map[int]*ProjectV2{},
		items:           map[int]*ProjectV2Item{},
		itemsByOwner:    map[int][]*ProjectV2Item{},
		fields:          map[int]*ProjectV2Field{},
		FieldsByProj:    map[int][]*ProjectV2Field{},
		views:           map[int]*ProjectV2View{},
		viewsByProj:     map[int][]*ProjectV2View{},
		statusUpdates:   map[int]*ProjectV2StatusUpdate{},
		statusByProj:    map[int][]*ProjectV2StatusUpdate{},
		workflows:       map[int]*ProjectV2Workflow{},
		workflowsByProj: map[int][]*ProjectV2Workflow{},
		nextProjectID:   1,
		nextItemID:      1,
		nextFieldID:     1,
		nextOptionSeed:  1,
		nextViewID:      1,
		nextStatusID:    1,
		nextWorkflowID:  1,
		Persist:         p,
	}
}

// CreateProject creates a new ProjectV2 owned by the given user or org,
// recording the creating user.
func (s *ProjectV2Store) CreateProject(ownerID int, ownerType, title string, creatorID int) *ProjectV2 {
	s.Mu.Lock()
	p := s.createProjectLocked(ownerID, ownerType, title, creatorID, nil)
	s.persistProjectLocked(p)
	id := p.ID
	s.Mu.Unlock()
	// Seeding takes the lock per mutator, so it runs after the project row is
	// published.
	s.SeedProjectDefaults(id, creatorID)
	return s.GetProject(id)
}

// GetProject returns a project by ID or nil.
func (s *ProjectV2Store) GetProject(id int) *ProjectV2 {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return cloneProjectV2(s.projects[id])
}

// LookupProjectByNodeID returns the project with the given global node id.
func (s *ProjectV2Store) LookupProjectByNodeID(nodeID string) *ProjectV2 {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "PVT_kgDO"); ok {
		if p := s.projects[id]; p != nil && p.NodeID == nodeID {
			return cloneProjectV2(p)
		}
	}
	for _, p := range s.projects {
		if p.NodeID == nodeID {
			return cloneProjectV2(p)
		}
	}
	return nil
}

// AddItem adds an Issue or PullRequest to a project. contentID is the issue
// or PR database ID; contentType is "Issue" or "PullRequest".
func (s *ProjectV2Store) AddItem(projectID int, contentType string, contentID, creatorID int) *ProjectV2Item {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if _, ok := s.projects[projectID]; !ok {
		return nil
	}
	// Dedup on (project, content).
	for _, it := range s.itemsByOwner[contentID] {
		if it.ProjectID == projectID && it.ContentType == contentType {
			return cloneProjectV2Item(it)
		}
	}
	id := s.nextItemID
	s.nextItemID++
	now := s.CurrentTime()
	it := &ProjectV2Item{
		ID:          id,
		NodeID:      fmt.Sprintf("PVTI_kgDO%08d", id),
		ProjectID:   projectID,
		ContentType: contentType,
		ContentID:   contentID,
		CreatorID:   creatorID,
		FieldValues: map[int]*ProjectV2ItemFieldValue{},
		CreatedAt:   now,
		UpdatedAt:   now,
		Position:    s.nextPositionLocked(projectID),
	}
	s.items[id] = it
	s.itemsByOwner[contentID] = append(s.itemsByOwner[contentID], it)
	if s.Persist != nil {
		s.Persist.MustPut("project_v2_items", strconv.Itoa(id), it)
	}
	return it
}

// AddDraftItem adds a draft issue to a project.
func (s *ProjectV2Store) AddDraftItem(projectID int, title, body string, creatorID int) *ProjectV2Item {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if _, ok := s.projects[projectID]; !ok {
		return nil
	}
	id := s.nextItemID
	s.nextItemID++
	now := s.CurrentTime()
	it := &ProjectV2Item{
		ID:          id,
		NodeID:      fmt.Sprintf("PVTI_kgDO%08d", id),
		ProjectID:   projectID,
		ContentType: "DraftIssue",
		CreatorID:   creatorID,
		DraftTitle:  title,
		DraftBody:   body,
		FieldValues: map[int]*ProjectV2ItemFieldValue{},
		CreatedAt:   now,
		UpdatedAt:   now,
		Position:    s.nextPositionLocked(projectID),
	}
	s.items[id] = it
	if s.Persist != nil {
		s.Persist.MustPut("project_v2_items", strconv.Itoa(id), it)
	}
	return it
}

// ListItemsForIssue returns every project item wrapping the issue with the
// given database ID.
func (s *ProjectV2Store) ListItemsForIssue(issueID int) []*ProjectV2Item {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	out := make([]*ProjectV2Item, 0)
	for _, it := range s.itemsByOwner[issueID] {
		if it.ContentType == "Issue" {
			out = append(out, cloneProjectV2Item(it))
		}
	}
	return out
}

// ListItemsForPR returns every project item wrapping the PR with the given
// database ID.
func (s *ProjectV2Store) ListItemsForPR(prID int) []*ProjectV2Item {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	out := make([]*ProjectV2Item, 0)
	for _, it := range s.itemsByOwner[prID] {
		if it.ContentType == "PullRequest" {
			out = append(out, cloneProjectV2Item(it))
		}
	}
	return out
}

// GetItem returns a project item by id.
func (s *ProjectV2Store) GetItem(id int) *ProjectV2Item {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return cloneProjectV2Item(s.items[id])
}

// LookupItemByNodeID returns the item with the given GraphQL node id.
func (s *ProjectV2Store) LookupItemByNodeID(nodeID string) *ProjectV2Item {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "PVTI_kgDO"); ok {
		if it := s.items[id]; it != nil && it.NodeID == nodeID {
			return cloneProjectV2Item(it)
		}
	}
	for _, it := range s.items {
		if it.NodeID == nodeID {
			return cloneProjectV2Item(it)
		}
	}
	return nil
}

// CreateField adds a field column to a project. options applies to
// SINGLE_SELECT and iteration to ITERATION; their IDs are assigned here.
func (s *ProjectV2Store) CreateField(projectID int, name string, dataType ProjectV2FieldDataType, options []*ProjectV2SingleSelectOption, iteration *ProjectV2IterationConfiguration) *ProjectV2Field {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if _, ok := s.projects[projectID]; !ok {
		return nil
	}
	id := s.nextFieldID
	s.nextFieldID++
	now := s.CurrentTime()
	f := &ProjectV2Field{
		ID:        id,
		NodeID:    fmt.Sprintf("PVTF_kgDO%08d", id),
		ProjectID: projectID,
		Name:      name,
		DataType:  dataType,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if dataType.SelectsOptions() {
		for _, opt := range options {
			color := opt.Color
			if color == "" {
				color = "GRAY" // real GitHub's default option color
			}
			f.Options = append(f.Options, &ProjectV2SingleSelectOption{
				ID:          s.nextOptionIDLocked(),
				Name:        opt.Name,
				Color:       color,
				Description: opt.Description,
			})
		}
	}
	if dataType == ProjectV2FieldIteration && iteration != nil {
		cfg := &ProjectV2IterationConfiguration{
			StartDate: iteration.StartDate,
			Duration:  iteration.Duration,
		}
		for _, it := range iteration.Iterations {
			cfg.Iterations = append(cfg.Iterations, &ProjectV2Iteration{
				ID:        s.nextOptionIDLocked(),
				Title:     it.Title,
				StartDate: it.StartDate,
				Duration:  it.Duration,
			})
		}
		f.Iteration = cfg
	}
	s.fields[id] = f
	s.FieldsByProj[projectID] = append(s.FieldsByProj[projectID], f)
	if s.Persist != nil {
		s.Persist.MustPut("project_v2_fields", strconv.Itoa(id), f)
	}
	return f
}

// nextPositionLocked returns the position that places a new item at the end of
// its project. Callers must hold s.Mu.
func (s *ProjectV2Store) nextPositionLocked(projectID int) int {
	next := 0
	for _, it := range s.items {
		if it.ProjectID == projectID && it.Position >= next {
			next = it.Position + 1
		}
	}
	return next
}

// nextOptionIDLocked mints the next 8-char hex ID shared by
// single-select options and iterations. Callers must hold s.Mu.
func (s *ProjectV2Store) nextOptionIDLocked() string {
	id := fmt.Sprintf("%08x", s.nextOptionSeed)
	s.nextOptionSeed++
	return id
}

// GetField returns the field by id.
func (s *ProjectV2Store) GetField(id int) *ProjectV2Field {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return cloneProjectV2Field(s.fields[id])
}

// LookupFieldByNodeID returns the field with the given GraphQL node id.
func (s *ProjectV2Store) LookupFieldByNodeID(nodeID string) *ProjectV2Field {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "PVTF_kgDO"); ok {
		if f := s.fields[id]; f != nil && f.NodeID == nodeID {
			return cloneProjectV2Field(f)
		}
	}
	for _, f := range s.fields {
		if f.NodeID == nodeID {
			return cloneProjectV2Field(f)
		}
	}
	return nil
}

// FieldsForProject returns every field defined on the project.
func (s *ProjectV2Store) FieldsForProject(projectID int) []*ProjectV2Field {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	out := make([]*ProjectV2Field, 0, len(s.FieldsByProj[projectID]))
	for _, f := range s.FieldsByProj[projectID] {
		out = append(out, cloneProjectV2Field(f))
	}
	return out
}

// FieldByNameOnProject returns the named field on the project, or nil.
func (s *ProjectV2Store) FieldByNameOnProject(projectID int, name string) *ProjectV2Field {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	for _, f := range s.FieldsByProj[projectID] {
		if f.Name == name {
			return cloneProjectV2Field(f)
		}
	}
	return nil
}

// SetFieldValue writes a value for (item, field). For SINGLE_SELECT, optionID
// must match one of the field's options; for TEXT/NUMBER it is ignored.
func (s *ProjectV2Store) SetFieldValue(itemID, fieldID int, optionID, textValue string, numberValue float64) (*ProjectV2ItemFieldValue, error) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	item, ok := s.items[itemID]
	if !ok {
		return nil, fmt.Errorf("item %d not found", itemID)
	}
	field, ok := s.fields[fieldID]
	if !ok {
		return nil, fmt.Errorf("field %d not found", fieldID)
	}
	if field.ProjectID != item.ProjectID {
		return nil, fmt.Errorf("field %d belongs to a different project than item %d", fieldID, itemID)
	}
	val := &ProjectV2ItemFieldValue{FieldID: fieldID}
	switch field.DataType {
	case ProjectV2FieldSingleSelect:
		if optionID == "" {
			return nil, fmt.Errorf("optionId is required for SINGLE_SELECT field %q", field.Name)
		}
		var match *ProjectV2SingleSelectOption
		for _, opt := range field.Options {
			if opt.ID == optionID {
				match = opt
				break
			}
		}
		if match == nil {
			return nil, fmt.Errorf("option %q not found on field %q", optionID, field.Name)
		}
		val.OptionID = match.ID
		val.OptionName = match.Name
	case ProjectV2FieldText:
		val.TextValue = textValue
	case ProjectV2FieldNumber:
		val.NumberValue = numberValue
	default:
		return nil, fmt.Errorf("unsupported field data type %q", field.DataType)
	}
	if item.FieldValues == nil {
		item.FieldValues = map[int]*ProjectV2ItemFieldValue{}
	}
	item.FieldValues[fieldID] = val
	if s.Persist != nil {
		s.Persist.MustPut("project_v2_items", strconv.Itoa(itemID), item)
	}
	return val, nil
}

// ListProjectsForOwner returns all projects owned by a user or organization.
func (s *ProjectV2Store) ListProjectsForOwner(ownerID int, ownerType string) []*ProjectV2 {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	out := make([]*ProjectV2, 0)
	for _, p := range s.projects {
		if p.OwnerID == ownerID && p.OwnerType == ownerType {
			out = append(out, cloneProjectV2(p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// UpdateProject patches a project's title/closed/public fields.
func (s *ProjectV2Store) UpdateProject(id int, title *string, closed, public *bool) *ProjectV2 {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	p := s.projects[id]
	if p == nil {
		return nil
	}
	if title != nil {
		p.Title = *title
	}
	if closed != nil {
		if *closed && !p.Closed {
			now := s.CurrentTime()
			p.ClosedAt = &now
		}
		if !*closed {
			p.ClosedAt = nil
		}
		p.Closed = *closed
	}
	if public != nil {
		p.Public = *public
	}
	p.UpdatedAt = s.CurrentTime()
	if s.Persist != nil {
		s.Persist.MustPut("projects_v2", strconv.Itoa(id), p)
	}
	return p
}

// DeleteProject removes a project and every entity it owns: fields, items,
// views, status updates and workflows.
func (s *ProjectV2Store) DeleteProject(id int) bool {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if s.projects[id] == nil {
		return false
	}
	// Delete the project with every field, item and view it owns in one
	// transaction, so a crash can't orphan them (STORE-001/002).
	batch := NewPersistBatch(s.Persist)
	delete(s.projects, id)
	for fid := range s.fields {
		if s.fields[fid].ProjectID == id {
			delete(s.fields, fid)
			batch.Delete("project_v2_fields", strconv.Itoa(fid))
		}
	}
	delete(s.FieldsByProj, id)
	for iid, it := range s.items {
		if it.ProjectID == id {
			delete(s.items, iid)
			s.unindexItemLocked(it)
			batch.Delete("project_v2_items", strconv.Itoa(iid))
		}
	}
	delete(s.viewsByProj, id)
	for vid := range s.views {
		if s.views[vid].ProjectID == id {
			delete(s.views, vid)
			batch.Delete("project_v2_views", strconv.Itoa(vid))
		}
	}
	// Status updates and workflows are also project-owned (SeedProjectDefaults
	// creates workflows for every project); cascade them too, or their in-memory
	// index entries leak and their durable rows reload orphaned to a nonexistent
	// project on restart.
	for sid, upd := range s.statusUpdates {
		if upd.ProjectID == id {
			delete(s.statusUpdates, sid)
			batch.Delete("project_v2_status_updates", strconv.Itoa(sid))
		}
	}
	delete(s.statusByProj, id)
	for wid, w := range s.workflows {
		if w.ProjectID == id {
			delete(s.workflows, wid)
			batch.Delete("project_v2_workflows", strconv.Itoa(wid))
		}
	}
	delete(s.workflowsByProj, id)
	batch.Delete("projects_v2", strconv.Itoa(id))
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "projects_v2", Err: err})
	}
	return true
}

// DeleteContentItems removes every item whose content is one of the supplied
// issue or PR database IDs.
func (s *ProjectV2Store) DeleteContentItems(contentType string, contentIDs map[int]bool) {
	s.DeleteContentItemsBatch(contentType, contentIDs, nil)
}

func (s *ProjectV2Store) DeleteContentItemsBatch(contentType string, contentIDs map[int]bool, batch *PersistBatch) {
	if len(contentIDs) == 0 {
		return
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()
	for id, it := range s.items {
		if it.ContentType != contentType || !contentIDs[it.ContentID] {
			continue
		}
		delete(s.items, id)
		s.unindexItemLocked(it)
		if batch != nil {
			batch.Delete("project_v2_items", strconv.Itoa(id))
		} else if s.Persist != nil {
			s.Persist.MustDelete("project_v2_items", strconv.Itoa(id))
		}
	}
}

// ListItemsForProject returns every item on a project.
func (s *ProjectV2Store) ListItemsForProject(projectID int) []*ProjectV2Item {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	out := make([]*ProjectV2Item, 0)
	for _, it := range s.items {
		if it.ProjectID == projectID {
			out = append(out, cloneProjectV2Item(it))
		}
	}
	// Position orders items; ID breaks ties for a stable insertion order.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// UpdateItem patches an item's draft title/body or field values.
func (s *ProjectV2Store) UpdateItem(id int, draftTitle, draftBody *string) *ProjectV2Item {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	it := s.items[id]
	if it == nil {
		return nil
	}
	if draftTitle != nil {
		it.DraftTitle = *draftTitle
	}
	if draftBody != nil {
		it.DraftBody = *draftBody
	}
	it.UpdatedAt = s.CurrentTime()
	if s.Persist != nil {
		s.Persist.MustPut("project_v2_items", strconv.Itoa(id), it)
	}
	return it
}

// SetFieldValueAny writes a REST field value, dispatching on data type:
// string for TEXT/DATE, float64 for NUMBER, option/iteration ID string for
// SINGLE_SELECT/ITERATION. A nil value clears the field.
func (s *ProjectV2Store) SetFieldValueAny(itemID, fieldID int, value interface{}) error {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	item, ok := s.items[itemID]
	if !ok {
		return fmt.Errorf("item %d not found", itemID)
	}
	field, ok := s.fields[fieldID]
	if !ok {
		return fmt.Errorf("field %d not found", fieldID)
	}
	if field.ProjectID != item.ProjectID {
		return fmt.Errorf("field %d belongs to a different project than item %d", fieldID, itemID)
	}
	if item.FieldValues == nil {
		item.FieldValues = map[int]*ProjectV2ItemFieldValue{}
	}
	if value == nil {
		delete(item.FieldValues, fieldID)
		item.UpdatedAt = s.CurrentTime()
		if s.Persist != nil {
			s.Persist.MustPut("project_v2_items", strconv.Itoa(itemID), item)
		}
		return nil
	}
	val := &ProjectV2ItemFieldValue{FieldID: fieldID}
	switch field.DataType {
	case ProjectV2FieldText:
		str, ok := value.(string)
		if !ok {
			return fmt.Errorf("field %q expects a string value", field.Name)
		}
		val.TextValue = str
	case ProjectV2FieldDate:
		str, ok := value.(string)
		if !ok {
			return fmt.Errorf("field %q expects a date string value", field.Name)
		}
		if _, err := time.Parse("2006-01-02", str); err != nil {
			return fmt.Errorf("field %q expects a YYYY-MM-DD date, got %q", field.Name, str)
		}
		val.DateValue = str
	case ProjectV2FieldNumber:
		num, ok := value.(float64)
		if !ok {
			return fmt.Errorf("field %q expects a number value", field.Name)
		}
		val.NumberValue = num
	case ProjectV2FieldSingleSelect:
		optionID, ok := value.(string)
		if !ok {
			return fmt.Errorf("field %q expects a single select option ID", field.Name)
		}
		var match *ProjectV2SingleSelectOption
		for _, opt := range field.Options {
			if opt.ID == optionID {
				match = opt
				break
			}
		}
		if match == nil {
			return fmt.Errorf("option %q not found on field %q", optionID, field.Name)
		}
		val.OptionID = match.ID
		val.OptionName = match.Name
	case ProjectV2FieldIteration:
		iterationID, ok := value.(string)
		if !ok {
			return fmt.Errorf("field %q expects an iteration ID", field.Name)
		}
		found := false
		if field.Iteration != nil {
			for _, it := range field.Iteration.Iterations {
				if it.ID == iterationID {
					found = true
					break
				}
			}
		}
		if !found {
			return fmt.Errorf("iteration %q not found on field %q", iterationID, field.Name)
		}
		val.IterationID = iterationID
	default:
		return fmt.Errorf("field %q of type %q cannot be set directly", field.Name, field.DataType)
	}
	item.FieldValues[fieldID] = val
	item.UpdatedAt = s.CurrentTime()
	if s.Persist != nil {
		s.Persist.MustPut("project_v2_items", strconv.Itoa(itemID), item)
	}
	return nil
}

// DeleteItem removes an item from a project.
func (s *ProjectV2Store) DeleteItem(id int) bool {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	it := s.items[id]
	if it == nil {
		return false
	}
	delete(s.items, id)
	s.unindexItemLocked(it)
	if s.Persist != nil {
		s.Persist.MustDelete("project_v2_items", strconv.Itoa(id))
	}
	return true
}

func (s *ProjectV2Store) unindexItemLocked(it *ProjectV2Item) {
	if it == nil || it.ContentID == 0 {
		return
	}
	owner := s.itemsByOwner[it.ContentID]
	kept := owner[:0]
	for _, x := range owner {
		if x.ID != it.ID {
			kept = append(kept, x)
		}
	}
	if len(kept) == 0 {
		delete(s.itemsByOwner, it.ContentID)
		return
	}
	s.itemsByOwner[it.ContentID] = kept
}

// UpdateField patches a field's name/options.
func (s *ProjectV2Store) UpdateField(id int, name *string, options []*ProjectV2SingleSelectOption) *ProjectV2Field {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	f := s.fields[id]
	if f == nil {
		return nil
	}
	if name != nil {
		f.Name = *name
	}
	if options != nil && f.DataType == ProjectV2FieldSingleSelect {
		// Keep the ID of any option matched by name; re-minting would dangle
		// items' stored OptionID for options that still exist. New names mint.
		existingIDByName := make(map[string]string, len(f.Options))
		for _, old := range f.Options {
			existingIDByName[old.Name] = old.ID
		}
		f.Options = nil
		for _, opt := range options {
			color := opt.Color
			if color == "" {
				color = "GRAY"
			}
			id := existingIDByName[opt.Name]
			if id == "" {
				id = s.nextOptionIDLocked()
			}
			f.Options = append(f.Options, &ProjectV2SingleSelectOption{
				ID:          id,
				Name:        opt.Name,
				Color:       color,
				Description: opt.Description,
			})
		}
	}
	f.UpdatedAt = s.CurrentTime()
	if s.Persist != nil {
		s.Persist.MustPut("project_v2_fields", strconv.Itoa(id), f)
	}
	return f
}

// DeleteField removes a field from a project.
func (s *ProjectV2Store) DeleteField(id int) bool {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	f := s.fields[id]
	if f == nil {
		return false
	}
	delete(s.fields, id)
	projFields := s.FieldsByProj[f.ProjectID]
	filtered := make([]*ProjectV2Field, 0, len(projFields))
	for _, x := range projFields {
		if x.ID != id {
			filtered = append(filtered, x)
		}
	}
	s.FieldsByProj[f.ProjectID] = filtered
	if s.Persist != nil {
		s.Persist.MustDelete("project_v2_fields", strconv.Itoa(id))
	}
	return true
}

// CreateView adds a view to a project.
func (s *ProjectV2Store) CreateView(projectID int, name, layout string, filter *string, visibleFields []int, creatorID int) *ProjectV2View {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if s.projects[projectID] == nil {
		return nil
	}
	id := s.nextViewID
	s.nextViewID++
	number := 1
	for _, v := range s.viewsByProj[projectID] {
		if v.Number >= number {
			number = v.Number + 1
		}
	}
	now := s.CurrentTime()
	v := &ProjectV2View{
		ID:            id,
		NodeID:        fmt.Sprintf("PVTV_kgDO%08d", id),
		ProjectID:     projectID,
		Number:        number,
		Name:          name,
		Layout:        layout,
		CreatorID:     creatorID,
		Filter:        filter,
		VisibleFields: visibleFields,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	s.views[id] = v
	s.viewsByProj[projectID] = append(s.viewsByProj[projectID], v)
	if s.Persist != nil {
		s.Persist.MustPut("project_v2_views", strconv.Itoa(id), v)
	}
	return v
}

// GetView returns a view by id.
func (s *ProjectV2Store) GetView(id int) *ProjectV2View {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return cloneProjectV2View(s.views[id])
}

// GetViewByNumber returns the project's view with the given per-project
// number, or nil.
func (s *ProjectV2Store) GetViewByNumber(projectID, number int) *ProjectV2View {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	for _, v := range s.viewsByProj[projectID] {
		if v.Number == number {
			return cloneProjectV2View(v)
		}
	}
	return nil
}

// ViewsForProject returns every view on a project.
func (s *ProjectV2Store) ViewsForProject(projectID int) []*ProjectV2View {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	out := make([]*ProjectV2View, 0, len(s.viewsByProj[projectID]))
	for _, v := range s.viewsByProj[projectID] {
		out = append(out, cloneProjectV2View(v))
	}
	return out
}

// GetProjectByOwnerNumber returns the owner's project with the given
// per-owner number, or nil.
func (s *ProjectV2Store) GetProjectByOwnerNumber(ownerID int, ownerType string, number int) *ProjectV2 {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	for _, p := range s.projects {
		if p.OwnerID == ownerID && p.OwnerType == ownerType && p.Number == number {
			return cloneProjectV2(p)
		}
	}
	return nil
}

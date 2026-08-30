package store

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

// Projects v2 — the administrative half of the store. Core create/read paths live in store_projects_v2.go.

// Snapshots
//
// STORE-021: every getter and List* below hands back a detached copy. The row types carry slices or maps,
// so a shallow struct copy is not enough — each has its own deep clone.

func cloneProjectV2(p *ProjectV2) *ProjectV2 {
	if p == nil {
		return nil
	}
	out := *p
	if p.ClosedAt != nil {
		closedAt := *p.ClosedAt
		out.ClosedAt = &closedAt
	}
	out.LinkedRepoIDs = append([]int(nil), p.LinkedRepoIDs...)
	out.LinkedTeamIDs = append([]int(nil), p.LinkedTeamIDs...)
	out.Collaborators = make([]*ProjectV2Collaborator, 0, len(p.Collaborators))
	for _, c := range p.Collaborators {
		if c == nil {
			continue
		}
		clone := *c
		out.Collaborators = append(out.Collaborators, &clone)
	}
	if len(out.Collaborators) == 0 {
		out.Collaborators = nil
	}
	return &out
}

func cloneProjectV2Item(it *ProjectV2Item) *ProjectV2Item {
	if it == nil {
		return nil
	}
	out := *it
	if it.ArchivedAt != nil {
		archivedAt := *it.ArchivedAt
		out.ArchivedAt = &archivedAt
	}
	if it.FieldValues != nil {
		out.FieldValues = make(map[int]*ProjectV2ItemFieldValue, len(it.FieldValues))
		for id, v := range it.FieldValues {
			out.FieldValues[id] = cloneProjectV2FieldValue(v)
		}
	}
	return &out
}

func cloneProjectV2FieldValue(v *ProjectV2ItemFieldValue) *ProjectV2ItemFieldValue {
	if v == nil {
		return nil
	}
	out := *v
	out.OptionIDs = append([]string(nil), v.OptionIDs...)
	out.OptionNames = append([]string(nil), v.OptionNames...)
	return &out
}

func cloneProjectV2Field(f *ProjectV2Field) *ProjectV2Field {
	if f == nil {
		return nil
	}
	out := *f
	out.Options = make([]*ProjectV2SingleSelectOption, 0, len(f.Options))
	for _, opt := range f.Options {
		if opt == nil {
			continue
		}
		clone := *opt
		out.Options = append(out.Options, &clone)
	}
	if len(out.Options) == 0 {
		out.Options = nil
	}
	if f.Iteration != nil {
		cfg := *f.Iteration
		cfg.Iterations = make([]*ProjectV2Iteration, 0, len(f.Iteration.Iterations))
		for _, it := range f.Iteration.Iterations {
			if it == nil {
				continue
			}
			clone := *it
			cfg.Iterations = append(cfg.Iterations, &clone)
		}
		out.Iteration = &cfg
	}
	return &out
}

func cloneProjectV2View(v *ProjectV2View) *ProjectV2View {
	if v == nil {
		return nil
	}
	out := *v
	if v.Filter != nil {
		filter := *v.Filter
		out.Filter = &filter
	}
	out.VisibleFields = append([]int(nil), v.VisibleFields...)
	out.GroupBy = append([]int(nil), v.GroupBy...)
	out.VerticalGroupBy = append([]int(nil), v.VerticalGroupBy...)
	out.SortBy = make([]*ProjectV2ViewSort, 0, len(v.SortBy))
	for _, s := range v.SortBy {
		if s == nil {
			continue
		}
		clone := *s
		out.SortBy = append(out.SortBy, &clone)
	}
	if len(out.SortBy) == 0 {
		out.SortBy = nil
	}
	return &out
}

// Project metadata

// ProjectV2Update is the patch updateProjectV2 applies. A nil member leaves the stored value alone (GraphQL's "not supplied" vs. "set to empty").
type ProjectV2Update struct {
	Title            *string
	ShortDescription *string
	Readme           *string
	Closed           *bool
	Public           *bool
}

// UpdateProjectDetails applies a patch and returns the updated snapshot, or nil when no such project exists.
func (s *ProjectV2Store) UpdateProjectDetails(id int, patch ProjectV2Update) *ProjectV2 {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	p := s.projects[id]
	if p == nil {
		return nil
	}
	if patch.Title != nil {
		p.Title = *patch.Title
	}
	if patch.ShortDescription != nil {
		p.ShortDescription = *patch.ShortDescription
	}
	if patch.Readme != nil {
		p.Readme = *patch.Readme
	}
	if patch.Closed != nil {
		if *patch.Closed && !p.Closed {
			now := s.CurrentTime()
			p.ClosedAt = &now
		}
		if !*patch.Closed {
			p.ClosedAt = nil
		}
		p.Closed = *patch.Closed
	}
	if patch.Public != nil {
		p.Public = *patch.Public
	}
	p.UpdatedAt = s.CurrentTime()
	s.persistProjectLocked(p)
	return cloneProjectV2(p)
}

// SetProjectTemplate marks or unmarks a project as a template.
func (s *ProjectV2Store) SetProjectTemplate(id int, template bool) *ProjectV2 {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	p := s.projects[id]
	if p == nil {
		return nil
	}
	p.Template = template
	p.UpdatedAt = s.CurrentTime()
	s.persistProjectLocked(p)
	return cloneProjectV2(p)
}

func (s *ProjectV2Store) persistProjectLocked(p *ProjectV2) {
	if s.Persist != nil {
		s.Persist.MustPut("projects_v2", strconv.Itoa(p.ID), p)
	}
}

// LinkRepository links a repository to a project. Linking one already linked is a no-op, returning the project either way.
func (s *ProjectV2Store) LinkRepository(projectID, repoID int) *ProjectV2 {
	return s.editIDList(projectID, repoID, true, func(p *ProjectV2) *[]int { return &p.LinkedRepoIDs })
}

// UnlinkRepository removes a repository link.
func (s *ProjectV2Store) UnlinkRepository(projectID, repoID int) *ProjectV2 {
	return s.editIDList(projectID, repoID, false, func(p *ProjectV2) *[]int { return &p.LinkedRepoIDs })
}

// LinkTeam links a team to a project.
func (s *ProjectV2Store) LinkTeam(projectID, teamID int) *ProjectV2 {
	return s.editIDList(projectID, teamID, true, func(p *ProjectV2) *[]int { return &p.LinkedTeamIDs })
}

// UnlinkTeam removes a team link.
func (s *ProjectV2Store) UnlinkTeam(projectID, teamID int) *ProjectV2 {
	return s.editIDList(projectID, teamID, false, func(p *ProjectV2) *[]int { return &p.LinkedTeamIDs })
}

// editIDList adds or removes one ID from a project's link list, which is a set: a repeated add does not duplicate, and removing an absent one is not an error.
func (s *ProjectV2Store) editIDList(projectID, id int, add bool, list func(*ProjectV2) *[]int) *ProjectV2 {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	p := s.projects[projectID]
	if p == nil {
		return nil
	}
	target := list(p)
	kept := make([]int, 0, len(*target)+1)
	present := false
	for _, existing := range *target {
		if existing == id {
			present = true
			if !add {
				continue
			}
		}
		kept = append(kept, existing)
	}
	if add && !present {
		kept = append(kept, id)
	}
	if len(kept) == 0 {
		kept = nil
	}
	*target = kept
	p.UpdatedAt = s.CurrentTime()
	s.persistProjectLocked(p)
	return cloneProjectV2(p)
}

// UpdateCollaborators applies role grants. Role "NONE" revokes the grant (how GitHub spells revocation on this mutation).
func (s *ProjectV2Store) UpdateCollaborators(projectID int, grants []*ProjectV2Collaborator) *ProjectV2 {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	p := s.projects[projectID]
	if p == nil {
		return nil
	}
	for _, grant := range grants {
		if grant == nil {
			continue
		}
		replaced := false
		kept := p.Collaborators[:0]
		for _, existing := range p.Collaborators {
			same := existing.UserID == grant.UserID && existing.TeamID == grant.TeamID
			if !same {
				kept = append(kept, existing)
				continue
			}
			if grant.Role == "NONE" {
				continue
			}
			existing.Role = grant.Role
			replaced = true
			kept = append(kept, existing)
		}
		p.Collaborators = kept
		if !replaced && grant.Role != "NONE" {
			clone := *grant
			p.Collaborators = append(p.Collaborators, &clone)
		}
	}
	p.UpdatedAt = s.CurrentTime()
	s.persistProjectLocked(p)
	return cloneProjectV2(p)
}

// CollaboratorRole returns the role explicitly granted to a user, or "".
func (s *ProjectV2Store) CollaboratorRole(projectID, userID int) string {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	p := s.projects[projectID]
	if p == nil {
		return ""
	}
	for _, c := range p.Collaborators {
		if c.UserID == userID && c.TeamID == 0 {
			return c.Role
		}
	}
	return ""
}

// CopyProject duplicates a project under a (possibly different) owner. Fields and views are always copied; items only when includeDraftIssues asks for it.
func (s *ProjectV2Store) CopyProject(sourceID, ownerID int, ownerType, title string, includeDraftIssues bool, creatorID int) *ProjectV2 {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	source := s.projects[sourceID]
	if source == nil {
		return nil
	}
	batch := NewPersistBatch(s.Persist)

	copied := s.createProjectLocked(ownerID, ownerType, title, creatorID, batch)
	copied.ShortDescription = source.ShortDescription
	copied.Readme = source.Readme
	copied.Public = source.Public
	batch.Put("projects_v2", strconv.Itoa(copied.ID), copied)

	// Field and option IDs change on clone, so item values must be remapped through these maps or they would dangle.
	fieldIDMap := map[int]int{}
	optionIDMap := map[string]string{}
	for _, f := range s.FieldsByProj[sourceID] {
		clone := cloneProjectV2Field(f)
		clone.ID = s.nextFieldID
		s.nextFieldID++
		clone.NodeID = fmt.Sprintf("PVTF_kgDO%08d", clone.ID)
		clone.ProjectID = copied.ID
		for _, opt := range clone.Options {
			fresh := s.nextOptionIDLocked()
			optionIDMap[fmt.Sprintf("%d:%s", f.ID, opt.ID)] = fresh
			opt.ID = fresh
		}
		if clone.Iteration != nil {
			for _, it := range clone.Iteration.Iterations {
				fresh := s.nextOptionIDLocked()
				optionIDMap[fmt.Sprintf("%d:%s", f.ID, it.ID)] = fresh
				it.ID = fresh
			}
		}
		fieldIDMap[f.ID] = clone.ID
		s.fields[clone.ID] = clone
		s.FieldsByProj[copied.ID] = append(s.FieldsByProj[copied.ID], clone)
		batch.Put("project_v2_fields", strconv.Itoa(clone.ID), clone)
	}

	for _, v := range s.viewsByProj[sourceID] {
		clone := cloneProjectV2View(v)
		clone.ID = s.nextViewID
		s.nextViewID++
		clone.NodeID = fmt.Sprintf("PVTV_kgDO%08d", clone.ID)
		clone.ProjectID = copied.ID
		clone.VisibleFields = remapIDs(clone.VisibleFields, fieldIDMap)
		clone.GroupBy = remapIDs(clone.GroupBy, fieldIDMap)
		clone.VerticalGroupBy = remapIDs(clone.VerticalGroupBy, fieldIDMap)
		for _, sortBy := range clone.SortBy {
			sortBy.FieldID = fieldIDMap[sortBy.FieldID]
		}
		s.views[clone.ID] = clone
		s.viewsByProj[copied.ID] = append(s.viewsByProj[copied.ID], clone)
		batch.Put("project_v2_views", strconv.Itoa(clone.ID), clone)
	}

	for _, it := range s.itemsForProjectLocked(sourceID) {
		if it.ContentType == "DraftIssue" && !includeDraftIssues {
			continue
		}
		clone := cloneProjectV2Item(it)
		clone.ID = s.nextItemID
		s.nextItemID++
		clone.NodeID = fmt.Sprintf("PVTI_kgDO%08d", clone.ID)
		clone.ProjectID = copied.ID
		remapped := map[int]*ProjectV2ItemFieldValue{}
		for fieldID, value := range clone.FieldValues {
			newFieldID, ok := fieldIDMap[fieldID]
			if !ok {
				continue
			}
			value.FieldID = newFieldID
			if value.OptionID != "" {
				value.OptionID = optionIDMap[fmt.Sprintf("%d:%s", fieldID, value.OptionID)]
			}
			if value.IterationID != "" {
				value.IterationID = optionIDMap[fmt.Sprintf("%d:%s", fieldID, value.IterationID)]
			}
			for i, optionID := range value.OptionIDs {
				value.OptionIDs[i] = optionIDMap[fmt.Sprintf("%d:%s", fieldID, optionID)]
			}
			remapped[newFieldID] = value
		}
		clone.FieldValues = remapped
		s.items[clone.ID] = clone
		if clone.ContentID != 0 {
			s.itemsByOwner[clone.ContentID] = append(s.itemsByOwner[clone.ContentID], clone)
		}
		batch.Put("project_v2_items", strconv.Itoa(clone.ID), clone)
	}

	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "projects_v2", Err: err})
	}
	return cloneProjectV2(copied)
}

func remapIDs(in []int, mapping map[int]int) []int {
	if in == nil {
		return nil
	}
	out := make([]int, 0, len(in))
	for _, id := range in {
		if mapped, ok := mapping[id]; ok {
			out = append(out, mapped)
		}
	}
	return out
}

// createProjectLocked mints a project row, staging it into batch so a larger transaction (CopyProject) commits it with everything else. Callers hold s.Mu.
func (s *ProjectV2Store) createProjectLocked(ownerID int, ownerType, title string, creatorID int, batch *PersistBatch) *ProjectV2 {
	id := s.nextProjectID
	s.nextProjectID++
	number := 1
	for _, p := range s.projects {
		if p.OwnerID == ownerID && p.OwnerType == ownerType && p.Number >= number {
			number = p.Number + 1
		}
	}
	now := s.CurrentTime()
	p := &ProjectV2{
		ID:        id,
		NodeID:    fmt.Sprintf("PVT_kgDO%08d", id),
		Number:    number,
		OwnerID:   ownerID,
		OwnerType: ownerType,
		CreatorID: creatorID,
		Title:     title,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.projects[id] = p
	if batch != nil {
		batch.Put("projects_v2", strconv.Itoa(id), p)
	}
	return p
}

func (s *ProjectV2Store) itemsForProjectLocked(projectID int) []*ProjectV2Item {
	out := make([]*ProjectV2Item, 0)
	for _, it := range s.items {
		if it.ProjectID == projectID {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Item lifecycle

// ArchiveItem archives or unarchives a project item, returning the updated snapshot. Re-archiving keeps the original ArchivedAt timestamp.
func (s *ProjectV2Store) ArchiveItem(id int, archived bool) *ProjectV2Item {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	it := s.items[id]
	if it == nil {
		return nil
	}
	switch {
	case archived && it.ArchivedAt == nil:
		now := s.CurrentTime()
		it.ArchivedAt = &now
	case !archived:
		it.ArchivedAt = nil
	}
	it.UpdatedAt = s.CurrentTime()
	s.persistItemLocked(it)
	return cloneProjectV2Item(it)
}

func (s *ProjectV2Store) persistItemLocked(it *ProjectV2Item) {
	if s.Persist != nil {
		s.Persist.MustPut("project_v2_items", strconv.Itoa(it.ID), it)
	}
}

// MoveItem places an item directly after afterID within its project, or at the head when afterID is 0. Positions are renumbered densely to stay total.
func (s *ProjectV2Store) MoveItem(id, afterID int) (*ProjectV2Item, error) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	moving := s.items[id]
	if moving == nil {
		return nil, fmt.Errorf("item %d not found", id)
	}
	if afterID == id {
		return nil, fmt.Errorf("an item cannot be positioned after itself")
	}
	var anchor *ProjectV2Item
	if afterID != 0 {
		anchor = s.items[afterID]
		if anchor == nil || anchor.ProjectID != moving.ProjectID {
			return nil, fmt.Errorf("item %d is not in the same project", afterID)
		}
	}

	ordered := s.itemsForProjectLocked(moving.ProjectID)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Position < ordered[j].Position })
	reordered := make([]*ProjectV2Item, 0, len(ordered))
	for _, it := range ordered {
		if it.ID == id {
			continue
		}
		if anchor == nil && len(reordered) == 0 {
			reordered = append(reordered, moving)
		}
		reordered = append(reordered, it)
		if anchor != nil && it.ID == anchor.ID {
			reordered = append(reordered, moving)
		}
	}
	if len(reordered) == 0 {
		reordered = append(reordered, moving)
	}

	batch := NewPersistBatch(s.Persist)
	now := s.CurrentTime()
	for position, it := range reordered {
		if it.Position == position {
			continue
		}
		it.Position = position
		it.UpdatedAt = now
		batch.Put("project_v2_items", strconv.Itoa(it.ID), it)
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "project_v2_items", Err: err})
	}
	return cloneProjectV2Item(moving), nil
}

// ConvertDraftToIssue repoints a draft item at a real issue, clearing the draft title and body so the two cannot drift.
func (s *ProjectV2Store) ConvertDraftToIssue(itemID, issueID int) (*ProjectV2Item, error) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	it := s.items[itemID]
	if it == nil {
		return nil, fmt.Errorf("item %d not found", itemID)
	}
	if it.ContentType != "DraftIssue" {
		return nil, fmt.Errorf("item %d is not a draft issue", itemID)
	}
	it.ContentType = "Issue"
	it.ContentID = issueID
	it.DraftTitle = ""
	it.DraftBody = ""
	it.UpdatedAt = s.CurrentTime()
	s.itemsByOwner[issueID] = append(s.itemsByOwner[issueID], it)
	s.persistItemLocked(it)
	return cloneProjectV2Item(it), nil
}

// ClearFieldValue removes an item's value for one field. Clearing an unset field is not an error.
func (s *ProjectV2Store) ClearFieldValue(itemID, fieldID int) (*ProjectV2Item, error) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	it := s.items[itemID]
	if it == nil {
		return nil, fmt.Errorf("item %d not found", itemID)
	}
	field := s.fields[fieldID]
	if field == nil {
		return nil, fmt.Errorf("field %d not found", fieldID)
	}
	if field.ProjectID != it.ProjectID {
		return nil, fmt.Errorf("field %d belongs to a different project than item %d", fieldID, itemID)
	}
	delete(it.FieldValues, fieldID)
	it.UpdatedAt = s.CurrentTime()
	s.persistItemLocked(it)
	return cloneProjectV2Item(it), nil
}

// SetMultiSelectValue writes a MULTI_SELECT value, validating every option ID against the field's options.
func (s *ProjectV2Store) SetMultiSelectValue(itemID, fieldID int, optionIDs []string) error {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	it := s.items[itemID]
	if it == nil {
		return fmt.Errorf("item %d not found", itemID)
	}
	field := s.fields[fieldID]
	if field == nil {
		return fmt.Errorf("field %d not found", fieldID)
	}
	if field.ProjectID != it.ProjectID {
		return fmt.Errorf("field %d belongs to a different project than item %d", fieldID, itemID)
	}
	if field.DataType != ProjectV2FieldMultiSelect {
		return fmt.Errorf("field %q is not a multi select field", field.Name)
	}
	names := make([]string, 0, len(optionIDs))
	for _, optionID := range optionIDs {
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
		names = append(names, match.Name)
	}
	if it.FieldValues == nil {
		it.FieldValues = map[int]*ProjectV2ItemFieldValue{}
	}
	it.FieldValues[fieldID] = &ProjectV2ItemFieldValue{
		FieldID:     fieldID,
		OptionIDs:   append([]string(nil), optionIDs...),
		OptionNames: names,
	}
	it.UpdatedAt = s.CurrentTime()
	s.persistItemLocked(it)
	return nil
}

// Status updates

// CreateStatusUpdate posts a status update on a project.
func (s *ProjectV2Store) CreateStatusUpdate(projectID, creatorID int, body, status, startDate, targetDate string) *ProjectV2StatusUpdate {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if s.projects[projectID] == nil {
		return nil
	}
	id := s.nextStatusID
	s.nextStatusID++
	now := s.CurrentTime()
	update := &ProjectV2StatusUpdate{
		ID:         id,
		NodeID:     fmt.Sprintf("PVTSU_kgDO%08d", id),
		ProjectID:  projectID,
		CreatorID:  creatorID,
		Body:       body,
		Status:     status,
		StartDate:  startDate,
		TargetDate: targetDate,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	s.statusUpdates[id] = update
	s.statusByProj[projectID] = append(s.statusByProj[projectID], update)
	if s.Persist != nil {
		s.Persist.MustPut("project_v2_status_updates", strconv.Itoa(id), update)
	}
	clone := *update
	return &clone
}

// ProjectV2StatusUpdatePatch is the patch updateProjectV2StatusUpdate applies.
type ProjectV2StatusUpdatePatch struct {
	Body       *string
	Status     *string
	StartDate  *string
	TargetDate *string
}

// UpdateStatusUpdate patches a status update.
func (s *ProjectV2Store) UpdateStatusUpdate(id int, patch ProjectV2StatusUpdatePatch) *ProjectV2StatusUpdate {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	update := s.statusUpdates[id]
	if update == nil {
		return nil
	}
	if patch.Body != nil {
		update.Body = *patch.Body
	}
	if patch.Status != nil {
		update.Status = *patch.Status
	}
	if patch.StartDate != nil {
		update.StartDate = *patch.StartDate
	}
	if patch.TargetDate != nil {
		update.TargetDate = *patch.TargetDate
	}
	update.UpdatedAt = s.CurrentTime()
	if s.Persist != nil {
		s.Persist.MustPut("project_v2_status_updates", strconv.Itoa(id), update)
	}
	clone := *update
	return &clone
}

// DeleteStatusUpdate removes a status update, returning the deleted snapshot.
func (s *ProjectV2Store) DeleteStatusUpdate(id int) *ProjectV2StatusUpdate {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	update := s.statusUpdates[id]
	if update == nil {
		return nil
	}
	delete(s.statusUpdates, id)
	kept := make([]*ProjectV2StatusUpdate, 0, len(s.statusByProj[update.ProjectID]))
	for _, existing := range s.statusByProj[update.ProjectID] {
		if existing.ID != id {
			kept = append(kept, existing)
		}
	}
	s.statusByProj[update.ProjectID] = kept
	if s.Persist != nil {
		s.Persist.MustDelete("project_v2_status_updates", strconv.Itoa(id))
	}
	clone := *update
	return &clone
}

// GetStatusUpdate returns one status update by database ID.
func (s *ProjectV2Store) GetStatusUpdate(id int) *ProjectV2StatusUpdate {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	update := s.statusUpdates[id]
	if update == nil {
		return nil
	}
	clone := *update
	return &clone
}

// LookupStatusUpdateByNodeID returns the status update with the given node id.
func (s *ProjectV2Store) LookupStatusUpdateByNodeID(nodeID string) *ProjectV2StatusUpdate {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "PVTSU_kgDO"); ok {
		if update := s.statusUpdates[id]; update != nil && update.NodeID == nodeID {
			clone := *update
			return &clone
		}
	}
	return nil
}

// StatusUpdatesForProject returns a project's status updates, newest first.
func (s *ProjectV2Store) StatusUpdatesForProject(projectID int) []*ProjectV2StatusUpdate {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	out := snapshotSlice(s.statusByProj[projectID])
	if out == nil {
		out = []*ProjectV2StatusUpdate{}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// Workflows

// CreateWorkflow records an automation rule on a project. GitHub has no create-workflow mutation, so this is the seam the UI and seeded defaults use.
func (s *ProjectV2Store) CreateWorkflow(projectID int, name string, enabled bool) *ProjectV2Workflow {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if s.projects[projectID] == nil {
		return nil
	}
	id := s.nextWorkflowID
	s.nextWorkflowID++
	number := 1
	for _, w := range s.workflowsByProj[projectID] {
		if w.Number >= number {
			number = w.Number + 1
		}
	}
	now := s.CurrentTime()
	w := &ProjectV2Workflow{
		ID:        id,
		NodeID:    fmt.Sprintf("PVTW_kgDO%08d", id),
		ProjectID: projectID,
		Number:    number,
		Name:      name,
		Enabled:   enabled,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.workflows[id] = w
	s.workflowsByProj[projectID] = append(s.workflowsByProj[projectID], w)
	if s.Persist != nil {
		s.Persist.MustPut("project_v2_workflows", strconv.Itoa(id), w)
	}
	clone := *w
	return &clone
}

// DeleteWorkflow removes an automation rule, returning the deleted snapshot.
func (s *ProjectV2Store) DeleteWorkflow(id int) *ProjectV2Workflow {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	w := s.workflows[id]
	if w == nil {
		return nil
	}
	delete(s.workflows, id)
	kept := make([]*ProjectV2Workflow, 0, len(s.workflowsByProj[w.ProjectID]))
	for _, existing := range s.workflowsByProj[w.ProjectID] {
		if existing.ID != id {
			kept = append(kept, existing)
		}
	}
	s.workflowsByProj[w.ProjectID] = kept
	if s.Persist != nil {
		s.Persist.MustDelete("project_v2_workflows", strconv.Itoa(id))
	}
	clone := *w
	return &clone
}

// GetWorkflow returns one workflow by database ID.
func (s *ProjectV2Store) GetWorkflow(id int) *ProjectV2Workflow {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	w := s.workflows[id]
	if w == nil {
		return nil
	}
	clone := *w
	return &clone
}

// LookupWorkflowByNodeID returns the workflow with the given node id.
func (s *ProjectV2Store) LookupWorkflowByNodeID(nodeID string) *ProjectV2Workflow {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "PVTW_kgDO"); ok {
		if w := s.workflows[id]; w != nil && w.NodeID == nodeID {
			clone := *w
			return &clone
		}
	}
	return nil
}

// WorkflowsForProject returns a project's automation rules by number.
func (s *ProjectV2Store) WorkflowsForProject(projectID int) []*ProjectV2Workflow {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	out := snapshotSlice(s.workflowsByProj[projectID])
	if out == nil {
		out = []*ProjectV2Workflow{}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// GetWorkflowByNumber returns the project's workflow with the given per-project number.
func (s *ProjectV2Store) GetWorkflowByNumber(projectID, number int) *ProjectV2Workflow {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	for _, w := range s.workflowsByProj[projectID] {
		if w.Number == number {
			clone := *w
			return &clone
		}
	}
	return nil
}

// Views

// ProjectV2ViewUpdate is the patch updateProjectV2View applies.
type ProjectV2ViewUpdate struct {
	Name            *string
	Layout          *string
	Filter          *string
	VisibleFields   []int
	GroupBy         []int
	VerticalGroupBy []int
	SortBy          []*ProjectV2ViewSort
}

// UpdateView patches a view's name, layout and configuration.
func (s *ProjectV2Store) UpdateView(id int, patch ProjectV2ViewUpdate) *ProjectV2View {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	v := s.views[id]
	if v == nil {
		return nil
	}
	if patch.Name != nil {
		v.Name = *patch.Name
	}
	if patch.Layout != nil {
		v.Layout = *patch.Layout
	}
	if patch.Filter != nil {
		filter := *patch.Filter
		v.Filter = &filter
	}
	if patch.VisibleFields != nil {
		v.VisibleFields = append([]int(nil), patch.VisibleFields...)
	}
	if patch.GroupBy != nil {
		v.GroupBy = append([]int(nil), patch.GroupBy...)
	}
	if patch.VerticalGroupBy != nil {
		v.VerticalGroupBy = append([]int(nil), patch.VerticalGroupBy...)
	}
	if patch.SortBy != nil {
		v.SortBy = nil
		for _, sortBy := range patch.SortBy {
			clone := *sortBy
			v.SortBy = append(v.SortBy, &clone)
		}
	}
	v.UpdatedAt = s.CurrentTime()
	if s.Persist != nil {
		s.Persist.MustPut("project_v2_views", strconv.Itoa(id), v)
	}
	return cloneProjectV2View(v)
}

// DeleteView removes a view, returning the deleted snapshot.
func (s *ProjectV2Store) DeleteView(id int) *ProjectV2View {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	v := s.views[id]
	if v == nil {
		return nil
	}
	delete(s.views, id)
	kept := make([]*ProjectV2View, 0, len(s.viewsByProj[v.ProjectID]))
	for _, existing := range s.viewsByProj[v.ProjectID] {
		if existing.ID != id {
			kept = append(kept, existing)
		}
	}
	s.viewsByProj[v.ProjectID] = kept
	if s.Persist != nil {
		s.Persist.MustDelete("project_v2_views", strconv.Itoa(id))
	}
	return cloneProjectV2View(v)
}

// LookupViewByNodeID returns the view with the given node id.
func (s *ProjectV2Store) LookupViewByNodeID(nodeID string) *ProjectV2View {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, "PVTV_kgDO"); ok {
		if v := s.views[id]; v != nil && v.NodeID == nodeID {
			return cloneProjectV2View(v)
		}
	}
	return nil
}

// Fields

// ProjectV2FieldUpdate is the patch updateProjectV2Field applies.
type ProjectV2FieldUpdate struct {
	Name    *string
	Options []*ProjectV2SingleSelectOption // replaces the option list wholesale when non-nil
	// Iteration replaces an ITERATION field's schedule wholesale when non-nil.
	Iteration *ProjectV2IterationConfiguration
}

// UpdateFieldDetails patches a field's name and, for option-bearing data types, its options. Option IDs survive a rename-free edit so item values stay valid.
func (s *ProjectV2Store) UpdateFieldDetails(id int, patch ProjectV2FieldUpdate) *ProjectV2Field {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	f := s.fields[id]
	if f == nil {
		return nil
	}
	if patch.Name != nil {
		f.Name = *patch.Name
	}
	if patch.Options != nil && f.DataType.SelectsOptions() {
		existingIDByName := make(map[string]string, len(f.Options))
		for _, old := range f.Options {
			existingIDByName[old.Name] = old.ID
		}
		f.Options = nil
		for _, opt := range patch.Options {
			color := opt.Color
			if color == "" {
				color = "GRAY"
			}
			optionID := opt.ID
			if optionID == "" {
				optionID = existingIDByName[opt.Name]
			}
			if optionID == "" {
				optionID = s.nextOptionIDLocked()
			}
			f.Options = append(f.Options, &ProjectV2SingleSelectOption{
				ID:          optionID,
				Name:        opt.Name,
				Color:       color,
				Description: opt.Description,
			})
		}
	}
	if patch.Iteration != nil && f.DataType == ProjectV2FieldIteration {
		// Iterations carry no natural key in the input, so an existing iteration's ID survives by title match, keeping item values valid.
		existingIDByTitle := map[string]string{}
		if f.Iteration != nil {
			for _, old := range f.Iteration.Iterations {
				existingIDByTitle[old.Title] = old.ID
			}
		}
		cfg := &ProjectV2IterationConfiguration{
			StartDate: patch.Iteration.StartDate,
			Duration:  patch.Iteration.Duration,
		}
		for _, it := range patch.Iteration.Iterations {
			iterationID := it.ID
			if iterationID == "" {
				iterationID = existingIDByTitle[it.Title]
			}
			if iterationID == "" {
				iterationID = s.nextOptionIDLocked()
			}
			cfg.Iterations = append(cfg.Iterations, &ProjectV2Iteration{
				ID:        iterationID,
				Title:     it.Title,
				StartDate: it.StartDate,
				Duration:  it.Duration,
			})
		}
		f.Iteration = cfg
	}
	f.UpdatedAt = s.CurrentTime()
	if s.Persist != nil {
		s.Persist.MustPut("project_v2_fields", strconv.Itoa(id), f)
	}
	return cloneProjectV2Field(f)
}

// Seeded defaults
//
// A project created on github.com arrives with built-in fields, a default Status option set, one table view and three
// default workflows. bleephub seeds the same so a fresh project is not blank.

// ProjectV2DefaultStatusOptions is the Status option set GitHub seeds.
var ProjectV2DefaultStatusOptions = []*ProjectV2SingleSelectOption{
	{Name: "Todo", Color: "RED", Description: "This item hasn't been started"},
	{Name: "In Progress", Color: "YELLOW", Description: "This is actively being worked on"},
	{Name: "Done", Color: "GREEN", Description: "This has been completed"},
}

// projectV2BuiltInFields are the read-only non-custom columns every project carries; their values come from the underlying issue or pull request.
var projectV2BuiltInFields = []struct {
	Name     string
	DataType ProjectV2FieldDataType
}{
	{"Title", "TITLE"},
	{"Assignees", "ASSIGNEES"},
	{"Labels", "LABELS"},
	{"Linked pull requests", "LINKED_PULL_REQUESTS"},
	{"Milestone", "MILESTONE"},
	{"Repository", "REPOSITORY"},
	{"Reviewers", "REVIEWERS"},
}

// SeedProjectDefaults gives a fresh project the fields, view and workflows github.com creates it with, attributing the default view to creatorID. Called by CreateProject.
func (s *ProjectV2Store) SeedProjectDefaults(projectID, creatorID int) {
	for _, builtIn := range projectV2BuiltInFields {
		s.CreateField(projectID, builtIn.Name, builtIn.DataType, nil, nil)
	}
	s.CreateField(projectID, "Status", ProjectV2FieldSingleSelect, ProjectV2DefaultStatusOptions, nil)
	visible := make([]int, 0, 3)
	for _, f := range s.FieldsForProject(projectID) {
		switch f.Name {
		case "Title", "Assignees", "Status":
			visible = append(visible, f.ID)
		}
	}
	s.CreateView(projectID, "View 1", "table", nil, visible, creatorID)
	for _, name := range []string{"Item added to project", "Item reopened", "Item closed"} {
		s.CreateWorkflow(projectID, name, false)
	}
}

// TouchProject stamps a project's updatedAt. Content mutations move it on GitHub and views ordered by UPDATED_AT depend on it, so content write paths call this.
func (s *ProjectV2Store) TouchProject(id int) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	p := s.projects[id]
	if p == nil {
		return
	}
	p.UpdatedAt = s.CurrentTime()
	s.persistProjectLocked(p)
}

// ProjectV2StatusUpdateStatuses is GitHub's ProjectV2StatusUpdateStatus enum.
var ProjectV2StatusUpdateStatuses = []string{"INACTIVE", "ON_TRACK", "AT_RISK", "OFF_TRACK", "COMPLETE"}

// ParseProjectV2Date validates a YYYY-MM-DD date, returning it unchanged.
func ParseProjectV2Date(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if _, err := time.Parse("2006-01-02", value); err != nil {
		return "", fmt.Errorf("expected a YYYY-MM-DD date, got %q", value)
	}
	return value, nil
}

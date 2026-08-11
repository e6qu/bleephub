package store

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// ProjectClassic is a GitHub Projects classic (v1) project, scoped to a
// repository. It contains columns, which in turn contain cards.
type ProjectClassic struct {
	ID        int       `json:"id"`
	NodeID    string    `json:"node_id"`
	RepoKey   string    `json:"repo_key"`
	Name      string    `json:"name"`
	Body      string    `json:"body"`
	State     string    `json:"state"` // "open" or "closed"
	Number    int       `json:"number"`
	CreatorID int       `json:"creator_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ProjectColumn is a column inside a ProjectClassic.
type ProjectColumn struct {
	ID        int       `json:"id"`
	NodeID    string    `json:"node_id"`
	ProjectID int       `json:"project_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Position  int64     `json:"position"` // ordering within the project; persisted, not surfaced by the API mapper
}

// ProjectCard is a card inside a ProjectColumn. It is either a note card
// (Note non-empty, IssueID 0) or an issue card (IssueID set, Note empty).
type ProjectCard struct {
	ID        int       `json:"id"`
	NodeID    string    `json:"node_id"`
	ColumnID  int       `json:"column_id"`
	Note      string    `json:"note"`
	IssueID   int       `json:"issue_id"`
	CreatorID int       `json:"creator_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Position  int64     `json:"position"` // ordering within the column; persisted, not surfaced by the API mapper
}

const (
	projectClassicPositionStep int64 = 1 << 40
)

func projectClassicNodeID(id int) string {
	return "MDExOlByb2plY3Q" + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d", id)))
}

func projectColumnNodeID(id int) string {
	return "MDEzOlByb2plY3RDb2x1bW4" + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d", id)))
}

func projectCardNodeID(id int) string {
	return "MDE1OlByb2plY3RDYXJk" + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d", id)))
}

// CreateProjectClassic creates a new repo-scoped project.
func (st *Store) CreateProjectClassic(repo *Repo, creatorID int, name, body, state string) *ProjectClassic {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	if state == "" {
		state = "open"
	}
	now := st.CurrentTime()
	proj := &ProjectClassic{
		ID:        st.NextProjectClassicID,
		NodeID:    projectClassicNodeID(st.NextProjectClassicID),
		RepoKey:   repo.FullName,
		Name:      name,
		Body:      body,
		State:     state,
		Number:    st.NextProjectClassicID,
		CreatorID: creatorID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	st.ProjectClassic[proj.ID] = proj
	st.NextProjectClassicID++
	st.persistProjectClassic(proj)
	return proj
}

// GetProjectClassic returns a project by ID.
func (st *Store) GetProjectClassic(id int) *ProjectClassic {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	// A copy so a reader can't mutate the stored project through the getter
	// (STORE-021); ProjectClassic is all-value. Edits go through
	// UpdateProjectClassic, which re-fetches the live row by id.
	proj := st.ProjectClassic[id]
	if proj == nil {
		return nil
	}
	clone := *proj
	return &clone
}

// ListProjectClassicsForRepo returns all projects in a repo, newest first.
func (st *Store) ListProjectClassicsForRepo(repoKey string) []*ProjectClassic {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*ProjectClassic
	for _, p := range st.ProjectClassic {
		if p.RepoKey == repoKey {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return snapshotSlice(out)
}

// UpdateProjectClassic applies updates to a project.
// UpdateProjectClassic applies the given field updates. `proj` now comes from
// GetProjectClassic, which returns a detached clone, so the mutation is applied
// to the LIVE row (re-fetched by id) and a fresh snapshot is returned for the
// caller to render — never the live pointer (STORE-021).
func (st *Store) UpdateProjectClassic(proj *ProjectClassic, name, body, state *string) *ProjectClassic {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	live := st.ProjectClassic[proj.ID]
	if live == nil {
		return nil
	}
	if name != nil {
		live.Name = *name
	}
	if body != nil {
		live.Body = *body
	}
	if state != nil {
		live.State = *state
	}
	live.UpdatedAt = st.CurrentTime()
	st.persistProjectClassic(live)
	clone := *live
	return &clone
}

// DeleteProjectClassic deletes a project and all its columns and cards.
func (st *Store) DeleteProjectClassic(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	proj := st.ProjectClassic[id]
	if proj == nil {
		return false
	}
	// One transaction: the project and every column and card beneath it are
	// deleted together, so a crash can never orphan a column or card pointing at a
	// project that no longer exists (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	for _, col := range st.ProjectColumns {
		if col.ProjectID == id {
			for _, card := range st.ProjectCards {
				if card.ColumnID == col.ID {
					st.deleteProjectCardBatchLocked(batch, card.ID)
				}
			}
			st.deleteProjectColumnBatchLocked(batch, col.ID)
		}
	}
	delete(st.ProjectClassic, id)
	batch.Delete("projects_classic", strconv.Itoa(id))
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "projects_classic", Err: err})
	}
	return true
}

// CreateProjectColumn creates a column in a project, appending it last.
func (st *Store) CreateProjectColumn(projectID int, name string) *ProjectColumn {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	pos := projectClassicPositionStep
	for _, col := range st.ProjectColumns {
		if col.ProjectID == projectID && col.Position >= pos {
			pos = col.Position + projectClassicPositionStep
		}
	}

	now := st.CurrentTime()
	col := &ProjectColumn{
		ID:        st.NextProjectColumnID,
		NodeID:    projectColumnNodeID(st.NextProjectColumnID),
		ProjectID: projectID,
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
		Position:  pos,
	}
	st.ProjectColumns[col.ID] = col
	st.NextProjectColumnID++
	st.persistProjectColumn(col)
	return col
}

// GetProjectColumn returns a column by ID.
func (st *Store) GetProjectColumn(id int) *ProjectColumn {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	// A copy so a reader can't mutate the stored column through the getter
	// (STORE-021); ProjectColumn is all-value. The Update/Move writers re-fetch
	// the live row by id.
	col := st.ProjectColumns[id]
	if col == nil {
		return nil
	}
	clone := *col
	return &clone
}

// ListProjectColumns returns columns for a project in visual order.
func (st *Store) ListProjectColumns(projectID int) []*ProjectColumn {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*ProjectColumn
	for _, col := range st.ProjectColumns {
		if col.ProjectID == projectID {
			out = append(out, col)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return snapshotSlice(out)
}

// UpdateProjectColumn renames a column.
func (st *Store) UpdateProjectColumn(col *ProjectColumn, name string) *ProjectColumn {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	// `col` is a detached clone from GetProjectColumn; mutate the live row and
	// return a fresh snapshot (STORE-021).
	live := st.ProjectColumns[col.ID]
	if live == nil {
		return nil
	}
	live.Name = name
	live.UpdatedAt = st.CurrentTime()
	st.persistProjectColumn(live)
	clone := *live
	return &clone
}

// DeleteProjectColumn deletes a column and all its cards.
func (st *Store) DeleteProjectColumn(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	col := st.ProjectColumns[id]
	if col == nil {
		return false
	}
	// One transaction: the column and every card it holds are deleted together,
	// so a crash can never orphan a card pointing at a deleted column (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	for _, card := range st.ProjectCards {
		if card.ColumnID == id {
			st.deleteProjectCardBatchLocked(batch, card.ID)
		}
	}
	st.deleteProjectColumnBatchLocked(batch, id)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "project_columns", Err: err})
	}
	return true
}

// deleteProjectColumnBatchLocked stages a column-row removal into batch. Its
// cards are staged separately by the caller so the whole column (or project)
// deletion commits atomically (STORE-001/002). Callers hold st.Mu for writing.
func (st *Store) deleteProjectColumnBatchLocked(batch *PersistBatch, id int) {
	delete(st.ProjectColumns, id)
	batch.Delete("project_columns", strconv.Itoa(id))
}

// MoveProjectColumn repositions a column within its project.
func (st *Store) MoveProjectColumn(col *ProjectColumn, position string) error {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	// `col` is a detached clone from GetProjectColumn; operate on the live row
	// so the position change reaches the store (STORE-021).
	col = st.ProjectColumns[col.ID]
	if col == nil {
		return fmt.Errorf("column not found")
	}

	cols := make([]*ProjectColumn, 0)
	for _, c := range st.ProjectColumns {
		if c.ProjectID == col.ProjectID {
			cols = append(cols, c)
		}
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].Position < cols[j].Position })

	switch position {
	case "first":
		min := int64(0)
		for _, c := range cols {
			if c.ID != col.ID && (min == 0 || c.Position < min) {
				min = c.Position
			}
		}
		col.Position = min - projectClassicPositionStep
	case "last":
		max := int64(0)
		for _, c := range cols {
			if c.ID != col.ID && c.Position > max {
				max = c.Position
			}
		}
		col.Position = max + projectClassicPositionStep
	default:
		var afterID int
		if _, err := fmt.Sscanf(position, "after:%d", &afterID); err != nil {
			return fmt.Errorf("invalid position")
		}
		var afterPos, nextPos int64
		found := false
		for i, c := range cols {
			if c.ID == afterID {
				afterPos = c.Position
				found = true
				if i+1 < len(cols) {
					nextPos = cols[i+1].Position
				}
				break
			}
		}
		if !found {
			return fmt.Errorf("after card not found")
		}
		if nextPos == 0 {
			col.Position = afterPos + projectClassicPositionStep
		} else {
			col.Position = (afterPos + nextPos) / 2
		}
	}
	col.UpdatedAt = st.CurrentTime()
	st.persistProjectColumn(col)
	return nil
}

// CreateProjectCard creates a card in a column. Exactly one of note or
// issueID must be provided.
func (st *Store) CreateProjectCard(columnID, creatorID int, note string, issueID int) *ProjectCard {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	pos := projectClassicPositionStep
	for _, card := range st.ProjectCards {
		if card.ColumnID == columnID && card.Position >= pos {
			pos = card.Position + projectClassicPositionStep
		}
	}

	now := st.CurrentTime()
	card := &ProjectCard{
		ID:        st.NextProjectCardID,
		NodeID:    projectCardNodeID(st.NextProjectCardID),
		ColumnID:  columnID,
		Note:      note,
		IssueID:   issueID,
		CreatorID: creatorID,
		CreatedAt: now,
		UpdatedAt: now,
		Position:  pos,
	}
	st.ProjectCards[card.ID] = card
	st.NextProjectCardID++
	st.persistProjectCard(card)
	return card
}

// GetProjectCard returns a card by ID.
func (st *Store) GetProjectCard(id int) *ProjectCard {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	// A copy so a reader can't mutate the stored card through the getter
	// (STORE-021); ProjectCard is all-value. The Update/Move/Convert writers
	// re-fetch the live row by id.
	card := st.ProjectCards[id]
	if card == nil {
		return nil
	}
	clone := *card
	return &clone
}

// ListProjectCards returns cards in a column in visual order.
func (st *Store) ListProjectCards(columnID int) []*ProjectCard {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*ProjectCard
	for _, card := range st.ProjectCards {
		if card.ColumnID == columnID {
			out = append(out, card)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return snapshotSlice(out)
}

// UpdateProjectCard updates a card's note. Converting a note card to an
// issue card is not supported by PATCH; real GitHub uses a separate flow.
func (st *Store) UpdateProjectCard(card *ProjectCard, note string) *ProjectCard {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	// `card` is a detached clone from GetProjectCard; mutate the live row and
	// return a fresh snapshot (STORE-021).
	live := st.ProjectCards[card.ID]
	if live == nil {
		return nil
	}
	live.Note = note
	live.UpdatedAt = st.CurrentTime()
	st.persistProjectCard(live)
	clone := *live
	return &clone
}

// DeleteProjectCard deletes a card.
func (st *Store) DeleteProjectCard(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	return st.deleteProjectCardLocked(id)
}

func (st *Store) deleteProjectCardLocked(id int) bool {
	batch := NewPersistBatch(st.Persist)
	ok := st.deleteProjectCardBatchLocked(batch, id)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "project_cards", Err: err})
	}
	return ok
}

// deleteProjectCardBatchLocked stages a card removal into batch so a cascade
// (column or project deletion) can commit every card with its parent in one
// transaction (STORE-001/002). Callers hold st.Mu for writing.
func (st *Store) deleteProjectCardBatchLocked(batch *PersistBatch, id int) bool {
	if st.ProjectCards[id] == nil {
		return false
	}
	delete(st.ProjectCards, id)
	batch.Delete("project_cards", strconv.Itoa(id))
	return true
}

// MoveProjectCard moves a card to a column and/or a new position within it.
func (st *Store) MoveProjectCard(card *ProjectCard, targetColumnID int, position string) error {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	// `card` is a detached clone from GetProjectCard; operate on the live row so
	// the column/position change reaches the store (STORE-021).
	card = st.ProjectCards[card.ID]
	if card == nil {
		return fmt.Errorf("card not found")
	}

	if targetColumnID != 0 && targetColumnID != card.ColumnID {
		target := st.ProjectColumns[targetColumnID]
		if target == nil {
			return fmt.Errorf("target column not found")
		}
		// A card may only move between columns of the same classic project.
		// The handler authorizes projects:write on the card's own project/repo;
		// the destination column's project is never separately authorized, so
		// without this a caller could move a card into another (private)
		// project's column — cross-tenant board injection. GitHub answers 422.
		source := st.ProjectColumns[card.ColumnID]
		if source == nil || target.ProjectID != source.ProjectID {
			return fmt.Errorf("target column is in a different project")
		}
		card.ColumnID = targetColumnID
	}

	columnID := card.ColumnID
	cards := make([]*ProjectCard, 0)
	for _, c := range st.ProjectCards {
		if c.ColumnID == columnID && c.ID != card.ID {
			cards = append(cards, c)
		}
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].Position < cards[j].Position })

	switch position {
	case "first":
		min := int64(0)
		for _, c := range cards {
			if min == 0 || c.Position < min {
				min = c.Position
			}
		}
		card.Position = min - projectClassicPositionStep
	case "last":
		max := int64(0)
		for _, c := range cards {
			if c.Position > max {
				max = c.Position
			}
		}
		card.Position = max + projectClassicPositionStep
	default:
		var afterID int
		if _, err := fmt.Sscanf(position, "after:%d", &afterID); err != nil {
			return fmt.Errorf("invalid position")
		}
		var afterPos, nextPos int64
		found := false
		for i, c := range cards {
			if c.ID == afterID {
				afterPos = c.Position
				found = true
				if i+1 < len(cards) {
					nextPos = cards[i+1].Position
				}
				break
			}
		}
		if !found {
			return fmt.Errorf("after card not found")
		}
		if nextPos == 0 {
			card.Position = afterPos + projectClassicPositionStep
		} else {
			card.Position = (afterPos + nextPos) / 2
		}
	}
	card.UpdatedAt = st.CurrentTime()
	st.persistProjectCard(card)
	return nil
}

// ConvertProjectCardToIssue replaces a note card with an issue card in the
// same column/position, preserving the card ID.
func (st *Store) ConvertProjectCardToIssue(card *ProjectCard, issueID int) *ProjectCard {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	// `card` is a detached clone from GetProjectCard; mutate the live row and
	// return a fresh snapshot (STORE-021).
	live := st.ProjectCards[card.ID]
	if live == nil {
		return nil
	}
	live.Note = ""
	live.IssueID = issueID
	live.UpdatedAt = st.CurrentTime()
	st.persistProjectCard(live)
	clone := *live
	return &clone
}

func (st *Store) persistProjectClassic(p *ProjectClassic) {
	if st.Persist != nil {
		st.Persist.MustPut("projects_classic", strconv.Itoa(p.ID), p)
	}
}

func (st *Store) persistProjectColumn(c *ProjectColumn) {
	if st.Persist != nil {
		st.Persist.MustPut("project_columns", strconv.Itoa(c.ID), c)
	}
}

func (st *Store) persistProjectCard(c *ProjectCard) {
	if st.Persist != nil {
		st.Persist.MustPut("project_cards", strconv.Itoa(c.ID), c)
	}
}

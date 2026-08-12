package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestPersistenceReload_ProjectBoardOrder covers the STORE data-loss fix (P4).
// ProjectColumn.Position and ProjectCard.Position were tagged json:"-", so the
// board ordering was dropped on persist; after any restart every Position was
// 0 and ListProjectColumns/ListProjectCards returned the board in arbitrary
// map-iteration order. The columns and one card are reordered so Position no
// longer matches creation/ID order; the reloaded board must preserve that
// order, which is only possible if Position round-trips through persistence.
func TestPersistenceReload_ProjectBoardOrder(t *testing.T) {
	var projectID, colBID int
	var wantColOrder []int
	var wantCardOrder []int

	st2 := reloadedStore(t, func(p *store.Persistence, st *store.Store) {
		st.SeedDefaultUser()
		user := st.UsersByLogin["admin"]
		repo := st.CreateRepo(user, "board", "", false)

		proj := st.CreateProjectClassic(repo, user.ID, "roadmap", "", "open")
		projectID = proj.ID

		colA := st.CreateProjectColumn(proj.ID, "A")
		colB := st.CreateProjectColumn(proj.ID, "B")
		colC := st.CreateProjectColumn(proj.ID, "C")
		colBID = colB.ID

		// Reorder so Position order (C, A, B) differs from creation/ID order.
		if err := st.MoveProjectColumn(colC, "first"); err != nil {
			t.Fatalf("move column: %v", err)
		}
		wantColOrder = []int{colC.ID, colA.ID, colB.ID}

		card1 := st.CreateProjectCard(colB.ID, user.ID, "one", 0)
		card2 := st.CreateProjectCard(colB.ID, user.ID, "two", 0)
		card3 := st.CreateProjectCard(colB.ID, user.ID, "three", 0)
		// Reorder so Position order (three, one, two) differs from ID order.
		if err := st.MoveProjectCard(card3, colB.ID, "first"); err != nil {
			t.Fatalf("move card: %v", err)
		}
		wantCardOrder = []int{card3.ID, card1.ID, card2.ID}
	})

	cols := st2.ListProjectColumns(projectID)
	if len(cols) != len(wantColOrder) {
		t.Fatalf("columns after reload = %d, want %d", len(cols), len(wantColOrder))
	}
	for i, want := range wantColOrder {
		if cols[i].ID != want {
			t.Fatalf("column order after reload = %v, want %v (Position not persisted)",
				columnIDs(cols), wantColOrder)
		}
	}
	// Positions must be strictly increasing across the ordered board. If
	// Position were dropped on persist every value would be 0 and this would
	// fail, and ListProjectColumns' order would be arbitrary.
	for i := 1; i < len(cols); i++ {
		if cols[i-1].Position >= cols[i].Position {
			t.Fatalf("column positions not strictly increasing after reload: %d then %d — ordering lost",
				cols[i-1].Position, cols[i].Position)
		}
	}

	cards := st2.ListProjectCards(colBID)
	if len(cards) != len(wantCardOrder) {
		t.Fatalf("cards after reload = %d, want %d", len(cards), len(wantCardOrder))
	}
	for i, want := range wantCardOrder {
		if cards[i].ID != want {
			t.Fatalf("card order after reload = %v, want %v (Position not persisted)",
				cardIDs(cards), wantCardOrder)
		}
	}
	for i := 1; i < len(cards); i++ {
		if cards[i-1].Position >= cards[i].Position {
			t.Fatalf("card positions not strictly increasing after reload: %d then %d — ordering lost",
				cards[i-1].Position, cards[i].Position)
		}
	}
}

func columnIDs(cols []*store.ProjectColumn) []int {
	ids := make([]int, len(cols))
	for i, c := range cols {
		ids[i] = c.ID
	}
	return ids
}

func cardIDs(cards []*store.ProjectCard) []int {
	ids := make([]int, len(cards))
	for i, c := range cards {
		ids[i] = c.ID
	}
	return ids
}

package bleephub

import (
	"net/http"
	"testing"
)

// TestMoveProjectCardRefusesCrossProjectColumn pins the fix for a cross-tenant
// board-injection IDOR: handleMoveProjectCard authorizes projects:write only on
// the card's own project/repo, then passed the body column_id to the store after
// only an existence check. A caller could therefore move a card into a column of
// another (private) project. The move is now rejected unless the target column
// belongs to the same project as the card.
func TestMoveProjectCardRefusesCrossProjectColumn(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	st := srv.store
	admin := st.LookupUserByLogin("admin")

	// Attacker's own repo/project/column/card.
	attackerRepo := st.CreateRepo(admin, "cards-idor-attacker", "", false)
	attackerProject := st.CreateProjectClassic(attackerRepo, admin.ID, "attacker board", "", "open")
	attackerColumn := st.CreateProjectColumn(attackerProject.ID, "todo")
	card := st.CreateProjectCard(attackerColumn.ID, admin.ID, "attacker card", 0, 0)
	if card == nil {
		t.Fatal("create attacker card")
	}

	// Victim's separate project with its own column.
	victimRepo := st.CreateRepo(admin, "cards-idor-victim", "", false)
	victimProject := st.CreateProjectClassic(victimRepo, admin.ID, "victim board", "", "open")
	victimColumn := st.CreateProjectColumn(victimProject.ID, "victim column")

	resp := srv.post(t, "/api/v3/projects/columns/cards/"+itoa(card.ID)+"/moves", defaultToken,
		map[string]interface{}{"column_id": victimColumn.ID, "position": "last"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("cross-project card move = %d, want 422", resp.StatusCode)
	}

	// The card must not have been reassigned into the victim's column.
	if got := st.GetProjectCard(card.ID); got == nil || got.ColumnID != attackerColumn.ID {
		t.Fatalf("card moved across projects: ColumnID=%v, want %d", got.ColumnID, attackerColumn.ID)
	}
	// The victim's column must hold no injected card.
	for _, c := range st.ListProjectCards(victimColumn.ID) {
		if c.ID == card.ID {
			t.Fatal("attacker card was injected into the victim project's column")
		}
	}

	// Control: a move within the attacker's own project still works.
	attackerColumn2 := st.CreateProjectColumn(attackerProject.ID, "doing")
	resp = srv.post(t, "/api/v3/projects/columns/cards/"+itoa(card.ID)+"/moves", defaultToken,
		map[string]interface{}{"column_id": attackerColumn2.ID, "position": "last"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("same-project card move = %d, want 201", resp.StatusCode)
	}
	if got := st.GetProjectCard(card.ID); got == nil || got.ColumnID != attackerColumn2.ID {
		t.Fatalf("same-project move did not take effect: ColumnID=%v, want %d", got.ColumnID, attackerColumn2.ID)
	}
}

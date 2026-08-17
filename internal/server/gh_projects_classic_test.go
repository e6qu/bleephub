package bleephub

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// TestProjectClassicGetIsDetached pins STORE-021: GetProjectClassic returns a
// copy, and UpdateProjectClassic still reaches the live row.
func TestProjectClassicGetIsDetached(t *testing.T) {
	s := newTestServer()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "proj-detach", "", false)
	p := s.store.CreateProjectClassic(repo, admin.ID, "Board", "body", "open")

	got := s.store.GetProjectClassic(p.ID)
	got.Name = "hacked"
	if fresh := s.store.GetProjectClassic(p.ID); fresh.Name != "Board" {
		t.Fatalf("project mutated through the getter: %q", fresh.Name)
	}

	newName := "Renamed"
	updated := s.store.UpdateProjectClassic(got, &newName, nil, nil)
	if updated.Name != "Renamed" {
		t.Fatalf("update returned %q, want Renamed", updated.Name)
	}
	if live := s.store.GetProjectClassic(p.ID); live.Name != "Renamed" {
		t.Fatalf("live project after update = %q, want Renamed", live.Name)
	}
}

func TestProjectsClassic_ProjectCRUD(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	s.store.CreateRepo(admin, "proj-classic-crud", "", false)

	// Create
	resp := s.post(t, "/api/v3/repos/admin/proj-classic-crud/projects", defaultToken, map[string]any{"name": "Roadmap", "body": "Q3 plans"})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create project: %d %s", resp.StatusCode, b)
	}
	created := decodeJSON(t, resp)
	if created["name"] != "Roadmap" {
		t.Fatalf("expected name Roadmap, got %v", created["name"])
	}
	projID := int(created["id"].(float64))

	// List
	resp = s.get(t, "/api/v3/repos/admin/proj-classic-crud/projects", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list projects: %d %s", resp.StatusCode, b)
	}
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	resp.Body.Close()
	if len(list) != 1 {
		t.Fatalf("expected 1 project, got %d", len(list))
	}

	// Get
	resp = s.get(t, "/api/v3/projects/"+itoa(projID), defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("get project: %d %s", resp.StatusCode, b)
	}
	got := decodeJSON(t, resp)
	if got["body"] != "Q3 plans" {
		t.Fatalf("expected body Q3 plans, got %v", got["body"])
	}

	// Update
	resp = s.patch(t, "/api/v3/projects/"+itoa(projID), defaultToken, map[string]any{"name": "Roadmap 2", "state": "closed"})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("update project: %d %s", resp.StatusCode, b)
	}
	updated := decodeJSON(t, resp)
	if updated["name"] != "Roadmap 2" || updated["state"] != "closed" {
		t.Fatalf("unexpected updated project: %v", updated)
	}

	// Delete
	resp = s.delete(t, "/api/v3/projects/"+itoa(projID), defaultToken)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("delete project: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	// Get 404
	resp = s.get(t, "/api/v3/projects/"+itoa(projID), defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestProjectsClassic_ColumnCRUDAndMove(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "proj-classic-col", "", false)
	proj := s.store.CreateProjectClassic(repo, admin.ID, "Board", "", "open")

	// Create columns
	c1 := s.createColumn(t, proj.ID, "Todo")
	c2 := s.createColumn(t, proj.ID, "Done")

	// List
	resp := s.get(t, "/api/v3/projects/"+itoa(proj.ID)+"/columns", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list columns: %d %s", resp.StatusCode, b)
	}
	var cols []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&cols); err != nil {
		t.Fatalf("decode columns: %v", err)
	}
	resp.Body.Close()
	if len(cols) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(cols))
	}

	// Get column
	resp = s.get(t, "/api/v3/projects/columns/"+itoa(c1), defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("get column: %d %s", resp.StatusCode, b)
	}
	col := decodeJSON(t, resp)
	if col["name"] != "Todo" {
		t.Fatalf("expected Todo, got %v", col["name"])
	}

	// Update
	resp = s.patch(t, "/api/v3/projects/columns/"+itoa(c1), defaultToken, map[string]any{"name": "Backlog"})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("update column: %d %s", resp.StatusCode, b)
	}
	updated := decodeJSON(t, resp)
	if updated["name"] != "Backlog" {
		t.Fatalf("expected Backlog, got %v", updated["name"])
	}

	// Move c2 first
	moveResp := s.moveColumn(t, c2, "first")
	if moveResp["id"] == nil {
		t.Fatalf("move column failed: %v", moveResp)
	}
	colsAfter := s.listColumns(t, proj.ID)
	if int(colsAfter[0]["id"].(float64)) != c2 {
		t.Fatalf("expected c2 first after move, got %v", colsAfter)
	}

	// Move c2 after c1
	moveResp = s.moveColumn(t, c2, "after:"+itoa(c1))
	if moveResp["id"] == nil {
		t.Fatalf("move column after failed: %v", moveResp)
	}
	colsAfter = s.listColumns(t, proj.ID)
	if int(colsAfter[1]["id"].(float64)) != c2 {
		t.Fatalf("expected c2 second after after-move, got %v", colsAfter)
	}

	// Delete
	resp = s.delete(t, "/api/v3/projects/columns/"+itoa(c2), defaultToken)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("delete column: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

func TestProjectsClassic_CardNoteAndIssue(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "proj-classic-cards", "", false)
	proj := s.store.CreateProjectClassic(repo, admin.ID, "Board", "", "open")
	col := s.store.CreateProjectColumn(proj.ID, "Col")
	issue := s.store.CreateIssue(repo.ID, admin.ID, "tracked issue", "body", nil, nil, 0)

	// Note card
	card1 := s.createCard(t, col.ID, map[string]any{"note": "remember this"})
	if card1["note"] != "remember this" {
		t.Fatalf("expected note, got %v", card1["note"])
	}
	// GitHub's card object always carries the archived flag (round-4).
	if archived, ok := card1["archived"].(bool); !ok || archived {
		t.Fatalf("expected archived=false present, got %v", card1["archived"])
	}

	// Issue card
	card2 := s.createCard(t, col.ID, map[string]any{"content_id": issue.ID, "content_type": "Issue"})
	if card2["content_url"] == nil {
		t.Fatalf("expected content_url for issue card, got nil")
	}
	if card2["note"] != nil {
		t.Fatalf("expected note nil for issue card, got %v", card2["note"])
	}

	// List
	resp := s.get(t, "/api/v3/projects/columns/"+itoa(col.ID)+"/cards", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list cards: %d %s", resp.StatusCode, b)
	}
	var cards []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&cards); err != nil {
		t.Fatalf("decode cards: %v", err)
	}
	resp.Body.Close()
	if len(cards) != 2 {
		t.Fatalf("expected 2 cards, got %d", len(cards))
	}

	// Get card
	cardID := int(card1["id"].(float64))
	resp = s.get(t, "/api/v3/projects/columns/cards/"+itoa(cardID), defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("get card: %d %s", resp.StatusCode, b)
	}
	got := decodeJSON(t, resp)
	if got["note"] != "remember this" {
		t.Fatalf("expected note, got %v", got["note"])
	}

	// Update note
	resp = s.patch(t, "/api/v3/projects/columns/cards/"+itoa(cardID), defaultToken, map[string]any{"note": "updated note"})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("update card: %d %s", resp.StatusCode, b)
	}
	updated := decodeJSON(t, resp)
	if updated["note"] != "updated note" {
		t.Fatalf("expected updated note, got %v", updated["note"])
	}

	// Delete
	resp = s.delete(t, "/api/v3/projects/columns/cards/"+itoa(cardID), defaultToken)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("delete card: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

func TestProjectsClassic_CardMove(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "proj-classic-move", "", false)
	proj := s.store.CreateProjectClassic(repo, admin.ID, "Board", "", "open")
	col1 := s.store.CreateProjectColumn(proj.ID, "Col1")
	col2 := s.store.CreateProjectColumn(proj.ID, "Col2")
	cardA := s.store.CreateProjectCard(col1.ID, admin.ID, "A", 0)
	cardB := s.store.CreateProjectCard(col1.ID, admin.ID, "B", 0)
	cardC := s.store.CreateProjectCard(col1.ID, admin.ID, "C", 0)
	_ = cardA

	// Move B to first
	s.moveCard(t, cardB.ID, 0, "first")
	cards := s.listCards(t, col1.ID)
	if int(cards[0]["id"].(float64)) != cardB.ID {
		t.Fatalf("expected B first, got %v", cards)
	}

	// Move B after C
	s.moveCard(t, cardB.ID, 0, "after:"+itoa(cardC.ID))
	cards = s.listCards(t, col1.ID)
	if int(cards[2]["id"].(float64)) != cardB.ID {
		t.Fatalf("expected B last, got %v", cards)
	}

	// Move B to col2 last
	s.moveCard(t, cardB.ID, col2.ID, "last")
	cards = s.listCards(t, col2.ID)
	if len(cards) != 1 || int(cards[0]["id"].(float64)) != cardB.ID {
		t.Fatalf("expected B in col2, got %v", cards)
	}
	cards = s.listCards(t, col1.ID)
	if len(cards) != 2 {
		t.Fatalf("expected 2 cards in col1, got %d", len(cards))
	}
}

func TestProjectsClassic_404s(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	// Missing project
	resp := s.get(t, "/api/v3/projects/999999", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing project, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Missing column
	resp = s.get(t, "/api/v3/projects/columns/999999", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing column, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Missing card
	resp = s.get(t, "/api/v3/projects/columns/cards/999999", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing card, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestProjectsClassic_RequiresAuth(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp := s.get(t, "/api/v3/repos/admin/proj-classic-crud/projects", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func (s *isolatedServer) moveColumn(t *testing.T, columnID int, position string) map[string]any {
	t.Helper()
	resp := s.post(t, "/api/v3/projects/columns/"+itoa(columnID)+"/moves", defaultToken, map[string]any{"position": position})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("move column: %d %s", resp.StatusCode, b)
	}
	return decodeJSON(t, resp)
}

func (s *isolatedServer) listColumns(t *testing.T, projectID int) []map[string]any {
	t.Helper()
	resp := s.get(t, "/api/v3/projects/"+itoa(projectID)+"/columns", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list columns: %d %s", resp.StatusCode, b)
	}
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode columns: %v", err)
	}
	resp.Body.Close()
	return out
}

func (s *isolatedServer) listCards(t *testing.T, columnID int) []map[string]any {
	t.Helper()
	resp := s.get(t, "/api/v3/projects/columns/"+itoa(columnID)+"/cards", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list cards: %d %s", resp.StatusCode, b)
	}
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode cards: %v", err)
	}
	resp.Body.Close()
	return out
}

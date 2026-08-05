package bleephub

import (
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestGistAndCodespaceResponseSnapshotRace drives the live-pointer readers
// against the store writers that mutate those records in place. Under -race,
// rendering a stored pointer directly is a failure; without -race, the final
// assertions prove request-derived gist URLs never leak back into the store.
func TestGistAndCodespaceResponseSnapshotRace(t *testing.T) {
	s := newTestServer()
	admin := s.store.LookupUserByLogin("admin")
	gist, err := s.store.CreateGistE(admin, "initial", false, map[string]*GistFile{
		"note.txt": {Filename: "note.txt", Content: "initial"},
	})
	if err != nil {
		t.Fatalf("create gist: %v", err)
	}
	now := fixedTestTime.UTC()
	codespace := &Codespace{
		ID:          1,
		Name:        "snapshot-race",
		OwnerLogin:  admin.Login,
		DisplayName: "initial",
		MachineName: codespaceDefaultMachine().Name,
		CreatedAt:   now,
		UpdatedAt:   now,
		LastUsedAt:  now,
		State:       "Available",
		LatestExport: &CodespaceExport{
			ID:          "latest",
			State:       "succeeded",
			CompletedAt: now,
		},
	}
	s.store.mu.Lock()
	s.store.Codespaces[codespace.ID] = codespace
	s.store.CodespacesByName[codespace.Name] = codespace
	s.store.mu.Unlock()

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			description := fmt.Sprintf("description-%d", i)
			if _, _, err := s.store.UpdateGistE(gist.ID, &description, map[string]*GistFile{
				"note.txt": {Filename: "note.txt", Content: description},
			}, nil); err != nil {
				t.Errorf("update gist: %v", err)
				return
			}
			s.store.UpdateCodespace(codespace.ID, description, "", i+1)
		}
	}()
	for reader := 0; reader < 2; reader++ {
		reader := reader
		go func() {
			defer wg.Done()
			request := httptest.NewRequest("GET", fmt.Sprintf("https://reader-%d.example/gists/%s", reader, gist.ID), nil)
			for i := 0; i < iterations; i++ {
				liveGist := s.store.GetGist(gist.ID)
				rendered := s.gistToJSON(liveGist, request, true)
				if rendered["url"] == "" {
					t.Errorf("gist response has no URL")
					return
				}
				liveCodespace := s.store.GetCodespace(codespace.ID)
				if rendered := s.codespaceToJSON(liveCodespace, "https://codespaces.example"); rendered["name"] != codespace.Name {
					t.Errorf("codespace response name = %v, want %s", rendered["name"], codespace.Name)
					return
				}
			}
		}()
	}
	wg.Wait()

	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	stored := s.store.Gists[gist.ID]
	if stored.URL != "" || stored.HTMLURL != "" || stored.GitPullURL != "" {
		t.Fatalf("request-derived URLs leaked into stored gist: URL=%q HTMLURL=%q GitPullURL=%q",
			stored.URL, stored.HTMLURL, stored.GitPullURL)
	}
	if raw := stored.Files["note.txt"].RawURL; raw != "" {
		t.Fatalf("request-derived raw URL leaked into stored gist file: %q", raw)
	}
}

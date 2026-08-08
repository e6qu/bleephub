package bleephub

import (
	"net/http"
	"testing"
)

func TestUIRepoViewerReturnsStableViewerState(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	admin := srv.store.UsersByLogin["admin"]
	repo := srv.store.CreateRepo(admin, "ui-viewer-state", "", false)
	if repo == nil {
		t.Fatal("create repository")
	}
	path := "/ui-data/repos/admin/ui-viewer-state/viewer"

	resp := srv.get(t, path, "")
	if resp.StatusCode != http.StatusUnauthorized {
		resp.Body.Close()
		t.Fatalf("unauthenticated viewer state = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	state := decodeJSONWithStatus(t, srv.get(t, path, defaultToken), http.StatusOK)
	if state["starred"] != false || state["subscribed"] != false {
		t.Fatalf("initial viewer state = %#v, want both false", state)
	}

	if !srv.store.StarRepo(admin.ID, "admin", repo.Name) {
		t.Fatal("star repository")
	}
	if !srv.store.SetRepoSubscription(admin.ID, repo.ID, true) {
		t.Fatal("subscribe to repository")
	}
	state = decodeJSONWithStatus(t, srv.get(t, path, defaultToken), http.StatusOK)
	if state["starred"] != true || state["subscribed"] != true {
		t.Fatalf("selected viewer state = %#v, want both true", state)
	}
}

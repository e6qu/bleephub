package bleephub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// Write on a project is not read on what goes into it. Adding an issue to a
// project republishes its title and state to everyone who can see the project,
// so a caller who cannot read the issue must not be able to pull it in.
//
// The GraphQL twin was fixed first and this handler still validated only that
// the content resolved. Both lanes consult the same predicate now; a fix in one
// lane that leaves the other open is how this existed in the first place.
func TestProjectItemAddRefusesContentTheCallerCannotRead(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	st := srv.store

	now := fixedTestTime.UTC()
	st.Mu.Lock()
	victim := &store.User{ID: st.NextUser, Login: "projcontent-victim", Type: "User", CreatedAt: now, UpdatedAt: now}
	st.Users[victim.ID] = victim
	st.UsersByLogin[victim.Login] = victim
	st.NextUser++
	snooper := &store.User{ID: st.NextUser, Login: "projcontent-snooper", Type: "User", CreatedAt: now, UpdatedAt: now}
	st.Users[snooper.ID] = snooper
	st.UsersByLogin[snooper.Login] = snooper
	st.NextUser++
	st.Mu.Unlock()

	private := st.CreateRepo(victim, "projcontent-private", "private fixture", true)
	if private == nil {
		t.Fatalf("could not create the victim's private repository")
	}
	secret := st.CreateIssue(private.ID, victim.ID, "TOP-SECRET-ISSUE-TITLE", "", nil, nil, 0)
	if secret == nil {
		t.Fatalf("could not create the private issue")
	}

	// The snooper owns a project of their own, so they genuinely hold project
	// write — the point is that it buys them nothing on the victim's content.
	project := st.ProjectsV2.CreateProject(snooper.ID, "User", "snooper board", snooper.ID)
	if project == nil {
		t.Fatalf("could not create the snooper's project")
	}
	token := st.CreateToken(snooper.ID, "repo, project")

	handler := srv.requestHandler()
	add := func(body map[string]any) *httptest.ResponseRecorder {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshalling the request: %v", err)
		}
		path := "/api/v3/users/" + snooper.Login + "/projectsV2/" +
			strconv.Itoa(project.Number) + "/items"
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "token "+token.Value)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	// Both addressing modes must be refused: the direct database id, and the
	// owner/repo/number triple, which resolves the repository but never asked
	// whether the caller may see it.
	for name, body := range map[string]map[string]any{
		"by database id": {"type": "Issue", "id": secret.ID},
		"by coordinates": {"type": "Issue", "owner": victim.Login, "repo": private.Name, "number": secret.Number},
	} {
		w := add(body)
		if w.Code >= 200 && w.Code < 300 {
			t.Errorf("adding a private issue %s returned %d; body=%s", name, w.Code, w.Body.String())
		}
	}

	if items := st.ProjectsV2.ListItemsForProject(project.ID); len(items) != 0 {
		t.Errorf("the project holds %d items, want 0 — the private issue was indexed against it", len(items))
	}

	// Positive control: content the caller can genuinely read still goes in, or
	// this would pass just as well against a handler that refuses everything.
	own := st.CreateRepo(snooper, "projcontent-own", "", false)
	if own == nil {
		t.Fatalf("could not create the snooper's own repository")
	}
	mine := st.CreateIssue(own.ID, snooper.ID, "my own issue", "", nil, nil, 0)
	if mine == nil {
		t.Fatalf("could not create the snooper's own issue")
	}
	if w := add(map[string]any{"type": "Issue", "id": mine.ID}); w.Code < 200 || w.Code >= 300 {
		t.Errorf("adding the caller's own readable issue = %d, want 2xx; body=%s", w.Code, w.Body.String())
	}
}

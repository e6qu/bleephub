package bleephub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The GraphQL mutation lane reads through the credential-aware choke point and
// then, for push and admin, asked only about the bearer. That made a ghu_ token
// strictly broader than the ghs_ token of the same app on every write mutation
// — the exact asymmetry the read path was rebuilt to prevent, one level down.
//
// /api/graphql is a plain route, so requirePerm never runs for it and nothing
// else intersects the credential. The write half has to do it itself.
func TestGraphQLWriteMutationsIntersectTheCredential(t *testing.T) {
	store := testServer.store
	now := fixedTestTime.UTC()

	mkUser := func(login string) *User {
		store.mu.Lock()
		defer store.mu.Unlock()
		u := &User{ID: store.NextUser, Login: login, Type: "User", CreatedAt: now, UpdatedAt: now}
		store.Users[u.ID] = u
		store.UsersByLogin[u.Login] = u
		store.NextUser++
		return u
	}

	victim := mkUser("gqlcred-victim")
	bearer := mkUser("gqlcred-bearer")

	// The repository is public on purpose. On a private one the read gate at
	// the top of the rule refuses these credentials first, and the write half
	// is never reached — which is how a test of this can pass while the hole
	// is wide open. Public means the read legitimately succeeds and the write
	// level is the only thing standing there.
	//
	// The bearer genuinely holds push. That is what makes the borrowed access
	// plausible: the user really can write, the app really cannot.
	repo := store.CreateRepo(victim, "gqlcred-repo", "fixture", false)
	if repo == nil {
		t.Fatalf("could not create the victim's repository")
	}
	if !store.AddRepoCollaborator(victim.Login, "gqlcred-repo", bearer.Login, "push") {
		t.Fatalf("could not give the bearer push")
	}
	// Authored by the victim, so the author-may-act arm cannot admit the write.
	issue := store.CreateIssue(repo.ID, victim.ID, "original title", "", nil, nil, 0)
	if issue == nil {
		t.Fatalf("could not create the issue")
	}

	// An app holding admin on paper and installed nowhere at all.
	perms := map[string]string{"administration": "admin", "contents": "write", "issues": "write", "metadata": "read"}
	app := store.CreateApp(bearer.ID, "Gqlcred App", "", perms, nil)
	if app == nil {
		t.Fatalf("could not create the app")
	}
	if insts := store.ListAppInstallations(app.ID); len(insts) != 0 {
		t.Fatalf("the fixture app must have no installations, got %d", len(insts))
	}
	ghu, _ := store.CreateUserToServerToken(bearer.ID, app.ID, "", "", time.Hour, false)
	if ghu == nil {
		t.Fatalf("could not mint the ghu_ token")
	}
	ghs := store.CreateInstallationToken(0, app.ID, perms, nil)
	if ghs == nil {
		t.Fatalf("could not mint the ghs_ token")
	}

	if issue.NodeID == "" {
		t.Fatalf("the fixture issue has no node id, so the mutation cannot address it")
	}
	mutation := `mutation{updateIssue(input:{id:"` + issue.NodeID + `",title:"hijacked"}){issue{title}}}`

	post := func(token string) (int, string) {
		body, _ := json.Marshal(map[string]string{"query": mutation})
		return ghuRequest(t, "POST", "/api/graphql", token, string(body))
	}

	// The ghs_ of an app installed nowhere is the baseline: it is refused. The
	// ghu_ of the same app must not do better.
	ghsStatus, ghsBody := post(ghs.Token)
	ghsDenied := ghsStatus >= 400 || strings.Contains(ghsBody, "errors")
	if !ghsDenied {
		t.Fatalf("the ghs_ baseline was served (%d): %s — the fixture no longer isolates the credential", ghsStatus, ghsBody)
	}

	ghuStatus, ghuBody := post(ghu.Token)
	t.Logf("ghs_=%d %s", ghsStatus, ghsBody)
	t.Logf("ghu_=%d %s", ghuStatus, ghuBody)
	if ghuStatus < 400 && !strings.Contains(ghuBody, "errors") {
		t.Errorf("updateIssue with a ghu_ of an app installed nowhere was served (%d) where its ghs_ was refused: %s",
			ghuStatus, ghuBody)
	}
	if refreshed := store.GetIssue(issue.ID); refreshed != nil && refreshed.Title != "original title" {
		t.Errorf("the issue title is now %q — the mutation took effect through a credential that cannot reach the repository", refreshed.Title)
	}

	// Positive control: the same bearer's own personal access token holds push
	// and must still be served, or this would pass against a lane that refuses
	// everything.
	pat := store.CreateToken(bearer.ID, "repo")
	if pat == nil {
		t.Fatalf("could not mint the bearer's PAT")
	}
	if status, body := post(pat.Value); status >= 400 || strings.Contains(body, "\"errors\"") {
		t.Errorf("updateIssue with the bearer's own push-holding PAT = %d: %s — the write half is now over-blocking", status, body)
	}
}

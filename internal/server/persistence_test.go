package bleephub

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPersistence_RoundTripAppsInstallationsTokensRepos(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)

	persistRoundTrip(t, func() (*Persistence, error) { return NewPersistence() })
}

func TestPersistence_DatabaseURLFailsLoud(t *testing.T) {
	t.Setenv("BLEEPHUB_DATABASE_URL", "postgres://ci:ci@localhost:5432/postgres?sslmode=disable")
	p, err := NewPersistence()
	if err == nil {
		t.Fatalf("NewPersistence succeeded with obsolete BLEEPHUB_DATABASE_URL: %#v", p)
	}
	if !strings.Contains(err.Error(), "BLEEPHUB_DATABASE_URL is no longer supported") {
		t.Fatalf("error = %v", err)
	}
}

func persistRoundTrip(t *testing.T, open func() (*Persistence, error)) {
	t.Helper()

	p1, err := open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st1 := NewStore()
	if err := st1.SetPersistence(p1); err != nil {
		t.Fatalf("SetPersistence: %v", err)
	}
	st1.SeedDefaultUser()
	user := st1.UsersByLogin["admin"]
	app := st1.CreateApp(user.ID, "Persist App", "desc", map[string]string{"issues": "write"}, []string{"push"})
	inst := st1.CreateInstallation(app.ID, "User", user.ID, user.Login, map[string]string{"issues": "write"}, nil)
	tok := st1.CreateInstallationToken(inst.ID, app.ID, map[string]string{"issues": "write"}, nil)
	st1.SuspendInstallation(inst.ID, user)
	oapp := st1.CreateOAuthApp(user.ID, "Persist OAuth", "", "", "")
	utsTok, _ := st1.CreateUserToServerToken(user.ID, 0, oapp.ClientID, "repo", 60_000_000_000, true)
	repo := st1.CreateRepo(user, "persist-target", "", false)
	expiresAt := time.Now().UTC().Add(24 * time.Hour)
	fineGrained, err := st1.CreateUserFineGrainedPAT(user.ID, createPersonalAccessTokenWebRequest{
		Name: "persist fine-grained", ResourceOwner: user.Login, RepositorySelection: "subset",
		RepositoryIDs: []int{repo.ID}, Permissions: OrgPATPermissions{Repository: map[string]string{"contents": "read"}}, ExpiresAt: &expiresAt,
	})
	if err != nil {
		t.Fatalf("create fine-grained personal access token: %v", err)
	}
	loginSession := &LoginSession{
		UserID: user.ID, CSRFToken: "persisted-csrf", ExpiresAt: time.Now().UTC().Add(time.Hour),
		OIDCProvider: "shauth", OIDCIssuer: "https://auth.example.test", OIDCSubject: "subject-1", OIDCSID: "sid-1", OIDCIDToken: "signed.id.token",
	}
	if err := st1.PutLoginSession("persisted-browser-session", loginSession); err != nil {
		t.Fatalf("persist login session: %v", err)
	}
	st1.UpdateRepo(user.Login, repo.Name, func(r *Repo) {
		r.HasDiscussions = boolPointer(false)
	})
	if err := p1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	p2, err := open()
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	st2 := NewStore()
	if err := st2.SetPersistence(p2); err != nil {
		t.Fatalf("re-load SetPersistence: %v", err)
	}
	defer p2.Close()

	if got := st2.UsersByLogin["admin"]; got == nil {
		t.Fatal("admin user did not persist")
	}
	gotLoginSession, err := st2.GetLoginSession("persisted-browser-session")
	if err != nil {
		t.Fatalf("read persisted login session: %v", err)
	}
	if gotLoginSession == nil || gotLoginSession.UserID != user.ID || gotLoginSession.CSRFToken != "persisted-csrf" || gotLoginSession.OIDCProvider != "shauth" || gotLoginSession.OIDCIssuer != "https://auth.example.test" || gotLoginSession.OIDCSubject != "subject-1" || gotLoginSession.OIDCSID != "sid-1" || gotLoginSession.OIDCIDToken != "signed.id.token" {
		t.Fatalf("login session did not round-trip: %+v", gotLoginSession)
	}
	if err := st2.DeleteLoginSessionsForOIDC("shauth", "https://auth.example.test", "sid-1", "subject-1"); err != nil {
		t.Fatalf("delete persisted OpenID Connect session: %v", err)
	}
	if session, err := st2.GetLoginSession("persisted-browser-session"); err != nil || session != nil {
		t.Fatalf("persisted OpenID Connect session survived back-channel deletion: session=%+v err=%v", session, err)
	}
	gotFineGrained, gotFineGrainedUser := st2.LookupToken(fineGrained.Value)
	if gotFineGrained == nil || gotFineGrainedUser == nil || !gotFineGrained.FineGrained || gotFineGrained.Name != "persist fine-grained" || gotFineGrained.ResourceOwner != user.Login || gotFineGrained.RepositorySelection != "subset" || len(gotFineGrained.RepositoryIDs) != 1 || gotFineGrained.RepositoryIDs[0] != repo.ID || gotFineGrained.Permissions.Repository["contents"] != "read" || gotFineGrained.ExpiresAt == nil {
		t.Fatalf("fine-grained personal access token did not round-trip: token=%+v user=%+v", gotFineGrained, gotFineGrainedUser)
	}

	got := st2.GetApp(app.ID)
	if got == nil {
		t.Fatalf("app %d did not persist", app.ID)
	}
	if got.Slug != app.Slug {
		t.Errorf("app slug round-trip: got %q want %q", got.Slug, app.Slug)
	}
	if got.Permissions["issues"] != "write" {
		t.Errorf("app permissions round-trip: got %v", got.Permissions)
	}

	if st2.GetAppBySlug(app.Slug) == nil {
		t.Error("AppsBySlug index missing after reload")
	}
	if st2.GetAppByClientID(app.ClientID) == nil {
		t.Error("AppsByClientID index missing after reload")
	}

	gotInst := st2.GetInstallation(inst.ID)
	if gotInst == nil {
		t.Fatal("installation did not persist")
	}
	if gotInst.SuspendedAt == nil {
		t.Error("installation SuspendedAt did not persist")
	}

	if gotTok, gotInstFromTok := st2.LookupInstallationToken(tok.Token); gotTok == nil || gotInstFromTok == nil {
		t.Error("installation token did not persist")
	}

	if got := st2.GetOAuthApp(oapp.ClientID); got == nil {
		t.Error("OAuth app did not persist")
	}

	if gotUts, _ := st2.LookupUserToServerToken(utsTok.Token); gotUts == nil {
		t.Error("user-to-server token did not persist")
	}

	if got := st2.GetRepo(user.Login, "persist-target"); got == nil {
		t.Fatal("repo did not persist")
	} else if got.ID != repo.ID {
		t.Errorf("repo ID round-trip: got %d want %d", got.ID, repo.ID)
	} else if repoHasDiscussions(got) {
		t.Error("repo has_discussions=false did not persist")
	}
}

func TestPersistence_DisabledWhenEnvUnset(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "")
	p, err := NewPersistence()
	if err != nil {
		t.Fatalf("NewPersistence: %v", err)
	}
	if p != nil {
		t.Error("expected nil persistence when BLEEPHUB_PERSIST unset")
	}
}

func TestPersistence_BadPathFailsLoud(t *testing.T) {
	// Point at a path that can't be created (under a regular file).
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", filepath.Join(blocker, "subdir"))

	if _, err := NewPersistence(); err == nil {
		t.Fatal("expected error when data dir cannot be created")
	}
}

func TestPersistentOIDCLogoutReplayCannotRevokeLaterSession(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())

	p1, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer p1.Close()
	p2, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	first := NewStore()
	if err := first.SetPersistence(p1); err != nil {
		t.Fatal(err)
	}
	second := NewStore()
	if err := second.SetPersistence(p2); err != nil {
		t.Fatal(err)
	}

	session := &LoginSession{
		UserID: 1, ExpiresAt: time.Now().Add(time.Hour), OIDCProvider: "shauth",
		OIDCIssuer: "https://auth.example.test/", OIDCSubject: "subject-1", OIDCSID: "sid-1",
	}
	if err := first.PutLoginSession("first-session", session); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claimed, err := first.ClaimOIDCLogoutAndDeleteSessions(
		"shauth", "https://auth.example.test/", "bleephub", "same-jti",
		now.Add(10*time.Minute), now, "sid-1", "subject-1",
	)
	if err != nil || !claimed {
		t.Fatalf("first logout claim = %v, err=%v", claimed, err)
	}
	if got, err := first.GetLoginSession("first-session"); err != nil || got != nil {
		t.Fatalf("first selected session survived: session=%#v err=%v", got, err)
	}

	// A later login can have the same provider sid. Replaying the already-used
	// token through another Store/database connection must not revoke it.
	if err := second.PutLoginSession("later-session", session); err != nil {
		t.Fatal(err)
	}
	claimed, err = second.ClaimOIDCLogoutAndDeleteSessions(
		"shauth", "https://auth.example.test/", "bleephub", "same-jti",
		now.Add(10*time.Minute), now, "sid-1", "subject-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("second Store claimed a persisted logout-token replay")
	}
	if got, err := second.GetLoginSession("later-session"); err != nil || got == nil {
		t.Fatalf("replay revoked later session: session=%#v err=%v", got, err)
	}
}

func TestPersistentOIDCLogoutClaimIsExclusiveAcrossConnections(t *testing.T) {
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", t.TempDir())
	p1, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer p1.Close()
	p2, err := NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	stores := []*Store{NewStore(), NewStore()}
	for index, persistence := range []*Persistence{p1, p2} {
		if err := stores[index].SetPersistence(persistence); err != nil {
			t.Fatal(err)
		}
	}

	type result struct {
		claimed bool
		err     error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	for _, store := range stores {
		go func(store *Store) {
			ready.Done()
			<-start
			now := time.Now()
			claimed, err := store.ClaimOIDCLogoutAndDeleteSessions(
				"shauth", "https://auth.example.test/", "bleephub", "concurrent-jti",
				now.Add(10*time.Minute), now, "sid-1", "subject-1",
			)
			results <- result{claimed: claimed, err: err}
		}(store)
	}
	ready.Wait()
	close(start)
	claims := 0
	for range stores {
		outcome := <-results
		if outcome.err != nil {
			t.Fatalf("concurrent claim: %v", outcome.err)
		}
		if outcome.claimed {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("successful concurrent claims = %d, want exactly 1", claims)
	}
}

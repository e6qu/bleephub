package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func TestResponseFacingStoreReadsAreDetached(t *testing.T) {
	s := newTestServer()
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "detached-store-reads", "", false)

	comment := s.store.CreateComment(s.store.CreateIssue(repo.ID, admin.ID, "issue", "", nil, nil, 0).ID, admin.ID, "stored")
	readComment := s.store.GetComment(comment.ID)
	readComment.Body = "caller mutation"
	if got := s.store.GetComment(comment.ID); got == nil || got.Body != "stored" {
		t.Fatalf("comment getter leaked store ownership: %#v", got)
	}

	migration := s.store.CreateUserMigration(admin.ID, []string{repo.FullName}, true, false, false, false, false, false, false)
	readMigration := s.store.GetUserMigration(migration.ID)
	readMigration.Repositories[0] = "caller/changed"
	readMigration.LockedRepos[repo.Name] = false
	if got := s.store.GetUserMigration(migration.ID); got.Repositories[0] != repo.FullName || !got.LockedRepos[repo.Name] {
		t.Fatalf("migration getter leaked nested store ownership: %#v", got)
	}

	ruleset := s.store.CreateRuleset(repo, &store.Ruleset{
		Name: "detached",
		Rules: []store.Rule{{
			Type: "required_status_checks",
			Parameters: map[string]interface{}{
				"required_status_checks": []interface{}{map[string]interface{}{"context": "build"}},
			},
		}},
	})
	readRuleset := s.store.GetRuleset(ruleset.ID)
	readRuleset.Rules[0].Parameters["required_status_checks"].([]interface{})[0].(map[string]interface{})["context"] = "caller"
	gotRuleset := s.store.GetRuleset(ruleset.ID)
	context := gotRuleset.Rules[0].Parameters["required_status_checks"].([]interface{})[0].(map[string]interface{})["context"]
	if context != "build" {
		t.Fatalf("ruleset getter leaked nested store ownership: context=%v", context)
	}

	app := s.store.CreateApp(admin.ID, "Detached App", "", map[string]string{"issues": "write"}, []string{"issues"})
	app.Permissions["issues"] = "admin"
	app.Events[0] = "push"
	if got := s.store.GetApp(app.ID); got.Permissions["issues"] != "write" || got.Events[0] != "issues" {
		t.Fatalf("App create response leaked store ownership: %#v", got)
	}
	readApp := s.store.GetApp(app.ID)
	readApp.Permissions["issues"] = "read"
	readApp.Events[0] = "push"
	if got := s.store.GetApp(app.ID); got.Permissions["issues"] != "write" || got.Events[0] != "issues" {
		t.Fatalf("App getter leaked nested store ownership: %#v", got)
	}

	storedApp := s.store.GetApp(app.ID)
	installation := s.store.CreateInstallation(app.ID, "User", admin.ID, admin.Login, storedApp.Permissions, storedApp.Events)
	installation.Permissions["issues"] = "admin"
	installation.Events[0] = "pull_request"
	if got := s.store.GetInstallation(installation.ID); got.Permissions["issues"] != "write" || got.Events[0] != "issues" {
		t.Fatalf("installation create response leaked store ownership: %#v", got)
	}
	readInstallation := s.store.GetInstallation(installation.ID)
	readInstallation.Permissions["issues"] = "read"
	readInstallation.Events[0] = "push"
	if got := s.store.GetInstallation(installation.ID); got.Permissions["issues"] != "write" || got.Events[0] != "issues" {
		t.Fatalf("installation getter leaked nested store ownership: %#v", got)
	}

	installationToken := s.store.CreateInstallationToken(installation.ID, app.ID, installation.Permissions, []int{repo.ID})
	installationToken.RepositoryIDs[0] = -1
	if got, _ := s.store.LookupInstallationToken(installationToken.Token); got.RepositoryIDs[0] != repo.ID {
		t.Fatalf("installation-token create response leaked store ownership: %#v", got)
	}
	readToken, _ := s.store.LookupInstallationToken(installationToken.Token)
	readToken.RepositoryIDs[0] = -1
	if got, _ := s.store.LookupInstallationToken(installationToken.Token); got.RepositoryIDs[0] != repo.ID {
		t.Fatalf("installation-token lookup leaked nested store ownership: %#v", got)
	}
}

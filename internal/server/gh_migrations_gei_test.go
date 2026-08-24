package bleephub

// The GitHub Enterprise Importer, end to end: the mutation surface's
// authorization table (refused for a stranger, served for a migrator), the
// repository migration worker driving a real git fetch out of a repository on
// this server and into a new one, the lock that freezes the source while it
// runs, the resume path, and the organization migration's fan-out.

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// --- fixture ----------------------------------------------------------------

// geiFixture is one organization with a migration source, a repository to
// migrate from, an owner, a granted migrator, and a stranger who owns a
// *different* organization — the cross-tenant control.
type geiFixture struct {
	tag           string
	enterprise    *store.Enterprise
	org           *store.Org
	owner         *store.User
	ownerToken    string
	migrator      *store.User
	migratorToken string
	// stranger owns otherOrg and holds the migrator role on it, and nothing at
	// all on org. Every refusal case is driven as this account, which is what
	// makes the refusals evidence of tenant isolation rather than of
	// authentication.
	stranger      *store.User
	strangerToken string
	otherOrg      *store.Org
	source        *store.MigrationSource
	sourceRepo    string
	migration     *store.RepositoryMigration
}

func (s *isolatedServer) newGEIFixture(t *testing.T, tag string) *geiFixture {
	t.Helper()
	f := &geiFixture{tag: tag}
	f.owner, f.ownerToken = s.newUser(t, "geiowner"+tag)
	f.migrator, f.migratorToken = s.newUser(t, "geimigrator"+tag)
	f.stranger, f.strangerToken = s.newUser(t, "geistranger"+tag)

	f.enterprise = s.store.CreateEnterprise("gei-"+tag, "GEI "+tag, "billing-"+tag+"@gei.test")
	if f.enterprise == nil {
		t.Fatalf("CreateEnterprise gei-%s failed", tag)
	}
	s.store.SetEnterpriseMembership(f.enterprise.ID, f.owner.ID, store.EnterpriseRoleOwner)

	f.org = s.store.CreateOrg(f.owner, "geiorg"+tag, "GEI Org "+tag, "")
	f.otherOrg = s.store.CreateOrg(f.stranger, "geiother"+tag, "Other Org "+tag, "")
	if f.org == nil || f.otherOrg == nil {
		t.Fatalf("CreateOrg for %s failed", tag)
	}
	s.store.AddEnterpriseOrganization(f.enterprise.ID, f.org.ID)

	// The migrator holds the role on f.org; the stranger holds it on their own
	// organization, so a refusal below cannot be explained by the stranger
	// simply never having been granted anything anywhere.
	if !s.store.SetOrgMigratorRole(f.org.ID, "USER", f.migrator.Login, f.owner.ID, true) {
		t.Fatal("granting the migrator role failed")
	}
	if !s.store.SetOrgMigratorRole(f.otherOrg.ID, "USER", f.stranger.Login, f.stranger.ID, true) {
		t.Fatal("granting the stranger the migrator role on their own org failed")
	}

	f.sourceRepo = s.createRepoWriteRepo(t, true)
	f.source = s.store.CreateMigrationSource(f.org.ID, "source-"+tag, store.MigrationSourceTypeGitHubArchive,
		s.baseURL, defaultToken, "")
	if f.source == nil {
		t.Fatal("CreateMigrationSource failed")
	}
	f.migration = s.store.CreateRepositoryMigration(store.NewRepositoryMigration{
		OwnerOrgID:      f.org.ID,
		SourceID:        f.source.ID,
		RepositoryName:  "seeded-" + tag,
		SourceURL:       s.baseURL + "/admin/" + f.sourceRepo + ".git",
		ContinueOnError: true,
		StartedByUserID: f.owner.ID,
	})
	if f.migration == nil {
		t.Fatal("CreateRepositoryMigration failed")
	}
	return f
}

func (f *geiFixture) sourceRepositoryURL(s *isolatedServer) string {
	return s.baseURL + "/admin/" + f.sourceRepo + ".git"
}

// --- the authorization table -------------------------------------------------

// gqlMigrationMutationCase is one row of the migration mutation surface.
type gqlMigrationMutationCase struct {
	name  string
	doc   string
	input func(s *isolatedServer, f *geiFixture) map[string]interface{}
	// entitled names the caller the mutation must serve. It is the migrator
	// for the organization-scoped rows and the enterprise owner for
	// startOrganizationMigration, which is an enterprise owner's act.
	entitled func(f *geiFixture) string
}

// gqlMigrationMutationCases covers every migration mutation. The subject of
// each is an organization or an enterprise rather than a repository, so these
// rows live here rather than in gqlMutationCases; the coverage gate in
// graphql_authz_test.go folds them in.
func gqlMigrationMutationCases() []gqlMigrationMutationCase {
	return []gqlMigrationMutationCase{
		{
			name: "createMigrationSource",
			doc:  `mutation($input:CreateMigrationSourceInput!){createMigrationSource(input:$input){migrationSource{id name type url}}}`,
			input: func(_ *isolatedServer, f *geiFixture) map[string]interface{} {
				return map[string]interface{}{
					"ownerId": f.org.NodeID, "name": "table-source-" + f.tag,
					"type": "GITHUB_ARCHIVE", "url": "https://github.com",
					"accessToken": "ghp_source_token", "githubPat": "ghp_target_pat",
				}
			},
			entitled: func(f *geiFixture) string { return f.migratorToken },
		},
		{
			name: "startRepositoryMigration",
			doc:  `mutation($input:StartRepositoryMigrationInput!){startRepositoryMigration(input:$input){repositoryMigration{id state repositoryName}}}`,
			input: func(s *isolatedServer, f *geiFixture) map[string]interface{} {
				return map[string]interface{}{
					"ownerId": f.org.NodeID, "sourceId": f.source.NodeID,
					"repositoryName": "table-" + f.tag, "sourceRepositoryUrl": f.sourceRepositoryURL(s),
					"continueOnError": true,
				}
			},
			entitled: func(f *geiFixture) string { return f.migratorToken },
		},
		{
			name: "abortRepositoryMigration",
			doc:  `mutation($input:AbortRepositoryMigrationInput!){abortRepositoryMigration(input:$input){success}}`,
			input: func(_ *isolatedServer, f *geiFixture) map[string]interface{} {
				return map[string]interface{}{"migrationId": f.migration.NodeID}
			},
			entitled: func(f *geiFixture) string { return f.migratorToken },
		},
		{
			name: "abortQueuedMigrations",
			doc:  `mutation($input:AbortQueuedMigrationsInput!){abortQueuedMigrations(input:$input){success}}`,
			input: func(_ *isolatedServer, f *geiFixture) map[string]interface{} {
				return map[string]interface{}{"ownerId": f.org.NodeID}
			},
			entitled: func(f *geiFixture) string { return f.migratorToken },
		},
		{
			name: "grantMigratorRole",
			doc:  `mutation($input:GrantMigratorRoleInput!){grantMigratorRole(input:$input){success}}`,
			input: func(_ *isolatedServer, f *geiFixture) map[string]interface{} {
				return map[string]interface{}{
					"organizationId": f.org.NodeID, "actor": f.stranger.Login, "actorType": "USER",
				}
			},
			entitled: func(f *geiFixture) string { return f.ownerToken },
		},
		{
			name: "revokeMigratorRole",
			doc:  `mutation($input:RevokeMigratorRoleInput!){revokeMigratorRole(input:$input){success}}`,
			input: func(_ *isolatedServer, f *geiFixture) map[string]interface{} {
				return map[string]interface{}{
					"organizationId": f.org.NodeID, "actor": f.migrator.Login, "actorType": "USER",
				}
			},
			entitled: func(f *geiFixture) string { return f.ownerToken },
		},
		{
			name: "startOrganizationMigration",
			doc:  `mutation($input:StartOrganizationMigrationInput!){startOrganizationMigration(input:$input){orgMigration{id state targetOrgName}}}`,
			input: func(s *isolatedServer, f *geiFixture) map[string]interface{} {
				return map[string]interface{}{
					"targetEnterpriseId": f.enterprise.NodeID,
					"sourceOrgUrl":       s.baseURL + "/" + f.org.Login,
					"sourceAccessToken":  defaultToken,
					"targetOrgName":      "migrated-" + f.tag,
				}
			},
			entitled: func(f *geiFixture) string { return f.ownerToken },
		},
	}
}

// TestGEIMutationsRefuseAnotherTenantsMigrator is the cross-tenant isolation
// proof. The refusing caller is not a stranger to migrations — they own an
// organization and hold the migrator role on it — so a refusal here can only
// be about *which* organization the standing is over.
func TestGEIMutationsRefuseAnotherTenantsMigrator(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlMigrationMutationCases() {
		f := s.newGEIFixture(t, "refuse"+strings.ToLower(tc.name))
		env := s.gqlAuthzPost(t, f.strangerToken, tc.doc, map[string]interface{}{"input": tc.input(s, f)})
		if len(gqlAuthzErrors(env)) == 0 {
			t.Errorf("%s: another tenant's migrator was served: %v", tc.name, env)
		}
		// The refusal has to be a refusal rather than an error reported after
		// the write landed.
		if migration := s.store.GetRepositoryMigration(f.migration.ID); migration == nil ||
			migration.State != store.GEIMigrationStateQueued {
			t.Errorf("%s: the fixture migration was moved by another tenant: %+v", tc.name, migration)
		}
		if !s.store.UserHoldsOrgMigratorRole(f.org.ID, f.migrator) {
			t.Errorf("%s: another tenant revoked this organization's migrator", tc.name)
		}
		if s.store.UserHoldsOrgMigratorRole(f.org.ID, f.stranger) {
			t.Errorf("%s: another tenant granted themselves the migrator role", tc.name)
		}
		if s.store.GetOrg("migrated-"+f.tag) != nil {
			t.Errorf("%s: another tenant started an organization migration", tc.name)
		}
	}
}

// TestGEIMutationsServeTheirEntitledCaller is the positive half: a guard that
// refuses everybody is not a guard.
func TestGEIMutationsServeTheirEntitledCaller(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	for _, tc := range gqlMigrationMutationCases() {
		f := s.newGEIFixture(t, "allow"+strings.ToLower(tc.name))
		env := s.gqlAuthzPost(t, tc.entitled(f), tc.doc, map[string]interface{}{"input": tc.input(s, f)})
		if errs := gqlAuthzErrors(env); len(errs) > 0 {
			t.Errorf("%s: the entitled caller was refused: %v", tc.name, errs)
		}
	}
}

// TestGEIRepositoryMigrationsAreInvisibleToAnotherTenant covers the read side.
// A global id is not a capability: guessing a migration's node id must not
// reveal it, and neither must reading the organization's connection.
func TestGEIRepositoryMigrationsAreInvisibleToAnotherTenant(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGEIFixture(t, "read")

	const connection = `query($login:String!){organization(login:$login){repositoryMigrations(first:10){totalCount nodes{id repositoryName state}}}}`
	env := s.gqlAuthzPost(t, f.strangerToken, connection, map[string]interface{}{"login": f.org.Login})
	if len(gqlAuthzErrors(env)) == 0 {
		t.Fatalf("another tenant read the migration connection: %v", env)
	}

	const node = `query($id:ID!){node(id:$id){... on RepositoryMigration{id repositoryName}}}`
	env = s.gqlAuthzPost(t, f.strangerToken, node, map[string]interface{}{"id": f.migration.NodeID})
	data, _ := env["data"].(map[string]interface{})
	if data == nil || data["node"] != nil {
		t.Fatalf("another tenant resolved the migration by global id: %v", env)
	}

	// The migrator sees both.
	env = s.gqlAuthzPost(t, f.migratorToken, connection, map[string]interface{}{"login": f.org.Login})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("the granted migrator was refused the connection: %v", errs)
	}
	data, _ = env["data"].(map[string]interface{})
	org, _ := data["organization"].(map[string]interface{})
	conn, _ := org["repositoryMigrations"].(map[string]interface{})
	if conn == nil || conn["totalCount"] != float64(1) {
		t.Fatalf("the migrator's connection = %v, want the one seeded migration", conn)
	}

	// The Migration interface resolves to its concrete type, so a client can
	// select through it rather than having to know which implementor it got.
	const asMigration = `query($id:ID!){node(id:$id){... on Migration{__typename state repositoryName continueOnError warningsCount migrationSource{name type url}}}}`
	env = s.gqlAuthzPost(t, f.migratorToken, asMigration, map[string]interface{}{"id": f.migration.NodeID})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("selecting through the Migration interface: %v", errs)
	}
	data, _ = env["data"].(map[string]interface{})
	resolved, _ := data["node"].(map[string]interface{})
	if resolved == nil || resolved["__typename"] != "RepositoryMigration" {
		t.Fatalf("Migration resolved to %v, want RepositoryMigration", resolved)
	}
	if resolved["state"] != store.GEIMigrationStateQueued {
		t.Fatalf("state through the interface = %v", resolved["state"])
	}
	if src, _ := resolved["migrationSource"].(map[string]interface{}); src == nil || src["name"] != f.source.Name {
		t.Fatalf("migrationSource through the interface = %v", resolved["migrationSource"])
	}
}

// TestGEIRepositoryMigrationCopiesTheSourceRepository is the whole point: a
// migration that reports SUCCEEDED has actually moved the repository.
//
// The source is a repository on this same server, reached over its smart-HTTP
// endpoint, so the fetch is a real git fetch over a real transport rather than
// a store-to-store copy — the same code path an import from another GitHub
// instance takes.
func TestGEIRepositoryMigrationCopiesTheSourceRepository(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGEIFixture(t, "copy")

	// Content on the source that the migration has to bring across alongside
	// the git data.
	sourceFull := "admin/" + f.sourceRepo
	sourceRepo := s.store.GetRepoByFullName(sourceFull)
	if sourceRepo == nil {
		t.Fatalf("the seeded source repository %s is missing", sourceFull)
	}
	s.store.CreateIssue(sourceRepo.ID, s.store.UsersByLogin["admin"].ID, "carried across", "", nil, nil, 0)
	s.store.Releases.Create(sourceRepo.ID, s.store.UsersByLogin["admin"].ID, "v1.0.0", "", "First", "", false, false, false)

	env := s.gqlAuthzPost(t, f.migratorToken,
		`mutation($input:StartRepositoryMigrationInput!){startRepositoryMigration(input:$input){repositoryMigration{id state repositoryName sourceUrl warningsCount migrationSource{name type}}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"ownerId": f.org.NodeID, "sourceId": f.source.NodeID,
			"repositoryName": "copied", "sourceRepositoryUrl": f.sourceRepositoryURL(s),
			"continueOnError": true, "lockSource": true, "targetRepoVisibility": "private",
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("startRepositoryMigration: %v", errs)
	}
	migration := lastRepositoryMigration(t, s, f.org.ID)

	waitFor(t, func() bool {
		m := s.store.GetRepositoryMigration(migration.ID)
		return m != nil && store.GEIMigrationTerminal(m.State)
	}, "the repository migration never reached a terminal state")

	done := s.store.GetRepositoryMigration(migration.ID)
	if done.State != store.GEIMigrationStateSucceeded {
		t.Fatalf("state = %s, failure = %q", done.State, done.FailureReason)
	}

	target := s.store.GetRepoByFullName(f.org.Login + "/copied")
	if target == nil {
		t.Fatal("the migration reported success without creating the target repository")
	}
	if !target.Private || target.Visibility != "private" {
		t.Errorf("target visibility = %q private = %v, want the requested private", target.Visibility, target.Private)
	}
	// The git data really landed: the source's README is readable through the
	// target's contents API.
	resp := s.get(t, "/api/v3/repos/"+target.FullName+"/contents/README.md", defaultToken)
	requireStatus(t, resp, 200)

	// And the content the source held came with it.
	issues := s.store.ListIssues(target.ID, "all")
	if len(issues) != 1 || issues[0].Title != "carried across" {
		t.Errorf("migrated issues = %+v, want the source's one issue", issues)
	}
	if releases := s.store.Releases.List(target.ID); len(releases) != 1 || releases[0].TagName != "v1.0.0" {
		t.Errorf("migrated releases = %+v, want the source's one release", releases)
	}

	// The migration produced a log, and it is served behind the same standing
	// the migration is.
	if done.MigrationLogKey == "" {
		t.Fatal("the migration recorded no log")
	}
	logPath := "/ui-data/orgs/" + f.org.Login + "/migrations/repositories/" + itoa(done.ID) + "/log"
	resp = s.get(t, logPath, f.migratorToken)
	requireStatus(t, resp, 200)
	resp.Body.Close()
	resp = s.get(t, logPath, f.strangerToken)
	requireStatus(t, resp, 404)
	resp.Body.Close()
}

// TestGEIRepositoryMigrationLockBlocksWritesToTheSource proves lockSource is a
// lock rather than a flag: while the migration holds it, the write path
// refuses the source repository, and releasing it lets writes through again.
func TestGEIRepositoryMigrationLockBlocksWritesToTheSource(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGEIFixture(t, "lock")
	sourceFull := "admin/" + f.sourceRepo

	if s.store.RepoLockedForMigration(sourceFull) {
		t.Fatal("the source is locked before any migration claims it")
	}
	migration := s.store.CreateRepositoryMigration(store.NewRepositoryMigration{
		OwnerOrgID: f.org.ID, SourceID: f.source.ID, RepositoryName: "locked",
		SourceURL: f.sourceRepositoryURL(s), LockSource: true, StartedByUserID: f.owner.ID,
	})
	if !s.store.SetRepositoryMigrationSourceLock(migration.ID, sourceFull) {
		t.Fatal("the migration could not take the source lock")
	}
	if !s.store.RepoLockedForMigration(sourceFull) {
		t.Fatal("the source is not locked while the migration holds it")
	}
	// The one predicate every repository write asks refuses, so a write
	// through any surface refuses.
	resp := s.put(t, "/api/v3/repos/"+sourceFull+"/contents/locked.txt", defaultToken,
		map[string]interface{}{"message": "while locked", "content": "bG9ja2VkCg=="})
	if resp.StatusCode < 400 {
		t.Fatalf("a write to a locked source was accepted: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// Reads are unaffected — the migration itself has to read the source.
	resp = s.get(t, "/api/v3/repos/"+sourceFull, defaultToken)
	requireStatus(t, resp, 200)
	resp.Body.Close()

	// A terminal migration releases the lock: nothing else has to remember to.
	s.store.SetRepositoryMigrationState(migration.ID, store.GEIMigrationStateFailed, "aborted by the test")
	if s.store.RepoLockedForMigration(sourceFull) {
		t.Fatal("the lock outlived the migration that took it")
	}
	resp = s.put(t, "/api/v3/repos/"+sourceFull+"/contents/unlocked.txt", defaultToken,
		map[string]interface{}{"message": "after unlock", "content": "b3Blbgo="})
	requireStatus(t, resp, 201)
}

// TestGEIRepositoryMigrationFailsWithARealReason covers the failure half of
// the state machine: a source nothing answers at must land the migration in a
// failure state carrying what actually went wrong.
func TestGEIRepositoryMigrationFailsWithARealReason(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGEIFixture(t, "fail")

	migration := s.store.CreateRepositoryMigration(store.NewRepositoryMigration{
		OwnerOrgID: f.org.ID, SourceID: f.source.ID, RepositoryName: "never-arrives",
		SourceURL: s.baseURL + "/admin/no-such-repository.git", ContinueOnError: true,
		StartedByUserID: f.owner.ID,
	})
	s.startGEIRepositoryMigration(migration.ID)
	waitFor(t, func() bool {
		m := s.store.GetRepositoryMigration(migration.ID)
		return m != nil && store.GEIMigrationTerminal(m.State)
	}, "the doomed migration never reached a terminal state")

	done := s.store.GetRepositoryMigration(migration.ID)
	if done.State == store.GEIMigrationStateSucceeded {
		t.Fatal("a migration from a source that does not exist reported success")
	}
	if done.FailureReason == "" {
		t.Fatal("the migration failed with no reason recorded")
	}
	if strings.Contains(done.FailureReason, "timer") {
		t.Fatalf("failure reason = %q", done.FailureReason)
	}
}

// TestGEIMigrationTargetNameCollisionFailsValidation: a migration must never be
// a way to write into a repository somebody else already owns the name of.
func TestGEIMigrationTargetNameCollisionFailsValidation(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGEIFixture(t, "collide")

	admin := s.store.UsersByLogin["admin"]
	occupied := s.store.CreateOrgRepo(f.org, admin, "occupied", "somebody else's repository", false)
	if occupied == nil {
		t.Fatal("could not seed the occupied name")
	}
	migration := s.store.CreateRepositoryMigration(store.NewRepositoryMigration{
		OwnerOrgID: f.org.ID, SourceID: f.source.ID, RepositoryName: "occupied",
		SourceURL: f.sourceRepositoryURL(s), ContinueOnError: true, StartedByUserID: f.owner.ID,
	})
	s.startGEIRepositoryMigration(migration.ID)
	waitFor(t, func() bool {
		m := s.store.GetRepositoryMigration(migration.ID)
		return m != nil && store.GEIMigrationTerminal(m.State)
	}, "the colliding migration never finished")

	done := s.store.GetRepositoryMigration(migration.ID)
	if done.State != store.GEIMigrationStateFailedValidation {
		t.Fatalf("state = %s, want FAILED_VALIDATION (reason %q)", done.State, done.FailureReason)
	}
	if repo := s.store.GetRepoByFullName(f.org.Login + "/occupied"); repo == nil ||
		repo.Description != "somebody else's repository" {
		t.Fatalf("the migration overwrote the occupied repository: %+v", repo)
	}
}

// TestGEIAbortIsFinal: a migration aborted mid-flight stays aborted, because a
// worker that finishes afterwards cannot leave a terminal state.
func TestGEIAbortIsFinal(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGEIFixture(t, "abort")

	env := s.gqlAuthzPost(t, f.migratorToken,
		`mutation($input:AbortRepositoryMigrationInput!){abortRepositoryMigration(input:$input){success}}`,
		map[string]interface{}{"input": map[string]interface{}{"migrationId": f.migration.NodeID}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("abortRepositoryMigration: %v", errs)
	}
	aborted := s.store.GetRepositoryMigration(f.migration.ID)
	if aborted.State != store.GEIMigrationStateFailed {
		t.Fatalf("state after abort = %s", aborted.State)
	}
	// A worker arriving late cannot claim it, and cannot overwrite the verdict.
	s.runGEIRepositoryMigration(f.migration.ID)
	if again := s.store.GetRepositoryMigration(f.migration.ID); again.State != store.GEIMigrationStateFailed ||
		again.FailureReason != aborted.FailureReason {
		t.Fatalf("a late worker rewrote an aborted migration: %+v", again)
	}
	// And a second abort reports false rather than rewriting history.
	env = s.gqlAuthzPost(t, f.migratorToken,
		`mutation($input:AbortRepositoryMigrationInput!){abortRepositoryMigration(input:$input){success}}`,
		map[string]interface{}{"input": map[string]interface{}{"migrationId": f.migration.NodeID}})
	data, _ := env["data"].(map[string]interface{})
	payload, _ := data["abortRepositoryMigration"].(map[string]interface{})
	if payload == nil || payload["success"] != false {
		t.Fatalf("re-aborting a terminal migration reported %v, want success:false", payload)
	}
}

// TestGEIResumeRequeuesInterruptedMigrations covers the restart path the whole
// worker design turns on: a migration a dead process left IN_PROGRESS is
// picked up and driven to completion, rather than sitting unfinished forever.
func TestGEIResumeRequeuesInterruptedMigrations(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGEIFixture(t, "resume")

	migration := s.store.CreateRepositoryMigration(store.NewRepositoryMigration{
		OwnerOrgID: f.org.ID, SourceID: f.source.ID, RepositoryName: "resumed",
		SourceURL: f.sourceRepositoryURL(s), ContinueOnError: true, StartedByUserID: f.owner.ID,
	})
	// Exactly the state a process killed mid-export leaves behind.
	if !s.store.ClaimRepositoryMigration(migration.ID) {
		t.Fatal("could not put the migration into the interrupted state")
	}
	if got := s.store.GetRepositoryMigration(migration.ID); got.State != store.GEIMigrationStateInProgress {
		t.Fatalf("state = %s, want IN_PROGRESS", got.State)
	}

	s.resumeGEIMigrations()
	waitFor(t, func() bool {
		m := s.store.GetRepositoryMigration(migration.ID)
		return m != nil && store.GEIMigrationTerminal(m.State)
	}, "resumeGEIMigrations left the interrupted migration unfinished")

	done := s.store.GetRepositoryMigration(migration.ID)
	if done.State != store.GEIMigrationStateSucceeded {
		t.Fatalf("resumed migration state = %s, failure = %q", done.State, done.FailureReason)
	}
	if s.store.GetRepoByFullName(f.org.Login+"/resumed") == nil {
		t.Fatal("the resumed migration reported success without creating the target")
	}
}

// TestGEIOrganizationMigrationFansOutOverTheSource drives the organization
// worker through its three phases against a source organization on this same
// server: it enumerates the source's repositories over the REST API, creates
// the target organization inside the enterprise, and migrates each repository.
func TestGEIOrganizationMigrationFansOutOverTheSource(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGEIFixture(t, "orgfan")

	admin := s.store.UsersByLogin["admin"]
	sourceOrg := s.store.CreateOrg(admin, "geisource"+f.tag, "Source Org", "")
	if sourceOrg == nil {
		t.Fatal("could not create the source organization")
	}
	// Two repositories with real git content to bring across.
	for _, name := range []string{"alpha", "beta"} {
		if s.store.CreateOrgRepo(sourceOrg, admin, name, "source "+name, false) == nil {
			t.Fatalf("could not seed %s", name)
		}
		resp := s.put(t, "/api/v3/repos/"+sourceOrg.Login+"/"+name+"/contents/README.md", defaultToken,
			map[string]interface{}{"message": "seed", "content": "aGVsbG8K"})
		if resp.StatusCode >= 300 {
			resp.Body.Close()
			t.Fatalf("seeding %s content: %d", name, resp.StatusCode)
		}
		resp.Body.Close()
	}

	env := s.gqlAuthzPost(t, f.ownerToken,
		`mutation($input:StartOrganizationMigrationInput!){startOrganizationMigration(input:$input){orgMigration{id state sourceOrgName targetOrgName}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"targetEnterpriseId": f.enterprise.NodeID,
			"sourceOrgUrl":       s.baseURL + "/" + sourceOrg.Login,
			"sourceAccessToken":  defaultToken,
			"targetOrgName":      "landed" + f.tag,
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("startOrganizationMigration: %v", errs)
	}

	migrations := s.store.ListOrganizationMigrations(f.enterprise.ID)
	if len(migrations) != 1 {
		t.Fatalf("organization migrations = %d, want 1", len(migrations))
	}
	id := migrations[0].ID
	waitFor(t, func() bool {
		m := s.store.GetOrganizationMigration(id)
		return m != nil && store.GEIMigrationTerminal(m.State)
	}, "the organization migration never reached a terminal state")

	done := s.store.GetOrganizationMigration(id)
	if done.State != store.GEIMigrationStateSucceeded {
		for _, child := range s.store.ListRepositoryMigrationsForOrgMigration(id) {
			t.Logf("child %s: %s %q", child.RepositoryName, child.State, child.FailureReason)
		}
		t.Fatalf("state = %s, failure = %q", done.State, done.FailureReason)
	}
	if done.TotalRepositoriesCount == nil || *done.TotalRepositoriesCount != 2 {
		t.Fatalf("totalRepositoriesCount = %v, want the source's 2 repositories", done.TotalRepositoriesCount)
	}
	if done.RemainingRepositoriesCount == nil || *done.RemainingRepositoriesCount != 0 {
		t.Fatalf("remainingRepositoriesCount = %v, want 0", done.RemainingRepositoriesCount)
	}
	targetOrg := s.store.GetOrg("landed" + f.tag)
	if targetOrg == nil {
		t.Fatal("the organization migration reported success without creating the target organization")
	}
	if s.store.EnterpriseIDForOrg(targetOrg.ID) != f.enterprise.ID {
		t.Error("the migrated organization was not attached to the target enterprise")
	}
	for _, name := range []string{"alpha", "beta"} {
		repo := s.store.GetRepoByFullName(targetOrg.Login + "/" + name)
		if repo == nil {
			t.Fatalf("%s did not land in the target organization", name)
		}
		resp := s.get(t, "/api/v3/repos/"+repo.FullName+"/contents/README.md", defaultToken)
		requireStatus(t, resp, 200)
		resp.Body.Close()
	}
	children := s.store.ListRepositoryMigrationsForOrgMigration(id)
	if len(children) != 2 {
		t.Fatalf("child migrations = %d, want 2", len(children))
	}
}

// TestGEIMigrationSourceCredentialsAreNeverServed: the source holds the token
// that reaches somebody else's instance, and no read surface hands it back.
func TestGEIMigrationSourceCredentialsAreNeverServed(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGEIFixture(t, "secrets")

	env := s.gqlAuthzPost(t, f.migratorToken,
		`mutation($input:CreateMigrationSourceInput!){createMigrationSource(input:$input){migrationSource{id name type url}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"ownerId": f.org.NodeID, "name": "secretive", "type": "GITHUB_ARCHIVE",
			"url": "https://github.com", "accessToken": "ghp_super_secret_source",
			"githubPat": "ghp_super_secret_target",
		}})
	if errs := gqlAuthzErrors(env); len(errs) > 0 {
		t.Fatalf("createMigrationSource: %v", errs)
	}

	resp := s.get(t, "/ui-data/orgs/"+f.org.Login+"/migrations/sources", f.migratorToken)
	if resp.StatusCode != 200 {
		resp.Body.Close()
		t.Fatalf("listing migration sources = %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, secret := range []string{"ghp_super_secret_source", "ghp_super_secret_target"} {
		if strings.Contains(body, secret) {
			t.Fatalf("the browser surface served a migration source credential: %s", body)
		}
	}
	// And the same surface is closed to another tenant entirely.
	resp = s.get(t, "/ui-data/orgs/"+f.org.Login+"/migrations/sources", f.strangerToken)
	requireStatus(t, resp, 404)
	resp.Body.Close()
}

// lastRepositoryMigration is the most recently created repository migration of
// an organization — what a mutation that queued one just made.
func lastRepositoryMigration(t *testing.T, s *isolatedServer, orgID int) *store.RepositoryMigration {
	t.Helper()
	migrations := s.store.ListRepositoryMigrations(orgID)
	if len(migrations) == 0 {
		t.Fatal("the organization has no repository migrations")
	}
	return migrations[len(migrations)-1]
}

// TestRepositoryImportEmitsItsWebhook covers the one webhook these flows
// define. GitHub's repository_import event has no activity types — its
// discriminator is the status field — so the assertion is that the status
// carried is the outcome the import actually reached.
func TestRepositoryImportEmitsItsWebhook(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	source := s.createRepoWriteRepo(t, true)
	target := s.createRepoWriteRepo(t, false)

	var received atomic.Int32
	var payload atomic.Value
	sink, cleanup := startWebhookReceiver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GitHub-Event") == "repository_import" {
			body, _ := io.ReadAll(r.Body)
			payload.Store(string(body))
			received.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	})
	defer cleanup()

	resp := s.post(t, "/api/v3/repos/admin/"+target+"/hooks", defaultToken, map[string]interface{}{
		"config": map[string]interface{}{"url": sink + "/import-hook", "content_type": "json"},
		"events": []string{"repository_import"},
	})
	requireStatus(t, resp, 201)
	resp.Body.Close()

	resp = s.put(t, "/api/v3/repos/admin/"+target+"/import", defaultToken, map[string]interface{}{
		"vcs": "git", "vcs_url": s.baseURL + "/admin/" + source + ".git",
	})
	requireStatus(t, resp, 201)
	resp.Body.Close()

	waitForWebhookCount(t, &received, 1)
	body, _ := payload.Load().(string)
	if !strings.Contains(body, `"status":"success"`) {
		t.Fatalf("repository_import payload = %s, want a success status", body)
	}
	if !strings.Contains(body, target) {
		t.Fatalf("repository_import payload does not name the repository: %s", body)
	}
}

// TestGEIMigrationIngestsAnExportArchive closes the loop: an export migration
// produces an archive, and a GEI repository migration handed that archive's URL
// as its gitArchiveUrl and metadataArchiveUrl rebuilds the repository from it.
//
// That is what makes gitArchiveUrl real rather than a second spelling of the
// clone URL — the archive is unpacked (packfile + refs) rather than fetched.
func TestGEIMigrationIngestsAnExportArchive(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := s.newGEIFixture(t, "archive")

	sourceFull := "admin/" + f.sourceRepo
	sourceRepo := s.store.GetRepoByFullName(sourceFull)
	if sourceRepo == nil {
		t.Fatalf("the seeded source repository %s is missing", sourceFull)
	}
	s.store.CreateIssue(sourceRepo.ID, s.store.UsersByLogin["admin"].ID, "archived issue", "", nil, nil, 0)

	// Export the source through the documented export migration.
	resp := s.post(t, "/api/v3/user/migrations", defaultToken, map[string]interface{}{
		"repositories": []string{sourceFull},
	})
	export := decodeJSONWithStatus(t, resp, 201)
	exportID := int(export["id"].(float64))
	archiveURL := s.baseURL + "/api/v3/user/migrations/" + itoa(exportID) + "/archive"
	s.waitForMigrationExport(t, "/api/v3/user/migrations/"+itoa(exportID))

	// Migrate it back in from that archive alone — no clone URL is reachable.
	migration := s.store.CreateRepositoryMigration(store.NewRepositoryMigration{
		OwnerOrgID: f.org.ID, SourceID: f.source.ID, RepositoryName: "from-archive",
		SourceURL:          "https://unreachable.invalid/never-dialled.git",
		GitArchiveURL:      archiveURL,
		MetadataArchiveURL: archiveURL,
		ContinueOnError:    true,
		StartedByUserID:    f.owner.ID,
	})
	s.startGEIRepositoryMigration(migration.ID)
	waitFor(t, func() bool {
		m := s.store.GetRepositoryMigration(migration.ID)
		return m != nil && store.GEIMigrationTerminal(m.State)
	}, "the archive-driven migration never finished")

	done := s.store.GetRepositoryMigration(migration.ID)
	if done.State != store.GEIMigrationStateSucceeded {
		t.Fatalf("state = %s, failure = %q", done.State, done.FailureReason)
	}
	target := s.store.GetRepoByFullName(f.org.Login + "/from-archive")
	if target == nil {
		t.Fatal("the archive-driven migration created no target repository")
	}
	// The packfile and refs really landed: the source's README reads back
	// through the target's contents API.
	resp = s.get(t, "/api/v3/repos/"+target.FullName+"/contents/README.md", defaultToken)
	requireStatus(t, resp, 200)
	resp.Body.Close()
	// And the archive's metadata documents were applied.
	issues := s.store.ListIssues(target.ID, "all")
	if len(issues) != 1 || issues[0].Title != "archived issue" {
		t.Fatalf("migrated issues = %+v, want the archive's one issue", issues)
	}
}

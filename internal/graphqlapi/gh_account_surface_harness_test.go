package graphqlapi

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/gqlerrors"

	"github.com/e6qu/bleephub/internal/store"
)

// The account-surface tests execute real GraphQL documents against a seeded
// store, because what they are asserting is the wire answer a client receives
// — including the authorization outcome for a stranger — not the shape of an
// internal helper.

type accountViewerKey struct{}

// accountTestAuthz answers the resolver's authorization questions from the
// seeded store, so a test's expectations about who may see what are the
// store's real ownership and membership, not a stub's blanket yes or no.
type accountTestAuthz struct{ st *store.Store }

func (a accountTestAuthz) viewer(ctx context.Context) *store.User {
	user, _ := ctx.Value(accountViewerKey{}).(*store.User)
	return user
}

// ownsRepo reports whether the viewer is the repository's owner, an owner of
// the organization that owns it, or a collaborator on it.
func (a accountTestAuthz) ownsRepo(ctx context.Context, repo *store.Repo) bool {
	viewer := a.viewer(ctx)
	if viewer == nil || repo == nil {
		return false
	}
	if repo.OwnerType == "Organization" {
		owner, _, _ := store.SplitRepoFullName(repo.FullName)
		membership := a.st.GetMembership(owner, viewer.ID)
		return membership != nil && membership.Role == store.OrgRoleAdmin
	}
	return repo.OwnerID == viewer.ID
}

func (a accountTestAuthz) ViewerCanReadRepo(ctx context.Context, repo *store.Repo) bool {
	if repo == nil {
		return false
	}
	if !repo.Private {
		return true
	}
	if a.ownsRepo(ctx, repo) {
		return true
	}
	viewer := a.viewer(ctx)
	if viewer == nil {
		return false
	}
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return false
	}
	return a.st.GetRepoCollaboratorPermission(owner, name, viewer.Login) != ""
}

func (a accountTestAuthz) ViewerCanPushRepo(ctx context.Context, repo *store.Repo) bool {
	return a.ownsRepo(ctx, repo)
}

func (a accountTestAuthz) ViewerCanAdminRepo(ctx context.Context, repo *store.Repo) bool {
	return a.ownsRepo(ctx, repo)
}

func (a accountTestAuthz) ViewerHasRepoPermission(ctx context.Context, repo *store.Repo, _ store.PermScope, level store.PermLevel) bool {
	if level == store.PermRead {
		return a.ViewerCanReadRepo(ctx, repo)
	}
	return a.ownsRepo(ctx, repo)
}

func (a accountTestAuthz) ViewerMayActOnRepo(ctx context.Context, repo *store.Repo, _ store.PermScope, _, _ store.PermLevel) bool {
	return a.ownsRepo(ctx, repo)
}

func (a accountTestAuthz) CredentialGrantsRepo(context.Context, *store.Repo, store.PermScope, store.PermLevel) bool {
	return true
}

func (a accountTestAuthz) CredentialGrantsAccount(context.Context, store.AccountKind, string, store.PermScope, store.PermLevel) bool {
	return true
}

func (a accountTestAuthz) PrincipalHoldsRepoCapability(ctx context.Context, repo *store.Repo, _ store.PermLevel) bool {
	return a.ownsRepo(ctx, repo)
}

func (a accountTestAuthz) ViewerIsOrgMember(ctx context.Context, orgLogin string) bool {
	viewer := a.viewer(ctx)
	if viewer == nil {
		return false
	}
	membership := a.st.GetMembership(orgLogin, viewer.ID)
	return membership != nil && membership.State == store.MembershipStateActive
}

func (a accountTestAuthz) ViewerCanAdminAccount(ctx context.Context, login string) bool {
	viewer := a.viewer(ctx)
	if viewer == nil {
		return false
	}
	if viewer.Login == login || viewer.SiteAdmin {
		return true
	}
	membership := a.st.GetMembership(login, viewer.ID)
	return membership != nil && membership.Role == store.OrgRoleAdmin
}

func (a accountTestAuthz) ViewerMayMigrateOrg(ctx context.Context, org *store.Org) bool {
	return org != nil && a.ViewerCanAdminAccount(ctx, org.Login)
}

func (a accountTestAuthz) VisibleRepos(ctx context.Context, repos []*store.Repo) []*store.Repo {
	out := make([]*store.Repo, 0, len(repos))
	for _, repo := range repos {
		if a.ViewerCanReadRepo(ctx, repo) {
			out = append(out, repo)
		}
	}
	return out
}

func (a accountTestAuthz) CanReadProjectV2(context.Context, *store.User, *store.ProjectV2Owner, *store.ProjectV2) bool {
	return false
}

func (a accountTestAuthz) CanWriteProjectV2(context.Context, *store.User, *store.ProjectV2Owner) bool {
	return false
}

// accountHarness is a resolver over a seeded store whose viewer the test
// chooses per query.
type accountHarness struct {
	t     *testing.T
	store *store.Store
	res   *Resolver
}

func newAccountHarness(t *testing.T) *accountHarness {
	t.Helper()
	st := store.NewStore()
	st.SeedDefaultUser()
	res := NewResolver(Config{
		Store:      st,
		Authz:      accountTestAuthz{st: st},
		Events:     stubEvents{},
		Pulls:      stubPulls{},
		Migrations: stubMigrations{},
		UserFromContext: func(ctx context.Context) *store.User {
			user, _ := ctx.Value(accountViewerKey{}).(*store.User)
			return user
		},
		APIRate: func(context.Context) RateSnapshot { return RateSnapshot{} },
	})
	return &accountHarness{t: t, store: st, res: res}
}

// user adds an account to the store. The store has no exported user-creation
// entry point (the management endpoint composes the row itself), so the test
// harness composes the same row.
func (h *accountHarness) user(login string) *store.User {
	h.t.Helper()
	h.store.Mu.Lock()
	defer h.store.Mu.Unlock()
	if existing := h.store.UserByLoginLocked(login); existing != nil {
		return existing
	}
	id := h.store.ReserveGlobalID("next_user", &h.store.NextUser)
	now := h.store.CurrentTime()
	user := &store.User{
		ID:           id,
		NodeID:       "U_kgDO" + zeroPaddedID(id),
		Login:        login,
		Name:         login,
		Email:        login + "@bleephub.test",
		Type:         "User",
		StarredRepos: map[string]bool{},
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	h.store.Users[id] = user
	h.store.UsersByLogin[login] = user
	h.store.IndexUserLoginLocked(login)
	return user
}

func zeroPaddedID(id int) string {
	digits := strconv.Itoa(id)
	for len(digits) < 8 {
		digits = "0" + digits
	}
	return digits
}

// follow records that follower follows target. The follow graph has no
// exported writer (the REST follow routes mutate it in place), so the harness
// writes the same edge under the same lock.
func (h *accountHarness) follow(follower, target string) {
	h.t.Helper()
	h.store.Misc.Mu.Lock()
	defer h.store.Misc.Mu.Unlock()
	if h.store.Misc.Follows[follower] == nil {
		h.store.Misc.Follows[follower] = map[string]bool{}
	}
	h.store.Misc.Follows[follower][target] = true
}

// addSSHKey registers an SSH authentication key on an account, the way the
// REST /user/keys route composes the row.
func (h *accountHarness) addSSHKey(user *store.User, title, key string) *store.UserKey {
	h.t.Helper()
	h.store.Misc.Mu.Lock()
	defer h.store.Misc.Mu.Unlock()
	id := h.store.Misc.NextKeyID
	h.store.Misc.NextKeyID++
	row := &store.UserKey{
		ID: id, Title: title, Key: key, Verified: true, UserID: user.ID,
		CreatedAt: h.store.CurrentTime(),
	}
	if err := store.CacheParsedKey(row); err != nil {
		h.t.Fatalf("test key does not parse: %v", err)
	}
	h.store.Misc.UserKeys[id] = row
	h.store.Misc.KeysByUser[user.ID] = append(h.store.Misc.KeysByUser[user.ID], row)
	return row
}

// query executes a document as viewer (nil for an anonymous request) and
// fails the test on any GraphQL error, since every field here must answer
// rather than error.
func (h *accountHarness) query(viewer *store.User, document string, variables map[string]interface{}) map[string]interface{} {
	h.t.Helper()
	ctx := context.WithValue(context.Background(), accountViewerKey{}, viewer)
	result := graphql.Do(graphql.Params{
		Schema:         h.res.Schema(),
		RequestString:  document,
		VariableValues: variables,
		Context:        ctx,
	})
	if len(result.Errors) != 0 {
		h.t.Fatalf("graphql errors: %v", result.Errors)
	}
	body, err := json.Marshal(result.Data)
	if err != nil {
		h.t.Fatal(err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// queryWithErrors executes a document and returns the data alongside the
// GraphQL errors, for the cases where a refusal is the expected answer.
func (h *accountHarness) queryWithErrors(viewer *store.User, document string, variables map[string]interface{}) (map[string]interface{}, []gqlerrors.FormattedError) {
	h.t.Helper()
	ctx := context.WithValue(context.Background(), accountViewerKey{}, viewer)
	result := graphql.Do(graphql.Params{
		Schema:         h.res.Schema(),
		RequestString:  document,
		VariableValues: variables,
		Context:        ctx,
	})
	body, err := json.Marshal(result.Data)
	if err != nil {
		h.t.Fatal(err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		h.t.Fatal(err)
	}
	return out, result.Errors
}

// at walks a decoded GraphQL response by key path, failing on a missing or
// wrongly-typed member so a test reads as one assertion rather than five
// type switches.
func at(t *testing.T, data map[string]interface{}, path ...string) interface{} {
	t.Helper()
	var current interface{} = data
	for i, key := range path {
		object, ok := current.(map[string]interface{})
		if !ok {
			t.Fatalf("path %v: %q is not an object at depth %d (%T)", path, key, i, current)
		}
		current, ok = object[key]
		if !ok {
			t.Fatalf("path %v: %q is absent at depth %d", path, key, i)
		}
	}
	return current
}

// commitRepoFiles writes files onto the repository's default branch, so a
// git-content-backed field has real content to read.
func (h *accountHarness) commitRepoFiles(repo *store.Repo, files map[string]string) {
	h.t.Helper()
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		h.t.Fatalf("malformed repository name %q", repo.FullName)
	}
	stor := h.store.GetGitStorage(owner, name)
	if stor == nil {
		h.t.Fatalf("repository %s has no git storage", repo.FullName)
	}
	root := newTestTreeBuilder()
	for path, content := range files {
		hash, err := writeTestBlob(stor, content)
		if err != nil {
			h.t.Fatal(err)
		}
		root.add(path, hash)
	}
	treeHash, err := root.write(stor)
	if err != nil {
		h.t.Fatal(err)
	}
	// A fixed signature time keeps the commit deterministic; the store's test
	// clock, not the wall clock, is the suite's source of time.
	when := h.store.CurrentTime()
	commit := &object.Commit{
		Message:   "seed repository content",
		TreeHash:  treeHash,
		Author:    object.Signature{Name: owner, Email: owner + "@bleephub.test", When: when},
		Committer: object.Signature{Name: owner, Email: owner + "@bleephub.test", When: when},
	}
	encoded := stor.NewEncodedObject()
	if err := commit.Encode(encoded); err != nil {
		h.t.Fatal(err)
	}
	commitHash, err := stor.SetEncodedObject(encoded)
	if err != nil {
		h.t.Fatal(err)
	}
	ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(repo.DefaultBranch), commitHash)
	if err := stor.SetReference(ref); err != nil {
		h.t.Fatal(err)
	}
}

func writeTestBlob(stor gitStorage.Storer, content string) (plumbing.Hash, error) {
	encoded := stor.NewEncodedObject()
	encoded.SetType(plumbing.BlobObject)
	writer, err := encoded.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := writer.Write([]byte(content)); err != nil {
		_ = writer.Close()
		return plumbing.ZeroHash, err
	}
	if err := writer.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return stor.SetEncodedObject(encoded)
}

// testTreeBuilder assembles a nested git tree from path/blob pairs.
type testTreeBuilder struct {
	blobs   map[string]plumbing.Hash
	subdirs map[string]*testTreeBuilder
}

func newTestTreeBuilder() *testTreeBuilder {
	return &testTreeBuilder{blobs: map[string]plumbing.Hash{}, subdirs: map[string]*testTreeBuilder{}}
}

func (b *testTreeBuilder) add(path string, hash plumbing.Hash) {
	dir, rest, nested := cutFirstPathSegment(path)
	if !nested {
		b.blobs[path] = hash
		return
	}
	sub, ok := b.subdirs[dir]
	if !ok {
		sub = newTestTreeBuilder()
		b.subdirs[dir] = sub
	}
	sub.add(rest, hash)
}

func cutFirstPathSegment(path string) (head, rest string, nested bool) {
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			return path[:i], path[i+1:], true
		}
	}
	return path, "", false
}

func (b *testTreeBuilder) write(stor gitStorage.Storer) (plumbing.Hash, error) {
	tree := &object.Tree{}
	names := make([]string, 0, len(b.blobs)+len(b.subdirs))
	for name := range b.blobs {
		names = append(names, name)
	}
	for name := range b.subdirs {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		if hash, ok := b.blobs[name]; ok {
			tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: 0o100644, Hash: hash})
			continue
		}
		hash, err := b.subdirs[name].write(stor)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: 0o40000, Hash: hash})
	}
	encoded := stor.NewEncodedObject()
	if err := tree.Encode(encoded); err != nil {
		return plumbing.ZeroHash, err
	}
	return stor.SetEncodedObject(encoded)
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// fixedTestTime is the instant the account-surface tests set the store clock
// to. Tests must not read the wall clock (the test-clock ratchet), and a
// deterministic clock is also what makes an expiry assertion decidable.
var fixedTestTime = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

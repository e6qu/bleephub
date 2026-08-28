package graphqlapi

import (
	"context"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/e6qu/bleephub/internal/store"
)

// defaultBranchCommit reads the commit sha at the head of a repository's
// default branch, so a test can point a release tag at real content.
func defaultBranchCommit(t *testing.T, st *store.Store, repo *store.Repo) string {
	t.Helper()
	owner, name, _ := store.SplitRepoFullName(repo.FullName)
	stor := st.GetGitStorage(owner, name)
	if stor == nil {
		t.Fatalf("repository %s has no git storage", repo.FullName)
	}
	ref, err := stor.Reference(plumbing.NewBranchReferenceName(repo.DefaultBranch))
	if err != nil {
		t.Fatalf("default branch ref: %v", err)
	}
	return ref.Hash().String()
}

// TestReleaseAccountFieldsAreBackedByRealData exercises the residual Release
// members added on the account/actions surface: author, descriptionHTML,
// shortDescriptionHTML, repository, resourcePath, updatedAt, tag, tagCommit,
// mentions and releaseAssets. Every value asserted comes from the real release,
// user, git and asset stores.
func TestReleaseAccountFieldsAreBackedByRealData(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.store.UsersByLogin["admin"]
	mentioned := h.user("mona")
	repo := h.store.CreateRepo(owner, "shipit", "", false)
	if repo == nil {
		t.Fatal("repository not created")
	}
	h.commitRepoFiles(repo, map[string]string{"README.md": "hello"})
	sha := defaultBranchCommit(t, h.store, repo)

	// A real annotated-free tag reference pointing at the head commit, so
	// tag/tagCommit resolve out of git storage.
	ownerLogin, name, _ := store.SplitRepoFullName(repo.FullName)
	stor := h.store.GetGitStorage(ownerLogin, name)
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName("v1.0.0"), plumbing.NewHash(sha))); err != nil {
		t.Fatalf("tag ref: %v", err)
	}

	rel := h.store.Releases.Create(repo.ID, owner.ID, "v1.0.0", repo.DefaultBranch,
		"First release", "Ship it. Thanks @mona for the help.\n\nMore detail here.", false, false, false)
	if rel == nil {
		t.Fatal("release not created")
	}
	if _, err := h.store.Releases.CreateReleaseAsset(rel.ID, owner.ID, "binary.tar.gz", "linux build", "application/gzip", []byte("payload")); err != nil {
		t.Fatalf("asset: %v", err)
	}

	document := `query($owner:String!, $name:String!) {
	  repository(owner:$owner, name:$name) {
	    releases(first:5) {
	      nodes {
	        tagName
	        resourcePath
	        updatedAt
	        author { login }
	        descriptionHTML
	        shortDescriptionHTML(limit: 20)
	        repository { nameWithOwner }
	        tag { name }
	        tagCommit { oid messageHeadlineHTML messageBodyHTML committedViaWeb onBehalfOf { login } }
	        mentions(first:5) { totalCount nodes { login } }
	        releaseAssets(first:5) {
	          totalCount
	          nodes { name contentType size downloadUrl url uploadedBy { login } release { tagName } }
	        }
	      }
	    }
	  }
	}`
	variables := map[string]interface{}{"owner": "admin", "name": "shipit"}
	data := h.query(owner, document, variables)

	nodes, ok := at(t, data, "repository", "releases", "nodes").([]interface{})
	if !ok || len(nodes) != 1 {
		t.Fatalf("expected one release node, got %#v", at(t, data, "repository", "releases", "nodes"))
	}
	node := nodes[0].(map[string]interface{})

	if got := node["resourcePath"]; got != "/admin/shipit/releases/tag/v1.0.0" {
		t.Errorf("resourcePath = %v", got)
	}
	if got := at(t, node, "author", "login"); got != "admin" {
		t.Errorf("author.login = %v", got)
	}
	if got := node["descriptionHTML"]; got == nil || !strings.Contains(got.(string), "Ship it") {
		t.Errorf("descriptionHTML = %v", got)
	}
	if got := node["shortDescriptionHTML"]; got == nil || strings.Contains(got.(string), "More detail") {
		t.Errorf("shortDescriptionHTML should be the truncated headline, got %v", got)
	}
	if got := at(t, node, "repository", "nameWithOwner"); got != "admin/shipit" {
		t.Errorf("repository.nameWithOwner = %v", got)
	}
	if node["updatedAt"] == nil {
		t.Error("updatedAt is null")
	}
	if got := at(t, node, "tag", "name"); got != "v1.0.0" {
		t.Errorf("tag.name = %v", got)
	}
	if got := at(t, node, "tagCommit", "oid"); got != sha {
		t.Errorf("tagCommit.oid = %v, want %v", got, sha)
	}
	if got := at(t, node, "tagCommit", "committedViaWeb"); got != false {
		t.Errorf("committedViaWeb = %v, want false", got)
	}
	if got := at(t, node, "tagCommit", "messageHeadlineHTML"); got == nil || got.(string) == "" {
		t.Errorf("messageHeadlineHTML is empty")
	}
	if got := at(t, node, "tagCommit", "onBehalfOf"); got != nil {
		t.Errorf("onBehalfOf should be null, got %v", got)
	}

	// mentions resolves the real @mona account, and only accounts that exist.
	if got := at(t, node, "mentions", "totalCount"); got != float64(1) {
		t.Errorf("mentions.totalCount = %v, want 1", got)
	}
	mentionNodes := at(t, node, "mentions", "nodes").([]interface{})
	if len(mentionNodes) != 1 || mentionNodes[0].(map[string]interface{})["login"] != "mona" {
		t.Errorf("mentions.nodes = %#v", mentionNodes)
	}
	_ = mentioned

	if got := at(t, node, "releaseAssets", "totalCount"); got != float64(1) {
		t.Errorf("releaseAssets.totalCount = %v, want 1", got)
	}
	assetNodes := at(t, node, "releaseAssets", "nodes").([]interface{})
	if len(assetNodes) != 1 {
		t.Fatalf("expected one asset, got %#v", assetNodes)
	}
	asset := assetNodes[0].(map[string]interface{})
	if asset["name"] != "binary.tar.gz" {
		t.Errorf("asset.name = %v", asset["name"])
	}
	if asset["size"] != float64(len("payload")) {
		t.Errorf("asset.size = %v", asset["size"])
	}
	if at(t, asset, "uploadedBy", "login") != "admin" {
		t.Errorf("asset.uploadedBy.login = %v", at(t, asset, "uploadedBy", "login"))
	}
	if at(t, asset, "release", "tagName") != "v1.0.0" {
		t.Errorf("asset.release.tagName = %v", at(t, asset, "release", "tagName"))
	}
}

// TestCommitFromStatusRefusesAStranger is the authorization test for the
// commit-resolving members added on the status-rollup and release surface
// (StatusContext.commit, StatusCheckRollup.commit, Release.tagCommit). The
// resolver must refuse a stranger a private repository's commit graph on its
// own, not only because a root field did.
//
// It builds a bare Resolver (store + authz seams only) rather than the full
// schema, so the private-repo visibility gate — which returns before any git
// access — is asserted directly and in isolation from the rest of the type
// graph.
func TestCommitFromStatusRefusesAStranger(t *testing.T) {
	st := store.NewStore()
	st.SeedDefaultUser()
	owner := st.UsersByLogin["admin"]
	stranger := &store.User{ID: 999001, Login: "outsider"}
	repo := st.CreateRepo(owner, "sealed", "", true)
	if repo == nil {
		t.Fatal("repository not created")
	}
	res := &Resolver{store: st, authz: accountTestAuthz{st: st}}
	const sha = "0123456789012345678901234567890123456789"

	strangerCtx := context.WithValue(context.Background(), accountViewerKey{}, stranger)
	if got := res.commitSourceForRepoSHA(strangerCtx, repo.FullName, sha); got != nil {
		t.Fatalf("stranger resolved a private repository's commit: %#v", got)
	}

	// The owner clears the visibility gate; the object is absent from git
	// storage, so the resolver truthfully answers the minimal Commit source
	// (its oid) rather than nil.
	ownerCtx := context.WithValue(context.Background(), accountViewerKey{}, owner)
	got := res.commitSourceForRepoSHA(ownerCtx, repo.FullName, sha)
	if got == nil {
		t.Fatal("owner was refused their own private repository's commit")
	}
	src, ok := got.(map[string]interface{})
	if !ok || src["oid"] != sha {
		t.Fatalf("owner got an unexpected commit source: %#v", got)
	}
}

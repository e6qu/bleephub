package graphqlapi

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// testSSHKey is a real Ed25519 authorized-key line, so the fingerprint the
// PublicKey type reports is computed rather than asserted against a stub.
const testSSHKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIP1PbBFI3Ll5ZDzHqvVQGZI+WvQPMEHNvvB0/6MyTPuz account@bleephub.test"

// TestUserProfileFieldsResolveFromTheStoredRow pins the profile members
// against the account row PATCH /user writes.
func TestUserProfileFieldsResolveFromTheStoredRow(t *testing.T) {
	h := newAccountHarness(t)
	admin := h.store.UsersByLogin["admin"]
	hireable := true
	if h.store.UpdateUserProfile(admin.ID, func(u *store.User) {
		u.Company = "@bleephub"
		u.Location = "Porto"
		u.Blog = "https://admin.example.test"
		u.TwitterUsername = "adminhandle"
		u.Bio = "Runs **this** instance"
		u.Hireable = &hireable
	}) == nil {
		t.Fatal("profile not updated")
	}
	if !h.store.SetUserSocialAccounts(admin.ID, []string{"https://mastodon.example.test/@admin"}) {
		t.Fatal("social accounts not recorded")
	}
	h.addSSHKey(admin, "laptop", testSSHKey)

	document := `{
	  user(login:"admin") {
	    company companyHTML location websiteUrl twitterUsername bioHTML
	    isSiteAdmin isHireable isEmployee isViewer userViewType
	    viewerCanFollow viewerIsFollowing isFollowingViewer
	    publicKeys(first:5) { totalCount nodes { key fingerprint isReadOnly accessedAt } }
	    socialAccounts(first:5) { totalCount nodes { provider displayName url } }
	  }
	}`

	selfView := h.query(admin, document, nil)
	userData, _ := at(t, selfView, "user").(map[string]interface{})
	for field, want := range map[string]interface{}{
		"company":         "@bleephub",
		"location":        "Porto",
		"websiteUrl":      "https://admin.example.test",
		"twitterUsername": "adminhandle",
		"isSiteAdmin":     true,
		"isHireable":      true,
		"isEmployee":      true,
		"isViewer":        true,
		"userViewType":    "PRIVATE",
		// An account cannot follow itself.
		"viewerCanFollow":   false,
		"viewerIsFollowing": false,
		"isFollowingViewer": false,
	} {
		if got := userData[field]; got != want {
			t.Errorf("%s = %#v, want %#v", field, got, want)
		}
	}
	if html, _ := userData["bioHTML"].(string); html == "" || html == "Runs **this** instance" {
		t.Errorf("bioHTML = %q, want the rendered markdown", html)
	}
	if html, _ := userData["companyHTML"].(string); html == "" {
		t.Error("companyHTML is empty, want the rendered company")
	}

	if got := at(t, selfView, "user", "publicKeys", "totalCount"); got != float64(1) {
		t.Fatalf("publicKeys totalCount = %v, want 1", got)
	}
	keys := at(t, selfView, "user", "publicKeys", "nodes").([]interface{})
	key, _ := keys[0].(map[string]interface{})
	if key["key"] != testSSHKey {
		t.Errorf("publicKey key = %v", key["key"])
	}
	fingerprint, _ := key["fingerprint"].(string)
	if len(fingerprint) < 10 || fingerprint[:7] != "SHA256:" {
		t.Errorf("publicKey fingerprint = %q, want the SHA-256 fingerprint of the key", fingerprint)
	}
	if key["accessedAt"] != nil {
		t.Errorf("publicKey accessedAt = %#v, want null (no last-use instant is recorded)", key["accessedAt"])
	}

	accounts := at(t, selfView, "user", "socialAccounts", "nodes").([]interface{})
	if len(accounts) != 1 {
		t.Fatalf("socialAccounts = %#v, want one", accounts)
	}
	account, _ := accounts[0].(map[string]interface{})
	if account["provider"] != "MASTODON" || account["displayName"] != "@admin" {
		t.Errorf("social account = %#v", account)
	}

	// Another account sees the public view of the same profile.
	otherView := h.query(h.user("visitor"), document, nil)
	if got := at(t, otherView, "user", "userViewType"); got != "PUBLIC" {
		t.Errorf("visitor userViewType = %v, want PUBLIC", got)
	}
	if got := at(t, otherView, "user", "isViewer"); got != false {
		t.Errorf("visitor isViewer = %v, want false", got)
	}
	if got := at(t, otherView, "user", "viewerCanFollow"); got != true {
		t.Errorf("visitor viewerCanFollow = %v, want true", got)
	}
}

// TestUserFollowGraphReportsBothDirections pins followers / following and the
// two viewer-relative follow members against the follow graph the REST follow
// routes write.
func TestUserFollowGraphReportsBothDirections(t *testing.T) {
	h := newAccountHarness(t)
	fan := h.user("fan")
	h.user("idol")
	h.follow("fan", "admin")
	h.follow("admin", "idol")

	data := h.query(fan, `{
	  user(login:"admin") {
	    followers(first:10) { totalCount nodes { login } }
	    following(first:10) { totalCount nodes { login } }
	    viewerIsFollowing
	    isFollowingViewer
	  }
	}`, nil)

	if got := at(t, data, "user", "followers", "totalCount"); got != float64(1) {
		t.Errorf("followers totalCount = %v, want 1", got)
	}
	followers := at(t, data, "user", "followers", "nodes").([]interface{})
	if node, _ := followers[0].(map[string]interface{}); node["login"] != "fan" {
		t.Errorf("follower = %#v, want fan", node)
	}
	following := at(t, data, "user", "following", "nodes").([]interface{})
	if node, _ := following[0].(map[string]interface{}); node["login"] != "idol" {
		t.Errorf("following = %#v, want idol", node)
	}
	if got := at(t, data, "user", "viewerIsFollowing"); got != true {
		t.Errorf("viewerIsFollowing = %v, want true (fan follows admin)", got)
	}
	if got := at(t, data, "user", "isFollowingViewer"); got != false {
		t.Errorf("isFollowingViewer = %v, want false (admin does not follow fan)", got)
	}
}

// TestUserAuthoredConnectionsHidePrivateRepositoryContent is the
// authorization test for the account-wide connections: an issue, pull request
// or comment in a repository the request cannot read must not surface through
// its author's profile.
func TestUserAuthoredConnectionsHidePrivateRepositoryContent(t *testing.T) {
	h := newAccountHarness(t)
	author := h.store.UsersByLogin["admin"]
	public := h.store.CreateRepo(author, "open", "", false)
	private := h.store.CreateRepo(author, "closed", "", true)
	if public == nil || private == nil {
		t.Fatal("repositories not created")
	}
	if h.store.CreateIssue(public.ID, author.ID, "public issue", "", nil, nil, 0) == nil {
		t.Fatal("public issue not created")
	}
	if h.store.CreateIssue(private.ID, author.ID, "private issue", "", nil, nil, 0) == nil {
		t.Fatal("private issue not created")
	}
	if h.store.CommitComments.Create(private.ID, "cafebabe", author.ID, "secret note", "", nil, nil) == nil {
		t.Fatal("private commit comment not created")
	}

	document := `{
	  user(login:"admin") {
	    issues(first:20) { totalCount nodes { title } }
	    commitComments(first:20) { totalCount }
	    repositoriesContributedTo(first:20) { totalCount nodes { nameWithOwner } }
	  }
	}`

	ownerView := h.query(author, document, nil)
	if got := at(t, ownerView, "user", "issues", "totalCount"); got != float64(2) {
		t.Errorf("owner issues totalCount = %v, want both issues", got)
	}
	if got := at(t, ownerView, "user", "commitComments", "totalCount"); got != float64(1) {
		t.Errorf("owner commitComments totalCount = %v, want 1", got)
	}

	strangerView := h.query(h.user("reader"), document, nil)
	titles := []string{}
	for _, raw := range at(t, strangerView, "user", "issues", "nodes").([]interface{}) {
		node, _ := raw.(map[string]interface{})
		title, _ := node["title"].(string)
		titles = append(titles, title)
	}
	if len(titles) != 1 || titles[0] != "public issue" {
		t.Errorf("stranger issues = %#v, want only the public one", titles)
	}
	if got := at(t, strangerView, "user", "commitComments", "totalCount"); got != float64(0) {
		t.Errorf("stranger commitComments totalCount = %v, want 0", got)
	}
	contributed := []string{}
	for _, raw := range at(t, strangerView, "user", "repositoriesContributedTo", "nodes").([]interface{}) {
		node, _ := raw.(map[string]interface{})
		name, _ := node["nameWithOwner"].(string)
		contributed = append(contributed, name)
	}
	if len(contributed) != 1 || contributed[0] != "admin/open" {
		t.Errorf("stranger repositoriesContributedTo = %#v, want only the public repository", contributed)
	}
}

// TestUserGistByNameKeepsSecretGistsSecret pins User.gist against the same
// rule the gists connection enforces: a secret gist is readable only by the
// account that owns it.
func TestUserGistByNameKeepsSecretGistsSecret(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.store.UsersByLogin["admin"]
	secret, err := h.store.CreateGistE(owner, "notes", false, map[string]*store.GistFile{
		"notes.md": {Filename: "notes.md", Content: "private"},
	})
	if err != nil || secret == nil {
		t.Fatalf("secret gist not created: %v", err)
	}
	public, err := h.store.CreateGistE(owner, "shared", true, map[string]*store.GistFile{
		"shared.md": {Filename: "shared.md", Content: "public"},
	})
	if err != nil || public == nil {
		t.Fatalf("public gist not created: %v", err)
	}

	document := `query($name:String!){ user(login:"admin") { gist(name:$name) { name description } } }`

	if got := at(t, h.query(owner, document, map[string]interface{}{"name": secret.ID}),
		"user", "gist", "description"); got != "notes" {
		t.Errorf("owner cannot read their own secret gist: %v", got)
	}
	strangerView := h.query(h.user("gistreader"), document, map[string]interface{}{"name": secret.ID})
	userData, _ := at(t, strangerView, "user").(map[string]interface{})
	if userData["gist"] != nil {
		t.Errorf("stranger read a secret gist = %#v", userData["gist"])
	}
	if got := at(t, h.query(h.user("gistreader"), document, map[string]interface{}{"name": public.ID}),
		"user", "gist", "description"); got != "shared" {
		t.Errorf("stranger cannot read the public gist: %v", got)
	}
}

// TestUserOrganizationLookupHidesPrivateMembership pins User.organization: a
// membership nobody publicized is not something a stranger can confirm.
func TestUserOrganizationLookupHidesPrivateMembership(t *testing.T) {
	h := newAccountHarness(t)
	admin := h.store.UsersByLogin["admin"]
	member := h.user("quietmember")
	if h.store.CreateOrg(admin, "lookuporg", "Lookup Org", "") == nil {
		t.Fatal("organization not created")
	}
	h.store.SetMembership("lookuporg", member.ID, store.OrgRoleMember, store.MembershipStateActive)

	document := `{ user(login:"quietmember") { organization(login:"lookuporg") { login } } }`

	if got := at(t, h.query(member, document, nil), "user", "organization", "login"); got != "lookuporg" {
		t.Errorf("the member cannot see their own membership: %v", got)
	}
	if got := at(t, h.query(admin, document, nil), "user", "organization", "login"); got != "lookuporg" {
		t.Errorf("an organization member cannot see a fellow member: %v", got)
	}
	strangerView := h.query(h.user("orgstranger"), document, nil)
	userData, _ := at(t, strangerView, "user").(map[string]interface{})
	if userData["organization"] != nil {
		t.Errorf("stranger confirmed a private membership = %#v", userData["organization"])
	}

	// Once publicized, anyone may confirm it.
	if !h.store.SetMembershipPublic("lookuporg", member.ID, true) {
		t.Fatal("membership not publicized")
	}
	if got := at(t, h.query(h.user("orgstranger"), document, nil), "user", "organization", "login"); got != "lookuporg" {
		t.Errorf("a publicized membership is not visible: %v", got)
	}
}

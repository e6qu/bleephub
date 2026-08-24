package graphqlapi

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// The comment-trait fields (bodyHTML/bodyText, createdViaEmail, the viewerCan*
// family, the repository/parent back-references) are exercised through real
// GraphQL documents against a seeded store, because what matters is the wire
// answer a client — including a stranger to a private repository — receives.

// TestIssueCommentTraitFieldsBackingAndAuthorization checks the IssueComment
// trait fields carry real data for a permitted viewer, answer from real
// permission for an unprivileged one, and leak nothing to a stranger of a
// private repository.
func TestIssueCommentTraitFieldsBackingAndAuthorization(t *testing.T) {
	h := newAccountHarness(t)
	author := h.user("amelia")
	stranger := h.user("stan")

	// --- a private repository: a stranger must get nothing --------------------
	priv := h.store.CreateRepo(author, "sealed", "", true)
	privIssue := h.store.CreateIssue(priv.ID, author.ID, "sealed issue", "", nil, nil, 0)
	if privIssue == nil {
		t.Fatal("private issue not created")
	}
	if h.store.CreateComment(privIssue.ID, author.ID, "secret plans") == nil {
		t.Fatal("private comment not created")
	}

	doc := `query($owner:String!,$name:String!,$number:Int!){
	  repository(owner:$owner,name:$name){
	    issue(number:$number){
	      comments(first:5){ nodes {
	        body bodyHTML bodyText createdViaEmail publishedAt
	        url resourcePath
	        viewerDidAuthor viewerCanUpdate viewerCanDelete
	        viewerCanMinimize viewerCanPin viewerCannotUpdateReasons
	        repository { nameWithOwner }
	      } }
	    }
	  }
	}`
	privVars := map[string]interface{}{"owner": "amelia", "name": "sealed", "number": privIssue.Number}

	// The author sees the real backing data.
	authorView := h.query(author, doc, privVars)
	node := firstCommentNode(t, authorView)
	if node["createdViaEmail"] != false {
		t.Errorf("createdViaEmail = %v, want false (bleephub has no email-reply path)", node["createdViaEmail"])
	}
	if node["bodyHTML"] == "" || node["bodyHTML"] == nil {
		t.Errorf("bodyHTML empty, want rendered HTML")
	}
	if node["bodyText"] != "secret plans" {
		t.Errorf("bodyText = %v, want %q", node["bodyText"], "secret plans")
	}
	if node["publishedAt"] == nil {
		t.Errorf("publishedAt is null, want createdAt for a published comment")
	}
	if node["viewerDidAuthor"] != true {
		t.Errorf("author viewerDidAuthor = %v, want true", node["viewerDidAuthor"])
	}
	if node["viewerCanUpdate"] != true {
		t.Errorf("author viewerCanUpdate = %v, want true", node["viewerCanUpdate"])
	}
	if reasons := node["viewerCannotUpdateReasons"].([]interface{}); len(reasons) != 0 {
		t.Errorf("author viewerCannotUpdateReasons = %v, want empty", reasons)
	}
	if got := stringAt(t, node, "repository", "nameWithOwner"); got != "amelia/sealed" {
		t.Errorf("repository.nameWithOwner = %q, want amelia/sealed", got)
	}
	if url, _ := node["url"].(string); url == "" {
		t.Error("url empty, want a permalink")
	}

	// The stranger is refused the repository entirely — no comment field leaks.
	strangerData, errs := h.queryWithErrors(stranger, doc, privVars)
	if len(errs) == 0 {
		t.Error("a stranger's query for a private repo's comment succeeded; want a NOT_FOUND refusal")
	}
	if repo := strangerData["repository"]; repo != nil {
		t.Errorf("stranger saw repository = %v, want null", repo)
	}

	// --- a public repository: fields answer from real permission --------------
	pub := h.store.CreateRepo(author, "open", "", false)
	pubIssue := h.store.CreateIssue(pub.ID, author.ID, "open issue", "", nil, nil, 0)
	if pubIssue == nil {
		t.Fatal("public issue not created")
	}
	if h.store.CreateComment(pubIssue.ID, author.ID, "hello") == nil {
		t.Fatal("public comment not created")
	}
	pubVars := map[string]interface{}{"owner": "amelia", "name": "open", "number": pubIssue.Number}

	// A signed-in non-author who can read but not write: cannot update.
	otherView := h.query(stranger, doc, pubVars)
	on := firstCommentNode(t, otherView)
	if on["viewerDidAuthor"] != false {
		t.Errorf("non-author viewerDidAuthor = %v, want false", on["viewerDidAuthor"])
	}
	if on["viewerCanUpdate"] != false {
		t.Errorf("non-author viewerCanUpdate = %v, want false", on["viewerCanUpdate"])
	}
	if reasons := on["viewerCannotUpdateReasons"].([]interface{}); len(reasons) != 1 || reasons[0] != "INSUFFICIENT_ACCESS" {
		t.Errorf("non-author viewerCannotUpdateReasons = %v, want [INSUFFICIENT_ACCESS]", reasons)
	}

	// An anonymous viewer: the reason is LOGIN_REQUIRED.
	anonView := h.query(nil, doc, pubVars)
	an := firstCommentNode(t, anonView)
	if an["viewerCanUpdate"] != false {
		t.Errorf("anonymous viewerCanUpdate = %v, want false", an["viewerCanUpdate"])
	}
	if reasons := an["viewerCannotUpdateReasons"].([]interface{}); len(reasons) != 1 || reasons[0] != "LOGIN_REQUIRED" {
		t.Errorf("anonymous viewerCannotUpdateReasons = %v, want [LOGIN_REQUIRED]", reasons)
	}
}

// TestCommitCommentTraitFieldsBacking checks a commit comment's trait fields
// resolve real data through Repository.commitComments.
func TestCommitCommentTraitFieldsBacking(t *testing.T) {
	h := newAccountHarness(t)
	author := h.user("cora")
	repo := h.store.CreateRepo(author, "code", "", false)
	if h.store.CommitComments.Create(repo.ID, "deadbeef", author.ID, "nice line", "main.go", nil, nil) == nil {
		t.Fatal("commit comment not created")
	}

	doc := `{
	  repository(owner:"cora",name:"code"){
	    commitComments(first:5){ nodes {
	      body bodyHTML bodyText createdViaEmail authorAssociation
	      url resourcePath updatedAt
	      viewerDidAuthor viewerCanUpdate viewerCannotUpdateReasons
	      repository { nameWithOwner }
	    } }
	  }
	}`
	view := h.query(author, doc, nil)
	node := firstNodeAt(t, view, "repository", "commitComments")
	if node["createdViaEmail"] != false {
		t.Errorf("createdViaEmail = %v, want false", node["createdViaEmail"])
	}
	if node["bodyText"] != "nice line" {
		t.Errorf("bodyText = %v, want %q", node["bodyText"], "nice line")
	}
	if node["authorAssociation"] != "OWNER" {
		t.Errorf("authorAssociation = %v, want OWNER (author owns the repo)", node["authorAssociation"])
	}
	if node["viewerDidAuthor"] != true || node["viewerCanUpdate"] != true {
		t.Errorf("owner-author viewerDidAuthor/viewerCanUpdate = %v/%v, want true/true", node["viewerDidAuthor"], node["viewerCanUpdate"])
	}
	if url, _ := node["url"].(string); url == "" {
		t.Error("commit comment url empty")
	}
}

// TestGistCommentTraitFieldsBacking checks a gist comment's trait fields resolve
// real data — including the gist back-reference — through User.gistComments.
func TestGistCommentTraitFieldsBacking(t *testing.T) {
	h := newAccountHarness(t)
	author := h.user("gina")
	gist, err := h.store.CreateGistE(author, "snippet", true, map[string]*store.GistFile{
		"a.txt": {Filename: "a.txt", Content: "x"},
	})
	if err != nil || gist == nil {
		t.Fatalf("gist not created: %v", err)
	}
	if h.store.CreateGistComment(gist.ID, author, "good gist") == nil {
		t.Fatal("gist comment not created")
	}

	doc := `{
	  user(login:"gina"){
	    gistComments(first:5){ nodes {
	      body bodyHTML bodyText createdViaEmail authorAssociation databaseId
	      viewerDidAuthor viewerCanUpdate viewerCannotUpdateReasons
	      gist { name }
	    } }
	  }
	}`
	view := h.query(author, doc, nil)
	node := firstNodeAt(t, view, "user", "gistComments")
	if node["createdViaEmail"] != false {
		t.Errorf("createdViaEmail = %v, want false", node["createdViaEmail"])
	}
	if node["bodyText"] != "good gist" {
		t.Errorf("bodyText = %v, want %q", node["bodyText"], "good gist")
	}
	if node["authorAssociation"] != "OWNER" {
		t.Errorf("authorAssociation = %v, want OWNER", node["authorAssociation"])
	}
	if node["viewerDidAuthor"] != true || node["viewerCanUpdate"] != true {
		t.Errorf("owner viewerDidAuthor/viewerCanUpdate = %v/%v, want true/true", node["viewerDidAuthor"], node["viewerCanUpdate"])
	}
	if got := stringAt(t, node, "gist", "name"); got != gist.ID {
		t.Errorf("gist.name = %q, want %q", got, gist.ID)
	}

	// A stranger who can see the public gist is not its author and cannot update.
	stranger := h.user("stella")
	strangerView := h.query(stranger, doc, nil)
	sn := firstNodeAt(t, strangerView, "user", "gistComments")
	if sn["viewerDidAuthor"] != false || sn["viewerCanUpdate"] != false {
		t.Errorf("stranger viewerDidAuthor/viewerCanUpdate = %v/%v, want false/false", sn["viewerDidAuthor"], sn["viewerCanUpdate"])
	}
}

// firstCommentNode returns the first node of repository.issue.comments.
func firstCommentNode(t *testing.T, data map[string]interface{}) map[string]interface{} {
	t.Helper()
	conn := at(t, data, "repository", "issue", "comments")
	return firstNode(t, conn)
}

// firstNodeAt returns the first node of the connection at the given path.
func firstNodeAt(t *testing.T, data map[string]interface{}, path ...string) map[string]interface{} {
	t.Helper()
	conn := at(t, data, path...)
	return firstNode(t, conn)
}

func firstNode(t *testing.T, conn interface{}) map[string]interface{} {
	t.Helper()
	m, ok := conn.(map[string]interface{})
	if !ok {
		t.Fatalf("connection is not an object: %T", conn)
	}
	nodes, ok := m["nodes"].([]interface{})
	if !ok || len(nodes) == 0 {
		t.Fatalf("connection has no nodes: %v", m)
	}
	node, ok := nodes[0].(map[string]interface{})
	if !ok {
		t.Fatalf("node is not an object: %T", nodes[0])
	}
	return node
}

func stringAt(t *testing.T, data map[string]interface{}, path ...string) string {
	t.Helper()
	v := at(t, data, path...)
	s, ok := v.(string)
	if !ok {
		t.Fatalf("value at %v is not a string: %T", path, v)
	}
	return s
}

package graphqlapi

import "testing"

// TestPullRequestSurfaceFieldsAuthorization proves the pull-request surface
// fields (gh_pulls_fields_graphql.go) neither leak a private PR to a stranger
// nor grant a viewer more standing than they hold: the private repository
// refuses at its boundary, and on a public PR the viewerCan* family answers
// from the viewer's real permissions rather than a blanket true.
func TestPullRequestSurfaceFieldsAuthorization(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.store.UsersByLogin["admin"]
	stranger := h.user("stranger")

	doc := `query($o:String!,$n:String!,$num:Int!){
		repository(owner:$o,name:$n){
			pullRequest(number:$num){
				bodyText
				viewerCanUpdate
				viewerDidAuthor
				viewerCanMergeAsAdmin
			}
		}
	}`

	// --- private repository: the stranger is refused at the repository
	// boundary, so the PR and every new field are unreachable. ---
	priv := h.store.CreateRepo(owner, "secret", "", true)
	if priv == nil {
		t.Fatal("private repo not created")
	}
	h.commitRepoFiles(priv, map[string]string{"README.md": "secret"})
	privPR := h.store.CreatePullRequest(priv.ID, owner.ID, "Secret work", "confidential body", priv.DefaultBranch, priv.DefaultBranch, false, nil, nil, 0)
	if privPR == nil {
		t.Fatal("private PR not created")
	}
	privVars := map[string]interface{}{"o": owner.Login, "n": "secret", "num": privPR.Number}

	ownerPriv := digPullRequest(t, h.query(owner, doc, privVars))
	if ownerPriv["bodyText"] != "confidential body" {
		t.Fatalf("owner bodyText = %v, want the real body", ownerPriv["bodyText"])
	}
	assertPRBool(t, ownerPriv, "viewerCanUpdate", true)
	assertPRBool(t, ownerPriv, "viewerDidAuthor", true)
	assertPRBool(t, ownerPriv, "viewerCanMergeAsAdmin", true)

	strangerPriv, _ := h.queryWithErrors(stranger, doc, privVars)
	if repo := strangerPriv["repository"]; repo != nil {
		t.Fatalf("private repository leaked to a stranger through the pull-request surface: %v", repo)
	}

	// --- public repository: fields are readable, but viewerCan* still reflect
	// the stranger's real (absent) write/admin/authorship standing. ---
	pub := h.store.CreateRepo(owner, "open", "", false)
	if pub == nil {
		t.Fatal("public repo not created")
	}
	// The PR's head and base branches have to exist before the store will
	// open it — the private case above seeded its branch through
	// commitRepoFiles and used it for both ends; do the same here rather than
	// naming branches that were never created.
	h.commitRepoFiles(pub, map[string]string{"README.md": "open"})
	pubPR := h.store.CreatePullRequest(pub.ID, owner.ID, "Open work", "public body", pub.DefaultBranch, pub.DefaultBranch, false, nil, nil, 0)
	if pubPR == nil {
		t.Fatal("public PR not created")
	}
	pubVars := map[string]interface{}{"o": owner.Login, "n": "open", "num": pubPR.Number}

	strangerPub := digPullRequest(t, h.query(stranger, doc, pubVars))
	if strangerPub["bodyText"] != "public body" {
		t.Fatalf("stranger bodyText on public PR = %v, want the real body", strangerPub["bodyText"])
	}
	assertPRBool(t, strangerPub, "viewerCanUpdate", false)
	assertPRBool(t, strangerPub, "viewerDidAuthor", false)
	assertPRBool(t, strangerPub, "viewerCanMergeAsAdmin", false)

	ownerPub := digPullRequest(t, h.query(owner, doc, pubVars))
	assertPRBool(t, ownerPub, "viewerCanUpdate", true)
	assertPRBool(t, ownerPub, "viewerDidAuthor", true)
}

func digPullRequest(t *testing.T, data map[string]interface{}) map[string]interface{} {
	t.Helper()
	repo, ok := data["repository"].(map[string]interface{})
	if !ok {
		t.Fatalf("repository absent from response: %v", data)
	}
	pr, ok := repo["pullRequest"].(map[string]interface{})
	if !ok {
		t.Fatalf("pullRequest absent from response: %v", repo)
	}
	return pr
}

func assertPRBool(t *testing.T, pr map[string]interface{}, key string, want bool) {
	t.Helper()
	got, ok := pr[key].(bool)
	if !ok {
		t.Fatalf("%s is not a bool: %v", key, pr[key])
	}
	if got != want {
		t.Fatalf("%s = %v, want %v", key, got, want)
	}
}

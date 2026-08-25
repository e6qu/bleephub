package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
)

// The GraphQL surface the official `gh` command line drives, pinned by the
// selections gh itself sends. Every case here corresponds to an operation the
// SDK/CLI conformance harness (test/conformance) runs against a live server:
// `gh pr view --json commits`, `gh pr checks`, `gh issue edit --add-label` and
// `gh gist list`. They are Go tests as well as harness rows because the harness
// is slow and only reports the first failing operation of a command.

// ghConformanceQuery posts one document and returns the decoded envelope
// without asserting anything about errors — the null-user case has to be able
// to look at them.
func (s *isolatedServer) ghConformanceQuery(t *testing.T, token, query string, variables map[string]interface{}) map[string]interface{} {
	t.Helper()
	body := map[string]interface{}{"query": query}
	if variables != nil {
		body["variables"] = variables
	}
	return decodeJSONWithStatus(t, s.post(t, "/api/graphql", token, body), 200)
}

// ghConformanceData runs a query and fails on any GraphQL error, returning the
// data map.
func (s *isolatedServer) ghConformanceData(t *testing.T, token, query string, variables map[string]interface{}) map[string]interface{} {
	t.Helper()
	envelope := s.ghConformanceQuery(t, token, query, variables)
	if errs := envelope["errors"]; errs != nil {
		t.Fatalf("GraphQL errors = %v", errs)
	}
	data, _ := envelope["data"].(map[string]interface{})
	if data == nil {
		t.Fatalf("no data in %v", envelope)
	}
	return data
}

// TestGraphQLCommitAuthorsUserIsNullForAnUnknownEmail covers the failure that
// broke `gh pr view --json commits` outright: a commit signed by an address no
// account owns was rendered with a User shell rather than no user at all, and
// GraphQL then failed the entire query on the non-null User.id rather than
// answering the null GitHub answers.
func TestGraphQLCommitAuthorsUserIsNullForAnUnknownEmail(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := s.createRepoWriteRepo(t, false)
	repo := s.store.GetRepo("admin", name)
	if repo == nil {
		t.Fatal("fixture repository is missing")
	}
	stor := s.store.GetGitStorage("admin", name)
	if stor == nil {
		t.Fatal("fixture repository has no git storage")
	}
	// A signature deliberately owned by nobody: this is what a contributor's
	// laptop-configured git identity looks like to a server that has never
	// seen the address.
	stranger := repoSignature("Nobody At All", "nobody@example.invalid")
	base, err := initRepoWithFiles(stor, "main", "initial commit", map[string]string{"README.md": "base\n"}, stranger)
	if err != nil {
		t.Fatalf("seed base commit: %v", err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), base)); err != nil {
		t.Fatalf("seed feature branch: %v", err)
	}
	if _, err := createFileCommit(stor, "feature", "feature.txt", "feature\n", "add feature", stranger); err != nil {
		t.Fatalf("seed feature commit: %v", err)
	}
	admin := s.store.LookupUserByLogin("admin")
	if admin == nil {
		t.Fatal("the seeded admin is missing")
	}
	pr := s.store.CreatePullRequest(repo.ID, admin.ID, "conformance", "body", "feature", "main", false, nil, nil, 0)
	if pr == nil {
		t.Fatal("the fixture pull request was refused")
	}

	// The selection gh sends for `gh pr view --json commits`.
	data := s.ghConformanceData(t, defaultToken, `query($owner:String!,$name:String!,$number:Int!){
		repository(owner:$owner,name:$name){
			pullRequest(number:$number){
				commits(first:100){
					nodes{
						commit{
							oid
							messageHeadline
							authors(first:10){nodes{name email user{id login}}}
						}
					}
				}
			}
		}
	}`, map[string]interface{}{"owner": "admin", "name": name, "number": pr.Number})

	nodes := prCommitNodes(t, data)
	if len(nodes) == 0 {
		t.Fatalf("no commits came back: %v", data)
	}
	sawNullUser := false
	for _, raw := range nodes {
		node, _ := raw.(map[string]interface{})
		commit, _ := node["commit"].(map[string]interface{})
		authors, _ := commit["authors"].(map[string]interface{})
		authorNodes, _ := authors["nodes"].([]interface{})
		if len(authorNodes) == 0 {
			t.Fatalf("a commit came back with no authors: %v", commit)
		}
		for _, rawAuthor := range authorNodes {
			author, _ := rawAuthor.(map[string]interface{})
			if author["email"] != "nobody@example.invalid" {
				t.Fatalf("author email = %v, want the stranger's address", author["email"])
			}
			user, present := author["user"]
			if !present {
				t.Fatalf("GitActor.user was not selected back: %v", author)
			}
			if user != nil {
				t.Fatalf("GitActor.user = %v for an address no account owns, want null", user)
			}
			sawNullUser = true
		}
	}
	if !sawNullUser {
		t.Fatal("the query never reached a commit author")
	}
}

// TestGraphQLCommitAuthorsUserResolvesAKnownEmail is the other half: the null
// must be the answer for an unknown address only, not for every commit.
func TestGraphQLCommitAuthorsUserResolvesAKnownEmail(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	name := s.createRepoWriteRepo(t, false)
	repo := s.store.GetRepo("admin", name)
	admin := s.store.LookupUserByLogin("admin")
	if repo == nil || admin == nil {
		t.Fatal("the fixture repository or the seeded admin is missing")
	}
	if admin.Email == "" {
		t.Fatal("the seeded admin has no email to sign a commit with")
	}
	stor := s.store.GetGitStorage("admin", name)
	sig := repoSignature(admin.Login, admin.Email)
	base, err := initRepoWithFiles(stor, "main", "initial commit", map[string]string{"README.md": "base\n"}, sig)
	if err != nil {
		t.Fatalf("seed base commit: %v", err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), base)); err != nil {
		t.Fatalf("seed feature branch: %v", err)
	}
	if _, err := createFileCommit(stor, "feature", "feature.txt", "feature\n", "add feature", sig); err != nil {
		t.Fatalf("seed feature commit: %v", err)
	}
	pr := s.store.CreatePullRequest(repo.ID, admin.ID, "conformance", "body", "feature", "main", false, nil, nil, 0)
	if pr == nil {
		t.Fatal("the fixture pull request was refused")
	}

	data := s.ghConformanceData(t, defaultToken, `query($owner:String!,$name:String!,$number:Int!){
		repository(owner:$owner,name:$name){
			pullRequest(number:$number){
				commits(first:100){nodes{commit{authors(first:10){nodes{user{login}}}}}}
			}
		}
	}`, map[string]interface{}{"owner": "admin", "name": name, "number": pr.Number})

	nodes := prCommitNodes(t, data)
	if len(nodes) == 0 {
		t.Fatalf("no commits came back: %v", data)
	}
	for _, raw := range nodes {
		node, _ := raw.(map[string]interface{})
		commit, _ := node["commit"].(map[string]interface{})
		authors, _ := commit["authors"].(map[string]interface{})
		authorNodes, _ := authors["nodes"].([]interface{})
		for _, rawAuthor := range authorNodes {
			author, _ := rawAuthor.(map[string]interface{})
			user, _ := author["user"].(map[string]interface{})
			if user == nil || user["login"] != admin.Login {
				t.Fatalf("GitActor.user = %v, want the admin account", author["user"])
			}
		}
	}
}

// prCommitNodes digs the commits connection's nodes out of a repository
// pullRequest response.
func prCommitNodes(t *testing.T, data map[string]interface{}) []interface{} {
	t.Helper()
	repository, _ := data["repository"].(map[string]interface{})
	pullRequest, _ := repository["pullRequest"].(map[string]interface{})
	if pullRequest == nil {
		t.Fatalf("pullRequest resolved to null: %v", data)
	}
	commits, _ := pullRequest["commits"].(map[string]interface{})
	nodes, _ := commits["nodes"].([]interface{})
	return nodes
}

// ghConformanceRepo is a repository with an issue and a pull request, the
// subjects `gh issue edit` and `gh pr checks` address.
type ghConformanceRepo struct {
	repo  *store.Repo
	name  string
	issue *store.Issue
	pr    *store.PullRequest
	head  string
	admin *store.User
}

func newGHConformanceRepo(t *testing.T, s *isolatedServer) *ghConformanceRepo {
	t.Helper()
	name := s.createRepoWriteRepo(t, false)
	repo := s.store.GetRepo("admin", name)
	admin := s.store.LookupUserByLogin("admin")
	if repo == nil || admin == nil {
		t.Fatal("the fixture repository or the seeded admin is missing")
	}
	branches := seedPullRequestBranches(t, s.Server, repo, "feature")
	issue := s.store.CreateIssue(repo.ID, admin.ID, "conformance issue", "body", nil, nil, 0)
	pr := s.store.CreatePullRequest(repo.ID, admin.ID, "conformance pr", "body", "feature", "main", false, nil, nil, 0)
	if issue == nil || pr == nil {
		t.Fatal("the store refused to seed the fixture subjects")
	}
	return &ghConformanceRepo{
		repo:  repo,
		name:  name,
		issue: issue,
		pr:    pr,
		head:  branches["feature"],
		admin: admin,
	}
}

// TestGraphQLStatusCheckIsRequiredReportsBranchProtection covers `gh pr
// checks`, whose document selects isRequired on every rollup context. The
// answer has to come from the repository's branch protection: a constant would
// mark either everything or nothing required, and the command's whole point is
// to tell a contributor which failures block the merge.
func TestGraphQLStatusCheckIsRequiredReportsBranchProtection(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGHConformanceRepo(t, s)

	// One required context of each kind, one optional context of each kind.
	s.setBranchProtection(f.repo, "main", &store.BranchProtection{
		RequiredStatusChecks: &store.BPStatusChecks{
			Contexts: []string{"required-status"},
			Checks:   []store.BPCheck{{Context: "required-run"}},
		},
	})
	admin := f.admin
	if s.store.CommitStatuses.Create(f.repo.FullName, f.head, admin.ID, "success", "", "", "required-status") == nil {
		t.Fatal("the required commit status was refused")
	}
	if s.store.CommitStatuses.Create(f.repo.FullName, f.head, admin.ID, "success", "", "", "optional-status") == nil {
		t.Fatal("the optional commit status was refused")
	}
	if s.store.CreateCheckRun(f.repo.FullName, f.head, "required-run", 0, 0) == nil {
		t.Fatal("the required check run was refused")
	}
	if s.store.CreateCheckRun(f.repo.FullName, f.head, "optional-run", 0, 0) == nil {
		t.Fatal("the optional check run was refused")
	}

	// The selection gh sends for `gh pr checks`, keyed on the pull request's
	// node id exactly as gh keys it.
	data := s.ghConformanceData(t, defaultToken, `query($owner:String!,$name:String!,$number:Int!,$id:ID!){
		repository(owner:$owner,name:$name){
			pullRequest(number:$number){
				commits(last:1){nodes{commit{statusCheckRollup{contexts(first:100){nodes{
					__typename
					...on StatusContext{context state isRequired(pullRequestId:$id)}
					...on CheckRun{name status conclusion isRequired(pullRequestId:$id)}
				}}}}}}
			}
		}
	}`, map[string]interface{}{
		"owner": "admin", "name": f.name, "number": f.pr.Number, "id": f.pr.NodeID,
	})

	required := rollupRequiredByName(t, data)
	want := map[string]bool{
		"required-status": true,
		"optional-status": false,
		"required-run":    true,
		"optional-run":    false,
	}
	if len(required) != len(want) {
		t.Fatalf("rollup contexts = %v, want one entry per seeded check", required)
	}
	for name, isRequired := range want {
		got, ok := required[name]
		if !ok {
			t.Errorf("%s is missing from the rollup", name)
			continue
		}
		if got != isRequired {
			t.Errorf("%s isRequired = %v, want %v", name, got, isRequired)
		}
	}
}

// TestGraphQLStatusCheckIsRequiredAcceptsThePullRequestNumber pins the other
// argument GitHub accepts, and that a pull request onto an unprotected branch
// requires nothing.
func TestGraphQLStatusCheckIsRequiredAcceptsThePullRequestNumber(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGHConformanceRepo(t, s)
	s.setBranchProtection(f.repo, "main", &store.BranchProtection{
		RequiredStatusChecks: &store.BPStatusChecks{Contexts: []string{"gate"}},
	})
	if s.store.CommitStatuses.Create(f.repo.FullName, f.head, f.admin.ID, "success", "", "", "gate") == nil {
		t.Fatal("the commit status was refused")
	}

	query := `query($owner:String!,$name:String!,$number:Int!,$prNumber:Int!){
		repository(owner:$owner,name:$name){
			pullRequest(number:$number){
				commits(last:1){nodes{commit{statusCheckRollup{contexts(first:100){nodes{
					__typename
					...on StatusContext{context isRequired(pullRequestNumber:$prNumber)}
				}}}}}}
			}
		}
	}`
	data := s.ghConformanceData(t, defaultToken, query, map[string]interface{}{
		"owner": "admin", "name": f.name, "number": f.pr.Number, "prNumber": f.pr.Number,
	})
	if required := rollupRequiredByName(t, data); !required["gate"] {
		t.Fatalf("isRequired(pullRequestNumber:) = %v, want the protected context to be required", required)
	}

	// A pull request onto a branch with no protection requires nothing.
	other := s.store.CreatePullRequest(f.repo.ID, f.admin.ID, "onto feature", "body", "main", "feature", false, nil, nil, 0)
	if other == nil {
		t.Fatal("the second pull request was refused")
	}
	data = s.ghConformanceData(t, defaultToken, query, map[string]interface{}{
		"owner": "admin", "name": f.name, "number": f.pr.Number, "prNumber": other.Number,
	})
	if required := rollupRequiredByName(t, data); required["gate"] {
		t.Fatal("a context was reported required for a pull request onto an unprotected branch")
	}
}

// rollupRequiredByName flattens a statusCheckRollup contexts selection into
// name → isRequired.
func rollupRequiredByName(t *testing.T, data map[string]interface{}) map[string]bool {
	t.Helper()
	nodes := prCommitNodes(t, data)
	out := map[string]bool{}
	for _, raw := range nodes {
		node, _ := raw.(map[string]interface{})
		commit, _ := node["commit"].(map[string]interface{})
		rollup, _ := commit["statusCheckRollup"].(map[string]interface{})
		if rollup == nil {
			t.Fatalf("statusCheckRollup resolved to null: %v", commit)
		}
		contexts, _ := rollup["contexts"].(map[string]interface{})
		contextNodes, _ := contexts["nodes"].([]interface{})
		for _, rawContext := range contextNodes {
			entry, _ := rawContext.(map[string]interface{})
			name, _ := entry["context"].(string)
			if name == "" {
				name, _ = entry["name"].(string)
			}
			if name == "" {
				continue
			}
			isRequired, ok := entry["isRequired"].(bool)
			if !ok {
				t.Fatalf("isRequired came back as %T for %s", entry["isRequired"], name)
			}
			out[name] = isRequired
		}
	}
	return out
}

// TestGraphQLLabelMutationsEditAnIssuesLabels covers `gh issue edit
// --add-label` / `--remove-label`, which speak addLabelsToLabelable and
// removeLabelsFromLabelable and have no REST fallback: without the mutations
// the command fails outright.
func TestGraphQLLabelMutationsEditAnIssuesLabels(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGHConformanceRepo(t, s)
	bug := s.store.CreateLabel(f.repo.ID, "conformance-bug", "", "d73a4a")
	chore := s.store.CreateLabel(f.repo.ID, "conformance-chore", "", "cfd3d7")
	if bug == nil || chore == nil {
		t.Fatal("the fixture labels were refused")
	}

	// gh selects only __typename off the payload, so that is what is asked
	// for here; the label set is read back off the store and the issue.
	data := s.ghConformanceData(t, defaultToken,
		`mutation($input:AddLabelsToLabelableInput!){addLabelsToLabelable(input:$input){labelable{__typename ... on Issue{labels(first:10){nodes{name}}}}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"labelableId": f.issue.NodeID,
			"labelIds":    []interface{}{bug.NodeID, chore.NodeID},
		}})
	if got := labelableNames(t, data, "addLabelsToLabelable"); !equalStringSets(got, []string{"conformance-bug", "conformance-chore"}) {
		t.Fatalf("labels after add = %v, want conformance-bug and conformance-chore", got)
	}
	if got := issueLabelNames(t, s, f.issue.ID); !equalStringSets(got, []string{"conformance-bug", "conformance-chore"}) {
		t.Fatalf("stored labels after add = %v, want conformance-bug and conformance-chore", got)
	}

	// Adding a label the issue already carries is not an error and does not
	// duplicate it — gh re-sends the whole --add-label set.
	data = s.ghConformanceData(t, defaultToken,
		`mutation($input:AddLabelsToLabelableInput!){addLabelsToLabelable(input:$input){labelable{__typename ... on Issue{labels(first:10){nodes{name}}}}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"labelableId": f.issue.NodeID,
			"labelIds":    []interface{}{bug.NodeID},
		}})
	if got := labelableNames(t, data, "addLabelsToLabelable"); !equalStringSets(got, []string{"conformance-bug", "conformance-chore"}) {
		t.Fatalf("labels after re-adding conformance-bug = %v, want the set unchanged", got)
	}

	data = s.ghConformanceData(t, defaultToken,
		`mutation($input:RemoveLabelsFromLabelableInput!){removeLabelsFromLabelable(input:$input){labelable{__typename ... on Issue{labels(first:10){nodes{name}}}}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"labelableId": f.issue.NodeID,
			"labelIds":    []interface{}{bug.NodeID},
		}})
	if got := labelableNames(t, data, "removeLabelsFromLabelable"); !equalStringSets(got, []string{"conformance-chore"}) {
		t.Fatalf("labels after remove = %v, want conformance-chore alone", got)
	}

	data = s.ghConformanceData(t, defaultToken,
		`mutation($input:ClearLabelsFromLabelableInput!){clearLabelsFromLabelable(input:$input){labelable{__typename ... on Issue{labels(first:10){nodes{name}}}}}}`,
		map[string]interface{}{"input": map[string]interface{}{"labelableId": f.issue.NodeID}})
	if got := labelableNames(t, data, "clearLabelsFromLabelable"); len(got) != 0 {
		t.Fatalf("labels after clear = %v, want none", got)
	}
	if got := issueLabelNames(t, s, f.issue.ID); len(got) != 0 {
		t.Fatalf("stored labels after clear = %v, want none", got)
	}
}

// TestGraphQLLabelMutationsEditAPullRequestsLabels pins the other half of the
// Labelable interface: on GitHub a pull request is a labelable too, and
// `gh pr edit --add-label` sends the same mutation.
func TestGraphQLLabelMutationsEditAPullRequestsLabels(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGHConformanceRepo(t, s)
	ready := s.store.CreateLabel(f.repo.ID, "conformance-ready", "", "0e8a16")
	if ready == nil {
		t.Fatal("the fixture label was refused")
	}

	data := s.ghConformanceData(t, defaultToken,
		`mutation($input:AddLabelsToLabelableInput!){addLabelsToLabelable(input:$input){labelable{__typename ... on PullRequest{labels(first:10){nodes{name}}}}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"labelableId": f.pr.NodeID,
			"labelIds":    []interface{}{ready.NodeID},
		}})
	payload, _ := data["addLabelsToLabelable"].(map[string]interface{})
	labelable, _ := payload["labelable"].(map[string]interface{})
	if labelable["__typename"] != "PullRequest" {
		t.Fatalf("labelable __typename = %v, want PullRequest", labelable["__typename"])
	}
	if got := labelableNames(t, data, "addLabelsToLabelable"); !equalStringSets(got, []string{"conformance-ready"}) {
		t.Fatalf("pull request labels = %v, want conformance-ready", got)
	}
	updated := s.store.GetPullRequest(f.pr.ID)
	if updated == nil || len(updated.LabelIDs) != 1 || updated.LabelIDs[0] != ready.ID {
		t.Fatalf("stored pull request labels = %v, want the ready label", updated)
	}
}

// TestGraphQLAddLabelsRefusesALabelFromAnotherRepository keeps the mutation
// from attaching a label the repository does not own — the id space is global,
// so a bare id is not by itself proof the label belongs here.
func TestGraphQLAddLabelsRefusesALabelFromAnotherRepository(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	f := newGHConformanceRepo(t, s)
	otherName := s.createRepoWriteRepo(t, false)
	other := s.store.GetRepo("admin", otherName)
	if other == nil {
		t.Fatal("the second repository is missing")
	}
	foreign := s.store.CreateLabel(other.ID, "conformance-foreign", "", "ededed")
	if foreign == nil {
		t.Fatal("the foreign label was refused")
	}

	envelope := s.ghConformanceQuery(t, defaultToken,
		`mutation($input:AddLabelsToLabelableInput!){addLabelsToLabelable(input:$input){labelable{__typename}}}`,
		map[string]interface{}{"input": map[string]interface{}{
			"labelableId": f.issue.NodeID,
			"labelIds":    []interface{}{foreign.NodeID},
		}})
	if envelope["errors"] == nil {
		t.Fatalf("a label from another repository was attached: %v", envelope)
	}
	if got := issueLabelNames(t, s, f.issue.ID); len(got) != 0 {
		t.Fatalf("labels after the refusal = %v, want none", got)
	}
}

// labelableNames reads the label names off a label mutation's payload.
func labelableNames(t *testing.T, data map[string]interface{}, mutation string) []string {
	t.Helper()
	payload, _ := data[mutation].(map[string]interface{})
	labelable, _ := payload["labelable"].(map[string]interface{})
	if labelable == nil {
		t.Fatalf("%s returned no labelable: %v", mutation, data)
	}
	labels, _ := labelable["labels"].(map[string]interface{})
	nodes, _ := labels["nodes"].([]interface{})
	out := make([]string, 0, len(nodes))
	for _, raw := range nodes {
		node, _ := raw.(map[string]interface{})
		name, _ := node["name"].(string)
		out = append(out, name)
	}
	return out
}

func issueLabelNames(t *testing.T, s *isolatedServer, issueID int) []string {
	t.Helper()
	issue := s.store.GetIssue(issueID)
	if issue == nil {
		t.Fatal("the issue disappeared")
	}
	out := make([]string, 0, len(issue.LabelIDs))
	for _, id := range issue.LabelIDs {
		if label := s.store.GetLabel(id); label != nil {
			out = append(out, label.Name)
		}
	}
	return out
}

func equalStringSets(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, value := range got {
		seen[value]++
	}
	for _, value := range want {
		seen[value]--
		if seen[value] < 0 {
			return false
		}
	}
	return true
}

// gistListQuery is the document `gh gist list` sends, minus the
// @include(if:) directive gh puts on `text` (which only decides whether the
// contents come back).
const gistListQuery = `query($privacy:GistPrivacy!,$first:Int!,$after:String){
	viewer{
		gists(first:$first, after:$after, privacy:$privacy, orderBy:{field:CREATED_AT, direction:DESC}){
			nodes{description files{name text} isPublic name updatedAt}
			pageInfo{hasNextPage endCursor}
			totalCount
		}
	}
}`

// TestGraphQLViewerGistsServesTheGistListQuery covers `gh gist list`, which
// reads gists over GraphQL only: without User.gists, GistPrivacy and the
// GistConnection the command's document fails validation outright.
func TestGraphQLViewerGistsServesTheGistListQuery(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	if admin == nil {
		t.Fatal("the seeded admin is missing")
	}
	public, err := s.store.CreateGistE(admin, "a public gist", true, map[string]*store.GistFile{
		"public.txt": {Filename: "public.txt", Content: "public content\n", Size: len("public content\n"), Type: "text/plain"},
	})
	if err != nil || public == nil {
		t.Fatalf("seed public gist: %v", err)
	}
	secret, err := s.store.CreateGistE(admin, "a secret gist", false, map[string]*store.GistFile{
		"secret.txt": {Filename: "secret.txt", Content: "secret content\n", Size: len("secret content\n"), Type: "text/plain"},
	})
	if err != nil || secret == nil {
		t.Fatalf("seed secret gist: %v", err)
	}

	data := s.ghConformanceData(t, defaultToken, gistListQuery, map[string]interface{}{
		"privacy": "ALL", "first": 10, "after": nil,
	})
	viewer, _ := data["viewer"].(map[string]interface{})
	gists, _ := viewer["gists"].(map[string]interface{})
	if gists == nil {
		t.Fatalf("viewer.gists resolved to null: %v", data)
	}
	if gists["totalCount"] != float64(2) {
		t.Fatalf("totalCount = %v, want 2", gists["totalCount"])
	}
	nodes, _ := gists["nodes"].([]interface{})
	byName := map[string]map[string]interface{}{}
	for _, raw := range nodes {
		node, _ := raw.(map[string]interface{})
		name, _ := node["name"].(string)
		byName[name] = node
	}
	publicNode := byName[public.ID]
	if publicNode == nil {
		t.Fatalf("the public gist is missing from %v", byName)
	}
	if publicNode["description"] != "a public gist" || publicNode["isPublic"] != true {
		t.Fatalf("public gist node = %v", publicNode)
	}
	files, _ := publicNode["files"].([]interface{})
	if len(files) != 1 {
		t.Fatalf("public gist files = %v, want one", files)
	}
	file, _ := files[0].(map[string]interface{})
	if file["name"] != "public.txt" || file["text"] != "public content\n" {
		t.Fatalf("public gist file = %v", file)
	}
	if secretNode := byName[secret.ID]; secretNode == nil || secretNode["isPublic"] != false {
		t.Fatalf("the owner could not see their own secret gist: %v", byName)
	}

	// The privacy argument filters, which is what `gh gist list --public` and
	// `--secret` ask for.
	for _, tc := range []struct {
		privacy string
		want    string
	}{{"PUBLIC", public.ID}, {"SECRET", secret.ID}} {
		data := s.ghConformanceData(t, defaultToken, gistListQuery, map[string]interface{}{
			"privacy": tc.privacy, "first": 10, "after": nil,
		})
		viewer, _ := data["viewer"].(map[string]interface{})
		gists, _ := viewer["gists"].(map[string]interface{})
		nodes, _ := gists["nodes"].([]interface{})
		if len(nodes) != 1 {
			t.Fatalf("privacy %s returned %d gists, want 1", tc.privacy, len(nodes))
		}
		node, _ := nodes[0].(map[string]interface{})
		if node["name"] != tc.want {
			t.Errorf("privacy %s returned %v, want %s", tc.privacy, node["name"], tc.want)
		}
	}
}

// TestGraphQLUserGistsHidesSecretGistsFromAStranger is the authorization half:
// a secret gist is readable by its owner alone, and reading somebody else's
// through GraphQL would be a way around the REST surface's own gate.
func TestGraphQLUserGistsHidesSecretGistsFromAStranger(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	if admin == nil {
		t.Fatal("the seeded admin is missing")
	}
	public, err := s.store.CreateGistE(admin, "a public gist", true, map[string]*store.GistFile{
		"public.txt": {Filename: "public.txt", Content: "public\n", Size: 7, Type: "text/plain"},
	})
	if err != nil || public == nil {
		t.Fatalf("seed public gist: %v", err)
	}
	secret, err := s.store.CreateGistE(admin, "a secret gist", false, map[string]*store.GistFile{
		"secret.txt": {Filename: "secret.txt", Content: "secret\n", Size: 7, Type: "text/plain"},
	})
	if err != nil || secret == nil {
		t.Fatalf("seed secret gist: %v", err)
	}
	_, strangerToken := s.newUser(t, "gist-stranger")

	const query = `query{user(login:"admin"){gists(first:10, privacy:ALL){nodes{name isPublic description} totalCount}}}`
	data := s.ghConformanceData(t, strangerToken, query, nil)
	user, _ := data["user"].(map[string]interface{})
	gists, _ := user["gists"].(map[string]interface{})
	if gists == nil {
		t.Fatalf("user.gists resolved to null: %v", data)
	}
	if gists["totalCount"] != float64(1) {
		t.Fatalf("a stranger saw %v gists, want only the public one", gists["totalCount"])
	}
	nodes, _ := gists["nodes"].([]interface{})
	for _, raw := range nodes {
		node, _ := raw.(map[string]interface{})
		if node["name"] == secret.ID || node["isPublic"] != true {
			t.Fatalf("a stranger was served a secret gist: %v", node)
		}
	}

	// The owner still sees both through the same field.
	data = s.ghConformanceData(t, defaultToken, query, nil)
	user, _ = data["user"].(map[string]interface{})
	gists, _ = user["gists"].(map[string]interface{})
	if gists["totalCount"] != float64(2) {
		t.Fatalf("the owner saw %v of their own gists, want 2", gists["totalCount"])
	}
}

// TestGraphQLViewerGistsPaginatesAndOrders pins the connection arguments gh
// walks the list with.
func TestGraphQLViewerGistsPaginatesAndOrders(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.LookupUserByLogin("admin")
	if admin == nil {
		t.Fatal("the seeded admin is missing")
	}
	var ids []string
	for _, description := range []string{"first gist", "second gist", "third gist"} {
		gist, err := s.store.CreateGistE(admin, description, true, map[string]*store.GistFile{
			"note.txt": {Filename: "note.txt", Content: description + "\n", Size: len(description) + 1, Type: "text/plain"},
		})
		if err != nil || gist == nil {
			t.Fatalf("seed %s: %v", description, err)
		}
		ids = append(ids, gist.ID)
	}

	page := func(after interface{}) map[string]interface{} {
		t.Helper()
		data := s.ghConformanceData(t, defaultToken, gistListQuery, map[string]interface{}{
			"privacy": "ALL", "first": 2, "after": after,
		})
		viewer, _ := data["viewer"].(map[string]interface{})
		gists, _ := viewer["gists"].(map[string]interface{})
		if gists == nil {
			t.Fatalf("viewer.gists resolved to null: %v", data)
		}
		return gists
	}

	first := page(nil)
	nodes, _ := first["nodes"].([]interface{})
	if len(nodes) != 2 {
		t.Fatalf("first page returned %d gists, want 2", len(nodes))
	}
	pageInfo, _ := first["pageInfo"].(map[string]interface{})
	if pageInfo["hasNextPage"] != true {
		t.Fatalf("hasNextPage = %v, want true", pageInfo["hasNextPage"])
	}
	// CREATED_AT DESC: the newest gist comes first, so the page opens with the
	// last one seeded.
	firstNode, _ := nodes[0].(map[string]interface{})
	if firstNode["name"] != ids[2] {
		t.Errorf("first node = %v, want the newest gist %s", firstNode["name"], ids[2])
	}

	second := page(pageInfo["endCursor"])
	nodes, _ = second["nodes"].([]interface{})
	if len(nodes) != 1 {
		t.Fatalf("second page returned %d gists, want 1", len(nodes))
	}
	lastNode, _ := nodes[0].(map[string]interface{})
	if lastNode["name"] != ids[0] {
		t.Errorf("last node = %v, want the oldest gist %s", lastNode["name"], ids[0])
	}
	pageInfo, _ = second["pageInfo"].(map[string]interface{})
	if pageInfo["hasNextPage"] != false {
		t.Errorf("hasNextPage on the last page = %v, want false", pageInfo["hasNextPage"])
	}
}

// Command gogithub-driver exercises a running Bleephub through the official
// google/go-github client and emits one JSON Lines conformance record per
// operation.
//
// go-github is the most precise oracle in the harness: every response is
// decoded into a typed struct, so a renamed field, a wrong JSON type or a
// missing envelope surfaces here as a decode error or a zero-valued field that
// the assertion catches. A record is only "pass" when the client's own parsing
// succeeded AND the decoded value says what the operation promised — never
// merely because the transport returned 200.
//
// Configuration comes from the environment so the same binary runs locally and
// in continuous integration:
//
//	BPH_BASE     http://host:port root of the Bleephub server (required)
//	BPH_TOKEN    authentication token (required)
//	BPH_RESULTS  file to append JSON Lines records to (default stdout)
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	github "github.com/google/go-github/v88/github"
)

// record is one conformance observation. The schema is shared by every driver
// in the harness; test/conformance/report.py is the only consumer.
type record struct {
	Client    string `json:"client"`
	Domain    string `json:"domain"`
	Operation string `json:"operation"`
	Status    string `json:"status"`
	Request   string `json:"request,omitempty"`
	Expected  string `json:"expected,omitempty"`
	Actual    string `json:"actual,omitempty"`
	Message   string `json:"message,omitempty"`
}

// recorder serialises records to the results stream.
type recorder struct {
	out    io.Writer
	client string
	pass   int
	fail   int
	skip   int
}

// truncate keeps a failure message readable in the summary table while still
// carrying enough of the server's response to diagnose the deviation.
func truncate(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

func (r *recorder) emit(rec record) {
	rec.Client = r.client
	switch rec.Status {
	case "pass":
		r.pass++
	case "skip":
		r.skip++
	default:
		r.fail++
	}
	line, err := json.Marshal(rec)
	if err != nil {
		panic(err)
	}
	fmt.Fprintln(r.out, string(line))
}

// check runs one operation. fn returns nil when the client parsed the response
// and the decoded value satisfies the operation's contract; any error is a
// conformance failure with the error text as the observed behaviour.
func (r *recorder) check(domain, operation, request string, fn func() error) {
	err := invoke(fn)
	// GitHub answers a few endpoints (update-branch, statistics) with 202
	// Accepted and an empty body; go-github surfaces that as *AcceptedError.
	// The client parsed the response correctly, and 202 is the documented
	// GitHub behaviour, so it is a pass.
	var accepted *github.AcceptedError
	if errors.As(err, &accepted) {
		err = nil
	}
	if err == nil {
		r.emit(record{Domain: domain, Operation: operation, Status: "pass", Request: request})
		return
	}
	var deviation *deviationError
	if errors.As(err, &deviation) {
		r.emit(record{
			Domain: domain, Operation: operation, Status: "fail", Request: request,
			Expected: truncate(deviation.expected), Actual: truncate(deviation.actual),
			Message: truncate(deviation.message),
		})
		return
	}
	r.emit(record{
		Domain: domain, Operation: operation, Status: "fail", Request: request,
		Expected: "client call succeeds and decodes", Actual: truncate(err.Error()),
		Message: truncate(err.Error()),
	})
}

// invoke runs one operation's body and converts a panic into that operation's
// failure. Without this a single nil dereference — the shape a missing nested
// object takes in Go — would abort the process and silently discard every
// operation after it, which would look like a shrinking scoreboard rather than
// the one deviation it actually is.
func invoke(fn func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = deviate("the operation completes", fmt.Sprintf("panic: %v", recovered),
				"the client panicked handling the response: %v", recovered)
		}
	}()
	return fn()
}

// skip records an operation that could not be attempted because a fixture it
// depends on failed. It is never "pass", so the ratchet still notices.
func (r *recorder) skip1(domain, operation, request, why string) {
	r.emit(record{
		Domain: domain, Operation: operation, Status: "skip", Request: request,
		Message: truncate(why),
	})
}

// deviationError carries a structured expected/actual pair.
type deviationError struct {
	expected string
	actual   string
	message  string
}

func (e *deviationError) Error() string { return e.message }

func deviate(expected, actual, format string, args ...any) error {
	return &deviationError{expected: expected, actual: actual, message: fmt.Sprintf(format, args...)}
}

// wantField fails the operation when a field the client relies on came back
// empty. A zero-valued typed field is the quiet form of a shape mismatch: the
// decode succeeded because the server simply did not send the key.
func wantField(name, got string) error {
	if strings.TrimSpace(got) == "" {
		return deviate("non-empty "+name, "empty", "%s is empty in the decoded response", name)
	}
	return nil
}

var (
	ctx     = context.Background()
	baseURL = strings.TrimSuffix(os.Getenv("BPH_BASE"), "/")
	token   = os.Getenv("BPH_TOKEN")
)

// raw issues a request go-github has no typed method for. It is used only to
// provision fixtures (site-admin organization creation), never to make an
// assertion.
func raw(method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	return resp.StatusCode, payload, err
}

func main() {
	if baseURL == "" || token == "" {
		fmt.Fprintln(os.Stderr, "BPH_BASE and BPH_TOKEN are required")
		os.Exit(2)
	}
	out := os.Stdout
	if path := os.Getenv("BPH_RESULTS"); path != "" {
		file, err := os.Create(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open results file:", err)
			os.Exit(2)
		}
		defer file.Close()
		out = file
	}
	rec := &recorder{out: out, client: "go-github"}

	client, err := github.NewClient(
		github.WithAuthToken(token),
		github.WithEnterpriseURLs(baseURL+"/", baseURL+"/"),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "construct client:", err)
		os.Exit(2)
	}

	fixtures := newFixtures(client, rec)
	runUsers(client, rec)
	runMeta(client, rec)
	runRepositories(client, rec, fixtures)
	runContents(client, rec, fixtures)
	runIssues(client, rec, fixtures)
	runPulls(client, rec, fixtures)
	runReleases(client, rec, fixtures)
	runGitData(client, rec, fixtures)
	runActions(client, rec, fixtures)
	runOrgs(client, rec, fixtures)
	runSearch(client, rec, fixtures)
	runGists(client, rec)
	runMisc(client, rec, fixtures)
	runBranchProtection(client, rec, fixtures)
	runChecks(client, rec, fixtures)
	runActionsWorkflows(client, rec, fixtures)
	runActionsConfiguration(client, rec, fixtures)
	runActionsArtifacts(client, rec, fixtures)
	runActionsOrgScope(client, rec, fixtures)
	runBranches(client, rec, fixtures)
	runOrgRulesets(client, rec, fixtures)
	runSecurityAdvisories(client, rec, fixtures)

	// One second account serves every domain that needs a principal other than
	// the authenticated user: invitations, team membership, organization
	// membership and the 404-versus-403 distinction for private resources.
	guest := newPrincipal("conformance-guest")
	runCollaborators(client, rec, fixtures, guest)
	runTeams(client, rec, fixtures, guest)
	runOrgMembership(client, rec, fixtures, guest)
	runDeployments(client, rec, fixtures, guest)
	runWebhooks(client, rec, fixtures)
	runApps(client, rec, fixtures)
	runDiscovery(client, rec, fixtures)
	runWiki(client, rec, fixtures)
	runEdgeSemantics(client, rec, fixtures, guest)
	runHypermedia(client, rec, fixtures)
	runDeviceFlow(rec, fixtures)

	fmt.Fprintf(os.Stderr, "go-github driver: %d passed, %d failed, %d skipped\n", rec.pass, rec.fail, rec.skip)
}

// fixtureSet holds the resources later operations depend on. A fixture that
// could not be created leaves its field empty and dependent operations record
// "skip" rather than a misleading failure.
type fixtureSet struct {
	owner   string
	repo    string
	org     string
	orgRepo string
	branch  string
	sha     string
	issue   int
	pull    int
	tag     string
}

func newFixtures(client *github.Client, rec *recorder) *fixtureSet {
	set := &fixtureSet{branch: "main", tag: "v1.0.0"}

	rec.check("users", "users.getAuthenticated", "GET /user", func() error {
		user, _, err := client.Users.Get(ctx, "")
		if err != nil {
			return err
		}
		set.owner = user.GetLogin()
		return wantField("user.login", user.GetLogin())
	})
	if set.owner == "" {
		return set
	}

	name := "conformance-gogithub"
	rec.check("repos", "repos.create", "POST /user/repos", func() error {
		created, _, err := client.Repositories.Create(ctx, "", &github.Repository{
			Name:        github.Ptr(name),
			Description: github.Ptr("go-github conformance fixture"),
			AutoInit:    github.Ptr(true),
			HasIssues:   github.Ptr(true),
			HasWiki:     github.Ptr(true),
		})
		if err != nil {
			return err
		}
		set.repo = created.GetName()
		if created.GetFullName() != set.owner+"/"+name {
			return deviate(set.owner+"/"+name, created.GetFullName(), "full_name of the created repository is wrong")
		}
		if created.GetOwner().GetLogin() == "" {
			return deviate("owner.login populated", "empty", "created repository has no owner.login")
		}
		if created.GetDefaultBranch() == "" {
			return deviate("default_branch populated", "empty", "created repository has no default_branch")
		}
		return nil
	})
	if set.repo == "" {
		return set
	}

	// Seed a commit so tree/blob/commit/branch operations have something real
	// to read. A repository created with auto_init already has one; this adds a
	// second file so contents-update paths have a known blob.
	rec.check("contents", "repos.createOrUpdateFileContents", "PUT /repos/{owner}/{repo}/contents/{path}", func() error {
		created, _, err := client.Repositories.CreateFile(ctx, set.owner, set.repo, "conformance.txt", &github.RepositoryContentFileOptions{
			Message: github.Ptr("add conformance fixture"),
			Content: []byte("bleephub conformance\n"),
			Branch:  github.Ptr(set.branch),
		})
		if err != nil {
			return err
		}
		if created.Commit.GetSHA() == "" {
			return deviate("commit.sha populated", "empty", "file creation response has no commit sha")
		}
		set.sha = created.Commit.GetSHA()
		if created.GetContent().GetPath() != "conformance.txt" {
			return deviate("conformance.txt", created.GetContent().GetPath(), "content.path of the created file is wrong")
		}
		return nil
	})

	rec.check("issues", "issues.create", "POST /repos/{owner}/{repo}/issues", func() error {
		issue, _, err := client.Issues.Create(ctx, set.owner, set.repo, &github.IssueRequest{
			Title: github.Ptr("conformance issue"),
			Body:  github.Ptr("created by the go-github conformance driver"),
		})
		if err != nil {
			return err
		}
		set.issue = issue.GetNumber()
		if issue.GetNumber() == 0 {
			return deviate("number >= 1", "0", "created issue has no number")
		}
		if issue.GetState() != "open" {
			return deviate("open", issue.GetState(), "created issue is not open")
		}
		if issue.GetUser().GetLogin() == "" {
			return deviate("user.login populated", "empty", "created issue has no author")
		}
		return nil
	})

	// A pull request needs a second branch with a divergent commit.
	rec.check("git", "git.createRef", "POST /repos/{owner}/{repo}/git/refs", func() error {
		base, _, err := client.Git.GetRef(ctx, set.owner, set.repo, "refs/heads/"+set.branch)
		if err != nil {
			return err
		}
		_, _, err = client.Git.CreateRef(ctx, set.owner, set.repo, github.CreateRef{
			Ref: "refs/heads/conformance-topic",
			SHA: base.GetObject().GetSHA(),
		})
		return err
	})

	rec.check("contents", "repos.createOrUpdateFileContents (branch)", "PUT /repos/{owner}/{repo}/contents/{path}?branch=", func() error {
		_, _, err := client.Repositories.CreateFile(ctx, set.owner, set.repo, "topic.txt", &github.RepositoryContentFileOptions{
			Message: github.Ptr("topic change"),
			Content: []byte("topic\n"),
			Branch:  github.Ptr("conformance-topic"),
		})
		return err
	})

	rec.check("pulls", "pulls.create", "POST /repos/{owner}/{repo}/pulls", func() error {
		pull, _, err := client.PullRequests.Create(ctx, set.owner, set.repo, &github.NewPullRequest{
			Title: github.Ptr("conformance pull request"),
			Head:  github.Ptr("conformance-topic"),
			Base:  github.Ptr(set.branch),
			Body:  github.Ptr("opened by the go-github conformance driver"),
		})
		if err != nil {
			return err
		}
		set.pull = pull.GetNumber()
		if pull.GetHead().GetRef() != "conformance-topic" {
			return deviate("conformance-topic", pull.GetHead().GetRef(), "head.ref of the created pull request is wrong")
		}
		if pull.GetBase().GetRef() != set.branch {
			return deviate(set.branch, pull.GetBase().GetRef(), "base.ref of the created pull request is wrong")
		}
		return nil
	})

	// Organizations are created through GitHub Enterprise Server's site-admin
	// route; go-github exposes no typed constructor for it, matching GitHub.
	org := "conformance-org"
	status, body, err := raw(http.MethodPost, "/api/v3/admin/organizations", map[string]string{
		"login": org, "admin": set.owner, "profile_name": "Conformance Org",
	})
	if err == nil && (status == http.StatusCreated || status == http.StatusOK) {
		set.org = org
		rec.check("orgs", "orgs.create (site-admin)", "POST /admin/organizations", func() error { return nil })
		rec.check("repos", "repos.createInOrg", "POST /orgs/{org}/repos", func() error {
			created, _, err := client.Repositories.Create(ctx, org, &github.Repository{
				Name:     github.Ptr("org-conformance"),
				AutoInit: github.Ptr(true),
			})
			if err != nil {
				return err
			}
			set.orgRepo = created.GetName()
			if created.GetOwner().GetLogin() != org {
				return deviate(org, created.GetOwner().GetLogin(), "organization repository owner.login is wrong")
			}
			return nil
		})
	} else {
		rec.emit(record{
			Domain: "orgs", Operation: "orgs.create (site-admin)", Status: "fail",
			Request:  "POST /admin/organizations",
			Expected: "201 Created", Actual: fmt.Sprintf("%d %s", status, truncate(string(body))),
			Message: "organization fixture could not be provisioned",
		})
	}
	return set
}

func runUsers(client *github.Client, rec *recorder) {
	rec.check("users", "users.get", "GET /users/{username}", func() error {
		user, _, err := client.Users.Get(ctx, "admin")
		if err != nil {
			return err
		}
		if user.GetType() != "User" {
			return deviate("User", user.GetType(), "user type is wrong")
		}
		if user.GetHTMLURL() == "" {
			return deviate("html_url populated", "empty", "user has no html_url")
		}
		return wantField("user.node_id", user.GetNodeID())
	})
	rec.check("users", "users.listEmails", "GET /user/emails", func() error {
		emails, _, err := client.Users.ListEmails(ctx, nil)
		if err != nil {
			return err
		}
		if len(emails) == 0 {
			return deviate("at least one email", "empty list", "authenticated user has no email addresses")
		}
		return nil
	})
	rec.check("users", "users.listFollowers", "GET /user/followers", func() error {
		_, _, err := client.Users.ListFollowers(ctx, "", nil)
		return err
	})
	rec.check("users", "users.listFollowing", "GET /user/following", func() error {
		_, _, err := client.Users.ListFollowing(ctx, "", nil)
		return err
	})
	rec.check("users", "users.listPublicKeys", "GET /user/keys", func() error {
		_, _, err := client.Users.ListKeys(ctx, "", nil)
		return err
	})
	rec.check("users", "users.listGpgKeys", "GET /user/gpg_keys", func() error {
		_, _, err := client.Users.ListGPGKeys(ctx, "", nil)
		return err
	})
	rec.check("users", "users.listBlockedByAuthenticatedUser", "GET /user/blocks", func() error {
		_, _, err := client.Users.ListBlockedUsers(ctx, nil)
		return err
	})
	rec.check("users", "users.list", "GET /users", func() error {
		users, _, err := client.Users.ListAll(ctx, &github.UserListOptions{})
		if err != nil {
			return err
		}
		if len(users) == 0 {
			return deviate("at least one user", "empty list", "site user listing is empty")
		}
		return nil
	})
	rec.check("activity", "activity.listNotifications", "GET /notifications", func() error {
		_, _, err := client.Activity.ListNotifications(ctx, nil)
		return err
	})
	rec.check("activity", "activity.listReposStarredByAuthenticatedUser", "GET /user/starred", func() error {
		_, _, err := client.Activity.ListStarred(ctx, "", nil)
		return err
	})
	rec.check("activity", "activity.listWatchedReposForAuthenticatedUser", "GET /user/subscriptions", func() error {
		_, _, err := client.Activity.ListWatched(ctx, "", nil)
		return err
	})
}

func runMeta(client *github.Client, rec *recorder) {
	rec.check("meta", "meta.get", "GET /meta", func() error {
		meta, _, err := client.Meta.Get(ctx)
		if err != nil {
			return err
		}
		if meta.VerifiablePasswordAuthentication == nil {
			return deviate("verifiable_password_authentication present", "absent",
				"/meta omits verifiable_password_authentication")
		}
		return nil
	})
	rec.check("meta", "rateLimit.get", "GET /rate_limit", func() error {
		limits, _, err := client.RateLimit.Get(ctx)
		if err != nil {
			return err
		}
		if limits.GetCore().Limit == 0 {
			return deviate("resources.core.limit > 0", "0", "rate limit response has no core limit")
		}
		return nil
	})
	rec.check("meta", "emojis.get", "GET /emojis", func() error {
		emojis, _, err := client.ListEmojis(ctx)
		if err != nil {
			return err
		}
		if len(emojis) == 0 {
			return deviate("non-empty emoji map", "empty", "/emojis returned no entries")
		}
		return nil
	})
	rec.check("meta", "licenses.getAllCommonlyUsed", "GET /licenses", func() error {
		licenses, _, err := client.Licenses.List(ctx, nil)
		if err != nil {
			return err
		}
		if len(licenses) == 0 {
			return deviate("non-empty license list", "empty", "/licenses returned no entries")
		}
		return nil
	})
	rec.check("meta", "markdown.render", "POST /markdown", func() error {
		rendered, _, err := client.Markdown.Render(ctx, "# conformance", nil)
		if err != nil {
			return err
		}
		if !strings.Contains(rendered, "<h1") {
			return deviate("HTML containing <h1", truncate(rendered), "rendered markdown is not HTML")
		}
		return nil
	})
	rec.check("meta", "apiVersions.list", "GET /versions", func() error {
		request, err := client.NewRequest(ctx, http.MethodGet, "versions", nil)
		if err != nil {
			return err
		}
		var versions []string
		if _, err := client.Do(request, &versions); err != nil {
			return err
		}
		if len(versions) == 0 {
			return deviate("non-empty version list", "empty", "/versions returned no entries")
		}
		return nil
	})
}

func runRepositories(client *github.Client, rec *recorder, set *fixtureSet) {
	if set.repo == "" {
		rec.skip1("repos", "repos.get", "GET /repos/{owner}/{repo}", "repository fixture unavailable")
		return
	}
	rec.check("repos", "repos.get", "GET /repos/{owner}/{repo}", func() error {
		repo, _, err := client.Repositories.Get(ctx, set.owner, set.repo)
		if err != nil {
			return err
		}
		if repo.GetCloneURL() == "" {
			return deviate("clone_url populated", "empty", "repository has no clone_url")
		}
		if repo.GetPermissions() == nil {
			return deviate("permissions object present", "absent", "repository omits the permissions object")
		}
		if repo.GetOwner().GetType() != "User" {
			return deviate("User", repo.GetOwner().GetType(), "repository owner type is wrong")
		}
		return wantField("repository.node_id", repo.GetNodeID())
	})
	rec.check("repos", "repos.listForAuthenticatedUser", "GET /user/repos", func() error {
		repos, _, err := client.Repositories.ListByAuthenticatedUser(ctx, nil)
		if err != nil {
			return err
		}
		for _, repo := range repos {
			if repo.GetName() == set.repo {
				return nil
			}
		}
		return deviate("listing contains "+set.repo, fmt.Sprintf("%d repositories, none matching", len(repos)),
			"the authenticated user's repository listing omits the fixture repository")
	})
	rec.check("repos", "repos.listForUser", "GET /users/{username}/repos", func() error {
		_, _, err := client.Repositories.ListByUser(ctx, set.owner, nil)
		return err
	})
	rec.check("repos", "repos.update", "PATCH /repos/{owner}/{repo}", func() error {
		updated, _, err := client.Repositories.Edit(ctx, set.owner, set.repo, &github.Repository{
			Description: github.Ptr("updated by the conformance driver"),
		})
		if err != nil {
			return err
		}
		if updated.GetDescription() != "updated by the conformance driver" {
			return deviate("updated by the conformance driver", updated.GetDescription(),
				"PATCH response does not reflect the new description")
		}
		return nil
	})
	rec.check("repos", "repos.listBranches", "GET /repos/{owner}/{repo}/branches", func() error {
		branches, _, err := client.Repositories.ListBranches(ctx, set.owner, set.repo, nil)
		if err != nil {
			return err
		}
		if len(branches) == 0 {
			return deviate("at least the default branch", "empty list", "branch listing is empty")
		}
		if branches[0].GetCommit().GetSHA() == "" {
			return deviate("branch.commit.sha populated", "empty", "listed branch has no commit sha")
		}
		return nil
	})
	rec.check("repos", "repos.getBranch", "GET /repos/{owner}/{repo}/branches/{branch}", func() error {
		branch, _, err := client.Repositories.GetBranch(ctx, set.owner, set.repo, set.branch, 0)
		if err != nil {
			return err
		}
		if branch.Protected == nil {
			return deviate("protected present", "absent", "branch omits the protected field")
		}
		return nil
	})
	rec.check("repos", "repos.listCommits", "GET /repos/{owner}/{repo}/commits", func() error {
		commits, _, err := client.Repositories.ListCommits(ctx, set.owner, set.repo, nil)
		if err != nil {
			return err
		}
		if len(commits) == 0 {
			return deviate("at least one commit", "empty list", "commit listing is empty")
		}
		if commits[0].GetCommit().GetAuthor().GetName() == "" {
			return deviate("commit.author.name populated", "empty", "listed commit has no author name")
		}
		return nil
	})
	rec.check("repos", "repos.getCommit", "GET /repos/{owner}/{repo}/commits/{ref}", func() error {
		commit, _, err := client.Repositories.GetCommit(ctx, set.owner, set.repo, set.branch, nil)
		if err != nil {
			return err
		}
		if len(commit.Files) == 0 {
			return deviate("files array populated", "empty", "commit detail carries no file list")
		}
		if commit.GetStats() == nil {
			return deviate("stats object present", "absent", "commit detail carries no stats")
		}
		return nil
	})
	rec.check("repos", "repos.compareCommits", "GET /repos/{owner}/{repo}/compare/{basehead}", func() error {
		comparison, _, err := client.Repositories.CompareCommits(ctx, set.owner, set.repo, set.branch, "conformance-topic", nil)
		if err != nil {
			return err
		}
		if comparison.GetStatus() == "" {
			return deviate("status populated", "empty", "comparison has no status")
		}
		return nil
	})
	rec.check("repos", "repos.listTags", "GET /repos/{owner}/{repo}/tags", func() error {
		_, _, err := client.Repositories.ListTags(ctx, set.owner, set.repo, nil)
		return err
	})
	rec.check("repos", "repos.listLanguages", "GET /repos/{owner}/{repo}/languages", func() error {
		_, _, err := client.Repositories.ListLanguages(ctx, set.owner, set.repo)
		return err
	})
	rec.check("repos", "repos.listContributors", "GET /repos/{owner}/{repo}/contributors", func() error {
		_, _, err := client.Repositories.ListContributors(ctx, set.owner, set.repo, nil)
		return err
	})
	rec.check("repos", "repos.listCollaborators", "GET /repos/{owner}/{repo}/collaborators", func() error {
		collaborators, _, err := client.Repositories.ListCollaborators(ctx, set.owner, set.repo, nil)
		if err != nil {
			return err
		}
		if len(collaborators) == 0 {
			return deviate("at least the owner", "empty list", "collaborator listing is empty")
		}
		if collaborators[0].GetPermissions() == nil {
			return deviate("permissions map present", "absent", "collaborator omits the permissions map")
		}
		return nil
	})
	rec.check("repos", "repos.listTopics", "GET /repos/{owner}/{repo}/topics", func() error {
		_, _, err := client.Repositories.ListAllTopics(ctx, set.owner, set.repo, nil)
		return err
	})
	rec.check("repos", "repos.replaceAllTopics", "PUT /repos/{owner}/{repo}/topics", func() error {
		topics, _, err := client.Repositories.ReplaceAllTopics(ctx, set.owner, set.repo, []string{"conformance", "testing"})
		if err != nil {
			return err
		}
		if len(topics) != 2 {
			return deviate("2 topics", fmt.Sprintf("%d topics", len(topics)), "topic replacement did not echo the new topics")
		}
		return nil
	})
	rec.check("repos", "repos.listLabelsForRepo", "GET /repos/{owner}/{repo}/labels", func() error {
		labels, _, err := client.Issues.ListLabels(ctx, set.owner, set.repo, nil)
		if err != nil {
			return err
		}
		if len(labels) == 0 {
			return deviate("GitHub's default label set", "empty list",
				"a new repository has no default labels")
		}
		return nil
	})
	rec.check("repos", "issues.createLabel", "POST /repos/{owner}/{repo}/labels", func() error {
		label, _, err := client.Issues.CreateLabel(ctx, set.owner, set.repo, &github.Label{
			Name: github.Ptr("conformance"), Color: github.Ptr("ededed"),
		})
		if err != nil {
			return err
		}
		return wantField("label.url", label.GetURL())
	})
	rec.check("repos", "issues.createMilestone", "POST /repos/{owner}/{repo}/milestones", func() error {
		milestone, _, err := client.Issues.CreateMilestone(ctx, set.owner, set.repo, &github.Milestone{
			Title: github.Ptr("conformance milestone"),
		})
		if err != nil {
			return err
		}
		if milestone.GetNumber() == 0 {
			return deviate("number >= 1", "0", "created milestone has no number")
		}
		return nil
	})
	rec.check("repos", "repos.listMilestones", "GET /repos/{owner}/{repo}/milestones", func() error {
		milestones, _, err := client.Issues.ListMilestones(ctx, set.owner, set.repo, nil)
		if err != nil {
			return err
		}
		if len(milestones) == 0 {
			return deviate("at least one milestone", "empty list", "milestone listing is empty")
		}
		return nil
	})
	rec.check("repos", "repos.createWebhook", "POST /repos/{owner}/{repo}/hooks", func() error {
		hook, _, err := client.Repositories.CreateHook(ctx, set.owner, set.repo, &github.Hook{
			Events: []string{"push"},
			Config: &github.HookConfig{
				URL:         github.Ptr("https://example.invalid/hook"),
				ContentType: github.Ptr("json"),
			},
		})
		if err != nil {
			return err
		}
		if hook.GetID() == 0 {
			return deviate("id > 0", "0", "created webhook has no id")
		}
		return nil
	})
	rec.check("repos", "repos.listWebhooks", "GET /repos/{owner}/{repo}/hooks", func() error {
		_, _, err := client.Repositories.ListHooks(ctx, set.owner, set.repo, nil)
		return err
	})
	rec.check("repos", "activity.starRepoForAuthenticatedUser", "PUT /user/starred/{owner}/{repo}", func() error {
		_, err := client.Activity.Star(ctx, set.owner, set.repo)
		return err
	})
	rec.check("repos", "activity.listStargazers", "GET /repos/{owner}/{repo}/stargazers", func() error {
		gazers, _, err := client.Activity.ListStargazers(ctx, set.owner, set.repo, nil)
		if err != nil {
			return err
		}
		if len(gazers) == 0 {
			return deviate("the star just created", "empty list", "stargazer listing does not reflect the new star")
		}
		return nil
	})
	rec.check("repos", "activity.setRepoSubscription", "PUT /repos/{owner}/{repo}/subscription", func() error {
		subscription, _, err := client.Activity.SetRepositorySubscription(ctx, set.owner, set.repo, &github.Subscription{
			Subscribed: github.Ptr(true),
		})
		if err != nil {
			return err
		}
		if !subscription.GetSubscribed() {
			return deviate("subscribed=true", "false", "subscription was not recorded")
		}
		return nil
	})
	rec.check("repos", "repos.getContributorsStats", "GET /repos/{owner}/{repo}/stats/contributors", func() error {
		_, _, err := client.Repositories.ListContributorsStats(ctx, set.owner, set.repo)
		return err
	})
	rec.check("repos", "repos.getCodeFrequencyStats", "GET /repos/{owner}/{repo}/stats/code_frequency", func() error {
		_, _, err := client.Repositories.ListCodeFrequency(ctx, set.owner, set.repo)
		return err
	})
	rec.check("repos", "repos.getViews", "GET /repos/{owner}/{repo}/traffic/views", func() error {
		_, _, err := client.Repositories.ListTrafficViews(ctx, set.owner, set.repo, nil)
		return err
	})
	rec.check("repos", "repos.listDeployments", "GET /repos/{owner}/{repo}/deployments", func() error {
		_, _, err := client.Repositories.ListDeployments(ctx, set.owner, set.repo, nil)
		return err
	})
	rec.check("repos", "repos.createDeployment", "POST /repos/{owner}/{repo}/deployments", func() error {
		deployment, _, err := client.Repositories.CreateDeployment(ctx, set.owner, set.repo, &github.DeploymentRequest{
			Ref:              github.Ptr(set.branch),
			Environment:      github.Ptr("conformance"),
			RequiredContexts: &[]string{},
		})
		if err != nil {
			return err
		}
		if deployment.GetID() == 0 {
			return deviate("id > 0", "0", "created deployment has no id")
		}
		return nil
	})
	rec.check("repos", "repos.getCombinedStatusForRef", "GET /repos/{owner}/{repo}/commits/{ref}/status", func() error {
		status, _, err := client.Repositories.GetCombinedStatus(ctx, set.owner, set.repo, set.branch, nil)
		if err != nil {
			return err
		}
		if status.GetState() == "" {
			return deviate("state populated", "empty", "combined status has no state")
		}
		return nil
	})
	rec.check("repos", "repos.createCommitStatus", "POST /repos/{owner}/{repo}/statuses/{sha}", func() error {
		commits, _, err := client.Repositories.ListCommits(ctx, set.owner, set.repo, nil)
		if err != nil {
			return err
		}
		if len(commits) == 0 {
			return deviate("a commit to attach a status to", "no commits", "cannot create a status without a commit")
		}
		status, _, err := client.Repositories.CreateStatus(ctx, set.owner, set.repo, commits[0].GetSHA(), github.RepoStatus{
			State:   github.Ptr("success"),
			Context: github.Ptr("conformance/driver"),
		})
		if err != nil {
			return err
		}
		if status.GetState() != "success" {
			return deviate("success", status.GetState(), "created commit status has the wrong state")
		}
		return nil
	})
	rec.check("repos", "checks.create", "POST /repos/{owner}/{repo}/check-runs", func() error {
		commits, _, err := client.Repositories.ListCommits(ctx, set.owner, set.repo, nil)
		if err != nil || len(commits) == 0 {
			return deviate("a commit to attach a check run to", "no commits", "cannot create a check run")
		}
		run, _, err := client.Checks.CreateCheckRun(ctx, set.owner, set.repo, github.CreateCheckRunOptions{
			Name:    "conformance",
			HeadSHA: commits[0].GetSHA(),
			Status:  github.Ptr("completed"),
			Conclusion: github.Ptr(
				"success"),
		})
		if err != nil {
			return err
		}
		if run.GetID() == 0 {
			return deviate("id > 0", "0", "created check run has no id")
		}
		return nil
	})
}

// runBranchProtection runs last: enabling a required status check on the
// default branch would otherwise block the pull-request merge operation.
func runBranchProtection(client *github.Client, rec *recorder, set *fixtureSet) {
	if set.repo == "" {
		return
	}
	rec.check("repos", "repos.updateBranchProtection", "PUT /repos/{owner}/{repo}/branches/{branch}/protection", func() error {
		_, _, err := client.Repositories.UpdateBranchProtection(ctx, set.owner, set.repo, set.branch, &github.ProtectionRequest{
			RequiredStatusChecks: &github.RequiredStatusChecks{Strict: true, Contexts: &[]string{"conformance/driver"}},
			EnforceAdmins:        true,
		})
		if err != nil {
			return err
		}
		protection, _, err := client.Repositories.GetBranchProtection(ctx, set.owner, set.repo, set.branch)
		if err != nil {
			return err
		}
		if protection.GetRequiredStatusChecks() == nil {
			return deviate("required_status_checks present", "absent", "branch protection lost the required status checks")
		}
		return nil
	})
}

func runContents(client *github.Client, rec *recorder, set *fixtureSet) {
	if set.repo == "" {
		return
	}
	rec.check("contents", "repos.getContent (file)", "GET /repos/{owner}/{repo}/contents/{path}", func() error {
		file, _, _, err := client.Repositories.GetContents(ctx, set.owner, set.repo, "conformance.txt", nil)
		if err != nil {
			return err
		}
		if file == nil {
			return deviate("file object", "nil", "file contents came back as a directory")
		}
		decoded, err := file.GetContent()
		if err != nil {
			return deviate("base64-decodable content", err.Error(), "content could not be decoded by the client")
		}
		if decoded != "bleephub conformance\n" {
			return deviate("bleephub conformance", truncate(decoded), "file content does not round-trip")
		}
		if file.GetEncoding() != "base64" {
			return deviate("base64", file.GetEncoding(), "content encoding is not base64")
		}
		return nil
	})
	rec.check("contents", "repos.getContent (directory)", "GET /repos/{owner}/{repo}/contents/", func() error {
		_, directory, _, err := client.Repositories.GetContents(ctx, set.owner, set.repo, "", nil)
		if err != nil {
			return err
		}
		if len(directory) == 0 {
			return deviate("directory entries", "empty", "root directory listing is empty")
		}
		return nil
	})
	rec.check("contents", "repos.getReadme", "GET /repos/{owner}/{repo}/readme", func() error {
		readme, _, err := client.Repositories.GetReadme(ctx, set.owner, set.repo, nil)
		if err != nil {
			return err
		}
		return wantField("readme.name", readme.GetName())
	})
	rec.check("contents", "repos.downloadContents", "GET raw file content", func() error {
		reader, _, err := client.Repositories.DownloadContents(ctx, set.owner, set.repo, "conformance.txt", nil)
		if err != nil {
			return err
		}
		defer reader.Close()
		body, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		if string(body) != "bleephub conformance\n" {
			return deviate("bleephub conformance", truncate(string(body)), "downloaded content does not match")
		}
		return nil
	})
	rec.check("contents", "repos.deleteFile", "DELETE /repos/{owner}/{repo}/contents/{path}", func() error {
		// The file this deletes is created here rather than reused from the
		// fixtures: deleting the pull request's only changed file would empty
		// its diff and make the pull request operations assert on nothing.
		created, _, err := client.Repositories.CreateFile(ctx, set.owner, set.repo, "delete-me.txt", &github.RepositoryContentFileOptions{
			Message: github.Ptr("add a file to delete"),
			Content: []byte("delete me\n"),
			Branch:  github.Ptr(set.branch),
		})
		if err != nil {
			return err
		}
		_, _, err = client.Repositories.DeleteFile(ctx, set.owner, set.repo, "delete-me.txt", &github.RepositoryContentFileOptions{
			Message: github.Ptr("remove the deletion fixture"),
			SHA:     created.GetContent().SHA,
			Branch:  github.Ptr(set.branch),
		})
		return err
	})
}

func runIssues(client *github.Client, rec *recorder, set *fixtureSet) {
	if set.issue == 0 {
		rec.skip1("issues", "issues.get", "GET /repos/{owner}/{repo}/issues/{number}", "issue fixture unavailable")
		return
	}
	rec.check("issues", "issues.get", "GET /repos/{owner}/{repo}/issues/{number}", func() error {
		issue, _, err := client.Issues.Get(ctx, set.owner, set.repo, set.issue)
		if err != nil {
			return err
		}
		if issue.GetHTMLURL() == "" {
			return deviate("html_url populated", "empty", "issue has no html_url")
		}
		if issue.GetAuthorAssociation() == "" {
			return deviate("author_association populated", "empty", "issue omits author_association")
		}
		if issue.Reactions == nil {
			return deviate("reactions summary present", "absent", "issue omits the reactions summary")
		}
		return nil
	})
	rec.check("issues", "issues.listForRepo", "GET /repos/{owner}/{repo}/issues", func() error {
		issues, _, err := client.Issues.ListByRepo(ctx, set.owner, set.repo, nil)
		if err != nil {
			return err
		}
		if len(issues) == 0 {
			return deviate("at least one issue", "empty list", "issue listing is empty")
		}
		return nil
	})
	rec.check("issues", "issues.update", "PATCH /repos/{owner}/{repo}/issues/{number}", func() error {
		updated, _, err := client.Issues.Edit(ctx, set.owner, set.repo, set.issue, &github.IssueRequest{
			Title: github.Ptr("conformance issue (edited)"),
		})
		if err != nil {
			return err
		}
		if updated.GetTitle() != "conformance issue (edited)" {
			return deviate("conformance issue (edited)", updated.GetTitle(), "issue title was not updated")
		}
		return nil
	})
	rec.check("issues", "issues.createComment", "POST /repos/{owner}/{repo}/issues/{number}/comments", func() error {
		comment, _, err := client.Issues.CreateComment(ctx, set.owner, set.repo, set.issue, &github.IssueComment{
			Body: github.Ptr("conformance comment"),
		})
		if err != nil {
			return err
		}
		if comment.GetID() == 0 {
			return deviate("id > 0", "0", "created comment has no id")
		}
		return nil
	})
	rec.check("issues", "issues.listComments", "GET /repos/{owner}/{repo}/issues/{number}/comments", func() error {
		comments, _, err := client.Issues.ListComments(ctx, set.owner, set.repo, set.issue, nil)
		if err != nil {
			return err
		}
		if len(comments) == 0 {
			return deviate("the comment just created", "empty list", "comment listing is empty")
		}
		return nil
	})
	rec.check("issues", "issues.addLabels", "POST /repos/{owner}/{repo}/issues/{number}/labels", func() error {
		labels, _, err := client.Issues.AddLabelsToIssue(ctx, set.owner, set.repo, set.issue, []string{"conformance"})
		if err != nil {
			return err
		}
		if len(labels) == 0 {
			return deviate("the applied label", "empty list", "label application returned nothing")
		}
		return nil
	})
	rec.check("issues", "issues.addAssignees", "POST /repos/{owner}/{repo}/issues/{number}/assignees", func() error {
		issue, _, err := client.Issues.AddAssignees(ctx, set.owner, set.repo, set.issue, []string{set.owner})
		if err != nil {
			return err
		}
		if len(issue.Assignees) == 0 {
			return deviate("assignees populated", "empty", "assignee was not applied")
		}
		return nil
	})
	rec.check("issues", "reactions.createForIssue", "POST /repos/{owner}/{repo}/issues/{number}/reactions", func() error {
		reaction, _, err := client.Reactions.CreateIssueReaction(ctx, set.owner, set.repo, set.issue, "+1")
		if err != nil {
			return err
		}
		if reaction.GetContent() != "+1" {
			return deviate("+1", reaction.GetContent(), "created reaction has the wrong content")
		}
		return nil
	})
	rec.check("issues", "issues.listEventsForTimeline", "GET /repos/{owner}/{repo}/issues/{number}/timeline", func() error {
		events, _, err := client.Issues.ListIssueTimeline(ctx, set.owner, set.repo, set.issue, nil)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return deviate("timeline entries for a labelled, assigned, commented issue", "empty list",
				"timeline is empty despite recorded activity")
		}
		return nil
	})
	rec.check("issues", "issues.lock", "PUT /repos/{owner}/{repo}/issues/{number}/lock", func() error {
		_, err := client.Issues.Lock(ctx, set.owner, set.repo, set.issue, &github.LockIssueOptions{LockReason: "resolved"})
		return err
	})
	rec.check("issues", "issues.unlock", "DELETE /repos/{owner}/{repo}/issues/{number}/lock", func() error {
		_, err := client.Issues.Unlock(ctx, set.owner, set.repo, set.issue)
		return err
	})
	rec.check("issues", "issues.update (close)", "PATCH /repos/{owner}/{repo}/issues/{number} state=closed", func() error {
		closed, _, err := client.Issues.Edit(ctx, set.owner, set.repo, set.issue, &github.IssueRequest{
			State: github.Ptr("closed"), StateReason: github.Ptr("completed"),
		})
		if err != nil {
			return err
		}
		if closed.GetState() != "closed" {
			return deviate("closed", closed.GetState(), "issue was not closed")
		}
		if closed.GetStateReason() != "completed" {
			return deviate("completed", closed.GetStateReason(), "state_reason was not recorded")
		}
		return nil
	})
	rec.check("issues", "issues.listForAuthenticatedUser", "GET /user/issues", func() error {
		_, _, err := client.Issues.ListUserIssues(ctx, &github.ListUserIssuesOptions{Filter: "all", State: "all"})
		return err
	})
}

func runPulls(client *github.Client, rec *recorder, set *fixtureSet) {
	if set.pull == 0 {
		rec.skip1("pulls", "pulls.get", "GET /repos/{owner}/{repo}/pulls/{number}", "pull request fixture unavailable")
		return
	}
	rec.check("pulls", "pulls.get", "GET /repos/{owner}/{repo}/pulls/{number}", func() error {
		pull, _, err := client.PullRequests.Get(ctx, set.owner, set.repo, set.pull)
		if err != nil {
			return err
		}
		if pull.Mergeable == nil {
			return deviate("mergeable computed", "null", "pull request never reports mergeability")
		}
		if pull.GetMergeableState() == "" {
			return deviate("mergeable_state populated", "empty", "pull request omits mergeable_state")
		}
		if pull.GetHead().GetRepo() == nil {
			return deviate("head.repo populated", "absent", "pull request omits head.repo")
		}
		return nil
	})
	rec.check("pulls", "pulls.list", "GET /repos/{owner}/{repo}/pulls", func() error {
		pulls, _, err := client.PullRequests.List(ctx, set.owner, set.repo, nil)
		if err != nil {
			return err
		}
		if len(pulls) == 0 {
			return deviate("at least one pull request", "empty list", "pull request listing is empty")
		}
		return nil
	})
	rec.check("pulls", "pulls.listFiles", "GET /repos/{owner}/{repo}/pulls/{number}/files", func() error {
		files, _, err := client.PullRequests.ListFiles(ctx, set.owner, set.repo, set.pull, nil)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			return deviate("the changed file", "empty list", "pull request file listing is empty")
		}
		if files[0].GetPatch() == "" {
			return deviate("patch populated", "empty", "changed file carries no patch")
		}
		return nil
	})
	rec.check("pulls", "pulls.listCommits", "GET /repos/{owner}/{repo}/pulls/{number}/commits", func() error {
		commits, _, err := client.PullRequests.ListCommits(ctx, set.owner, set.repo, set.pull, nil)
		if err != nil {
			return err
		}
		if len(commits) == 0 {
			return deviate("at least one commit", "empty list", "pull request commit listing is empty")
		}
		return nil
	})
	rec.check("pulls", "pulls.createReviewComment", "POST /repos/{owner}/{repo}/pulls/{number}/comments", func() error {
		files, _, err := client.PullRequests.ListFiles(ctx, set.owner, set.repo, set.pull, nil)
		if err != nil || len(files) == 0 {
			return deviate("a changed file to comment on", "none", "cannot place a review comment")
		}
		comment, _, err := client.PullRequests.CreateComment(ctx, set.owner, set.repo, set.pull, &github.PullRequestComment{
			Body: github.Ptr("conformance review comment"),
			Path: files[0].Filename,
			Line: github.Ptr(1),
			Side: github.Ptr("RIGHT"),
		})
		if err != nil {
			return err
		}
		if comment.GetID() == 0 {
			return deviate("id > 0", "0", "created review comment has no id")
		}
		return nil
	})
	rec.check("pulls", "pulls.createReview", "POST /repos/{owner}/{repo}/pulls/{number}/reviews", func() error {
		review, _, err := client.PullRequests.CreateReview(ctx, set.owner, set.repo, set.pull, &github.PullRequestReviewRequest{
			Body: github.Ptr("conformance review"), Event: github.Ptr("COMMENT"),
		})
		if err != nil {
			return err
		}
		if review.GetState() == "" {
			return deviate("state populated", "empty", "created review has no state")
		}
		return nil
	})
	rec.check("pulls", "pulls.listReviews", "GET /repos/{owner}/{repo}/pulls/{number}/reviews", func() error {
		reviews, _, err := client.PullRequests.ListReviews(ctx, set.owner, set.repo, set.pull, nil)
		if err != nil {
			return err
		}
		if len(reviews) == 0 {
			return deviate("the review just created", "empty list", "review listing is empty")
		}
		return nil
	})
	rec.check("pulls", "pulls.updateBranch", "PUT /repos/{owner}/{repo}/pulls/{number}/update-branch", func() error {
		_, _, err := client.PullRequests.UpdateBranch(ctx, set.owner, set.repo, set.pull, nil)
		return err
	})
	rec.check("pulls", "pulls.checkIfMerged", "GET /repos/{owner}/{repo}/pulls/{number}/merge", func() error {
		merged, _, err := client.PullRequests.IsMerged(ctx, set.owner, set.repo, set.pull)
		if err != nil {
			return err
		}
		if merged {
			return deviate("not merged", "merged", "an open pull request reports as merged")
		}
		return nil
	})
	rec.check("pulls", "pulls.merge", "PUT /repos/{owner}/{repo}/pulls/{number}/merge", func() error {
		result, _, err := client.PullRequests.Merge(ctx, set.owner, set.repo, set.pull, "conformance merge", &github.PullRequestOptions{
			MergeMethod: "merge",
		})
		if err != nil {
			return err
		}
		if !result.GetMerged() {
			return deviate("merged=true", "false", "merge response does not report a merge")
		}
		if result.GetSHA() == "" {
			return deviate("sha populated", "empty", "merge response has no sha")
		}
		return nil
	})
}

func runReleases(client *github.Client, rec *recorder, set *fixtureSet) {
	if set.repo == "" {
		return
	}
	var releaseID int64
	rec.check("releases", "repos.createRelease", "POST /repos/{owner}/{repo}/releases", func() error {
		release, _, err := client.Repositories.CreateRelease(ctx, set.owner, set.repo, &github.RepositoryRelease{
			TagName: github.Ptr(set.tag), Name: github.Ptr("Conformance 1.0"), Body: github.Ptr("notes"),
		})
		if err != nil {
			return err
		}
		releaseID = release.GetID()
		if release.GetUploadURL() == "" {
			return deviate("upload_url populated", "empty", "release omits upload_url, so no client can attach assets")
		}
		if release.GetTarballURL() == "" {
			return deviate("tarball_url populated", "empty", "release omits tarball_url")
		}
		return nil
	})
	if releaseID == 0 {
		rec.skip1("releases", "repos.getRelease", "GET /repos/{owner}/{repo}/releases/{id}", "release fixture unavailable")
		return
	}
	rec.check("releases", "repos.getRelease", "GET /repos/{owner}/{repo}/releases/{id}", func() error {
		release, _, err := client.Repositories.GetRelease(ctx, set.owner, set.repo, releaseID)
		if err != nil {
			return err
		}
		if release.GetAuthor().GetLogin() == "" {
			return deviate("author.login populated", "empty", "release omits its author")
		}
		return nil
	})
	rec.check("releases", "repos.getReleaseByTag", "GET /repos/{owner}/{repo}/releases/tags/{tag}", func() error {
		_, _, err := client.Repositories.GetReleaseByTag(ctx, set.owner, set.repo, set.tag)
		return err
	})
	rec.check("releases", "repos.getLatestRelease", "GET /repos/{owner}/{repo}/releases/latest", func() error {
		release, _, err := client.Repositories.GetLatestRelease(ctx, set.owner, set.repo)
		if err != nil {
			return err
		}
		if release.GetTagName() != set.tag {
			return deviate(set.tag, release.GetTagName(), "latest release is not the release just created")
		}
		return nil
	})
	rec.check("releases", "repos.listReleases", "GET /repos/{owner}/{repo}/releases", func() error {
		releases, _, err := client.Repositories.ListReleases(ctx, set.owner, set.repo, nil)
		if err != nil {
			return err
		}
		if len(releases) == 0 {
			return deviate("the release just created", "empty list", "release listing is empty")
		}
		return nil
	})
	rec.check("releases", "repos.uploadReleaseAsset", "POST {upload_url}", func() error {
		asset := "conformance-asset.txt"
		file, err := os.CreateTemp("", "conformance-asset-*.txt")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		if _, err := file.WriteString("asset payload\n"); err != nil {
			return err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		uploaded, _, err := client.Repositories.UploadReleaseAsset(ctx, set.owner, set.repo, releaseID, &github.UploadOptions{
			Name: asset, MediaType: "text/plain",
		}, file)
		if err != nil {
			return err
		}
		if uploaded.GetBrowserDownloadURL() == "" {
			return deviate("browser_download_url populated", "empty", "uploaded asset has no download URL")
		}
		return nil
	})
	rec.check("releases", "repos.listReleaseAssets", "GET /repos/{owner}/{repo}/releases/{id}/assets", func() error {
		assets, _, err := client.Repositories.ListReleaseAssets(ctx, set.owner, set.repo, releaseID, nil)
		if err != nil {
			return err
		}
		if len(assets) == 0 {
			return deviate("the asset just uploaded", "empty list", "asset listing is empty")
		}
		return nil
	})
	rec.check("releases", "repos.generateReleaseNotes", "POST /repos/{owner}/{repo}/releases/generate-notes", func() error {
		notes, _, err := client.Repositories.GenerateReleaseNotes(ctx, set.owner, set.repo, &github.GenerateNotesOptions{
			TagName: "v2.0.0",
		})
		if err != nil {
			return err
		}
		return wantField("generated notes name", notes.GetName())
	})
}

func runGitData(client *github.Client, rec *recorder, set *fixtureSet) {
	if set.repo == "" {
		return
	}
	var commitSHA, treeSHA, blobSHA string
	rec.check("git", "git.getRef", "GET /repos/{owner}/{repo}/git/ref/{ref}", func() error {
		ref, _, err := client.Git.GetRef(ctx, set.owner, set.repo, "refs/heads/"+set.branch)
		if err != nil {
			return err
		}
		commitSHA = ref.GetObject().GetSHA()
		if ref.GetObject().GetType() != "commit" {
			return deviate("commit", ref.GetObject().GetType(), "ref object type is wrong")
		}
		return nil
	})
	rec.check("git", "git.listMatchingRefs", "GET /repos/{owner}/{repo}/git/matching-refs/{ref}", func() error {
		refs, _, err := client.Git.ListMatchingRefs(ctx, set.owner, set.repo, "heads/")
		if err != nil {
			return err
		}
		if len(refs) == 0 {
			return deviate("at least the default branch ref", "empty list", "matching-refs is empty")
		}
		return nil
	})
	if commitSHA == "" {
		return
	}
	rec.check("git", "git.getCommit", "GET /repos/{owner}/{repo}/git/commits/{sha}", func() error {
		commit, _, err := client.Git.GetCommit(ctx, set.owner, set.repo, commitSHA)
		if err != nil {
			return err
		}
		treeSHA = commit.GetTree().GetSHA()
		if treeSHA == "" {
			return deviate("tree.sha populated", "empty", "git commit has no tree")
		}
		if commit.GetAuthor().GetEmail() == "" {
			return deviate("author.email populated", "empty", "git commit has no author email")
		}
		return nil
	})
	if treeSHA != "" {
		rec.check("git", "git.getTree", "GET /repos/{owner}/{repo}/git/trees/{sha}", func() error {
			tree, _, err := client.Git.GetTree(ctx, set.owner, set.repo, treeSHA, true)
			if err != nil {
				return err
			}
			if len(tree.Entries) == 0 {
				return deviate("tree entries", "empty", "git tree has no entries")
			}
			for _, entry := range tree.Entries {
				if entry.GetType() == "blob" {
					blobSHA = entry.GetSHA()
					break
				}
			}
			return nil
		})
	}
	if blobSHA != "" {
		rec.check("git", "git.getBlob", "GET /repos/{owner}/{repo}/git/blobs/{sha}", func() error {
			blob, _, err := client.Git.GetBlob(ctx, set.owner, set.repo, blobSHA)
			if err != nil {
				return err
			}
			if blob.GetEncoding() != "base64" {
				return deviate("base64", blob.GetEncoding(), "blob encoding is not base64")
			}
			if _, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.GetContent(), "\n", "")); err != nil {
				return deviate("decodable base64 content", err.Error(), "blob content is not valid base64")
			}
			return nil
		})
	}
	rec.check("git", "git.createBlob", "POST /repos/{owner}/{repo}/git/blobs", func() error {
		blob, _, err := client.Git.CreateBlob(ctx, set.owner, set.repo, github.Blob{
			Content:  github.Ptr(base64.StdEncoding.EncodeToString([]byte("created blob\n"))),
			Encoding: github.Ptr("base64"),
		})
		if err != nil {
			return err
		}
		return wantField("blob.sha", blob.GetSHA())
	})
	rec.check("git", "git.createTag", "POST /repos/{owner}/{repo}/git/tags", func() error {
		tag, _, err := client.Git.CreateTag(ctx, set.owner, set.repo, github.CreateTag{
			Tag:     "conformance-annotated",
			Message: "annotated tag",
			Object:  commitSHA,
			Type:    "commit",
			Tagger:  &github.CommitAuthor{Name: github.Ptr("Conformance"), Email: github.Ptr("conformance@bleephub.local")},
		})
		if err != nil {
			return err
		}
		return wantField("tag.sha", tag.GetSHA())
	})
}

func runActions(client *github.Client, rec *recorder, set *fixtureSet) {
	if set.repo == "" {
		return
	}
	rec.check("actions", "actions.listRepoWorkflows", "GET /repos/{owner}/{repo}/actions/workflows", func() error {
		_, _, err := client.Actions.ListWorkflows(ctx, set.owner, set.repo, nil)
		return err
	})
	rec.check("actions", "actions.listWorkflowRunsForRepo", "GET /repos/{owner}/{repo}/actions/runs", func() error {
		_, _, err := client.Actions.ListRepositoryWorkflowRuns(ctx, set.owner, set.repo, nil)
		return err
	})
	rec.check("actions", "actions.getRepoPublicKey", "GET /repos/{owner}/{repo}/actions/secrets/public-key", func() error {
		key, _, err := client.Actions.GetRepoPublicKey(ctx, set.owner, set.repo)
		if err != nil {
			return err
		}
		if key.GetKeyID() == "" || key.GetKey() == "" {
			return deviate("key_id and key populated", "empty", "actions public key is incomplete")
		}
		return nil
	})
	rec.check("actions", "actions.listRepoSecrets", "GET /repos/{owner}/{repo}/actions/secrets", func() error {
		_, _, err := client.Actions.ListRepoSecrets(ctx, set.owner, set.repo, nil)
		return err
	})
	rec.check("actions", "actions.createOrUpdateRepoVariable", "POST /repos/{owner}/{repo}/actions/variables", func() error {
		_, err := client.Actions.CreateRepoVariable(ctx, set.owner, set.repo, &github.ActionsVariable{
			Name: "CONFORMANCE", Value: "1",
		})
		return err
	})
	rec.check("actions", "actions.listRepoVariables", "GET /repos/{owner}/{repo}/actions/variables", func() error {
		variables, _, err := client.Actions.ListRepoVariables(ctx, set.owner, set.repo, nil)
		if err != nil {
			return err
		}
		if variables.TotalCount == 0 {
			return deviate("the variable just created", "total_count 0", "variable listing is empty")
		}
		return nil
	})
	rec.check("actions", "actions.getGithubActionsPermissionsRepository", "GET /repos/{owner}/{repo}/actions/permissions", func() error {
		_, _, err := client.Repositories.GetActionsPermissions(ctx, set.owner, set.repo)
		return err
	})
	rec.check("actions", "actions.listSelfHostedRunnersForRepo", "GET /repos/{owner}/{repo}/actions/runners", func() error {
		_, _, err := client.Actions.ListRunners(ctx, set.owner, set.repo, nil)
		return err
	})
	rec.check("actions", "actions.getActionsCacheUsage", "GET /repos/{owner}/{repo}/actions/cache/usage", func() error {
		_, _, err := client.Actions.GetCacheUsageForRepo(ctx, set.owner, set.repo)
		return err
	})
	rec.check("actions", "actions.getActionsCacheList", "GET /repos/{owner}/{repo}/actions/caches", func() error {
		_, _, err := client.Actions.ListCaches(ctx, set.owner, set.repo, nil)
		return err
	})
	rec.check("actions", "actions.listEnvironments", "GET /repos/{owner}/{repo}/environments", func() error {
		_, _, err := client.Repositories.ListEnvironments(ctx, set.owner, set.repo, nil)
		return err
	})
	rec.check("actions", "actions.createOrUpdateEnvironment", "PUT /repos/{owner}/{repo}/environments/{name}", func() error {
		environment, _, err := client.Repositories.CreateUpdateEnvironment(ctx, set.owner, set.repo, "conformance", &github.CreateUpdateEnvironment{})
		if err != nil {
			return err
		}
		return wantField("environment.name", environment.GetName())
	})
}

func runOrgs(client *github.Client, rec *recorder, set *fixtureSet) {
	if set.org == "" {
		rec.skip1("orgs", "orgs.get", "GET /orgs/{org}", "organization fixture unavailable")
		return
	}
	rec.check("orgs", "orgs.get", "GET /orgs/{org}", func() error {
		org, _, err := client.Organizations.Get(ctx, set.org)
		if err != nil {
			return err
		}
		if org.GetLogin() != set.org {
			return deviate(set.org, org.GetLogin(), "organization login is wrong")
		}
		if org.GetType() != "Organization" {
			return deviate("Organization", org.GetType(), "organization type is wrong")
		}
		return wantField("organization.node_id", org.GetNodeID())
	})
	rec.check("orgs", "orgs.listForAuthenticatedUser", "GET /user/orgs", func() error {
		orgs, _, err := client.Organizations.List(ctx, "", nil)
		if err != nil {
			return err
		}
		for _, org := range orgs {
			if org.GetLogin() == set.org {
				return nil
			}
		}
		return deviate("listing contains "+set.org, fmt.Sprintf("%d organizations", len(orgs)),
			"the authenticated user's organization listing omits the fixture organization")
	})
	rec.check("orgs", "orgs.listMembers", "GET /orgs/{org}/members", func() error {
		members, _, err := client.Organizations.ListMembers(ctx, set.org, nil)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return deviate("at least the admin member", "empty list", "organization has no members")
		}
		return nil
	})
	rec.check("orgs", "repos.listForOrg", "GET /orgs/{org}/repos", func() error {
		_, _, err := client.Repositories.ListByOrg(ctx, set.org, nil)
		return err
	})
	rec.check("orgs", "teams.create", "POST /orgs/{org}/teams", func() error {
		team, _, err := client.Teams.CreateTeam(ctx, set.org, github.NewTeam{
			Name: "conformance-team", Privacy: github.Ptr("closed"),
		})
		if err != nil {
			return err
		}
		if team.GetSlug() == "" {
			return deviate("slug populated", "empty", "created team has no slug")
		}
		return nil
	})
	rec.check("orgs", "teams.list", "GET /orgs/{org}/teams", func() error {
		teams, _, err := client.Teams.ListTeams(ctx, set.org, nil)
		if err != nil {
			return err
		}
		if len(teams) == 0 {
			return deviate("the team just created", "empty list", "team listing is empty")
		}
		return nil
	})
	rec.check("orgs", "orgs.listWebhooks", "GET /orgs/{org}/hooks", func() error {
		_, _, err := client.Organizations.ListHooks(ctx, set.org, nil)
		return err
	})
	rec.check("orgs", "actions.getOrgPublicKey", "GET /orgs/{org}/actions/secrets/public-key", func() error {
		_, _, err := client.Actions.GetOrgPublicKey(ctx, set.org)
		return err
	})
	rec.check("orgs", "orgs.listMembershipsForAuthenticatedUser", "GET /user/memberships/orgs", func() error {
		_, _, err := client.Organizations.ListOrgMemberships(ctx, nil)
		return err
	})
}

func runSearch(client *github.Client, rec *recorder, set *fixtureSet) {
	rec.check("search", "search.repos", "GET /search/repositories", func() error {
		result, _, err := client.Search.Repositories(ctx, "conformance", nil)
		if err != nil {
			return err
		}
		if result.GetTotal() == 0 {
			return deviate("at least the fixture repository", "total_count 0", "repository search found nothing")
		}
		if result.IncompleteResults == nil {
			return deviate("incomplete_results present", "absent", "search response omits incomplete_results")
		}
		return nil
	})
	rec.check("search", "search.issuesAndPullRequests", "GET /search/issues", func() error {
		result, _, err := client.Search.Issues(ctx, "conformance", nil)
		if err != nil {
			return err
		}
		if result.GetTotal() == 0 {
			return deviate("at least the fixture issue", "total_count 0", "issue search found nothing")
		}
		return nil
	})
	rec.check("search", "search.users", "GET /search/users", func() error {
		result, _, err := client.Search.Users(ctx, "admin", nil)
		if err != nil {
			return err
		}
		if result.GetTotal() == 0 {
			return deviate("at least the admin user", "total_count 0", "user search found nothing")
		}
		return nil
	})
	rec.check("search", "search.code", "GET /search/code", func() error {
		_, _, err := client.Search.Code(ctx, "conformance", nil)
		return err
	})
	rec.check("search", "search.commits", "GET /search/commits", func() error {
		_, _, err := client.Search.Commits(ctx, "conformance", nil)
		return err
	})
	rec.check("search", "search.labels", "GET /search/labels", func() error {
		if set.repo == "" {
			return deviate("a repository to search labels in", "none", "repository fixture unavailable")
		}
		repo, _, err := client.Repositories.Get(ctx, set.owner, set.repo)
		if err != nil {
			return err
		}
		_, _, err = client.Search.Labels(ctx, repo.GetID(), "conformance", nil)
		return err
	})
	rec.check("search", "search.topics", "GET /search/topics", func() error {
		_, _, err := client.Search.Topics(ctx, "conformance", nil)
		return err
	})
}

func runGists(client *github.Client, rec *recorder) {
	var gistID string
	rec.check("gists", "gists.create", "POST /gists", func() error {
		gist, _, err := client.Gists.Create(ctx, &github.Gist{
			Description: github.Ptr("conformance gist"),
			Public:      github.Ptr(true),
			Files: map[github.GistFilename]github.GistFile{
				"conformance.txt": {Content: github.Ptr("gist content\n")},
			},
		})
		if err != nil {
			return err
		}
		gistID = gist.GetID()
		if gistID == "" {
			return deviate("id populated", "empty", "created gist has no id")
		}
		if gist.GetHTMLURL() == "" {
			return deviate("html_url populated", "empty", "created gist has no html_url")
		}
		return nil
	})
	rec.check("gists", "gists.list", "GET /gists", func() error {
		gists, _, err := client.Gists.List(ctx, "", nil)
		if err != nil {
			return err
		}
		if len(gists) == 0 {
			return deviate("the gist just created", "empty list", "gist listing is empty")
		}
		return nil
	})
	if gistID == "" {
		return
	}
	rec.check("gists", "gists.get", "GET /gists/{id}", func() error {
		gist, _, err := client.Gists.Get(ctx, gistID)
		if err != nil {
			return err
		}
		file, ok := gist.Files["conformance.txt"]
		if !ok {
			return deviate("files[conformance.txt]", fmt.Sprintf("%d files", len(gist.Files)), "gist lost its file")
		}
		if file.GetContent() != "gist content\n" {
			return deviate("gist content", truncate(file.GetContent()), "gist file content does not round-trip")
		}
		return nil
	})
	rec.check("gists", "gists.createComment", "POST /gists/{id}/comments", func() error {
		_, _, err := client.Gists.CreateComment(ctx, gistID, &github.GistComment{Body: github.Ptr("conformance")})
		return err
	})
	rec.check("gists", "gists.star", "PUT /gists/{id}/star", func() error {
		if _, err := client.Gists.Star(ctx, gistID); err != nil {
			return err
		}
		starred, _, err := client.Gists.IsStarred(ctx, gistID)
		if err != nil {
			return err
		}
		if !starred {
			return deviate("starred", "not starred", "the star was not recorded")
		}
		return nil
	})
	rec.check("gists", "gists.listCommits", "GET /gists/{id}/commits", func() error {
		_, _, err := client.Gists.ListCommits(ctx, gistID, nil)
		return err
	})
}

func runMisc(client *github.Client, rec *recorder, set *fixtureSet) {
	rec.check("pagination", "per_page and Link header", "GET /repos/{owner}/{repo}/issues?per_page=1", func() error {
		// Three extra issues so there is genuinely a second page to advertise;
		// asserting on a collection that fits in one page proves nothing.
		for index := 0; index < 3; index++ {
			if _, _, err := client.Issues.Create(ctx, set.owner, set.repo, &github.IssueRequest{
				Title: github.Ptr(fmt.Sprintf("pagination fixture %d", index)),
			}); err != nil {
				return err
			}
		}
		issues, resp, err := client.Issues.ListByRepo(ctx, set.owner, set.repo, &github.IssueListByRepoOptions{
			State:       "all",
			ListOptions: github.ListOptions{PerPage: 1},
		})
		if err != nil {
			return err
		}
		if len(issues) != 1 {
			return deviate("1 result", fmt.Sprintf("%d results", len(issues)), "per_page is ignored")
		}
		if resp.NextPage == 0 {
			return deviate("a Link header the client can follow", "no next page",
				"the response carries no RFC 5988 Link header, so client pagination stops after one page")
		}
		return nil
	})
	rec.check("pagination", "search results paginate", "GET /search/issues?per_page=1", func() error {
		result, resp, err := client.Search.Issues(ctx, "pagination", &github.SearchOptions{
			ListOptions: github.ListOptions{PerPage: 1},
		})
		if err != nil {
			return err
		}
		if result.GetTotal() <= 1 {
			return deviate("more than one matching issue", fmt.Sprintf("total_count %d", result.GetTotal()),
				"the search fixture did not produce enough matches to page over")
		}
		if resp.NextPage == 0 {
			return deviate("a Link header the client can follow", "no next page",
				"search responses carry no RFC 5988 Link header, so no client can read past the first page of results")
		}
		return nil
	})
	rec.check("errors", "404 shape", "GET /repos/{owner}/does-not-exist", func() error {
		_, resp, err := client.Repositories.Get(ctx, set.owner, "definitely-does-not-exist")
		if err == nil {
			return deviate("404 Not Found", "success", "a missing repository was served successfully")
		}
		if resp == nil || resp.StatusCode != http.StatusNotFound {
			return deviate("404 Not Found", err.Error(), "a missing repository does not return 404")
		}
		var apiErr *github.ErrorResponse
		if !errors.As(err, &apiErr) {
			return deviate("*github.ErrorResponse", fmt.Sprintf("%T", err), "the client could not decode the error body")
		}
		if apiErr.Message == "" {
			return deviate("message populated", "empty", "the error body has no message field")
		}
		if apiErr.DocumentationURL == "" {
			return deviate("documentation_url populated", "empty",
				"the error body omits documentation_url, which real GitHub always sends")
		}
		return nil
	})
	rec.check("errors", "422 validation shape", "POST /user/repos with an invalid body", func() error {
		_, _, err := client.Repositories.Create(ctx, "", &github.Repository{Name: github.Ptr("")})
		if err == nil {
			return deviate("422 Unprocessable Entity", "success", "an empty repository name was accepted")
		}
		var apiErr *github.ErrorResponse
		if !errors.As(err, &apiErr) {
			return deviate("*github.ErrorResponse", fmt.Sprintf("%T", err), "the client could not decode the error body")
		}
		if len(apiErr.Errors) == 0 {
			return deviate("errors array populated", "empty",
				"the validation error carries no errors array, which clients use to point at the offending field")
		}
		return nil
	})
	rec.check("errors", "401 shape", "GET /user with a bad token", func() error {
		bad, err := github.NewClient(
			github.WithAuthToken("clearly-not-a-valid-token"),
			github.WithEnterpriseURLs(baseURL+"/", baseURL+"/"),
		)
		if err != nil {
			return err
		}
		_, resp, err := bad.Users.Get(ctx, "")
		if err == nil {
			return deviate("401 Unauthorized", "success", "an invalid token was accepted")
		}
		if resp == nil || resp.StatusCode != http.StatusUnauthorized {
			return deviate("401 Unauthorized", err.Error(), "an invalid token does not produce 401")
		}
		return nil
	})
	rec.check("conditional", "ETag / If-None-Match", "GET /user twice", func() error {
		request, err := client.NewRequest(ctx, http.MethodGet, "user", nil)
		if err != nil {
			return err
		}
		first, err := client.Do(request, nil)
		if err != nil {
			return err
		}
		etag := first.Header.Get("ETag")
		if etag == "" {
			return deviate("an ETag header", "none", "responses carry no ETag, so conditional requests cannot work")
		}
		conditional, err := client.NewRequest(ctx, http.MethodGet, "user", nil)
		if err != nil {
			return err
		}
		conditional.Header.Set("If-None-Match", etag)
		second, err := client.Do(conditional, nil)
		if second != nil && second.StatusCode == http.StatusNotModified {
			return nil
		}
		if err != nil {
			return err
		}
		return deviate("304 Not Modified", fmt.Sprintf("%d", second.StatusCode),
			"a conditional request with a matching ETag was not answered with 304")
	})
}

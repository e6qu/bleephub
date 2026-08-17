package bleephub

import (
	"encoding/base64"
	"fmt"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// attachInstallationBlock injects `installation: {id, node_id}` at the top
// level of every event payload, mirroring what real GH does for events
// delivered through an App installation.
func attachInstallationBlock(payload map[string]interface{}, inst *store.Installation) map[string]interface{} {
	if inst == nil {
		return payload
	}
	payload["installation"] = map[string]interface{}{
		"id":      inst.ID,
		"node_id": installationNodeID(inst.ID),
	}
	return payload
}

// installationNodeID returns the GraphQL global node id for an installation,
// matching GitHub's scheme: base64 of the literal "012:Installation{id}".
func installationNodeID(id int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("012:Installation%d", id)))
}

// buildInstallationEventPayload builds the `installation` event payload
// (action: created | deleted | suspend | unsuspend | new_permissions_accepted).
// The app argument is retained for call-site symmetry with the other event
// builders; the app's identity is carried inside the installation object
// (app_id/app_slug), not as a top-level key, matching GitHub's wire shape.
func buildInstallationEventPayload(_ *store.App, action string, inst *store.Installation, sender *store.User) map[string]interface{} {
	repos := []map[string]interface{}{}
	return map[string]interface{}{
		"action":       action,
		"installation": installationToJSON(inst),
		"repositories": repos,
		"sender":       senderPayload(sender),
	}
}

// buildInstallationRepositoriesEventPayload builds installation_repositories
// (action: added | removed).
func buildInstallationRepositoriesEventPayload(app *store.App, action string, inst *store.Installation, repoIDsChanged []int, sender *store.User) map[string]interface{} {
	changes := []map[string]interface{}{}
	for _, id := range repoIDsChanged {
		changes = append(changes, map[string]interface{}{"id": id})
	}
	out := map[string]interface{}{
		"action":               action,
		"installation":         installationToJSON(inst),
		"repository_selection": inst.RepositorySelection,
		"sender":               senderPayload(sender),
	}
	switch action {
	case "added":
		out["repositories_added"] = changes
		out["repositories_removed"] = []map[string]interface{}{}
	case "removed":
		out["repositories_added"] = []map[string]interface{}{}
		out["repositories_removed"] = changes
	}
	return out
}

func buildPushPayload(st *store.Store, repo *store.Repo, sender *store.User, ref, before, after string) map[string]interface{} {
	repo = st.SnapRepo(repo)
	sender = st.SnapUser(sender)
	payload := buildPushPayloadWithInstallation(repo, sender, ref, before, after, nil)
	stor, _ := st.GitStorageForRepoID(repo.ID)
	oldHash := plumbing.NewHash(before)
	newHash := plumbing.NewHash(after)
	payload["forced"] = classifyRefUpdate(stor, oldHash, newHash) == "force_push"
	if !oldHash.IsZero() && !newHash.IsZero() {
		payload["compare"] = "/" + repo.FullName + "/compare/" + before + "..." + after
	}
	commits := pushCommitPayloads(stor, oldHash, newHash, repo.FullName)
	payload["commits"] = commits
	if len(commits) > 0 {
		payload["head_commit"] = commits[len(commits)-1]
	}
	payload["pusher"] = map[string]interface{}{
		"name":  coalesceUserLogin(sender),
		"email": coalesceUserEmail(sender),
	}
	return payload
}

func buildPushPayloadWithInstallation(repo *store.Repo, sender *store.User, ref, before, after string, inst *store.Installation) map[string]interface{} {
	return attachInstallationBlock(map[string]interface{}{
		"ref":         ref,
		"before":      before,
		"after":       after,
		"created":     before == "0000000000000000000000000000000000000000",
		"deleted":     after == "0000000000000000000000000000000000000000",
		"forced":      false,
		"compare":     "",
		"commits":     []interface{}{},
		"head_commit": nil,
		"repository":  repoPayload(repo),
		"sender":      senderPayload(sender),
	}, inst)
}

func pushCommitPayloads(stor gitStorage.Storer, before, after plumbing.Hash, repoFullName string) []map[string]interface{} {
	if stor == nil || after.IsZero() {
		return []map[string]interface{}{}
	}
	afterCommit, err := object.GetCommit(stor, after)
	if err != nil {
		return []map[string]interface{}{}
	}
	commits := []*object.Commit{}
	iter := object.NewCommitPreorderIter(afterCommit, nil, nil)
	_ = iter.ForEach(func(commit *object.Commit) error {
		if !before.IsZero() && commit.Hash == before {
			return storer.ErrStop
		}
		commits = append(commits, commit)
		if len(commits) >= 2048 {
			return storer.ErrStop
		}
		return nil
	})
	out := make([]map[string]interface{}, 0, len(commits))
	for i := len(commits) - 1; i >= 0; i-- {
		commit := commits[i]
		added, removed, modified := commitFileChanges(commit)
		out = append(out, map[string]interface{}{
			"id":        commit.Hash.String(),
			"tree_id":   commit.TreeHash.String(),
			"distinct":  true,
			"message":   commit.Message,
			"timestamp": commit.Author.When.UTC().Format(time.RFC3339),
			"url":       "/" + repoFullName + "/commit/" + commit.Hash.String(),
			"author": map[string]interface{}{
				"name":     commit.Author.Name,
				"email":    commit.Author.Email,
				"username": nil,
			},
			"committer": map[string]interface{}{
				"name":     commit.Committer.Name,
				"email":    commit.Committer.Email,
				"username": nil,
			},
			"added":    added,
			"removed":  removed,
			"modified": modified,
		})
	}
	return out
}

// commitFileChanges diffs a commit against its first parent to fill a push
// event's per-commit added/removed/modified paths. GitHub populates these and
// consumers (CI path filters, deploy bots) branch on them, so an empty set is a
// fidelity break. A root commit (no parent) reports every file as added.
func commitFileChanges(commit *object.Commit) (added, removed, modified []string) {
	added, removed, modified = []string{}, []string{}, []string{}
	commitTree, err := commit.Tree()
	if err != nil {
		return
	}
	if commit.NumParents() == 0 {
		_ = commitTree.Files().ForEach(func(f *object.File) error {
			added = append(added, f.Name)
			return nil
		})
		return
	}
	parent, err := commit.Parent(0)
	if err != nil {
		return
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return
	}
	changes, err := object.DiffTree(parentTree, commitTree)
	if err != nil {
		return
	}
	for _, c := range changes {
		switch {
		case c.From.Name == "":
			added = append(added, c.To.Name)
		case c.To.Name == "":
			removed = append(removed, c.From.Name)
		default:
			modified = append(modified, c.To.Name)
		}
	}
	return
}

func coalesceUserLogin(user *store.User) string {
	if user == nil || user.Login == "" {
		return "web-flow"
	}
	return user.Login
}

func coalesceUserEmail(user *store.User) string {
	if user == nil || user.Email == "" {
		return "noreply@github.com"
	}
	return user.Email
}

func buildPullRequestPayload(st *store.Store, repo *store.Repo, pr *store.PullRequest, sender *store.User, action string) map[string]interface{} {
	// Snapshot the shared entities before reading their mutable fields.
	headRepo := store.PullRequestHeadRepo(st, pr)
	repo = st.SnapRepo(repo)
	headRepo = st.SnapRepo(headRepo)
	pr = st.SnapPR(pr)
	sender = st.SnapUser(sender)
	state := "open"
	if pr.State == "CLOSED" || pr.State == "MERGED" {
		state = "closed"
	}

	prJSON := map[string]interface{}{
		"id":      pr.ID,
		"node_id": pr.NodeID,
		"number":  pr.Number,
		"title":   pr.Title,
		"body":    pr.Body,
		"state":   state,
		"locked":  pr.Locked,
		"user":    senderPayload(st.GetUserByID(pr.AuthorID)),
		"draft":   pr.IsDraft,
		"merged":  pr.State == "MERGED",
		// A merged PR reports its real merge commit; an open one reports its
		// test-merge commit (GitHub's "potential merge"), so a pull_request
		// workflow run runs against the merge ref rather than the head (ACT-027).
		// Both fall back to the head SHA when no merge commit is available.
		"merge_commit_sha": store.CoalesceStr(pr.MergeCommitSHA, store.CoalesceStr(pr.PotentialMergeCommitSHA, pullRequestHeadSHA(pr, st))),
		"head": map[string]interface{}{
			"ref":  pr.HeadRefName,
			"sha":  pullRequestHeadSHA(pr, st),
			"repo": repoPayload(headRepo),
		},
		"base": map[string]interface{}{
			"ref":  pr.BaseRefName,
			"sha":  pr.BaseSHA,
			"repo": repoPayload(repo),
		},
		"created_at": pr.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": pr.UpdatedAt.UTC().Format(time.RFC3339),
	}

	if pr.MergedAt != nil {
		prJSON["merged_at"] = pr.MergedAt.UTC().Format(time.RFC3339)
	}
	if pr.ClosedAt != nil {
		prJSON["closed_at"] = pr.ClosedAt.UTC().Format(time.RFC3339)
	}

	return buildPullRequestPayloadInner(action, pr, prJSON, repo, sender, nil)
}

func buildPullRequestPayloadInner(action string, pr *store.PullRequest, prJSON map[string]interface{}, repo *store.Repo, sender *store.User, inst *store.Installation) map[string]interface{} {
	return attachInstallationBlock(map[string]interface{}{
		"action":       action,
		"number":       pr.Number,
		"pull_request": prJSON,
		"repository":   repoPayload(repo),
		"sender":       senderPayload(sender),
	}, inst)
}

func buildIssuesPayload(st *store.Store, repo *store.Repo, issue *store.Issue, sender *store.User, action string) map[string]interface{} {
	// Snapshot the shared entities before reading their mutable fields.
	repo = st.SnapRepo(repo)
	issue = st.SnapIssue(issue)
	sender = st.SnapUser(sender)
	state := "open"
	if issue.State == "CLOSED" {
		state = "closed"
	}

	issueJSON := map[string]interface{}{
		"id":         issue.ID,
		"node_id":    issue.NodeID,
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      state,
		"locked":     issue.Locked,
		"user":       senderPayload(st.GetUserByID(issue.AuthorID)),
		"created_at": issue.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": issue.UpdatedAt.UTC().Format(time.RFC3339),
	}

	if issue.ClosedAt != nil {
		issueJSON["closed_at"] = issue.ClosedAt.UTC().Format(time.RFC3339)
	}

	return attachInstallationBlock(map[string]interface{}{
		"action":     action,
		"issue":      issueJSON,
		"repository": repoPayload(repo),
		"sender":     senderPayload(sender),
	}, nil)
}

func buildIssueCommentPayload(
	st *store.Store,
	repo *store.Repo,
	comment *store.Comment,
	sender *store.User,
	action, baseURL string,
	parentNumber int,
) map[string]interface{} {
	repo = st.SnapRepo(repo)
	comment = st.SnapComment(comment)
	sender = st.SnapUser(sender)
	var issueJSON map[string]interface{}
	switch comment.ParentType {
	case "pull_request":
		if pr := st.GetPullRequest(comment.IssueID); pr != nil {
			issueJSON = issueToJSONForPR(st.SnapPR(pr), st, baseURL, repo.FullName)
		}
	default:
		if issue := st.GetIssue(comment.IssueID); issue != nil {
			issueJSON = issueToJSON(st.SnapIssue(issue), st, baseURL, repo.FullName)
		}
	}
	return attachInstallationBlock(map[string]interface{}{
		"action":     action,
		"issue":      issueJSON,
		"comment":    store.CommentToJSON(comment, st, baseURL, repo.FullName, parentNumber),
		"repository": repoPayload(repo),
		"sender":     senderPayload(sender),
	}, nil)
}

func buildPullRequestReviewPayload(
	st *store.Store,
	repo *store.Repo,
	pr *store.PullRequest,
	review *store.PullRequestReview,
	sender *store.User,
	action, baseURL string,
) map[string]interface{} {
	repo = st.SnapRepo(repo)
	pr = st.SnapPR(pr)
	review = st.SnapPullRequestReview(review)
	sender = st.SnapUser(sender)
	payload := buildPullRequestPayload(st, repo, pr, sender, action)
	payload["review"] = reviewToJSON(review, st, baseURL, repo.FullName, pr.Number)
	return payload
}

func buildPingPayload(repo *store.Repo, hook *store.Webhook, sender *store.User) map[string]interface{} {
	payload := map[string]interface{}{
		"zen":     "Keep it logically awesome.",
		"hook_id": hook.ID,
		"hook": map[string]interface{}{
			"id":     hook.ID,
			"type":   "Repository",
			"name":   "web",
			"active": hook.Active,
			"events": hook.Events,
			"config": map[string]interface{}{
				"url":          hook.URL,
				"content_type": store.CoalesceStr(hook.ContentType, "json"),
			},
		},
		// GitHub's ping event carries the acting user like every other event.
		"sender": senderPayload(sender),
	}
	if repo != nil {
		payload["repository"] = repoPayload(repo)
	}
	return payload
}

func repoPayload(repo *store.Repo) map[string]interface{} {
	if repo == nil {
		return nil
	}
	// Hypermedia is relative, matching the sender object (store.UserToJSON) that
	// already ships on every payload; the whole webhook body uses one convention.
	result := map[string]interface{}{
		"id":             repo.ID,
		"node_id":        repo.NodeID,
		"name":           repo.Name,
		"full_name":      repo.FullName,
		"private":        repo.Private,
		"url":            "/api/v3/repos/" + repo.FullName,
		"html_url":       "/" + repo.FullName,
		"description":    repo.Description,
		"fork":           repo.Fork,
		"default_branch": repo.DefaultBranch,
		"created_at":     repo.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":     repo.UpdatedAt.UTC().Format(time.RFC3339),
		"pushed_at":      repo.PushedAt.UTC().Format(time.RFC3339),
	}
	if repo.Owner != nil {
		result["owner"] = senderPayload(repo.Owner)
	}
	return result
}

// orgWebhookPayload is the `organization` block on event payloads for
// org-owned repos.
func orgWebhookPayload(org *store.Org) map[string]interface{} {
	return map[string]interface{}{
		"login":       org.Login,
		"id":          org.ID,
		"node_id":     org.NodeID,
		"url":         "/api/v3/orgs/" + org.Login,
		"avatar_url":  org.AvatarURL,
		"description": org.Description,
	}
}

func senderPayload(user *store.User) map[string]interface{} {
	if user == nil {
		// GitHub guarantees `sender` is always a populated user object. Events
		// with no originating user (e.g. system-driven pushes) fall back to the
		// deterministic "ghost" actor GitHub uses for absent accounts.
		return ghostSenderPayload()
	}
	copy := *user
	if copy.Type == "" {
		copy.Type = "User"
	}
	return store.UserToJSON(&copy)
}

// ghostSenderPayload returns GitHub's "ghost" deleted-user actor, used as the
// sender for events that have no originating user account.
func ghostSenderPayload() map[string]interface{} {
	return store.UserToJSON(nil)
}

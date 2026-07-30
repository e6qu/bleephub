package bleephub

import (
	"encoding/base64"
	"fmt"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// snapRepo / snapIssue / snapPR / snapUser return shallow value copies of a
// shared store entity taken under the store read lock. Webhook payload
// builders read mutable scalar fields (title, body, state, description,
// timestamps) off these entities; a concurrent Update* writer mutates the
// same fields under st.mu.Lock, so the builders must read a private copy
// rather than the live pointer. Each snapshot takes and releases the read
// lock independently — they are never nested, so a queued writer cannot
// deadlock them (sync.RWMutex read locks are not reentrant).
func (st *Store) snapRepo(r *Repo) *Repo {
	if r == nil {
		return nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	cp := *r
	return &cp
}

func (st *Store) snapIssue(i *Issue) *Issue {
	if i == nil {
		return nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	cp := *i
	cp.AssigneeIDs = append([]int(nil), i.AssigneeIDs...)
	cp.LabelIDs = append([]int(nil), i.LabelIDs...)
	return &cp
}

func (st *Store) snapPR(pr *PullRequest) *PullRequest {
	if pr == nil {
		return nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	cp := *pr
	cp.AssigneeIDs = append([]int(nil), pr.AssigneeIDs...)
	cp.LabelIDs = append([]int(nil), pr.LabelIDs...)
	cp.RequestedReviewerIDs = append([]int(nil), pr.RequestedReviewerIDs...)
	cp.RequestedTeamIDs = append([]int(nil), pr.RequestedTeamIDs...)
	return &cp
}

func (st *Store) snapComment(comment *Comment) *Comment {
	if comment == nil {
		return nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	cp := *comment
	return &cp
}

func (st *Store) snapPullRequestReview(review *PullRequestReview) *PullRequestReview {
	if review == nil {
		return nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	cp := *review
	return &cp
}

func (st *Store) snapUser(u *User) *User {
	if u == nil {
		return nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	cp := *u
	return &cp
}

// attachInstallationBlock injects `installation: {id, node_id}` at the top
// level of every event payload, mirroring what real GH does for events
// delivered through an App installation.
func attachInstallationBlock(payload map[string]interface{}, inst *Installation) map[string]interface{} {
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
func buildInstallationEventPayload(_ *App, action string, inst *Installation, sender *User) map[string]interface{} {
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
func buildInstallationRepositoriesEventPayload(app *App, action string, inst *Installation, repoIDsChanged []int, sender *User) map[string]interface{} {
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

func buildPushPayload(st *Store, repo *Repo, sender *User, ref, before, after string) map[string]interface{} {
	repo = st.snapRepo(repo)
	sender = st.snapUser(sender)
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

func buildPushPayloadWithInstallation(repo *Repo, sender *User, ref, before, after string, inst *Installation) map[string]interface{} {
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
			"added":    []string{},
			"removed":  []string{},
			"modified": []string{},
		})
	}
	return out
}

func coalesceUserLogin(user *User) string {
	if user == nil || user.Login == "" {
		return "web-flow"
	}
	return user.Login
}

func coalesceUserEmail(user *User) string {
	if user == nil || user.Email == "" {
		return "noreply@github.com"
	}
	return user.Email
}

func buildPullRequestPayload(st *Store, repo *Repo, pr *PullRequest, sender *User, action string) map[string]interface{} {
	// Snapshot the shared entities before reading their mutable fields.
	headRepo := pullRequestHeadRepo(st, pr)
	repo = st.snapRepo(repo)
	headRepo = st.snapRepo(headRepo)
	pr = st.snapPR(pr)
	sender = st.snapUser(sender)
	state := "open"
	if pr.State == "CLOSED" || pr.State == "MERGED" {
		state = "closed"
	}

	prJSON := map[string]interface{}{
		"number":           pr.Number,
		"title":            pr.Title,
		"body":             pr.Body,
		"state":            state,
		"draft":            pr.IsDraft,
		"merged":           pr.State == "MERGED",
		"merge_commit_sha": coalesceStr(pr.MergeCommitSHA, pullRequestHeadSHA(pr, st)),
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

func buildPullRequestPayloadInner(action string, pr *PullRequest, prJSON map[string]interface{}, repo *Repo, sender *User, inst *Installation) map[string]interface{} {
	return attachInstallationBlock(map[string]interface{}{
		"action":       action,
		"number":       pr.Number,
		"pull_request": prJSON,
		"repository":   repoPayload(repo),
		"sender":       senderPayload(sender),
	}, inst)
}

func buildIssuesPayload(st *Store, repo *Repo, issue *Issue, sender *User, action string) map[string]interface{} {
	// Snapshot the shared entities before reading their mutable fields.
	repo = st.snapRepo(repo)
	issue = st.snapIssue(issue)
	sender = st.snapUser(sender)
	state := "open"
	if issue.State == "CLOSED" {
		state = "closed"
	}

	issueJSON := map[string]interface{}{
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      state,
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
	st *Store,
	repo *Repo,
	comment *Comment,
	sender *User,
	action, baseURL string,
	parentNumber int,
) map[string]interface{} {
	repo = st.snapRepo(repo)
	comment = st.snapComment(comment)
	sender = st.snapUser(sender)
	var issueJSON map[string]interface{}
	switch comment.ParentType {
	case "pull_request":
		if pr := st.GetPullRequest(comment.IssueID); pr != nil {
			issueJSON = issueToJSONForPR(st.snapPR(pr), st, baseURL, repo.FullName)
		}
	default:
		if issue := st.GetIssue(comment.IssueID); issue != nil {
			issueJSON = issueToJSON(st.snapIssue(issue), st, baseURL, repo.FullName)
		}
	}
	return attachInstallationBlock(map[string]interface{}{
		"action":     action,
		"issue":      issueJSON,
		"comment":    commentToJSON(comment, st, baseURL, repo.FullName, parentNumber),
		"repository": repoPayload(repo),
		"sender":     senderPayload(sender),
	}, nil)
}

func buildPullRequestReviewPayload(
	st *Store,
	repo *Repo,
	pr *PullRequest,
	review *PullRequestReview,
	sender *User,
	action, baseURL string,
) map[string]interface{} {
	repo = st.snapRepo(repo)
	pr = st.snapPR(pr)
	review = st.snapPullRequestReview(review)
	sender = st.snapUser(sender)
	payload := buildPullRequestPayload(st, repo, pr, sender, action)
	payload["review"] = reviewToJSON(review, st, baseURL, repo.FullName, pr.Number)
	return payload
}

func buildPingPayload(repo *Repo, hook *Webhook) map[string]interface{} {
	payload := map[string]interface{}{
		"zen":     "Keep it logically awesome.",
		"hook_id": hook.ID,
		"hook": map[string]interface{}{
			"id":     hook.ID,
			"type":   "Repository",
			"active": hook.Active,
			"events": hook.Events,
			"config": map[string]interface{}{
				"url":          hook.URL,
				"content_type": "json",
			},
		},
	}
	if repo != nil {
		payload["repository"] = repoPayload(repo)
	}
	return payload
}

func repoPayload(repo *Repo) map[string]interface{} {
	if repo == nil {
		return nil
	}
	result := map[string]interface{}{
		"id":             repo.ID,
		"name":           repo.Name,
		"full_name":      repo.FullName,
		"private":        repo.Private,
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
func orgWebhookPayload(org *Org) map[string]interface{} {
	return map[string]interface{}{
		"login":       org.Login,
		"id":          org.ID,
		"node_id":     org.NodeID,
		"avatar_url":  org.AvatarURL,
		"description": org.Description,
	}
}

func senderPayload(user *User) map[string]interface{} {
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
	return userToJSON(&copy)
}

// ghostSenderPayload returns GitHub's "ghost" deleted-user actor, used as the
// sender for events that have no originating user account.
func ghostSenderPayload() map[string]interface{} {
	return userToJSON(nil)
}

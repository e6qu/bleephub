package bleephub

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
)

func (s *Server) afterCommittedRefUpdate(repo *store.Repo, sender *store.User, ref, before, after, baseURL string) {
	if repo == nil {
		return
	}
	stor, _ := s.store.GitStorageForRepoID(repo.ID)
	if owner, name, ok := store.SplitRepoFullName(repo.FullName); ok {
		s.store.UpdateRepo(owner, name, func(current *store.Repo) {
			current.PushedAt = time.Now().UTC()
		})
	}
	actorID := 0
	if sender != nil {
		actorID = sender.ID
	}
	oldHash := plumbing.NewHash(before)
	newHash := plumbing.NewHash(after)
	s.store.RecordRepoActivity(repo.ID, ref, before, after, actorID, classifyRefUpdate(stor, oldHash, newHash))
	payload := buildPushPayload(s.store, repo, sender, ref, before, after, baseURL)
	s.emitWebhookEvent(repo.FullName, "push", "", payload)
	s.recordCommitIssueReferences(repo, actorID, payload)
	if branch := strings.TrimPrefix(ref, "refs/heads/"); branch != ref {
		s.recordPullRequestHeadRefLifecycle(repo, actorID, branch, before, after)
		s.firePullRequestSynchronize(repo, repo.FullName, branch)
	}
	s.triggerPagesBuildForRef(repo, sender, ref, baseURL)
}

// recordCommitIssueReferences records github's `referenced` timeline event for
// every issue or pull request a pushed commit's message names. The push
// payload already carries the commits this update introduced, so the reference
// is read off the same message the push webhook delivers rather than by
// re-walking history at read time.
//
// A commit reaching a second branch is not a second reference: the event is
// keyed on (subject, commit), so re-pushing an already-referenced commit adds
// nothing.
func (s *Server) recordCommitIssueReferences(repo *store.Repo, actorID int, payload map[string]interface{}) {
	commits, _ := payload["commits"].([]map[string]interface{})
	for _, commit := range commits {
		sha, _ := commit["id"].(string)
		message, _ := commit["message"].(string)
		if sha == "" || message == "" {
			continue
		}
		seen := map[int]bool{}
		for _, match := range timelineRefRE.FindAllStringSubmatch(message, -1) {
			if match[1] != "" && !strings.EqualFold(match[1], repo.FullName) {
				continue
			}
			number, err := strconv.Atoi(match[2])
			if err != nil || number <= 0 || seen[number] {
				continue
			}
			seen[number] = true
			if s.commitAlreadyReferenced(repo, number, sha) {
				continue
			}
			s.store.RecordIssueOrPREvent(repo.ID, number, actorID, "referenced", map[string]interface{}{
				"commit_id": sha,
			})
		}
	}
}

// commitAlreadyReferenced reports whether this commit has already been
// recorded as referencing the given issue or pull-request number.
func (s *Server) commitAlreadyReferenced(repo *store.Repo, number int, sha string) bool {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	subjectID := 0
	for _, issue := range s.store.Issues {
		if issue.RepoID == repo.ID && issue.Number == number {
			subjectID = issue.ID
			break
		}
	}
	if subjectID == 0 {
		for _, pr := range s.store.PullRequests {
			if pr.RepoID == repo.ID && pr.Number == number {
				subjectID = pr.ID
				break
			}
		}
	}
	if subjectID == 0 {
		return true
	}
	for _, event := range s.store.IssueEvents {
		if event.RepoID == repo.ID && event.IssueID == subjectID &&
			event.Event == "referenced" && event.CommitID == sha {
			return true
		}
	}
	return false
}

// recordPullRequestHeadRefLifecycle records github's head_ref_deleted /
// head_ref_restored timeline events for every pull request whose head branch
// this ref update removed or brought back. Deleting the branch a pull request
// was opened from is an ordinary part of merging one, and the pull request's
// history is where GitHub reports it; nothing recorded it before, so a merged
// pull request whose branch was tidied away showed no trace of it.
func (s *Server) recordPullRequestHeadRefLifecycle(repo *store.Repo, actorID int, branch, before, after string) {
	zero := plumbing.ZeroHash.String()
	deleted := after == zero && before != zero
	restored := before == zero && after != zero
	if !deleted && !restored {
		return
	}
	event := "head_ref_restored"
	if deleted {
		event = "head_ref_deleted"
	}
	s.store.Mu.RLock()
	var prIDs []int
	for _, pr := range s.store.PullRequests {
		if store.PullRequestHeadRepoID(pr) == repo.ID && pr.HeadRefName == branch {
			prIDs = append(prIDs, pr.ID)
		}
	}
	s.store.Mu.RUnlock()
	sort.Ints(prIDs)
	for _, prID := range prIDs {
		pr := s.store.GetPullRequest(prID)
		if pr == nil {
			continue
		}
		s.store.RecordPullRequestEvent(pr.RepoID, pr.ID, actorID, event, "", 0)
	}
}

func (s *Server) triggerPagesBuildForRef(repo *store.Repo, sender *store.User, ref, baseURL string) {
	s.store.Misc.Mu.RLock()
	site := s.store.Misc.PagesByRepo[repo.ID]
	buildType := ""
	branch := ""
	if site != nil {
		if site.BuildType != nil {
			buildType = *site.BuildType
		}
		branch, _ = site.Source["branch"].(string)
	}
	s.store.Misc.Mu.RUnlock()
	if site == nil || buildType != "legacy" || ref != "refs/heads/"+branch {
		return
	}
	actor := "bleephub-system"
	var pusher *store.PagesPusher
	if sender != nil {
		actor = sender.Login
		pusher = &store.PagesPusher{Login: sender.Login, ID: sender.ID, Type: store.CoalesceStr(sender.Type, "User")}
	}
	_, _ = s.runPagesBuild(context.Background(), repo, pusher, actor, baseURL)
}

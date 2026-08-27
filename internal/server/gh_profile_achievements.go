package bleephub

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/e6qu/bleephub/internal/store"
)

// Profile achievements (Pull Shark, YOLO, …). GitHub has no public API for
// these, so they live under browser-only /ui-data, not an invented /api/v3 path.
// GET /ui-data/users/{login}/achievements returns 200 [{slug, name, tier,
// count}], derived on demand from stored state; an empty list is a 200 [].
func (s *Server) registerGHAchievementsRoutes() {
	s.route("GET /ui-data/users/{login}/achievements", s.handleListUserAchievements)
}

type profileAchievement struct {
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	Tier  int    `json:"tier"`
	Count int    `json:"count"`
}

// achievementTier is the number of thresholds reached (0 = not earned).
func achievementTier(count int, thresholds []int) int {
	tier := 0
	for _, threshold := range thresholds {
		if count >= threshold {
			tier++
		}
	}
	return tier
}

func appendAchievement(out []profileAchievement, slug, name string, count int, thresholds []int) []profileAchievement {
	tier := achievementTier(count, thresholds)
	if tier == 0 {
		return out
	}
	return append(out, profileAchievement{Slug: slug, Name: name, Tier: tier, Count: count})
}

// mergedPRForCoauthorScan is a detached snapshot the pair-extraordinaire git
// scan reads after the store lock is released (STORE-021).
type mergedPRForCoauthorScan struct {
	baseRepoFullName string
	headRepoFullName string
	mergeCommitSHA   string
	headRefName      string
	baseRefName      string
	baseSHA          string
}

func (s *Server) handleListUserAchievements(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	user := s.store.LookupUserByLogin(login)
	if user == nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	uid := user.ID

	const quickdrawWindow = 5 * time.Minute

	var (
		mergedPRs   int // pull-shark
		yoloMerges  int // merged with zero APPROVED reviews at merge time
		quickdraws  int // issues/PRs the user opened and that closed within 5 minutes
		answers     int // galaxy-brain: discussion comments marked as answer
		topStars    int // starstruck: the most-starred repo the user owns
		coauthScans []mergedPRForCoauthorScan
	)

	s.store.Mu.RLock()
	for _, pr := range s.store.PullRequests {
		if pr.AuthorID != uid {
			continue
		}
		if pr.ClosedAt != nil && pr.ClosedAt.Sub(pr.CreatedAt) <= quickdrawWindow {
			quickdraws++
		}
		if pr.State != "MERGED" {
			continue
		}
		mergedPRs++
		approvedAtMerge := false
		for _, review := range s.store.PRReviewsByPR[pr.ID] {
			if review.State != "APPROVED" {
				continue
			}
			// A review submitted after the merge cannot have approved it.
			if pr.MergedAt != nil && review.SubmittedAt != nil && review.SubmittedAt.After(*pr.MergedAt) {
				continue
			}
			approvedAtMerge = true
			break
		}
		if !approvedAtMerge {
			yoloMerges++
		}
		scan := mergedPRForCoauthorScan{
			mergeCommitSHA: pr.MergeCommitSHA,
			headRefName:    pr.HeadRefName,
			baseRefName:    pr.BaseRefName,
			baseSHA:        pr.BaseSHA,
		}
		if baseRepo := s.store.Repos[pr.RepoID]; baseRepo != nil {
			scan.baseRepoFullName = baseRepo.FullName
		}
		if headRepo := s.store.Repos[store.PullRequestHeadRepoID(pr)]; headRepo != nil {
			scan.headRepoFullName = headRepo.FullName
		}
		coauthScans = append(coauthScans, scan)
	}
	for _, issue := range s.store.Issues {
		if issue.AuthorID == uid && issue.ClosedAt != nil && issue.ClosedAt.Sub(issue.CreatedAt) <= quickdrawWindow {
			quickdraws++
		}
	}
	for _, comment := range s.store.DiscussionComments {
		if comment.AuthorID != uid || !comment.IsAnswer || comment.Deleted {
			continue
		}
		if discussion := s.store.Discussions[comment.DiscussionID]; discussion == nil || discussion.Deleted {
			continue
		}
		answers++
	}
	for _, repo := range s.store.Repos {
		if repo.OwnerID == uid && repo.OwnerType == "User" && repo.StargazersCount > topStars {
			topStars = repo.StargazersCount
		}
	}
	s.store.Mu.RUnlock()

	// pair-extraordinaire reads git objects, so it runs after the store lock is
	// released, over the detached snapshots.
	coauthoredPRs := 0
	for _, scan := range coauthScans {
		if s.mergedPRHasCoauthoredCommit(scan) {
			coauthoredPRs++
		}
	}

	out := make([]profileAchievement, 0, 6)
	out = appendAchievement(out, "pull-shark", "Pull Shark", mergedPRs, []int{2, 16, 128, 1024})
	out = appendAchievement(out, "pair-extraordinaire", "Pair Extraordinaire", coauthoredPRs, []int{1, 10, 24, 48})
	out = appendAchievement(out, "yolo", "YOLO", yoloMerges, []int{1})
	out = appendAchievement(out, "quickdraw", "Quickdraw", quickdraws, []int{1})
	out = appendAchievement(out, "galaxy-brain", "Galaxy Brain", answers, []int{2, 8, 16, 32})
	out = appendAchievement(out, "starstruck", "Starstruck", topStars, []int{16, 128, 512, 4096})
	writeJSON(w, http.StatusOK, out)
}

// mergedPRHasCoauthoredCommit reports whether any commit the PR carried — its
// branch commits or the merge commit itself (a squash merge folds the trailers
// in) — has a Co-authored-by trailer. Missing git storage yields false:
// achievements are best-effort, never errors.
func (s *Server) mergedPRHasCoauthoredCommit(scan mergedPRForCoauthorScan) bool {
	if scan.baseRepoFullName != "" && scan.mergeCommitSHA != "" {
		if baseOwner, baseName, ok := store.SplitRepoFullName(scan.baseRepoFullName); ok {
			if stor := s.store.GetGitStorage(baseOwner, baseName); stor != nil {
				if commit, err := object.GetCommit(stor, plumbing.NewHash(scan.mergeCommitSHA)); err == nil && commit != nil {
					if messageHasCoauthorTrailer(commit.Message) {
						return true
					}
				}
			}
		}
	}
	headOwner, headName, ok := store.SplitRepoFullName(scan.headRepoFullName)
	if !ok {
		return false
	}
	stor := s.store.GetGitStorage(headOwner, headName)
	if stor == nil {
		return false
	}
	prShape := &store.PullRequest{
		HeadRefName: scan.headRefName,
		BaseRefName: scan.baseRefName,
		BaseSHA:     scan.baseSHA,
	}
	commits, err := store.PullRequestCommitObjectsFromStorage(stor, prShape)
	if err != nil {
		return false
	}
	for _, commit := range commits {
		if messageHasCoauthorTrailer(commit.Message) {
			return true
		}
	}
	return false
}

// messageHasCoauthorTrailer reports whether a commit message has a
// Co-authored-by trailer, matched case-insensitively as git does.
func messageHasCoauthorTrailer(message string) bool {
	for _, line := range strings.Split(message, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) >= len("Co-authored-by:") && strings.EqualFold(trimmed[:len("Co-authored-by:")], "Co-authored-by:") {
			return true
		}
	}
	return false
}

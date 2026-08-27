package store

// Contribution aggregation for GraphQL User.contributionsCollection, computed
// from real store data. It composes the store's individually-locked readers
// rather than holding st.Mu itself, so it never deadlocks against them and
// always reads committed snapshots.

import (
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// ContributionDayKey is a calendar day rendered as "2006-01-02" (UTC).
type ContributionDayKey = string

// CommitContributionDay is one repository's commits by the user on one UTC day.
type CommitContributionDay struct {
	RepoID     int
	Date       time.Time // midnight UTC of the day
	Count      int
	OccurredAt time.Time // latest commit time on that day
}

// ContributionData is everything GraphQL's ContributionsCollection needs for
// one user over one window. Record slices are detached snapshots.
type ContributionData struct {
	UserID   int
	User     *User
	From, To time.Time
	Now      time.Time
	OrgID    int

	// Window-scoped contributions, sorted ascending by creation/submission.
	Issues       []*Issue
	PullRequests []*PullRequest
	Reviews      []*PullRequestReview
	ReviewRepoID map[int]int // review.ID -> repository ID
	Repos        []*Repo     // repositories the user created in the window

	CommitDays   []CommitContributionDay
	TotalCommits int

	// Distinct repositories the user contributed to in the window, per kind.
	ReposWithCommits map[int]bool
	ReposWithIssues  map[int]bool
	ReposWithPRs     map[int]bool
	ReposWithReviews map[int]bool

	// Comment counts for the window's issues/PRs, keyed by record ID; drive the
	// popular* selection and the excludePopular argument.
	IssueComments map[int]int
	PRComments    map[int]int

	// The user's all-time first issue/PR/repository. GraphQL's
	// first*Contribution surfaces these only when they fall inside the window.
	FirstIssue *Issue
	FirstPR    *PullRequest
	FirstRepo  *Repo

	// The window's most-commented issue and pull request.
	PopularIssue *Issue
	PopularPR    *PullRequest

	// Per-day contribution counts (commits + issues/PRs opened + reviews
	// submitted), keyed by "2006-01-02" (UTC).
	DayCounts map[ContributionDayKey]int

	// Distinct years with any all-time contribution, most recent first.
	ContributionYears []int

	HasActivityInThePast bool
}

// ContributionDate renders a time as its calendar day key.
func ContributionDate(t time.Time) ContributionDayKey {
	return t.UTC().Format("2006-01-02")
}

// ComputeContributions aggregates the user's contributions over [from, to]. A
// non-zero orgID restricts the aggregate to that organization's repositories.
func (st *Store) ComputeContributions(userID int, from, to time.Time, orgID int) *ContributionData {
	data := &ContributionData{
		UserID:           userID,
		From:             from,
		To:               to,
		Now:              st.CurrentTime(),
		OrgID:            orgID,
		ReviewRepoID:     map[int]int{},
		ReposWithCommits: map[int]bool{},
		ReposWithIssues:  map[int]bool{},
		ReposWithPRs:     map[int]bool{},
		ReposWithReviews: map[int]bool{},
		IssueComments:    map[int]int{},
		PRComments:       map[int]int{},
		DayCounts:        map[ContributionDayKey]int{},
	}
	user := st.GetUserByID(userID)
	if user == nil {
		return data
	}
	data.User = user

	years := map[int]bool{}

	inWindow := func(t time.Time) bool {
		return !t.Before(from) && !t.After(to)
	}
	bumpDay := func(t time.Time, n int) {
		data.DayCounts[ContributionDate(t)] += n
	}

	for _, repo := range st.ListEveryRepo() {
		if orgID != 0 && !(repo.OwnerType == "Organization" && repo.OwnerID == orgID) {
			continue
		}

		// Repositories created by the user (forks included, as GitHub counts them).
		if orgID == 0 && repo.OwnerType == "User" && repo.OwnerID == userID {
			years[repo.CreatedAt.UTC().Year()] = true
			if data.FirstRepo == nil || repo.CreatedAt.Before(data.FirstRepo.CreatedAt) {
				data.FirstRepo = repo
			}
			if repo.CreatedAt.Before(from) {
				data.HasActivityInThePast = true
			}
			if inWindow(repo.CreatedAt) {
				data.Repos = append(data.Repos, repo)
			}
		}

		for _, issue := range st.ListIssues(repo.ID, "all") {
			if issue.AuthorID != userID {
				continue
			}
			years[issue.CreatedAt.UTC().Year()] = true
			if data.FirstIssue == nil || issue.CreatedAt.Before(data.FirstIssue.CreatedAt) {
				data.FirstIssue = issue
			}
			if issue.CreatedAt.Before(from) {
				data.HasActivityInThePast = true
			}
			if inWindow(issue.CreatedAt) {
				data.Issues = append(data.Issues, issue)
				data.ReposWithIssues[repo.ID] = true
				data.IssueComments[issue.ID] = st.CountCommentsFor("issue", issue.ID)
				bumpDay(issue.CreatedAt, 1)
			}
		}

		for _, pr := range st.ListPullRequests(repo.ID, "all") {
			if pr.AuthorID == userID {
				years[pr.CreatedAt.UTC().Year()] = true
				if data.FirstPR == nil || pr.CreatedAt.Before(data.FirstPR.CreatedAt) {
					data.FirstPR = pr
				}
				if pr.CreatedAt.Before(from) {
					data.HasActivityInThePast = true
				}
				if inWindow(pr.CreatedAt) {
					data.PullRequests = append(data.PullRequests, pr)
					data.ReposWithPRs[repo.ID] = true
					data.PRComments[pr.ID] = st.CountCommentsFor("pull_request", pr.ID)
					bumpDay(pr.CreatedAt, 1)
				}
			}
			for _, review := range st.ListPullRequestReviews(repo.FullName, pr.Number) {
				if review.AuthorID != userID || review.State == "PENDING" {
					continue
				}
				when := review.CreatedAt
				if review.SubmittedAt != nil {
					when = *review.SubmittedAt
				}
				years[when.UTC().Year()] = true
				if when.Before(from) {
					data.HasActivityInThePast = true
				}
				if inWindow(when) {
					data.Reviews = append(data.Reviews, review)
					data.ReviewRepoID[review.ID] = repo.ID
					data.ReposWithReviews[repo.ID] = true
					bumpDay(when, 1)
				}
			}
		}

		st.accumulateCommitContributions(data, repo, user, from, to, years, bumpDay)
	}

	sortIssuesByCreation(data.Issues)
	sortPullRequestsByCreation(data.PullRequests)
	sortReviewsBySubmission(data.Reviews)
	sort.Slice(data.Repos, func(a, b int) bool {
		if !data.Repos[a].CreatedAt.Equal(data.Repos[b].CreatedAt) {
			return data.Repos[a].CreatedAt.Before(data.Repos[b].CreatedAt)
		}
		return data.Repos[a].ID < data.Repos[b].ID
	})

	data.PopularIssue = mostCommented(data.Issues, func(i *Issue) (int, time.Time) {
		return data.IssueComments[i.ID], i.CreatedAt
	})
	data.PopularPR = mostCommented(data.PullRequests, func(p *PullRequest) (int, time.Time) {
		return data.PRComments[p.ID], p.CreatedAt
	})

	if user.CreatedAt.Before(from) {
		data.HasActivityInThePast = true
	}

	data.ContributionYears = sortedYearsDesc(years)
	return data
}

// accumulateCommitContributions folds the user's in-window commits on the
// repository's default branch into the aggregate, grouped by day.
func (st *Store) accumulateCommitContributions(data *ContributionData, repo *Repo, user *User, from, to time.Time, years map[int]bool, bumpDay func(time.Time, int)) {
	storer, _ := st.GitStorageForRepoID(repo.ID)
	if storer == nil {
		return
	}
	branch := repo.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	ref, err := storer.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil || ref == nil {
		return
	}
	head, err := object.GetCommit(storer, ref.Hash())
	if err != nil {
		return
	}
	dayCounts := map[string]int{}
	dayLatest := map[string]time.Time{}
	dayDate := map[string]time.Time{}

	iter := object.NewCommitPreorderIter(head, nil, nil)
	defer iter.Close()
	_ = iter.ForEach(func(commit *object.Commit) error {
		when := commit.Author.When
		if when.Before(from) || when.After(to) {
			return nil
		}
		if !st.commitAuthoredBy(user, commit.Author) {
			return nil
		}
		key := ContributionDate(when)
		dayCounts[key]++
		if latest, ok := dayLatest[key]; !ok || when.After(latest) {
			dayLatest[key] = when
		}
		if _, ok := dayDate[key]; !ok {
			u := when.UTC()
			dayDate[key] = time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
		}
		return nil
	})

	if len(dayCounts) == 0 {
		return
	}
	data.ReposWithCommits[repo.ID] = true
	for key, count := range dayCounts {
		data.TotalCommits += count
		years[dayDate[key].Year()] = true
		bumpDay(dayDate[key], count)
		data.CommitDays = append(data.CommitDays, CommitContributionDay{
			RepoID:     repo.ID,
			Date:       dayDate[key],
			Count:      count,
			OccurredAt: dayLatest[key],
		})
	}
}

// commitAuthoredBy reports whether a git signature is the given user's, using
// the same email-then-name attribution as ResolveUserBySignature but scoped to
// one account.
func (st *Store) commitAuthoredBy(user *User, sig object.Signature) bool {
	if sig.Email != "" {
		if strings.EqualFold(sig.Email, user.Email) {
			return true
		}
		for _, e := range user.Emails {
			if strings.EqualFold(e.Email, sig.Email) {
				return true
			}
		}
		// An address owned by another account is not this user's.
		if owner := st.LookupUserByEmail(sig.Email); owner != nil {
			return owner.ID == user.ID
		}
	}
	// No account owns the address; fall back to the display name.
	if sig.Name == "" {
		return false
	}
	return strings.EqualFold(sig.Name, user.Login) ||
		(user.Name != "" && strings.EqualFold(sig.Name, user.Name))
}

func sortIssuesByCreation(issues []*Issue) {
	sort.Slice(issues, func(a, b int) bool {
		if !issues[a].CreatedAt.Equal(issues[b].CreatedAt) {
			return issues[a].CreatedAt.Before(issues[b].CreatedAt)
		}
		return issues[a].ID < issues[b].ID
	})
}

func sortPullRequestsByCreation(prs []*PullRequest) {
	sort.Slice(prs, func(a, b int) bool {
		if !prs[a].CreatedAt.Equal(prs[b].CreatedAt) {
			return prs[a].CreatedAt.Before(prs[b].CreatedAt)
		}
		return prs[a].ID < prs[b].ID
	})
}

func sortReviewsBySubmission(reviews []*PullRequestReview) {
	when := func(r *PullRequestReview) time.Time {
		if r.SubmittedAt != nil {
			return *r.SubmittedAt
		}
		return r.CreatedAt
	}
	sort.Slice(reviews, func(a, b int) bool {
		wa, wb := when(reviews[a]), when(reviews[b])
		if !wa.Equal(wb) {
			return wa.Before(wb)
		}
		return reviews[a].ID < reviews[b].ID
	})
}

// mostCommented returns the record with the greatest comment count, ties
// broken toward the earliest.
func mostCommented[T any](items []T, metric func(T) (int, time.Time)) T {
	var best T
	var bestCount int
	var bestWhen time.Time
	found := false
	for _, item := range items {
		count, when := metric(item)
		if !found || count > bestCount || (count == bestCount && when.Before(bestWhen)) {
			best, bestCount, bestWhen, found = item, count, when, true
		}
	}
	return best
}

func sortedYearsDesc(set map[int]bool) []int {
	years := make([]int, 0, len(set))
	for year := range set {
		years = append(years, year)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(years)))
	return years
}

package store

// Contribution aggregation for the GraphQL User.contributionsCollection
// surface (GQL contributions). The GraphQL layer renders these raw materials
// into the ContributionsCollection type graph; everything here is computed
// from real store data — issues, pull requests, reviews, repositories, and the
// git commit history — so the counts a client reads are the store's own
// activity rather than a fabricated number.
//
// The computation composes the store's public, individually-locked readers
// (ListEveryRepo, ListIssues, ListPullRequests, ListPullRequestReviews,
// CountCommentsFor, GitStorageForRepoID) rather than holding st.Mu itself, so
// it never deadlocks against them and always reads committed snapshots.

import (
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// ContributionDayKey is a calendar day rendered as "2006-01-02" (UTC), the key
// the contribution calendar buckets activity by.
type ContributionDayKey = string

// CommitContributionDay is one repository's commits by the user on one UTC day
// — the unit GitHub's CreatedCommitContribution represents.
type CommitContributionDay struct {
	RepoID     int
	Date       time.Time // midnight UTC of the day
	Count      int
	OccurredAt time.Time // the latest commit time on that day, for occurredAt
}

// ContributionData is everything the GraphQL ContributionsCollection needs,
// computed for one user over one window. Slices of store records are detached
// snapshots (the List* readers return snapshots); the GraphQL layer renders
// them into contribution source maps.
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
	ReviewRepoID map[int]int // review.ID -> repository ID (for grouping/rendering)
	Repos        []*Repo     // repositories the user created in the window

	// Commit contributions.
	CommitDays   []CommitContributionDay
	TotalCommits int

	// Distinct repositories the user contributed to in the window, per kind.
	ReposWithCommits map[int]bool
	ReposWithIssues  map[int]bool
	ReposWithPRs     map[int]bool
	ReposWithReviews map[int]bool

	// Comment counts for the window's issues/PRs, keyed by record ID. Used for
	// the popular* selection and the excludePopular argument.
	IssueComments map[int]int
	PRComments    map[int]int

	// The user's all-time first issue / pull request / repository. GitHub's
	// first*Contribution fields return these only when they fall inside the
	// window; they are surfaced here so the GraphQL layer can apply that test.
	FirstIssue *Issue
	FirstPR    *PullRequest
	FirstRepo  *Repo

	// The window's most-commented issue and pull request (popular*).
	PopularIssue *Issue
	PopularPR    *PullRequest

	// Per-day contribution counts for the calendar (commits + issues opened +
	// pull requests opened + reviews submitted), keyed by "2006-01-02" (UTC).
	DayCounts map[ContributionDayKey]int

	// Distinct years, most recent first, in which the user has any all-time
	// contribution (issues/PRs/repos/reviews, plus window commits).
	ContributionYears []int

	// Whether the user has any activity before the window's start.
	HasActivityInThePast bool
}

// ContributionDate renders a time as the calendar day key the aggregate uses.
func ContributionDate(t time.Time) ContributionDayKey {
	return t.UTC().Format("2006-01-02")
}

// ComputeContributions aggregates the user's real contributions over
// [from, to]. When orgID is non-zero the aggregate is restricted to
// repositories owned by that organization (GitHub's
// contributionsCollection(organizationID:)).
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

		// Repositories created by the user (ownership is the creation record
		// the store keeps; forks are included exactly as GitHub counts created
		// repositories).
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

		// Issues opened by the user.
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

		// Pull requests opened by the user, and the reviews the user submitted
		// on the repository's pull requests.
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

		// Commits authored by the user on the repository's default branch.
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

// accumulateCommitContributions walks a repository's default branch and folds
// the user's commits in the window into the aggregate, grouped by day.
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
	// dayCounts groups this repository's in-window authored commits by day.
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
			// midnight UTC of the day the square represents.
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
// the same email-then-name attribution ResolveUserBySignature applies, but
// scoped to one account so a foreign commit is rejected with a single map
// lookup rather than a full user scan.
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
		// The address belongs to some other account: it is not this user's.
		if owner := st.LookupUserByEmail(sig.Email); owner != nil {
			return owner.ID == user.ID
		}
	}
	// No account owns the address; fall back to the display name, matching the
	// login or the account name.
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

// mostCommented returns the record with the greatest comment count, breaking
// ties toward the earliest record, or nil when the slice is empty.
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

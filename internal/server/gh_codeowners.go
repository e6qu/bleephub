package bleephub

import (
	"regexp"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// CODEOWNERS ownership: the changed-path matcher, the automatic reviewer
// requests a pull request collects when it is opened or its head advances, and
// the `require_code_owner_reviews` half of a protected branch's review rule.
//
// The matcher follows GitHub's documented CODEOWNERS syntax, which is
// gitignore-shaped with deliberate departures:
//
//   - the LAST matching pattern wins, so a narrow rule placed below a broad one
//     takes its files over;
//   - a pattern with no owners CLEARS ownership for the paths it matches rather
//     than inheriting the broader rule above it, which is how GitHub's own
//     "/apps/ @octocat" + "/apps/github" example exempts a subdirectory;
//   - `!` negation, `[...]` character ranges and `\` escapes are not part of the
//     syntax at all, so a line trying to use one owns nothing.
//
// Anchoring is gitignore's: a pattern whose only slash is a trailing one (or
// that has no slash) matches at any depth, anything else is anchored at the
// repository root. A pattern ending in a literal segment or in a slash also
// owns everything beneath it, while one ending in a wildcard does not —
// `docs/*` owns `docs/getting-started.md` but not
// `docs/build-app/troubleshooting.md`, exactly as GitHub's example spells out.

// codeownersRule is one CODEOWNERS line: the path pattern, the owner tokens it
// names (empty when the line clears ownership), and the pattern compiled to the
// expression that decides whether a changed path belongs to it.
type codeownersRule struct {
	pattern string
	owners  []string
	re      *regexp.Regexp
}

// parseCodeownersRules turns a CODEOWNERS file into its rules, in file order —
// the order the last-match-wins rule is resolved against. Comment and blank
// lines, and lines whose pattern uses syntax GitHub does not support, yield no
// rule.
func parseCodeownersRules(content string) []codeownersRule {
	var rules []codeownersRule
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		// GitHub's CODEOWNERS has no negation: a `!` pattern is not a rule
		// that un-owns anything, it is a line that matches nothing.
		if strings.HasPrefix(fields[0], "!") {
			continue
		}
		re := codeownersPatternRegexp(fields[0])
		if re == nil {
			continue
		}
		rules = append(rules, codeownersRule{pattern: fields[0], owners: fields[1:], re: re})
	}
	return rules
}

// codeownersPatternRegexp compiles one CODEOWNERS pattern into the expression
// that matches the repository-relative paths it owns. It returns nil for a
// pattern that owns nothing: the empty pattern, a bare "/", and "***", which
// GitHub rejects.
func codeownersPatternRegexp(pattern string) *regexp.Regexp {
	if pattern == "" || pattern == "/" || strings.Contains(pattern, "***") {
		return nil
	}

	segments := strings.Split(pattern, "/")
	switch {
	case segments[0] == "":
		// A leading slash anchors the pattern at the repository root.
		segments = segments[1:]
	case len(segments) == 1 || (len(segments) == 2 && segments[1] == ""):
		// No slash of its own ("*.js") or only a trailing one ("apps/"):
		// the pattern matches at any depth, as a leading "**/" would.
		if segments[0] != "**" {
			segments = append([]string{"**"}, segments...)
		}
	}
	if len(segments) > 1 && segments[len(segments)-1] == "" {
		// A trailing slash names a directory and owns its whole subtree.
		segments[len(segments)-1] = "**"
	}
	if len(segments) == 0 {
		return nil
	}

	last := len(segments) - 1
	needSlash := false
	var b strings.Builder
	b.WriteString(`\A`)
	for i, segment := range segments {
		if segment == "**" {
			switch {
			case i == 0 && i == last:
				b.WriteString(`.+`)
			case i == 0:
				b.WriteString(`(?:.+/)?`)
			case i == last:
				b.WriteString(`/.*`)
			default:
				b.WriteString(`(?:/.+)?`)
				needSlash = true
			}
			continue
		}
		if needSlash {
			b.WriteString(`/`)
		}
		b.WriteString(codeownersSegmentRegexp(segment))
		// A final segment that names something literally names a directory as
		// readily as a file, so it owns everything beneath it ("**/logs" owns
		// "build/logs/out.txt"). A final segment that is a wildcard does not:
		// "docs/*" stops at the directory's immediate children.
		if i == last && !strings.ContainsAny(segment, "*?") {
			b.WriteString(`(?:/.*)?`)
		}
		needSlash = true
	}
	b.WriteString(`\z`)

	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

// codeownersSegmentRegexp translates one path segment's globs: `*` spans any
// run of characters within a segment and `?` exactly one, neither crossing a
// separator. Everything else is literal.
func codeownersSegmentRegexp(segment string) string {
	var b strings.Builder
	for _, r := range segment {
		switch r {
		case '*':
			b.WriteString(`[^/]*`)
		case '?':
			b.WriteString(`[^/]`)
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return b.String()
}

// codeownersRuleForPath returns the index of the last rule matching path, which
// is the rule that owns it, and whether any rule matched at all.
func codeownersRuleForPath(rules []codeownersRule, path string) (int, bool) {
	for i := len(rules) - 1; i >= 0; i-- {
		if rules[i].re.MatchString(path) {
			return i, true
		}
	}
	return 0, false
}

// codeownersFileAtRef returns the operative CODEOWNERS file at ref: its content
// and the path it was found at, searching GitHub's three locations in
// precedence order. A repository without one yields ok == false, which is what
// leaves it untouched by every rule below.
func (s *Server) codeownersFileAtRef(repo *store.Repo, ref string) (content, path string, ok bool) {
	tree, _, err := s.repoTreeAtRef(repo, ref)
	if err != nil {
		return "", "", false
	}
	stor := s.gitStorageForRepo(repo)
	if stor == nil {
		return "", "", false
	}
	for _, candidate := range codeownersLocations {
		entry, err := tree.FindEntry(candidate)
		if err != nil || !entry.Mode.IsFile() {
			continue
		}
		blob, err := store.ReadGitBlob(stor, entry.Hash)
		if err != nil {
			continue
		}
		return string(blob), candidate, true
	}
	return "", "", false
}

// codeownerOwners is one rule's owners after resolution against the store: the
// users and organization teams that can actually be asked to review.
type codeownerOwners struct {
	userIDs []int
	teamIDs []int
}

// resolveCodeowners turns a rule's owner tokens into store principals.
// A `@login` is a user (an organization login owns files on GitHub but cannot
// review, so it resolves to nothing), a `@org/team` is a team of the
// repository's own organization, and a bare token that looks like an address is
// the user holding that email. Owners that cannot read the repository are
// dropped: they can never supply the review the request is asking for.
func (s *Server) resolveCodeowners(repo *store.Repo, tokens []string) codeownerOwners {
	var out codeownerOwners
	seenUser := map[int]bool{}
	seenTeam := map[int]bool{}
	repoOwner := ownerFromRepoFullName(repo.FullName)

	addUser := func(u *store.User) {
		if u == nil || seenUser[u.ID] || !namedUserCanReadRepo(s.store, u, repo) {
			return
		}
		seenUser[u.ID] = true
		out.userIDs = append(out.userIDs, u.ID)
	}

	for _, token := range tokens {
		if !strings.HasPrefix(token, "@") {
			// An email owner is only an owner when it belongs to a user; an
			// address nobody holds names no reviewer.
			if at := strings.Index(token, "@"); at > 0 && at < len(token)-1 {
				addUser(s.store.LookupUserByEmail(token))
			}
			continue
		}
		name := strings.TrimPrefix(token, "@")
		orgLogin, teamSlug, isTeam := strings.Cut(name, "/")
		if !isTeam {
			addUser(s.store.LookupUserByLogin(name))
			continue
		}
		// Only a team of the repository's own organization can be a reviewer,
		// the same constraint the explicit review-request endpoint applies.
		if !strings.EqualFold(orgLogin, repoOwner) {
			continue
		}
		team := s.store.GetTeam(orgLogin, teamSlug)
		if team == nil || seenTeam[team.ID] || !s.codeownerTeamCanReadRepo(team, repo) {
			continue
		}
		seenTeam[team.ID] = true
		out.teamIDs = append(out.teamIDs, team.ID)
	}
	return out
}

// codeownerTeamCanReadRepo is the team half of the read-access filter: a team
// that holds the repository through a grant of its own, or whose members can
// reach it some other way (an organization base permission, a public
// repository), can review it.
func (s *Server) codeownerTeamCanReadRepo(team *store.Team, repo *store.Repo) bool {
	for _, name := range team.RepoNames {
		if name == repo.FullName {
			return true
		}
	}
	for _, memberID := range team.MemberIDs {
		if namedUserCanReadRepo(s.store, s.store.GetUserByID(memberID), repo) {
			return true
		}
	}
	return false
}

// codeownerRequirements resolves a pull request's changed paths against the
// CODEOWNERS file on its base branch — the branch whose protection rule governs
// the merge and the branch GitHub reads the file from — and returns one entry
// per distinct rule the changes matched. An empty result means no code owner
// governs this pull request: the repository has no CODEOWNERS file, nothing it
// owns changed, or every match was a rule that clears ownership.
func (s *Server) codeownerRequirements(repo *store.Repo, pr *store.PullRequest) []codeownerOwners {
	if repo == nil || pr == nil {
		return nil
	}
	content, _, ok := s.codeownersFileAtRef(repo, pr.BaseRefName)
	if !ok {
		return nil
	}
	rules := parseCodeownersRules(content)
	if len(rules) == 0 {
		return nil
	}
	files, err := pullRequestChangedFiles(s.store, repo, pr, "")
	if err != nil {
		return nil
	}
	var out []codeownerOwners
	matched := map[int]bool{}
	for _, file := range files {
		path, _ := file["filename"].(string)
		if path == "" {
			continue
		}
		index, ok := codeownersRuleForPath(rules, path)
		if !ok || matched[index] {
			continue
		}
		matched[index] = true
		if len(rules[index].owners) == 0 {
			// The last matching pattern names nobody, so these paths are
			// deliberately unowned.
			continue
		}
		owners := s.resolveCodeowners(repo, rules[index].owners)
		if len(owners.userIDs) == 0 && len(owners.teamIDs) == 0 {
			continue
		}
		out = append(out, owners)
	}
	return out
}

// autoRequestCodeOwners requests the code owners of a pull request's changed
// paths as reviewers, which GitHub does when a pull request is opened and again
// when a push moves its head and brings owned files into the diff. The request
// is additive and never asks the same person twice: the pull request's own
// author is never a reviewer of it, an owner already requested stays requested
// once, and an owner who has already reviewed is not asked again.
func (s *Server) autoRequestCodeOwners(repo *store.Repo, pr *store.PullRequest, sender *store.User) {
	if repo == nil || pr == nil {
		return
	}
	// Work from a fresh snapshot: the caller may hold the live row, and the
	// review-request webhook delta needs the reviewer sets as they were before
	// this call added to them (STORE-021).
	before := s.store.GetPullRequestByNumber(repo.ID, pr.Number)
	if before == nil || before.State != "OPEN" {
		return
	}
	requirements := s.codeownerRequirements(repo, before)
	if len(requirements) == 0 {
		return
	}

	skipUser := map[int]bool{before.AuthorID: true}
	for _, id := range before.RequestedReviewerIDs {
		skipUser[id] = true
	}
	for _, review := range s.store.ListPullRequestReviews(repo.FullName, before.Number) {
		skipUser[review.AuthorID] = true
	}
	skipTeam := map[int]bool{}
	for _, id := range before.RequestedTeamIDs {
		skipTeam[id] = true
	}

	var userIDs, teamIDs []int
	for _, requirement := range requirements {
		for _, id := range requirement.userIDs {
			if skipUser[id] {
				continue
			}
			skipUser[id] = true
			userIDs = append(userIDs, id)
		}
		for _, id := range requirement.teamIDs {
			if skipTeam[id] {
				continue
			}
			skipTeam[id] = true
			teamIDs = append(teamIDs, id)
		}
	}
	if len(userIDs) == 0 && len(teamIDs) == 0 {
		return
	}

	// The request is attributed to whoever caused it — the opener, or the
	// pusher whose commits brought the owned files in — as GitHub attributes
	// its own automatic requests.
	actorID := before.AuthorID
	if sender != nil {
		actorID = sender.ID
	}
	if len(userIDs) > 0 {
		s.store.RequestReviewers(repo.FullName, before.Number, userIDs, actorID)
	}
	if len(teamIDs) > 0 {
		s.store.RequestTeamReviewers(repo.FullName, before.Number, teamIDs)
	}
	updated := s.store.GetPullRequestByNumber(repo.ID, before.Number)
	if updated == nil {
		return
	}
	s.pullRequestEmitter(repo, updated, sender).emitReviewRequestDelta(
		before.RequestedReviewerIDs, updated.RequestedReviewerIDs,
		before.RequestedTeamIDs, updated.RequestedTeamIDs)
}

// codeOwnerReviewMissing reports whether the base branch's protection requires
// code owner review and the pull request does not have it yet. It is the
// predicate behind both the merge refusal and the `blocked` mergeable_state the
// read paths report, so a caller can never see a pull request described as
// mergeable that the merge gate would refuse.
func (s *Server) codeOwnerReviewMissing(repo *store.Repo, pr *store.PullRequest) bool {
	bp := s.effectiveBranchProtectionFor(repo.ID, pr.BaseRefName)
	if bp == nil || bp.RequiredPullRequestReviews == nil || !bp.RequiredPullRequestReviews.RequireCodeOwnerReviews {
		return false
	}
	return !s.codeOwnerApprovalsSatisfied(repo, pr)
}

// codeOwnerApprovalsSatisfied reports whether every code owner rule the pull
// request's changed paths matched holds an approving review. Each rule is
// satisfied on its own: an approval from one of the owners of a file is not an
// approval of a file owned by somebody else. Approvals are read through the
// same latest-per-reviewer gate the required-approving-count and
// requested-changes rules use, so a review that was dismissed — by a new push
// or by hand — stops counting here too.
func (s *Server) codeOwnerApprovalsSatisfied(repo *store.Repo, pr *store.PullRequest) bool {
	requirements := s.codeownerRequirements(repo, pr)
	if len(requirements) == 0 {
		return true
	}
	s.store.Mu.RLock()
	states := s.latestGateReviewStatesLocked(pr.ID)
	s.store.Mu.RUnlock()
	approved := map[int]bool{}
	for userID, state := range states {
		if state == "APPROVED" {
			approved[userID] = true
		}
	}
	if len(approved) == 0 {
		return false
	}
	for _, requirement := range requirements {
		if !s.codeownerRequirementApproved(requirement, approved) {
			return false
		}
	}
	return true
}

// codeownerRequirementApproved reports whether one rule's owners have approved:
// directly for a user owner, or through any member of a team owner, which is
// how a team review request is satisfied on GitHub.
func (s *Server) codeownerRequirementApproved(owners codeownerOwners, approved map[int]bool) bool {
	for _, id := range owners.userIDs {
		if approved[id] {
			return true
		}
	}
	for _, teamID := range owners.teamIDs {
		team := s.store.GetTeamByID(teamID)
		if team == nil {
			continue
		}
		for _, memberID := range team.MemberIDs {
			if approved[memberID] {
				return true
			}
		}
	}
	return false
}

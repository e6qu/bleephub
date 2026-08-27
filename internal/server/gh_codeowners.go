package bleephub

import (
	"regexp"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// CODEOWNERS matcher, following GitHub's gitignore-shaped syntax with its
// deliberate departures: the LAST matching pattern wins; a pattern with no
// owners CLEARS ownership for its paths; `!`, `[...]` ranges and `\` escapes
// are not syntax, so a line using one owns nothing. Anchoring is gitignore's —
// a pattern with no slash or only a trailing one matches at any depth,
// otherwise it anchors at the repo root — and a pattern ending in a literal
// segment or a slash owns everything beneath it while one ending in a wildcard
// does not (`docs/*` owns `docs/x.md` but not `docs/sub/y.md`).

// codeownersRule is one CODEOWNERS line: the path pattern, its owner tokens
// (empty when the line clears ownership), and the compiled matcher.
type codeownersRule struct {
	pattern string
	owners  []string
	re      *regexp.Regexp
}

// parseCodeownersRules turns a CODEOWNERS file into its rules in file order.
// Blank, comment, and unsupported-syntax lines yield no rule.
func parseCodeownersRules(content string) []codeownersRule {
	var rules []codeownersRule
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		// No negation in CODEOWNERS: a `!` pattern matches nothing.
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

// codeownersPatternRegexp compiles one pattern into the matcher for the
// repo-relative paths it owns, or nil for a pattern that owns nothing (empty,
// bare "/", or "***", which GitHub rejects).
func codeownersPatternRegexp(pattern string) *regexp.Regexp {
	if pattern == "" || pattern == "/" || strings.Contains(pattern, "***") {
		return nil
	}

	segments := strings.Split(pattern, "/")
	switch {
	case segments[0] == "":
		// Leading slash anchors at the repo root.
		segments = segments[1:]
	case len(segments) == 1 || (len(segments) == 2 && segments[1] == ""):
		// No own slash or only a trailing one: match at any depth (leading "**/").
		if segments[0] != "**" {
			segments = append([]string{"**"}, segments...)
		}
	}
	if len(segments) > 1 && segments[len(segments)-1] == "" {
		// Trailing slash names a directory and owns its subtree.
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
		// A literal final segment owns its subtree ("**/logs" owns
		// "build/logs/out.txt"); a wildcard one does not ("docs/*" stops at
		// immediate children).
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

// codeownersSegmentRegexp translates one segment's globs: `*` spans any run
// within the segment, `?` exactly one; neither crosses a separator.
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

// codeownersRuleForPath returns the index of the last matching rule (the owner)
// and whether any matched.
func codeownersRuleForPath(rules []codeownersRule, path string) (int, bool) {
	for i := len(rules) - 1; i >= 0; i-- {
		if rules[i].re.MatchString(path) {
			return i, true
		}
	}
	return 0, false
}

// codeownersFileAtRef returns the operative CODEOWNERS content and its path at
// ref, searching GitHub's three locations in precedence order.
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

// codeownerOwners is one rule's resolved owners: the users and teams that can
// actually be asked to review.
type codeownerOwners struct {
	userIDs []int
	teamIDs []int
}

// resolveCodeowners turns a rule's owner tokens into store principals: `@login`
// is a user (an org login cannot review, so it resolves to nothing), `@org/team`
// is a team of the repo's own org, and an address token is the user holding
// that email. Owners that cannot read the repo are dropped.
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
			// An address owner counts only when a user holds it.
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
		// Only a team of the repo's own org can review, as with explicit
		// review requests.
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

// codeownerTeamCanReadRepo reports whether a team can read repo, through its own
// grant or through any member's access.
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

// codeownerRequirements resolves a PR's changed paths against the CODEOWNERS
// file on its base branch (where GitHub reads it) and returns one entry per
// distinct matched rule. Empty means no code owner governs this PR.
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
			// Last matching pattern names nobody: deliberately unowned.
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

// autoRequestCodeOwners requests the code owners of a PR's changed paths as
// reviewers, as GitHub does on open and on a push that brings owned files into
// the diff. Additive and idempotent: never the author, an already-requested
// owner, or one who has already reviewed.
func (s *Server) autoRequestCodeOwners(repo *store.Repo, pr *store.PullRequest, sender *store.User) {
	if repo == nil || pr == nil {
		return
	}
	// Fresh snapshot: the webhook delta below needs the reviewer sets as they
	// were before this call (STORE-021; the caller may hold the live row).
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

	// Attribute the request to whoever caused it (opener or pusher), as GitHub does.
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

// codeOwnerReviewMissing reports whether base-branch protection requires code
// owner review and the PR lacks it. Shared by the merge refusal and the
// `blocked` mergeable_state so the two never disagree.
func (s *Server) codeOwnerReviewMissing(repo *store.Repo, pr *store.PullRequest) bool {
	bp := s.effectiveBranchProtectionFor(repo.ID, pr.BaseRefName)
	if bp == nil || bp.RequiredPullRequestReviews == nil || !bp.RequiredPullRequestReviews.RequireCodeOwnerReviews {
		return false
	}
	return !s.codeOwnerApprovalsSatisfied(repo, pr)
}

// codeOwnerApprovalsSatisfied reports whether every matched code owner rule
// holds an approving review; each rule is satisfied independently. Reads
// through the same latest-per-reviewer gate, so a dismissed review stops
// counting here too.
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
// a user owner directly, or a team owner through any member.
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

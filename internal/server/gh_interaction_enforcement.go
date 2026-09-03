package bleephub

import (
	"net/http"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// Content-interaction enforcement. GitHub bars two classes of user from creating
// new content (issues, pull requests, comments, reactions) in a repository:
//
//   - a user the repository's owner — or, for an org repo, the owning org — has
//     blocked; and
//   - via interaction limits, a user whose standing is below the active limit's
//     threshold (collaborators_only, contributors_only, existing_users).
//
// The set/get endpoints for both features already exist; this is the write-path
// enforcement they imply. It is the one predicate every content-create handler
// asks, so a new handler inherits it by calling rejectIfInteractionLimited.

// existingUserAccountAge is the account age below which the existing_users
// interaction limit treats an account as "recently created" and bars it.
// GitHub does not publish the exact window; we model it as one day, matching the
// one_day limit-duration vocabulary.
const existingUserAccountAge = 24 * time.Hour

// interactionLimitedMessage is the 403 body GitHub returns to a user barred by
// an interaction limit.
const interactionLimitedMessage = "You are not allowed to perform this action because the repository has interaction limits enabled."

// rejectIfInteractionLimited writes the refusal and returns true when actor is
// barred from creating content in repo (a block, or an interaction limit their
// standing does not clear). It mirrors rejectIfArchived: callers guard the
// create path with `if s.rejectIfInteractionLimited(...) { return }`.
func (s *Server) rejectIfInteractionLimited(w http.ResponseWriter, actor *store.User, repo *store.Repo) bool {
	status, msg, refused := s.contentInteractionRefusal(actor, repo)
	if refused {
		writeGHError(w, status, msg)
		return true
	}
	return false
}

// contentInteractionRefusal decides whether actor may create new content in
// repo. Blocks bind everyone; interaction limits exempt owners, collaborators
// (anyone with push access) and — for contributors_only — prior committers to
// the default branch. Returns (status, message, refused); when refused is false
// the other results are unused.
func (s *Server) contentInteractionRefusal(actor *store.User, repo *store.Repo) (int, string, bool) {
	if actor == nil || repo == nil {
		return 0, "", false
	}

	// Resolve the owning org once (nil for user-owned repos).
	var org *store.Org
	if strings.EqualFold(repo.OwnerType, "Organization") {
		org = s.store.GetOrgByID(repo.OwnerID)
	}

	// Blocks bind unconditionally — even a former collaborator, once blocked,
	// can no longer interact.
	if org != nil {
		if s.store.IsUserBlockedByOrg(org.Login, actor.ID) {
			return http.StatusForbidden, "You have been blocked from interacting with this organization's repositories.", true
		}
	} else if s.store.IsUserBlocked(repo.OwnerID, actor.ID) {
		return http.StatusForbidden, "You have been blocked from interacting with this repository.", true
	}

	// Effective interaction limit: the stricter of the repository's own limit and
	// the owner-wide (org or user) limit. GitHub layers them and the more
	// restrictive one wins.
	limit := s.effectiveInteractionLimit(repo, org)
	if limit == "" {
		return 0, "", false
	}

	// Owners and collaborators (anyone with push access, via ownership, org role,
	// team, or a direct grant) are never interaction-limited.
	if store.CanPushRepo(s.store, actor, repo) {
		return 0, "", false
	}

	switch limit {
	case "collaborators_only":
		return http.StatusForbidden, interactionLimitedMessage, true
	case "contributors_only":
		if s.userHasContributedToDefaultBranch(repo, actor) {
			return 0, "", false
		}
		return http.StatusForbidden, interactionLimitedMessage, true
	case "existing_users":
		if s.currentTime().Sub(actor.CreatedAt) < existingUserAccountAge {
			return http.StatusForbidden, interactionLimitedMessage, true
		}
		return 0, "", false
	}
	return 0, "", false
}

// interactionLimitRank orders the limit groups from least to most restrictive so
// the effective limit can be chosen. An unset or unknown group ranks 0.
func interactionLimitRank(limit string) int {
	switch limit {
	case "existing_users":
		return 1
	case "contributors_only":
		return 2
	case "collaborators_only":
		return 3
	}
	return 0
}

// effectiveInteractionLimit returns the stricter of the repo's own active limit
// and the owner-wide (org or user) active limit, or "" when neither applies.
func (s *Server) effectiveInteractionLimit(repo *store.Repo, org *store.Org) string {
	now := s.currentTime()

	repoLim := ""
	if repo.InteractionLimit != "" && repo.InteractionLimitExpiry != nil && !now.After(*repo.InteractionLimitExpiry) {
		repoLim = repo.InteractionLimit
	}

	ownerLim := ""
	if org != nil {
		if l := s.store.GetOrgInteractionLimit(org.Login); l != nil {
			ownerLim = l.Limit
		}
	} else if l, _ := s.store.GetUserInteractionLimit(repo.OwnerID); l != "" {
		ownerLim = l
	}

	if interactionLimitRank(ownerLim) > interactionLimitRank(repoLim) {
		return ownerLim
	}
	return repoLim
}

// userHasContributedToDefaultBranch reports whether actor authored any commit on
// repo's default branch — the exemption the contributors_only limit grants. Only
// reached when a contributors_only limit is active and actor lacks push access,
// so the default-branch walk stays off the common path.
func (s *Server) userHasContributedToDefaultBranch(repo *store.Repo, actor *store.User) bool {
	buckets, ok := s.aggregateContributors(repo)
	if !ok {
		return false
	}
	for _, b := range buckets {
		if b.user != nil && b.user.ID == actor.ID {
			return true
		}
	}
	return false
}

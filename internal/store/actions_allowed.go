package store

import (
	"fmt"
	"strings"
)

// Which actions and reusable workflows a workflow may use. The policy is set at
// three levels — enterprise, organization, repository — and a narrower level
// may restrict what a broader one allows and never widen it, so a `uses:`
// reference has to be admitted by every level that governs the repository.
//
// At any level: `all` admits everything; `local_only` admits only what the
// repository's own owner publishes; `selected` admits that, plus GitHub's own
// actions, verified creators' actions, and whatever matches a listed pattern,
// each as the level has chosen.

// githubOwnedActionOwners are the accounts `github_owned_allowed` covers.
var githubOwnedActionOwners = map[string]bool{"actions": true, "github": true}

// ActionUseRefusal reports why a workflow in repo may not use the action or
// reusable workflow named by uses, or "" when it may. uses is the value of a
// step's or a job's `uses:` key.
func (st *Store) ActionUseRefusal(repo *Repo, uses string) string {
	owner, reference, governed := parseActionUse(uses)
	if !governed {
		return ""
	}
	repoOwner, _, _ := strings.Cut(repo.FullName, "/")
	if strings.EqualFold(owner, repoOwner) {
		// The owner's own actions pass every level: `local_only` means exactly
		// these, and `selected` always includes them.
		return ""
	}

	st.Mu.RLock()
	defer st.Mu.RUnlock()
	type level struct {
		scope   string
		allowed string
		chosen  *ActionsAllowed
	}
	var levels []level
	if repo.OwnerType == "Organization" {
		levels = append(levels, level{"enterprise", st.EnterpriseSettings.ActionsAllowedActions, st.EnterpriseSettings.ActionsAllowed})
		if org := st.OrgActionsPermissions[repoOwner]; org != nil {
			levels = append(levels, level{"organization", org.AllowedActions, org.ActionsAllowed})
		}
	}
	if own := st.RepoActionsPermissions[repo.FullName]; own != nil {
		levels = append(levels, level{"repository", own.AllowedActions, own.ActionsAllowed})
	}

	for _, l := range levels {
		switch l.allowed {
		case "", "all":
			continue
		case "local_only":
			return fmt.Sprintf("%s is not allowed to be used in %s. The %s allows only actions and reusable workflows in repositories owned by %s.",
				uses, repo.FullName, l.scope, repoOwner)
		case "selected":
			if l.chosen != nil {
				if l.chosen.GithubOwnedAllowed && githubOwnedActionOwners[strings.ToLower(owner)] {
					continue
				}
				if l.chosen.VerifiedAllowed && st.actionOwnerIsVerifiedLocked(owner) {
					continue
				}
				if actionMatchesAnyPattern(l.chosen.PatternsAllowed, reference) {
					continue
				}
			}
			return fmt.Sprintf("%s is not allowed to be used in %s. The %s allows only selected actions and reusable workflows.",
				uses, repo.FullName, l.scope)
		}
	}
	return ""
}

// parseActionUse splits a `uses:` value into the owner it names and the
// `owner/repo[/path]@ref` reference patterns are matched against. A path in
// the workflow's own repository and a container image are not governed by the
// policy: the first is the repository's own code, and the second is not an
// action at all.
func parseActionUse(uses string) (owner, reference string, governed bool) {
	uses = strings.TrimSpace(uses)
	if uses == "" || strings.HasPrefix(uses, "./") || strings.HasPrefix(uses, "../") || strings.HasPrefix(uses, "docker://") {
		return "", "", false
	}
	owner, _, found := strings.Cut(uses, "/")
	if !found || owner == "" {
		return "", "", false
	}
	return owner, uses, true
}

// actionOwnerIsVerifiedLocked reports whether the account is a verified
// creator, which GitHub grants an organization that has verified a domain.
func (st *Store) actionOwnerIsVerifiedLocked(owner string) bool {
	org := st.OrgByLoginLocked(owner)
	if org == nil {
		return false
	}
	for _, domain := range st.VerifiableDomains {
		if domain.OwnerType == VerifiableDomainOwnerOrganization && domain.OwnerID == org.ID && domain.IsVerified {
			return true
		}
	}
	return false
}

// actionMatchesAnyPattern applies `patterns_allowed`. A pattern names
// `owner/repo[/path]@ref` with `*` as a wildcard; GitHub also lets the `@ref`
// be left off, which admits any ref, and `owner/*` for every repository of an
// owner.
func actionMatchesAnyPattern(patterns []string, reference string) bool {
	name, _, _ := strings.Cut(reference, "@")
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		subject := reference
		if !strings.Contains(pattern, "@") {
			subject = name
		}
		if actionPatternMatches(strings.ToLower(pattern), strings.ToLower(subject)) {
			return true
		}
	}
	return false
}

// actionPatternMatches matches `*` against any run of characters, slashes
// included: `octo-org/*` has to cover `octo-org/repo/path`.
func actionPatternMatches(pattern, subject string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == subject
	}
	if !strings.HasPrefix(subject, parts[0]) {
		return false
	}
	subject = subject[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		at := strings.Index(subject, part)
		if at < 0 {
			return false
		}
		subject = subject[at+len(part):]
	}
	return strings.HasSuffix(subject, parts[len(parts)-1])
}

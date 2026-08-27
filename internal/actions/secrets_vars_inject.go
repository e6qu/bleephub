package actions

import (
	"fmt"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
)

// OrgItemVisibleToRepo reports whether an org-level secret or variable applies
// to a repo: "all" everywhere, "private" only to private/internal repos,
// "selected" only to the listed repo IDs.
func OrgItemVisibleToRepo(visibility string, selectedIDs []int, repo *store.Repo) bool {
	switch visibility {
	case "all":
		return true
	case "private":
		return repo != nil && repo.Private
	case "selected":
		if repo == nil {
			return false
		}
		for _, id := range selectedIDs {
			if id == repo.ID {
				return true
			}
		}
	}
	return false
}

// JobSecretsEntitled reports whether a runner in scope may receive the job
// message for repoFullName, which carries repo/org/env secrets in plaintext.
// A message naming no repository (operator /internal/exec/submit) carries no
// secrets and is unrestricted.
func JobSecretsEntitled(scope store.RunnerScope, repoFullName string) bool {
	if repoFullName == "" {
		return true
	}
	return scope.CoversRepo(repoFullName)
}

// CollectJobSecretsAndVars resolves the secrets and variables a job in
// repoFullName (optionally environment envName) receives, merging org (lowest,
// visibility-filtered), then repository, then environment. Secrets and
// variables merge independently. Returned maps are fresh copies.
func (s *Engine) CollectJobSecretsAndVars(repoFullName, envName string) (secrets map[string]string, vars map[string]string, err error) {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return s.collectJobSecretsAndVarsLocked(repoFullName, envName)
}

// collectJobSecretsAndVarsLocked is the core of CollectJobSecretsAndVars for
// callers already holding the store lock.
func (s *Engine) collectJobSecretsAndVarsLocked(repoFullName, envName string) (secrets map[string]string, vars map[string]string, err error) {
	secrets = make(map[string]string)
	vars = make(map[string]string)

	repo := s.store.ReposByName[repoFullName]
	if repo == nil {
		return nil, nil, fmt.Errorf("repository %q not found for Actions secrets and variables", repoFullName)
	}

	// Org scope (lowest precedence); user-owned repos have no org scope.
	owner, _, _ := strings.Cut(repoFullName, "/")
	if org := s.store.OrgsByLogin[owner]; org != nil {
		for name, sec := range s.store.OrgSecrets[org.Login] {
			if OrgItemVisibleToRepo(sec.Visibility, sec.SelectedRepoIDs, repo) {
				secrets[name] = sec.Value
			}
		}
		for name, v := range s.store.OrgVariables[org.Login] {
			if OrgItemVisibleToRepo(v.Visibility, v.SelectedRepoIDs, repo) {
				vars[name] = v.Value
			}
		}
	}

	for name, sec := range s.store.RepoSecrets[repoFullName] {
		secrets[name] = sec.Value
	}
	for name, v := range s.store.RepoVariables[repoFullName] {
		vars[name] = v.Value
	}

	// Environment scope (highest precedence) overrides both.
	if envName != "" {
		key := store.EnvScopeKey(repoFullName, envName)
		for name, sec := range s.store.EnvSecrets[key] {
			secrets[name] = sec.Value
		}
		for name, v := range s.store.EnvVariables[key] {
			vars[name] = v.Value
		}
	}

	return secrets, vars, nil
}

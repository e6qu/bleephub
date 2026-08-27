package bleephub

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
)

// repositoryMatchesSearch evaluates the repository-search qualifier set against
// repository state. Every parsed qualifier reaches this function, exclusions
// included.
func repositoryMatchesSearch(st *store.Store, repo *store.Repo, query searchQuery) bool {
	hasForkQualifier := false
	for _, qualifier := range query.Qualifiers {
		if qualifier.Key == "fork" {
			hasForkQualifier = true
			break
		}
	}
	// GitHub excludes forks unless the query explicitly requests fork:true or
	// fork:only (a negative fork qualifier is still explicit, evaluated below).
	if repo.Fork && !hasForkQualifier {
		return false
	}

	textFields := repositorySearchTextFields(query)
	text := repositorySearchText(st, repo, textFields)
	if !query.matchesText(text) {
		return false
	}

	for _, qualifier := range query.Qualifiers {
		if qualifier.Key == "in" {
			continue
		}
		if strings.HasPrefix(qualifier.Key, "props.") && query.Org == "" {
			// GitHub documents unscoped property qualifiers as ignored.
			continue
		}
		if qualifier.Key == "fork" && !qualifier.Negated && qualifier.Value == "true" {
			// fork:true includes both forks and sources.
			continue
		}
		matches := repositoryMatchesQualifier(st, repo, qualifier)
		if qualifier.Negated {
			matches = !matches
		}
		if !matches {
			return false
		}
	}
	return true
}

func repositorySearchTextFields(query searchQuery) map[string]bool {
	fields := map[string]bool{}
	for _, qualifier := range query.Qualifiers {
		if qualifier.Key != "in" || qualifier.Negated {
			continue
		}
		for _, field := range strings.Split(strings.ToLower(qualifier.Value), ",") {
			fields[field] = true
		}
	}
	if len(fields) == 0 {
		fields["name"] = true
		fields["description"] = true
		fields["topics"] = true
	}
	return fields
}

func repositorySearchText(st *store.Store, repo *store.Repo, fields map[string]bool) string {
	parts := make([]string, 0, 4)
	if fields["name"] {
		parts = append(parts, repo.Name)
	}
	if fields["description"] {
		parts = append(parts, repo.Description)
	}
	if fields["topics"] {
		parts = append(parts, strings.Join(repo.Topics, " "))
	}
	if fields["readme"] {
		parts = append(parts, repositoryReadme(st, repo))
	}
	return strings.Join(parts, " ")
}

func repositoryMatchesQualifier(st *store.Store, repo *store.Repo, qualifier searchQualifier) bool {
	value := strings.ToLower(qualifier.Value)
	owner, _, _ := strings.Cut(repo.FullName, "/")
	switch qualifier.Key {
	case "repo":
		return strings.EqualFold(repo.FullName, qualifier.Value)
	case "user":
		return repo.OwnerType != "Organization" && strings.EqualFold(owner, qualifier.Value)
	case "org":
		return repo.OwnerType == "Organization" && strings.EqualFold(owner, qualifier.Value)
	case "language":
		return strings.EqualFold(repo.Language, qualifier.Value)
	case "topic":
		return repoHasTopic(repo, qualifier.Value)
	case "is":
		switch value {
		case "public":
			return !repo.Private && !strings.EqualFold(repo.Visibility, "internal")
		case "private":
			return repo.Private
		case "internal":
			return strings.EqualFold(repo.Visibility, "internal")
		case "sponsorable":
			// No sponsors profile model, so nothing matches.
			return false
		}
	case "visibility":
		return strings.EqualFold(repo.Visibility, qualifier.Value)
	case "archived":
		return repo.Archived == (value == "true")
	case "template":
		return repo.IsTemplate == (value == "true")
	case "mirror":
		// Repositories hosted by bleephub are never remote mirrors.
		return value == "false"
	case "fork":
		switch value {
		case "true", "only":
			return repo.Fork
		case "false":
			return !repo.Fork
		}
	case "license":
		return strings.EqualFold(repo.LicenseKey, qualifier.Value) ||
			strings.EqualFold(repo.LicenseSPDX, qualifier.Value)
	case "size":
		return matchesNumericQualifier(qualifier.Value, st.RepoSize(repo.FullName))
	case "followers":
		return matchesNumericQualifier(qualifier.Value, int64(len(st.ListRepoSubscribers(repo.ID))))
	case "forks":
		return matchesNumericQualifier(qualifier.Value, int64(st.CountForks(repo.ID)))
	case "stars":
		return matchesNumericQualifier(qualifier.Value, int64(repo.StargazersCount))
	case "topics":
		return matchesNumericQualifier(qualifier.Value, int64(len(repo.Topics)))
	case "good-first-issues":
		return matchesNumericQualifier(qualifier.Value, int64(repoIssueLabelCount(st, repo.ID, "good first issue")))
	case "help-wanted-issues":
		return matchesNumericQualifier(qualifier.Value, int64(repoIssueLabelCount(st, repo.ID, "help wanted")))
	case "created":
		return matchesDateQualifier(qualifier.Value, repo.CreatedAt)
	case "pushed":
		return !repo.PushedAt.IsZero() && matchesDateQualifier(qualifier.Value, repo.PushedAt)
	case "has":
		return value == "funding-file" && repositoryHasFundingFile(st, repo)
	case "deployable":
		return repositoryHasLinkedArtifactState(st, repo.FullName, false) == (value == "true")
	case "deployed":
		return repositoryHasLinkedArtifactState(st, repo.FullName, true) == (value == "true")
	}
	if propertyName, ok := strings.CutPrefix(qualifier.Key, "props."); ok {
		// props.* qualifiers apply only for a single-org query; the caller checks
		// that before invoking this matcher.
		return repositoryHasCustomPropertyValue(st, repo, propertyName, qualifier.Value)
	}
	return false
}

func repositoryHasCustomPropertyValue(st *store.Store, repo *store.Repo, propertyName, wanted string) bool {
	owner, _, _ := strings.Cut(repo.FullName, "/")
	for _, property := range st.EffectiveRepoCustomPropertyValues(owner, repo.FullName) {
		name, _ := property["property_name"].(string)
		if !strings.EqualFold(name, propertyName) {
			continue
		}
		switch value := property["value"].(type) {
		case string:
			return strings.EqualFold(value, wanted)
		case bool:
			return strings.EqualFold(wanted, "true") == value
		case []string:
			for _, item := range value {
				if strings.EqualFold(item, wanted) {
					return true
				}
			}
		case []interface{}:
			for _, item := range value {
				if strings.EqualFold(strings.TrimSpace(fmt.Sprint(item)), wanted) {
					return true
				}
			}
		default:
			return strings.EqualFold(strings.TrimSpace(fmt.Sprint(value)), wanted)
		}
		return false
	}
	return false
}

func repositoryHasLinkedArtifactState(st *store.Store, fullName string, deployment bool) bool {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if deployment {
		for _, record := range st.ArtifactDeploymentRecords {
			if strings.EqualFold(record.GitHubRepository, fullName) && record.Status == "deployed" {
				return true
			}
		}
		return false
	}
	for _, record := range st.ArtifactStorageRecords {
		if strings.EqualFold(record.GitHubRepository, fullName) && record.Status == "active" {
			return true
		}
	}
	return false
}

func repoHasTopic(repo *store.Repo, wanted string) bool {
	for _, topic := range repo.Topics {
		if strings.EqualFold(topic, wanted) {
			return true
		}
	}
	return false
}

func matchesNumericQualifier(expression string, value int64) bool {
	constraint, err := parseNumericSearchConstraint(expression)
	return err == nil && constraint.matches(value)
}

func matchesDateQualifier(expression string, value time.Time) bool {
	constraint, err := parseDateSearchConstraint(expression)
	return err == nil && constraint.matches(value)
}

func repositoryDefaultTree(st *store.Store, repo *store.Repo) (gitStorage.Storer, *object.Tree, bool) {
	owner, name, ok := strings.Cut(repo.FullName, "/")
	if !ok {
		return nil, nil, false
	}
	stor := st.GetGitStorage(owner, name)
	if stor == nil {
		return nil, nil, false
	}
	ref, err := stor.Reference(plumbing.NewBranchReferenceName(repo.DefaultBranch))
	if err != nil {
		return nil, nil, false
	}
	commit, err := object.GetCommit(stor, ref.Hash())
	if err != nil {
		return nil, nil, false
	}
	tree, err := commit.Tree()
	return stor, tree, err == nil
}

func repositoryReadme(st *store.Store, repo *store.Repo) string {
	stor, tree, ok := repositoryDefaultTree(st, repo)
	if !ok {
		return ""
	}
	_, _, content, found := findFirstFile(stor, tree, "", readmeFileCandidates)
	if !found {
		return ""
	}
	return string(content)
}

func repositoryHasFundingFile(st *store.Store, repo *store.Repo) bool {
	stor, tree, ok := repositoryDefaultTree(st, repo)
	if !ok {
		return false
	}
	for _, path := range []string{".github/FUNDING.yml", ".github/FUNDING.yaml", "FUNDING.yml", "FUNDING.yaml"} {
		entry, err := tree.FindEntry(path)
		if err != nil {
			continue
		}
		blob, err := object.GetBlob(stor, entry.Hash)
		if err != nil {
			continue
		}
		reader, err := blob.Reader()
		if err != nil {
			continue
		}
		_, err = io.Copy(io.Discard, io.LimitReader(reader, 1))
		_ = reader.Close()
		if err == nil {
			return true
		}
	}
	return false
}

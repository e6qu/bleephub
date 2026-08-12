package store

import "strings"

// RunnerScope names the repository, organization, or enterprise a runner
// credential acts for. Exactly one field is set.
type RunnerScope struct {
	Repo       string `json:"repo,omitempty"` // owner/repo
	Org        string `json:"org,omitempty"`
	Enterprise string `json:"enterprise,omitempty"`
}

func (sc RunnerScope) String() string {
	switch {
	case sc.Repo != "":
		return "repo:" + sc.Repo
	case sc.Org != "":
		return "org:" + sc.Org
	case sc.Enterprise != "":
		return "enterprise:" + sc.Enterprise
	}
	return "unscoped"
}

func (sc RunnerScope) Empty() bool {
	return sc.Repo == "" && sc.Org == "" && sc.Enterprise == ""
}

// coversRepo reports whether the scope entitles its holder to act for
// repoFullName. Repository names are case-insensitive on GitHub.
func (sc RunnerScope) CoversRepo(repoFullName string) bool {
	if repoFullName == "" {
		return false
	}
	if sc.Repo != "" {
		return strings.EqualFold(sc.Repo, repoFullName)
	}
	if sc.Org != "" {
		owner, _, ok := strings.Cut(repoFullName, "/")
		return ok && strings.EqualFold(sc.Org, owner)
	}
	if sc.Enterprise != "" {
		return true
	}
	return false
}

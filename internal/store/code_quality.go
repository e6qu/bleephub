package store

import (
	"sort"
	"time"
)

// CodeQualitySetup is a repository's code quality configuration. Empty
// strings model the null runner_type / runner_label / schedule members.
type CodeQualitySetup struct {
	RepoFullName string     `json:"repo_full_name"`
	State        string     `json:"state"`
	Languages    []string   `json:"languages"`
	RunnerType   string     `json:"runner_type"`
	RunnerLabel  string     `json:"runner_label"`
	Schedule     string     `json:"schedule"`
	UpdatedAt    *time.Time `json:"updated_at"`
}

type CodeQualityFinding struct {
	Number    int                        `json:"number"`
	RepoKey   string                     `json:"repo_key"`
	State     string                     `json:"state"`
	Rule      CodeQualityFindingRule     `json:"rule"`
	Location  CodeQualityFindingLocation `json:"location"`
	Message   CodeQualityFindingMessage  `json:"message"`
	CreatedAt time.Time                  `json:"created_at"`
}

// GetCodeQualitySetup returns the repository's code quality setup, or
// the unconfigured default.
func (st *Store) GetCodeQualitySetup(repoFullName string) *CodeQualitySetup {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if setup, ok := st.CodeQualitySetups[repoFullName]; ok && setup != nil {
		return cloneCodeQualitySetup(setup)
	}
	return &CodeQualitySetup{RepoFullName: repoFullName, State: "not-configured", Languages: []string{}}
}

func (st *Store) SetCodeQualitySetup(setup *CodeQualitySetup) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	stored := cloneCodeQualitySetup(setup)
	st.CodeQualitySetups[setup.RepoFullName] = stored
	if st.Persist != nil {
		st.Persist.MustPut("code_quality_setups", setup.RepoFullName, stored)
	}
}

// PutCodeQualityFinding ingests a finding. Numbers are repository-scoped.
func (st *Store) PutCodeQualityFinding(finding *CodeQualityFinding) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.CodeQualityFindings[finding.RepoKey] == nil {
		st.CodeQualityFindings[finding.RepoKey] = map[int]*CodeQualityFinding{}
	}
	copy := *finding
	if copy.CreatedAt.IsZero() {
		copy.CreatedAt = time.Now().UTC()
	}
	st.CodeQualityFindings[finding.RepoKey][finding.Number] = &copy
	if st.Persist != nil {
		st.Persist.MustPut("code_quality_findings", finding.RepoKey, st.CodeQualityFindings[finding.RepoKey])
	}
}

func (st *Store) ListCodeQualityFindings(repoKey, state string) []*CodeQualityFinding {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*CodeQualityFinding
	for _, finding := range st.CodeQualityFindings[repoKey] {
		if state == "" || finding.State == state {
			copy := *finding
			out = append(out, &copy)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return snapshotSlice(out)
}

func (st *Store) GetCodeQualityFinding(repoKey string, number int) *CodeQualityFinding {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	finding := st.CodeQualityFindings[repoKey][number]
	if finding == nil {
		return nil
	}
	copy := *finding
	return &copy
}

func cloneCodeQualitySetup(setup *CodeQualitySetup) *CodeQualitySetup {
	if setup == nil {
		return nil
	}
	cp := *setup
	cp.Languages = append([]string(nil), setup.Languages...)
	if setup.UpdatedAt != nil {
		updatedAt := *setup.UpdatedAt
		cp.UpdatedAt = &updatedAt
	}
	return &cp
}

type CodeQualityFindingLocation struct {
	Path        string `json:"path"`
	StartLine   int    `json:"start_line,omitempty"`
	StartColumn int    `json:"start_column,omitempty"`
	EndLine     int    `json:"end_line,omitempty"`
	EndColumn   int    `json:"end_column,omitempty"`
}

type CodeQualityFindingMessage struct {
	Text     string `json:"text"`
	Markdown string `json:"markdown"`
}

type CodeQualityFindingRule struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Help        string `json:"help,omitempty"`
	Severity    string `json:"severity"`
	Category    string `json:"category"`
}

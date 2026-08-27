package store

import (
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// WorkflowFile is the file-level workflow entity (the YAML on disk), distinct
// from the run-level [Workflow]. Source is "submitted" (registered when YAML
// lands at /api/v3/bleephub/workflow) or "discovered" (walked from git). The
// latest registration of a (repo, path) pair wins, so a fresh push refreshes the
// cached YAML. Every field must round-trip: a restored row with empty
// RepoFullName or YAML is undispatchable (422).
type WorkflowFile struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Path         string    `json:"path"`
	State        string    `json:"state"`
	RepoFullName string    `json:"repo_full_name"`
	NodeID       string    `json:"node_id"`
	BadgeURL     string    `json:"badge_url"`
	YAML         string    `json:"yaml"`
	Source       string    `json:"source"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// StableWorkflowFileID returns the GitHub-shape int64 ID for (repo, path):
// FNV-1a 64-bit, JSON-safe-integer masked.
func StableWorkflowFileID(repoFullName, path string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(repoFullName + "\x00" + path))
	return JsonSafePositiveID(h.Sum64())
}

// RegisterWorkflowFile creates or updates the WorkflowFile keyed by (repo,
// path). The latest call wins on YAML/Name; CreatedAt is preserved across
// updates.
func (st *Store) RegisterWorkflowFile(repoFullName, path, name, yamlBody, source string) *WorkflowFile {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.WorkflowFiles == nil {
		st.WorkflowFiles = map[int64]*WorkflowFile{}
	}
	id := StableWorkflowFileID(repoFullName, path)
	now := st.CurrentTime()
	if existing, ok := st.WorkflowFiles[id]; ok {
		yamlChanged := existing.YAML != yamlBody
		existing.Name = name
		existing.YAML = yamlBody
		existing.Source = source
		existing.UpdatedAt = now
		if yamlChanged && existing.State == "disabled_inactivity" {
			existing.State = "active"
		}
		if st.Persist != nil {
			st.Persist.MustPut("workflow_files", strconv.FormatInt(id, 10), existing)
		}
		return existing
	}
	state := "active"
	if repo := st.ReposByName[repoFullName]; repo != nil && repo.Fork {
		state = "disabled_fork"
	}
	wf := &WorkflowFile{
		ID:           id,
		Name:         name,
		Path:         path,
		State:        state,
		RepoFullName: repoFullName,
		NodeID:       "WF_" + path,
		YAML:         yamlBody,
		Source:       source,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	st.WorkflowFiles[id] = wf
	if st.Persist != nil {
		st.Persist.MustPut("workflow_files", strconv.FormatInt(id, 10), wf)
	}
	return wf
}

// SetWorkflowFileState updates the persisted state of one discovered workflow.
func (st *Store) SetWorkflowFileState(repoFullName, path, state string) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	id := StableWorkflowFileID(repoFullName, path)
	wf := st.WorkflowFiles[id]
	if wf == nil {
		return false
	}
	if wf.State == state {
		return true
	}
	wf.State = state
	wf.UpdatedAt = st.CurrentTime()
	if st.Persist != nil {
		st.Persist.MustPut("workflow_files", strconv.FormatInt(id, 10), wf)
	}
	return true
}

// GetWorkflowFile returns the WorkflowFile keyed by (repo, id), or nil. The repo
// check guards against a cross-repo FNV ID collision.
func (st *Store) GetWorkflowFile(repoFullName string, id int64) *WorkflowFile {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	wf, ok := st.WorkflowFiles[id]
	if !ok {
		return nil
	}
	if wf.RepoFullName != repoFullName {
		return nil
	}
	// Detach: WorkflowFile is all-value, so a shallow copy is a full snapshot
	// (STORE-021).
	clone := *wf
	return &clone
}

// ListWorkflowFiles returns the repo's WorkflowFiles ordered by ID for stable
// pagination.
func (st *Store) ListWorkflowFiles(repoFullName string) []*WorkflowFile {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*WorkflowFile
	for _, wf := range st.WorkflowFiles {
		if wf.RepoFullName == repoFullName {
			out = append(out, wf)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotSlice(out)
}

// DiscoverWorkflowFilesFromGit walks the repo's default branch and registers
// every `.github/workflows/*.{yml,yaml}` file with source="discovered",
// re-discovering on every call. No-ops without git storage, a default-branch
// ref, or a tree; returns the count registered.
func (st *Store) DiscoverWorkflowFilesFromGit(repoFullName string) int {
	st.Mu.RLock()
	storer := st.GitStorages[repoFullName]
	defaultBranch := "main"
	if repo := st.ReposByName[repoFullName]; repo != nil && repo.DefaultBranch != "" {
		defaultBranch = repo.DefaultBranch
	}
	st.Mu.RUnlock()
	if storer == nil {
		return 0
	}
	repo, err := git.Open(storer, nil)
	if err != nil {
		return 0
	}
	headRef, err := repo.Storer.Reference(plumbing.NewBranchReferenceName(defaultBranch))
	if err != nil {
		return 0
	}
	commit, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		// The ref may resolve to a tag rather than a commit.
		tag, terr := repo.TagObject(headRef.Hash())
		if terr != nil {
			return 0
		}
		commit, err = tag.Commit()
		if err != nil {
			return 0
		}
	}
	tree, err := commit.Tree()
	if err != nil {
		return 0
	}
	count := 0
	_ = tree.Files().ForEach(func(f *object.File) error {
		if !IsWorkflowYAMLPath(f.Name) {
			return nil
		}
		body, err := f.Contents()
		if err != nil {
			// A transient content-read failure (e.g. a lazily-fetched S3 blob) must
			// not re-register the workflow with an empty, undispatchable body. Skip
			// it; the next discovery re-reads it.
			return nil
		}
		name := workflowDisplayName(body, f.Name)
		st.RegisterWorkflowFile(repoFullName, f.Name, name, body, "discovered")
		count++
		return nil
	})
	return count
}

func IsWorkflowYAMLPath(p string) bool {
	if !strings.HasPrefix(p, ".github/workflows/") {
		return false
	}
	rest := p[len(".github/workflows/"):]
	if strings.Contains(rest, "/") {
		// GitHub ignores files in subdirectories under .github/workflows/.
		return false
	}
	return strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".yaml")
}

// workflowDisplayName returns the YAML's `name:` field, falling back to the
// file's basename when absent.
func workflowDisplayName(yamlBody, path string) string {
	def, err := ParseWorkflow([]byte(yamlBody))
	if err == nil && def.Name != "" {
		return def.Name
	}
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	for _, ext := range []string{".yml", ".yaml"} {
		if strings.HasSuffix(base, ext) {
			base = strings.TrimSuffix(base, ext)
			break
		}
	}
	if base == "" {
		return "workflow"
	}
	return base
}

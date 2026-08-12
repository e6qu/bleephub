package store

import (
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AgentTask is a Copilot coding agent task.
type AgentTask struct {
	ID          string             `json:"id"`
	RepoID      int                `json:"repo_id"`
	OwnerID     int                `json:"owner_id"`
	CreatorID   int                `json:"creator_id"`
	CreatorType string             `json:"creator_type"` // user | organization
	Name        string             `json:"name"`
	Prompt      string             `json:"prompt"`
	Model       string             `json:"model"`
	CreatePR    bool               `json:"create_pull_request"`
	BaseRef     string             `json:"base_ref"`
	HeadRef     string             `json:"head_ref"`
	State       string             `json:"state"`
	Sessions    []AgentTaskSession `json:"sessions"`
	ArchivedAt  *time.Time         `json:"archived_at"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`
}

// CreateAgentTask stores a new Copilot coding agent task for a repository
// with its initial session.
func (st *Store) CreateAgentTask(repo *Repo, creator *User, prompt, model string, createPR bool, baseRef, headRef string) *AgentTask {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	now := time.Now().UTC()

	// The task name is derived from the first line of the prompt.
	name := prompt
	if idx := strings.IndexByte(name, '\n'); idx >= 0 {
		name = name[:idx]
	}
	if len(name) > 80 {
		name = name[:80]
	}

	task := &AgentTask{
		ID:          uuid.New().String(),
		RepoID:      repo.ID,
		OwnerID:     repo.OwnerID,
		CreatorID:   creator.ID,
		CreatorType: "user",
		Name:        name,
		Prompt:      prompt,
		Model:       model,
		CreatePR:    createPR,
		BaseRef:     baseRef,
		HeadRef:     headRef,
		State:       "queued",
		Sessions: []AgentTaskSession{{
			ID:        uuid.New().String(),
			Name:      name,
			State:     "queued",
			Prompt:    prompt,
			HeadRef:   headRef,
			BaseRef:   baseRef,
			Model:     model,
			CreatedAt: now,
			UpdatedAt: now,
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	st.AgentTasks[task.ID] = task
	if st.Persist != nil {
		st.Persist.MustPut("agent_tasks", task.ID, task)
	}
	return task
}

// GetAgentTask returns a task by ID, or nil.
func (st *Store) GetAgentTask(id string) *AgentTask {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return st.AgentTasks[id]
}

// ListAgentTasks returns the tasks matching the filter, sorted, plus the
// active/archived totals within the filter's repo/creator scope.
func (st *Store) ListAgentTasks(f AgentTaskFilter) (tasks []*AgentTask, totalActive, totalArchived int) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	for _, t := range st.AgentTasks {
		if f.RepoID != 0 && t.RepoID != f.RepoID {
			continue
		}
		if f.CreatorID != 0 && t.CreatorID != f.CreatorID {
			continue
		}
		if len(f.CreatorIDs) > 0 {
			found := false
			for _, id := range f.CreatorIDs {
				if t.CreatorID == id {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if t.ArchivedAt != nil {
			totalArchived++
		} else {
			totalActive++
		}
		if f.IsArchived != (t.ArchivedAt != nil) {
			continue
		}
		if len(f.States) > 0 {
			found := false
			for _, s := range f.States {
				if t.State == s {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if f.Since != nil && t.UpdatedAt.Before(*f.Since) {
			continue
		}
		tasks = append(tasks, t)
	}

	sort.SliceStable(tasks, func(i, j int) bool {
		var less bool
		if f.SortField == "created_at" {
			less = tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
		} else {
			less = tasks[i].UpdatedAt.Before(tasks[j].UpdatedAt)
		}
		if f.Direction == "asc" {
			return less
		}
		return !less
	})
	return tasks, totalActive, totalArchived
}

// AgentTaskFilter carries the documented list-tasks query filters.
type AgentTaskFilter struct {
	RepoID     int        `json:"-"` // 0 = any repository
	CreatorID  int        `json:"-"` // 0 = any creator
	CreatorIDs []int      `json:"-"` // non-empty = restrict to these creators
	States     []string   `json:"-"`
	IsArchived bool       `json:"-"`
	Since      *time.Time `json:"-"`
	SortField  string     // "updated_at" (default) | "created_at"
	Direction  string     // "desc" (default) | "asc"
}

// AgentTaskSession is one Copilot coding agent session within a task.
type AgentTaskSession struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	State     string    `json:"state"`
	Prompt    string    `json:"prompt"`
	HeadRef   string    `json:"head_ref"`
	BaseRef   string    `json:"base_ref"`
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

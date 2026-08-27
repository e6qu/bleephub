package store

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Deployment json tags shape the persisted row, not client responses (those go
// through deploymentToJSON). Statuses stays json:"-": statuses persist in their
// own bucket and the loader relinks them via DeploymentID.
type Deployment struct {
	ID            int                    `json:"id"`
	NodeID        string                 `json:"node_id"`
	URL           string                 `json:"url"`
	Sha           string                 `json:"sha"`
	Ref           string                 `json:"ref"`
	Task          string                 `json:"task"`
	Payload       map[string]interface{} `json:"payload"`
	OriginalEnv   string                 `json:"original_environment"`
	Environment   string                 `json:"environment"`
	Description   string                 `json:"description"`
	CreatorID     int                    `json:"creator_id"`
	RepoID        int                    `json:"repo_id"`
	AutoMerge     bool                   `json:"auto_merge"`
	ProductionEnv bool                   `json:"production_environment"`
	TransientEnv  bool                   `json:"transient_environment"`
	CreatedAt     time.Time              `json:"created_at"`
	UpdatedAt     time.Time              `json:"updated_at"`
	Statuses      []*DeploymentStatus    `json:"-"`
}

type DeploymentStatus struct {
	ID             int                   `json:"id"`
	NodeID         string                `json:"node_id"`
	State          DeploymentStatusState `json:"state"`
	CreatorID      int                   `json:"creator_id"`
	DeploymentID   int                   `json:"deployment_id"`
	Description    string                `json:"description"`
	Environment    string                `json:"environment"`
	TargetURL      string                `json:"target_url"`
	LogURL         string                `json:"log_url"`
	EnvironmentURL string                `json:"environment_url"`
	AutoInactive   bool                  `json:"auto_inactive"`
	CreatedAt      time.Time             `json:"created_at"`
	UpdatedAt      time.Time             `json:"updated_at"`
}

type Environment struct {
	ID        int                      `json:"id"`
	NodeID    string                   `json:"node_id"`
	Name      string                   `json:"name"`
	URL       string                   `json:"url"`
	HTMLURL   string                   `json:"html_url"`
	RepoID    int                      `json:"repo_id"`
	WaitTimer int                      `json:"wait_timer"`
	Reviewers []map[string]interface{} `json:"reviewers"`
	// PreventSelfReview refuses a review from the user who triggered the run.
	PreventSelfReview      bool                     `json:"prevent_self_review"`
	DeploymentBranchPolicy *DeploymentBranchPolicy  `json:"deployment_branch_policy"`
	CreatedAt              time.Time                `json:"created_at"`
	UpdatedAt              time.Time                `json:"updated_at"`
	ProtectionRules        []map[string]interface{} `json:"protection_rules"`
}

// PinnedEnvironment pins an environment at a 1-based position in its
// repository's pinned list.
type PinnedEnvironment struct {
	ID        int       `json:"id"`
	NodeID    string    `json:"node_id"`
	RepoID    int       `json:"repo_id"`
	EnvID     int       `json:"env_id"`
	Position  int       `json:"position"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type DeploymentStore struct {
	Mu           sync.RWMutex `json:"-"`
	deployments  map[int]*Deployment
	ByRepo       map[int][]*Deployment     `json:"-"`
	Statuses     map[int]*DeploymentStatus `json:"-"`
	environments map[string]*Environment   // Key: "repoID:name"
	envsByRepo   map[int][]*Environment
	pinnedEnvs   map[int]*PinnedEnvironment // Key: pin id
	pinsByRepo   map[int][]*PinnedEnvironment
	nextDepID    int
	nextStatusID int
	nextEnvID    int
	nextPinID    int
	Persist      *Persistence `json:"-"`
}

func newDeploymentStore(p *Persistence) *DeploymentStore {
	return &DeploymentStore{
		deployments:  map[int]*Deployment{},
		ByRepo:       map[int][]*Deployment{},
		Statuses:     map[int]*DeploymentStatus{},
		environments: map[string]*Environment{},
		envsByRepo:   map[int][]*Environment{},
		pinnedEnvs:   map[int]*PinnedEnvironment{},
		pinsByRepo:   map[int][]*PinnedEnvironment{},
		nextDepID:    1,
		nextStatusID: 1,
		nextEnvID:    1,
		nextPinID:    1,
		Persist:      p,
	}
}

func (ds *DeploymentStore) CreateDeployment(repoID, creatorID int, ref, sha, task, env, description string, payload map[string]interface{}, productionEnv, transientEnv bool) *Deployment {
	ds.Mu.Lock()
	defer ds.Mu.Unlock()
	id := ds.nextDepID
	ds.nextDepID++
	now := time.Now().UTC()
	d := &Deployment{
		ID:            id,
		NodeID:        fmt.Sprintf("DE_kgDO%08d", id),
		Sha:           sha,
		Ref:           ref,
		Task:          CoalesceStr(task, "deploy"),
		Payload:       payload,
		OriginalEnv:   env,
		Environment:   env,
		Description:   description,
		CreatorID:     creatorID,
		RepoID:        repoID,
		ProductionEnv: productionEnv,
		TransientEnv:  transientEnv,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	ds.deployments[id] = d
	ds.ByRepo[repoID] = append(ds.ByRepo[repoID], d)
	if ds.Persist != nil {
		ds.Persist.MustPut("deployments", strconv.Itoa(id), d)
	}
	return d
}

func (ds *DeploymentStore) GetDeployment(id int) *Deployment {
	ds.Mu.RLock()
	defer ds.Mu.RUnlock()
	return ds.deployments[id]
}

func (ds *DeploymentStore) ListDeployments(repoID int) []*Deployment {
	ds.Mu.RLock()
	defer ds.Mu.RUnlock()
	out := make([]*Deployment, len(ds.ByRepo[repoID]))
	copy(out, ds.ByRepo[repoID])
	// Most-recent first. Reload repopulates ByRepo in map order, so without this
	// sort pagination boundaries shift across restarts.
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

func (ds *DeploymentStore) DeleteDeployment(id int) bool {
	ds.Mu.Lock()
	defer ds.Mu.Unlock()
	d := ds.deployments[id]
	if d == nil {
		return false
	}
	ds.deleteDeploymentLocked(d)
	return true
}

func (ds *DeploymentStore) deleteDeploymentLocked(d *Deployment) {
	// Deployment row and its statuses commit in one transaction so a crash can't
	// orphan the statuses (STORE-001/002).
	batch := NewPersistBatch(ds.Persist)
	ds.deleteDeploymentBatchLocked(d, batch)
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "deployments", Key: strconv.Itoa(d.ID), Err: err})
	}
}

func (ds *DeploymentStore) deleteDeploymentBatchLocked(d *Deployment, batch *PersistBatch) {
	delete(ds.deployments, d.ID)
	src := ds.ByRepo[d.RepoID]
	for i, x := range src {
		if x.ID == d.ID {
			ds.ByRepo[d.RepoID] = append(src[:i], src[i+1:]...)
			break
		}
	}
	for id, status := range ds.Statuses {
		if status.DeploymentID == d.ID {
			delete(ds.Statuses, id)
			if batch != nil {
				batch.Delete("deployment_statuses", strconv.Itoa(id))
			} else if ds.Persist != nil {
				ds.Persist.MustDelete("deployment_statuses", strconv.Itoa(id))
			}
		}
	}
	if batch != nil {
		batch.Delete("deployments", strconv.Itoa(d.ID))
	} else if ds.Persist != nil {
		ds.Persist.MustDelete("deployments", strconv.Itoa(d.ID))
	}
}

func (ds *DeploymentStore) AddStatus(deploymentID, creatorID int, state, description, targetURL, logURL, envURL, env string, autoInactive bool) (*DeploymentStatus, []autoInactiveDeployment) {
	ds.Mu.Lock()
	defer ds.Mu.Unlock()
	d := ds.deployments[deploymentID]
	if d == nil {
		return nil, nil
	}
	id := ds.nextStatusID
	ds.nextStatusID++
	now := time.Now().UTC()
	status := &DeploymentStatus{
		ID:             id,
		NodeID:         fmt.Sprintf("DS_kgDO%08d", id),
		State:          DeploymentStatusState(state),
		CreatorID:      creatorID,
		DeploymentID:   deploymentID,
		Description:    description,
		Environment:    env,
		TargetURL:      targetURL,
		LogURL:         logURL,
		EnvironmentURL: envURL,
		AutoInactive:   autoInactive,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	ds.Statuses[id] = status
	d.Statuses = append(d.Statuses, status)
	d.UpdatedAt = now
	var batch *PersistBatch
	if ds.Persist != nil {
		batch = NewPersistBatch(ds.Persist)
		batch.Put("deployment_statuses", strconv.Itoa(id), status)
		batch.Put("deployments", strconv.Itoa(deploymentID), d)
	}
	var auto []autoInactiveDeployment
	if autoInactive && isCompletingDeploymentState(state) {
		for _, prior := range ds.ByRepo[d.RepoID] {
			if prior.ID == d.ID {
				continue
			}
			if prior.TransientEnv {
				continue
			}
			if !strings.EqualFold(prior.Environment, d.Environment) {
				continue
			}
			alreadyInactive := false
			for _, st := range prior.Statuses {
				if st.State == DeploymentStateInactive {
					alreadyInactive = true
					break
				}
			}
			if alreadyInactive {
				continue
			}
			sid := ds.nextStatusID
			ds.nextStatusID++
			snow := time.Now().UTC()
			inactiveStatus := &DeploymentStatus{
				ID:           sid,
				NodeID:       fmt.Sprintf("DS_kgDO%08d", sid),
				State:        DeploymentStateInactive,
				CreatorID:    creatorID,
				DeploymentID: prior.ID,
				Description:  fmt.Sprintf("Auto-inactivated by deployment #%d", d.ID),
				Environment:  prior.Environment,
				AutoInactive: false,
				CreatedAt:    snow,
				UpdatedAt:    snow,
			}
			ds.Statuses[sid] = inactiveStatus
			prior.Statuses = append(prior.Statuses, inactiveStatus)
			prior.UpdatedAt = snow
			if batch != nil {
				batch.Put("deployment_statuses", strconv.Itoa(sid), inactiveStatus)
				batch.Put("deployments", strconv.Itoa(prior.ID), prior)
			}
			auto = append(auto, autoInactiveDeployment{
				DeploymentID: prior.ID,
				Status:       inactiveStatus,
				environment:  prior.Environment,
			})
		}
	}
	if batch != nil {
		if err := batch.Commit(); err != nil {
			panic(fmt.Sprintf("persistence commit failed: %v", err))
		}
	}
	return status, auto
}

func (ds *DeploymentStore) ListStatuses(deploymentID int) []*DeploymentStatus {
	ds.Mu.RLock()
	defer ds.Mu.RUnlock()
	d := ds.deployments[deploymentID]
	if d == nil {
		return nil
	}
	out := make([]*DeploymentStatus, len(d.Statuses))
	copy(out, d.Statuses)
	return out
}

func (ds *DeploymentStore) GetStatus(id int) *DeploymentStatus {
	ds.Mu.RLock()
	defer ds.Mu.RUnlock()
	return ds.Statuses[id]
}

func (ds *DeploymentStore) UpsertEnvironment(repoID int, name string) *Environment {
	ds.Mu.Lock()
	defer ds.Mu.Unlock()
	key := fmt.Sprintf("%d:%s", repoID, name)
	if existing := ds.environments[key]; existing != nil {
		existing.UpdatedAt = time.Now().UTC()
		return existing
	}
	id := ds.nextEnvID
	ds.nextEnvID++
	now := time.Now().UTC()
	env := &Environment{
		ID:        id,
		NodeID:    fmt.Sprintf("EN_kgDO%08d", id),
		Name:      name,
		RepoID:    repoID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	ds.environments[key] = env
	ds.envsByRepo[repoID] = append(ds.envsByRepo[repoID], env)
	if ds.Persist != nil {
		ds.Persist.MustPut("environments", key, env)
	}
	return env
}

// SetEnvironmentProtection updates an environment's reviewer and wait-timer config.
func (ds *DeploymentStore) SetEnvironmentProtection(repoID int, name string, waitTimer *int, reviewers []map[string]interface{}) {
	ds.Mu.Lock()
	defer ds.Mu.Unlock()
	key := fmt.Sprintf("%d:%s", repoID, name)
	env := ds.environments[key]
	if env == nil {
		return
	}
	if waitTimer != nil {
		env.WaitTimer = *waitTimer
	}
	if reviewers != nil {
		env.Reviewers = reviewers
	}
	env.UpdatedAt = time.Now().UTC()
	if ds.Persist != nil {
		ds.Persist.MustPut("environments", key, env)
	}
}

// SetEnvironmentBranchPolicyConfig sets an environment's deployment branch
// policy. nil clears it (all branches may deploy), matching GitHub's full-replace.
func (ds *DeploymentStore) SetEnvironmentBranchPolicyConfig(repoID int, name string, policy *DeploymentBranchPolicy) {
	ds.Mu.Lock()
	defer ds.Mu.Unlock()
	key := fmt.Sprintf("%d:%s", repoID, name)
	env := ds.environments[key]
	if env == nil {
		return
	}
	env.DeploymentBranchPolicy = policy
	env.UpdatedAt = time.Now().UTC()
	if ds.Persist != nil {
		ds.Persist.MustPut("environments", key, env)
	}
}

// SetEnvironmentPreventSelfReview flips the environment's self-review refusal.
func (ds *DeploymentStore) SetEnvironmentPreventSelfReview(repoID int, name string, prevent bool) {
	ds.Mu.Lock()
	defer ds.Mu.Unlock()
	key := fmt.Sprintf("%d:%s", repoID, name)
	env := ds.environments[key]
	if env == nil {
		return
	}
	env.PreventSelfReview = prevent
	env.UpdatedAt = time.Now().UTC()
	if ds.Persist != nil {
		ds.Persist.MustPut("environments", key, env)
	}
}

func (ds *DeploymentStore) GetEnvironment(repoID int, name string) *Environment {
	ds.Mu.RLock()
	defer ds.Mu.RUnlock()
	return ds.environments[fmt.Sprintf("%d:%s", repoID, name)]
}

func (ds *DeploymentStore) GetEnvironmentByID(id int) *Environment {
	ds.Mu.RLock()
	defer ds.Mu.RUnlock()
	for _, env := range ds.environments {
		if env.ID == id {
			return env
		}
	}
	return nil
}

func (ds *DeploymentStore) ListEnvironments(repoID int) []*Environment {
	ds.Mu.RLock()
	defer ds.Mu.RUnlock()
	out := make([]*Environment, len(ds.envsByRepo[repoID]))
	copy(out, ds.envsByRepo[repoID])
	return out
}

func (ds *DeploymentStore) DeleteEnvironment(repoID int, name string) bool {
	ds.Mu.Lock()
	defer ds.Mu.Unlock()
	key := fmt.Sprintf("%d:%s", repoID, name)
	env := ds.environments[key]
	if env == nil {
		return false
	}
	delete(ds.environments, key)
	src := ds.envsByRepo[repoID]
	for i, x := range src {
		if x.ID == env.ID {
			ds.envsByRepo[repoID] = append(src[:i], src[i+1:]...)
			break
		}
	}
	if ds.Persist != nil {
		ds.Persist.MustDelete("environments", key)
	}
	ds.unpinEnvironmentLocked(repoID, env.ID, time.Now().UTC())
	return true
}

// --- pinned environments -----------------------------------------------------

// GetDeploymentByNodeID returns a detached snapshot (STORE-021), or nil.
func (ds *DeploymentStore) GetDeploymentByNodeID(nodeID string) *Deployment {
	ds.Mu.RLock()
	defer ds.Mu.RUnlock()
	for _, d := range ds.deployments {
		if d.NodeID == nodeID {
			cp := *d
			return &cp
		}
	}
	return nil
}

// GetEnvironmentByNodeID returns a detached snapshot (STORE-021), or nil.
func (ds *DeploymentStore) GetEnvironmentByNodeID(nodeID string) *Environment {
	ds.Mu.RLock()
	defer ds.Mu.RUnlock()
	for _, env := range ds.environments {
		if env.NodeID == nodeID {
			cp := *env
			return &cp
		}
	}
	return nil
}

// PinEnvironment pins an environment at the end of its repository's pinned list
// and returns a detached snapshot (STORE-021). Idempotent: an already-pinned
// environment returns its existing pin unchanged.
func (ds *DeploymentStore) PinEnvironment(repoID, envID int, now time.Time) *PinnedEnvironment {
	ds.Mu.Lock()
	defer ds.Mu.Unlock()
	for _, pin := range ds.pinsByRepo[repoID] {
		if pin.EnvID == envID {
			cp := *pin
			return &cp
		}
	}
	id := ds.nextPinID
	ds.nextPinID++
	pin := &PinnedEnvironment{
		ID:        id,
		NodeID:    fmt.Sprintf("PEN_kgDO%08d", id),
		RepoID:    repoID,
		EnvID:     envID,
		Position:  len(ds.pinsByRepo[repoID]) + 1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	ds.pinnedEnvs[id] = pin
	ds.pinsByRepo[repoID] = append(ds.pinsByRepo[repoID], pin)
	if ds.Persist != nil {
		ds.Persist.MustPut("pinned_environments", strconv.Itoa(id), pin)
	}
	cp := *pin
	return &cp
}

// UnpinEnvironment removes an environment's pin and closes the position gap.
// Reports whether a pin existed.
func (ds *DeploymentStore) UnpinEnvironment(repoID, envID int, now time.Time) bool {
	ds.Mu.Lock()
	defer ds.Mu.Unlock()
	return ds.unpinEnvironmentLocked(repoID, envID, now)
}

func (ds *DeploymentStore) unpinEnvironmentLocked(repoID, envID int, now time.Time) bool {
	pins := ds.pinsByRepo[repoID]
	index := -1
	for i, pin := range pins {
		if pin.EnvID == envID {
			index = i
			break
		}
	}
	if index < 0 {
		return false
	}
	removed := pins[index]
	delete(ds.pinnedEnvs, removed.ID)
	ds.pinsByRepo[repoID] = append(pins[:index], pins[index+1:]...)
	if ds.Persist != nil {
		ds.Persist.MustDelete("pinned_environments", strconv.Itoa(removed.ID))
	}
	ds.renumberPinsLocked(repoID, now)
	return true
}

// GetPinnedEnvironment returns a detached snapshot (STORE-021) of the pin, or
// nil when the environment is not pinned.
func (ds *DeploymentStore) GetPinnedEnvironment(repoID, envID int) *PinnedEnvironment {
	ds.Mu.RLock()
	defer ds.Mu.RUnlock()
	for _, pin := range ds.pinsByRepo[repoID] {
		if pin.EnvID == envID {
			cp := *pin
			return &cp
		}
	}
	return nil
}

// ListPinnedEnvironments returns the repository's pins as detached snapshots
// (STORE-021) in position order.
func (ds *DeploymentStore) ListPinnedEnvironments(repoID int) []*PinnedEnvironment {
	ds.Mu.RLock()
	defer ds.Mu.RUnlock()
	out := make([]*PinnedEnvironment, 0, len(ds.pinsByRepo[repoID]))
	for _, pin := range ds.pinsByRepo[repoID] {
		cp := *pin
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out
}

// ReorderPinnedEnvironment moves a pinned environment to the 1-based position,
// shifting neighbours; out-of-range positions clamp. Reports whether the
// environment was pinned.
func (ds *DeploymentStore) ReorderPinnedEnvironment(repoID, envID, position int, now time.Time) bool {
	ds.Mu.Lock()
	defer ds.Mu.Unlock()
	pins := ds.pinsByRepo[repoID]
	sort.Slice(pins, func(i, j int) bool { return pins[i].Position < pins[j].Position })
	index := -1
	for i, pin := range pins {
		if pin.EnvID == envID {
			index = i
			break
		}
	}
	if index < 0 {
		return false
	}
	if position < 1 {
		position = 1
	}
	if position > len(pins) {
		position = len(pins)
	}
	moved := pins[index]
	pins = append(pins[:index], pins[index+1:]...)
	pins = append(pins[:position-1], append([]*PinnedEnvironment{moved}, pins[position-1:]...)...)
	ds.pinsByRepo[repoID] = pins
	ds.renumberPinsLocked(repoID, now)
	return true
}

// renumberPinsLocked rewrites pin positions to the dense 1..n sequence their
// slice order holds, persisting every moved row.
func (ds *DeploymentStore) renumberPinsLocked(repoID int, now time.Time) {
	for i, pin := range ds.pinsByRepo[repoID] {
		if pin.Position == i+1 {
			continue
		}
		pin.Position = i + 1
		pin.UpdatedAt = now
		if ds.Persist != nil {
			ds.Persist.MustPut("pinned_environments", strconv.Itoa(pin.ID), pin)
		}
	}
}

func (ds *DeploymentStore) DeleteRepo(repoID int) []int {
	return ds.DeleteRepoBatch(repoID, nil)
}

func (ds *DeploymentStore) DeleteRepoBatch(repoID int, batch *PersistBatch) []int {
	ds.Mu.Lock()
	defer ds.Mu.Unlock()

	for _, d := range append([]*Deployment(nil), ds.ByRepo[repoID]...) {
		ds.deleteDeploymentBatchLocked(d, batch)
	}
	delete(ds.ByRepo, repoID)

	envIDs := make([]int, 0, len(ds.envsByRepo[repoID]))
	for _, env := range append([]*Environment(nil), ds.envsByRepo[repoID]...) {
		envIDs = append(envIDs, env.ID)
		key := fmt.Sprintf("%d:%s", repoID, env.Name)
		delete(ds.environments, key)
		if batch != nil {
			batch.Delete("environments", key)
		} else if ds.Persist != nil {
			ds.Persist.MustDelete("environments", key)
		}
	}
	delete(ds.envsByRepo, repoID)

	for _, pin := range ds.pinsByRepo[repoID] {
		delete(ds.pinnedEnvs, pin.ID)
		if batch != nil {
			batch.Delete("pinned_environments", strconv.Itoa(pin.ID))
		} else if ds.Persist != nil {
			ds.Persist.MustDelete("pinned_environments", strconv.Itoa(pin.ID))
		}
	}
	delete(ds.pinsByRepo, repoID)
	return envIDs
}

type autoInactiveDeployment struct {
	DeploymentID int               `json:"-"`
	Status       *DeploymentStatus `json:"-"`
	environment  string
}

type DeploymentBranchPolicy struct {
	ProtectedBranches    bool `json:"protected_branches"`
	CustomBranchPolicies bool `json:"custom_branch_policies"`
}

// DeploymentStatusState is one of the seven states GitHub emits.
type DeploymentStatusState string

func isCompletingDeploymentState(state string) bool {
	switch state {
	case "in_progress", "queued", "pending", "success", "failure", "error":
		return true
	}
	return false
}

const (
	DeploymentStateError      DeploymentStatusState = "error"
	DeploymentStateFailure    DeploymentStatusState = "failure"
	DeploymentStateInactive   DeploymentStatusState = "inactive"
	DeploymentStateInProgress DeploymentStatusState = "in_progress"
	DeploymentStateQueued     DeploymentStatusState = "queued"
	DeploymentStatePending    DeploymentStatusState = "pending"
	DeploymentStateSuccess    DeploymentStatusState = "success"
)

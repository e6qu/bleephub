package bleephub

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Ruleset is a GitHub repository or organization ruleset.
type Ruleset struct {
	ID                   int                    `json:"id"`
	NodeID               string                 `json:"node_id"`
	RepoID               int                    `json:"repo_id"`
	OrgID                int                    `json:"org_id"`
	Name                 string                 `json:"name"`
	Target               string                 `json:"target"` // branch, tag
	SourceType           string                 `json:"source_type"`
	Source               string                 `json:"source"`
	Enforcement          string                 `json:"enforcement"` // active, evaluate, disabled
	BypassActors         []RulesetBypassActor   `json:"bypass_actors"`
	CurrentUserCanBypass string                 `json:"current_user_can_bypass"`
	Conditions           RulesetConditions      `json:"conditions"`
	Rules                []Rule                 `json:"rules"`
	CreatedAt            time.Time              `json:"created_at"`
	UpdatedAt            time.Time              `json:"updated_at"`
	Versions             map[int]RulesetVersion `json:"versions,omitempty"`
	NextVersionID        int                    `json:"next_version_id,omitempty"`
}

// RulesetSuite is a single ruleset evaluation run.
type RulesetSuite struct {
	ID               int                 `json:"id"`
	ActorID          *int                `json:"actor_id"`
	ActorName        *string             `json:"actor_name"`
	BeforeSHA        string              `json:"before_sha"`
	AfterSHA         string              `json:"after_sha"`
	Ref              string              `json:"ref"`
	RepositoryID     int                 `json:"repository_id"`
	RepositoryName   string              `json:"repository_name"`
	OrganizationID   int                 `json:"organization_id,omitempty"`
	PushedAt         time.Time           `json:"pushed_at"`
	Result           string              `json:"result"`
	EvaluationResult *string             `json:"evaluation_result"`
	RuleEvaluations  []RulesetEvaluation `json:"rule_evaluations"`
}

// RulesetEvaluation is the result of one rule inside a rule suite.
type RulesetEvaluation struct {
	RuleSource  RulesetEvaluationSource `json:"rule_source"`
	Enforcement string                  `json:"enforcement"`
	Result      string                  `json:"result"`
	RuleType    string                  `json:"rule_type"`
	Details     *string                 `json:"details"`
}

// RulesetEvaluationSource identifies the repository or organization ruleset
// that contributed a rule to an evaluation.
type RulesetEvaluationSource struct {
	Type string  `json:"type"`
	ID   *int    `json:"id"`
	Name *string `json:"name"`
}

// RulesetBypassActor represents an actor that can bypass a ruleset.
type RulesetBypassActor struct {
	ActorID    int    `json:"actor_id"`
	ActorType  string `json:"actor_type"`
	BypassMode string `json:"bypass_mode"`
}

// RulesetConditions holds the conditions under which a ruleset applies.
type RulesetConditions struct {
	RefName RefNameCondition `json:"ref_name,omitempty"`
}

// RefNameCondition matches ref names.
type RefNameCondition struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

// Rule is a single rule inside a ruleset.
type Rule struct {
	Type       string                 `json:"type"`
	Parameters map[string]interface{} `json:"parameters,omitempty"`
}

// RulesetVersion is a historical snapshot of a ruleset. ActorID records the
// user who performed the update that superseded this version.
type RulesetVersion struct {
	VersionID int       `json:"version_id"`
	Ruleset   Ruleset   `json:"ruleset"`
	ActorID   int       `json:"actor_id"`
	CreatedAt time.Time `json:"created_at"`
}

func cloneRulesetParameter(value interface{}) interface{} {
	switch value := value.(type) {
	case map[string]interface{}:
		copy := make(map[string]interface{}, len(value))
		for key, item := range value {
			copy[key] = cloneRulesetParameter(item)
		}
		return copy
	case []interface{}:
		copy := make([]interface{}, len(value))
		for index, item := range value {
			copy[index] = cloneRulesetParameter(item)
		}
		return copy
	case []string:
		return append([]string(nil), value...)
	case map[string]string:
		copy := make(map[string]string, len(value))
		for key, item := range value {
			copy[key] = item
		}
		return copy
	default:
		return value
	}
}

func cloneRuleset(rs *Ruleset) *Ruleset {
	if rs == nil {
		return nil
	}
	copy := *rs
	copy.BypassActors = append([]RulesetBypassActor(nil), rs.BypassActors...)
	copy.Conditions.RefName.Include = append([]string(nil), rs.Conditions.RefName.Include...)
	copy.Conditions.RefName.Exclude = append([]string(nil), rs.Conditions.RefName.Exclude...)
	copy.Rules = make([]Rule, len(rs.Rules))
	for index, rule := range rs.Rules {
		copy.Rules[index] = rule
		if rule.Parameters != nil {
			copy.Rules[index].Parameters = cloneRulesetParameter(rule.Parameters).(map[string]interface{})
		}
	}
	copy.Versions = make(map[int]RulesetVersion, len(rs.Versions))
	for id, version := range rs.Versions {
		version.Ruleset = *cloneRuleset(&version.Ruleset)
		copy.Versions[id] = version
	}
	return &copy
}

// CreateRuleset creates and persists a new ruleset for a repository.
func (st *Store) CreateRuleset(repo *Repo, rs *Ruleset) *Ruleset {
	st.mu.Lock()
	defer st.mu.Unlock()

	rs = cloneRuleset(rs)
	rs.ID = st.NextRulesetID
	st.NextRulesetID++
	rs.NodeID = rulesetNodeID(rs.ID)
	rs.RepoID = repo.ID
	rs.SourceType = "Repository"
	rs.Source = repo.FullName
	rs.CurrentUserCanBypass = "never"
	if rs.Enforcement == "" {
		rs.Enforcement = "active"
	}
	if rs.Target == "" {
		rs.Target = "branch"
	}
	now := st.currentTime()
	rs.CreatedAt = now
	rs.UpdatedAt = now
	rs.Versions = map[int]RulesetVersion{}
	rs.NextVersionID = 1

	st.Rulesets[rs.ID] = rs
	st.persistRuleset(rs)
	return cloneRuleset(rs)
}

// UpdateRuleset updates an existing ruleset and records a history snapshot
// attributed to actorID.
func (st *Store) UpdateRuleset(repo *Repo, rs *Ruleset, updates *Ruleset, actorID int) *Ruleset {
	st.mu.Lock()
	defer st.mu.Unlock()
	if repo == nil || rs == nil || updates == nil {
		return nil
	}
	updates = cloneRuleset(updates)
	rs = st.Rulesets[rs.ID]
	if rs == nil || rs.RepoID != repo.ID {
		return nil
	}

	// Snapshot current state to history before mutating.
	snapshot := *rs
	snapshot.Versions = nil
	snapshot.NextVersionID = 0
	if rs.Versions == nil {
		rs.Versions = map[int]RulesetVersion{}
	}
	rs.Versions[rs.NextVersionID] = RulesetVersion{
		VersionID: rs.NextVersionID,
		Ruleset:   snapshot,
		ActorID:   actorID,
		CreatedAt: st.currentTime(),
	}
	rs.NextVersionID++

	if updates.Name != "" {
		rs.Name = updates.Name
	}
	if updates.Target != "" {
		rs.Target = updates.Target
	}
	if updates.Enforcement != "" {
		rs.Enforcement = updates.Enforcement
	}
	if updates.BypassActors != nil {
		rs.BypassActors = updates.BypassActors
	}
	if updates.CurrentUserCanBypass != "" {
		rs.CurrentUserCanBypass = updates.CurrentUserCanBypass
	}
	if len(updates.Conditions.RefName.Include) > 0 || len(updates.Conditions.RefName.Exclude) > 0 {
		rs.Conditions = updates.Conditions
	}
	if updates.Rules != nil {
		rs.Rules = updates.Rules
	}
	rs.UpdatedAt = st.currentTime()
	st.persistRuleset(rs)
	return cloneRuleset(rs)
}

// DeleteRuleset removes a ruleset.
func (st *Store) DeleteRuleset(id int) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.Rulesets[id]; !ok {
		return false
	}
	delete(st.Rulesets, id)
	if st.persist != nil {
		st.persist.MustDelete("repo_rulesets", strconv.Itoa(id))
	}
	return true
}

// GetRuleset returns a ruleset by ID.
func (st *Store) GetRuleset(id int) *Ruleset {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return cloneRuleset(st.Rulesets[id])
}

// CreateOrgRuleset creates and persists a new organization-level ruleset.
func (st *Store) CreateOrgRuleset(orgID int, name string, target string, enforcement string, conditions RulesetConditions, rules []Rule) *Ruleset {
	st.mu.Lock()
	defer st.mu.Unlock()

	rs := &Ruleset{
		ID:                   st.NextRulesetID,
		NodeID:               rulesetNodeID(st.NextRulesetID),
		OrgID:                orgID,
		RepoID:               0,
		Name:                 name,
		Target:               target,
		SourceType:           "Organization",
		Enforcement:          enforcement,
		CurrentUserCanBypass: "never",
		Conditions:           conditions,
		Rules:                rules,
		CreatedAt:            st.currentTime(),
		UpdatedAt:            st.currentTime(),
		Versions:             map[int]RulesetVersion{},
		NextVersionID:        1,
	}
	if rs.Target == "" {
		rs.Target = "branch"
	}
	if rs.Enforcement == "" {
		rs.Enforcement = "active"
	}
	if org := st.Orgs[orgID]; org != nil {
		rs.Source = org.Login
	}
	rs = cloneRuleset(rs)
	st.NextRulesetID++
	st.Rulesets[rs.ID] = rs
	st.persistRuleset(rs)
	return cloneRuleset(rs)
}

// ListOrgRulesets returns all rulesets for an organization, sorted by ID.
func (st *Store) ListOrgRulesets(orgID int) []*Ruleset {
	st.mu.RLock()
	defer st.mu.RUnlock()
	var out []*Ruleset
	for _, rs := range st.Rulesets {
		if rs.OrgID == orgID {
			out = append(out, cloneRuleset(rs))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// GetOrgRuleset returns a ruleset by ID.
func (st *Store) GetOrgRuleset(id int) *Ruleset {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return cloneRuleset(st.Rulesets[id])
}

// UpdateOrgRuleset applies a mutation to an organization ruleset and records
// a history snapshot attributed to actorID. Returns true when the ruleset
// existed.
func (st *Store) UpdateOrgRuleset(id int, actorID int, fn func(*Ruleset)) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	rs := st.Rulesets[id]
	if rs == nil {
		return false
	}

	// Snapshot current state to history before mutating.
	snapshot := *rs
	snapshot.Versions = nil
	snapshot.NextVersionID = 0
	if rs.Versions == nil {
		rs.Versions = map[int]RulesetVersion{}
	}
	rs.Versions[rs.NextVersionID] = RulesetVersion{
		VersionID: rs.NextVersionID,
		Ruleset:   snapshot,
		ActorID:   actorID,
		CreatedAt: st.currentTime(),
	}
	rs.NextVersionID++

	fn(rs)
	rs.UpdatedAt = st.currentTime()
	st.persistRuleset(rs)
	return true
}

// DeleteOrgRuleset removes an organization ruleset by ID.
func (st *Store) DeleteOrgRuleset(id int) bool {
	return st.DeleteRuleset(id)
}

// RecordRulesetSuite persists one completed evaluation. pushedAt is supplied
// by the caller so tests and importers never need to consult the wall clock.
func (st *Store) RecordRulesetSuite(repo *Repo, actor *User, ref, beforeSHA, afterSHA, result string, evaluationResult *string, evaluations []RulesetEvaluation, pushedAt time.Time) *RulesetSuite {
	if repo == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()

	suite := &RulesetSuite{
		ID:               st.NextRulesetSuiteID,
		BeforeSHA:        beforeSHA,
		AfterSHA:         afterSHA,
		Ref:              ref,
		RepositoryID:     repo.ID,
		RepositoryName:   repo.Name,
		PushedAt:         pushedAt.UTC(),
		Result:           result,
		EvaluationResult: evaluationResult,
		RuleEvaluations:  append([]RulesetEvaluation(nil), evaluations...),
	}
	if actor != nil {
		actorID, actorName := actor.ID, actor.Login
		suite.ActorID, suite.ActorName = &actorID, &actorName
	}
	if repo.OwnerType == "Organization" {
		suite.OrganizationID = repo.OwnerID
	}
	st.NextRulesetSuiteID++
	st.RulesetSuites[suite.ID] = suite
	if st.persist != nil {
		st.persist.MustPut("ruleset_suites", strconv.Itoa(suite.ID), suite)
	}
	return cloneRulesetSuite(suite)
}

// ListOrgRulesetSuites returns rule suites for an organization, newest first.
func (st *Store) ListOrgRulesetSuites(orgID int) []RulesetSuite {
	st.mu.RLock()
	defer st.mu.RUnlock()
	var out []RulesetSuite
	for _, suite := range st.RulesetSuites {
		if suite.OrganizationID == orgID {
			out = append(out, *cloneRulesetSuite(suite))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// GetOrgRulesetSuite returns a single rule suite for an organization.
func (st *Store) GetOrgRulesetSuite(orgID int, suiteID int) *RulesetSuite {
	st.mu.RLock()
	defer st.mu.RUnlock()
	suite := st.RulesetSuites[suiteID]
	if suite == nil || suite.OrganizationID != orgID {
		return nil
	}
	return cloneRulesetSuite(suite)
}

// GetRepoRulesetSuite returns a single rule suite for a repository.
func (st *Store) GetRepoRulesetSuite(repoID int, suiteID int) *RulesetSuite {
	st.mu.RLock()
	defer st.mu.RUnlock()
	suite := st.RulesetSuites[suiteID]
	if suite == nil || suite.RepositoryID != repoID {
		return nil
	}
	return cloneRulesetSuite(suite)
}

// ListRepoRulesetSuites returns rule suites for a repository, newest first.
func (st *Store) ListRepoRulesetSuites(repoID int) []RulesetSuite {
	st.mu.RLock()
	defer st.mu.RUnlock()
	var out []RulesetSuite
	for _, suite := range st.RulesetSuites {
		if suite.RepositoryID == repoID {
			out = append(out, *cloneRulesetSuite(suite))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// ListRulesetsForRepository returns repository rulesets and, when requested,
// organization rulesets that apply to the repository, sorted by ID.
func (st *Store) ListRulesetsForRepository(repo *Repo, includeParents bool) []*Ruleset {
	if repo == nil {
		return nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	var out []*Ruleset
	for _, rs := range st.Rulesets {
		if rs.RepoID == repo.ID ||
			(includeParents && repo.OwnerType == "Organization" && rs.OrgID != 0 && rs.OrgID == repo.OwnerID) {
			out = append(out, cloneRuleset(rs))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ApplicableRulesets snapshots every repository and organization ruleset that
// targets ref. Returning values rather than the live map entries lets push
// evaluation run without holding the global store lock across Git reads.
func (st *Store) ApplicableRulesets(repo *Repo, ref string) []Ruleset {
	if repo == nil {
		return nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()

	var target, short string
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		target, short = "branch", strings.TrimPrefix(ref, "refs/heads/")
	case strings.HasPrefix(ref, "refs/tags/"):
		target, short = "tag", strings.TrimPrefix(ref, "refs/tags/")
	default:
		return nil
	}
	var out []Ruleset
	for _, rs := range st.Rulesets {
		if !rulesetAppliesToRepo(rs, repo) || rs.Enforcement == "disabled" {
			continue
		}
		if rs.Target != "" && rs.Target != target {
			continue
		}
		if !rulesetMatchesBranch(rs, repo.DefaultBranch, short) {
			continue
		}
		clone := cloneRuleset(rs)
		out = append(out, *clone)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ListRulesForBranch evaluates active branch-targeting rulesets against a branch
// and returns the flattened rule objects GitHub's "list rules for a branch"
// endpoint produces.
func (st *Store) ListRulesForBranch(repo *Repo, branch string) []map[string]interface{} {
	st.mu.RLock()
	defer st.mu.RUnlock()

	var out []map[string]interface{}
	for _, rs := range st.Rulesets {
		if !rulesetAppliesToRepo(rs, repo) {
			continue
		}
		if rs.Enforcement == "disabled" {
			continue
		}
		if rs.Target != "" && rs.Target != "branch" {
			continue
		}
		if !rulesetMatchesBranch(rs, repo.DefaultBranch, branch) {
			continue
		}
		for _, rule := range rs.Rules {
			obj := map[string]interface{}{
				"type":                rule.Type,
				"ruleset_id":          rs.ID,
				"ruleset_source_type": rs.SourceType,
				"ruleset_source":      rs.Source,
			}
			if rule.Parameters != nil {
				obj["parameters"] = cloneRulesetParameter(rule.Parameters)
			}
			out = append(out, obj)
		}
	}
	return out
}

// BranchProtectedByRuleset reports whether an enforced branch-targeting
// repository or organization ruleset applies to the branch. Rulesets in
// evaluate mode are observable through the rules API but do not enforce
// restrictions and therefore do not make the branch protected.
func (st *Store) BranchProtectedByRuleset(repo *Repo, branch string) bool {
	st.mu.RLock()
	defer st.mu.RUnlock()

	for _, rs := range st.Rulesets {
		if !rulesetAppliesToRepo(rs, repo) ||
			rs.Enforcement != "active" ||
			(rs.Target != "" && rs.Target != "branch") ||
			len(rs.Rules) == 0 {
			continue
		}
		if rulesetMatchesBranch(rs, repo.DefaultBranch, branch) {
			return true
		}
	}
	return false
}

func rulesetAppliesToRepo(rs *Ruleset, repo *Repo) bool {
	return rs.RepoID == repo.ID ||
		(repo.OwnerType == "Organization" && rs.OrgID != 0 && rs.OrgID == repo.OwnerID)
}

// GetRulesetHistory returns prior versions of a ruleset.
func (st *Store) GetRulesetHistory(rs *Ruleset) []RulesetVersion {
	st.mu.RLock()
	defer st.mu.RUnlock()
	var out []RulesetVersion
	for _, v := range rs.Versions {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VersionID < out[j].VersionID })
	return out
}

// GetRulesetVersion returns a specific historical version.
func (st *Store) GetRulesetVersion(rs *Ruleset, versionID int) *RulesetVersion {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if v, ok := rs.Versions[versionID]; ok {
		return &v
	}
	return nil
}

func (st *Store) persistRuleset(rs *Ruleset) {
	if st.persist != nil {
		st.persist.MustPut("repo_rulesets", strconv.Itoa(rs.ID), rs)
	}
}

func rulesetNodeID(id int) string {
	return "RSR_" + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("ruleset:%d", id)))
}

func cloneRulesetSuite(suite *RulesetSuite) *RulesetSuite {
	if suite == nil {
		return nil
	}
	clone := *suite
	clone.RuleEvaluations = append([]RulesetEvaluation(nil), suite.RuleEvaluations...)
	return &clone
}

func rulesetMatchesBranch(rs *Ruleset, defaultBranch, branch string) bool {
	cond := rs.Conditions.RefName
	if len(cond.Include) == 0 && len(cond.Exclude) == 0 {
		return true
	}
	included := false
	for _, pat := range cond.Include {
		if matchRefPattern(pat, defaultBranch, branch) {
			included = true
			break
		}
	}
	if !included {
		return false
	}
	for _, pat := range cond.Exclude {
		if matchRefPattern(pat, defaultBranch, branch) {
			return false
		}
	}
	return true
}

func matchRefPattern(pat, defaultBranch, branch string) bool {
	pat = strings.TrimPrefix(pat, "refs/heads/")
	switch pat {
	case "~ALL", "*":
		return true
	case "~DEFAULT_BRANCH":
		return branch == defaultBranch
	}
	// Very small glob subset: trailing *.
	if strings.HasSuffix(pat, "*") {
		return strings.HasPrefix(branch, strings.TrimSuffix(pat, "*"))
	}
	return branch == pat
}

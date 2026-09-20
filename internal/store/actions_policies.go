package store

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Actions policies restrict which actors may trigger Actions workflows and
// which events may start them. A policy belongs to one repository or to one
// organization; an organization's policy applies to the repositories its
// conditions select. It is keyed by owner id, not name, so a rename or a
// transfer needs no cascade: the `source` a response reports is read from the
// owner as it is named now.

const (
	ActionsPolicyRuleRestrictActors = "restrict_actions_actors"
	ActionsPolicyRuleRestrictEvents = "restrict_action_events"
)

// ActionsPolicy is one stored policy. Exactly one of RepoID and OrgID is set.
type ActionsPolicy struct {
	ID          int                      `json:"id"`
	NodeID      string                   `json:"node_id"`
	RepoID      int                      `json:"repo_id,omitempty"`
	OrgID       int                      `json:"org_id,omitempty"`
	Name        string                   `json:"name"`
	Enforcement string                   `json:"enforcement"`
	Conditions  *ActionsPolicyConditions `json:"conditions,omitempty"`
	Rules       []ActionsPolicyRule      `json:"rules"`
	CreatedAt   time.Time                `json:"created_at"`
	UpdatedAt   time.Time                `json:"updated_at"`
}

// ActionsPolicyConditions selects what a policy governs. The three repository
// selectors belong to organization policies and at most one is set; a
// repository policy carries WorkflowPath alone. A nil WorkflowPath is an
// omitted condition, which targets every workflow.
type ActionsPolicyConditions struct {
	RepositoryName     *ActionsPolicyNameCondition     `json:"repository_name,omitempty"`
	RepositoryID       *ActionsPolicyIDCondition       `json:"repository_id,omitempty"`
	RepositoryProperty *ActionsPolicyPropertyCondition `json:"repository_property,omitempty"`
	WorkflowPath       *ActionsPolicyPathCondition     `json:"workflow_path,omitempty"`
}

type ActionsPolicyNameCondition struct {
	Include   []string `json:"include"`
	Exclude   []string `json:"exclude"`
	Protected bool     `json:"protected,omitempty"`
}

type ActionsPolicyIDCondition struct {
	RepositoryIDs []int `json:"repository_ids"`
}

type ActionsPolicyPropertyCondition struct {
	Include []ActionsPolicyPropertyTarget `json:"include"`
	Exclude []ActionsPolicyPropertyTarget `json:"exclude"`
}

type ActionsPolicyPropertyTarget struct {
	Name           string   `json:"name"`
	PropertyValues []string `json:"property_values"`
	Source         string   `json:"source,omitempty"`
}

type ActionsPolicyPathCondition struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

// ActionsPolicyRule is one rule of a policy. AllowedActors is meaningful for
// restrict_actions_actors and AllowedEvents for restrict_action_events.
type ActionsPolicyRule struct {
	Type          string               `json:"type"`
	AllowedActors []ActionsPolicyActor `json:"allowed_actors,omitempty"`
	AllowedEvents []string             `json:"allowed_events,omitempty"`
}

// ActionsPolicyActor names who a restrict_actions_actors rule admits. Type is
// one of ActionsPolicyActorTypes; ID is an id of that kind.
type ActionsPolicyActor struct {
	ID   int    `json:"id"`
	Type string `json:"type"`
}

// ActionsPolicyActorTypes are the actor kinds GitHub documents for the rule.
var ActionsPolicyActorTypes = map[string]bool{
	"User": true, "Bot": true, "Team": true, "BusinessTeam": true, "EnterpriseTeam": true,
	"IntegrationInstallation": true, "App": true, "RepositoryRole": true,
}

func actionsPolicyNodeID(id int) string {
	return "AP_" + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("actions-policy:%d", id)))
}

func cloneActionsPolicy(p *ActionsPolicy) *ActionsPolicy {
	if p == nil {
		return nil
	}
	clone := *p
	clone.Conditions = cloneActionsPolicyConditions(p.Conditions)
	clone.Rules = make([]ActionsPolicyRule, len(p.Rules))
	for i, rule := range p.Rules {
		rule.AllowedActors = append([]ActionsPolicyActor(nil), rule.AllowedActors...)
		rule.AllowedEvents = append([]string(nil), rule.AllowedEvents...)
		clone.Rules[i] = rule
	}
	return &clone
}

func cloneActionsPolicyConditions(c *ActionsPolicyConditions) *ActionsPolicyConditions {
	if c == nil {
		return nil
	}
	clone := &ActionsPolicyConditions{}
	if c.RepositoryName != nil {
		clone.RepositoryName = &ActionsPolicyNameCondition{
			Include:   append([]string(nil), c.RepositoryName.Include...),
			Exclude:   append([]string(nil), c.RepositoryName.Exclude...),
			Protected: c.RepositoryName.Protected,
		}
	}
	if c.RepositoryID != nil {
		clone.RepositoryID = &ActionsPolicyIDCondition{RepositoryIDs: append([]int(nil), c.RepositoryID.RepositoryIDs...)}
	}
	if c.RepositoryProperty != nil {
		clone.RepositoryProperty = &ActionsPolicyPropertyCondition{
			Include: cloneActionsPolicyPropertyTargets(c.RepositoryProperty.Include),
			Exclude: cloneActionsPolicyPropertyTargets(c.RepositoryProperty.Exclude),
		}
	}
	if c.WorkflowPath != nil {
		clone.WorkflowPath = &ActionsPolicyPathCondition{
			Include: append([]string(nil), c.WorkflowPath.Include...),
			Exclude: append([]string(nil), c.WorkflowPath.Exclude...),
		}
	}
	return clone
}

func cloneActionsPolicyPropertyTargets(in []ActionsPolicyPropertyTarget) []ActionsPolicyPropertyTarget {
	out := make([]ActionsPolicyPropertyTarget, len(in))
	for i, target := range in {
		target.PropertyValues = append([]string(nil), target.PropertyValues...)
		out[i] = target
	}
	return out
}

func (st *Store) persistActionsPolicy(p *ActionsPolicy) {
	if st.Persist != nil {
		st.Persist.MustPut("actions_policies", strconv.Itoa(p.ID), p)
	}
}

// CreateActionsPolicy stores a new policy for the repository or organization
// its RepoID or OrgID names.
func (st *Store) CreateActionsPolicy(p *ActionsPolicy) *ActionsPolicy {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	p = cloneActionsPolicy(p)
	p.ID = st.ReserveGlobalID("next_actions_policy", &st.NextActionsPolicyID)
	p.NodeID = actionsPolicyNodeID(p.ID)
	now := st.CurrentTime()
	p.CreatedAt, p.UpdatedAt = now, now
	st.ActionsPolicies[p.ID] = p
	st.persistActionsPolicy(p)
	return cloneActionsPolicy(p)
}

// GetActionsPolicy returns a detached snapshot of a policy, or nil.
func (st *Store) GetActionsPolicy(id int) *ActionsPolicy {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return cloneActionsPolicy(st.ActionsPolicies[id])
}

// UpdateActionsPolicy applies mutate to a policy and restamps it.
func (st *Store) UpdateActionsPolicy(id int, mutate func(*ActionsPolicy)) *ActionsPolicy {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	p := st.ActionsPolicies[id]
	if p == nil {
		return nil
	}
	mutate(p)
	p.UpdatedAt = st.CurrentTime()
	st.persistActionsPolicy(p)
	return cloneActionsPolicy(p)
}

// DeleteActionsPolicy removes a policy, reporting whether it existed.
func (st *Store) DeleteActionsPolicy(id int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.ActionsPolicies[id] == nil {
		return false
	}
	delete(st.ActionsPolicies, id)
	if st.Persist != nil {
		st.Persist.MustDelete("actions_policies", strconv.Itoa(id))
	}
	return true
}

// ListOrgActionsPolicies returns the policies an organization owns, by id.
func (st *Store) ListOrgActionsPolicies(orgID int) []*ActionsPolicy {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*ActionsPolicy
	for _, p := range st.ActionsPolicies {
		if p.OrgID == orgID {
			out = append(out, cloneActionsPolicy(p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ListRepoActionsPolicies returns the policies a repository owns and, with
// includeParents, those of its organization that select it, by id.
func (st *Store) ListRepoActionsPolicies(repo *Repo, includeParents bool) []*ActionsPolicy {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*ActionsPolicy
	for _, p := range st.ActionsPolicies {
		if p.RepoID == repo.ID || (includeParents && st.orgActionsPolicySelectsRepoLocked(p, repo)) {
			out = append(out, cloneActionsPolicy(p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ActionsPolicySource reports the kind and current name of a policy's owner.
func (st *Store) ActionsPolicySource(p *ActionsPolicy) (sourceType, source string) {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if p.OrgID != 0 {
		if org := st.Orgs[p.OrgID]; org != nil {
			return "Organization", org.Login
		}
		return "Organization", ""
	}
	if repo := st.Repos[p.RepoID]; repo != nil {
		return "Repository", repo.FullName
	}
	return "Repository", ""
}

// deleteActionsPoliciesLocked drops every policy owned by the repository or
// organization. Exactly one id is non-zero. Caller holds st.Mu.
func (st *Store) deleteActionsPoliciesLocked(repoID, orgID int, batch interface{ Delete(bucket, key string) }) {
	for id, p := range st.ActionsPolicies {
		if (repoID != 0 && p.RepoID == repoID) || (orgID != 0 && p.OrgID == orgID) {
			delete(st.ActionsPolicies, id)
			batch.Delete("actions_policies", strconv.Itoa(id))
		}
	}
}

// orgActionsPolicySelectsRepoLocked reports whether an organization's policy
// governs the repository. A policy with no repository selector governs every
// repository of the organization.
func (st *Store) orgActionsPolicySelectsRepoLocked(p *ActionsPolicy, repo *Repo) bool {
	if p.OrgID == 0 || repo.OwnerType != "Organization" || repo.OwnerID != p.OrgID {
		return false
	}
	c := p.Conditions
	switch {
	case c == nil:
		return true
	case c.RepositoryID != nil:
		for _, id := range c.RepositoryID.RepositoryIDs {
			if id == repo.ID {
				return true
			}
		}
		return false
	case c.RepositoryName != nil:
		return actionsPolicyPatternsSelect(c.RepositoryName.Include, c.RepositoryName.Exclude, repo.Name)
	case c.RepositoryProperty != nil:
		values := st.RepoCustomPropertyValues[repo.FullName]
		for _, target := range c.RepositoryProperty.Exclude {
			if actionsPolicyPropertyMatches(target, values) {
				return false
			}
		}
		if len(c.RepositoryProperty.Include) == 0 {
			return true
		}
		for _, target := range c.RepositoryProperty.Include {
			if actionsPolicyPropertyMatches(target, values) {
				return true
			}
		}
		return false
	}
	return true
}

func actionsPolicyPropertyMatches(target ActionsPolicyPropertyTarget, values map[string]interface{}) bool {
	held, ok := values[target.Name]
	if !ok {
		return false
	}
	var heldValues []string
	switch v := held.(type) {
	case string:
		heldValues = []string{v}
	case []string:
		heldValues = v
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				heldValues = append(heldValues, s)
			}
		}
	}
	for _, want := range target.PropertyValues {
		for _, have := range heldValues {
			if want == have {
				return true
			}
		}
	}
	return false
}

// actionsPolicyPatternsSelect applies an include/exclude pattern pair: an
// excluded name is never selected; otherwise an empty include list, or `~ALL`,
// selects everything not excluded.
func actionsPolicyPatternsSelect(include, exclude []string, name string) bool {
	for _, pattern := range exclude {
		if actionsPolicyGlobMatches(pattern, name) {
			return false
		}
	}
	if len(include) == 0 {
		return true
	}
	for _, pattern := range include {
		if pattern == "~ALL" || actionsPolicyGlobMatches(pattern, name) {
			return true
		}
	}
	return false
}

// actionsPolicyGlobMatches implements the fnmatch subset GitHub documents for
// these patterns: `*` within a path segment, `**` across segments, `?` for one
// character.
func actionsPolicyGlobMatches(pattern, name string) bool {
	var expr strings.Builder
	expr.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch {
		case strings.HasPrefix(pattern[i:], "**"):
			expr.WriteString(".*")
			i++
		case pattern[i] == '*':
			expr.WriteString("[^/]*")
		case pattern[i] == '?':
			expr.WriteString("[^/]")
		default:
			expr.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	expr.WriteString("$")
	matcher, err := regexp.Compile(expr.String())
	return err == nil && matcher.MatchString(name)
}

// ApplicableActionsPolicies returns the active policies that govern a workflow
// of a repository, by id: the repository's own and those of its organization
// that select it, narrowed to the ones whose workflow_path condition matches.
// `evaluate` and `disabled` policies are left out, since they never refuse.
func (st *Store) ApplicableActionsPolicies(repo *Repo, workflowPath string) []*ActionsPolicy {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*ActionsPolicy
	for _, p := range st.ActionsPolicies {
		if p.Enforcement != "active" {
			continue
		}
		if p.RepoID != repo.ID && !st.orgActionsPolicySelectsRepoLocked(p, repo) {
			continue
		}
		if p.Conditions != nil && p.Conditions.WorkflowPath != nil &&
			!actionsPolicyPatternsSelect(p.Conditions.WorkflowPath.Include, p.Conditions.WorkflowPath.Exclude, workflowPath) {
			continue
		}
		out = append(out, cloneActionsPolicy(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ActionsPolicyVerdict is what a set of policies makes of a trigger. A refusal
// names the policy and rule responsible, so it can be explained.
type ActionsPolicyVerdict struct {
	Refused    bool
	PolicyID   int
	PolicyName string
	Rule       string
}

// EvaluateActionsPolicies decides whether an event may start a workflow under
// the given policies. Every rule of every policy must admit it. A
// restrict_action_events rule admits only its listed events. A
// restrict_actions_actors rule admits the trigger when admits reports true for
// any actor it lists; admits is the caller's, because resolving a team, an app
// or a repository role needs more than the policy knows. A trigger with no
// actor at all (a schedule) matches no listed actor, so such a rule refuses it.
func EvaluateActionsPolicies(policies []*ActionsPolicy, event string, admits func(ActionsPolicyActor) bool) ActionsPolicyVerdict {
	for _, p := range policies {
		for _, rule := range p.Rules {
			admitted := true
			switch rule.Type {
			case ActionsPolicyRuleRestrictEvents:
				admitted = false
				for _, allowed := range rule.AllowedEvents {
					if allowed == event {
						admitted = true
						break
					}
				}
			case ActionsPolicyRuleRestrictActors:
				admitted = false
				for _, actor := range rule.AllowedActors {
					if admits(actor) {
						admitted = true
						break
					}
				}
			}
			if !admitted {
				return ActionsPolicyVerdict{Refused: true, PolicyID: p.ID, PolicyName: p.Name, Rule: rule.Type}
			}
		}
	}
	return ActionsPolicyVerdict{}
}

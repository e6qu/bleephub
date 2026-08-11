package store

import (
	"fmt"
	"strconv"
)

// DeploymentBranchPolicyRule is one branch/tag name pattern allowed to
// deploy to an environment.
type DeploymentBranchPolicyRule struct {
	ID     int                        `json:"id"`
	NodeID string                     `json:"node_id"`
	Name   string                     `json:"name"`
	Type   DeploymentBranchPolicyType `json:"type"`
}

// EnvCustomProtectionRule is a custom deployment protection rule enabled on
// an environment, backed by a GitHub App integration.
type EnvCustomProtectionRule struct {
	ID      int    `json:"id"`
	NodeID  string `json:"node_id"`
	Enabled bool   `json:"enabled"`
	AppID   int    `json:"app_id"`
}

// CreateEnvBranchPolicy appends a branch/tag policy to an environment.
// Returns (nil, existing) when a policy with the same name+type already
// exists (the API answers 303 See Other pointing at it).
func (st *Store) CreateEnvBranchPolicy(envID int, name, policyType string) (created, existing *DeploymentBranchPolicyRule) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	for _, p := range st.EnvBranchPolicies[envID] {
		if p.Name == name && p.Type == DeploymentBranchPolicyType(policyType) {
			return nil, p
		}
	}
	p := &DeploymentBranchPolicyRule{
		ID:     st.NextEnvBranchPolicyID,
		NodeID: fmt.Sprintf("DBP_kwDO%08d", st.NextEnvBranchPolicyID),
		Name:   name,
		Type:   DeploymentBranchPolicyType(policyType),
	}
	st.NextEnvBranchPolicyID++
	st.EnvBranchPolicies[envID] = append(st.EnvBranchPolicies[envID], p)
	if st.Persist != nil {
		st.Persist.MustPut("env_branch_policies", strconv.Itoa(envID), st.EnvBranchPolicies[envID])
	}
	return p, nil
}

// ListEnvBranchPolicies returns an environment's branch/tag policies in
// creation order.
func (st *Store) ListEnvBranchPolicies(envID int) []*DeploymentBranchPolicyRule {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := make([]*DeploymentBranchPolicyRule, len(st.EnvBranchPolicies[envID]))
	copy(out, st.EnvBranchPolicies[envID])
	return snapshotSlice(out)
}

// GetEnvBranchPolicy returns one policy by ID, or nil.
func (st *Store) GetEnvBranchPolicy(envID, policyID int) *DeploymentBranchPolicyRule {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, p := range st.EnvBranchPolicies[envID] {
		if p.ID == policyID {
			// Detach: DeploymentBranchPolicyRule is all-value (STORE-021).
			clone := *p
			return &clone
		}
	}
	return nil
}

// UpdateEnvBranchPolicy renames a policy's pattern. Returns the updated
// policy, or nil when not found.
func (st *Store) UpdateEnvBranchPolicy(envID, policyID int, name string) *DeploymentBranchPolicyRule {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	for _, p := range st.EnvBranchPolicies[envID] {
		if p.ID == policyID {
			p.Name = name
			if st.Persist != nil {
				st.Persist.MustPut("env_branch_policies", strconv.Itoa(envID), st.EnvBranchPolicies[envID])
			}
			return p
		}
	}
	return nil
}

// DeleteEnvBranchPolicy removes a policy. Returns true if it existed.
func (st *Store) DeleteEnvBranchPolicy(envID, policyID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	policies := st.EnvBranchPolicies[envID]
	for i, p := range policies {
		if p.ID == policyID {
			st.EnvBranchPolicies[envID] = append(policies[:i], policies[i+1:]...)
			if st.Persist != nil {
				st.Persist.MustPut("env_branch_policies", strconv.Itoa(envID), st.EnvBranchPolicies[envID])
			}
			return true
		}
	}
	return false
}

// CreateEnvProtectionRule enables a custom protection rule (a GitHub App
// integration) on an environment. Returns nil when the app already has an
// enabled rule on the environment.
func (st *Store) CreateEnvProtectionRule(envID, appID int) *EnvCustomProtectionRule {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	for _, rule := range st.EnvProtectionRules[envID] {
		if rule.AppID == appID {
			return nil
		}
	}
	rule := &EnvCustomProtectionRule{
		ID:      st.NextEnvProtectionRuleID,
		NodeID:  fmt.Sprintf("GA_kwDP%08d", st.NextEnvProtectionRuleID),
		Enabled: true,
		AppID:   appID,
	}
	st.NextEnvProtectionRuleID++
	st.EnvProtectionRules[envID] = append(st.EnvProtectionRules[envID], rule)
	if st.Persist != nil {
		st.Persist.MustPut("env_protection_rules", strconv.Itoa(envID), st.EnvProtectionRules[envID])
	}
	return rule
}

// ListEnvProtectionRules returns an environment's enabled custom protection
// rules in creation order.
func (st *Store) ListEnvProtectionRules(envID int) []*EnvCustomProtectionRule {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	out := make([]*EnvCustomProtectionRule, len(st.EnvProtectionRules[envID]))
	copy(out, st.EnvProtectionRules[envID])
	return snapshotSlice(out)
}

// GetEnvProtectionRule returns one rule by ID, or nil.
func (st *Store) GetEnvProtectionRule(envID, ruleID int) *EnvCustomProtectionRule {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, rule := range st.EnvProtectionRules[envID] {
		if rule.ID == ruleID {
			// Detach: EnvCustomProtectionRule is all-value (STORE-021).
			clone := *rule
			return &clone
		}
	}
	return nil
}

// DeleteEnvProtectionRule disables (removes) a rule. Returns true if it existed.
func (st *Store) DeleteEnvProtectionRule(envID, ruleID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	rules := st.EnvProtectionRules[envID]
	for i, rule := range rules {
		if rule.ID == ruleID {
			st.EnvProtectionRules[envID] = append(rules[:i], rules[i+1:]...)
			if st.Persist != nil {
				st.Persist.MustPut("env_protection_rules", strconv.Itoa(envID), st.EnvProtectionRules[envID])
			}
			return true
		}
	}
	return false
}

// PruneEnvironmentPolicies drops all policies and protection rules for a
// deleted environment.
func (st *Store) PruneEnvironmentPolicies(envID int) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	// One transaction: an environment's branch policies and protection rules are
	// pruned together, so a crash cannot leave half an environment's policy set
	// behind after it is gone (STORE-001/002).
	batch := NewPersistBatch(st.Persist)
	if _, ok := st.EnvBranchPolicies[envID]; ok {
		delete(st.EnvBranchPolicies, envID)
		batch.Delete("env_branch_policies", strconv.Itoa(envID))
	}
	if _, ok := st.EnvProtectionRules[envID]; ok {
		delete(st.EnvProtectionRules, envID)
		batch.Delete("env_protection_rules", strconv.Itoa(envID))
	}
	if err := batch.Commit(); err != nil {
		panic(&PersistenceFailure{Op: "batch", Bucket: "env_branch_policies", Err: err})
	}
}

// DeploymentBranchPolicyType is the kind of a deployment branch/tag policy
// rule. GitHub accepts only these two. A typed string marshals to JSON
// identically to a plain string.
type DeploymentBranchPolicyType string

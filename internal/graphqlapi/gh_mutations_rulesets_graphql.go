package graphqlapi

// The repository-ruleset write mutations: createRepositoryRuleset,
// updateRepositoryRuleset and deleteRepositoryRuleset. They write through the
// same store primitives the REST ruleset routes write through —
// CreateRuleset/CreateOrgRuleset, UpdateRuleset/UpdateOrgRuleset and
// DeleteRuleset — so a ruleset authored over GraphQL is evaluated by the same
// enforcement chokepoint and read back identically over REST.
//
// A ruleset's source may be a repository, an organization or an enterprise.
// The first two are supported here; the enterprise scope stays with the REST
// enterprise-rulesets surface because bleephub's RuleSource union (built with
// the read surface, before the Enterprise type exists) cannot yet name an
// enterprise, and a payload that could not render its ruleset would be worse
// than an explicit refusal.

import (
	"fmt"
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

func init() {
	for name, rule := range map[string]mutationRule{
		"createRepositoryRuleset": rulesetOwnerRule{sourceKey: "sourceId"},
		"updateRepositoryRuleset": rulesetOwnerRule{rulesetKey: "repositoryRulesetId"},
		"deleteRepositoryRuleset": rulesetOwnerRule{rulesetKey: "repositoryRulesetId"},
	} {
		if _, exists := graphqlMutationAuthz[name]; exists {
			panic(fmt.Sprintf("graphql mutation %q already has a policy row", name))
		}
		graphqlMutationAuthz[name] = rule
	}
}

// rulesetOwnerRule is the policy for the ruleset mutations. The entitlement
// is over the ruleset's source: repository administration for a repository
// ruleset, organization administration for an organization ruleset, and
// enterprise ownership for an enterprise one (whose write the resolver then
// declines in favor of the REST surface).
type rulesetOwnerRule struct {
	// sourceKey names the source account/repository directly; rulesetKey
	// names an existing ruleset whose source is looked up. Exactly one is set.
	sourceKey  string
	rulesetKey string
}

func (r rulesetOwnerRule) check() error {
	if (r.sourceKey == "") == (r.rulesetKey == "") {
		return fmt.Errorf("exactly one of sourceKey or rulesetKey must be set")
	}
	return nil
}

func (r rulesetOwnerRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	if r.rulesetKey != "" {
		nodeID, _ := input[r.rulesetKey].(string)
		rs := store.FindRulesetByNodeID(s.store, nodeID)
		if rs == nil {
			return gqlMissingNode("RepositoryRuleset", nodeID)
		}
		switch {
		case rs.RepoID != 0:
			return r.authorizeRepo(s, p, s.store.GetRepoByID(rs.RepoID), nodeID)
		case rs.OrgID != 0:
			org := s.store.GetOrgByID(rs.OrgID)
			if org == nil {
				return gqlMissingNode("RepositoryRuleset", nodeID)
			}
			return s.authorizeOrgAdministration(p, org.Login, store.ScopeOrgAdministration)
		default:
			return r.authorizeEnterprise(s, p, rs.Enterprise)
		}
	}
	nodeID, _ := input[r.sourceKey].(string)
	if repo := store.FindRepoByNodeID(s.store, nodeID); repo != nil {
		return r.authorizeRepo(s, p, repo, nodeID)
	}
	if org := s.orgByNodeID(nodeID); org != nil {
		return s.authorizeOrgAdministration(p, org.Login, store.ScopeOrgAdministration)
	}
	if e := store.FindEnterpriseByNodeID(s.store, nodeID); e != nil {
		if !s.store.IsEnterpriseOwner(e.ID, s.ghUserFromContext(p.Context)) {
			return enterpriseOwnerRequired()
		}
		return nil
	}
	return gqlMissingNode("RuleSource", nodeID)
}

func (r rulesetOwnerRule) authorizeRepo(s *Resolver, p graphql.ResolveParams, repo *store.Repo, nodeID string) error {
	missing := gqlMissingNode("RepositoryRuleset", nodeID)
	if r.sourceKey != "" {
		missing = gqlMissingNode("Repository", nodeID)
	}
	if repo == nil || !s.viewerCanReadRepo(p.Context, repo) {
		return missing
	}
	if !s.credentialGrantsRepo(p.Context, repo, store.ScopeAdministration, store.PermWrite) {
		return &ghForbiddenError{message: "resource not accessible by integration"}
	}
	if !s.principalHoldsRepoCapability(p.Context, repo, store.PermAdmin) {
		return &ghForbiddenError{message: "must have admin rights to Repository"}
	}
	return nil
}

func (r rulesetOwnerRule) authorizeEnterprise(s *Resolver, p graphql.ResolveParams, slug string) error {
	e := s.store.GetEnterprise(slug)
	if e == nil || !s.store.IsEnterpriseOwner(e.ID, s.ghUserFromContext(p.Context)) {
		return enterpriseOwnerRequired()
	}
	return nil
}

// --- schema -----------------------------------------------------------------

// addRulesetMutationsToSchema installs the three ruleset write mutations. It
// runs after the ruleset read surface, whose RepositoryRuleset object the
// payloads return.
func (s *Resolver) addRulesetMutationsToSchema(mutationType *graphql.Object) {
	rulesetType := s.accountSurfaceRegistry().ruleset
	targetEnum := s.sharedEnum("RepositoryRulesetTarget", "BRANCH", "PUSH", "REPOSITORY", "TAG")
	enforcementEnum := s.sharedEnum("RuleEnforcement", "ACTIVE", "DISABLED", "EVALUATE")

	bypassActorInput := s.mutationInput("RepositoryRulesetBypassActorInput", graphql.InputObjectConfigFieldMap{
		"actorId":                  gqlID(),
		"bypassMode":               gqlNonNullInputOf(s.sharedEnum("RepositoryRulesetBypassActorBypassMode", "ALWAYS", "EXEMPT", "PULL_REQUEST")),
		"deployKey":                gqlBool(),
		"enterpriseOwner":          gqlBool(),
		"organizationAdmin":        gqlBool(),
		"repositoryRoleDatabaseId": gqlInt(),
	})

	conditionsInput := s.mutationInput("RepositoryRuleConditionsInput", graphql.InputObjectConfigFieldMap{
		"refName": gqlInputOf(s.mutationInput("RefNameConditionTargetInput", graphql.InputObjectConfigFieldMap{
			"exclude": gqlNonNullListOf(graphql.String),
			"include": gqlNonNullListOf(graphql.String),
		})),
	})

	ruleInput := s.mutationInput("RepositoryRuleInput", graphql.InputObjectConfigFieldMap{
		"parameters": gqlInputOf(s.gqlRuleParametersInput()),
		"type":       gqlNonNullInputOf(s.sharedRepositoryRuleTypeEnum()),
	})

	s.registerMutation(mutationType, "createRepositoryRuleset", &graphql.Field{
		Type: s.mutationPayload("CreateRepositoryRulesetPayload", graphql.Fields{
			"ruleset": gqlField(rulesetType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("CreateRepositoryRulesetInput", graphql.InputObjectConfigFieldMap{
				"bypassActors": gqlListOf(bypassActorInput),
				"conditions":   gqlNonNullInputOf(conditionsInput),
				"enforcement":  gqlNonNullInputOf(enforcementEnum),
				"name":         gqlNonNullString(),
				"rules":        gqlListOf(ruleInput),
				"sourceId":     gqlNonNullID(),
				"target":       gqlInputOf(targetEnum),
			})),
		}},
		Resolve: s.resolveCreateRepositoryRuleset,
	})

	s.registerMutation(mutationType, "updateRepositoryRuleset", &graphql.Field{
		Type: s.mutationPayload("UpdateRepositoryRulesetPayload", graphql.Fields{
			"ruleset": gqlField(rulesetType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateRepositoryRulesetInput", graphql.InputObjectConfigFieldMap{
				"bypassActors":        gqlListOf(bypassActorInput),
				"conditions":          gqlInputOf(conditionsInput),
				"enforcement":         gqlInputOf(enforcementEnum),
				"name":                gqlString(),
				"repositoryRulesetId": gqlNonNullID(),
				"rules":               gqlListOf(ruleInput),
				"target":              gqlInputOf(targetEnum),
			})),
		}},
		Resolve: s.resolveUpdateRepositoryRuleset,
	})

	s.registerMutation(mutationType, "deleteRepositoryRuleset", &graphql.Field{
		Type: s.mutationPayload("DeleteRepositoryRulesetPayload", graphql.Fields{}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("DeleteRepositoryRulesetInput", graphql.InputObjectConfigFieldMap{
				"repositoryRulesetId": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveDeleteRepositoryRuleset,
	})
}

// gqlRuleParametersInput transcribes RuleParametersInput and the per-rule
// parameter inputs bleephub's ruleset evaluator understands. The camelCase
// members convert to the snake_case parameter maps the REST ruleset bodies
// carry, so one stored shape serves both surfaces.
func (s *Resolver) gqlRuleParametersInput() *graphql.InputObject {
	pattern := func(name string) *graphql.InputObject {
		return s.mutationInput(name, graphql.InputObjectConfigFieldMap{
			"name":     gqlString(),
			"negate":   gqlBool(),
			"operator": gqlNonNullString(),
			"pattern":  gqlNonNullString(),
		})
	}
	statusCheck := s.mutationInput("StatusCheckConfigurationInput", graphql.InputObjectConfigFieldMap{
		"context":       gqlNonNullString(),
		"integrationId": gqlInt(),
	})
	workflowFile := s.mutationInput("WorkflowFileReferenceInput", graphql.InputObjectConfigFieldMap{
		"path":         gqlNonNullString(),
		"ref":          gqlString(),
		"repositoryId": gqlNonNullInt(),
		"sha":          gqlString(),
	})
	return s.mutationInput("RuleParametersInput", graphql.InputObjectConfigFieldMap{
		"branchNamePattern":        gqlInputOf(pattern("BranchNamePatternParametersInput")),
		"commitAuthorEmailPattern": gqlInputOf(pattern("CommitAuthorEmailPatternParametersInput")),
		"commitMessagePattern":     gqlInputOf(pattern("CommitMessagePatternParametersInput")),
		"committerEmailPattern":    gqlInputOf(pattern("CommitterEmailPatternParametersInput")),
		"tagNamePattern":           gqlInputOf(pattern("TagNamePatternParametersInput")),
		"fileExtensionRestriction": gqlInputOf(s.mutationInput("FileExtensionRestrictionParametersInput", graphql.InputObjectConfigFieldMap{
			"restrictedFileExtensions": gqlNonNullListOf(graphql.String),
		})),
		"filePathRestriction": gqlInputOf(s.mutationInput("FilePathRestrictionParametersInput", graphql.InputObjectConfigFieldMap{
			"restrictedFilePaths": gqlNonNullListOf(graphql.String),
		})),
		"maxFilePathLength": gqlInputOf(s.mutationInput("MaxFilePathLengthParametersInput", graphql.InputObjectConfigFieldMap{
			"maxFilePathLength": gqlNonNullInt(),
		})),
		"maxFileSize": gqlInputOf(s.mutationInput("MaxFileSizeParametersInput", graphql.InputObjectConfigFieldMap{
			"maxFileSize": gqlNonNullInt(),
		})),
		"pullRequest": gqlInputOf(s.mutationInput("PullRequestParametersInput", graphql.InputObjectConfigFieldMap{
			"dismissStaleReviewsOnPush":      gqlNonNullBool(),
			"requireCodeOwnerReview":         gqlNonNullBool(),
			"requireLastPushApproval":        gqlNonNullBool(),
			"requiredApprovingReviewCount":   gqlNonNullInt(),
			"requiredReviewThreadResolution": gqlNonNullBool(),
		})),
		"requiredDeployments": gqlInputOf(s.mutationInput("RequiredDeploymentsParametersInput", graphql.InputObjectConfigFieldMap{
			"requiredDeploymentEnvironments": gqlNonNullListOf(graphql.String),
		})),
		"requiredStatusChecks": gqlInputOf(s.mutationInput("RequiredStatusChecksParametersInput", graphql.InputObjectConfigFieldMap{
			"doNotEnforceOnCreate":             gqlBool(),
			"requiredStatusChecks":             gqlNonNullListOf(statusCheck),
			"strictRequiredStatusChecksPolicy": gqlNonNullBool(),
		})),
		"update": gqlInputOf(s.mutationInput("UpdateParametersInput", graphql.InputObjectConfigFieldMap{
			"updateAllowsFetchAndMerge": gqlNonNullBool(),
		})),
		"workflows": gqlInputOf(s.mutationInput("WorkflowsParametersInput", graphql.InputObjectConfigFieldMap{
			"doNotEnforceOnCreate": gqlBool(),
			"workflows":            gqlNonNullListOf(workflowFile),
		})),
	})
}

// --- input conversion --------------------------------------------------------

// rulesetConditionsFromInput reads the refName condition into the store shape.
func rulesetConditionsFromInput(input map[string]interface{}) (store.RulesetConditions, bool) {
	conditions, ok := gqlInputObject(input, "conditions")
	if !ok {
		return store.RulesetConditions{}, false
	}
	refName, ok := conditions["refName"].(map[string]interface{})
	if !ok {
		return store.RulesetConditions{}, true
	}
	include, _ := gqlInputStrings(refName, "include")
	exclude, _ := gqlInputStrings(refName, "exclude")
	return store.RulesetConditions{RefName: store.RefNameCondition{Include: include, Exclude: exclude}}, true
}

// rulesetRulesFromInput converts [RepositoryRuleInput!] to store rules; the
// enum type lowers to the REST spelling, the camelCase parameters to the
// snake_case parameter map.
func rulesetRulesFromInput(input map[string]interface{}) ([]store.Rule, bool) {
	if _, present := input["rules"]; !present {
		return nil, false
	}
	items := gqlInputObjects(input, "rules")
	rules := make([]store.Rule, 0, len(items))
	for _, item := range items {
		ruleType, _ := item["type"].(string)
		if ruleType == "" {
			continue
		}
		rule := store.Rule{Type: strings.ToLower(ruleType)}
		if parameters, ok := item["parameters"].(map[string]interface{}); ok {
			for _, member := range parameters {
				if body, ok := member.(map[string]interface{}); ok {
					rule.Parameters = snakeCaseParameterMap(body)
					break
				}
			}
		}
		rules = append(rules, rule)
	}
	return rules, true
}

// snakeCaseParameterMap converts a camelCase GraphQL parameter object into
// the snake_case map the REST ruleset bodies store, recursively.
func snakeCaseParameterMap(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for key, value := range in {
		out[camelToSnake(key)] = snakeCaseParameterValue(value)
	}
	return out
}

func snakeCaseParameterValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		return snakeCaseParameterMap(typed)
	case []interface{}:
		out := make([]interface{}, len(typed))
		for i, item := range typed {
			out[i] = snakeCaseParameterValue(item)
		}
		return out
	default:
		return value
	}
}

func camelToSnake(name string) string {
	var b strings.Builder
	for i, r := range name {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// rulesetBypassActorsFromInput converts [RepositoryRulesetBypassActorInput!]
// to the actor rows the REST bypass_actors member stores.
func (s *Resolver) rulesetBypassActorsFromInput(input map[string]interface{}) ([]store.RulesetBypassActor, bool) {
	if _, present := input["bypassActors"]; !present {
		return nil, false
	}
	items := gqlInputObjects(input, "bypassActors")
	actors := make([]store.RulesetBypassActor, 0, len(items))
	for _, item := range items {
		mode, _ := item["bypassMode"].(string)
		actor := store.RulesetBypassActor{BypassMode: strings.ToLower(mode)}
		switch {
		case boolMember(item, "organizationAdmin"):
			actor.ActorType = "OrganizationAdmin"
		case boolMember(item, "deployKey"):
			actor.ActorType = "DeployKey"
		case boolMember(item, "enterpriseOwner"):
			actor.ActorType = "EnterpriseOwner"
		default:
			if roleID, ok := gqlInputInt(item, "repositoryRoleDatabaseId"); ok {
				actor.ActorType = "RepositoryRole"
				actor.ActorID = roleID
				break
			}
			nodeID, _ := item["actorId"].(string)
			if team, _ := store.FindTeamByNodeID(s.store, nodeID); team != nil {
				actor.ActorType = "Team"
				actor.ActorID = team.ID
			} else if user := store.FindUserByNodeID(s.store, nodeID); user != nil {
				actor.ActorType = "Integration"
				actor.ActorID = user.ID
			} else {
				continue
			}
		}
		actors = append(actors, actor)
	}
	return actors, true
}

func boolMember(input map[string]interface{}, key string) bool {
	value, _ := input[key].(bool)
	return value
}

// --- resolvers ---------------------------------------------------------------

func (s *Resolver) resolveCreateRepositoryRuleset(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	name, _ := gqlInputString(input, "name")
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("Name can't be blank")
	}
	enforcement, _ := gqlInputString(input, "enforcement")
	target, _ := gqlInputString(input, "target")
	conditions, _ := rulesetConditionsFromInput(input)
	rules, _ := rulesetRulesFromInput(input)
	bypassActors, _ := s.rulesetBypassActorsFromInput(input)

	body := &store.Ruleset{
		Name:         name,
		Enforcement:  strings.ToLower(enforcement),
		Target:       strings.ToLower(target),
		Conditions:   conditions,
		Rules:        rules,
		BypassActors: bypassActors,
	}

	sourceID, _ := gqlInputString(input, "sourceId")
	if repo := store.FindRepoByNodeID(s.store, sourceID); repo != nil {
		created := s.store.CreateRuleset(repo, body)
		if created == nil {
			return nil, gqlMissingNodeType("Repository")
		}
		return map[string]interface{}{"ruleset": optionalObject(s.rulesetToGraphQL(created, repo))}, nil
	}
	if org := s.orgByNodeID(sourceID); org != nil {
		created := s.store.CreateOrgRuleset(org.ID, body.Name, body.Target, body.Enforcement, body.Conditions, body.Rules, body.BypassActors)
		if created == nil {
			return nil, gqlMissingNodeType("Organization")
		}
		return map[string]interface{}{"ruleset": optionalObject(s.orgRulesetSource(created, org))}, nil
	}
	if e := store.FindEnterpriseByNodeID(s.store, sourceID); e != nil {
		return nil, fmt.Errorf("enterprise-scoped rulesets are managed through the enterprise rulesets REST API")
	}
	return nil, gqlMissingNode("RuleSource", sourceID)
}

func (s *Resolver) resolveUpdateRepositoryRuleset(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "repositoryRulesetId")
	existing := store.FindRulesetByNodeID(s.store, nodeID)
	if existing == nil {
		return nil, gqlMissingNode("RepositoryRuleset", nodeID)
	}
	if existing.Enterprise != "" {
		return nil, fmt.Errorf("enterprise-scoped rulesets are managed through the enterprise rulesets REST API")
	}
	rulesetID := existing.ID
	actor := s.ghUserFromContext(p.Context)

	name, hasName := gqlInputString(input, "name")
	enforcement, hasEnforcement := gqlInputString(input, "enforcement")
	target, hasTarget := gqlInputString(input, "target")
	conditions, hasConditions := rulesetConditionsFromInput(input)
	rules, hasRules := rulesetRulesFromInput(input)
	bypassActors, hasBypassActors := s.rulesetBypassActorsFromInput(input)

	apply := func(rs *store.Ruleset) {
		if hasName && strings.TrimSpace(name) != "" {
			rs.Name = name
		}
		if hasEnforcement {
			rs.Enforcement = strings.ToLower(enforcement)
		}
		if hasTarget {
			rs.Target = strings.ToLower(target)
		}
		if hasConditions {
			rs.Conditions = conditions
		}
		if hasRules {
			rs.Rules = rules
		}
		if hasBypassActors {
			rs.BypassActors = bypassActors
		}
	}

	if existing.RepoID != 0 {
		repo := s.store.GetRepoByID(existing.RepoID)
		if repo == nil {
			return nil, gqlMissingNode("RepositoryRuleset", nodeID)
		}
		// UpdateRuleset merges a sparse updates record; build it from the
		// present members so an absent member leaves the field alone — the
		// same merge PUT /repos/{owner}/{repo}/rulesets/{id} performs.
		updates := &store.Ruleset{}
		apply(updates)
		updated := s.store.UpdateRuleset(repo, existing, updates, actor.ID)
		if updated == nil {
			return nil, gqlMissingNode("RepositoryRuleset", nodeID)
		}
		return map[string]interface{}{"ruleset": optionalObject(s.rulesetToGraphQL(updated, repo))}, nil
	}

	org := s.store.GetOrgByID(existing.OrgID)
	if org == nil {
		return nil, gqlMissingNode("RepositoryRuleset", nodeID)
	}
	if !s.store.UpdateOrgRuleset(rulesetID, actor.ID, apply) {
		return nil, gqlMissingNode("RepositoryRuleset", nodeID)
	}
	updated := s.store.GetRuleset(rulesetID)
	if updated == nil {
		return nil, gqlMissingNode("RepositoryRuleset", nodeID)
	}
	return map[string]interface{}{"ruleset": optionalObject(s.orgRulesetSource(updated, org))}, nil
}

func (s *Resolver) resolveDeleteRepositoryRuleset(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "repositoryRulesetId")
	existing := store.FindRulesetByNodeID(s.store, nodeID)
	if existing == nil {
		return nil, gqlMissingNode("RepositoryRuleset", nodeID)
	}
	if existing.Enterprise != "" {
		return nil, fmt.Errorf("enterprise-scoped rulesets are managed through the enterprise rulesets REST API")
	}
	if !s.store.DeleteRuleset(existing.ID) {
		return nil, gqlMissingNode("RepositoryRuleset", nodeID)
	}
	return map[string]interface{}{}, nil
}

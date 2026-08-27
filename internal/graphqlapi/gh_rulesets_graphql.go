package graphqlapi

import (
	"fmt"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// Repository.rulesets — the GraphQL view of the rulesets the REST surface
// serves. `gh ruleset list` reads rulesets only through GraphQL.

// addRulesetFieldsToSchema installs Repository.rulesets and Repository.ruleset.
// Runs after the repository and organization types exist (a ruleset's source is one).
func (s *Resolver) addRulesetFieldsToSchema(repoType, orgType *graphql.Object) {
	dateTime := s.graphQLStringScalar("DateTime")
	targetEnum := s.sharedEnum("RepositoryRulesetTarget", "BRANCH", "PUSH", "REPOSITORY", "TAG")
	enforcementEnum := s.sharedEnum("RuleEnforcement", "ACTIVE", "DISABLED", "EVALUATE")

	ruleSourceUnion := graphql.NewUnion(graphql.UnionConfig{
		Name:  "RuleSource",
		Types: []*graphql.Object{orgType, repoType},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			if source, ok := p.Value.(map[string]interface{}); ok {
				if name, _ := source["__typename"].(string); name == "Organization" {
					return orgType
				}
			}
			return repoType
		},
	})

	ruleType := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryRule",
		Fields: graphql.Fields{
			"id":   &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"type": &graphql.Field{Type: graphql.NewNonNull(s.sharedRepositoryRuleTypeEnum())},
		},
	})
	ruleConnection := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryRuleConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(ruleType)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})

	rulesetType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "RepositoryRuleset",
		Interfaces: []*graphql.Interface{s.graphqlTypes.node},
		Fields: graphql.Fields{
			"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"databaseId":  &graphql.Field{Type: graphql.Int},
			"name":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"target":      &graphql.Field{Type: targetEnum},
			"enforcement": &graphql.Field{Type: graphql.NewNonNull(enforcementEnum)},
			"createdAt":   &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":   &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"source":      &graphql.Field{Type: graphql.NewNonNull(ruleSourceUnion)},
			"rules": &graphql.Field{
				Type: ruleConnection,
				Args: graphql.FieldConfigArgument{
					"after":  &graphql.ArgumentConfig{Type: graphql.String},
					"before": &graphql.ArgumentConfig{Type: graphql.String},
					"first":  &graphql.ArgumentConfig{Type: graphql.Int},
					"last":   &graphql.ArgumentConfig{Type: graphql.Int},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					source, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return repaginateConnection(source["rules"], p.Args), nil
				},
			},
		},
	})

	// Wire the rule/ruleset detail surface, closing the rule↔ruleset cycle with
	// AddFieldConfig — runs once all three objects exist.
	s.installRuleDetailTypes(ruleType, ruleConnection, rulesetType)

	// Ref.rules reuses this one RepositoryRuleConnection; stash it so the residue
	// installer reaches it by name rather than minting a duplicate.
	s.stashNamedObject(ruleConnection)

	rulesetEdge := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryRulesetEdge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: rulesetType},
		},
	})
	rulesetConnection := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryRulesetConnection",
		Fields: graphql.Fields{
			"edges":      &graphql.Field{Type: graphql.NewList(rulesetEdge)},
			"nodes":      &graphql.Field{Type: graphql.NewList(rulesetType)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})

	// Record these for the account surface so Organization.ruleset(s) reuses them
	// rather than re-minting (graphql-go rejects two types with one name).
	accountTypes := s.accountSurfaceRegistry()
	accountTypes.ruleset = rulesetType
	accountTypes.rulesetConnection = rulesetConnection

	repoType.AddFieldConfig("rulesets", &graphql.Field{
		Type: rulesetConnection,
		Args: graphql.FieldConfigArgument{
			"after":          &graphql.ArgumentConfig{Type: graphql.String},
			"before":         &graphql.ArgumentConfig{Type: graphql.String},
			"first":          &graphql.ArgumentConfig{Type: graphql.Int},
			"includeParents": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: true},
			"last":           &graphql.ArgumentConfig{Type: graphql.Int},
			"targets":        &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(targetEnum))},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromGraphQLSource(p.Source)
			if err != nil {
				return nil, err
			}
			if repo == nil || !s.viewerCanReadRepo(p.Context, repo) {
				return nil, nil
			}
			includeParents := true
			if value, ok := p.Args["includeParents"].(bool); ok {
				includeParents = value
			}
			wanted := map[string]bool{}
			if targets, ok := p.Args["targets"].([]interface{}); ok {
				for _, target := range targets {
					if name, ok := target.(string); ok {
						wanted[name] = true
					}
				}
			}
			var nodes []map[string]interface{}
			for _, ruleset := range s.store.ListRulesetsForRepository(repo, includeParents) {
				node := s.rulesetToGraphQL(ruleset, repo)
				if len(wanted) > 0 && !wanted[node["target"].(string)] {
					continue
				}
				nodes = append(nodes, node)
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})

	repoType.AddFieldConfig("ruleset", &graphql.Field{
		Type: rulesetType,
		Args: graphql.FieldConfigArgument{
			"databaseId":     &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)},
			"includeParents": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: true},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromGraphQLSource(p.Source)
			if err != nil {
				return nil, err
			}
			if repo == nil || !s.viewerCanReadRepo(p.Context, repo) {
				return nil, nil
			}
			includeParents := true
			if value, ok := p.Args["includeParents"].(bool); ok {
				includeParents = value
			}
			databaseID, _ := intArg(p.Args, "databaseId")
			for _, ruleset := range s.store.ListRulesetsForRepository(repo, includeParents) {
				if ruleset.ID == databaseID {
					return s.rulesetToGraphQL(ruleset, repo), nil
				}
			}
			return nil, nil
		},
	})
}

// repoFromGraphQLSource resolves the stored repository from a Repository source map.
func (s *Resolver) repoFromGraphQLSource(source interface{}) (*store.Repo, error) {
	fields, ok := source.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("resolve source: unexpected type %T", source)
	}
	fullName, _ := fields["nameWithOwner"].(string)
	owner, name, _ := strings.Cut(fullName, "/")
	return s.store.GetRepo(owner, name), nil
}

// sharedRepositoryRuleTypeEnum is RepositoryRule.type, limited to the rule types
// bleephub's ruleset store records (a subset of GitHub's values).
func (s *Resolver) sharedRepositoryRuleTypeEnum() *graphql.Enum {
	return s.sharedEnum("RepositoryRuleType",
		"CREATION", "UPDATE", "DELETION", "REQUIRED_LINEAR_HISTORY", "REQUIRED_DEPLOYMENTS",
		"REQUIRED_SIGNATURES", "PULL_REQUEST", "REQUIRED_STATUS_CHECKS", "NON_FAST_FORWARD",
		"COMMIT_MESSAGE_PATTERN", "COMMIT_AUTHOR_EMAIL_PATTERN", "COMMITTER_EMAIL_PATTERN",
		"BRANCH_NAME_PATTERN", "TAG_NAME_PATTERN", "LOCK_BRANCH",
		"FILE_EXTENSION_RESTRICTION", "FILE_PATH_RESTRICTION",
		"MAX_FILE_PATH_LENGTH", "MAX_FILE_SIZE", "WORKFLOWS")
}

// rulesetToGraphQL renders one stored ruleset as its GraphQL source map. The
// source member names the account it is configured on: the repository, or the
// organization for an inherited one.
func (s *Resolver) rulesetToGraphQL(ruleset *store.Ruleset, repo *store.Repo) map[string]interface{} {
	rules := make([]map[string]interface{}, 0, len(ruleset.Rules))
	for i, rule := range ruleset.Rules {
		rules = append(rules, map[string]interface{}{
			"id":   fmt.Sprintf("RR_%d_%d", ruleset.ID, i),
			"type": strings.ToUpper(rule.Type),
			// Private keys backing RepositoryRule.parameters.
			"_type":       rule.Type,
			"_parameters": rule.Parameters,
		})
	}
	source := repoToGraphQL(s.store, repo)
	source["__typename"] = "Repository"
	if ruleset.RepoID != repo.ID && ruleset.OrgID != 0 {
		if org := s.store.GetOrgByID(ruleset.OrgID); org != nil {
			source = orgToGraphQL(org)
			source["__typename"] = "Organization"
		}
	}
	result := map[string]interface{}{
		"nodeID":      ruleset.NodeID,
		"id":          ruleset.NodeID,
		"databaseId":  ruleset.ID,
		"name":        ruleset.Name,
		"target":      strings.ToUpper(ruleset.Target),
		"enforcement": strings.ToUpper(ruleset.Enforcement),
		"createdAt":   ruleset.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":   ruleset.UpdatedAt.UTC().Format(time.RFC3339),
		"source":      source,
		"rules": map[string]interface{}{
			"nodes":      rules,
			"totalCount": len(rules),
			"pageInfo": map[string]interface{}{
				"hasNextPage": false, "hasPreviousPage": false,
				"startCursor": nil, "endCursor": nil,
			},
		},
		// Private keys backing RepositoryRuleset.conditions and .bypassActors.
		"_conditions":   rulesetConditionsSource(ruleset),
		"_bypassActors": append([]store.RulesetBypassActor(nil), ruleset.BypassActors...),
	}
	// Back-reference each rule to its ruleset for RepositoryRule.repositoryRuleset;
	// the cycle is bounded by query depth and the typed-nil audit's visited set.
	for _, rule := range rules {
		rule["_ruleset"] = result
	}
	return result
}

// rulesetConditionsSource renders a ruleset's conditions. Only ref_name is
// modeled; include/exclude are non-nil arrays so the non-null list fields never
// resolve to null.
func rulesetConditionsSource(ruleset *store.Ruleset) map[string]interface{} {
	include := ruleset.Conditions.RefName.Include
	if include == nil {
		include = []string{}
	}
	exclude := ruleset.Conditions.RefName.Exclude
	if exclude == nil {
		exclude = []string{}
	}
	return map[string]interface{}{
		"refName": map[string]interface{}{
			"include": include,
			"exclude": exclude,
		},
	}
}

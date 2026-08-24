package graphqlapi

import (
	"fmt"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// Repository.rulesets — the GraphQL view of the rulesets the REST surface
// already serves. `gh ruleset list` reads rulesets only through GraphQL, so
// without this field the command fails outright with "Cannot query field
// \"rulesets\" on type \"Repository\"" no matter what REST holds.

// addRulesetFieldsToSchema installs Repository.rulesets and Repository.ruleset.
// It runs after the repository and organization types exist, because a
// ruleset's `source` is one of them.
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

	// Organization.ruleset / Organization.rulesets return the same two types,
	// so they are recorded for the account surface rather than re-minted
	// there (graphql-go rejects two types with one name).
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

// repoFromGraphQLSource resolves the stored repository a Repository field is
// being resolved on, from the source map the repository renderer produced.
func (s *Resolver) repoFromGraphQLSource(source interface{}) (*store.Repo, error) {
	fields, ok := source.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("resolve source: unexpected type %T", source)
	}
	fullName, _ := fields["nameWithOwner"].(string)
	owner, name, _ := strings.Cut(fullName, "/")
	return s.store.GetRepo(owner, name), nil
}

// sharedRepositoryRuleTypeEnum is the rule-type enum GitHub declares on
// RepositoryRule.type, limited to the rule types bleephub's ruleset store
// actually records. A subset of the official values is still an exact match
// for every value it does declare.
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
// `source` member names the account the ruleset is configured on — the
// repository for a repository ruleset, the organization for one inherited from
// the owning organization — which is how `gh ruleset list` labels each row.
func (s *Resolver) rulesetToGraphQL(ruleset *store.Ruleset, repo *store.Repo) map[string]interface{} {
	rules := make([]map[string]interface{}, 0, len(ruleset.Rules))
	for i, rule := range ruleset.Rules {
		rules = append(rules, map[string]interface{}{
			"id":   fmt.Sprintf("RR_%d_%d", ruleset.ID, i),
			"type": strings.ToUpper(rule.Type),
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
	return map[string]interface{}{
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
	}
}

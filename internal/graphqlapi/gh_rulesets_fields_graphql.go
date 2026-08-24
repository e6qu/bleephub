package graphqlapi

// The RepositoryRule / RepositoryRuleset detail surface: the rule parameter
// union, the ruleset conditions object and the bypass-actor connection. GitHub
// exposes a ruleset's rules with their fully typed parameters, the conditions
// that gate the ruleset and the actors permitted to bypass it; `gh ruleset
// view` and the web ruleset editor read all three through GraphQL.
//
// Every type here is a transcription of GitHub's SDL, rendered from the same
// stored ruleset the REST ruleset routes serve (store.Ruleset), so a ruleset
// authored over either surface reads back identically. Rule parameters are
// stored as the snake_case maps the REST bodies carry; the union renderer
// lowers each into the camelCase member GitHub's GraphQL declares, and returns
// a truthful null when a rule carries no parameters of a shape it can name.

import (
	"fmt"
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// gqlNonNullFieldListOf is `[T!]!` on an output object.
func gqlNonNullFieldListOf(t graphql.Output) *graphql.Field {
	return &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(t)))}
}

// installRuleDetailTypes wires RepositoryRule.parameters,
// RepositoryRule.repositoryRuleset, RepositoryRuleConnection.edges,
// RepositoryRuleset.conditions and RepositoryRuleset.bypassActors onto the
// three ruleset objects the read surface already created. It runs once, after
// those objects exist; the fields it adds reference types built here, and the
// cross-references between the rule and ruleset objects are closed with
// AddFieldConfig so the cycle is legal.
func (s *Resolver) installRuleDetailTypes(ruleType, ruleConnection, rulesetType *graphql.Object) {
	// --- RepositoryRuleConnection.edges -------------------------------------
	ruleEdge := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryRuleEdge",
		Fields: graphql.Fields{
			"cursor": gqlNonNull(graphql.String),
			"node":   gqlField(ruleType),
		},
	})
	ruleConnection.AddFieldConfig("edges", &graphql.Field{Type: graphql.NewList(ruleEdge)})

	// --- RepositoryRule.parameters (the RuleParameters union) ---------------
	parametersUnion := s.ruleParametersUnion()
	ruleType.AddFieldConfig("parameters", &graphql.Field{
		Type: parametersUnion,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, err := graphQLSourceMap(p.Source)
			if err != nil {
				return nil, err
			}
			ruleKind, _ := src["_type"].(string)
			params, _ := src["_parameters"].(map[string]interface{})
			return optionalObject(ruleParametersSource(ruleKind, params)), nil
		},
	})

	// --- RepositoryRule.repositoryRuleset -----------------------------------
	// Each rendered rule carries a back-reference to its ruleset source map
	// under the private "_ruleset" key (set in rulesetToGraphQL). The map is
	// self-referential, which the executor bounds by query depth and the
	// typed-nil source audit bounds by its visited-pointer set; the audit is
	// keyed by GraphQL field name, so it never follows the private key.
	ruleType.AddFieldConfig("repositoryRuleset", &graphql.Field{
		Type: rulesetType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, err := graphQLSourceMap(p.Source)
			if err != nil {
				return nil, err
			}
			ruleset, _ := src["_ruleset"].(map[string]interface{})
			return optionalObject(ruleset), nil
		},
	})

	// --- RepositoryRuleset.conditions ---------------------------------------
	conditionsType := s.repositoryRuleConditionsType()
	rulesetType.AddFieldConfig("conditions", &graphql.Field{
		Type: graphql.NewNonNull(conditionsType),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, err := graphQLSourceMap(p.Source)
			if err != nil {
				return nil, err
			}
			// _conditions is always a non-nil map (rulesetToGraphQL builds it),
			// so the non-null field never resolves to null.
			return src["_conditions"], nil
		},
	})

	// --- RepositoryRuleset.bypassActors -------------------------------------
	bypassConnection := s.repositoryRulesetBypassActorConnectionType()
	rulesetType.AddFieldConfig("bypassActors", &graphql.Field{
		Type: bypassConnection,
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, err := graphQLSourceMap(p.Source)
			if err != nil {
				return nil, err
			}
			actors, _ := src["_bypassActors"].([]store.RulesetBypassActor)
			rulesetNodeID, _ := src["nodeID"].(string)
			items := make([]gqlConnItem, 0, len(actors))
			for i := range actors {
				node := bypassActorSource(rulesetNodeID, i, actors[i])
				identity, _ := node["id"].(string)
				items = append(items, gqlConnItem{
					identity: identity,
					render:   func() map[string]interface{} { return node },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})
}

// repositoryRuleConditionsType is RepositoryRuleConditions. bleephub models the
// ref_name condition; the organization/repository property and id/name
// conditions are declared by GitHub but not yet stored, so those members are
// omitted rather than rendered empty (an absent nullable member is truthful,
// an invented one is not).
func (s *Resolver) repositoryRuleConditionsType() *graphql.Object {
	refNameTarget := graphql.NewObject(graphql.ObjectConfig{
		Name: "RefNameConditionTarget",
		Fields: graphql.Fields{
			"exclude": gqlNonNullFieldListOf(graphql.String),
			"include": gqlNonNullFieldListOf(graphql.String),
		},
	})
	return graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryRuleConditions",
		Fields: graphql.Fields{
			"refName": gqlField(refNameTarget),
		},
	})
}

// repositoryRulesetBypassActorConnectionType is the Relay connection over
// RepositoryRulesetBypassActor.
func (s *Resolver) repositoryRulesetBypassActorConnectionType() *graphql.Object {
	actorType := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryRulesetBypassActor",
		Fields: graphql.Fields{
			"id":                       gqlNonNull(graphql.ID),
			"bypassMode":               gqlField(s.sharedEnum("RepositoryRulesetBypassActorBypassMode", "ALWAYS", "EXEMPT", "PULL_REQUEST")),
			"deployKey":                gqlNonNull(graphql.Boolean),
			"enterpriseOwner":          gqlNonNull(graphql.Boolean),
			"enterpriseRole":           gqlNonNull(graphql.Boolean),
			"organizationAdmin":        gqlNonNull(graphql.Boolean),
			"repositoryRoleDatabaseId": gqlField(graphql.Int),
			"repositoryRoleName":       gqlField(graphql.String),
		},
	})
	edge := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryRulesetBypassActorEdge",
		Fields: graphql.Fields{
			"cursor": gqlNonNull(graphql.String),
			"node":   gqlField(actorType),
		},
	})
	return graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryRulesetBypassActorConnection",
		Fields: graphql.Fields{
			"edges":      &graphql.Field{Type: graphql.NewList(edge)},
			"nodes":      &graphql.Field{Type: graphql.NewList(actorType)},
			"pageInfo":   gqlNonNull(s.gqlPageInfoType()),
			"totalCount": gqlNonNull(graphql.Int),
		},
	})
}

// bypassActorSource renders one stored bypass actor as its GraphQL source map.
// The boolean discriminators are derived from the stored actor type, and the
// repository-role id is surfaced only for a repository-role actor.
func bypassActorSource(rulesetNodeID string, index int, actor store.RulesetBypassActor) map[string]interface{} {
	node := map[string]interface{}{
		"id":                fmt.Sprintf("%s:bypass:%d", rulesetNodeID, index),
		"deployKey":         actor.ActorType == "DeployKey",
		"enterpriseOwner":   actor.ActorType == "EnterpriseOwner",
		"enterpriseRole":    actor.ActorType == "EnterpriseRole",
		"organizationAdmin": actor.ActorType == "OrganizationAdmin",
	}
	if mode := strings.ToUpper(actor.BypassMode); mode == "ALWAYS" || mode == "EXEMPT" || mode == "PULL_REQUEST" {
		node["bypassMode"] = mode
	}
	if actor.ActorType == "RepositoryRole" {
		node["repositoryRoleDatabaseId"] = actor.ActorID
	}
	return node
}

// ruleParametersUnion is RuleParameters: the discriminated union of every
// per-rule parameter object GitHub declares. bleephub renders each member from
// the stored snake_case parameter map.
func (s *Resolver) ruleParametersUnion() *graphql.Union {
	patternFields := func() graphql.Fields {
		return graphql.Fields{
			"name":     gqlField(graphql.String),
			"negate":   gqlNonNull(graphql.Boolean),
			"operator": gqlNonNull(graphql.String),
			"pattern":  gqlNonNull(graphql.String),
		}
	}
	patternType := func(name string) *graphql.Object {
		return graphql.NewObject(graphql.ObjectConfig{Name: name, Fields: patternFields()})
	}

	codeScanningTool := graphql.NewObject(graphql.ObjectConfig{
		Name: "CodeScanningTool",
		Fields: graphql.Fields{
			"alertsThreshold":         gqlNonNull(graphql.String),
			"securityAlertsThreshold": gqlNonNull(graphql.String),
			"tool":                    gqlNonNull(graphql.String),
		},
	})
	dismissalRestriction := graphql.NewObject(graphql.ObjectConfig{
		Name: "DismissalRestriction",
		Fields: graphql.Fields{
			"allowedActors": gqlFieldListOf(graphql.ID),
			"enabled":       gqlNonNull(graphql.Boolean),
		},
	})
	requiredReviewer := graphql.NewObject(graphql.ObjectConfig{
		Name: "RequiredReviewerConfiguration",
		Fields: graphql.Fields{
			"filePatterns":     gqlNonNullFieldListOf(graphql.String),
			"minimumApprovals": gqlNonNull(graphql.Int),
			"reviewerId":       gqlNonNull(graphql.ID),
		},
	})
	statusCheck := graphql.NewObject(graphql.ObjectConfig{
		Name: "StatusCheckConfiguration",
		Fields: graphql.Fields{
			"context":       gqlNonNull(graphql.String),
			"integrationId": gqlField(graphql.Int),
		},
	})
	workflowFile := graphql.NewObject(graphql.ObjectConfig{
		Name: "WorkflowFileReference",
		Fields: graphql.Fields{
			"path":         gqlNonNull(graphql.String),
			"ref":          gqlField(graphql.String),
			"repositoryId": gqlNonNull(graphql.Int),
			"sha":          gqlField(graphql.String),
		},
	})

	members := map[string]*graphql.Object{
		"BranchNamePatternParameters":        patternType("BranchNamePatternParameters"),
		"CommitAuthorEmailPatternParameters": patternType("CommitAuthorEmailPatternParameters"),
		"CommitMessagePatternParameters":     patternType("CommitMessagePatternParameters"),
		"CommitterEmailPatternParameters":    patternType("CommitterEmailPatternParameters"),
		"TagNamePatternParameters":           patternType("TagNamePatternParameters"),
		"CodeScanningParameters": graphql.NewObject(graphql.ObjectConfig{
			Name:   "CodeScanningParameters",
			Fields: graphql.Fields{"codeScanningTools": gqlNonNullFieldListOf(codeScanningTool)},
		}),
		"CopilotCodeReviewParameters": graphql.NewObject(graphql.ObjectConfig{
			Name: "CopilotCodeReviewParameters",
			Fields: graphql.Fields{
				"reviewDraftPullRequests": gqlNonNull(graphql.Boolean),
				"reviewOnPush":            gqlNonNull(graphql.Boolean),
			},
		}),
		"FileExtensionRestrictionParameters": graphql.NewObject(graphql.ObjectConfig{
			Name:   "FileExtensionRestrictionParameters",
			Fields: graphql.Fields{"restrictedFileExtensions": gqlNonNullFieldListOf(graphql.String)},
		}),
		"FilePathRestrictionParameters": graphql.NewObject(graphql.ObjectConfig{
			Name:   "FilePathRestrictionParameters",
			Fields: graphql.Fields{"restrictedFilePaths": gqlNonNullFieldListOf(graphql.String)},
		}),
		"MaxFilePathLengthParameters": graphql.NewObject(graphql.ObjectConfig{
			Name:   "MaxFilePathLengthParameters",
			Fields: graphql.Fields{"maxFilePathLength": gqlNonNull(graphql.Int)},
		}),
		"MaxFileSizeParameters": graphql.NewObject(graphql.ObjectConfig{
			Name:   "MaxFileSizeParameters",
			Fields: graphql.Fields{"maxFileSize": gqlNonNull(graphql.Int)},
		}),
		"MergeQueueParameters": graphql.NewObject(graphql.ObjectConfig{
			Name: "MergeQueueParameters",
			Fields: graphql.Fields{
				"checkResponseTimeoutMinutes":  gqlNonNull(graphql.Int),
				"groupingStrategy":             gqlNonNull(s.sharedEnum("MergeQueueGroupingStrategy", "ALLGREEN", "HEADGREEN")),
				"maxEntriesToBuild":            gqlNonNull(graphql.Int),
				"maxEntriesToMerge":            gqlNonNull(graphql.Int),
				"mergeMethod":                  gqlNonNull(s.sharedEnum("MergeQueueMergeMethod", "MERGE", "REBASE", "SQUASH")),
				"minEntriesToMerge":            gqlNonNull(graphql.Int),
				"minEntriesToMergeWaitMinutes": gqlNonNull(graphql.Int),
			},
		}),
		"PullRequestParameters": graphql.NewObject(graphql.ObjectConfig{
			Name: "PullRequestParameters",
			Fields: graphql.Fields{
				"allowedMergeMethods":            gqlFieldListOf(s.sharedEnum("PullRequestAllowedMergeMethods", "MERGE", "REBASE", "SQUASH")),
				"dismissStaleReviewsOnPush":      gqlNonNull(graphql.Boolean),
				"dismissalRestriction":           gqlField(dismissalRestriction),
				"requireCodeOwnerReview":         gqlNonNull(graphql.Boolean),
				"requireLastPushApproval":        gqlNonNull(graphql.Boolean),
				"requiredApprovingReviewCount":   gqlNonNull(graphql.Int),
				"requiredReviewThreadResolution": gqlNonNull(graphql.Boolean),
				"requiredReviewers":              gqlFieldListOf(requiredReviewer),
			},
		}),
		"RequiredDeploymentsParameters": graphql.NewObject(graphql.ObjectConfig{
			Name:   "RequiredDeploymentsParameters",
			Fields: graphql.Fields{"requiredDeploymentEnvironments": gqlNonNullFieldListOf(graphql.String)},
		}),
		"RequiredStatusChecksParameters": graphql.NewObject(graphql.ObjectConfig{
			Name: "RequiredStatusChecksParameters",
			Fields: graphql.Fields{
				"doNotEnforceOnCreate":             gqlNonNull(graphql.Boolean),
				"requiredStatusChecks":             gqlNonNullFieldListOf(statusCheck),
				"strictRequiredStatusChecksPolicy": gqlNonNull(graphql.Boolean),
			},
		}),
		"UpdateParameters": graphql.NewObject(graphql.ObjectConfig{
			Name:   "UpdateParameters",
			Fields: graphql.Fields{"updateAllowsFetchAndMerge": gqlNonNull(graphql.Boolean)},
		}),
		"WorkflowsParameters": graphql.NewObject(graphql.ObjectConfig{
			Name: "WorkflowsParameters",
			Fields: graphql.Fields{
				"doNotEnforceOnCreate": gqlNonNull(graphql.Boolean),
				"workflows":            gqlNonNullFieldListOf(workflowFile),
			},
		}),
	}

	types := make([]*graphql.Object, 0, len(members))
	for _, member := range members {
		types = append(types, member)
	}
	return graphql.NewUnion(graphql.UnionConfig{
		Name:  "RuleParameters",
		Types: types,
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			if source, ok := p.Value.(map[string]interface{}); ok {
				if name, _ := source["__typename"].(string); name != "" {
					if member := members[name]; member != nil {
						return member
					}
				}
			}
			return nil
		},
	})
}

// ruleParametersSource lowers a stored rule's snake_case parameter map into the
// camelCase RuleParameters union member GitHub declares for its type. A rule
// that carries no parameters — or whose type has no parameter object — yields
// nil, which the executor renders as a truthful null RepositoryRule.parameters.
func ruleParametersSource(ruleType string, params map[string]interface{}) map[string]interface{} {
	if len(params) == 0 {
		return nil
	}
	switch ruleType {
	case "branch_name_pattern":
		return patternParametersSource("BranchNamePatternParameters", params)
	case "commit_author_email_pattern":
		return patternParametersSource("CommitAuthorEmailPatternParameters", params)
	case "commit_message_pattern":
		return patternParametersSource("CommitMessagePatternParameters", params)
	case "committer_email_pattern":
		return patternParametersSource("CommitterEmailPatternParameters", params)
	case "tag_name_pattern":
		return patternParametersSource("TagNamePatternParameters", params)
	case "code_scanning":
		tools := make([]map[string]interface{}, 0)
		for _, tool := range rpMaps(params, "code_scanning_tools") {
			tools = append(tools, map[string]interface{}{
				"alertsThreshold":         rpString(tool, "alerts_threshold"),
				"securityAlertsThreshold": rpString(tool, "security_alerts_threshold"),
				"tool":                    rpString(tool, "tool"),
			})
		}
		return map[string]interface{}{"__typename": "CodeScanningParameters", "codeScanningTools": tools}
	case "copilot_code_review":
		return map[string]interface{}{
			"__typename":              "CopilotCodeReviewParameters",
			"reviewDraftPullRequests": rpBool(params, "review_draft_pull_requests"),
			"reviewOnPush":            rpBool(params, "review_on_push"),
		}
	case "file_extension_restriction":
		return map[string]interface{}{
			"__typename":               "FileExtensionRestrictionParameters",
			"restrictedFileExtensions": rpStringSlice(params, "restricted_file_extensions"),
		}
	case "file_path_restriction":
		return map[string]interface{}{
			"__typename":          "FilePathRestrictionParameters",
			"restrictedFilePaths": rpStringSlice(params, "restricted_file_paths"),
		}
	case "max_file_path_length":
		return map[string]interface{}{
			"__typename":        "MaxFilePathLengthParameters",
			"maxFilePathLength": rpInt(params, "max_file_path_length"),
		}
	case "max_file_size":
		return map[string]interface{}{
			"__typename":  "MaxFileSizeParameters",
			"maxFileSize": rpInt(params, "max_file_size"),
		}
	case "merge_queue":
		grouping := strings.ToUpper(rpString(params, "grouping_strategy"))
		method := strings.ToUpper(rpString(params, "merge_method"))
		if grouping != "ALLGREEN" && grouping != "HEADGREEN" {
			return nil
		}
		if method != "MERGE" && method != "REBASE" && method != "SQUASH" {
			return nil
		}
		return map[string]interface{}{
			"__typename":                   "MergeQueueParameters",
			"checkResponseTimeoutMinutes":  rpInt(params, "check_response_timeout_minutes"),
			"groupingStrategy":             grouping,
			"maxEntriesToBuild":            rpInt(params, "max_entries_to_build"),
			"maxEntriesToMerge":            rpInt(params, "max_entries_to_merge"),
			"mergeMethod":                  method,
			"minEntriesToMerge":            rpInt(params, "min_entries_to_merge"),
			"minEntriesToMergeWaitMinutes": rpInt(params, "min_entries_to_merge_wait_minutes"),
		}
	case "pull_request":
		return pullRequestParametersSource(params)
	case "required_deployments":
		return map[string]interface{}{
			"__typename":                     "RequiredDeploymentsParameters",
			"requiredDeploymentEnvironments": rpStringSlice(params, "required_deployment_environments"),
		}
	case "required_status_checks":
		checks := make([]map[string]interface{}, 0)
		for _, check := range rpMaps(params, "required_status_checks") {
			entry := map[string]interface{}{"context": rpString(check, "context")}
			if _, present := check["integration_id"]; present {
				entry["integrationId"] = rpInt(check, "integration_id")
			}
			checks = append(checks, entry)
		}
		return map[string]interface{}{
			"__typename":                       "RequiredStatusChecksParameters",
			"doNotEnforceOnCreate":             rpBool(params, "do_not_enforce_on_create"),
			"requiredStatusChecks":             checks,
			"strictRequiredStatusChecksPolicy": rpBool(params, "strict_required_status_checks_policy"),
		}
	case "update":
		return map[string]interface{}{
			"__typename":                "UpdateParameters",
			"updateAllowsFetchAndMerge": rpBool(params, "update_allows_fetch_and_merge"),
		}
	case "workflows":
		workflows := make([]map[string]interface{}, 0)
		for _, workflow := range rpMaps(params, "workflows") {
			entry := map[string]interface{}{
				"path":         rpString(workflow, "path"),
				"repositoryId": rpInt(workflow, "repository_id"),
			}
			if ref, ok := workflow["ref"].(string); ok {
				entry["ref"] = ref
			}
			if sha, ok := workflow["sha"].(string); ok {
				entry["sha"] = sha
			}
			workflows = append(workflows, entry)
		}
		return map[string]interface{}{
			"__typename":           "WorkflowsParameters",
			"doNotEnforceOnCreate": rpBool(params, "do_not_enforce_on_create"),
			"workflows":            workflows,
		}
	default:
		return nil
	}
}

func patternParametersSource(typename string, params map[string]interface{}) map[string]interface{} {
	source := map[string]interface{}{
		"__typename": typename,
		"negate":     rpBool(params, "negate"),
		"operator":   rpString(params, "operator"),
		"pattern":    rpString(params, "pattern"),
	}
	if name, ok := params["name"].(string); ok {
		source["name"] = name
	}
	return source
}

func pullRequestParametersSource(params map[string]interface{}) map[string]interface{} {
	source := map[string]interface{}{
		"__typename":                     "PullRequestParameters",
		"dismissStaleReviewsOnPush":      rpBool(params, "dismiss_stale_reviews_on_push"),
		"requireCodeOwnerReview":         rpBool(params, "require_code_owner_review"),
		"requireLastPushApproval":        rpBool(params, "require_last_push_approval"),
		"requiredApprovingReviewCount":   rpInt(params, "required_approving_review_count"),
		"requiredReviewThreadResolution": rpBool(params, "required_review_thread_resolution"),
	}
	methods := make([]string, 0)
	for _, method := range rpStringSlice(params, "allowed_merge_methods") {
		if upper := strings.ToUpper(method); upper == "MERGE" || upper == "REBASE" || upper == "SQUASH" {
			methods = append(methods, upper)
		}
	}
	if len(methods) > 0 {
		source["allowedMergeMethods"] = methods
	}
	if restriction, ok := params["dismissal_restriction"].(map[string]interface{}); ok {
		rendered := map[string]interface{}{"enabled": rpBool(restriction, "enabled")}
		if actors := rpStringSlice(restriction, "allowed_actors"); len(actors) > 0 {
			rendered["allowedActors"] = actors
		}
		source["dismissalRestriction"] = rendered
	}
	if reviewers := rpMaps(params, "required_reviewers"); len(reviewers) > 0 {
		out := make([]map[string]interface{}, 0, len(reviewers))
		for _, reviewer := range reviewers {
			out = append(out, map[string]interface{}{
				"filePatterns":     rpStringSlice(reviewer, "file_patterns"),
				"minimumApprovals": rpInt(reviewer, "minimum_approvals"),
				"reviewerId":       rpString(reviewer, "reviewer_id"),
			})
		}
		source["requiredReviewers"] = out
	}
	return source
}

// --- stored-parameter accessors ---------------------------------------------
//
// Rule parameters round-trip through JSON, so numbers arrive as float64 and
// every container is []interface{}/map[string]interface{}. These readers hand
// back the concrete Go types the source maps need, with zero values for absent
// keys so a non-null scalar field never resolves to null.

func rpString(m map[string]interface{}, key string) string {
	value, _ := m[key].(string)
	return value
}

func rpBool(m map[string]interface{}, key string) bool {
	value, _ := m[key].(bool)
	return value
}

func rpInt(m map[string]interface{}, key string) int {
	switch n := m[key].(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

func rpStringSlice(m map[string]interface{}, key string) []string {
	out := []string{}
	switch values := m[key].(type) {
	case []string:
		out = append(out, values...)
	case []interface{}:
		for _, value := range values {
			if s, ok := value.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func rpMaps(m map[string]interface{}, key string) []map[string]interface{} {
	out := []map[string]interface{}{}
	switch values := m[key].(type) {
	case []map[string]interface{}:
		out = append(out, values...)
	case []interface{}:
		for _, value := range values {
			if entry, ok := value.(map[string]interface{}); ok {
				out = append(out, entry)
			}
		}
	}
	return out
}

package graphqlapi

// The branch-protection GraphQL family: the complete BranchProtectionRule
// object (the two-field shell it replaces answered only what `gh pr status`
// asked), Repository.branchProtectionRules, and the three write mutations —
// createBranchProtectionRule, updateBranchProtectionRule and
// deleteBranchProtectionRule.
//
// The mutations write the very records the REST protection routes serve:
// a pattern without wildcards is the exact-name rule PUT/DELETE
// /repos/{owner}/{repo}/branches/{branch}/protection reads and removes, and a
// wildcard pattern is a web-only fnmatch rule on the /ui-data
// branch-protection-patterns surface. Both feed the same enforcement
// chokepoint, so a rule created over GraphQL refuses a push exactly as one
// created over REST does. The GraphQL-only members REST's shape cannot hold
// (deployment requirements, force-push bypass actors, the creator) live in
// the store's BranchProtectionRuleExtras beside the rule.

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

func init() {
	for name, rule := range map[string]mutationRule{
		"createBranchProtectionRule": repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},
		"updateBranchProtectionRule": repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetBranchProtectionRule("branchProtectionRuleId")},
		"deleteBranchProtectionRule": repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetBranchProtectionRule("branchProtectionRuleId")},
	} {
		if _, exists := graphqlMutationAuthz[name]; exists {
			panic(fmt.Sprintf("graphql mutation %q already has a policy row", name))
		}
		graphqlMutationAuthz[name] = rule
	}
}

// mutationTargetBranchProtectionRule resolves a BranchProtectionRule global id
// to the repository whose rule it names.
func mutationTargetBranchProtectionRule(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("BranchProtectionRule", nodeID)}
		if repoID, _, ok := store.ParseBranchProtectionRuleNodeID(nodeID); ok {
			target.repo = s.store.GetRepoByID(repoID)
		}
		return target
	}
}

// --- types ------------------------------------------------------------------

// gqlAppType is the App object the allowance unions and
// RequiredStatusCheckDescription name (memoized). bleephub renders no App
// actors yet, but the type has to exist with GitHub's signature for the
// unions to be GitHub's unions.
func (s *Resolver) gqlAppType() *graphql.Object {
	dateTime := s.graphQLStringScalar("DateTime")
	return s.mutationObject("App", graphql.Fields{
		"id":          gqlNonNull(graphql.ID),
		"databaseId":  gqlField(graphql.Int),
		"clientId":    gqlField(graphql.String),
		"description": gqlField(graphql.String),
		"name":        gqlNonNull(graphql.String),
		"slug":        gqlNonNull(graphql.String),
		"createdAt":   gqlNonNull(dateTime),
		"updatedAt":   gqlNonNull(dateTime),
	})
}

// branchAllowanceActorUnion mints one of the three App | Team | User actor
// unions the allowance objects name; GitHub declares them as three distinct
// union types with identical membership.
func (s *Resolver) branchAllowanceActorUnion(name string) *graphql.Union {
	appType := s.gqlAppType()
	return s.mutationUnion(name,
		func() []*graphql.Object {
			return []*graphql.Object{appType, s.graphqlTypes.team, s.graphqlTypes.user}
		},
		func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			switch source["__typename"] {
			case "App":
				return appType
			case "Team":
				return s.graphqlTypes.team
			default:
				return s.graphqlTypes.user
			}
		})
}

// gqlAllowanceConnection mints one allowance object (actor,
// branchProtectionRule, id) plus its edge and connection.
func (s *Resolver) gqlAllowanceConnection(nodeName string, actorUnion *graphql.Union) *graphql.Object {
	node := s.mutationObject(nodeName, graphql.Fields{
		"id":                   gqlNonNull(graphql.ID),
		"actor":                gqlField(actorUnion),
		"branchProtectionRule": s.branchProtectionRuleBacklinkField(),
	})
	edge := s.mutationObject(nodeName+"Edge", graphql.Fields{
		"cursor": gqlNonNull(graphql.String),
		"node":   gqlField(node),
	})
	return s.mutationObject(nodeName+"Connection", graphql.Fields{
		"edges":      gqlField(graphql.NewList(edge)),
		"nodes":      gqlField(graphql.NewList(node)),
		"pageInfo":   gqlNonNull(s.gqlPageInfoType()),
		"totalCount": gqlNonNull(graphql.Int),
	})
}

// branchProtectionRuleBacklinkField resolves the owning rule from the hidden
// repo/pattern keys an allowance or conflict source carries, so the backlink
// is computed only when a client selects it.
func (s *Resolver) branchProtectionRuleBacklinkField() *graphql.Field {
	return &graphql.Field{
		Type: s.gqlBranchProtectionRuleType(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			source, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, _ := source["_repoID"].(int)
			pattern, _ := source["_pattern"].(string)
			repo := s.store.GetRepoByID(repoID)
			if repo == nil {
				return nil, nil
			}
			_, bp, _, found := s.lookupBranchProtectionRule(repo, pattern)
			if !found {
				return nil, nil
			}
			return s.branchProtectionRuleSource(repo, pattern, bp), nil
		},
	}
}

// gqlBranchProtectionRuleType returns the complete BranchProtectionRule
// object (memoized). Its fields are a thunk because the type is minted while
// the Ref type is being built and refers back to Ref through matchingRefs.
func (s *Resolver) gqlBranchProtectionRuleType() *graphql.Object {
	if s.graphqlTypes.branchProtectionRule != nil {
		return s.graphqlTypes.branchProtectionRule
	}
	s.graphqlTypes.branchProtectionRule = graphql.NewObject(graphql.ObjectConfig{
		Name:       "BranchProtectionRule",
		Interfaces: []*graphql.Interface{s.graphqlTypes.node},
		Fields:     graphql.FieldsThunk(func() graphql.Fields { return s.branchProtectionRuleFields() }),
	})
	return s.graphqlTypes.branchProtectionRule
}

func connectionPagingArgs() graphql.FieldConfigArgument {
	return graphql.FieldConfigArgument{
		"after":  &graphql.ArgumentConfig{Type: graphql.String},
		"before": &graphql.ArgumentConfig{Type: graphql.String},
		"first":  &graphql.ArgumentConfig{Type: graphql.Int},
		"last":   &graphql.ArgumentConfig{Type: graphql.Int},
	}
}

// repaginatedField serves a pre-rendered connection stored under the source
// key, re-paged by the field's own arguments.
func repaginatedField(connection *graphql.Object, key string) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(connection),
		Args: connectionPagingArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			source, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			return repaginateConnection(source[key], p.Args), nil
		},
	}
}

func (s *Resolver) branchProtectionRuleFields() graphql.Fields {
	appType := s.gqlAppType()
	requiredCheckType := s.mutationObject("RequiredStatusCheckDescription", graphql.Fields{
		"app":     gqlField(appType),
		"context": gqlNonNull(graphql.String),
	})
	conflictType := s.mutationObject("BranchProtectionRuleConflict", graphql.Fields{
		"branchProtectionRule":            s.branchProtectionRuleBacklinkField(),
		"conflictingBranchProtectionRule": gqlField(s.gqlBranchProtectionRuleType()),
		"ref":                             gqlField(s.gqlRefType()),
	})
	conflictEdge := s.mutationObject("BranchProtectionRuleConflictEdge", graphql.Fields{
		"cursor": gqlNonNull(graphql.String),
		"node":   gqlField(conflictType),
	})
	conflictConnection := s.mutationObject("BranchProtectionRuleConflictConnection", graphql.Fields{
		"edges":      gqlField(graphql.NewList(conflictEdge)),
		"nodes":      gqlField(graphql.NewList(conflictType)),
		"pageInfo":   gqlNonNull(s.gqlPageInfoType()),
		"totalCount": gqlNonNull(graphql.Int),
	})
	bypassForcePushConnection := s.gqlAllowanceConnection("BypassForcePushAllowance", s.branchAllowanceActorUnion("BranchActorAllowanceActor"))
	bypassPullRequestConnection := s.gqlAllowanceConnection("BypassPullRequestAllowance", s.branchAllowanceActorUnion("BranchActorAllowanceActor"))
	pushAllowanceConnection := s.gqlAllowanceConnection("PushAllowance", s.branchAllowanceActorUnion("PushAllowanceActor"))
	reviewDismissalConnection := s.gqlAllowanceConnection("ReviewDismissalAllowance", s.branchAllowanceActorUnion("ReviewDismissalAllowanceActor"))

	return graphql.Fields{
		"id":         gqlNonNull(graphql.ID),
		"databaseId": gqlField(graphql.Int),
		"pattern":    gqlNonNull(graphql.String),
		"creator":    gqlField(s.graphqlTypes.actor),
		"repository": &graphql.Field{
			Type: s.graphqlTypes.repository,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				repo := s.branchProtectionRuleSourceRepo(p.Source)
				if repo == nil {
					return nil, nil
				}
				return repoToGraphQL(s.store, repo), nil
			},
		},

		"allowsDeletions":                gqlNonNull(graphql.Boolean),
		"allowsForcePushes":              gqlNonNull(graphql.Boolean),
		"blocksCreations":                gqlNonNull(graphql.Boolean),
		"dismissesStaleReviews":          gqlNonNull(graphql.Boolean),
		"isAdminEnforced":                gqlNonNull(graphql.Boolean),
		"lockAllowsFetchAndMerge":        gqlNonNull(graphql.Boolean),
		"lockBranch":                     gqlNonNull(graphql.Boolean),
		"requireLastPushApproval":        gqlNonNull(graphql.Boolean),
		"requiresApprovingReviews":       gqlNonNull(graphql.Boolean),
		"requiresCodeOwnerReviews":       gqlNonNull(graphql.Boolean),
		"requiresCommitSignatures":       gqlNonNull(graphql.Boolean),
		"requiresConversationResolution": gqlNonNull(graphql.Boolean),
		"requiresDeployments":            gqlNonNull(graphql.Boolean),
		"requiresLinearHistory":          gqlNonNull(graphql.Boolean),
		"requiresStatusChecks":           gqlNonNull(graphql.Boolean),
		"requiresStrictStatusChecks":     gqlNonNull(graphql.Boolean),
		"restrictsPushes":                gqlNonNull(graphql.Boolean),
		"restrictsReviewDismissals":      gqlNonNull(graphql.Boolean),

		"requiredApprovingReviewCount":   gqlField(graphql.Int),
		"requiredDeploymentEnvironments": gqlField(graphql.NewList(graphql.String)),
		"requiredStatusCheckContexts":    gqlField(graphql.NewList(graphql.String)),
		"requiredStatusChecks":           gqlFieldListOf(requiredCheckType),

		"bypassForcePushAllowances":   repaginatedField(bypassForcePushConnection, "bypassForcePushAllowances"),
		"bypassPullRequestAllowances": repaginatedField(bypassPullRequestConnection, "bypassPullRequestAllowances"),
		"pushAllowances":              repaginatedField(pushAllowanceConnection, "pushAllowances"),
		"reviewDismissalAllowances":   repaginatedField(reviewDismissalConnection, "reviewDismissalAllowances"),

		// bleephub records no overlapping-rule analysis, so the conflict
		// connection is served empty rather than absent — the field GitHub
		// declares, with nothing to report.
		"branchProtectionRuleConflicts": &graphql.Field{
			Type: graphql.NewNonNull(conflictConnection),
			Args: connectionPagingArgs(),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return paginateGQLMaps(nil, p.Args), nil
			},
		},

		"matchingRefs": &graphql.Field{
			Type: graphql.NewNonNull(s.gqlRefConnectionType()),
			Args: graphql.FieldConfigArgument{
				"after":  &graphql.ArgumentConfig{Type: graphql.String},
				"before": &graphql.ArgumentConfig{Type: graphql.String},
				"first":  &graphql.ArgumentConfig{Type: graphql.Int},
				"last":   &graphql.ArgumentConfig{Type: graphql.Int},
				"query":  &graphql.ArgumentConfig{Type: graphql.String},
			},
			Resolve: s.resolveBranchProtectionMatchingRefs,
		},
	}
}

func (s *Resolver) branchProtectionRuleSourceRepo(source interface{}) *store.Repo {
	fields, ok := source.(map[string]interface{})
	if !ok {
		return nil
	}
	repoID, _ := fields["_repoID"].(int)
	return s.store.GetRepoByID(repoID)
}

func (s *Resolver) resolveBranchProtectionMatchingRefs(p graphql.ResolveParams) (interface{}, error) {
	source, ok := p.Source.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
	}
	pattern, _ := source["pattern"].(string)
	repo := s.branchProtectionRuleSourceRepo(p.Source)
	if repo == nil {
		return paginateGQLMaps(nil, p.Args), nil
	}
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return paginateGQLMaps(nil, p.Args), nil
	}
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		return paginateGQLMaps(nil, p.Args), nil
	}
	refs, err := store.ListGitReferences(stor, "refs/heads/")
	if err != nil {
		return paginateGQLMaps(nil, p.Args), nil
	}
	query, _ := p.Args["query"].(string)
	items := make([]gqlConnItem, 0, len(refs))
	for _, reference := range refs {
		ref := reference
		shortName := strings.TrimPrefix(ref.Name().String(), "refs/heads/")
		if !store.MatchBranchPattern(pattern, shortName) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(shortName), strings.ToLower(query)) {
			continue
		}
		items = append(items, gqlConnItem{
			identity: ref.Name().String(),
			render: func() map[string]interface{} {
				oid := ""
				if hash, err := store.ResolvedReferenceHash(stor, ref, map[plumbing.ReferenceName]bool{}); err == nil {
					oid = hash.String()
				}
				return s.decorateRefSource(repo, gitRefSource(repo.FullName, ref.Name().String(), oid))
			},
		})
	}
	return paginateGQLItems(items, p.Args), nil
}

// --- rendering --------------------------------------------------------------

// branchProtectionRuleForPR renders baseRef.branchProtectionRule: the
// exact-name rule protecting the pull request's base branch, in the full
// GraphQL shape. It reads the same stored record the REST protection GET
// serves.
func (s *Resolver) branchProtectionRuleForPR(repo *store.Repo, baseBranch string) map[string]interface{} {
	bp := s.store.GetBranchProtection(repo.ID, baseBranch)
	if bp == nil {
		return nil
	}
	return s.branchProtectionRuleSource(repo, baseBranch, bp)
}

func bpEnabled(rule *store.BPEnabled) bool { return rule != nil && rule.Enabled }

// branchProtectionRuleSource renders one protection rule as its
// BranchProtectionRule source map.
func (s *Resolver) branchProtectionRuleSource(repo *store.Repo, pattern string, bp *store.BranchProtection) map[string]interface{} {
	extras := s.store.GetBranchProtectionExtras(repo.ID, pattern)
	if extras == nil {
		extras = &store.BranchProtectionRuleExtras{}
	}

	source := map[string]interface{}{
		"id":         store.BranchProtectionRuleNodeID(repo.ID, pattern),
		"databaseId": nil,
		"pattern":    pattern,
		"_repoID":    repo.ID,
		"creator":    optionalRendered(s.store.LookupUserByLogin(extras.CreatorLogin), userToGraphQL),

		"allowsDeletions":                bpEnabled(bp.AllowDeletions),
		"allowsForcePushes":              bpEnabled(bp.AllowForcePushes),
		"blocksCreations":                bpEnabled(bp.BlockCreations),
		"isAdminEnforced":                bp.EnforceAdmins != nil && bp.EnforceAdmins.Enabled,
		"lockAllowsFetchAndMerge":        bpEnabled(bp.AllowForkSyncing),
		"lockBranch":                     bpEnabled(bp.LockBranch),
		"requiresCommitSignatures":       bp.RequiredSignatures != nil && bp.RequiredSignatures.Enabled,
		"requiresConversationResolution": bpEnabled(bp.RequiredConversationResolution),
		"requiresLinearHistory":          bpEnabled(bp.RequiredLinearHistory),
		"requiresDeployments":            extras.RequiresDeployments,

		"requiresApprovingReviews":     bp.RequiredPullRequestReviews != nil,
		"dismissesStaleReviews":        false,
		"requiresCodeOwnerReviews":     false,
		"requireLastPushApproval":      false,
		"restrictsReviewDismissals":    false,
		"requiredApprovingReviewCount": nil,

		"requiresStatusChecks":        bp.RequiredStatusChecks != nil,
		"requiresStrictStatusChecks":  bp.RequiredStatusChecks != nil && bp.RequiredStatusChecks.Strict,
		"requiredStatusCheckContexts": nil,
		"requiredStatusChecks":        nil,

		"restrictsPushes": bp.Restrictions != nil,

		"requiredDeploymentEnvironments": nil,
	}
	if len(extras.RequiredDeploymentEnvironments) > 0 || extras.RequiresDeployments {
		source["requiredDeploymentEnvironments"] = append([]string(nil), extras.RequiredDeploymentEnvironments...)
	}
	if reviews := bp.RequiredPullRequestReviews; reviews != nil {
		source["dismissesStaleReviews"] = reviews.DismissStaleReviews
		source["requiresCodeOwnerReviews"] = reviews.RequireCodeOwnerReviews
		source["requireLastPushApproval"] = reviews.RequireLastPushApproval
		source["requiredApprovingReviewCount"] = reviews.RequiredApprovingReviewCount
		source["restrictsReviewDismissals"] = reviews.DismissalRestrictions != nil
	}
	if checks := bp.RequiredStatusChecks; checks != nil {
		source["requiredStatusCheckContexts"] = append([]string(nil), checks.Contexts...)
		descriptions := make([]map[string]interface{}, 0, len(checks.Checks))
		for _, check := range checks.Checks {
			descriptions = append(descriptions, map[string]interface{}{
				"app":     nil,
				"context": check.Context,
			})
		}
		source["requiredStatusChecks"] = descriptions
	}

	source["bypassForcePushAllowances"] = s.allowanceConnectionSource(repo, pattern, "bypass-force-push", extras.BypassForcePushActors)
	var bypassPR, pushActors, dismissalActors []store.BPActor
	if bp.RequiredPullRequestReviews != nil {
		if allowances := bp.RequiredPullRequestReviews.BypassPullRequestAllowances; allowances != nil {
			bypassPR = flattenBPActors(allowances.Users, allowances.Teams, allowances.Apps)
		}
		if restrictions := bp.RequiredPullRequestReviews.DismissalRestrictions; restrictions != nil {
			dismissalActors = flattenBPActors(restrictions.Users, restrictions.Teams, restrictions.Apps)
		}
	}
	if bp.Restrictions != nil {
		pushActors = flattenBPActors(bp.Restrictions.Users, bp.Restrictions.Teams, bp.Restrictions.Apps)
	}
	source["bypassPullRequestAllowances"] = s.allowanceConnectionSource(repo, pattern, "bypass-pull-request", bypassPR)
	source["pushAllowances"] = s.allowanceConnectionSource(repo, pattern, "push", pushActors)
	source["reviewDismissalAllowances"] = s.allowanceConnectionSource(repo, pattern, "review-dismissal", dismissalActors)
	return source
}

func flattenBPActors(groups ...[]store.BPActor) []store.BPActor {
	var out []store.BPActor
	for _, group := range groups {
		out = append(out, group...)
	}
	return out
}

// allowanceConnectionSource renders one allowance list as a pre-paginated
// connection source. Each node's synthetic id names the rule, the allowance
// kind and the actor, which is enough to be stable across reads.
func (s *Resolver) allowanceConnectionSource(repo *store.Repo, pattern, kind string, actors []store.BPActor) map[string]interface{} {
	nodes := make([]map[string]interface{}, 0, len(actors))
	for _, actor := range actors {
		nodes = append(nodes, map[string]interface{}{
			"id":       store.BranchProtectionRuleNodeID(repo.ID, pattern) + "/" + kind + "/" + strings.ToLower(actor.Type) + "/" + actor.Login,
			"actor":    s.bpActorSource(repo, actor),
			"_repoID":  repo.ID,
			"_pattern": pattern,
		})
	}
	return paginateGQLMaps(nodes, nil)
}

// bpActorSource renders a restriction actor as its union member: the user by
// login, the team by slug under the repository's owning organization. An
// actor that no longer resolves renders a null actor on an allowance that
// still lists it, which is what GitHub serves for a deleted account.
func (s *Resolver) bpActorSource(repo *store.Repo, actor store.BPActor) interface{} {
	owner, _, _ := store.SplitRepoFullName(repo.FullName)
	switch actor.Type {
	case "Team":
		if team := s.store.GetTeam(owner, actor.Login); team != nil {
			return s.teamToGQL(team)
		}
	case "User", "":
		if user := s.store.LookupUserByLogin(actor.Login); user != nil {
			source := userToGraphQL(user)
			source["__typename"] = "User"
			return source
		}
	}
	return nil
}

// --- Repository.branchProtectionRules ---------------------------------------

// addBranchProtectionFieldsToSchema installs Repository.branchProtectionRules
// and the three branch-protection mutations.
func (s *Resolver) addBranchProtectionFieldsToSchema(repoType *graphql.Object, mutationType *graphql.Object) {
	ruleType := s.gqlBranchProtectionRuleType()
	ruleEdge := s.mutationObject("BranchProtectionRuleEdge", graphql.Fields{
		"cursor": gqlNonNull(graphql.String),
		"node":   gqlField(ruleType),
	})
	ruleConnection := s.mutationObject("BranchProtectionRuleConnection", graphql.Fields{
		"edges":      gqlField(graphql.NewList(ruleEdge)),
		"nodes":      gqlField(graphql.NewList(ruleType)),
		"pageInfo":   gqlNonNull(s.gqlPageInfoType()),
		"totalCount": gqlNonNull(graphql.Int),
	})

	repoType.AddFieldConfig("branchProtectionRules", &graphql.Field{
		Type: graphql.NewNonNull(ruleConnection),
		Args: connectionPagingArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromGraphQLSource(p.Source)
			if err != nil {
				return nil, err
			}
			if repo == nil || !s.viewerCanReadRepo(p.Context, repo) {
				return paginateGQLMaps(nil, p.Args), nil
			}
			var nodes []map[string]interface{}
			for _, branch := range s.store.ListBranchProtectedBranches(repo.ID) {
				if bp := s.store.GetBranchProtection(repo.ID, branch); bp != nil {
					nodes = append(nodes, s.branchProtectionRuleSource(repo, branch, bp))
				}
			}
			for _, rule := range s.store.ListBranchProtectionPatterns(repo.ID) {
				if rule.Protection != nil {
					nodes = append(nodes, s.branchProtectionRuleSource(repo, rule.Pattern, rule.Protection))
				}
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})

	s.addBranchProtectionMutations(mutationType)
}

// --- mutations ---------------------------------------------------------------

func (s *Resolver) addBranchProtectionMutations(mutationType *graphql.Object) {
	ruleType := s.gqlBranchProtectionRuleType()
	requiredStatusCheckInput := s.mutationInput("RequiredStatusCheckInput", graphql.InputObjectConfigFieldMap{
		"appId":   gqlID(),
		"context": gqlNonNullString(),
	})

	// The create and update inputs share every configuration member; they
	// differ only in how the subject is named (repositoryId + pattern! vs
	// branchProtectionRuleId + optional pattern).
	configurationInputFields := func() graphql.InputObjectConfigFieldMap {
		return graphql.InputObjectConfigFieldMap{
			"allowsDeletions":                gqlBool(),
			"allowsForcePushes":              gqlBool(),
			"blocksCreations":                gqlBool(),
			"bypassForcePushActorIds":        gqlListOf(graphql.ID),
			"bypassPullRequestActorIds":      gqlListOf(graphql.ID),
			"dismissesStaleReviews":          gqlBool(),
			"isAdminEnforced":                gqlBool(),
			"lockAllowsFetchAndMerge":        gqlBool(),
			"lockBranch":                     gqlBool(),
			"pushActorIds":                   gqlListOf(graphql.ID),
			"requireLastPushApproval":        gqlBool(),
			"requiredApprovingReviewCount":   gqlInt(),
			"requiredDeploymentEnvironments": gqlListOf(graphql.String),
			"requiredStatusCheckContexts":    gqlListOf(graphql.String),
			"requiredStatusChecks":           gqlListOf(requiredStatusCheckInput),
			"requiresApprovingReviews":       gqlBool(),
			"requiresCodeOwnerReviews":       gqlBool(),
			"requiresCommitSignatures":       gqlBool(),
			"requiresConversationResolution": gqlBool(),
			"requiresDeployments":            gqlBool(),
			"requiresLinearHistory":          gqlBool(),
			"requiresStatusChecks":           gqlBool(),
			"requiresStrictStatusChecks":     gqlBool(),
			"restrictsPushes":                gqlBool(),
			"restrictsReviewDismissals":      gqlBool(),
			"reviewDismissalActorIds":        gqlListOf(graphql.ID),
		}
	}

	createFields := configurationInputFields()
	createFields["repositoryId"] = gqlNonNullID()
	createFields["pattern"] = gqlNonNullString()
	s.registerMutation(mutationType, "createBranchProtectionRule", &graphql.Field{
		Type: s.mutationPayload("CreateBranchProtectionRulePayload", graphql.Fields{
			"branchProtectionRule": gqlField(ruleType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("CreateBranchProtectionRuleInput", createFields)),
		}},
		Resolve: s.resolveCreateBranchProtectionRule,
	})

	updateFields := configurationInputFields()
	updateFields["branchProtectionRuleId"] = gqlNonNullID()
	updateFields["pattern"] = gqlString()
	s.registerMutation(mutationType, "updateBranchProtectionRule", &graphql.Field{
		Type: s.mutationPayload("UpdateBranchProtectionRulePayload", graphql.Fields{
			"branchProtectionRule": gqlField(ruleType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateBranchProtectionRuleInput", updateFields)),
		}},
		Resolve: s.resolveUpdateBranchProtectionRule,
	})

	s.registerMutation(mutationType, "deleteBranchProtectionRule", &graphql.Field{
		Type: s.mutationPayload("DeleteBranchProtectionRulePayload", graphql.Fields{}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("DeleteBranchProtectionRuleInput", graphql.InputObjectConfigFieldMap{
				"branchProtectionRuleId": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveDeleteBranchProtectionRule,
	})
}

// enterpriseForbidsProtectedBranchUpdate is the enterprise-policy gate the
// REST protection handlers apply; the GraphQL mutations must refuse the same
// callers or the policy has a side door.
func (s *Resolver) enterpriseForbidsProtectedBranchUpdate(p graphql.ResolveParams, repo *store.Repo) error {
	policy, enterprise := s.store.EnterprisePolicyForRepo(repo)
	if s.store.EnterprisePolicyForbids(enterprise, policy.MembersCanUpdateProtectedBranches, s.ghUserFromContext(p.Context)) {
		return &ghForbiddenError{message: "Updating protected branches is disabled by an enterprise policy."}
	}
	return nil
}

// branchProtectionPatternIsWildcard reports whether the pattern addresses
// matching branches (the web-rule store) rather than one exact name (the
// REST-visible exact rule).
func branchProtectionPatternIsWildcard(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

// lookupBranchProtectionRule finds the rule a pattern names in either store.
func (s *Resolver) lookupBranchProtectionRule(repo *store.Repo, pattern string) (isPattern bool, bp *store.BranchProtection, patternRules []*store.BranchProtectionPatternRule, found bool) {
	if bp := s.store.GetBranchProtection(repo.ID, pattern); bp != nil {
		return false, bp, nil, true
	}
	rules := s.store.ListBranchProtectionPatterns(repo.ID)
	for _, rule := range rules {
		if rule.Pattern == pattern {
			return true, rule.Protection, rules, true
		}
	}
	return false, nil, rules, false
}

// applyBranchProtectionRuleInput merges the configuration members present in
// a create/update input into the protection record — sparse, exactly as the
// REST PUT merges its body.
func (s *Resolver) applyBranchProtectionRuleInput(bp *store.BranchProtection, input map[string]interface{}) {
	setEnabled := func(field **store.BPEnabled, key string) {
		if value, ok := gqlInputBool(input, key); ok {
			*field = &store.BPEnabled{Enabled: value}
		}
	}
	setEnabled(&bp.RequiredLinearHistory, "requiresLinearHistory")
	setEnabled(&bp.AllowForcePushes, "allowsForcePushes")
	setEnabled(&bp.AllowDeletions, "allowsDeletions")
	setEnabled(&bp.BlockCreations, "blocksCreations")
	setEnabled(&bp.RequiredConversationResolution, "requiresConversationResolution")
	setEnabled(&bp.LockBranch, "lockBranch")
	setEnabled(&bp.AllowForkSyncing, "lockAllowsFetchAndMerge")
	if value, ok := gqlInputBool(input, "isAdminEnforced"); ok {
		bp.EnforceAdmins = &store.BPEnforceAdmins{Enabled: value}
	}
	if value, ok := gqlInputBool(input, "requiresCommitSignatures"); ok {
		bp.RequiredSignatures = &store.BPEnabledURL{Enabled: value}
	}

	// Status checks: any of the four check members establishes the rule.
	contexts, hasContexts := gqlInputStrings(input, "requiredStatusCheckContexts")
	checkInputs := gqlInputObjects(input, "requiredStatusChecks")
	_, hasChecksList := input["requiredStatusChecks"]
	requiresChecks, hasRequiresChecks := gqlInputBool(input, "requiresStatusChecks")
	strict, hasStrict := gqlInputBool(input, "requiresStrictStatusChecks")
	if hasContexts || hasChecksList || hasRequiresChecks || hasStrict {
		if hasRequiresChecks && !requiresChecks {
			bp.RequiredStatusChecks = nil
		} else {
			checks := bp.RequiredStatusChecks
			if checks == nil {
				checks = &store.BPStatusChecks{}
			}
			if hasStrict {
				checks.Strict = strict
			}
			switch {
			case hasChecksList:
				rows := make([]store.BPCheck, 0, len(checkInputs))
				for _, check := range checkInputs {
					context, _ := check["context"].(string)
					if context == "" {
						continue
					}
					rows = append(rows, store.BPCheck{Context: context})
				}
				checks.SetChecks(rows)
			case hasContexts:
				checks.SetContexts(contexts)
			default:
				if checks.Contexts == nil {
					checks.SetContexts(nil)
				}
			}
			bp.RequiredStatusChecks = checks
		}
	}

	// Reviews: any review member establishes the rule; requiresApprovingReviews
	// false removes it, exactly as a null required_pull_request_reviews does.
	requiresReviews, hasRequiresReviews := gqlInputBool(input, "requiresApprovingReviews")
	count, hasCount := gqlInputInt(input, "requiredApprovingReviewCount")
	dismissStale, hasDismissStale := gqlInputBool(input, "dismissesStaleReviews")
	codeOwners, hasCodeOwners := gqlInputBool(input, "requiresCodeOwnerReviews")
	lastPush, hasLastPush := gqlInputBool(input, "requireLastPushApproval")
	restrictsDismissals, hasRestrictsDismissals := gqlInputBool(input, "restrictsReviewDismissals")
	dismissalIDs, hasDismissalIDs := gqlInputStrings(input, "reviewDismissalActorIds")
	bypassPRIDs, hasBypassPRIDs := gqlInputStrings(input, "bypassPullRequestActorIds")
	touchesReviews := hasRequiresReviews || hasCount || hasDismissStale || hasCodeOwners ||
		hasLastPush || hasRestrictsDismissals || hasDismissalIDs || hasBypassPRIDs
	if touchesReviews {
		if hasRequiresReviews && !requiresReviews {
			bp.RequiredPullRequestReviews = nil
		} else {
			reviews := bp.RequiredPullRequestReviews
			if reviews == nil {
				reviews = &store.BPPullRequestReviews{}
			}
			if hasCount {
				reviews.RequiredApprovingReviewCount = count
			}
			if hasDismissStale {
				reviews.DismissStaleReviews = dismissStale
			}
			if hasCodeOwners {
				reviews.RequireCodeOwnerReviews = codeOwners
			}
			if hasLastPush {
				reviews.RequireLastPushApproval = lastPush
			}
			if hasRestrictsDismissals && !restrictsDismissals {
				reviews.DismissalRestrictions = nil
			} else if hasDismissalIDs || (hasRestrictsDismissals && restrictsDismissals) {
				reviews.DismissalRestrictions = s.bpRestrictionsFromActorIDs(dismissalIDs)
			}
			if hasBypassPRIDs {
				restrictions := s.bpRestrictionsFromActorIDs(bypassPRIDs)
				reviews.BypassPullRequestAllowances = &store.BPBypassAllowances{
					Users: restrictions.Users, Teams: restrictions.Teams, Apps: restrictions.Apps,
				}
			}
			bp.RequiredPullRequestReviews = reviews
		}
	}

	// Push restrictions.
	restrictsPushes, hasRestrictsPushes := gqlInputBool(input, "restrictsPushes")
	pushIDs, hasPushIDs := gqlInputStrings(input, "pushActorIds")
	if hasRestrictsPushes && !restrictsPushes {
		bp.Restrictions = nil
	} else if hasPushIDs || (hasRestrictsPushes && restrictsPushes) {
		bp.Restrictions = s.bpRestrictionsFromActorIDs(pushIDs)
	}
}

// applyBranchProtectionExtrasInput merges the GraphQL-only members.
func (s *Resolver) applyBranchProtectionExtrasInput(extras *store.BranchProtectionRuleExtras, input map[string]interface{}) {
	if value, ok := gqlInputBool(input, "requiresDeployments"); ok {
		extras.RequiresDeployments = value
	}
	if environments, ok := gqlInputStrings(input, "requiredDeploymentEnvironments"); ok {
		extras.RequiredDeploymentEnvironments = environments
		if len(environments) > 0 {
			extras.RequiresDeployments = true
		}
	}
	if ids, ok := gqlInputStrings(input, "bypassForcePushActorIds"); ok {
		restrictions := s.bpRestrictionsFromActorIDs(ids)
		extras.BypassForcePushActors = flattenBPActors(restrictions.Users, restrictions.Teams, restrictions.Apps)
	}
}

// bpRestrictionsFromActorIDs resolves User/Team global ids to the actor rows
// the REST restrictions objects hold. Ids that name neither are dropped, as
// the REST routes drop unknown logins.
func (s *Resolver) bpRestrictionsFromActorIDs(nodeIDs []string) *store.BPRestrictions {
	restrictions := &store.BPRestrictions{Users: []store.BPActor{}, Teams: []store.BPActor{}, Apps: []store.BPActor{}}
	for _, nodeID := range nodeIDs {
		if user := store.FindUserByNodeID(s.store, nodeID); user != nil {
			restrictions.Users = append(restrictions.Users, store.BPActor{Login: user.Login, ID: user.ID, Type: "User"})
			continue
		}
		if team, _ := store.FindTeamByNodeID(s.store, nodeID); team != nil {
			restrictions.Teams = append(restrictions.Teams, store.BPActor{Login: team.Slug, ID: team.ID, Type: "Team"})
		}
	}
	return restrictions
}

// writeBranchProtectionRule stores the rule under its pattern in whichever
// store the pattern belongs to, replacing patternRules for the wildcard case.
func (s *Resolver) writeBranchProtectionRule(repo *store.Repo, pattern string, bp *store.BranchProtection, patternRules []*store.BranchProtectionPatternRule) {
	if branchProtectionPatternIsWildcard(pattern) {
		replaced := false
		for i, rule := range patternRules {
			if rule.Pattern == pattern {
				patternRules[i] = &store.BranchProtectionPatternRule{Pattern: pattern, Protection: bp}
				replaced = true
				break
			}
		}
		if !replaced {
			patternRules = append(patternRules, &store.BranchProtectionPatternRule{Pattern: pattern, Protection: bp})
		}
		s.store.SetBranchProtectionPatterns(repo.ID, patternRules)
		return
	}
	s.store.SetBranchProtection(repo.ID, pattern, bp)
}

func (s *Resolver) removeBranchProtectionRule(repo *store.Repo, pattern string, isPattern bool) {
	if isPattern {
		var remaining []*store.BranchProtectionPatternRule
		for _, rule := range s.store.ListBranchProtectionPatterns(repo.ID) {
			if rule.Pattern != pattern {
				remaining = append(remaining, rule)
			}
		}
		s.store.SetBranchProtectionPatterns(repo.ID, remaining)
		return
	}
	s.store.SetBranchProtection(repo.ID, pattern, nil)
}

// emitBranchProtectionRuleEvent delivers the `branch_protection_rule` webhook
// the REST protection routes deliver, through the same repo-keyed fan-out.
func (s *Resolver) emitBranchProtectionRuleEvent(p graphql.ResolveParams, repo *store.Repo, pattern, action string) {
	s.emitWebhookEvent(repo.FullName, "branch_protection_rule", action, map[string]interface{}{
		"action":     action,
		"rule":       map[string]interface{}{"name": pattern, "repository_id": repo.ID},
		"repository": s.repoPayload(repo),
		"sender":     s.senderPayload(s.ghUserFromContext(p.Context)),
	})
}

func (s *Resolver) resolveCreateBranchProtectionRule(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	repo, err := s.mutationRepoFromInput(input, "repositoryId")
	if err != nil {
		return nil, err
	}
	if err := s.enterpriseForbidsProtectedBranchUpdate(p, repo); err != nil {
		return nil, err
	}
	pattern, _ := gqlInputString(input, "pattern")
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, fmt.Errorf("Pattern can't be blank")
	}
	_, _, patternRules, exists := s.lookupBranchProtectionRule(repo, pattern)
	if exists {
		return nil, fmt.Errorf("Pattern has already been taken")
	}

	bp := &store.BranchProtection{Enabled: true}
	s.applyBranchProtectionRuleInput(bp, input)
	s.writeBranchProtectionRule(repo, pattern, bp, patternRules)

	extras := &store.BranchProtectionRuleExtras{CreatorLogin: s.ghUserFromContext(p.Context).Login}
	s.applyBranchProtectionExtrasInput(extras, input)
	s.store.SetBranchProtectionExtras(repo.ID, pattern, extras)

	s.emitBranchProtectionRuleEvent(p, repo, pattern, "created")
	s.maybeAutoMergeRepo(repo)
	return map[string]interface{}{
		"branchProtectionRule": s.branchProtectionRuleSource(repo, pattern, bp),
	}, nil
}

func (s *Resolver) resolveUpdateBranchProtectionRule(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "branchProtectionRuleId")
	repoID, pattern, ok := store.ParseBranchProtectionRuleNodeID(nodeID)
	if !ok {
		return nil, gqlMissingNode("BranchProtectionRule", nodeID)
	}
	repo := s.store.GetRepoByID(repoID)
	if repo == nil {
		return nil, gqlMissingNode("BranchProtectionRule", nodeID)
	}
	if err := s.enterpriseForbidsProtectedBranchUpdate(p, repo); err != nil {
		return nil, err
	}
	isPattern, bp, patternRules, found := s.lookupBranchProtectionRule(repo, pattern)
	if !found {
		return nil, gqlMissingNode("BranchProtectionRule", nodeID)
	}

	newPattern := pattern
	if renamed, ok := gqlInputString(input, "pattern"); ok && strings.TrimSpace(renamed) != "" {
		newPattern = strings.TrimSpace(renamed)
	}
	if newPattern != pattern {
		if _, _, _, taken := s.lookupBranchProtectionRule(repo, newPattern); taken {
			return nil, fmt.Errorf("Pattern has already been taken")
		}
	}

	s.applyBranchProtectionRuleInput(bp, input)

	if newPattern != pattern {
		s.removeBranchProtectionRule(repo, pattern, isPattern)
		s.store.MoveBranchProtectionExtras(repo.ID, pattern, newPattern)
		// The rule list was re-read by the removal; write against the fresh set.
		patternRules = s.store.ListBranchProtectionPatterns(repo.ID)
	}
	s.writeBranchProtectionRule(repo, newPattern, bp, patternRules)

	extras := s.store.GetBranchProtectionExtras(repo.ID, newPattern)
	if extras == nil {
		extras = &store.BranchProtectionRuleExtras{}
	}
	s.applyBranchProtectionExtrasInput(extras, input)
	s.store.SetBranchProtectionExtras(repo.ID, newPattern, extras)

	s.emitBranchProtectionRuleEvent(p, repo, newPattern, "edited")
	s.maybeAutoMergeRepo(repo)
	return map[string]interface{}{
		"branchProtectionRule": s.branchProtectionRuleSource(repo, newPattern, bp),
	}, nil
}

func (s *Resolver) resolveDeleteBranchProtectionRule(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "branchProtectionRuleId")
	repoID, pattern, ok := store.ParseBranchProtectionRuleNodeID(nodeID)
	if !ok {
		return nil, gqlMissingNode("BranchProtectionRule", nodeID)
	}
	repo := s.store.GetRepoByID(repoID)
	if repo == nil {
		return nil, gqlMissingNode("BranchProtectionRule", nodeID)
	}
	if err := s.enterpriseForbidsProtectedBranchUpdate(p, repo); err != nil {
		return nil, err
	}
	isPattern, _, _, found := s.lookupBranchProtectionRule(repo, pattern)
	if !found {
		return nil, gqlMissingNode("BranchProtectionRule", nodeID)
	}
	s.removeBranchProtectionRule(repo, pattern, isPattern)
	s.store.SetBranchProtectionExtras(repo.ID, pattern, nil)
	s.emitBranchProtectionRuleEvent(p, repo, pattern, "deleted")
	s.maybeAutoMergeRepo(repo)
	return map[string]interface{}{}, nil
}

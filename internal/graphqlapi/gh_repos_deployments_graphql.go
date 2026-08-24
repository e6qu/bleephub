package graphqlapi

// Repository.deployments / environments / environment, over the deployment
// store the REST deployment and environment routes write.

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// addRepositoryDeploymentFields installs the deployment graph on Repository.
func (s *Resolver) addRepositoryDeploymentFields(types *accountSurfaceTypes) {
	repoType := types.repository
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")

	deploymentStatusState := s.sharedEnum("DeploymentStatusState",
		"ERROR", "FAILURE", "INACTIVE", "IN_PROGRESS", "PENDING", "QUEUED", "SUCCESS", "WAITING")
	deploymentState := s.sharedEnum("DeploymentState",
		"ABANDONED", "ACTIVE", "DESTROYED", "ERROR", "FAILURE", "INACTIVE", "IN_PROGRESS",
		"PENDING", "QUEUED", "SUCCESS", "WAITING")

	deploymentType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Deployment",
		Fields: graphql.Fields{
			"commitOid":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"createdAt":           &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"databaseId":          &graphql.Field{Type: graphql.Int},
			"description":         &graphql.Field{Type: graphql.String},
			"environment":         &graphql.Field{Type: graphql.String},
			"id":                  &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"latestEnvironment":   &graphql.Field{Type: graphql.String},
			"originalEnvironment": &graphql.Field{Type: graphql.String},
			"payload":             &graphql.Field{Type: graphql.String},
			"state":               &graphql.Field{Type: deploymentState},
			"task":                &graphql.Field{Type: graphql.String},
			"updatedAt":           &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"creator":             &graphql.Field{Type: graphql.NewNonNull(s.graphqlTypes.actor)},
			"repository":          &graphql.Field{Type: graphql.NewNonNull(repoType)},
			"commit":              &graphql.Field{Type: s.graphqlTypes.commit},
			"ref":                 &graphql.Field{Type: s.graphqlTypes.ref},
		},
	})

	deploymentStatusType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DeploymentStatus",
		Fields: graphql.Fields{
			"createdAt":      &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"description":    &graphql.Field{Type: graphql.String},
			"environment":    &graphql.Field{Type: graphql.String},
			"environmentUrl": &graphql.Field{Type: uri},
			"id":             &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"logUrl":         &graphql.Field{Type: uri},
			"state":          &graphql.Field{Type: graphql.NewNonNull(deploymentStatusState)},
			"updatedAt":      &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"creator":        &graphql.Field{Type: graphql.NewNonNull(s.graphqlTypes.actor)},
			"deployment":     &graphql.Field{Type: graphql.NewNonNull(deploymentType)},
		},
	})
	deploymentType.AddFieldConfig("latestStatus", &graphql.Field{Type: deploymentStatusType})
	deploymentType.AddFieldConfig("statuses", &graphql.Field{
		Type: s.accountConnectionType(types, "DeploymentStatus", deploymentStatusType, false, nil),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, err := graphQLSourceMap(p.Source)
			if err != nil {
				return nil, err
			}
			deploymentID, _ := src["databaseId"].(int)
			deployment := s.store.Deployments.GetDeployment(deploymentID)
			if deployment == nil {
				return nil, nil
			}
			statuses := s.store.Deployments.ListStatuses(deploymentID)
			sort.Slice(statuses, func(i, j int) bool { return statuses[i].ID < statuses[j].ID })
			items := make([]gqlConnItem, 0, len(statuses))
			for i := range statuses {
				row := statuses[i]
				items = append(items, gqlConnItem{
					identity: row.NodeID,
					render: func() map[string]interface{} {
						return s.deploymentStatusSource(row, src)
					},
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	// --- protection rules --------------------------------------------------
	deploymentReviewer := graphql.NewUnion(graphql.UnionConfig{
		Name:  "DeploymentReviewer",
		Types: []*graphql.Object{types.user, s.graphqlTypes.team},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			if source, ok := p.Value.(map[string]interface{}); ok {
				if name, _ := source["__typename"].(string); name == "Team" {
					return s.graphqlTypes.team
				}
			}
			return types.user
		},
	})
	// Expose the reviewer connection for the Actions run graph
	// (DeploymentRequest.reviewers reuses this exact object).
	s.graphqlTypes.deploymentReviewerConnection = s.accountConnectionType(types, "DeploymentReviewer", deploymentReviewer, false, nil)
	protectionRuleType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DeploymentProtectionRule",
		Fields: graphql.Fields{
			"databaseId": &graphql.Field{Type: graphql.Int},
			"timeout":    &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"type": &graphql.Field{Type: graphql.NewNonNull(s.sharedEnum("DeploymentProtectionRuleType",
				"BRANCH_POLICY", "REQUIRED_REVIEWERS", "WAIT_TIMER"))},
			"reviewers": &graphql.Field{
				Type: graphql.NewNonNull(s.accountConnectionType(types, "DeploymentReviewer", deploymentReviewer, false, nil)),
				Args: connectionArgs(nil),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, err := graphQLSourceMap(p.Source)
					if err != nil {
						return nil, err
					}
					reviewers, _ := src["_reviewers"].([]map[string]interface{})
					items := make([]gqlConnItem, 0, len(reviewers))
					for i := range reviewers {
						node := reviewers[i]
						identity, _ := node["nodeID"].(string)
						items = append(items, gqlConnItem{
							identity: identity,
							render:   func() map[string]interface{} { return node },
						})
					}
					return paginateGQLItems(items, p.Args), nil
				},
			},
		},
	})

	environmentType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Environment",
		Fields: graphql.Fields{
			"databaseId":     &graphql.Field{Type: graphql.Int},
			"id":             &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"name":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"isPinned":       &graphql.Field{Type: graphql.Boolean},
			"pinnedPosition": &graphql.Field{Type: graphql.Int},
			"latestCompletedDeployment": &graphql.Field{
				Type: deploymentType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, err := graphQLSourceMap(p.Source)
					if err != nil {
						return nil, err
					}
					return src["latestCompletedDeployment"], nil
				},
			},
			"protectionRules": &graphql.Field{
				Type: graphql.NewNonNull(s.accountConnectionType(types, "DeploymentProtectionRule", protectionRuleType, false, nil)),
				Args: connectionArgs(nil),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, err := graphQLSourceMap(p.Source)
					if err != nil {
						return nil, err
					}
					rules, _ := src["_protectionRules"].([]map[string]interface{})
					items := make([]gqlConnItem, 0, len(rules))
					for i := range rules {
						rule := rules[i]
						identity, _ := rule["_identity"].(string)
						items = append(items, gqlConnItem{
							identity: identity,
							render:   func() map[string]interface{} { return rule },
						})
					}
					return paginateGQLItems(items, p.Args), nil
				},
			},
		},
	})

	// The mutation surface's payloads name the same objects, so they are
	// memoized here where they are built.
	s.graphqlTypes.deployment = deploymentType
	s.graphqlTypes.deploymentStatus = deploymentStatusType
	s.graphqlTypes.environment = environmentType
	// Expose the environment connection for the Actions run graph
	// (DeploymentReview.environments reuses this exact object).
	s.graphqlTypes.environmentConnection = s.accountConnectionType(types, "Environment", environmentType, false, nil)

	// --- Repository fields -------------------------------------------------
	repoType.AddFieldConfig("deployments", &graphql.Field{
		Type: graphql.NewNonNull(s.accountConnectionType(types, "Deployment", deploymentType, false, nil)),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"environments": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"orderBy": &graphql.ArgumentConfig{
				Type: s.gqlOrderInput(types, "DeploymentOrder", "DeploymentOrderField", "CREATED_AT"),
			},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if !s.viewerCanReadRepo(p.Context, repo) {
				return paginateGQLItems(nil, p.Args), nil
			}
			deployments := s.store.Deployments.ListDeployments(repo.ID)
			if wanted := stringListArg(p.Args["environments"]); len(wanted) > 0 {
				kept := make([]*store.Deployment, 0, len(deployments))
				for _, deployment := range deployments {
					for _, name := range wanted {
						if deployment.Environment == name {
							kept = append(kept, deployment)
							break
						}
					}
				}
				deployments = kept
			}
			descending := orderDirectionDescending(p.Args, "orderBy", false)
			sort.Slice(deployments, func(i, j int) bool {
				if descending {
					return deployments[i].ID > deployments[j].ID
				}
				return deployments[i].ID < deployments[j].ID
			})
			repoSource := repoToGraphQL(s.store, s.store.SnapRepo(repo))
			items := make([]gqlConnItem, 0, len(deployments))
			for i := range deployments {
				row := deployments[i]
				items = append(items, gqlConnItem{
					identity: row.NodeID,
					render:   func() map[string]interface{} { return s.deploymentSource(repo, row, repoSource) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	repoType.AddFieldConfig("environments", &graphql.Field{
		Type: graphql.NewNonNull(s.accountConnectionType(types, "Environment", environmentType, false, nil)),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"names": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"orderBy": &graphql.ArgumentConfig{
				Type: s.gqlOrderInput(types, "Environments", "EnvironmentOrderField", "NAME"),
			},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if !s.viewerCanReadRepo(p.Context, repo) {
				return paginateGQLItems(nil, p.Args), nil
			}
			environments := s.store.Deployments.ListEnvironments(repo.ID)
			if wanted := stringListArg(p.Args["names"]); len(wanted) > 0 {
				kept := make([]*store.Environment, 0, len(environments))
				for _, env := range environments {
					for _, name := range wanted {
						if env.Name == name {
							kept = append(kept, env)
							break
						}
					}
				}
				environments = kept
			}
			descending := orderDirectionDescending(p.Args, "orderBy", false)
			sort.Slice(environments, func(i, j int) bool {
				if descending {
					return environments[i].Name > environments[j].Name
				}
				return environments[i].Name < environments[j].Name
			})
			repoSource := repoToGraphQL(s.store, s.store.SnapRepo(repo))
			items := make([]gqlConnItem, 0, len(environments))
			for i := range environments {
				row := environments[i]
				items = append(items, gqlConnItem{
					identity: row.NodeID,
					render:   func() map[string]interface{} { return s.environmentSource(repo, row, repoSource) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})

	repoType.AddFieldConfig("environment", &graphql.Field{
		Type: environmentType,
		Args: graphql.FieldConfigArgument{
			"name": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			if !s.viewerCanReadRepo(p.Context, repo) {
				return nil, nil
			}
			name, _ := p.Args["name"].(string)
			env := s.store.Deployments.GetEnvironment(repo.ID, name)
			if env == nil {
				return nil, nil
			}
			repoSource := repoToGraphQL(s.store, s.store.SnapRepo(repo))
			return s.environmentSource(repo, env, repoSource), nil
		},
	})
}

// deploymentSource renders one deployment, including the commit and ref it
// points at and the status that last landed on it.
func (s *Resolver) deploymentSource(repo *store.Repo, d *store.Deployment, repoSource map[string]interface{}) map[string]interface{} {
	source := map[string]interface{}{
		"nodeID":              d.NodeID,
		"id":                  d.NodeID,
		"databaseId":          d.ID,
		"commitOid":           d.Sha,
		"createdAt":           d.CreatedAt.UTC().Format(rfc3339),
		"updatedAt":           d.UpdatedAt.UTC().Format(rfc3339),
		"description":         nilStr(d.Description),
		"environment":         nilStr(d.Environment),
		"latestEnvironment":   nilStr(d.Environment),
		"originalEnvironment": nilStr(d.OriginalEnv),
		"task":                nilStr(d.Task),
		"payload":             deploymentPayloadJSON(d.Payload),
		"creator":             optionalRendered(s.store.GetUserByID(d.CreatorID), userToGraphQL),
		"repository":          repoSource,
	}

	statuses := s.store.Deployments.ListStatuses(d.ID)
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].ID < statuses[j].ID })
	if len(statuses) > 0 {
		latest := statuses[len(statuses)-1]
		source["latestStatus"] = s.deploymentStatusSource(latest, source)
		source["state"] = strings.ToUpper(string(latest.State))
	} else {
		// GitHub reports a deployment with no status yet as null-state, and
		// `state` is nullable precisely for that case.
		source["state"] = nil
		source["latestStatus"] = nil
	}

	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if ok && d.Sha != "" {
		if stor := s.store.GetGitStorage(owner, name); stor != nil {
			source["commit"] = optionalObject(gitObjectSourceOfType(
				stor, s.store, repo.FullName, plumbing.NewHash(d.Sha), plumbing.CommitObject))
		}
	}
	if d.Ref != "" {
		source["ref"] = gitRefSource(repo.FullName, qualifiedDeploymentRef(d.Ref), d.Sha)
	}
	return source
}

// deploymentStatusSource renders one deployment status. The deployment source
// is threaded in so DeploymentStatus.deployment resolves without re-reading.
func (s *Resolver) deploymentStatusSource(status *store.DeploymentStatus, deployment map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"nodeID":         status.NodeID,
		"id":             status.NodeID,
		"createdAt":      status.CreatedAt.UTC().Format(rfc3339),
		"updatedAt":      status.UpdatedAt.UTC().Format(rfc3339),
		"description":    nilStr(status.Description),
		"environment":    nilStr(status.Environment),
		"environmentUrl": nilStr(status.EnvironmentURL),
		"logUrl":         nilStr(status.LogURL),
		"state":          strings.ToUpper(string(status.State)),
		"creator":        optionalRendered(s.store.GetUserByID(status.CreatorID), userToGraphQL),
		"deployment":     deployment,
	}
}

// environmentSource renders one environment with the protection rules the
// REST environment routes persist and its last completed deployment.
func (s *Resolver) environmentSource(repo *store.Repo, env *store.Environment, repoSource map[string]interface{}) map[string]interface{} {
	source := map[string]interface{}{
		"nodeID":                    env.NodeID,
		"id":                        env.NodeID,
		"databaseId":                env.ID,
		"name":                      env.Name,
		"isPinned":                  false,
		"pinnedPosition":            nil,
		"_protectionRules":          s.environmentProtectionRules(env),
		"latestCompletedDeployment": optionalObject(s.latestCompletedDeployment(repo, env, repoSource)),
	}
	if pin := s.store.Deployments.GetPinnedEnvironment(repo.ID, env.ID); pin != nil {
		source["isPinned"] = true
		source["pinnedPosition"] = pin.Position
	}
	return source
}

// environmentProtectionRules renders the environment's wait timer, required
// reviewers and branch policy as GitHub's three protection-rule kinds. Only
// the rules the environment actually configures are reported.
func (s *Resolver) environmentProtectionRules(env *store.Environment) []map[string]interface{} {
	var rules []map[string]interface{}
	if env.WaitTimer > 0 {
		rules = append(rules, map[string]interface{}{
			"_identity":  env.NodeID + ":wait_timer",
			"databaseId": env.ID,
			"timeout":    env.WaitTimer,
			"type":       "WAIT_TIMER",
			"_reviewers": []map[string]interface{}{},
		})
	}
	if len(env.Reviewers) > 0 {
		rules = append(rules, map[string]interface{}{
			"_identity":  env.NodeID + ":required_reviewers",
			"databaseId": env.ID,
			"timeout":    0,
			"type":       "REQUIRED_REVIEWERS",
			"_reviewers": s.environmentReviewerSources(env),
		})
	}
	if env.DeploymentBranchPolicy != nil {
		rules = append(rules, map[string]interface{}{
			"_identity":  env.NodeID + ":branch_policy",
			"databaseId": env.ID,
			"timeout":    0,
			"type":       "BRANCH_POLICY",
			"_reviewers": []map[string]interface{}{},
		})
	}
	return rules
}

// environmentReviewerSources renders the accounts and teams that must approve
// a deployment to this environment.
func (s *Resolver) environmentReviewerSources(env *store.Environment) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(env.Reviewers))
	for _, reviewer := range env.Reviewers {
		kind, _ := reviewer["type"].(string)
		id := reviewerID(reviewer["id"])
		if kind == "Team" {
			team := s.store.GetTeamByID(id)
			if team == nil {
				continue
			}
			org := s.store.GetOrgByID(team.OrgID)
			if org == nil {
				continue
			}
			node := s.teamSource(team, org)
			node["__typename"] = "Team"
			out = append(out, node)
			continue
		}
		user := s.store.GetUserByID(id)
		if user == nil {
			continue
		}
		node := userToGraphQL(user)
		node["__typename"] = "User"
		out = append(out, node)
	}
	return out
}

// reviewerID coerces a persisted reviewer id, which round-trips through JSON
// as a float64.
func reviewerID(value interface{}) int {
	switch typed := value.(type) {
	case int:
		return typed
	case float64:
		return int(typed)
	}
	return 0
}

// latestCompletedDeployment is the newest deployment to this environment whose
// last status is a terminal one, which is what GitHub reports.
func (s *Resolver) latestCompletedDeployment(repo *store.Repo, env *store.Environment, repoSource map[string]interface{}) map[string]interface{} {
	deployments := s.store.Deployments.ListDeployments(repo.ID)
	sort.Slice(deployments, func(i, j int) bool { return deployments[i].ID > deployments[j].ID })
	for _, deployment := range deployments {
		if deployment.Environment != env.Name {
			continue
		}
		statuses := s.store.Deployments.ListStatuses(deployment.ID)
		if len(statuses) == 0 {
			continue
		}
		sort.Slice(statuses, func(i, j int) bool { return statuses[i].ID < statuses[j].ID })
		if !completedDeploymentState(string(statuses[len(statuses)-1].State)) {
			continue
		}
		return s.deploymentSource(repo, deployment, repoSource)
	}
	return nil
}

// completedDeploymentState reports whether a status state is terminal.
func completedDeploymentState(state string) bool {
	switch strings.ToLower(state) {
	case "success", "failure", "error", "inactive":
		return true
	}
	return false
}

// deploymentPayloadJSON renders a deployment's payload as the JSON string
// GitHub serves it as, or null when the deployment carried none.
func deploymentPayloadJSON(payload map[string]interface{}) interface{} {
	if len(payload) == 0 {
		return nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return string(encoded)
}

// qualifiedDeploymentRef expands a deployment's ref, which the REST surface
// accepts as a branch name, tag name or sha, into the fully qualified form
// Ref reports.
func qualifiedDeploymentRef(ref string) string {
	if strings.HasPrefix(ref, "refs/") {
		return ref
	}
	return "refs/heads/" + ref
}

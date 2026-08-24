package graphqlapi

// The deployments and environments mutation families: createDeployment,
// createDeploymentStatus, deleteDeployment; approveDeployments and
// rejectDeployments over a workflow run's pending reviewer-protected
// environments; and the environment CRUD plus the pinned-environment pair
// (pinEnvironment, reorderEnvironment).
//
// Every mutation writes the same DeploymentStore the REST deployment and
// environment routes write, emits the same deployment/deployment_status
// webhooks, and hands deployment reviews to the same actions-engine path the
// pending_deployments route runs, so the two surfaces cannot diverge.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

func init() {
	for name, rule := range map[string]mutationRule{
		// Deployments are gated on the Deployments permission at write with
		// push standing, as POST /deployments' requirePerm is.
		"createDeployment":       repoRule{scope: store.ScopeDeployments, level: mutationPushRepo, target: mutationTargetRepo("repositoryId")},
		"createDeploymentStatus": repoRule{scope: store.ScopeDeployments, level: mutationPushRepo, target: mutationTargetDeployment("deploymentId")},
		"deleteDeployment":       repoRule{scope: store.ScopeDeployments, level: mutationPushRepo, target: mutationTargetDeployment("id")},

		// Reviewing a run's pending deployments is the Actions write the
		// REST pending_deployments route demands.
		"approveDeployments": repoRule{scope: store.ScopeActions, level: mutationPushRepo, target: mutationTargetWorkflowRun("workflowRunId")},
		"rejectDeployments":  repoRule{scope: store.ScopeActions, level: mutationPushRepo, target: mutationTargetWorkflowRun("workflowRunId")},

		// Environments are repository settings: the REST environment routes
		// demand Administration at write, and admin standing carries the
		// pinned-list curation with it.
		"createEnvironment":  repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetRepo("repositoryId")},
		"updateEnvironment":  repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetEnvironment("environmentId")},
		"deleteEnvironment":  repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetEnvironment("id")},
		"pinEnvironment":     repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetEnvironment("environmentId")},
		"reorderEnvironment": repoRule{scope: store.ScopeAdministration, level: mutationAdminRepo, target: mutationTargetEnvironment("environmentId")},
	} {
		if _, exists := graphqlMutationAuthz[name]; exists {
			panic(fmt.Sprintf("graphql mutation %q already has a policy row", name))
		}
		graphqlMutationAuthz[name] = rule
	}
}

// mutationTargetDeployment resolves a Deployment global id to the repository
// it was created on.
func mutationTargetDeployment(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("Deployment", nodeID)}
		if d := s.store.Deployments.GetDeploymentByNodeID(nodeID); d != nil {
			target.repo = s.store.GetRepoByID(d.RepoID)
		}
		return target
	}
}

// mutationTargetEnvironment resolves an Environment global id to the
// repository that configures it.
func mutationTargetEnvironment(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("Environment", nodeID)}
		if env := s.store.Deployments.GetEnvironmentByNodeID(nodeID); env != nil {
			target.repo = s.store.GetRepoByID(env.RepoID)
		}
		return target
	}
}

// mutationTargetWorkflowRun resolves a WorkflowRun global id to the
// repository the run belongs to.
func mutationTargetWorkflowRun(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("WorkflowRun", nodeID)}
		if wf := s.workflowRunByNodeID(nodeID); wf != nil {
			target.repo = s.store.GetRepoByFullName(wf.RepoFullName)
		}
		return target
	}
}

// workflowRunByNodeID resolves the "WFR_"-prefixed global id the REST run
// shape serves to the live workflow record (the review path mutates it).
func (s *Resolver) workflowRunByNodeID(nodeID string) *store.Workflow {
	id, ok := strings.CutPrefix(nodeID, "WFR_")
	if !ok || id == "" {
		return nil
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return s.store.Workflows[id]
}

func (s *Resolver) addDeploymentsMutationsToSchema(mutationType *graphql.Object) {
	dateTime := s.graphQLStringScalar("DateTime")
	deploymentType := s.graphqlTypes.deployment
	environmentType := s.graphqlTypes.environment
	deploymentStatusState := s.sharedEnum("DeploymentStatusState",
		"ERROR", "FAILURE", "INACTIVE", "IN_PROGRESS", "PENDING", "QUEUED", "SUCCESS", "WAITING")

	pinnedEnvironmentType := graphql.NewObject(graphql.ObjectConfig{
		Name: "PinnedEnvironment",
		Fields: graphql.Fields{
			"createdAt":   &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"databaseId":  &graphql.Field{Type: graphql.Int},
			"environment": &graphql.Field{Type: graphql.NewNonNull(environmentType)},
			"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"position":    &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"repository":  &graphql.Field{Type: graphql.NewNonNull(s.graphqlTypes.repository)},
		},
	})
	s.graphqlTypes.pinnedEnvironment = pinnedEnvironmentType

	// --- deployment reviews ------------------------------------------------

	reviewInput := func(name string) *graphql.InputObject {
		return s.mutationInput(name, graphql.InputObjectConfigFieldMap{
			"comment":        &graphql.InputObjectFieldConfig{Type: graphql.String, DefaultValue: ""},
			"environmentIds": gqlNonNullListOf(graphql.ID),
			"workflowRunId":  gqlNonNullID(),
		})
	}
	resolveReview := func(state string) graphql.FieldResolveFn {
		return func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID := str(input["workflowRunId"])
			wf := s.workflowRunByNodeID(nodeID)
			if wf == nil {
				return nil, gqlMissingNode("WorkflowRun", nodeID)
			}
			repo := s.store.GetRepoByFullName(wf.RepoFullName)
			if repo == nil {
				return nil, gqlMissingNode("WorkflowRun", nodeID)
			}
			rawIDs, _ := input["environmentIds"].([]interface{})
			envIDs := make([]int, 0, len(rawIDs))
			envNames := map[int]string{}
			for _, raw := range rawIDs {
				envNodeID, _ := raw.(string)
				env := s.store.Deployments.GetEnvironmentByNodeID(envNodeID)
				if env == nil || env.RepoID != repo.ID {
					return nil, gqlMissingNode("Environment", envNodeID)
				}
				envIDs = append(envIDs, env.ID)
				envNames[env.ID] = env.Name
			}
			if len(envIDs) == 0 {
				return nil, fmt.Errorf("environmentIds is required")
			}
			// The same not-pending refusal the REST route answers with 422.
			s.store.Mu.RLock()
			pendingByID := map[int]bool{}
			for _, pending := range wf.PendingDeployments {
				pendingByID[pending.EnvID] = true
			}
			s.store.Mu.RUnlock()
			for _, id := range envIDs {
				if !pendingByID[id] {
					return nil, fmt.Errorf("environment %d has no pending deployment for this run", id)
				}
			}
			reviewer := s.ghUserFromContext(p.Context)
			names, err := s.repos.ReviewPendingDeployments(p.Context, wf, envIDs, state, str(input["comment"]), reviewer)
			if err != nil {
				return nil, err
			}
			// Approval creates the deployments the released jobs target,
			// exactly as the REST route's response returns them.
			deployments := []interface{}{}
			if state == "approved" {
				repoSource := repoToGraphQL(s.store, s.store.SnapRepo(repo))
				for _, name := range names {
					d := s.store.Deployments.CreateDeployment(repo.ID, reviewer.ID, wf.Ref, wf.Sha, "deploy", name, "", nil, false, false)
					deployments = append(deployments, s.deploymentSource(repo, d, repoSource))
				}
			}
			return map[string]interface{}{"deployments": deployments}, nil
		}
	}

	s.registerMutation(mutationType, "approveDeployments", &graphql.Field{
		Type: s.mutationPayload("ApproveDeploymentsPayload", graphql.Fields{
			"deployments": gqlFieldListOf(deploymentType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(reviewInput("ApproveDeploymentsInput")),
		}},
		Resolve: resolveReview("approved"),
	})

	s.registerMutation(mutationType, "rejectDeployments", &graphql.Field{
		Type: s.mutationPayload("RejectDeploymentsPayload", graphql.Fields{
			"deployments": gqlFieldListOf(deploymentType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(reviewInput("RejectDeploymentsInput")),
		}},
		Resolve: resolveReview("rejected"),
	})

	// --- deployments -------------------------------------------------------

	s.registerMutation(mutationType, "createDeployment", &graphql.Field{
		Type: s.mutationPayload("CreateDeploymentPayload", graphql.Fields{
			"autoMerged": gqlField(graphql.Boolean),
			"deployment": gqlField(deploymentType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("CreateDeploymentInput", graphql.InputObjectConfigFieldMap{
				"autoMerge":        &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: true},
				"description":      &graphql.InputObjectFieldConfig{Type: graphql.String, DefaultValue: ""},
				"environment":      &graphql.InputObjectFieldConfig{Type: graphql.String, DefaultValue: "production"},
				"payload":          &graphql.InputObjectFieldConfig{Type: graphql.String, DefaultValue: "{}"},
				"refId":            gqlNonNullID(),
				"repositoryId":     gqlNonNullID(),
				"requiredContexts": gqlListOf(graphql.String),
				"task":             &graphql.InputObjectFieldConfig{Type: graphql.String, DefaultValue: "deploy"},
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			repo := store.FindRepoByNodeID(s.store, str(input["repositoryId"]))
			if repo == nil {
				return nil, gqlMissingNode("Repository", str(input["repositoryId"]))
			}
			refNodeID := str(input["refId"])
			prefix, refRepoID, qualified, ok := store.ParseGitObjectNodeID(refNodeID)
			if !ok || prefix != store.GitRefNodeIDPrefix || refRepoID != repo.ID {
				return nil, gqlMissingNode("Ref", refNodeID)
			}
			owner, name, ok := store.SplitRepoFullName(repo.FullName)
			if !ok {
				return nil, gqlMissingNode("Repository", str(input["repositoryId"]))
			}
			stor := s.store.GetGitStorage(owner, name)
			if stor == nil {
				return nil, fmt.Errorf("the repository has no git storage")
			}
			ref, err := stor.Reference(plumbing.ReferenceName(qualified))
			if err != nil {
				return nil, gqlMissingNode("Ref", refNodeID)
			}
			var payload map[string]interface{}
			if raw, ok := gqlInputString(input, "payload"); ok && raw != "" && raw != "{}" {
				if err := json.Unmarshal([]byte(raw), &payload); err != nil {
					return nil, fmt.Errorf("payload is not valid JSON: %v", err)
				}
			}
			environment := str(input["environment"])
			if environment == "" {
				environment = "production"
			}
			shortRef := strings.TrimPrefix(strings.TrimPrefix(qualified, "refs/heads/"), "refs/tags/")
			s.store.Deployments.UpsertEnvironment(repo.ID, environment)
			d := s.store.Deployments.CreateDeployment(repo.ID, user.ID, shortRef, ref.Hash().String(),
				str(input["task"]), environment, str(input["description"]), payload, false, false)
			s.events.EmitDeploymentEvent(repo, d, user, "created")
			repoSource := repoToGraphQL(s.store, s.store.SnapRepo(repo))
			return map[string]interface{}{
				// Bleephub's deployment path never merges the default branch
				// into the ref (the REST route ignores auto_merge the same
				// way), so the answer is honestly false.
				"autoMerged": false,
				"deployment": s.deploymentSource(repo, d, repoSource),
			}, nil
		},
	})

	s.registerMutation(mutationType, "createDeploymentStatus", &graphql.Field{
		Type: s.mutationPayload("CreateDeploymentStatusPayload", graphql.Fields{
			"deploymentStatus": gqlField(s.graphqlTypes.deploymentStatus),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("CreateDeploymentStatusInput", graphql.InputObjectConfigFieldMap{
				"autoInactive":   &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: true},
				"deploymentId":   gqlNonNullID(),
				"description":    &graphql.InputObjectFieldConfig{Type: graphql.String, DefaultValue: ""},
				"environment":    gqlString(),
				"environmentUrl": &graphql.InputObjectFieldConfig{Type: graphql.String, DefaultValue: ""},
				"logUrl":         &graphql.InputObjectFieldConfig{Type: graphql.String, DefaultValue: ""},
				"state":          gqlNonNullInputOf(deploymentStatusState),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID := str(input["deploymentId"])
			d := s.store.Deployments.GetDeploymentByNodeID(nodeID)
			if d == nil {
				return nil, gqlMissingNode("Deployment", nodeID)
			}
			repo := s.store.GetRepoByID(d.RepoID)
			if repo == nil {
				return nil, gqlMissingNode("Deployment", nodeID)
			}
			environment := str(input["environment"])
			if environment == "" {
				environment = d.Environment
			}
			autoInactive := true
			if flag, ok := gqlInputBool(input, "autoInactive"); ok {
				autoInactive = flag
			}
			status, autoInactivated := s.store.Deployments.AddStatus(d.ID, user.ID,
				strings.ToLower(str(input["state"])), str(input["description"]), "",
				str(input["logUrl"]), str(input["environmentUrl"]), environment, autoInactive)
			if status == nil {
				return nil, gqlMissingNode("Deployment", nodeID)
			}
			s.events.EmitDeploymentStatusEvent(repo, d, status, user)
			for _, inactive := range autoInactivated {
				priorDep := s.store.Deployments.GetDeployment(inactive.DeploymentID)
				if priorDep == nil {
					continue
				}
				priorRepo := s.store.GetRepoByID(priorDep.RepoID)
				if priorRepo == nil {
					continue
				}
				s.events.EmitDeploymentStatusEvent(priorRepo, priorDep, inactive.Status, user)
			}
			repoSource := repoToGraphQL(s.store, s.store.SnapRepo(repo))
			deploymentSource := s.deploymentSource(repo, s.store.Deployments.GetDeployment(d.ID), repoSource)
			return map[string]interface{}{
				"deploymentStatus": s.deploymentStatusSource(status, deploymentSource),
			}, nil
		},
	})

	s.registerMutation(mutationType, "deleteDeployment", &graphql.Field{
		Type: s.mutationPayload("DeleteDeploymentPayload", graphql.Fields{}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("DeleteDeploymentInput", graphql.InputObjectConfigFieldMap{
				"id": gqlNonNullID(),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID := str(input["id"])
			d := s.store.Deployments.GetDeploymentByNodeID(nodeID)
			if d == nil {
				return nil, gqlMissingNode("Deployment", nodeID)
			}
			s.store.Deployments.DeleteDeployment(d.ID)
			return map[string]interface{}{}, nil
		},
	})

	// --- environments ------------------------------------------------------

	s.registerMutation(mutationType, "createEnvironment", &graphql.Field{
		Type: s.mutationPayload("CreateEnvironmentPayload", graphql.Fields{
			"environment": gqlField(environmentType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("CreateEnvironmentInput", graphql.InputObjectConfigFieldMap{
				"name":         gqlNonNullString(),
				"repositoryId": gqlNonNullID(),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			repo := store.FindRepoByNodeID(s.store, str(input["repositoryId"]))
			if repo == nil {
				return nil, gqlMissingNode("Repository", str(input["repositoryId"]))
			}
			name := str(input["name"])
			if name == "" {
				return nil, fmt.Errorf("name is required")
			}
			env := s.store.Deployments.UpsertEnvironment(repo.ID, name)
			return map[string]interface{}{
				"environment": s.environmentSource(repo, env, repoToGraphQL(s.store, s.store.SnapRepo(repo))),
			}, nil
		},
	})

	s.registerMutation(mutationType, "updateEnvironment", &graphql.Field{
		Type: s.mutationPayload("UpdateEnvironmentPayload", graphql.Fields{
			"environment": gqlField(environmentType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateEnvironmentInput", graphql.InputObjectConfigFieldMap{
				"environmentId":     gqlNonNullID(),
				"preventSelfReview": gqlBool(),
				"reviewers":         gqlListOf(graphql.ID),
				"waitTimer":         gqlInt(),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			env, repo, err := s.environmentSubject(input, "environmentId")
			if err != nil {
				return nil, err
			}
			var waitTimer *int
			if timer, ok := gqlInputInt(input, "waitTimer"); ok {
				waitTimer = &timer
			}
			var reviewers []map[string]interface{}
			if raw, ok := input["reviewers"].([]interface{}); ok {
				reviewers = []map[string]interface{}{}
				for _, entry := range raw {
					reviewerNodeID, _ := entry.(string)
					if user := store.FindUserByNodeID(s.store, reviewerNodeID); user != nil {
						reviewers = append(reviewers, map[string]interface{}{"type": "User", "id": user.ID})
						continue
					}
					if team := s.teamByNodeID(reviewerNodeID); team != nil {
						reviewers = append(reviewers, map[string]interface{}{"type": "Team", "id": team.ID})
						continue
					}
					return nil, gqlMissingNode("DeploymentReviewer", reviewerNodeID)
				}
			}
			if waitTimer != nil || reviewers != nil {
				s.store.Deployments.SetEnvironmentProtection(repo.ID, env.Name, waitTimer, reviewers)
			}
			if prevent, ok := gqlInputBool(input, "preventSelfReview"); ok {
				s.store.Deployments.SetEnvironmentPreventSelfReview(repo.ID, env.Name, prevent)
			}
			return map[string]interface{}{
				"environment": s.environmentSource(repo, s.store.Deployments.GetEnvironment(repo.ID, env.Name),
					repoToGraphQL(s.store, s.store.SnapRepo(repo))),
			}, nil
		},
	})

	s.registerMutation(mutationType, "deleteEnvironment", &graphql.Field{
		Type: s.mutationPayload("DeleteEnvironmentPayload", graphql.Fields{}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("DeleteEnvironmentInput", graphql.InputObjectConfigFieldMap{
				"id": gqlNonNullID(),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			env, repo, err := s.environmentSubject(input, "id")
			if err != nil {
				return nil, err
			}
			if !s.store.Deployments.DeleteEnvironment(repo.ID, env.Name) {
				return nil, gqlMissingNode("Environment", env.NodeID)
			}
			// The branch policies configured on the environment go with it,
			// exactly as DELETE /environments/{name} prunes them.
			s.store.PruneEnvironmentPolicies(env.ID)
			return map[string]interface{}{}, nil
		},
	})

	s.registerMutation(mutationType, "pinEnvironment", &graphql.Field{
		Type: s.mutationPayload("PinEnvironmentPayload", graphql.Fields{
			"environment":       gqlField(environmentType),
			"pinnedEnvironment": gqlField(pinnedEnvironmentType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("PinEnvironmentInput", graphql.InputObjectConfigFieldMap{
				"environmentId": gqlNonNullID(),
				"pinned":        gqlNonNullBool(),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			env, repo, err := s.environmentSubject(input, "environmentId")
			if err != nil {
				return nil, err
			}
			pinned, _ := input["pinned"].(bool)
			var pinSource interface{}
			repoSource := repoToGraphQL(s.store, s.store.SnapRepo(repo))
			if pinned {
				pin := s.store.Deployments.PinEnvironment(repo.ID, env.ID, s.store.CurrentTime())
				pinSource = s.pinnedEnvironmentSource(repo, env, pin, repoSource)
			} else {
				s.store.Deployments.UnpinEnvironment(repo.ID, env.ID, s.store.CurrentTime())
			}
			return map[string]interface{}{
				"environment":       s.environmentSource(repo, env, repoSource),
				"pinnedEnvironment": pinSource,
			}, nil
		},
	})

	s.registerMutation(mutationType, "reorderEnvironment", &graphql.Field{
		Type: s.mutationPayload("ReorderEnvironmentPayload", graphql.Fields{
			"environment": gqlField(environmentType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("ReorderEnvironmentInput", graphql.InputObjectConfigFieldMap{
				"environmentId": gqlNonNullID(),
				"position":      gqlNonNullInt(),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			env, repo, err := s.environmentSubject(input, "environmentId")
			if err != nil {
				return nil, err
			}
			position, _ := gqlInputInt(input, "position")
			if !s.store.Deployments.ReorderPinnedEnvironment(repo.ID, env.ID, position, s.store.CurrentTime()) {
				return nil, fmt.Errorf("environment %q is not pinned; only pinned environments can be reordered", env.Name)
			}
			return map[string]interface{}{
				"environment": s.environmentSource(repo, env, repoToGraphQL(s.store, s.store.SnapRepo(repo))),
			}, nil
		},
	})
}

// environmentSubject resolves an Environment input id to the environment and
// its repository, or the missing-node refusal.
func (s *Resolver) environmentSubject(input map[string]interface{}, key string) (*store.Environment, *store.Repo, error) {
	nodeID, _ := input[key].(string)
	env := s.store.Deployments.GetEnvironmentByNodeID(nodeID)
	if env == nil {
		return nil, nil, gqlMissingNode("Environment", nodeID)
	}
	repo := s.store.GetRepoByID(env.RepoID)
	if repo == nil {
		return nil, nil, gqlMissingNode("Environment", nodeID)
	}
	return env, repo, nil
}

// pinnedEnvironmentSource renders one pin with the environment and repository
// it holds.
func (s *Resolver) pinnedEnvironmentSource(repo *store.Repo, env *store.Environment, pin *store.PinnedEnvironment, repoSource map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"id":          pin.NodeID,
		"databaseId":  pin.ID,
		"position":    pin.Position,
		"createdAt":   pin.CreatedAt.UTC().Format(rfc3339),
		"environment": s.environmentSource(repo, env, repoSource),
		"repository":  repoSource,
	}
}

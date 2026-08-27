package graphqlapi

import (
	"context"
	"fmt"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// --- ProjectV2 mutation authorization ---
//
// A project belongs to a user or organization, so the entitlement is the
// owner-scoped pair the REST surface enforces: canReadProjectV2 for visibility,
// canWriteProjectV2 for change.

// projectMutationTarget is what a ProjectV2 mutation acts on: the owning account
// (whose membership decides write access) and the project itself when the input
// names one rather than creating it.
type projectMutationTarget struct {
	owner   *store.ProjectV2Owner
	project *store.ProjectV2
	// missing answers both "no such project" and "you may not see it".
	missing error
}

// projectRule is the policy for a mutation whose subject belongs to a project.
// Projects v2 has one write capability, held by the owning user or any active
// member of the owning organization.
type projectRule struct {
	target func(s *Resolver, input map[string]interface{}) projectMutationTarget
}

func (r projectRule) check() error {
	if r.target == nil {
		return fmt.Errorf("no project target lookup")
	}
	return nil
}

func (r projectRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	target := r.target(s, input)
	if target.owner == nil {
		return target.missing
	}
	user := s.ghUserFromContext(p.Context)
	if target.project != nil && !s.canReadProjectV2(p.Context, user, target.owner, target.project) {
		return target.missing
	}
	if !s.canWriteProjectV2(p.Context, user, target.owner) {
		return fmt.Errorf("must have write access to the project")
	}
	return nil
}

// projectTargetOwner reads the owner from the input, for createProjectV2 which
// has no project yet: the entitlement is over the account the project would
// belong to.
func projectTargetOwner(key string) func(*Resolver, map[string]interface{}) projectMutationTarget {
	return func(s *Resolver, input map[string]interface{}) projectMutationTarget {
		nodeID, _ := input[key].(string)
		target := projectMutationTarget{missing: &ghNotFoundError{
			message: fmt.Sprintf("Could not resolve to an owner with the global id of '%s'.", nodeID),
		}}
		ownerID, ownerType, ok := resolveProjectOwner(s.store, nodeID)
		if !ok {
			return target
		}
		target.owner = s.projectV2OwnerByID(ownerID, ownerType)
		return target
	}
}

// projectTargetProject resolves the project the input names and its owner.
func projectTargetProject(key string) func(*Resolver, map[string]interface{}) projectMutationTarget {
	return func(s *Resolver, input map[string]interface{}) projectMutationTarget {
		nodeID, _ := input[key].(string)
		target := projectMutationTarget{missing: &ghNotFoundError{
			message: fmt.Sprintf("Could not resolve to a project with the global id of '%s'.", nodeID),
		}}
		proj := s.store.ProjectsV2.LookupProjectByNodeID(nodeID)
		if proj == nil {
			return target
		}
		target.project = proj
		target.owner = s.projectV2OwnerByID(proj.OwnerID, proj.OwnerType)
		return target
	}
}

// projectTargetItem, projectTargetField, projectTargetView,
// projectTargetStatusUpdate and projectTargetWorkflow resolve a mutation keyed
// on a subject inside a project. Each walks from the subject to its project so
// the entitlement stays the owner-scoped write on that project.
func projectTargetItem(key string) func(*Resolver, map[string]interface{}) projectMutationTarget {
	return projectTargetVia(key, "node", func(s *Resolver, nodeID string) (int, bool) {
		if item := s.store.ProjectsV2.LookupItemByNodeID(nodeID); item != nil {
			return item.ProjectID, true
		}
		return 0, false
	})
}

func projectTargetField(key string) func(*Resolver, map[string]interface{}) projectMutationTarget {
	return projectTargetVia(key, "field", func(s *Resolver, nodeID string) (int, bool) {
		if field := s.store.ProjectsV2.LookupFieldByNodeID(nodeID); field != nil {
			return field.ProjectID, true
		}
		return 0, false
	})
}

func projectTargetView(key string) func(*Resolver, map[string]interface{}) projectMutationTarget {
	return projectTargetVia(key, "view", func(s *Resolver, nodeID string) (int, bool) {
		if view := s.store.ProjectsV2.LookupViewByNodeID(nodeID); view != nil {
			return view.ProjectID, true
		}
		return 0, false
	})
}

func projectTargetStatusUpdate(key string) func(*Resolver, map[string]interface{}) projectMutationTarget {
	return projectTargetVia(key, "status update", func(s *Resolver, nodeID string) (int, bool) {
		if update := s.store.ProjectsV2.LookupStatusUpdateByNodeID(nodeID); update != nil {
			return update.ProjectID, true
		}
		return 0, false
	})
}

func projectTargetWorkflow(key string) func(*Resolver, map[string]interface{}) projectMutationTarget {
	return projectTargetVia(key, "workflow", func(s *Resolver, nodeID string) (int, bool) {
		if workflow := s.store.ProjectsV2.LookupWorkflowByNodeID(nodeID); workflow != nil {
			return workflow.ProjectID, true
		}
		return 0, false
	})
}

// projectTargetVia builds a target lookup for a subject that resolves to a
// project id. subject names the thing in the refusal message.
func projectTargetVia(key, subject string, projectIDOf func(*Resolver, string) (int, bool)) func(*Resolver, map[string]interface{}) projectMutationTarget {
	return func(s *Resolver, input map[string]interface{}) projectMutationTarget {
		nodeID, _ := input[key].(string)
		target := projectMutationTarget{missing: &ghNotFoundError{
			message: fmt.Sprintf("Could not resolve to a %s with the global id of '%s'.", subject, nodeID),
		}}
		projectID, ok := projectIDOf(s, nodeID)
		if !ok {
			return target
		}
		proj := s.store.ProjectsV2.GetProject(projectID)
		if proj == nil {
			return target
		}
		target.project = proj
		target.owner = s.projectV2OwnerByID(proj.OwnerID, proj.OwnerType)
		return target
	}
}

// projectV2OwnerByID builds the owner record the access predicates take from a
// project's stored owner. An owner that no longer resolves yields nil, so the
// caller denies.
func (s *Resolver) projectV2OwnerByID(ownerID int, ownerType string) *store.ProjectV2Owner {
	if ownerType == "Organization" {
		org := s.store.GetOrgByID(ownerID)
		if org == nil {
			return nil
		}
		return &store.ProjectV2Owner{ID: org.ID, OwnerType: "Organization", Login: org.Login, Org: org}
	}
	u := s.store.GetUserByID(ownerID)
	if u == nil {
		return nil
	}
	return &store.ProjectV2Owner{ID: u.ID, OwnerType: "User", Login: u.Login, User: u}
}

// viewerCanReadProjectContent reports whether the request may read the issue or
// pull request a project item would point at. Adding content republishes its
// title and state to everyone who can see the project, so a caller who cannot
// read the content cannot add it.
func (s *Resolver) viewerCanReadProjectContent(ctx context.Context, contentType string, contentID int) bool {
	repoID := 0
	switch contentType {
	case "Issue":
		if issue := s.store.GetIssue(contentID); issue != nil {
			repoID = issue.RepoID
		}
	case "PullRequest":
		if pr := s.store.GetPullRequest(contentID); pr != nil {
			repoID = pr.RepoID
		}
	}
	repo := s.store.GetRepoByID(repoID)
	return repo != nil && s.viewerCanReadRepo(ctx, repo)
}

// addProjectV2MutationsToSchema registers the ProjectV2 create/add/field/value
// mutations `gh project` uses.
func (s *Resolver) addProjectV2MutationsToSchema(mutationType *graphql.Object) {
	projectV2Type := s.projectV2GraphQLTypes()

	// createProjectV2
	createProjectInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreateProjectV2Input",
		Fields: graphql.InputObjectConfigFieldMap{
			"ownerId":      &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"title":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"repositoryId": &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"teamId":       &graphql.InputObjectFieldConfig{Type: graphql.ID},
		},
	})
	createProjectPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "CreateProjectV2Payload",
		Fields: graphql.Fields{
			"projectV2": &graphql.Field{Type: projectV2Type},
		},
	})

	s.registerMutation(mutationType, "createProjectV2", &graphql.Field{
		Type: createProjectPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(createProjectInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			ownerNodeID, _ := input["ownerId"].(string)
			title, _ := input["title"].(string)

			ownerID, ownerType, ok := resolveProjectOwner(s.store, ownerNodeID)
			if !ok {
				return nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to an owner with the global id of '%s'.", ownerNodeID)}
			}
			if err := s.enterpriseOrganizationProjectsRefusal(p, ownerType, ownerID); err != nil {
				return nil, err
			}
			proj := s.store.ProjectsV2.CreateProject(ownerID, ownerType, title, user.ID)

			// An optional repositoryId links a readable repository; an
			// unresolvable one is ignored rather than failing the create.
			if repoNodeID, _ := input["repositoryId"].(string); repoNodeID != "" {
				if repo := store.FindRepoByNodeID(s.store, repoNodeID); repo != nil && s.viewerCanReadRepo(p.Context, repo) {
					if linked := s.store.ProjectsV2.LinkRepository(proj.ID, repo.ID); linked != nil {
						proj = linked
					}
				}
			}
			// An optional teamId grants a team in the owning organization
			// read access.
			if teamNodeID, _ := input["teamId"].(string); teamNodeID != "" {
				if team := s.projectV2TeamByNodeID(teamNodeID); team != nil &&
					proj.OwnerType == "Organization" && team.OrgID == proj.OwnerID {
					if linked := s.store.ProjectsV2.LinkTeam(proj.ID, team.ID); linked != nil {
						proj = linked
					}
				}
			}

			s.emitProjectV2Event(s.projectV2Event(p, store.ProjectV2EventProject, "created", proj))
			return map[string]interface{}{
				"projectV2": projectV2ToGQL(s.store, proj),
			}, nil
		},
	})

	// addProjectV2ItemById
	addItemInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "AddProjectV2ItemByIdInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"projectId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"contentId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	addItemPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "AddProjectV2ItemByIdPayload",
		Fields: graphql.Fields{
			"item": &graphql.Field{Type: s.projectV2ItemType()},
		},
	})

	s.registerMutation(mutationType, "addProjectV2ItemById", &graphql.Field{
		Type: addItemPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(addItemInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			projectNodeID, _ := input["projectId"].(string)
			contentNodeID, _ := input["contentId"].(string)

			proj := s.store.ProjectsV2.LookupProjectByNodeID(projectNodeID)
			if proj == nil {
				return nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a project with the global id of '%s'.", projectNodeID)}
			}
			// Write access to the project is not read access to its content;
			// unreadable content answers as if absent.
			contentType, contentID, ok := resolveContentByNodeID(s.store, contentNodeID)
			if !ok || !s.viewerCanReadProjectContent(p.Context, contentType, contentID) {
				return nil, &ghNotFoundError{
					message: fmt.Sprintf("Could not resolve to an issue or pull request with the global id of '%s'.", contentNodeID),
				}
			}
			item := s.store.ProjectsV2.AddItem(proj.ID, contentType, contentID, user.ID)
			if item == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to a project."}
			}
			event := s.projectV2Event(p, store.ProjectV2EventItem, "created", proj)
			event.Item = item
			s.emitProjectV2Event(event)
			return map[string]interface{}{
				"item": projectV2ItemToGQL(item, s.store),
			}, nil
		},
	})

	// deleteProjectV2Item
	deleteItemInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "DeleteProjectV2ItemInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"projectId":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"itemId":           &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})
	deleteItemPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DeleteProjectV2ItemPayload",
		Fields: graphql.Fields{
			"clientMutationId": &graphql.Field{Type: graphql.String},
			"deletedItemId":    &graphql.Field{Type: graphql.ID},
		},
	})

	s.registerMutation(mutationType, "deleteProjectV2Item", &graphql.Field{
		Type: deleteItemPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(deleteItemInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			clientMutationID, _ := input["clientMutationId"].(string)
			projectNodeID, _ := input["projectId"].(string)
			itemNodeID, _ := input["itemId"].(string)

			proj := s.store.ProjectsV2.LookupProjectByNodeID(projectNodeID)
			if proj == nil {
				return nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a project with the global id of '%s'.", projectNodeID)}
			}
			item := s.store.ProjectsV2.LookupItemByNodeID(itemNodeID)
			// An item outside this project is not this project's item;
			// answer as if absent.
			if item == nil || item.ProjectID != proj.ID {
				return nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a node with the global id of '%s'.", itemNodeID)}
			}
			s.store.ProjectsV2.DeleteItem(item.ID)
			event := s.projectV2Event(p, store.ProjectV2EventItem, "deleted", proj)
			event.Item = item
			s.emitProjectV2Event(event)
			return map[string]interface{}{
				"clientMutationId": clientMutationID,
				"deletedItemId":    item.NodeID,
			}, nil
		},
	})

	// --- createProjectV2Field ---

	dataTypeEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "ProjectV2CustomFieldType",
		Values: graphql.EnumValueConfigMap{
			"SINGLE_SELECT": &graphql.EnumValueConfig{Value: "SINGLE_SELECT"},
			"MULTI_SELECT":  &graphql.EnumValueConfig{Value: "MULTI_SELECT"},
			"TEXT":          &graphql.EnumValueConfig{Value: "TEXT"},
			"NUMBER":        &graphql.EnumValueConfig{Value: "NUMBER"},
			"DATE":          &graphql.EnumValueConfig{Value: "DATE"},
			"ITERATION":     &graphql.EnumValueConfig{Value: "ITERATION"},
		},
	})

	// Reused by updateProjectV2Field.
	singleSelectOptionInputType := s.projectV2SingleSelectOptionInput()
	dateScalar := s.graphQLStringScalar("Date")
	iterationConfigInputType := s.projectV2IterationConfigurationInput()

	createFieldInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreateProjectV2FieldInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"projectId":              &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"dataType":               &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(dataTypeEnum)},
			"name":                   &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"singleSelectOptions":    &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(singleSelectOptionInputType))},
			"multiSelectOptions":     &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(s.projectV2MultiSelectOptionInput()))},
			"iterationConfiguration": &graphql.InputObjectFieldConfig{Type: iterationConfigInputType},
		},
	})

	createFieldPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "CreateProjectV2FieldPayload",
		Fields: graphql.Fields{
			"projectV2Field": &graphql.Field{Type: s.projectV2FieldConfigurationUnion()},
		},
	})

	s.registerMutation(mutationType, "createProjectV2Field", &graphql.Field{
		Type: createFieldPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(createFieldInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			projectNodeID, _ := input["projectId"].(string)
			name, _ := input["name"].(string)
			dataType, _ := input["dataType"].(string)
			rawOptions, _ := input["singleSelectOptions"].([]interface{})
			if multi, ok := input["multiSelectOptions"].([]interface{}); ok && len(multi) > 0 {
				rawOptions = multi
			}
			rawIteration, _ := input["iterationConfiguration"].(map[string]interface{})
			// The option input carries colour and description too; reading
			// only the name discarded both.
			options := projectV2OptionsFromInput(rawOptions)
			iteration, err := projectV2IterationFromInput(rawIteration)
			if err != nil {
				return nil, err
			}

			proj := s.store.ProjectsV2.LookupProjectByNodeID(projectNodeID)
			if proj == nil {
				return nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a project with the global id of '%s'.", projectNodeID)}
			}
			if dataType == string(store.ProjectV2FieldIteration) && iteration == nil {
				return nil, fmt.Errorf("iterationConfiguration is required for ITERATION fields")
			}
			field := s.store.ProjectsV2.CreateField(proj.ID, name, store.ProjectV2FieldDataType(dataType), options, iteration)
			if field == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to a project."}
			}
			s.projectV2EmitProjectEdited(p, proj.ID)
			return map[string]interface{}{
				"projectV2Field": projectV2FieldToGQL(field),
			}, nil
		},
	})

	// --- updateProjectV2ItemFieldValue ---

	fieldValueInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "ProjectV2FieldValue",
		Fields: graphql.InputObjectConfigFieldMap{
			"singleSelectOptionId": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"multiSelectOptionIds": &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"text":                 &graphql.InputObjectFieldConfig{Type: graphql.String},
			"number":               &graphql.InputObjectFieldConfig{Type: graphql.Float},
			"date":                 &graphql.InputObjectFieldConfig{Type: dateScalar},
			"iterationId":          &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})
	updateValueInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UpdateProjectV2ItemFieldValueInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"projectId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"itemId":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"fieldId":   &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"value":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(fieldValueInputType)},
		},
	})

	updateValuePayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "UpdateProjectV2ItemFieldValuePayload",
		Fields: graphql.Fields{
			"projectV2Item": &graphql.Field{Type: s.projectV2ItemType()},
		},
	})

	s.registerMutation(mutationType, "updateProjectV2ItemFieldValue", &graphql.Field{
		Type: updateValuePayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(updateValueInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			projectNodeID, _ := input["projectId"].(string)
			itemNodeID, _ := input["itemId"].(string)
			fieldNodeID, _ := input["fieldId"].(string)
			value, _ := input["value"].(map[string]interface{})

			proj := s.store.ProjectsV2.LookupProjectByNodeID(projectNodeID)
			if proj == nil {
				return nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a project with the global id of '%s'.", projectNodeID)}
			}
			item := s.store.ProjectsV2.LookupItemByNodeID(itemNodeID)
			if item == nil {
				return nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to an item with the global id of '%s'.", itemNodeID)}
			}
			if item.ProjectID != proj.ID {
				return nil, fmt.Errorf("project does not contain item with the global id of '%s'", itemNodeID)
			}
			field := s.store.ProjectsV2.LookupFieldByNodeID(fieldNodeID)
			if field == nil {
				return nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a field with the global id of '%s'.", fieldNodeID)}
			}
			if field.ProjectID != proj.ID {
				return nil, fmt.Errorf("field does not belong to project with the global id of '%s'", projectNodeID)
			}
			fieldValue, err := projectV2GraphQLFieldValueInput(field, value)
			if err != nil {
				return nil, err
			}
			// A multi-select value is an ordered set, so it takes the store's
			// list writer.
			if optionIDs, ok := fieldValue.([]string); ok {
				if err := s.store.ProjectsV2.SetMultiSelectValue(item.ID, field.ID, optionIDs); err != nil {
					return nil, err
				}
			} else if err := s.store.ProjectsV2.SetFieldValueAny(item.ID, field.ID, fieldValue); err != nil {
				return nil, err
			}
			// Re-read after the write so the payload carries the new value.
			updated := s.store.ProjectsV2.GetItem(item.ID)
			if updated == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to an item."}
			}
			event := s.projectV2Event(p, store.ProjectV2EventItem, "edited", proj)
			event.Item = updated
			s.emitProjectV2Event(event)
			return map[string]interface{}{
				"projectV2Item": projectV2ItemToGQL(updated, s.store),
			}, nil
		},
	})

	s.addProjectV2RemainingMutations(mutationType)
}

func projectV2GraphQLFieldValueInput(field *store.ProjectV2Field, value map[string]interface{}) (interface{}, error) {
	if value == nil {
		return nil, fmt.Errorf("value is required")
	}
	type candidate struct {
		name  string
		value interface{}
	}
	candidates := make([]candidate, 0, 6)
	for _, key := range []string{"singleSelectOptionId", "multiSelectOptionIds", "text", "number", "date", "iterationId"} {
		v, ok := value[key]
		if !ok || v == nil {
			continue
		}
		candidates = append(candidates, candidate{name: key, value: v})
	}
	if len(candidates) != 1 {
		return nil, fmt.Errorf("exactly one field value must be provided")
	}
	got := candidates[0]
	want := map[store.ProjectV2FieldDataType]string{
		store.ProjectV2FieldSingleSelect: "singleSelectOptionId",
		store.ProjectV2FieldMultiSelect:  "multiSelectOptionIds",
		store.ProjectV2FieldText:         "text",
		store.ProjectV2FieldNumber:       "number",
		store.ProjectV2FieldDate:         "date",
		store.ProjectV2FieldIteration:    "iterationId",
	}[field.DataType]
	if got.name != want {
		return nil, fmt.Errorf("field %q expects %s", field.Name, want)
	}
	if field.DataType == store.ProjectV2FieldMultiSelect {
		raw, ok := got.value.([]interface{})
		if !ok {
			return nil, fmt.Errorf("field %q expects a list of option ids", field.Name)
		}
		optionIDs := make([]string, 0, len(raw))
		for _, entry := range raw {
			id, ok := entry.(string)
			if !ok {
				return nil, fmt.Errorf("field %q expects a list of option ids", field.Name)
			}
			optionIDs = append(optionIDs, id)
		}
		return optionIDs, nil
	}
	if field.DataType == store.ProjectV2FieldNumber {
		switch n := got.value.(type) {
		case float64:
			return n, nil
		case float32:
			return float64(n), nil
		case int:
			return float64(n), nil
		case int32:
			return float64(n), nil
		case int64:
			return float64(n), nil
		}
	}
	return got.value, nil
}

// resolveProjectOwner maps a node ID to (ownerID, ownerType) for User and
// Organization nodes.
func resolveProjectOwner(st *store.Store, nodeID string) (int, string, bool) {
	if nodeID == "" {
		return 0, "", false
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, u := range st.Users {
		if u.NodeID == nodeID {
			return u.ID, "User", true
		}
	}
	for _, org := range st.Orgs {
		if org.NodeID == nodeID {
			return org.ID, "Organization", true
		}
	}
	return 0, "", false
}

// resolveContentByNodeID maps a node ID to an Issue or PullRequest.
func resolveContentByNodeID(st *store.Store, nodeID string) (string, int, bool) {
	if issue := store.FindIssueByNodeID(st, nodeID); issue != nil {
		return "Issue", issue.ID, true
	}
	if pr := store.FindPullRequestByNodeID(st, nodeID); pr != nil {
		return "PullRequest", pr.ID, true
	}
	return "", 0, false
}

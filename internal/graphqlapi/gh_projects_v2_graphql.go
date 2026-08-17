package graphqlapi

import (
	"context"
	"fmt"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// --- ProjectV2 mutation authorization ---
//
// A project belongs to a user or an organization, so none of the repository
// lookups apply: the entitlement is the owner-scoped pair the REST surface
// already enforces — canReadProjectV2 for visibility, canWriteProjectV2 for
// change. Reusing them keeps the two surfaces from drifting apart, which is how
// GraphQL became a way around REST in the first place.

// projectMutationTarget is what a ProjectV2 mutation acts on: the account whose
// membership decides write access, and the project itself when the input names
// one rather than creating it.
type projectMutationTarget struct {
	owner   *store.ProjectV2Owner
	project *store.ProjectV2
	// missing answers both "no such project" and "you may not see it", for the
	// same reason the repository rule keeps them indistinguishable.
	missing error
}

// projectRule is the policy for a mutation whose subject belongs to a project.
// There is no level: Projects v2 has one write capability, held by the owning
// user or by any active member of the owning organization.
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

// projectTargetOwner reads the owner straight out of the input. createProjectV2
// has no project yet, so the entitlement is over the account the project would
// belong to — without this, any signed-in account created projects under any
// user or organization on the instance.
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

// projectTargetProject resolves the project the input names and the account
// behind it.
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

// projectV2OwnerByID builds the owner record the access predicates take from a
// project's stored owner, which is how the GraphQL lane reaches them: there is
// no request path to parse. An owner that no longer resolves yields nil and the
// caller denies, rather than an owner record nobody controls.
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
// pull request a project item would point at. Adding content to a project
// republishes its title and state to everyone who can see the project, so a
// caller who cannot read the content cannot pull it in either.
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

// addProjectV2MutationsToSchema registers the ProjectV2 GraphQL mutations
// gh CLI's `gh project create` + `gh project item-add` use:
//   - createProjectV2(input{ownerId, title}) → ProjectV2
//   - addProjectV2ItemById(input{projectId, contentId}) → ProjectV2Item
//   - createProjectV2Field(input{projectId, dataType, name}) → ProjectV2FieldConfiguration
//   - updateProjectV2ItemFieldValue(input{projectId,itemId,fieldId,value}) → ProjectV2Item
func (s *Resolver) addProjectV2MutationsToSchema(mutationType *graphql.Object) {
	projectV2Type := s.projectV2GraphQLTypes()

	// createProjectV2
	createProjectInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreateProjectV2Input",
		Fields: graphql.InputObjectConfigFieldMap{
			"ownerId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"title":   &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
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
			proj := s.store.ProjectsV2.CreateProject(ownerID, ownerType, title, user.ID)
			return map[string]interface{}{
				"projectV2": projectV2ToGQL(proj),
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
			// The shared ProjectV2Item type (GitHub's payload shape); the
			// resolver feeds it a full projectV2ItemToGQL source map.
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
			// Write access to the project is not read access to what is being
			// pulled into it; content the caller cannot read answers the same
			// way content that does not exist does.
			contentType, contentID, ok := resolveContentByNodeID(s.store, contentNodeID)
			if !ok || !s.viewerCanReadProjectContent(p.Context, contentType, contentID) {
				return nil, &ghNotFoundError{
					message: fmt.Sprintf("Could not resolve to an issue or pull request with the global id of '%s'.", contentNodeID),
				}
			}
			item := s.store.ProjectsV2.AddItem(proj.ID, contentType, contentID, user.ID)
			return map[string]interface{}{
				"item": projectV2ItemToGQL(item, s.store),
			}, nil
		},
	})

	// deleteProjectV2Item — the mutation backing `gh project item-delete`.
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
			// An item id that names an item outside this project is not this
			// project's item; answer as if it does not exist.
			if item == nil || item.ProjectID != proj.ID {
				return nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a node with the global id of '%s'.", itemNodeID)}
			}
			s.store.ProjectsV2.DeleteItem(item.ID)
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

	singleSelectOptionInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "ProjectV2SingleSelectFieldOptionInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"name": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	// GitHub's input names: ProjectV2Iteration is officially an INPUT_OBJECT
	// (the object flavor is ProjectV2IterationFieldIteration), and the
	// configuration input is ProjectV2IterationFieldConfigurationInput.
	dateScalar := s.graphQLStringScalar("Date")
	iterationInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "ProjectV2Iteration",
		Fields: graphql.InputObjectConfigFieldMap{
			"title":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"startDate": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(dateScalar)},
			"duration":  &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
	iterationConfigInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "ProjectV2IterationFieldConfigurationInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"startDate":  &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(dateScalar)},
			"duration":   &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.Int)},
			"iterations": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(iterationInputType)))},
		},
	})

	createFieldInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreateProjectV2FieldInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"projectId":              &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"dataType":               &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(dataTypeEnum)},
			"name":                   &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"singleSelectOptions":    &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(singleSelectOptionInputType))},
			"iterationConfiguration": &graphql.InputObjectFieldConfig{Type: iterationConfigInputType},
		},
	})

	createFieldPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "CreateProjectV2FieldPayload",
		Fields: graphql.Fields{
			// GitHub's payload shape: the ProjectV2FieldConfiguration union;
			// the resolver feeds it a full projectV2FieldToGQL source map.
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
			rawIteration, _ := input["iterationConfiguration"].(map[string]interface{})
			options := make([]*store.ProjectV2SingleSelectOption, 0, len(rawOptions))
			for _, raw := range rawOptions {
				m, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				n, _ := m["name"].(string)
				options = append(options, &store.ProjectV2SingleSelectOption{Name: n})
			}
			var iteration *store.ProjectV2IterationConfiguration
			if rawIteration != nil {
				startDate, _ := rawIteration["startDate"].(string)
				duration, _ := rawIteration["duration"].(int)
				iteration = &store.ProjectV2IterationConfiguration{StartDate: startDate, Duration: duration}
				rawIterations, _ := rawIteration["iterations"].([]interface{})
				for _, raw := range rawIterations {
					m, ok := raw.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("iterationConfiguration.iterations contains an invalid item")
					}
					title, _ := m["title"].(string)
					start, _ := m["startDate"].(string)
					iterDuration := duration
					if d, ok := m["duration"].(int); ok && d > 0 {
						iterDuration = d
					}
					iteration.Iterations = append(iteration.Iterations, &store.ProjectV2Iteration{
						Title:     title,
						StartDate: start,
						Duration:  iterDuration,
					})
				}
			}

			proj := s.store.ProjectsV2.LookupProjectByNodeID(projectNodeID)
			if proj == nil {
				return nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a project with the global id of '%s'.", projectNodeID)}
			}
			if dataType == string(store.ProjectV2FieldIteration) && iteration == nil {
				return nil, fmt.Errorf("iterationConfiguration is required for ITERATION fields")
			}
			field := s.store.ProjectsV2.CreateField(proj.ID, name, store.ProjectV2FieldDataType(dataType), options, iteration)
			return map[string]interface{}{
				"projectV2Field": projectV2FieldToGQL(field),
			}, nil
		},
	})

	// --- updateProjectV2ItemFieldValue ---

	// GitHub's input name is ProjectV2FieldValue (with a Date-typed date).
	fieldValueInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "ProjectV2FieldValue",
		Fields: graphql.InputObjectConfigFieldMap{
			"singleSelectOptionId": &graphql.InputObjectFieldConfig{Type: graphql.String},
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
			// The shared ProjectV2Item type (GitHub's payload shape); the
			// resolver feeds it a full projectV2ItemToGQL source map.
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
			if err := s.store.ProjectsV2.SetFieldValueAny(item.ID, field.ID, fieldValue); err != nil {
				return nil, err
			}
			return map[string]interface{}{
				"projectV2Item": projectV2ItemToGQL(item, s.store),
			}, nil
		},
	})
}

func projectV2GraphQLFieldValueInput(field *store.ProjectV2Field, value map[string]interface{}) (interface{}, error) {
	if value == nil {
		return nil, fmt.Errorf("value is required")
	}
	type candidate struct {
		name  string
		value interface{}
	}
	candidates := make([]candidate, 0, 5)
	for _, key := range []string{"singleSelectOptionId", "text", "number", "date", "iterationId"} {
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
		store.ProjectV2FieldText:         "text",
		store.ProjectV2FieldNumber:       "number",
		store.ProjectV2FieldDate:         "date",
		store.ProjectV2FieldIteration:    "iterationId",
	}[field.DataType]
	if got.name != want {
		return nil, fmt.Errorf("field %q expects %s", field.Name, want)
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

// resolveProjectOwner maps a GraphQL node ID to (ownerID, ownerType).
// Supports User + Organization nodes.
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

// resolveContentByNodeID maps a GraphQL node ID to either an Issue or
// PullRequest. Returns (contentType, contentID, ok).
func resolveContentByNodeID(st *store.Store, nodeID string) (string, int, bool) {
	if issue := store.FindIssueByNodeID(st, nodeID); issue != nil {
		return "Issue", issue.ID, true
	}
	if pr := store.FindPullRequestByNodeID(st, nodeID); pr != nil {
		return "PullRequest", pr.ID, true
	}
	return "", 0, false
}

package graphqlapi

import (
	"fmt"
	"strings"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// Projects v2 — the rest of the mutation surface (the five `gh` reaches for
// first live in gh_projects_v2_graphql.go). Every mutation goes through
// registerMutation, so each has an owner-scoped graphqlMutationAuthz row.

// addProjectV2RemainingMutations runs from addProjectV2MutationsToSchema, after
// the shared project/item/field types exist.
func (s *Resolver) addProjectV2RemainingMutations(mutationType *graphql.Object) {
	projectType := s.projectV2GraphQLTypes()
	itemType := s.projectV2ItemType()
	viewType := s.projectV2ViewObjectType()
	fieldUnion := s.projectV2FieldConfigurationUnion()
	statusType := s.graphqlTypes.projectV2StatusUpdateType
	draftIssueType := s.graphqlTypes.projectV2DraftIssueType

	s.addProjectV2LifecycleMutations(mutationType, projectType)
	s.addProjectV2LinkMutations(mutationType)
	s.addProjectV2CollaboratorMutations(mutationType)
	s.addProjectV2FieldMutations(mutationType, fieldUnion)
	s.addProjectV2ViewMutations(mutationType, viewType)
	s.addProjectV2ItemMutations(mutationType, itemType, draftIssueType)
	s.addProjectV2StatusUpdateMutations(mutationType, projectType, statusType)
	s.addProjectV2WorkflowMutations(mutationType, projectType)
}

// Shared helpers

// projectV2MutationProject resolves the project a mutation names. The policy row
// already authorized the caller, so this is a lookup, not a second check.
func (s *Resolver) projectV2MutationProject(input map[string]interface{}, key string) (*store.ProjectV2, error) {
	nodeID, _ := input[key].(string)
	project := s.store.ProjectsV2.LookupProjectByNodeID(nodeID)
	if project == nil {
		return nil, &ghNotFoundError{
			message: fmt.Sprintf("Could not resolve to a project with the global id of '%s'.", nodeID),
		}
	}
	return project, nil
}

// projectV2MutationItem resolves an item and checks it belongs to the named
// project. An item in another project is answered as not-found, never confirmed.
func (s *Resolver) projectV2MutationItem(input map[string]interface{}, itemKey string, project *store.ProjectV2) (*store.ProjectV2Item, error) {
	nodeID, _ := input[itemKey].(string)
	item := s.store.ProjectsV2.LookupItemByNodeID(nodeID)
	if item == nil || (project != nil && item.ProjectID != project.ID) {
		return nil, &ghNotFoundError{
			message: fmt.Sprintf("Could not resolve to a node with the global id of '%s'.", nodeID),
		}
	}
	return item, nil
}

// projectV2Event is the pair every emitted event needs: who acted, and the
// project after the change.
func (s *Resolver) projectV2Event(p graphql.ResolveParams, event, action string, project *store.ProjectV2) store.ProjectV2Event {
	return store.ProjectV2Event{
		Event:   event,
		Action:  action,
		Project: project,
		Sender:  s.ghUserFromContext(p.Context),
	}
}

// stringChange records a before/after pair only when the value moved; `edited`
// payloads carry only changed fields.
func stringChange(changes map[string]store.ProjectV2Change, name, before, after string) {
	if before != after {
		changes[name] = store.ProjectV2Change{From: nullableString(before), To: nullableString(after)}
	}
}

// optionalString reads a nullable String input member: nil (absent, "leave
// alone") is distinct from an empty string ("clear").
func optionalString(input map[string]interface{}, key string) *string {
	raw, present := input[key]
	if !present || raw == nil {
		return nil
	}
	value, ok := raw.(string)
	if !ok {
		return nil
	}
	return &value
}

func optionalBool(input map[string]interface{}, key string) *bool {
	raw, present := input[key]
	if !present || raw == nil {
		return nil
	}
	value, ok := raw.(bool)
	if !ok {
		return nil
	}
	return &value
}

// projectV2NodeIDs reads a list-of-ID input member.
func projectV2NodeIDs(input map[string]interface{}, key string) []string {
	raw, _ := input[key].([]interface{})
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		if id, ok := entry.(string); ok {
			out = append(out, id)
		}
	}
	return out
}

// Project lifecycle

func (s *Resolver) addProjectV2LifecycleMutations(mutationType *graphql.Object, projectType *graphql.Object) {
	s.registerMutation(mutationType, "updateProjectV2", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name:   "UpdateProjectV2Payload",
			Fields: graphql.Fields{"projectV2": &graphql.Field{Type: projectType}},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "UpdateProjectV2Input",
			Fields: graphql.InputObjectConfigFieldMap{
				"projectId":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				"title":            &graphql.InputObjectFieldConfig{Type: graphql.String},
				"shortDescription": &graphql.InputObjectFieldConfig{Type: graphql.String},
				"readme":           &graphql.InputObjectFieldConfig{Type: graphql.String},
				"closed":           &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
				"public":           &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			before, err := s.projectV2MutationProject(input, "projectId")
			if err != nil {
				return nil, err
			}
			patch := store.ProjectV2Update{
				Title:            optionalString(input, "title"),
				ShortDescription: optionalString(input, "shortDescription"),
				Readme:           optionalString(input, "readme"),
				Closed:           optionalBool(input, "closed"),
				Public:           optionalBool(input, "public"),
			}
			after := s.store.ProjectsV2.UpdateProjectDetails(before.ID, patch)
			if after == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to a project."}
			}

			// Open/closed transitions are their own actions; everything else is
			// `edited` with a diff.
			action := "edited"
			changes := map[string]store.ProjectV2Change{}
			stringChange(changes, "title", before.Title, after.Title)
			stringChange(changes, "short_description", before.ShortDescription, after.ShortDescription)
			stringChange(changes, "description", before.Readme, after.Readme)
			if before.Public != after.Public {
				changes["public"] = store.ProjectV2Change{From: before.Public, To: after.Public}
			}
			if before.Closed != after.Closed {
				if after.Closed {
					action = "closed"
				} else {
					action = "reopened"
				}
				changes = nil
			}
			event := s.projectV2Event(p, store.ProjectV2EventProject, action, after)
			event.Changes = changes
			s.emitProjectV2Event(event)
			return map[string]interface{}{"projectV2": projectV2ToGQLFull(s.store, after)}, nil
		},
	})

	s.registerMutation(mutationType, "deleteProjectV2", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name:   "DeleteProjectV2Payload",
			Fields: graphql.Fields{"projectV2": &graphql.Field{Type: projectType}},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "DeleteProjectV2Input",
			Fields: graphql.InputObjectConfigFieldMap{
				"projectId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			project, err := s.projectV2MutationProject(input, "projectId")
			if err != nil {
				return nil, err
			}
			// Render before deleting; afterwards the row is gone.
			rendered := projectV2ToGQLFull(s.store, project)
			if !s.store.ProjectsV2.DeleteProject(project.ID) {
				return nil, &ghNotFoundError{message: "Could not resolve to a project."}
			}
			s.emitProjectV2Event(s.projectV2Event(p, store.ProjectV2EventProject, "deleted", project))
			return map[string]interface{}{"projectV2": rendered}, nil
		},
	})

	s.registerMutation(mutationType, "copyProjectV2", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name:   "CopyProjectV2Payload",
			Fields: graphql.Fields{"projectV2": &graphql.Field{Type: projectType}},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "CopyProjectV2Input",
			Fields: graphql.InputObjectConfigFieldMap{
				"projectId":          &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				"ownerId":            &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				"title":              &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
				"includeDraftIssues": &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: false},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			source, err := s.projectV2MutationProject(input, "projectId")
			if err != nil {
				return nil, err
			}
			ownerNodeID, _ := input["ownerId"].(string)
			ownerID, ownerType, ok := resolveProjectOwner(s.store, ownerNodeID)
			if !ok {
				return nil, &ghNotFoundError{
					message: fmt.Sprintf("Could not resolve to an owner with the global id of '%s'.", ownerNodeID),
				}
			}
			title, _ := input["title"].(string)
			includeDrafts, _ := input["includeDraftIssues"].(bool)
			user := s.ghUserFromContext(p.Context)
			copied := s.store.ProjectsV2.CopyProject(source.ID, ownerID, ownerType, title, includeDrafts, user.ID)
			if copied == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to a project."}
			}
			s.emitProjectV2Event(s.projectV2Event(p, store.ProjectV2EventProject, "created", copied))
			return map[string]interface{}{"projectV2": projectV2ToGQLFull(s.store, copied)}, nil
		},
	})

	for _, variant := range []struct {
		name     string
		template bool
	}{{"markProjectV2AsTemplate", true}, {"unmarkProjectV2AsTemplate", false}} {
		variant := variant
		payloadName := strings.ToUpper(variant.name[:1]) + variant.name[1:] + "Payload"
		inputName := strings.ToUpper(variant.name[:1]) + variant.name[1:] + "Input"
		s.registerMutation(mutationType, variant.name, &graphql.Field{
			Type: graphql.NewObject(graphql.ObjectConfig{
				Name:   payloadName,
				Fields: graphql.Fields{"projectV2": &graphql.Field{Type: projectType}},
			}),
			Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
				Name: inputName,
				Fields: graphql.InputObjectConfigFieldMap{
					"projectId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				},
			}))}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				input, _ := p.Args["input"].(map[string]interface{})
				project, err := s.projectV2MutationProject(input, "projectId")
				if err != nil {
					return nil, err
				}
				updated := s.store.ProjectsV2.SetProjectTemplate(project.ID, variant.template)
				if updated == nil {
					return nil, &ghNotFoundError{message: "Could not resolve to a project."}
				}
				event := s.projectV2Event(p, store.ProjectV2EventProject, "edited", updated)
				event.Changes = map[string]store.ProjectV2Change{
					"template": {From: project.Template, To: updated.Template},
				}
				s.emitProjectV2Event(event)
				return map[string]interface{}{"projectV2": projectV2ToGQLFull(s.store, updated)}, nil
			},
		})
	}
}

// Links to repositories and teams

func (s *Resolver) addProjectV2LinkMutations(mutationType *graphql.Object) {
	repoType := s.graphqlTypes.repository
	teamType := s.graphqlTypes.team

	for _, variant := range []struct {
		name string
		link bool
	}{{"linkProjectV2ToRepository", true}, {"unlinkProjectV2FromRepository", false}} {
		variant := variant
		s.registerMutation(mutationType, variant.name, &graphql.Field{
			Type: graphql.NewObject(graphql.ObjectConfig{
				Name:   projectV2PayloadName(variant.name),
				Fields: graphql.Fields{"repository": &graphql.Field{Type: repoType}},
			}),
			Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
				Name: projectV2InputName(variant.name),
				Fields: graphql.InputObjectConfigFieldMap{
					"projectId":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
					"repositoryId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				},
			}))}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				input, _ := p.Args["input"].(map[string]interface{})
				project, err := s.projectV2MutationProject(input, "projectId")
				if err != nil {
					return nil, err
				}
				repoNodeID, _ := input["repositoryId"].(string)
				repo := store.FindRepoByNodeID(s.store, repoNodeID)
				// A repository the caller cannot read answers as not-found, since
				// linking would republish its name wherever the project is visible.
				if repo == nil || !s.viewerCanReadRepo(p.Context, repo) {
					return nil, &ghNotFoundError{
						message: fmt.Sprintf("Could not resolve to a Repository with the global id of '%s'.", repoNodeID),
					}
				}
				var updated *store.ProjectV2
				if variant.link {
					updated = s.store.ProjectsV2.LinkRepository(project.ID, repo.ID)
				} else {
					updated = s.store.ProjectsV2.UnlinkRepository(project.ID, repo.ID)
				}
				if updated == nil {
					return nil, &ghNotFoundError{message: "Could not resolve to a project."}
				}
				s.emitProjectV2Event(s.projectV2Event(p, store.ProjectV2EventProject, "edited", updated))
				return map[string]interface{}{"repository": repoToGraphQL(s.store, repo)}, nil
			},
		})
	}

	for _, variant := range []struct {
		name string
		link bool
	}{{"linkProjectV2ToTeam", true}, {"unlinkProjectV2FromTeam", false}} {
		variant := variant
		s.registerMutation(mutationType, variant.name, &graphql.Field{
			Type: graphql.NewObject(graphql.ObjectConfig{
				Name:   projectV2PayloadName(variant.name),
				Fields: graphql.Fields{"team": &graphql.Field{Type: teamType}},
			}),
			Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
				Name: projectV2InputName(variant.name),
				Fields: graphql.InputObjectConfigFieldMap{
					"projectId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
					"teamId":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				},
			}))}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				input, _ := p.Args["input"].(map[string]interface{})
				project, err := s.projectV2MutationProject(input, "projectId")
				if err != nil {
					return nil, err
				}
				teamNodeID, _ := input["teamId"].(string)
				team := s.projectV2TeamByNodeID(teamNodeID)
				// The team must belong to the project's owning organization.
				if team == nil || project.OwnerType != "Organization" || team.OrgID != project.OwnerID {
					return nil, &ghNotFoundError{
						message: fmt.Sprintf("Could not resolve to a Team with the global id of '%s'.", teamNodeID),
					}
				}
				var updated *store.ProjectV2
				if variant.link {
					updated = s.store.ProjectsV2.LinkTeam(project.ID, team.ID)
				} else {
					updated = s.store.ProjectsV2.UnlinkTeam(project.ID, team.ID)
				}
				if updated == nil {
					return nil, &ghNotFoundError{message: "Could not resolve to a project."}
				}
				s.emitProjectV2Event(s.projectV2Event(p, store.ProjectV2EventProject, "edited", updated))
				org := s.store.GetOrgByID(team.OrgID)
				payload := map[string]interface{}{
					"id":   team.NodeID,
					"name": team.Name,
					"slug": team.Slug,
				}
				if org != nil {
					payload["organization"] = orgToGraphQL(org)
				}
				return map[string]interface{}{"team": payload}, nil
			},
		})
	}
}

// projectV2TeamByNodeID finds a team by its global node id.
func (s *Resolver) projectV2TeamByNodeID(nodeID string) *store.Team {
	if nodeID == "" {
		return nil
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, team := range s.store.Teams {
		if team.NodeID == nodeID {
			return team
		}
	}
	return nil
}

// projectV2PayloadName / projectV2InputName derive the payload and input type
// names from the mutation name.
func projectV2PayloadName(mutation string) string {
	return strings.ToUpper(mutation[:1]) + mutation[1:] + "Payload"
}

func projectV2InputName(mutation string) string {
	return strings.ToUpper(mutation[:1]) + mutation[1:] + "Input"
}

// Collaborators

func (s *Resolver) addProjectV2CollaboratorMutations(mutationType *graphql.Object) {
	rolesEnum := s.graphQLEnum("ProjectV2Roles", "ADMIN", "NONE", "READER", "WRITER")
	collaboratorInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "ProjectV2Collaborator",
		Fields: graphql.InputObjectConfigFieldMap{
			"role":   &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(rolesEnum)},
			"userId": &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"teamId": &graphql.InputObjectFieldConfig{Type: graphql.ID},
		},
	})
	actorUnion := graphql.NewUnion(graphql.UnionConfig{
		Name:  "ProjectV2Actor",
		Types: []*graphql.Object{s.graphqlTypes.user, s.graphqlTypes.team},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			if name, _ := source["__typename"].(string); name == "Team" {
				return s.graphqlTypes.team
			}
			return s.graphqlTypes.user
		},
	})

	s.registerMutation(mutationType, "updateProjectV2Collaborators", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name: "UpdateProjectV2CollaboratorsPayload",
			Fields: graphql.Fields{
				"collaborators": &graphql.Field{
					Type: s.gqlConnectionType("ProjectV2Actor", actorUnion),
					Args: relayConnectionArgs(),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						src, ok := p.Source.(map[string]interface{})
						if !ok {
							return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
						}
						actors, _ := src["collaboratorActors"].([]map[string]interface{})
						return paginateGQLMaps(actors, p.Args), nil
					},
				},
			},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "UpdateProjectV2CollaboratorsInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"projectId":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				"collaborators": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(collaboratorInput)))},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			project, err := s.projectV2MutationProject(input, "projectId")
			if err != nil {
				return nil, err
			}
			raw, _ := input["collaborators"].([]interface{})
			grants := make([]*store.ProjectV2Collaborator, 0, len(raw))
			for _, entry := range raw {
				grant, _ := entry.(map[string]interface{})
				if grant == nil {
					continue
				}
				role, _ := grant["role"].(string)
				userNodeID, _ := grant["userId"].(string)
				teamNodeID, _ := grant["teamId"].(string)
				if userNodeID == "" && teamNodeID == "" {
					return nil, fmt.Errorf("each collaborator must name a userId or a teamId")
				}
				resolved := &store.ProjectV2Collaborator{Role: role}
				if userNodeID != "" {
					user := store.FindUserByNodeID(s.store, userNodeID)
					if user == nil {
						return nil, &ghNotFoundError{
							message: fmt.Sprintf("Could not resolve to a User with the global id of '%s'.", userNodeID),
						}
					}
					resolved.UserID = user.ID
				}
				if teamNodeID != "" {
					team := s.projectV2TeamByNodeID(teamNodeID)
					if team == nil {
						return nil, &ghNotFoundError{
							message: fmt.Sprintf("Could not resolve to a Team with the global id of '%s'.", teamNodeID),
						}
					}
					resolved.TeamID = team.ID
				}
				grants = append(grants, resolved)
			}
			updated := s.store.ProjectsV2.UpdateCollaborators(project.ID, grants)
			if updated == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to a project."}
			}
			actors := make([]map[string]interface{}, 0, len(updated.Collaborators))
			for _, collaborator := range updated.Collaborators {
				if collaborator.TeamID != 0 {
					team := s.store.GetTeamByID(collaborator.TeamID)
					if team == nil {
						continue
					}
					actor := map[string]interface{}{
						"__typename": "Team",
						"id":         team.NodeID,
						"name":       team.Name,
						"slug":       team.Slug,
					}
					if org := s.store.GetOrgByID(team.OrgID); org != nil {
						actor["organization"] = orgToGraphQL(org)
					}
					actors = append(actors, actor)
					continue
				}
				user := s.store.GetUserByID(collaborator.UserID)
				if user == nil {
					continue
				}
				actor := userToGraphQL(user)
				actor["__typename"] = "User"
				actors = append(actors, actor)
			}
			s.emitProjectV2Event(s.projectV2Event(p, store.ProjectV2EventProject, "edited", updated))
			return map[string]interface{}{"collaboratorActors": actors}, nil
		},
	})
}

// Fields

func (s *Resolver) addProjectV2FieldMutations(mutationType *graphql.Object, fieldUnion *graphql.Union) {
	// The two option inputs are structurally identical but separately named.
	singleSelectOptionInput := s.projectV2SingleSelectOptionInput()
	multiSelectOptionInput := s.projectV2MultiSelectOptionInput()

	s.registerMutation(mutationType, "updateProjectV2Field", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name:   "UpdateProjectV2FieldPayload",
			Fields: graphql.Fields{"projectV2Field": &graphql.Field{Type: fieldUnion}},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "UpdateProjectV2FieldInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"fieldId":                &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				"name":                   &graphql.InputObjectFieldConfig{Type: graphql.String},
				"singleSelectOptions":    &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(singleSelectOptionInput))},
				"multiSelectOptions":     &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(multiSelectOptionInput))},
				"iterationConfiguration": &graphql.InputObjectFieldConfig{Type: s.projectV2IterationConfigurationInput()},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			field, err := s.projectV2MutationField(input, "fieldId")
			if err != nil {
				return nil, err
			}
			patch := store.ProjectV2FieldUpdate{Name: optionalString(input, "name")}
			for _, key := range []string{"singleSelectOptions", "multiSelectOptions"} {
				if raw, ok := input[key].([]interface{}); ok && raw != nil {
					patch.Options = projectV2OptionsFromInput(raw)
				}
			}
			if rawIteration, ok := input["iterationConfiguration"].(map[string]interface{}); ok {
				iteration, err := projectV2IterationFromInput(rawIteration)
				if err != nil {
					return nil, err
				}
				patch.Iteration = iteration
			}
			updated := s.store.ProjectsV2.UpdateFieldDetails(field.ID, patch)
			if updated == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to a field."}
			}
			s.projectV2EmitProjectEdited(p, updated.ProjectID)
			return map[string]interface{}{"projectV2Field": projectV2FieldToGQL(updated)}, nil
		},
	})

	s.registerMutation(mutationType, "deleteProjectV2Field", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name:   "DeleteProjectV2FieldPayload",
			Fields: graphql.Fields{"projectV2Field": &graphql.Field{Type: fieldUnion}},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "DeleteProjectV2FieldInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"fieldId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			field, err := s.projectV2MutationField(input, "fieldId")
			if err != nil {
				return nil, err
			}
			rendered := projectV2FieldToGQL(field)
			if !s.store.ProjectsV2.DeleteField(field.ID) {
				return nil, &ghNotFoundError{message: "Could not resolve to a field."}
			}
			s.projectV2EmitProjectEdited(p, field.ProjectID)
			return map[string]interface{}{"projectV2Field": rendered}, nil
		},
	})

	// createProjectV2IssueField promotes an org issue field onto a project, so the
	// column and the issue field stay one definition.
	s.registerMutation(mutationType, "createProjectV2IssueField", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name:   "CreateProjectV2IssueFieldPayload",
			Fields: graphql.Fields{"projectV2Field": &graphql.Field{Type: fieldUnion}},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "CreateProjectV2IssueFieldInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"projectId":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				"issueFieldId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			project, err := s.projectV2MutationProject(input, "projectId")
			if err != nil {
				return nil, err
			}
			issueFieldNodeID, _ := input["issueFieldId"].(string)
			issueField := s.projectV2IssueFieldByNodeID(issueFieldNodeID)
			if issueField == nil {
				return nil, &ghNotFoundError{
					message: fmt.Sprintf("Could not resolve to an issue field with the global id of '%s'.", issueFieldNodeID),
				}
			}
			options := make([]*store.ProjectV2SingleSelectOption, 0, len(issueField.Options))
			for _, opt := range issueField.Options {
				description := ""
				if opt.Description != nil {
					description = *opt.Description
				}
				options = append(options, &store.ProjectV2SingleSelectOption{
					Name: opt.Name, Color: strings.ToUpper(opt.Color), Description: description,
				})
			}
			created := s.store.ProjectsV2.CreateField(
				project.ID, issueField.Name,
				projectV2DataTypeForIssueField(issueField.DataType),
				options, nil,
			)
			if created == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to a project."}
			}
			s.projectV2EmitProjectEdited(p, project.ID)
			return map[string]interface{}{"projectV2Field": projectV2FieldToGQL(created)}, nil
		},
	})
}

// projectV2IssueFieldByNodeID finds an org issue field by node id, scanning the
// per-org buckets it is keyed in.
func (s *Resolver) projectV2IssueFieldByNodeID(nodeID string) *store.IssueField {
	if nodeID == "" {
		return nil
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, fields := range s.store.OrgIssueFields {
		for _, field := range fields {
			if field.NodeID == nodeID {
				return field
			}
		}
	}
	return nil
}

// projectV2SingleSelectOptionInput is ProjectV2SingleSelectFieldOptionInput,
// memoized (createProjectV2Field and updateProjectV2Field both declare it).
func (s *Resolver) projectV2SingleSelectOptionInput() *graphql.InputObject {
	if s.graphqlTypes.projectV2OptionInput != nil {
		return s.graphqlTypes.projectV2OptionInput
	}
	s.graphqlTypes.projectV2OptionInput = graphql.NewInputObject(graphql.InputObjectConfig{
		Name:   "ProjectV2SingleSelectFieldOptionInput",
		Fields: s.projectV2OptionInputFields(),
	})
	return s.graphqlTypes.projectV2OptionInput
}

// projectV2MultiSelectOptionInput is ProjectV2MultiSelectFieldOptionInput,
// memoized like its single-select twin.
func (s *Resolver) projectV2MultiSelectOptionInput() *graphql.InputObject {
	if s.graphqlTypes.projectV2MultiOptionInput != nil {
		return s.graphqlTypes.projectV2MultiOptionInput
	}
	s.graphqlTypes.projectV2MultiOptionInput = graphql.NewInputObject(graphql.InputObjectConfig{
		Name:   "ProjectV2MultiSelectFieldOptionInput",
		Fields: s.projectV2OptionInputFields(),
	})
	return s.graphqlTypes.projectV2MultiOptionInput
}

// projectV2OptionInputFields is the member set both option inputs declare.
func (s *Resolver) projectV2OptionInputFields() graphql.InputObjectConfigFieldMap {
	colorEnum := s.graphQLEnum(
		"ProjectV2SingleSelectFieldOptionColor",
		"BLUE", "GRAY", "GREEN", "ORANGE", "PINK", "PURPLE", "RED", "YELLOW",
	)
	return graphql.InputObjectConfigFieldMap{
		"name":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		"color":       &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(colorEnum)},
		"description": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		"id":          &graphql.InputObjectFieldConfig{Type: graphql.String},
	}
}

// projectV2OptionsFromInput reads an option list input into store options.
func projectV2OptionsFromInput(raw []interface{}) []*store.ProjectV2SingleSelectOption {
	options := make([]*store.ProjectV2SingleSelectOption, 0, len(raw))
	for _, entry := range raw {
		option, _ := entry.(map[string]interface{})
		if option == nil {
			continue
		}
		name, _ := option["name"].(string)
		color, _ := option["color"].(string)
		description, _ := option["description"].(string)
		id, _ := option["id"].(string)
		options = append(options, &store.ProjectV2SingleSelectOption{
			ID: id, Name: name, Color: color, Description: description,
		})
	}
	return options
}

// projectV2IterationConfigurationInput is GitHub's
// ProjectV2IterationFieldConfigurationInput, memoized (together with its nested
// ProjectV2Iteration input) because createProjectV2Field and
// updateProjectV2Field both declare it and a schema may name a type once.
func (s *Resolver) projectV2IterationConfigurationInput() *graphql.InputObject {
	dateScalar := s.graphQLStringScalar("Date")
	iterationInput := s.mutationInput("ProjectV2Iteration", graphql.InputObjectConfigFieldMap{
		"title":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		"startDate": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(dateScalar)},
		"duration":  &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.Int)},
	})
	return s.mutationInput("ProjectV2IterationFieldConfigurationInput", graphql.InputObjectConfigFieldMap{
		"startDate":  &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(dateScalar)},
		"duration":   &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.Int)},
		"iterations": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(iterationInput)))},
	})
}

// projectV2IterationFromInput reads an iterationConfiguration input into a store
// schedule. IDs are minted by the store; existing ones survive by title match.
func projectV2IterationFromInput(rawIteration map[string]interface{}) (*store.ProjectV2IterationConfiguration, error) {
	if rawIteration == nil {
		return nil, nil
	}
	startDate, _ := rawIteration["startDate"].(string)
	duration, _ := rawIteration["duration"].(int)
	iteration := &store.ProjectV2IterationConfiguration{StartDate: startDate, Duration: duration}
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
	return iteration, nil
}

// projectV2DataTypeForIssueField maps an org issue field's type onto the project
// field data type that stores the same values.
func projectV2DataTypeForIssueField(dataType string) store.ProjectV2FieldDataType {
	switch strings.ToUpper(dataType) {
	case "NUMBER":
		return store.ProjectV2FieldNumber
	case "DATE":
		return store.ProjectV2FieldDate
	case "SINGLE_SELECT":
		return store.ProjectV2FieldSingleSelect
	case "MULTI_SELECT":
		return store.ProjectV2FieldMultiSelect
	default:
		return store.ProjectV2FieldText
	}
}

// projectV2MutationField resolves the field a mutation names.
func (s *Resolver) projectV2MutationField(input map[string]interface{}, key string) (*store.ProjectV2Field, error) {
	nodeID, _ := input[key].(string)
	field := s.store.ProjectsV2.LookupFieldByNodeID(nodeID)
	if field == nil {
		return nil, &ghNotFoundError{
			message: fmt.Sprintf("Could not resolve to a field with the global id of '%s'.", nodeID),
		}
	}
	return field, nil
}

// projectV2EmitProjectEdited reports a field or view change as an `edited`
// project event; there is no per-field webhook.
func (s *Resolver) projectV2EmitProjectEdited(p graphql.ResolveParams, projectID int) {
	s.store.ProjectsV2.TouchProject(projectID)
	project := s.store.ProjectsV2.GetProject(projectID)
	if project == nil {
		return
	}
	s.emitProjectV2Event(s.projectV2Event(p, store.ProjectV2EventProject, "edited", project))
}

// Views

func (s *Resolver) addProjectV2ViewMutations(mutationType *graphql.Object, viewType *graphql.Object) {
	layoutEnum := s.graphQLEnum("ProjectV2ViewLayout", "BOARD_LAYOUT", "ROADMAP_LAYOUT", "TABLE_LAYOUT")
	configurationInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "ProjectV2ViewConfigurationInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"visibleFieldIds": &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.ID))},
		},
	})

	s.registerMutation(mutationType, "createProjectV2View", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name:   "CreateProjectV2ViewPayload",
			Fields: graphql.Fields{"projectV2View": &graphql.Field{Type: viewType}},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "CreateProjectV2ViewInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"projectId":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				"name":          &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
				"layout":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(layoutEnum)},
				"configuration": &graphql.InputObjectFieldConfig{Type: configurationInput},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			project, err := s.projectV2MutationProject(input, "projectId")
			if err != nil {
				return nil, err
			}
			name, _ := input["name"].(string)
			layout, _ := input["layout"].(string)
			visible, err := s.projectV2VisibleFieldIDs(input, project)
			if err != nil {
				return nil, err
			}
			if visible == nil {
				// A new view defaults to every field on the project.
				for _, f := range s.store.ProjectsV2.FieldsForProject(project.ID) {
					visible = append(visible, f.ID)
				}
			}
			user := s.ghUserFromContext(p.Context)
			view := s.store.ProjectsV2.CreateView(project.ID, name, projectV2StoreLayout(layout), nil, visible, user.ID)
			if view == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to a project."}
			}
			s.projectV2EmitProjectEdited(p, project.ID)
			return map[string]interface{}{"projectV2View": projectV2ViewToGQL(view)}, nil
		},
	})

	s.registerMutation(mutationType, "updateProjectV2View", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name:   "UpdateProjectV2ViewPayload",
			Fields: graphql.Fields{"projectV2View": &graphql.Field{Type: viewType}},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "UpdateProjectV2ViewInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"viewId":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				"name":          &graphql.InputObjectFieldConfig{Type: graphql.String},
				"layout":        &graphql.InputObjectFieldConfig{Type: layoutEnum},
				"filter":        &graphql.InputObjectFieldConfig{Type: graphql.String},
				"configuration": &graphql.InputObjectFieldConfig{Type: configurationInput},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			view, err := s.projectV2MutationView(input, "viewId")
			if err != nil {
				return nil, err
			}
			project := s.store.ProjectsV2.GetProject(view.ProjectID)
			if project == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to a project."}
			}
			patch := store.ProjectV2ViewUpdate{
				Name:   optionalString(input, "name"),
				Filter: optionalString(input, "filter"),
			}
			if layout := optionalString(input, "layout"); layout != nil {
				stored := projectV2StoreLayout(*layout)
				patch.Layout = &stored
			}
			visible, err := s.projectV2VisibleFieldIDs(input, project)
			if err != nil {
				return nil, err
			}
			patch.VisibleFields = visible
			updated := s.store.ProjectsV2.UpdateView(view.ID, patch)
			if updated == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to a view."}
			}
			s.projectV2EmitProjectEdited(p, project.ID)
			return map[string]interface{}{"projectV2View": projectV2ViewToGQL(updated)}, nil
		},
	})

	s.registerMutation(mutationType, "deleteProjectV2View", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name:   "DeleteProjectV2ViewPayload",
			Fields: graphql.Fields{"projectV2View": &graphql.Field{Type: viewType}},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "DeleteProjectV2ViewInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"viewId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			view, err := s.projectV2MutationView(input, "viewId")
			if err != nil {
				return nil, err
			}
			deleted := s.store.ProjectsV2.DeleteView(view.ID)
			if deleted == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to a view."}
			}
			s.projectV2EmitProjectEdited(p, view.ProjectID)
			return map[string]interface{}{"projectV2View": projectV2ViewToGQL(deleted)}, nil
		},
	})
}

// projectV2VisibleFieldIDs reads a view configuration input's visibleFieldIds,
// mapping node ids to database ids and refusing any field that is not on this
// project. Returns nil when the client supplied no configuration.
func (s *Resolver) projectV2VisibleFieldIDs(input map[string]interface{}, project *store.ProjectV2) ([]int, error) {
	configuration, _ := input["configuration"].(map[string]interface{})
	if configuration == nil {
		return nil, nil
	}
	nodeIDs := projectV2NodeIDs(configuration, "visibleFieldIds")
	if nodeIDs == nil {
		return nil, nil
	}
	visible := make([]int, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		field := s.store.ProjectsV2.LookupFieldByNodeID(nodeID)
		if field == nil || field.ProjectID != project.ID {
			return nil, fmt.Errorf("field %q is not on this project", nodeID)
		}
		visible = append(visible, field.ID)
	}
	return visible, nil
}

// projectV2StoreLayout maps the ProjectV2ViewLayout enum onto the store's
// lowercase layout name.
func projectV2StoreLayout(layout string) string {
	return strings.ToLower(strings.TrimSuffix(layout, "_LAYOUT"))
}

// projectV2MutationView resolves the view a mutation names.
func (s *Resolver) projectV2MutationView(input map[string]interface{}, key string) (*store.ProjectV2View, error) {
	nodeID, _ := input[key].(string)
	view := s.store.ProjectsV2.LookupViewByNodeID(nodeID)
	if view == nil {
		return nil, &ghNotFoundError{
			message: fmt.Sprintf("Could not resolve to a view with the global id of '%s'.", nodeID),
		}
	}
	return view, nil
}

// Items

func (s *Resolver) addProjectV2ItemMutations(mutationType *graphql.Object, itemType, draftIssueType *graphql.Object) {
	s.registerMutation(mutationType, "addProjectV2DraftIssue", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name:   "AddProjectV2DraftIssuePayload",
			Fields: graphql.Fields{"projectItem": &graphql.Field{Type: itemType}},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "AddProjectV2DraftIssueInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"projectId":   &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				"title":       &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
				"body":        &graphql.InputObjectFieldConfig{Type: graphql.String},
				"assigneeIds": &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.ID))},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			project, err := s.projectV2MutationProject(input, "projectId")
			if err != nil {
				return nil, err
			}
			title, _ := input["title"].(string)
			if strings.TrimSpace(title) == "" {
				return nil, fmt.Errorf("title is required")
			}
			body, _ := input["body"].(string)
			user := s.ghUserFromContext(p.Context)
			item := s.store.ProjectsV2.AddDraftItem(project.ID, title, body, user.ID)
			if item == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to a project."}
			}
			event := s.projectV2Event(p, store.ProjectV2EventItem, "created", project)
			event.Item = item
			s.emitProjectV2Event(event)
			return map[string]interface{}{"projectItem": projectV2ItemToGQL(item, s.store)}, nil
		},
	})

	s.registerMutation(mutationType, "updateProjectV2DraftIssue", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name:   "UpdateProjectV2DraftIssuePayload",
			Fields: graphql.Fields{"draftIssue": &graphql.Field{Type: draftIssueType}},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "UpdateProjectV2DraftIssueInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"draftIssueId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				"title":        &graphql.InputObjectFieldConfig{Type: graphql.String},
				"body":         &graphql.InputObjectFieldConfig{Type: graphql.String},
				"assigneeIds":  &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.ID))},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			// A draft issue has no row of its own; the item's global id is the draft's.
			item, err := s.projectV2MutationItem(input, "draftIssueId", nil)
			if err != nil {
				return nil, err
			}
			if item.ContentType != "DraftIssue" {
				return nil, fmt.Errorf("item is not a draft issue")
			}
			updated := s.store.ProjectsV2.UpdateItem(item.ID, optionalString(input, "title"), optionalString(input, "body"))
			if updated == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to a draft issue."}
			}
			project := s.store.ProjectsV2.GetProject(updated.ProjectID)
			if project != nil {
				event := s.projectV2Event(p, store.ProjectV2EventItem, "edited", project)
				event.Item = updated
				s.emitProjectV2Event(event)
			}
			return map[string]interface{}{
				"draftIssue": optionalObject(projectV2ItemContentToGQL(s.store, updated)),
			}, nil
		},
	})

	for _, variant := range []struct {
		name     string
		archived bool
		action   string
	}{
		{"archiveProjectV2Item", true, "archived"},
		{"unarchiveProjectV2Item", false, "restored"},
	} {
		variant := variant
		s.registerMutation(mutationType, variant.name, &graphql.Field{
			Type: graphql.NewObject(graphql.ObjectConfig{
				Name:   projectV2PayloadName(variant.name),
				Fields: graphql.Fields{"item": &graphql.Field{Type: itemType}},
			}),
			Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
				Name: projectV2InputName(variant.name),
				Fields: graphql.InputObjectConfigFieldMap{
					"projectId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
					"itemId":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				},
			}))}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				input, _ := p.Args["input"].(map[string]interface{})
				project, err := s.projectV2MutationProject(input, "projectId")
				if err != nil {
					return nil, err
				}
				item, err := s.projectV2MutationItem(input, "itemId", project)
				if err != nil {
					return nil, err
				}
				updated := s.store.ProjectsV2.ArchiveItem(item.ID, variant.archived)
				if updated == nil {
					return nil, &ghNotFoundError{message: "Could not resolve to an item."}
				}
				event := s.projectV2Event(p, store.ProjectV2EventItem, variant.action, project)
				event.Item = updated
				s.emitProjectV2Event(event)
				return map[string]interface{}{"item": projectV2ItemToGQL(updated, s.store)}, nil
			},
		})
	}

	s.registerMutation(mutationType, "clearProjectV2ItemFieldValue", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name:   "ClearProjectV2ItemFieldValuePayload",
			Fields: graphql.Fields{"projectV2Item": &graphql.Field{Type: itemType}},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "ClearProjectV2ItemFieldValueInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"projectId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				"itemId":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				"fieldId":   &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			project, err := s.projectV2MutationProject(input, "projectId")
			if err != nil {
				return nil, err
			}
			item, err := s.projectV2MutationItem(input, "itemId", project)
			if err != nil {
				return nil, err
			}
			field, err := s.projectV2MutationField(input, "fieldId")
			if err != nil {
				return nil, err
			}
			if field.ProjectID != project.ID {
				return nil, fmt.Errorf("field does not belong to this project")
			}
			updated, err := s.store.ProjectsV2.ClearFieldValue(item.ID, field.ID)
			if err != nil {
				return nil, err
			}
			event := s.projectV2Event(p, store.ProjectV2EventItem, "edited", project)
			event.Item = updated
			s.emitProjectV2Event(event)
			return map[string]interface{}{"projectV2Item": projectV2ItemToGQL(updated, s.store)}, nil
		},
	})

	s.registerMutation(mutationType, "updateProjectV2ItemPosition", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name: "UpdateProjectV2ItemPositionPayload",
			Fields: graphql.Fields{
				"items": &graphql.Field{
					Type: s.projectV2ItemConnectionType(),
					Args: relayConnectionArgs(),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						src, ok := p.Source.(map[string]interface{})
						if !ok {
							return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
						}
						items, _ := src["orderedItems"].([]map[string]interface{})
						return paginateGQLMaps(items, p.Args), nil
					},
				},
			},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "UpdateProjectV2ItemPositionInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"projectId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				"itemId":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				"afterId":   &graphql.InputObjectFieldConfig{Type: graphql.ID},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			project, err := s.projectV2MutationProject(input, "projectId")
			if err != nil {
				return nil, err
			}
			item, err := s.projectV2MutationItem(input, "itemId", project)
			if err != nil {
				return nil, err
			}
			afterID := 0
			// A null/absent afterId means move to the front.
			if afterNodeID, _ := input["afterId"].(string); afterNodeID != "" {
				after, err := s.projectV2MutationItem(map[string]interface{}{"afterId": afterNodeID}, "afterId", project)
				if err != nil {
					return nil, err
				}
				afterID = after.ID
			}
			moved, err := s.store.ProjectsV2.MoveItem(item.ID, afterID)
			if err != nil {
				return nil, err
			}
			ordered := s.store.ProjectsV2.ListItemsForProject(project.ID)
			rendered := make([]map[string]interface{}, 0, len(ordered))
			for _, entry := range ordered {
				rendered = append(rendered, projectV2ItemToGQL(entry, s.store))
			}
			event := s.projectV2Event(p, store.ProjectV2EventItem, "reordered", project)
			event.Item = moved
			s.emitProjectV2Event(event)
			return map[string]interface{}{"orderedItems": rendered}, nil
		},
	})

	s.registerMutation(mutationType, "convertProjectV2DraftIssueItemToIssue", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name:   "ConvertProjectV2DraftIssueItemToIssuePayload",
			Fields: graphql.Fields{"item": &graphql.Field{Type: itemType}},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "ConvertProjectV2DraftIssueItemToIssueInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"itemId":       &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				"repositoryId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			item, err := s.projectV2MutationItem(input, "itemId", nil)
			if err != nil {
				return nil, err
			}
			if item.ContentType != "DraftIssue" {
				return nil, fmt.Errorf("item is not a draft issue")
			}
			repoNodeID, _ := input["repositoryId"].(string)
			repo := store.FindRepoByNodeID(s.store, repoNodeID)
			if repo == nil || !s.viewerCanReadRepo(p.Context, repo) {
				return nil, &ghNotFoundError{
					message: fmt.Sprintf("Could not resolve to a Repository with the global id of '%s'.", repoNodeID),
				}
			}
			// Project write is not repository write; the caller must be able to
			// file an issue in the repository in their own right.
			if !s.credentialGrantsRepo(p.Context, repo, store.ScopeIssues, store.PermWrite) {
				return nil, &ghForbiddenError{message: "resource not accessible by integration"}
			}
			user := s.ghUserFromContext(p.Context)
			issue := s.store.CreateIssue(repo.ID, user.ID, item.DraftTitle, item.DraftBody, nil, nil, 0)
			if issue == nil {
				return nil, fmt.Errorf("could not create the issue")
			}
			converted, err := s.store.ProjectsV2.ConvertDraftToIssue(item.ID, issue.ID)
			if err != nil {
				return nil, err
			}
			s.emitWebhookEvent(repo.FullName, "issues", "opened", s.buildIssuesPayload(repo, issue, user, "opened"))
			if project := s.store.ProjectsV2.GetProject(converted.ProjectID); project != nil {
				event := s.projectV2Event(p, store.ProjectV2EventItem, "converted", project)
				event.Item = converted
				s.emitProjectV2Event(event)
			}
			return map[string]interface{}{"item": projectV2ItemToGQL(converted, s.store)}, nil
		},
	})
}

// Status updates

func (s *Resolver) addProjectV2StatusUpdateMutations(mutationType, projectType, statusType *graphql.Object) {
	statusEnum := s.graphQLEnum("ProjectV2StatusUpdateStatus", store.ProjectV2StatusUpdateStatuses...)
	date := s.graphQLStringScalar("Date")

	s.registerMutation(mutationType, "createProjectV2StatusUpdate", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name:   "CreateProjectV2StatusUpdatePayload",
			Fields: graphql.Fields{"statusUpdate": &graphql.Field{Type: statusType}},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "CreateProjectV2StatusUpdateInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"projectId":  &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				"body":       &graphql.InputObjectFieldConfig{Type: graphql.String},
				"status":     &graphql.InputObjectFieldConfig{Type: statusEnum},
				"startDate":  &graphql.InputObjectFieldConfig{Type: date},
				"targetDate": &graphql.InputObjectFieldConfig{Type: date},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			project, err := s.projectV2MutationProject(input, "projectId")
			if err != nil {
				return nil, err
			}
			body, _ := input["body"].(string)
			status, _ := input["status"].(string)
			startDate, targetDate, err := projectV2StatusDates(input)
			if err != nil {
				return nil, err
			}
			user := s.ghUserFromContext(p.Context)
			update := s.store.ProjectsV2.CreateStatusUpdate(project.ID, user.ID, body, status, startDate, targetDate)
			if update == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to a project."}
			}
			event := s.projectV2Event(p, store.ProjectV2EventStatusUpdate, "created", project)
			event.StatusUpdate = update
			s.emitProjectV2Event(event)
			return map[string]interface{}{"statusUpdate": s.projectV2StatusUpdateToGQL(update)}, nil
		},
	})

	s.registerMutation(mutationType, "updateProjectV2StatusUpdate", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name:   "UpdateProjectV2StatusUpdatePayload",
			Fields: graphql.Fields{"statusUpdate": &graphql.Field{Type: statusType}},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "UpdateProjectV2StatusUpdateInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"statusUpdateId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
				"body":           &graphql.InputObjectFieldConfig{Type: graphql.String},
				"status":         &graphql.InputObjectFieldConfig{Type: statusEnum},
				"startDate":      &graphql.InputObjectFieldConfig{Type: date},
				"targetDate":     &graphql.InputObjectFieldConfig{Type: date},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			existing, err := s.projectV2MutationStatusUpdate(input, "statusUpdateId")
			if err != nil {
				return nil, err
			}
			if _, _, err := projectV2StatusDates(input); err != nil {
				return nil, err
			}
			patch := store.ProjectV2StatusUpdatePatch{
				Body:       optionalString(input, "body"),
				Status:     optionalString(input, "status"),
				StartDate:  optionalString(input, "startDate"),
				TargetDate: optionalString(input, "targetDate"),
			}
			updated := s.store.ProjectsV2.UpdateStatusUpdate(existing.ID, patch)
			if updated == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to a status update."}
			}
			if project := s.store.ProjectsV2.GetProject(updated.ProjectID); project != nil {
				event := s.projectV2Event(p, store.ProjectV2EventStatusUpdate, "edited", project)
				event.StatusUpdate = updated
				s.emitProjectV2Event(event)
			}
			return map[string]interface{}{"statusUpdate": s.projectV2StatusUpdateToGQL(updated)}, nil
		},
	})

	s.registerMutation(mutationType, "deleteProjectV2StatusUpdate", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name: "DeleteProjectV2StatusUpdatePayload",
			Fields: graphql.Fields{
				"deletedStatusUpdateId": &graphql.Field{Type: graphql.ID},
				"projectV2":             &graphql.Field{Type: projectType},
			},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "DeleteProjectV2StatusUpdateInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"statusUpdateId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			existing, err := s.projectV2MutationStatusUpdate(input, "statusUpdateId")
			if err != nil {
				return nil, err
			}
			deleted := s.store.ProjectsV2.DeleteStatusUpdate(existing.ID)
			if deleted == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to a status update."}
			}
			project := s.store.ProjectsV2.GetProject(deleted.ProjectID)
			if project != nil {
				event := s.projectV2Event(p, store.ProjectV2EventStatusUpdate, "deleted", project)
				event.StatusUpdate = deleted
				s.emitProjectV2Event(event)
			}
			return map[string]interface{}{
				"deletedStatusUpdateId": deleted.NodeID,
				"projectV2":             projectV2ToGQLFull(s.store, project),
			}, nil
		},
	})
}

// projectV2StatusDates validates the two date members a status-update may carry.
func projectV2StatusDates(input map[string]interface{}) (string, string, error) {
	startDate, _ := input["startDate"].(string)
	targetDate, _ := input["targetDate"].(string)
	if _, err := store.ParseProjectV2Date(startDate); err != nil {
		return "", "", fmt.Errorf("startDate: %w", err)
	}
	if _, err := store.ParseProjectV2Date(targetDate); err != nil {
		return "", "", fmt.Errorf("targetDate: %w", err)
	}
	return startDate, targetDate, nil
}

// projectV2MutationStatusUpdate resolves the status update a mutation names.
func (s *Resolver) projectV2MutationStatusUpdate(input map[string]interface{}, key string) (*store.ProjectV2StatusUpdate, error) {
	nodeID, _ := input[key].(string)
	update := s.store.ProjectsV2.LookupStatusUpdateByNodeID(nodeID)
	if update == nil {
		return nil, &ghNotFoundError{
			message: fmt.Sprintf("Could not resolve to a status update with the global id of '%s'.", nodeID),
		}
	}
	return update, nil
}

// Workflows

func (s *Resolver) addProjectV2WorkflowMutations(mutationType, projectType *graphql.Object) {
	s.registerMutation(mutationType, "deleteProjectV2Workflow", &graphql.Field{
		Type: graphql.NewObject(graphql.ObjectConfig{
			Name: "DeleteProjectV2WorkflowPayload",
			Fields: graphql.Fields{
				"deletedWorkflowId": &graphql.Field{Type: graphql.ID},
				"projectV2":         &graphql.Field{Type: projectType},
			},
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewInputObject(graphql.InputObjectConfig{
			Name: "DeleteProjectV2WorkflowInput",
			Fields: graphql.InputObjectConfigFieldMap{
				"workflowId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			},
		}))}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["workflowId"].(string)
			workflow := s.store.ProjectsV2.LookupWorkflowByNodeID(nodeID)
			if workflow == nil {
				return nil, &ghNotFoundError{
					message: fmt.Sprintf("Could not resolve to a workflow with the global id of '%s'.", nodeID),
				}
			}
			deleted := s.store.ProjectsV2.DeleteWorkflow(workflow.ID)
			if deleted == nil {
				return nil, &ghNotFoundError{message: "Could not resolve to a workflow."}
			}
			s.store.ProjectsV2.TouchProject(deleted.ProjectID)
			project := s.store.ProjectsV2.GetProject(deleted.ProjectID)
			if project != nil {
				s.emitProjectV2Event(s.projectV2Event(p, store.ProjectV2EventProject, "edited", project))
			}
			return map[string]interface{}{
				"deletedWorkflowId": deleted.NodeID,
				"projectV2":         projectV2ToGQLFull(s.store, project),
			}, nil
		},
	})
}

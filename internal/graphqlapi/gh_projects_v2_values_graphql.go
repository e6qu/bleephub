package graphqlapi

import (
	"fmt"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// Projects v2 — the ProjectV2ItemFieldValue union in full. Six members are
// stored values (text, number, date, single select, multi select, iteration),
// each implementing Node and ProjectV2ItemFieldValueCommon; the other six are
// built-in columns read off the item's issue or pull request. All twelve must
// exist — `gh project item-list` sends one fragment naming every member, and
// the query is validated against the whole schema before any resolver runs.

// projectV2FieldValueTypes builds every union member and the union itself.
// graphql-go fixes union members and object interfaces at construction, so all
// twelve members and the common interface are made here in one pass.
func (s *Resolver) projectV2FieldValueTypes() *graphql.Union {
	if s.graphqlTypes.projectV2ItemFieldValueUnionMemo != nil {
		return s.graphqlTypes.projectV2ItemFieldValueUnionMemo
	}
	common := s.projectV2ItemFieldValueCommonInterface()
	stored := s.projectV2StoredValueTypes(common)
	derived := s.projectV2ContentDerivedValueTypes()

	members := append([]*graphql.Object{}, stored...)
	members = append(members, derived...)
	s.graphqlTypes.projectV2ItemFieldValueUnionMemo = graphql.NewUnion(graphql.UnionConfig{
		Name:  "ProjectV2ItemFieldValue",
		Types: members,
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			return s.projectV2ValueTypeFor(p.Value)
		},
	})
	return s.graphqlTypes.projectV2ItemFieldValueUnionMemo
}

// projectV2StoredValueTypes builds the six members carrying a value stored on
// the item, each implementing Node and ProjectV2ItemFieldValueCommon.
func (s *Resolver) projectV2StoredValueTypes(common *graphql.Interface) []*graphql.Object {
	interfaces := []*graphql.Interface{s.graphqlTypes.node, common}
	// The common members name ProjectV2Item, which is built from the item
	// connection that builds this union, so they go behind a thunk.
	withCommon := func(own func() graphql.Fields) graphql.FieldsThunk {
		return func() graphql.Fields {
			fields := own()
			for name, field := range s.projectV2ValueCommonFields() {
				fields[name] = field
			}
			return fields
		}
	}

	s.graphqlTypes.projectV2TextValueMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemFieldTextValue", Interfaces: interfaces,
		Fields: withCommon(func() graphql.Fields {
			return graphql.Fields{
				"text": &graphql.Field{Type: graphql.String, Resolve: sourceKeyResolver("text")},
			}
		}),
	})
	s.graphqlTypes.projectV2NumberValueMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemFieldNumberValue", Interfaces: interfaces,
		Fields: withCommon(func() graphql.Fields {
			return graphql.Fields{
				"number": &graphql.Field{Type: graphql.Float, Resolve: sourceKeyResolver("number")},
			}
		}),
	})
	s.graphqlTypes.projectV2DateValueMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemFieldDateValue", Interfaces: interfaces,
		Fields: withCommon(func() graphql.Fields {
			return graphql.Fields{
				"date": &graphql.Field{Type: s.graphQLStringScalar("Date"), Resolve: sourceKeyResolver("date")},
			}
		}),
	})
	s.graphqlTypes.projectV2SingleSelectValueMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemFieldSingleSelectValue", Interfaces: interfaces,
		Fields: withCommon(func() graphql.Fields {
			return graphql.Fields{
				"optionId": &graphql.Field{Type: graphql.String, Resolve: sourceKeyResolver("optionId")},
				"name":     &graphql.Field{Type: graphql.String, Resolve: sourceKeyResolver("name")},
				"nameHTML": &graphql.Field{Type: graphql.String, Resolve: sourceKeyResolver("name")},
				"color": &graphql.Field{
					Type: graphql.NewNonNull(s.graphQLEnum(
						"ProjectV2SingleSelectFieldOptionColor",
						"BLUE", "GRAY", "GREEN", "ORANGE", "PINK", "PURPLE", "RED", "YELLOW",
					)),
					Resolve: sourceKeyResolver("color"),
				},
				"description": &graphql.Field{Type: graphql.String, Resolve: sourceKeyResolver("description")},
				"descriptionHTML": &graphql.Field{
					Type: graphql.String, Resolve: sourceKeyResolver("description"),
				},
			}
		}),
	})
	s.graphqlTypes.projectV2IterationValueMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemFieldIterationValue", Interfaces: interfaces,
		Fields: withCommon(func() graphql.Fields {
			return graphql.Fields{
				"iterationId": &graphql.Field{Type: graphql.NewNonNull(graphql.String), Resolve: sourceKeyResolver("iterationId")},
				"title":       &graphql.Field{Type: graphql.NewNonNull(graphql.String), Resolve: sourceKeyResolver("title")},
				"titleHTML":   &graphql.Field{Type: graphql.NewNonNull(graphql.String), Resolve: sourceKeyResolver("title")},
				"startDate":   &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("Date")), Resolve: sourceKeyResolver("startDate")},
				"duration":    &graphql.Field{Type: graphql.NewNonNull(graphql.Int), Resolve: sourceKeyResolver("duration")},
			}
		}),
	})
	s.graphqlTypes.projectV2MultiSelectValueMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemFieldMultiSelectValue", Interfaces: interfaces,
		Fields: withCommon(func() graphql.Fields {
			return graphql.Fields{
				"options": &graphql.Field{
					Type:    graphql.NewList(graphql.NewNonNull(s.projectV2MultiSelectOptionType())),
					Resolve: sourceKeyResolver("options"),
				},
				// The comma-separated rendering of the selection.
				"value": &graphql.Field{
					Type:    graphql.NewNonNull(graphql.String),
					Resolve: sourceKeyResolver("value"),
				},
			}
		}),
	})
	return []*graphql.Object{
		s.graphqlTypes.projectV2TextValueMemo,
		s.graphqlTypes.projectV2NumberValueMemo,
		s.graphqlTypes.projectV2DateValueMemo,
		s.graphqlTypes.projectV2SingleSelectValueMemo,
		s.graphqlTypes.projectV2IterationValueMemo,
		s.graphqlTypes.projectV2MultiSelectValueMemo,
	}
}

// projectV2MultiSelectOptionType is the option object a multi-select value
// lists, memoized so its name is declared once.
func (s *Resolver) projectV2MultiSelectOptionType() *graphql.Object {
	if s.graphqlTypes.projectV2MultiSelectOption != nil {
		return s.graphqlTypes.projectV2MultiSelectOption
	}
	s.graphqlTypes.projectV2MultiSelectOption = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2MultiSelectFieldOption",
		Fields: graphql.Fields{
			"id":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"name": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"nameHTML": &graphql.Field{
				Type: graphql.NewNonNull(graphql.String), Resolve: sourceKeyResolver("name"),
			},
			"description": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"descriptionHTML": &graphql.Field{
				Type: graphql.NewNonNull(graphql.String), Resolve: sourceKeyResolver("description"),
			},
			"color": &graphql.Field{Type: graphql.NewNonNull(s.graphQLEnum(
				"ProjectV2SingleSelectFieldOptionColor",
				"BLUE", "GRAY", "GREEN", "ORANGE", "PINK", "PURPLE", "RED", "YELLOW",
			))},
		},
	})
	return s.graphqlTypes.projectV2MultiSelectOption
}

// projectV2ItemFieldValueCommonInterface is the interface the stored value
// members share, using the union's discriminator as its ResolveType.
func (s *Resolver) projectV2ItemFieldValueCommonInterface() *graphql.Interface {
	if s.graphqlTypes.projectV2ValueCommonInterface != nil {
		return s.graphqlTypes.projectV2ValueCommonInterface
	}
	dateTime := s.graphQLStringScalar("DateTime")
	s.graphqlTypes.projectV2ValueCommonInterface = graphql.NewInterface(graphql.InterfaceConfig{
		Name: "ProjectV2ItemFieldValueCommon",
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
				"databaseId": &graphql.Field{Type: graphql.Int},
				"createdAt":  &graphql.Field{Type: graphql.NewNonNull(dateTime)},
				"updatedAt":  &graphql.Field{Type: graphql.NewNonNull(dateTime)},
				"creator":    &graphql.Field{Type: s.graphqlTypes.actor},
				"field":      &graphql.Field{Type: graphql.NewNonNull(s.projectV2FieldConfigurationUnion())},
				"item":       &graphql.Field{Type: graphql.NewNonNull(s.projectV2ItemType())},
			}
		}),
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			return s.projectV2ValueTypeFor(p.Value)
		},
	})
	return s.graphqlTypes.projectV2ValueCommonInterface
}

// projectV2ValueTypeFor maps a rendered value's discriminator onto its concrete
// type. Shared by the union and the common interface so both agree.
func (s *Resolver) projectV2ValueTypeFor(value interface{}) *graphql.Object {
	src, _ := value.(map[string]interface{})
	switch src["kind"] {
	case string(store.ProjectV2FieldText):
		return s.graphqlTypes.projectV2TextValueMemo
	case string(store.ProjectV2FieldNumber):
		return s.graphqlTypes.projectV2NumberValueMemo
	case string(store.ProjectV2FieldDate):
		return s.graphqlTypes.projectV2DateValueMemo
	case string(store.ProjectV2FieldIteration):
		return s.graphqlTypes.projectV2IterationValueMemo
	case string(store.ProjectV2FieldMultiSelect):
		return s.graphqlTypes.projectV2MultiSelectValueMemo
	case "LABELS":
		return s.graphqlTypes.projectV2LabelValueMemo
	case "MILESTONE":
		return s.graphqlTypes.projectV2MilestoneValueMemo
	case "REPOSITORY":
		return s.graphqlTypes.projectV2RepositoryValueMemo
	case "ASSIGNEES":
		return s.graphqlTypes.projectV2UserValueMemo
	case "REVIEWERS":
		return s.graphqlTypes.projectV2ReviewerValueMemo
	case "LINKED_PULL_REQUESTS":
		return s.graphqlTypes.projectV2PullRequestValueMemo
	default:
		return s.graphqlTypes.projectV2SingleSelectValueMemo
	}
}

// projectV2ValueCommonFields is the ProjectV2ItemFieldValueCommon member set,
// built fresh per type because graphql-go binds a *graphql.Field to its object.
func (s *Resolver) projectV2ValueCommonFields() graphql.Fields {
	dateTime := s.graphQLStringScalar("DateTime")
	return graphql.Fields{
		"id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID), Resolve: sourceKeyResolver("valueNodeID")},
		"databaseId": &graphql.Field{Type: graphql.Int, Resolve: sourceKeyResolver("databaseId")},
		"createdAt":  &graphql.Field{Type: graphql.NewNonNull(dateTime), Resolve: sourceKeyResolver("createdAt")},
		"updatedAt":  &graphql.Field{Type: graphql.NewNonNull(dateTime), Resolve: sourceKeyResolver("updatedAt")},
		"creator":    &graphql.Field{Type: s.graphqlTypes.actor, Resolve: sourceKeyResolver("creator")},
		"field": &graphql.Field{
			Type:    graphql.NewNonNull(s.projectV2FieldConfigurationUnion()),
			Resolve: sourceKeyResolver("field"),
		},
		"item": &graphql.Field{
			Type: graphql.NewNonNull(s.projectV2ItemType()),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, ok := p.Source.(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
				}
				itemNodeID, _ := src["itemNodeID"].(string)
				item := s.store.ProjectsV2.LookupItemByNodeID(itemNodeID)
				if item == nil {
					return nil, fmt.Errorf("field value is not attached to an item")
				}
				return projectV2ItemToGQL(item, s.store), nil
			},
		},
	}
}

// projectV2ContentDerivedValueTypes builds the six members whose value is read
// off the item's content rather than stored on the item.
func (s *Resolver) projectV2ContentDerivedValueTypes() []*graphql.Object {
	// Every member is behind a thunk: these are built while the issue family
	// assembles, before the pull-request family creates the
	// PullRequestConnection and RequestedReviewer types they name.
	fieldField := func() *graphql.Field {
		return &graphql.Field{
			Type:    graphql.NewNonNull(s.projectV2FieldConfigurationUnion()),
			Resolve: sourceKeyResolver("field"),
		}
	}
	connectionOf := func(key string) graphql.FieldResolveFn {
		return func(p graphql.ResolveParams) (interface{}, error) {
			src, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			nodes, _ := src[key].([]map[string]interface{})
			return paginateGQLMaps(nodes, p.Args), nil
		}
	}

	s.graphqlTypes.projectV2LabelValueMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemFieldLabelValue",
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"field": fieldField(),
				"labels": &graphql.Field{
					Type:    s.gqlLabelConnectionType(),
					Args:    relayConnectionArgs(),
					Resolve: connectionOf("labels"),
				},
			}
		}),
	})
	s.graphqlTypes.projectV2MilestoneValueMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemFieldMilestoneValue",
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"field":     fieldField(),
				"milestone": &graphql.Field{Type: s.graphqlTypes.milestone, Resolve: sourceKeyResolver("milestone")},
			}
		}),
	})
	s.graphqlTypes.projectV2RepositoryValueMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemFieldRepositoryValue",
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"field":      fieldField(),
				"repository": &graphql.Field{Type: s.graphqlTypes.repository, Resolve: sourceKeyResolver("repository")},
			}
		}),
	})
	s.graphqlTypes.projectV2UserValueMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemFieldUserValue",
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"field": fieldField(),
				"users": &graphql.Field{
					Type:    s.gqlUserConnectionType(s.graphqlTypes.user),
					Args:    relayConnectionArgs(),
					Resolve: connectionOf("users"),
				},
			}
		}),
	})
	s.graphqlTypes.projectV2ReviewerValueMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemFieldReviewerValue",
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"field": fieldField(),
				"reviewers": &graphql.Field{
					Type:    s.gqlConnectionType("RequestedReviewer", s.graphqlTypes.requestedReviewerUnion),
					Args:    relayConnectionArgs(),
					Resolve: connectionOf("reviewers"),
				},
			}
		}),
	})
	s.graphqlTypes.projectV2PullRequestValueMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemFieldPullRequestValue",
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"field": fieldField(),
				"pullRequests": &graphql.Field{
					Type:    s.graphqlTypes.pullRequestConnection,
					Args:    relayConnectionArgs(),
					Resolve: connectionOf("pullRequests"),
				},
			}
		}),
	})
	return []*graphql.Object{
		s.graphqlTypes.projectV2LabelValueMemo,
		s.graphqlTypes.projectV2MilestoneValueMemo,
		s.graphqlTypes.projectV2RepositoryValueMemo,
		s.graphqlTypes.projectV2UserValueMemo,
		s.graphqlTypes.projectV2ReviewerValueMemo,
		s.graphqlTypes.projectV2PullRequestValueMemo,
	}
}

// projectV2BuiltInFieldValue renders one built-in column for an item, reading it
// off the item's issue or pull request. A draft issue has no content, so every
// built-in column is null on it, matching github.com.
func projectV2BuiltInFieldValue(st *store.Store, it *store.ProjectV2Item, field *store.ProjectV2Field) map[string]interface{} {
	out := map[string]interface{}{
		"kind":  string(field.DataType),
		"field": projectV2FieldToGQL(field),
	}
	var repoID int
	var labelIDs, assigneeIDs []int
	var milestoneID int
	switch it.ContentType {
	case "Issue":
		issue := st.GetIssue(it.ContentID)
		if issue == nil {
			return nil
		}
		repoID, labelIDs, assigneeIDs, milestoneID = issue.RepoID, issue.LabelIDs, issue.AssigneeIDs, issue.MilestoneID
	case "PullRequest":
		pr := st.GetPullRequest(it.ContentID)
		if pr == nil {
			return nil
		}
		repoID, labelIDs, assigneeIDs, milestoneID = pr.RepoID, pr.LabelIDs, pr.AssigneeIDs, pr.MilestoneID
	default:
		return nil
	}

	switch field.DataType {
	case "LABELS":
		nodes := make([]map[string]interface{}, 0, len(labelIDs))
		for _, labelID := range labelIDs {
			label := st.GetLabel(labelID)
			if label == nil {
				continue
			}
			nodes = append(nodes, map[string]interface{}{
				"nodeID": label.NodeID, "name": label.Name,
				"description": label.Description, "color": label.Color,
			})
		}
		if len(nodes) == 0 {
			return nil
		}
		out["labels"] = nodes
	case "ASSIGNEES":
		nodes := make([]map[string]interface{}, 0, len(assigneeIDs))
		for _, id := range assigneeIDs {
			if u := st.GetUserByID(id); u != nil {
				nodes = append(nodes, userToGraphQL(u))
			}
		}
		if len(nodes) == 0 {
			return nil
		}
		out["users"] = nodes
	case "REPOSITORY":
		repo := st.GetRepoByID(repoID)
		if repo == nil {
			return nil
		}
		out["repository"] = repoToGraphQL(st, repo)
	case "MILESTONE":
		milestone := st.GetMilestone(milestoneID)
		if milestone == nil {
			return nil
		}
		out["milestone"] = map[string]interface{}{
			"nodeID": milestone.NodeID, "number": milestone.Number,
			"title": milestone.Title, "description": milestone.Description,
			"state": string(milestone.State),
		}
	case "REVIEWERS", "LINKED_PULL_REQUESTS", "TITLE":
		// TITLE is the item's own title, not a field value; reviewers and linked
		// pull requests have no stored backing here. An unset column is absent
		// from the connection, as GitHub reports it.
		return nil
	default:
		return nil
	}
	return out
}

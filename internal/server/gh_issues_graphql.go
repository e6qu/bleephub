package bleephub

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/graphql-go/graphql"
)

// addIssueFieldsToSchema adds Issue types, queries, and mutations to the schema.
func (s *Server) addIssueFieldsToSchema(userType, repoType, mutationType, queryType *graphql.Object, nodeInterface *graphql.Interface) (*graphql.Object, *graphql.Object) {
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	commentAuthorAssociationEnum := s.graphQLEnum(
		"CommentAuthorAssociation",
		"COLLABORATOR", "CONTRIBUTOR", "FIRST_TIMER", "FIRST_TIME_CONTRIBUTOR",
		"MANNEQUIN", "MEMBER", "NONE", "OWNER",
	)
	lockReasonEnum := s.graphQLEnum("LockReason", "OFF_TOPIC", "RESOLVED", "SPAM", "TOO_HEATED")
	issueStateReasonEnum := s.graphQLEnum("IssueStateReason", "COMPLETED", "NOT_PLANNED", "REOPENED")
	issueStateEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "IssueState",
		Values: graphql.EnumValueConfigMap{
			"OPEN":   &graphql.EnumValueConfig{Value: "OPEN"},
			"CLOSED": &graphql.EnumValueConfig{Value: "CLOSED"},
		},
	})
	issueClosedStateReasonEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "IssueClosedStateReason",
		Values: graphql.EnumValueConfigMap{
			"COMPLETED":   &graphql.EnumValueConfig{Value: "COMPLETED"},
			"NOT_PLANNED": &graphql.EnumValueConfig{Value: "NOT_PLANNED"},
		},
	})
	milestoneStateEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "MilestoneState",
		Values: graphql.EnumValueConfigMap{
			"OPEN":   &graphql.EnumValueConfig{Value: "OPEN"},
			"CLOSED": &graphql.EnumValueConfig{Value: "CLOSED"},
		},
	})
	// --- Label types ---
	issueLabelType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Label",
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					l, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("label source: unexpected type %T", p.Source)
					}
					return l["nodeID"], nil
				},
			},
			"name":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"description": &graphql.Field{Type: graphql.String},
			"color":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})

	labelConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "LabelConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(issueLabelType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})

	// --- Milestone type ---
	issueMilestoneType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Milestone",
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					m, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("milestone source: unexpected type %T", p.Source)
					}
					return m["nodeID"], nil
				},
			},
			"number":      &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"title":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"description": &graphql.Field{Type: graphql.String},
			"state":       &graphql.Field{Type: graphql.NewNonNull(milestoneStateEnum)},
			"dueOn":       &graphql.Field{Type: dateTime},
		},
	})

	milestoneConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "MilestoneConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(issueMilestoneType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})

	// --- Reaction group type (static) ---
	reactionGroupType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ReactionGroup",
		Fields: graphql.Fields{
			"content": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"users": &graphql.Field{
				Type: graphql.NewObject(graphql.ObjectConfig{
					Name: "ReactingUserConnection",
					Fields: graphql.Fields{
						"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
					},
				}),
			},
		},
	})

	// --- Comment types ---
	issueCommentType := graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueComment",
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return c["nodeID"], nil
				},
			},
			"body":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"url":       &graphql.Field{Type: graphql.NewNonNull(uri)},
			"createdAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"author": &graphql.Field{
				Type: s.graphqlTypes.actor,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return c["author"], nil
				},
			},
			"authorAssociation": &graphql.Field{Type: graphql.NewNonNull(commentAuthorAssociationEnum)},
			// Fields gh CLI's `gh issue view` queries on IssueComment — defaults
			// fine for bleephub (we don't model edit history or moderation).
			"includesCreatedEdit": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return c["includesCreatedEdit"], nil
				},
			},
			"lastEditedAt": &graphql.Field{
				Type: dateTime,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return c["lastEditedAt"], nil
				},
			},
			"editor": &graphql.Field{
				Type: s.graphqlTypes.actor,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return c["editor"], nil
				},
			},
			"isMinimized": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return c["isMinimized"], nil
				},
			},
			"isPinned": &graphql.Field{
				Type: graphql.Boolean,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return c["isPinned"], nil
				},
			},
			"minimizedReason": &graphql.Field{
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return c["minimizedReason"], nil
				},
			},
			"reactionGroups": &graphql.Field{
				Type: graphql.NewList(reactionGroupType),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return c["reactionGroups"], nil
				},
			},
			// gh's shared comments fragment (issue view + pr view) selects
			// viewerDidAuthor; mirrors PRComment's resolver.
			"viewerDidAuthor": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					viewer := ghUserFromContext(p.Context)
					authorID, _ := c["authorID"].(int)
					return viewer != nil && authorID == viewer.ID, nil
				},
			},
		},
	})

	commentConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueCommentConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(issueCommentType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})

	// --- Assignee connection ---

	assigneeConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "UserConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(userType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})

	// --- Issue-type and sub-issue support types ---
	// gh CLI's `gh issue view` selects GitHub's issue-type and sub-issue
	// fields. Issue types resolve from the organization definitions assigned
	// to the issue row. Sub-issues are backed by the same ordered store links
	// used by the REST API.
	issueTypeMetaType := graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueType",
		Fields: graphql.Fields{
			"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"name":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"description": &graphql.Field{Type: graphql.String},
			"color": &graphql.Field{Type: graphql.NewNonNull(s.graphQLEnum(
				"IssueTypeColor",
				"BLUE", "GRAY", "GREEN", "ORANGE", "PINK", "PURPLE", "RED", "YELLOW",
			))},
		},
	})

	relatedIssueRepoType := graphql.NewObject(graphql.ObjectConfig{
		Name: "RelatedIssueRepository",
		Fields: graphql.Fields{
			"nameWithOwner": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})

	relatedIssueType := graphql.NewObject(graphql.ObjectConfig{
		Name: "RelatedIssue",
		Fields: graphql.Fields{
			"id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"number":     &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"title":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"url":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"state":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"repository": &graphql.Field{Type: relatedIssueRepoType},
		},
	})

	subIssueConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "SubIssueConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(relatedIssueType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})

	subIssuesSummaryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "SubIssuesSummary",
		Fields: graphql.Fields{
			"total":            &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"completed":        &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"percentCompleted": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})

	issueFieldValueConnectionType := s.issueFieldValueGraphQLConnectionType()

	// --- Issue type ---
	issueType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "Issue",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					i, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return i["nodeID"], nil
				},
			},
			"databaseId":  &graphql.Field{Type: graphql.Int},
			"number":      &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"title":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"body":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"state":       &graphql.Field{Type: graphql.NewNonNull(issueStateEnum)},
			"stateReason": &graphql.Field{Type: issueStateReasonEnum},
			// gh's shared issue/PR field set selects `closed`.
			"closed": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					i, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					state, _ := i["state"].(string)
					return state == "CLOSED", nil
				},
			},
			"url":              &graphql.Field{Type: graphql.NewNonNull(uri)},
			"createdAt":        &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":        &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"closedAt":         &graphql.Field{Type: dateTime},
			"isPinned":         &graphql.Field{Type: graphql.Boolean},
			"locked":           &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"activeLockReason": &graphql.Field{Type: lockReasonEnum},
			"author": &graphql.Field{
				Type: s.graphqlTypes.actor,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					i, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return i["author"], nil
				},
			},
			"labels": &graphql.Field{
				Type: labelConnectionType,
				Args: graphql.FieldConfigArgument{
					"first": &graphql.ArgumentConfig{Type: graphql.Int},
					"after": &graphql.ArgumentConfig{Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					i, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return repaginateConnection(i["labels"], p.Args), nil
				},
			},
			"assignees": &graphql.Field{
				Type: graphql.NewNonNull(assigneeConnectionType),
				Args: graphql.FieldConfigArgument{
					"first": &graphql.ArgumentConfig{Type: graphql.Int},
					"after": &graphql.ArgumentConfig{Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					i, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return repaginateConnection(i["assignees"], p.Args), nil
				},
			},
			"milestone": &graphql.Field{
				Type: issueMilestoneType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					i, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					m, ok := i["milestone"].(map[string]interface{})
					if !ok || m == nil {
						// graphql-go's NonNull checks fire even on a nil-valued
						// map[string]interface{}; return untyped nil so the field
						// resolves to null cleanly.
						return nil, nil
					}
					return m, nil
				},
			},
			// ProjectV2 items — gh CLI's `gh issue view` queries Issue.projectItems
			// as a second round-trip. Returns the real ProjectV2Item nodes the
			// issue has been added to via addProjectV2ItemById.
			"projectItems": &graphql.Field{
				Type: s.projectV2ItemConnectionType(),
				Args: relayConnectionArgs(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					i, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					issueID, _ := i["databaseId"].(int)
					return projectItemsConnectionForIssue(s.store, issueID, p.Args), nil
				},
			},
			"comments": &graphql.Field{
				Type: graphql.NewNonNull(commentConnectionType),
				Args: graphql.FieldConfigArgument{
					"first":  &graphql.ArgumentConfig{Type: graphql.Int},
					"last":   &graphql.ArgumentConfig{Type: graphql.Int},
					"after":  &graphql.ArgumentConfig{Type: graphql.String},
					"before": &graphql.ArgumentConfig{Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					i, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return repaginateConnection(i["comments"], p.Args), nil
				},
			},
			"reactionGroups": &graphql.Field{
				Type: graphql.NewList(graphql.NewNonNull(reactionGroupType)),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					i, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return i["reactionGroups"], nil
				},
			},
			"issueType": &graphql.Field{
				Type: issueTypeMetaType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					i, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					it, ok := i["issueType"].(map[string]interface{})
					if !ok || it == nil {
						return nil, nil
					}
					return it, nil
				},
			},
			"parent": &graphql.Field{
				Type: relatedIssueType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					i, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					parent, ok := i["parent"].(map[string]interface{})
					if !ok || parent == nil {
						return nil, nil
					}
					return parent, nil
				},
			},
			"subIssues": &graphql.Field{
				Type: subIssueConnectionType,
				Args: graphql.FieldConfigArgument{
					"first": &graphql.ArgumentConfig{Type: graphql.Int},
					"after": &graphql.ArgumentConfig{Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					i, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return repaginateConnection(i["subIssues"], p.Args), nil
				},
			},
			"subIssuesSummary": &graphql.Field{
				Type: graphql.NewNonNull(subIssuesSummaryType),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					i, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return i["subIssuesSummary"], nil
				},
			},
			"issueFieldValues": &graphql.Field{
				Type: issueFieldValueConnectionType,
				Args: graphql.FieldConfigArgument{
					"first":  &graphql.ArgumentConfig{Type: graphql.Int},
					"last":   &graphql.ArgumentConfig{Type: graphql.Int},
					"after":  &graphql.ArgumentConfig{Type: graphql.String},
					"before": &graphql.ArgumentConfig{Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					i, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return repaginateConnection(i["issueFieldValues"], p.Args), nil
				},
			},
		},
	})

	// --- Issue connection ---
	issueEdgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueEdge",
		Fields: graphql.Fields{
			"node":   &graphql.Field{Type: issueType},
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})

	issueConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(issueType)},
			"edges":      &graphql.Field{Type: graphql.NewList(issueEdgeType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})

	// --- Issue filters input ---
	issueFiltersInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "IssueFilters",
		Fields: graphql.InputObjectConfigFieldMap{
			"assignee":  &graphql.InputObjectFieldConfig{Type: graphql.String},
			"createdBy": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"mentioned": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"labels":    &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"states":    &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(issueStateEnum))},
		},
	})

	// --- Add fields to Repository type ---

	repoType.AddFieldConfig("hasIssuesEnabled", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			v, ok := r["hasIssues"].(bool)
			if !ok {
				return nil, fmt.Errorf("repository source missing hasIssues")
			}
			return v, nil
		},
	})

	repoType.AddFieldConfig("viewerPermission", &graphql.Field{
		Type: s.graphQLEnum("RepositoryPermission", "ADMIN", "MAINTAIN", "READ", "TRIAGE", "WRITE"),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			// Real GitHub computes viewerPermission from the viewer's actual
			// access — ADMIN/WRITE/READ (bleephub models pull/push/admin; it
			// does not track MAINTAIN/TRIAGE). Return null for no access.
			src, _ := p.Source.(map[string]interface{})
			fullName, _ := src["nameWithOwner"].(string)
			parts := strings.SplitN(fullName, "/", 2)
			if len(parts) != 2 {
				return nil, nil
			}
			repo := s.store.GetRepo(parts[0], parts[1])
			if repo == nil {
				return nil, nil
			}
			switch {
			case s.viewerCanAdminRepo(p.Context, repo):
				return "ADMIN", nil
			case s.viewerCanPushRepo(p.Context, repo):
				return "WRITE", nil
			case s.viewerCanReadRepo(p.Context, repo):
				return "READ", nil
			default:
				return nil, nil
			}
		},
	})

	repoType.AddFieldConfig("mergeCommitAllowed", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			v, ok := r["allowMergeCommit"].(bool)
			if !ok {
				return nil, fmt.Errorf("repository source missing allowMergeCommit")
			}
			return v, nil
		},
	})

	repoType.AddFieldConfig("rebaseMergeAllowed", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			v, ok := r["allowRebaseMerge"].(bool)
			if !ok {
				return nil, fmt.Errorf("repository source missing allowRebaseMerge")
			}
			return v, nil
		},
	})

	repoType.AddFieldConfig("squashMergeAllowed", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			r, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			v, ok := r["allowSquashMerge"].(bool)
			if !ok {
				return nil, fmt.Errorf("repository source missing allowSquashMerge")
			}
			return v, nil
		},
	})

	// IssueOrderField + OrderDirection enums — gh CLI sends enum names like
	// CREATED_AT / DESC, not strings.
	issueOrderFieldEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "IssueOrderField",
		Values: graphql.EnumValueConfigMap{
			"CREATED_AT": &graphql.EnumValueConfig{Value: "CREATED_AT"},
			"UPDATED_AT": &graphql.EnumValueConfig{Value: "UPDATED_AT"},
			"COMMENTS":   &graphql.EnumValueConfig{Value: "COMMENTS"},
		},
	})
	issueOrderDirectionEnum := s.graphQLEnum("OrderDirection", "ASC", "DESC")
	issueOrderInput := s.gqlIssueOrderType(issueOrderFieldEnum, issueOrderDirectionEnum)

	repoType.AddFieldConfig("issues", &graphql.Field{
		Type: graphql.NewNonNull(issueConnectionType),
		Args: graphql.FieldConfigArgument{
			"first":    &graphql.ArgumentConfig{Type: graphql.Int},
			"after":    &graphql.ArgumentConfig{Type: graphql.String},
			"states":   &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(issueStateEnum))},
			"labels":   &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"filterBy": &graphql.ArgumentConfig{Type: issueFiltersInput},
			"orderBy":  &graphql.ArgumentConfig{Type: issueOrderInput},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, _ := repo["databaseId"].(int)
			repoFullName, _ := repo["nameWithOwner"].(string)

			storedIssues := s.store.ListIssues(repoID, "")
			issues := make([]*Issue, 0, len(storedIssues))
			for _, issue := range storedIssues {
				issues = append(issues, s.store.snapIssue(issue))
			}

			// Filter by states arg
			if states, ok := p.Args["states"].([]interface{}); ok && len(states) > 0 {
				stateMap := make(map[string]bool)
				for _, st := range states {
					stateMap[fmt.Sprintf("%v", st)] = true
				}
				var filtered []*Issue
				for _, i := range issues {
					if stateMap[i.State] {
						filtered = append(filtered, i)
					}
				}
				issues = filtered
			}

			// Filter by labels arg
			if labelNames, ok := p.Args["labels"].([]interface{}); ok && len(labelNames) > 0 {
				var names []string
				for _, ln := range labelNames {
					names = append(names, fmt.Sprintf("%v", ln))
				}
				var filtered []*Issue
				for _, i := range issues {
					if issueHasAllLabels(s.store, i, names, repoID) {
						filtered = append(filtered, i)
					}
				}
				issues = filtered
			}

			// Filter by filterBy
			if filterBy, ok := p.Args["filterBy"].(map[string]interface{}); ok {
				if assignee, ok := filterBy["assignee"].(string); ok && assignee != "" {
					u := s.store.LookupUserByLogin(assignee)
					var filtered []*Issue
					if u != nil {
						for _, i := range issues {
							for _, aid := range i.AssigneeIDs {
								if aid == u.ID {
									filtered = append(filtered, i)
									break
								}
							}
						}
					}
					// An unresolved login means no issue can match. Keeping the
					// old list silently widened a typo to every issue.
					issues = filtered
				}
				if creator, ok := filterBy["createdBy"].(string); ok && creator != "" {
					u := s.store.LookupUserByLogin(creator)
					var filtered []*Issue
					if u != nil {
						for _, issue := range issues {
							if issue.AuthorID == u.ID {
								filtered = append(filtered, issue)
							}
						}
					}
					issues = filtered
				}
				if states, ok := filterBy["states"].([]interface{}); ok && len(states) > 0 {
					allowed := map[string]bool{}
					for _, state := range states {
						allowed[fmt.Sprint(state)] = true
					}
					var filtered []*Issue
					for _, issue := range issues {
						if allowed[issue.State] {
							filtered = append(filtered, issue)
						}
					}
					issues = filtered
				}
				if labels, ok := filterBy["labels"].([]interface{}); ok && len(labels) > 0 {
					names := make([]string, 0, len(labels))
					for _, label := range labels {
						names = append(names, fmt.Sprint(label))
					}
					var filtered []*Issue
					for _, issue := range issues {
						if issueHasAllLabels(s.store, issue, names, repoID) {
							filtered = append(filtered, issue)
						}
					}
					issues = filtered
				}
				if mentioned, ok := filterBy["mentioned"].(string); ok && mentioned != "" {
					needle := "@" + strings.ToLower(mentioned)
					var filtered []*Issue
					for _, issue := range issues {
						found := strings.Contains(strings.ToLower(issue.Body), needle)
						if !found {
							for _, comment := range s.store.ListIssueComments(repoFullName, issue.Number) {
								if strings.Contains(strings.ToLower(comment.Body), needle) {
									found = true
									break
								}
							}
						}
						if found {
							filtered = append(filtered, issue)
						}
					}
					issues = filtered
				}
			}

			orderField, orderDirection := "CREATED_AT", "DESC"
			if order, ok := p.Args["orderBy"].(map[string]interface{}); ok {
				if field, ok := order["field"].(string); ok && field != "" {
					orderField = field
				}
				if direction, ok := order["direction"].(string); ok && direction != "" {
					orderDirection = direction
				}
			}
			sort.SliceStable(issues, func(a, b int) bool {
				var comparison int
				switch orderField {
				case "UPDATED_AT":
					comparison = issues[a].UpdatedAt.Compare(issues[b].UpdatedAt)
				case "COMMENTS":
					left := len(s.store.ListComments(issues[a].ID))
					right := len(s.store.ListComments(issues[b].ID))
					switch {
					case left < right:
						comparison = -1
					case left > right:
						comparison = 1
					}
				default:
					comparison = issues[a].CreatedAt.Compare(issues[b].CreatedAt)
				}
				if comparison == 0 {
					comparison = issues[a].Number - issues[b].Number
				}
				if orderDirection == "DESC" {
					return comparison > 0
				}
				return comparison < 0
			})

			first := 30
			if f, ok := intArg(p.Args, "first"); ok && f > 0 {
				first = f
			}
			after, _ := p.Args["after"].(string)

			return paginateIssuesGQL(issues, s.store, first, after), nil
		},
	})

	repoType.AddFieldConfig("issue", &graphql.Field{
		Type: issueType,
		Args: graphql.FieldConfigArgument{
			"number": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, _ := repo["databaseId"].(int)
			number, _ := intArg(p.Args, "number")

			issue := s.store.GetIssueByNumber(repoID, number)
			if issue == nil {
				// Real GitHub returns a typed NOT_FOUND error, not bare null.
				return nil, &ghNotFoundError{
					message: fmt.Sprintf("Could not resolve to an Issue with the number of %d.", number),
				}
			}
			return issueToGQL(issue, s.store), nil
		},
	})

	// issueOrPullRequest is defined in addPullRequestFieldsToSchema (after the
	// PullRequest type exists), so it can return a union of Issue|PullRequest.
	// gh CLI's `gh issue view <N>` uses `...on Issue` + `...on PullRequest`
	// fragments which require a real union return type.

	repoType.AddFieldConfig("labels", &graphql.Field{
		Type: labelConnectionType,
		Args: graphql.FieldConfigArgument{
			"first":  &graphql.ArgumentConfig{Type: graphql.Int},
			"last":   &graphql.ArgumentConfig{Type: graphql.Int},
			"after":  &graphql.ArgumentConfig{Type: graphql.String},
			"before": &graphql.ArgumentConfig{Type: graphql.String},
			"query":  &graphql.ArgumentConfig{Type: graphql.String},
			// gh sends literal enum names (gh label list/create issue
			// `labels(orderBy: {field: NAME, direction: ASC})`), so
			// field/direction must be enums — string-typed inputs reject
			// the literals.
			"orderBy": &graphql.ArgumentConfig{Type: graphql.NewInputObject(graphql.InputObjectConfig{
				Name: "LabelOrder",
				Fields: graphql.InputObjectConfigFieldMap{
					"field": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.NewEnum(graphql.EnumConfig{
						Name: "LabelOrderField",
						Values: graphql.EnumValueConfigMap{
							"NAME":       &graphql.EnumValueConfig{Value: "NAME"},
							"CREATED_AT": &graphql.EnumValueConfig{Value: "CREATED_AT"},
						},
					}))},
					"direction": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(s.graphQLEnum("OrderDirection", "ASC", "DESC"))},
				},
			})},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, _ := repo["databaseId"].(int)

			labels := s.store.ListLabels(repoID)

			// Filter by query
			if q, ok := p.Args["query"].(string); ok && q != "" {
				q = strings.ToLower(q)
				var filtered []*IssueLabel
				for _, l := range labels {
					if strings.Contains(strings.ToLower(l.Name), q) {
						filtered = append(filtered, l)
					}
				}
				labels = filtered
			}

			if order, ok := p.Args["orderBy"].(map[string]interface{}); ok {
				field, _ := order["field"].(string)
				direction, _ := order["direction"].(string)
				sort.Slice(labels, func(i, j int) bool {
					var less bool
					if field == "CREATED_AT" {
						less = labels[i].CreatedAt.Before(labels[j].CreatedAt)
					} else {
						less = strings.ToLower(labels[i].Name) < strings.ToLower(labels[j].Name)
					}
					if direction == "DESC" {
						return !less
					}
					return less
				})
			}
			nodes := make([]map[string]interface{}, 0, len(labels))
			for _, label := range labels {
				nodes = append(nodes, labelToGQL(label))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})

	repoType.AddFieldConfig("milestones", &graphql.Field{
		Type: milestoneConnectionType,
		Args: graphql.FieldConfigArgument{
			"first":  &graphql.ArgumentConfig{Type: graphql.Int},
			"after":  &graphql.ArgumentConfig{Type: graphql.String},
			"states": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(milestoneStateEnum))},
			"query":  &graphql.ArgumentConfig{Type: graphql.String},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, _ := repo["databaseId"].(int)

			state := ""
			if states, ok := p.Args["states"].([]interface{}); ok && len(states) > 0 {
				// Use first state as filter (or "all" if multiple)
				if len(states) == 1 {
					state = strings.ToLower(fmt.Sprintf("%v", states[0]))
				}
			}

			milestones := s.store.ListMilestones(repoID, state)

			// Filter by query
			if q, ok := p.Args["query"].(string); ok && q != "" {
				q = strings.ToLower(q)
				var filtered []*Milestone
				for _, ms := range milestones {
					if strings.Contains(strings.ToLower(ms.Title), q) {
						filtered = append(filtered, ms)
					}
				}
				milestones = filtered
			}

			first := 0
			if f, ok := intArg(p.Args, "first"); ok {
				first = f
			}
			after, _ := p.Args["after"].(string)
			return paginateGQL(milestones, first, after, milestoneToGQL, func(m *Milestone) string { return m.NodeID }), nil
		},
	})

	repoType.AddFieldConfig("assignableUsers", &graphql.Field{
		Type: graphql.NewNonNull(assigneeConnectionType),
		Args: graphql.FieldConfigArgument{
			"first": &graphql.ArgumentConfig{Type: graphql.Int},
			"after": &graphql.ArgumentConfig{Type: graphql.String},
			"query": &graphql.ArgumentConfig{Type: graphql.String},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			source, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, _ := source["databaseId"].(int)
			repo := s.store.GetRepoByID(repoID)
			if repo == nil {
				return nil, fmt.Errorf("repository not found")
			}
			usersByID := map[int]*User{}
			if repo.OwnerType == "User" && repo.Owner != nil {
				usersByID[repo.Owner.ID] = repo.Owner
			}
			if repo.OwnerType == "Organization" {
				for _, member := range s.store.ListOrgMembers(repo.Owner.Login) {
					usersByID[member.ID] = member
				}
			}
			owner, name, _ := strings.Cut(repo.FullName, "/")
			for login := range s.store.ListRepoCollaborators(owner, name) {
				if user := s.store.LookupUserByLogin(login); user != nil {
					usersByID[user.ID] = user
				}
			}
			users := make([]*User, 0, len(usersByID))
			for _, user := range usersByID {
				users = append(users, user)
			}

			// Filter by query
			if q, ok := p.Args["query"].(string); ok && q != "" {
				q = strings.ToLower(q)
				var filtered []*User
				for _, u := range users {
					if strings.Contains(strings.ToLower(u.Login), q) || strings.Contains(strings.ToLower(u.Name), q) {
						filtered = append(filtered, u)
					}
				}
				users = filtered
			}

			// assignableUsers iterates a Go map, so order is nondeterministic;
			// sort by ID to make cursor pagination stable across pages.
			sort.Slice(users, func(a, b int) bool { return users[a].ID < users[b].ID })

			first := 0
			if f, ok := intArg(p.Args, "first"); ok {
				first = f
			}
			after, _ := p.Args["after"].(string)
			return paginateGQL(users, first, after, userToGraphQL, func(u *User) string { return u.NodeID }), nil
		},
	})

	// --- Mutations ---

	createIssueInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreateIssueInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"repositoryId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"title":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"body":         &graphql.InputObjectFieldConfig{Type: graphql.String},
			"labelIds":     &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.ID))},
			"milestoneId":  &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"assigneeIds":  &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.ID))},
			"issueTypeId":  &graphql.InputObjectFieldConfig{Type: graphql.ID},
			// gh's IssueCreate mutation always serializes projectIds (null
			// unless --project) and issueTemplate when a template applies — the
			// input must declare them or variable coercion rejects the whole
			// mutation. projectIds, when supplied, add the issue to those
			// ProjectV2 boards (see the resolver). issueTemplate is a client
			// hint with no server-side state, accepted for coercion.
			"projectIds":    &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.ID))},
			"issueTemplate": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})

	createIssuePayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "CreateIssuePayload",
		Fields: graphql.Fields{
			"issue": &graphql.Field{Type: issueType},
		},
	})

	s.registerMutation(mutationType, "createIssue", &graphql.Field{
		Type: createIssuePayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(createIssueInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := ghUserFromContext(p.Context)

			input, _ := p.Args["input"].(map[string]interface{})
			repoNodeID, _ := input["repositoryId"].(string)
			title, _ := input["title"].(string)
			body, _ := input["body"].(string)

			repo := findRepoByNodeID(s.store, repoNodeID)
			if repo == nil {
				return nil, fmt.Errorf("could not resolve to a Repository with the global id of '%s'", repoNodeID)
			}

			labelIDPtr, err := resolveGQLLabelIDs(s.store, repo.ID, input["labelIds"])
			if err != nil {
				return nil, err
			}
			assigneeIDPtr, err := resolveGQLAssigneeIDs(s.store, input["assigneeIds"])
			if err != nil {
				return nil, err
			}
			milestoneIDPtr, err := resolveGQLMilestoneID(s.store, repo.ID, input, "milestoneId")
			if err != nil {
				return nil, err
			}
			var labelIDs, assigneeIDs []int
			if labelIDPtr != nil {
				labelIDs = *labelIDPtr
			}
			if assigneeIDPtr != nil {
				assigneeIDs = *assigneeIDPtr
			}
			milestoneID := 0
			if milestoneIDPtr != nil {
				milestoneID = *milestoneIDPtr
			}
			var issueTypeID int
			if itNodeID, ok := input["issueTypeId"].(string); ok && itNodeID != "" {
				it := findIssueTypeByNodeID(s.store, itNodeID)
				if it == nil || s.store.GetAssignableIssueTypeForRepo(repo, it.ID) == nil {
					return nil, fmt.Errorf("could not resolve to an IssueType with the global id of '%s'", itNodeID)
				}
				issueTypeID = it.ID
			}

			issue := s.store.CreateIssue(repo.ID, user.ID, title, body, labelIDs, assigneeIDs, milestoneID)
			if issue == nil {
				return nil, fmt.Errorf("issue creation failed")
			}
			if issueTypeID > 0 {
				s.store.UpdateIssue(issue.ID, func(i *Issue) {
					i.IssueTypeID = issueTypeID
				})
				issue = s.store.GetIssue(issue.ID)
			}

			// Honor projectIds: add the new issue to each named ProjectV2 board.
			// gh sends null unless --project, but a supplied list must not be
			// silently dropped.
			if raw, ok := input["projectIds"].([]interface{}); ok {
				for _, v := range raw {
					nodeID, _ := v.(string)
					if nodeID == "" {
						continue
					}
					proj := s.store.ProjectsV2.LookupProjectByNodeID(nodeID)
					if proj == nil {
						return nil, fmt.Errorf("could not resolve to a ProjectV2 with the global id of '%s'", nodeID)
					}
					s.store.ProjectsV2.AddItem(proj.ID, "Issue", issue.ID, user.ID)
				}
			}

			// Parity with the REST create path: deliver the issues/opened
			// webhook so `on: issues` workflows fire for GraphQL-created issues
			// (the gh CLI uses GraphQL).
			s.emitWebhookEvent(repo.FullName, "issues", "opened", buildIssuesPayload(s.store, repo, issue, user, "opened"))

			return map[string]interface{}{
				"issue": issueToGQL(issue, s.store),
			}, nil
		},
	})

	closeIssueInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CloseIssueInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"issueId":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"stateReason": &graphql.InputObjectFieldConfig{Type: issueClosedStateReasonEnum},
		},
	})

	closeIssuePayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "CloseIssuePayload",
		Fields: graphql.Fields{
			"issue": &graphql.Field{Type: issueType},
		},
	})

	s.registerMutation(mutationType, "closeIssue", &graphql.Field{
		Type: closeIssuePayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(closeIssueInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			issueNodeID, _ := input["issueId"].(string)
			stateReason, _ := input["stateReason"].(string)
			if stateReason == "" {
				stateReason = "COMPLETED"
			}

			issue := findIssueByNodeID(s.store, issueNodeID)
			if issue == nil {
				return nil, fmt.Errorf("could not resolve to an Issue")
			}
			previousState := issue.State

			s.store.UpdateIssue(issue.ID, func(i *Issue) {
				i.State = "CLOSED"
				i.StateReason = stateReason
				now := time.Now()
				i.ClosedAt = &now
			})

			updated := s.store.GetIssue(issue.ID)
			s.emitIssueStateChange(updated, user, previousState, "closed")
			return map[string]interface{}{
				"issue": issueToGQL(updated, s.store),
			}, nil
		},
	})

	reopenIssueInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "ReopenIssueInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"issueId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})

	reopenIssuePayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ReopenIssuePayload",
		Fields: graphql.Fields{
			"issue": &graphql.Field{Type: issueType},
		},
	})

	s.registerMutation(mutationType, "reopenIssue", &graphql.Field{
		Type: reopenIssuePayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(reopenIssueInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			issueNodeID, _ := input["issueId"].(string)

			issue := findIssueByNodeID(s.store, issueNodeID)
			if issue == nil {
				return nil, fmt.Errorf("could not resolve to an Issue")
			}
			previousState := issue.State

			s.store.UpdateIssue(issue.ID, func(i *Issue) {
				i.State = "OPEN"
				i.StateReason = ""
				i.ClosedAt = nil
			})

			updated := s.store.GetIssue(issue.ID)
			s.emitIssueStateChange(updated, user, previousState, "reopened")
			return map[string]interface{}{
				"issue": issueToGQL(updated, s.store),
			}, nil
		},
	})

	addCommentInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "AddCommentInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"subjectId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"body":      &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		},
	})

	commentEdgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueCommentEdge",
		Fields: graphql.Fields{
			"node": &graphql.Field{Type: issueCommentType},
		},
	})

	addCommentPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "AddCommentPayload",
		Fields: graphql.Fields{
			"commentEdge": &graphql.Field{Type: commentEdgeType},
			"subject":     &graphql.Field{Type: nodeInterface},
		},
	})

	s.registerMutation(mutationType, "addComment", &graphql.Field{
		Type: addCommentPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(addCommentInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := ghUserFromContext(p.Context)

			input, _ := p.Args["input"].(map[string]interface{})
			subjectNodeID, _ := input["subjectId"].(string)
			body, _ := input["body"].(string)

			// On real GitHub a PR is an issue, so addComment's subjectId may be
			// either; `gh pr comment` passes a PR node id. Resolve both.
			if issue := findIssueByNodeID(s.store, subjectNodeID); issue != nil {
				comment := s.store.CreateComment(issue.ID, user.ID, body)
				if comment == nil {
					return nil, fmt.Errorf("comment creation failed")
				}
				return map[string]interface{}{
					"commentEdge": map[string]interface{}{"node": commentToGQL(comment, s.store)},
					"subject":     issueToGQL(issue, s.store),
				}, nil
			}
			if pr := findPullRequestByNodeID(s.store, subjectNodeID); pr != nil {
				comment := s.store.CreateCommentFor("pull_request", pr.ID, user.ID, body)
				if comment == nil {
					return nil, fmt.Errorf("comment creation failed")
				}
				return map[string]interface{}{
					"commentEdge": map[string]interface{}{"node": commentToGQL(comment, s.store)},
					"subject":     pullRequestToGQL(pr, s.store),
				}, nil
			}
			return nil, fmt.Errorf("could not resolve to a node with the global id of '%s'", subjectNodeID)
		},
	})

	updateIssueInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UpdateIssueInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"id":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"title": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"body":  &graphql.InputObjectFieldConfig{Type: graphql.String},
			// IssueState, not String: real GitHub types this field with the
			// enum, and a free-form string was being written into the store
			// verbatim, so `state: "banana"` became the issue's state.
			"state":       &graphql.InputObjectFieldConfig{Type: issueStateEnum},
			"milestoneId": &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"labelIds":    &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.ID))},
			"assigneeIds": &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.ID))},
			"issueTypeId": &graphql.InputObjectFieldConfig{Type: graphql.ID},
		},
	})

	updateIssuePayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "UpdateIssuePayload",
		Fields: graphql.Fields{
			"issue": &graphql.Field{Type: issueType},
		},
	})

	s.registerMutation(mutationType, "updateIssue", &graphql.Field{
		Type: updateIssuePayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(updateIssueInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			issueNodeID, _ := input["id"].(string)

			issue := findIssueByNodeID(s.store, issueNodeID)
			if issue == nil {
				return nil, fmt.Errorf("could not resolve to an Issue")
			}
			repo := s.store.GetRepoByID(issue.RepoID)
			if repo == nil {
				return nil, fmt.Errorf("could not resolve to a Repository")
			}
			var issueTypeID *int
			if raw, present := input["issueTypeId"]; present {
				if itNodeID, ok := raw.(string); ok && itNodeID != "" {
					it := findIssueTypeByNodeID(s.store, itNodeID)
					if it == nil || s.store.GetAssignableIssueTypeForRepo(repo, it.ID) == nil {
						return nil, fmt.Errorf("could not resolve to an IssueType with the global id of '%s'", itNodeID)
					}
					resolved := it.ID
					issueTypeID = &resolved
				} else {
					cleared := 0
					issueTypeID = &cleared
				}
			}
			// Triage — labels, assignees, milestone — is a push-level act even
			// for the issue's own author, who reaches this mutation to edit
			// their title and body.
			triage := false
			for _, key := range []string{"labelIds", "assigneeIds", "milestoneId"} {
				if _, present := input[key]; present {
					triage = true
				}
			}
			if triage && !s.viewerHasRepoPermission(p.Context, repo, scopeIssues, permWrite) {
				return nil, fmt.Errorf("must have push access to Repository")
			}
			// The schema types this as IssueState; the second check catches a
			// caller that reached the resolver another way rather than letting
			// an unknown word become the issue's state.
			newState := ""
			if raw, present := input["state"]; present && raw != nil {
				v, ok := raw.(string)
				if !ok || (v != "OPEN" && v != "CLOSED") {
					return nil, fmt.Errorf("state must be OPEN or CLOSED, got %v", raw)
				}
				newState = v
			}
			labelIDs, err := resolveGQLLabelIDs(s.store, repo.ID, input["labelIds"])
			if err != nil {
				return nil, err
			}
			assigneeIDs, err := resolveGQLAssigneeIDs(s.store, input["assigneeIds"])
			if err != nil {
				return nil, err
			}
			milestoneID, err := resolveGQLMilestoneID(s.store, repo.ID, input, "milestoneId")
			if err != nil {
				return nil, err
			}

			s.store.UpdateIssue(issue.ID, func(i *Issue) {
				if v, ok := input["title"].(string); ok {
					i.Title = v
				}
				if v, ok := input["body"].(string); ok {
					i.Body = v
				}
				if newState != "" {
					applyIssueState(i, newState)
				}
				if labelIDs != nil {
					i.LabelIDs = *labelIDs
				}
				if assigneeIDs != nil {
					i.AssigneeIDs = *assigneeIDs
				}
				if milestoneID != nil {
					i.MilestoneID = *milestoneID
				}
				if issueTypeID != nil {
					i.IssueTypeID = *issueTypeID
				}
			})

			updated := s.store.GetIssue(issue.ID)
			return map[string]interface{}{
				"issue": issueToGQL(updated, s.store),
			}, nil
		},
	})

	return issueType, issueMilestoneType
}

// --- GraphQL converter helpers ---

func (s *Server) issueFieldValueGraphQLConnectionType() *graphql.Object {
	dataTypeEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "IssueFieldDataType",
		Values: graphql.EnumValueConfigMap{
			"TEXT":          &graphql.EnumValueConfig{Value: "TEXT"},
			"SINGLE_SELECT": &graphql.EnumValueConfig{Value: "SINGLE_SELECT"},
			"DATE":          &graphql.EnumValueConfig{Value: "DATE"},
			"NUMBER":        &graphql.EnumValueConfig{Value: "NUMBER"},
			"MULTI_SELECT":  &graphql.EnumValueConfig{Value: "MULTI_SELECT"},
		},
	})
	visibilityEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "IssueFieldVisibility",
		Values: graphql.EnumValueConfigMap{
			"ORG_ONLY": &graphql.EnumValueConfig{Value: "ORG_ONLY"},
			"ALL":      &graphql.EnumValueConfig{Value: "ALL"},
		},
	})
	colorEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "IssueFieldSingleSelectOptionColor",
		Values: graphql.EnumValueConfigMap{
			"GRAY":   &graphql.EnumValueConfig{Value: "GRAY"},
			"BLUE":   &graphql.EnumValueConfig{Value: "BLUE"},
			"GREEN":  &graphql.EnumValueConfig{Value: "GREEN"},
			"YELLOW": &graphql.EnumValueConfig{Value: "YELLOW"},
			"ORANGE": &graphql.EnumValueConfig{Value: "ORANGE"},
			"RED":    &graphql.EnumValueConfig{Value: "RED"},
			"PINK":   &graphql.EnumValueConfig{Value: "PINK"},
			"PURPLE": &graphql.EnumValueConfig{Value: "PURPLE"},
		},
	})

	optionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueFieldSingleSelectOption",
		Fields: graphql.Fields{
			"id":             &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"databaseId":     &graphql.Field{Type: graphql.Int},
			"fullDatabaseId": &graphql.Field{Type: s.graphQLStringScalar("BigInt")},
			"name":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"description":    &graphql.Field{Type: graphql.String},
			"color":          &graphql.Field{Type: graphql.NewNonNull(colorEnum)},
			"priority":       &graphql.Field{Type: graphql.Int},
		},
	})

	commonFieldFields := func(withOptions bool) graphql.Fields {
		fields := graphql.Fields{
			"id":             &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"fullDatabaseId": &graphql.Field{Type: s.graphQLStringScalar("BigInt")},
			"name":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"description":    &graphql.Field{Type: graphql.String},
			"dataType":       &graphql.Field{Type: graphql.NewNonNull(dataTypeEnum)},
			"visibility":     &graphql.Field{Type: graphql.NewNonNull(visibilityEnum)},
			"createdAt":      &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime"))},
		}
		if withOptions {
			fields["options"] = &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(optionType)))}
		}
		return fields
	}
	textFieldType := graphql.NewObject(graphql.ObjectConfig{Name: "IssueFieldText", Fields: commonFieldFields(false)})
	dateFieldType := graphql.NewObject(graphql.ObjectConfig{Name: "IssueFieldDate", Fields: commonFieldFields(false)})
	numberFieldType := graphql.NewObject(graphql.ObjectConfig{Name: "IssueFieldNumber", Fields: commonFieldFields(false)})
	singleSelectFieldType := graphql.NewObject(graphql.ObjectConfig{Name: "IssueFieldSingleSelect", Fields: commonFieldFields(true)})
	multiSelectFieldType := graphql.NewObject(graphql.ObjectConfig{Name: "IssueFieldMultiSelect", Fields: commonFieldFields(true)})

	fieldUnion := graphql.NewUnion(graphql.UnionConfig{
		Name:  "IssueFields",
		Types: []*graphql.Object{textFieldType, dateFieldType, numberFieldType, singleSelectFieldType, multiSelectFieldType},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			src, _ := p.Value.(map[string]interface{})
			switch src["__typename"] {
			case "IssueFieldDate":
				return dateFieldType
			case "IssueFieldNumber":
				return numberFieldType
			case "IssueFieldSingleSelect":
				return singleSelectFieldType
			case "IssueFieldMultiSelect":
				return multiSelectFieldType
			default:
				return textFieldType
			}
		},
	})

	commonValueFields := func(valueType graphql.Output) graphql.Fields {
		return graphql.Fields{
			"id":    &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"field": &graphql.Field{Type: fieldUnion},
			"value": &graphql.Field{Type: valueType},
		}
	}
	textValueType := graphql.NewObject(graphql.ObjectConfig{Name: "IssueFieldTextValue", Fields: commonValueFields(graphql.NewNonNull(graphql.String))})
	dateValueType := graphql.NewObject(graphql.ObjectConfig{Name: "IssueFieldDateValue", Fields: commonValueFields(graphql.NewNonNull(graphql.String))})
	numberValueType := graphql.NewObject(graphql.ObjectConfig{Name: "IssueFieldNumberValue", Fields: commonValueFields(graphql.NewNonNull(graphql.Float))})
	singleSelectValueFields := commonValueFields(graphql.NewNonNull(graphql.String))
	singleSelectValueFields["name"] = &graphql.Field{Type: graphql.NewNonNull(graphql.String)}
	singleSelectValueFields["description"] = &graphql.Field{Type: graphql.String}
	singleSelectValueFields["color"] = &graphql.Field{Type: graphql.NewNonNull(colorEnum)}
	singleSelectValueFields["optionId"] = &graphql.Field{Type: graphql.String}
	singleSelectValueType := graphql.NewObject(graphql.ObjectConfig{Name: "IssueFieldSingleSelectValue", Fields: singleSelectValueFields})
	multiSelectValueFields := commonValueFields(graphql.String)
	multiSelectValueFields["options"] = &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(optionType)))}
	multiSelectValueType := graphql.NewObject(graphql.ObjectConfig{Name: "IssueFieldMultiSelectValue", Fields: multiSelectValueFields})

	valueUnion := graphql.NewUnion(graphql.UnionConfig{
		Name:  "IssueFieldValue",
		Types: []*graphql.Object{dateValueType, multiSelectValueType, numberValueType, singleSelectValueType, textValueType},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			src, _ := p.Value.(map[string]interface{})
			switch src["__typename"] {
			case "IssueFieldDateValue":
				return dateValueType
			case "IssueFieldMultiSelectValue":
				return multiSelectValueType
			case "IssueFieldNumberValue":
				return numberValueType
			case "IssueFieldSingleSelectValue":
				return singleSelectValueType
			default:
				return textValueType
			}
		},
	})
	edgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueFieldValueEdge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: valueUnion},
		},
	})
	return graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueFieldValueConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(valueUnion)},
			"edges":      &graphql.Field{Type: graphql.NewList(edgeType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})
}

func issueToGQL(issue *Issue, st *Store) map[string]interface{} {
	st.mu.RLock()
	defer st.mu.RUnlock()

	// Author
	var author map[string]interface{}
	if u, ok := st.Users[issue.AuthorID]; ok {
		author = userToGraphQL(u)
	}

	// Labels
	labelNodes := make([]map[string]interface{}, 0)
	for _, lid := range issue.LabelIDs {
		if l, ok := st.Labels[lid]; ok {
			labelNodes = append(labelNodes, labelToGQL(l))
		}
	}

	// Assignees
	assigneeNodes := make([]map[string]interface{}, 0)
	for _, aid := range issue.AssigneeIDs {
		if u, ok := st.Users[aid]; ok {
			assigneeNodes = append(assigneeNodes, userToGraphQL(u))
		}
	}

	// Milestone
	var milestone map[string]interface{}
	if issue.MilestoneID > 0 {
		if ms, ok := st.Milestones[issue.MilestoneID]; ok {
			milestone = milestoneToGQL(ms)
		}
	}
	var issueType map[string]interface{}
	if it := st.issueTypeForIssueLocked(issue); it != nil {
		color := "GRAY"
		if it.Color != nil && *it.Color != "" {
			color = strings.ToUpper(*it.Color)
		}
		issueType = map[string]interface{}{
			"id":          it.NodeID,
			"name":        it.Name,
			"description": nilStrPtr(it.Description),
			"color":       color,
		}
	}

	// Comments — resolved through the by-parent index so rendering a page of
	// issues no longer scans every comment in the store once per issue.
	indexed := st.CommentsByParent[commentCountKey("issue", issue.ID)]
	commentNodes := make([]map[string]interface{}, 0, len(indexed))
	for _, c := range indexed {
		commentNodes = append(commentNodes, commentToGQLLocked(c, st))
	}
	// The index order is nondeterministic across a reload; sort for stable
	// cursor pagination (oldest first, like GitHub's comments feed).
	sortGQLNodesByCreatedAt(commentNodes)

	// Resolve repo for URL
	repo := st.Repos[issue.RepoID]
	url := ""
	if repo != nil {
		url = externalURL("/" + repo.FullName + "/issues/" + strconv.Itoa(issue.Number))
	}

	var parent map[string]interface{}
	if parentID, ok := st.SubIssueParent[issue.ID]; ok {
		if parentIssue := st.Issues[parentID]; parentIssue != nil {
			parent = relatedIssueToGQLLocked(parentIssue, st)
		}
	}
	subIssueNodes := make([]map[string]interface{}, 0, len(st.SubIssueLists[issue.ID]))
	completedSubIssues := 0
	for _, childID := range st.SubIssueLists[issue.ID] {
		child := st.Issues[childID]
		if child == nil {
			continue
		}
		if child.State == "CLOSED" {
			completedSubIssues++
		}
		subIssueNodes = append(subIssueNodes, relatedIssueToGQLLocked(child, st))
	}
	percentCompleted := 0
	if len(subIssueNodes) > 0 {
		percentCompleted = completedSubIssues * 100 / len(subIssueNodes)
	}

	var closedAt interface{}
	if issue.ClosedAt != nil {
		closedAt = issue.ClosedAt.Format(time.RFC3339)
	}

	var stateReason interface{}
	if issue.StateReason != "" {
		stateReason = issue.StateReason
	}

	return map[string]interface{}{
		"nodeID":           issue.NodeID,
		"databaseId":       issue.ID,
		"number":           issue.Number,
		"title":            issue.Title,
		"body":             issue.Body,
		"state":            issue.State,
		"stateReason":      stateReason,
		"url":              url,
		"createdAt":        issue.CreatedAt.Format(time.RFC3339),
		"updatedAt":        issue.UpdatedAt.Format(time.RFC3339),
		"closedAt":         closedAt,
		"isPinned":         false,
		"locked":           issue.Locked,
		"activeLockReason": graphQLLockReason(issue.ActiveLockReason),
		"author":           author,
		"labels": map[string]interface{}{
			"nodes":      labelNodes,
			"totalCount": len(labelNodes),
			"pageInfo": map[string]interface{}{
				"hasNextPage":     false,
				"hasPreviousPage": false,
				"startCursor":     nil,
				"endCursor":       nil,
			},
		},
		"assignees": map[string]interface{}{
			"nodes":      assigneeNodes,
			"totalCount": len(assigneeNodes),
			"pageInfo": map[string]interface{}{
				"hasNextPage":     false,
				"hasPreviousPage": false,
				"startCursor":     nil,
				"endCursor":       nil,
			},
		},
		"milestone": milestone,
		"issueType": issueType,
		"parent":    parent,
		"subIssues": map[string]interface{}{
			"nodes":      subIssueNodes,
			"totalCount": len(subIssueNodes),
			"pageInfo": map[string]interface{}{
				"hasNextPage":     false,
				"hasPreviousPage": false,
				"startCursor":     nil,
				"endCursor":       nil,
			},
		},
		"subIssuesSummary": map[string]interface{}{
			"total":            len(subIssueNodes),
			"completed":        completedSubIssues,
			"percentCompleted": percentCompleted,
		},
		"issueFieldValues": issueFieldValuesConnectionLocked(st, issue),
		"comments": map[string]interface{}{
			"nodes":      commentNodes,
			"totalCount": len(commentNodes),
			"pageInfo": map[string]interface{}{
				"hasNextPage":     false,
				"hasPreviousPage": false,
				"startCursor":     nil,
				"endCursor":       nil,
			},
		},
		"reactionGroups": reactionGroupsForGraphQL(st.Reactions, "issue", issue.ID),
	}
}

func graphQLLockReason(reason string) interface{} {
	if reason == "" {
		return nil
	}
	return strings.ToUpper(strings.ReplaceAll(reason, "-", "_"))
}

func (s *Server) gqlPageInfoType() *graphql.Object {
	if s.graphqlTypes.pageInfo != nil {
		return s.graphqlTypes.pageInfo
	}
	s.graphqlTypes.pageInfo = graphql.NewObject(graphql.ObjectConfig{
		Name: "PageInfo",
		Fields: graphql.Fields{
			"hasNextPage":     &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"hasPreviousPage": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"startCursor":     &graphql.Field{Type: graphql.String},
			"endCursor":       &graphql.Field{Type: graphql.String},
		},
	})
	return s.graphqlTypes.pageInfo
}

func (s *Server) graphQLEnum(name string, values ...string) *graphql.Enum {
	if s.graphqlTypes.enums == nil {
		s.graphqlTypes.enums = make(map[string]*graphql.Enum)
	}
	if enum := s.graphqlTypes.enums[name]; enum != nil {
		return enum
	}
	config := make(graphql.EnumValueConfigMap, len(values))
	for _, value := range values {
		config[value] = &graphql.EnumValueConfig{Value: value}
	}
	enum := graphql.NewEnum(graphql.EnumConfig{Name: name, Values: config})
	s.graphqlTypes.enums[name] = enum
	return enum
}

func (s *Server) gqlIssueOrderType(fieldType, directionType graphql.Input) *graphql.InputObject {
	if s.graphqlTypes.issueOrder != nil {
		return s.graphqlTypes.issueOrder
	}
	s.graphqlTypes.issueOrder = graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "IssueOrder",
		Fields: graphql.InputObjectConfigFieldMap{
			"field":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(fieldType)},
			"direction": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(directionType)},
		},
	})
	return s.graphqlTypes.issueOrder
}

func relayConnectionArgs() graphql.FieldConfigArgument {
	return graphql.FieldConfigArgument{
		"first":  &graphql.ArgumentConfig{Type: graphql.Int},
		"after":  &graphql.ArgumentConfig{Type: graphql.String},
		"last":   &graphql.ArgumentConfig{Type: graphql.Int},
		"before": &graphql.ArgumentConfig{Type: graphql.String},
	}
}

func issueFieldValuesConnectionLocked(st *Store, issue *Issue) map[string]interface{} {
	repo := st.Repos[issue.RepoID]
	org := ""
	if repo != nil {
		org = issueFieldsOrgLocked(st, repo)
	}
	values := st.IssueFieldValues[issue.ID]
	fieldIDs := make([]int, 0, len(values))
	for id := range values {
		fieldIDs = append(fieldIDs, id)
	}
	sort.Ints(fieldIDs)
	nodes := make([]map[string]interface{}, 0, len(fieldIDs))
	for _, fieldID := range fieldIDs {
		field := st.OrgIssueFields[org][fieldID]
		if field == nil {
			continue
		}
		nodes = append(nodes, issueFieldValueToGQLLocked(field, issue.ID, values[fieldID]))
	}
	// Return the full node set with a truthful totalCount; the field resolver
	// applies the client's page window via repaginateConnection. Pre-paginating
	// here with paginateGQL clamped the list to 100, so an issue with more than
	// 100 field values reported totalCount 100 and hid the remainder from
	// pagination (GQL-022). Match the shape of the sibling connections.
	return map[string]interface{}{
		"nodes":      nodes,
		"totalCount": len(nodes),
		"pageInfo": map[string]interface{}{
			"hasNextPage":     false,
			"hasPreviousPage": false,
			"startCursor":     nil,
			"endCursor":       nil,
		},
	}
}

func issueFieldsOrgLocked(st *Store, repo *Repo) string {
	orgLogin, _, _ := strings.Cut(repo.FullName, "/")
	if st.OrgsByLogin[orgLogin] == nil {
		return ""
	}
	return orgLogin
}

func issueFieldValueToGQLLocked(field *IssueField, issueID int, value interface{}) map[string]interface{} {
	out := map[string]interface{}{
		"id":    fmt.Sprintf("IFV_kwDO%08d%08d", issueID, field.ID),
		"field": issueFieldToGQLLocked(field),
		"value": value,
	}
	switch field.DataType {
	case "date":
		out["__typename"] = "IssueFieldDateValue"
	case "number":
		out["__typename"] = "IssueFieldNumberValue"
	case "single_select":
		out["__typename"] = "IssueFieldSingleSelectValue"
		if name, ok := value.(string); ok {
			out["name"] = name
			if opt := issueFieldOptionByName(field, name); opt != nil {
				out["optionId"] = issueFieldOptionNodeID(opt.ID)
				out["description"] = nilStrPtr(opt.Description)
				out["color"] = issueFieldColorEnum(opt.Color)
			} else {
				out["description"] = nil
				out["color"] = "GRAY"
			}
		}
	case "multi_select":
		out["__typename"] = "IssueFieldMultiSelectValue"
		names := toStringSlice(value)
		opts := make([]map[string]interface{}, 0, len(names))
		for _, name := range names {
			if opt := issueFieldOptionByName(field, name); opt != nil {
				opts = append(opts, issueFieldOptionToGQL(opt))
			}
		}
		out["options"] = opts
		out["value"] = nil
	default:
		out["__typename"] = "IssueFieldTextValue"
	}
	return out
}

func issueFieldToGQLLocked(field *IssueField) map[string]interface{} {
	out := map[string]interface{}{
		"id":             field.NodeID,
		"fullDatabaseId": strconv.Itoa(field.ID),
		"name":           field.Name,
		"description":    nilStrPtr(field.Description),
		"dataType":       strings.ToUpper(field.DataType),
		"visibility":     issueFieldVisibilityEnum(field.Visibility),
		"createdAt":      field.CreatedAt.Format(time.RFC3339),
	}
	switch field.DataType {
	case "date":
		out["__typename"] = "IssueFieldDate"
	case "number":
		out["__typename"] = "IssueFieldNumber"
	case "single_select":
		out["__typename"] = "IssueFieldSingleSelect"
		out["options"] = issueFieldOptionsToGQL(field.Options)
	case "multi_select":
		out["__typename"] = "IssueFieldMultiSelect"
		out["options"] = issueFieldOptionsToGQL(field.Options)
	default:
		out["__typename"] = "IssueFieldText"
	}
	return out
}

func issueFieldOptionsToGQL(options []*IssueFieldOption) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(options))
	for _, opt := range options {
		out = append(out, issueFieldOptionToGQL(opt))
	}
	return out
}

func issueFieldOptionToGQL(opt *IssueFieldOption) map[string]interface{} {
	return map[string]interface{}{
		"id":             issueFieldOptionNodeID(opt.ID),
		"databaseId":     opt.ID,
		"fullDatabaseId": strconv.Itoa(opt.ID),
		"name":           opt.Name,
		"description":    nilStrPtr(opt.Description),
		"color":          issueFieldColorEnum(opt.Color),
		"priority":       opt.Priority,
	}
}

func issueFieldOptionByName(field *IssueField, name string) *IssueFieldOption {
	for _, opt := range field.Options {
		if opt.Name == name {
			return opt
		}
	}
	return nil
}

func issueFieldOptionNodeID(id int) string {
	return fmt.Sprintf("IFO_kwDO%08d", id)
}

func issueFieldColorEnum(color string) string {
	color = strings.ToUpper(color)
	if color == "" {
		return "GRAY"
	}
	return color
}

func issueFieldVisibilityEnum(visibility string) string {
	if visibility == "all" {
		return "ALL"
	}
	return "ORG_ONLY"
}

func labelToGQL(l *IssueLabel) map[string]interface{} {
	return map[string]interface{}{
		"nodeID":      l.NodeID,
		"name":        l.Name,
		"description": l.Description,
		"color":       l.Color,
	}
}

func relatedIssueToGQLLocked(issue *Issue, st *Store) map[string]interface{} {
	repo := st.Repos[issue.RepoID]
	nameWithOwner := ""
	url := ""
	if repo != nil {
		nameWithOwner = repo.FullName
		url = externalURL("/" + repo.FullName + "/issues/" + strconv.Itoa(issue.Number))
	}
	return map[string]interface{}{
		"id":     issue.NodeID,
		"nodeID": issue.NodeID,
		"number": issue.Number,
		"title":  issue.Title,
		"url":    url,
		"state":  issue.State,
		"repository": map[string]interface{}{
			"nameWithOwner": nameWithOwner,
		},
	}
}

func milestoneToGQL(ms *Milestone) map[string]interface{} {
	var dueOn interface{}
	if ms.DueOn != nil {
		dueOn = ms.DueOn.Format(time.RFC3339)
	}
	return map[string]interface{}{
		"nodeID":      ms.NodeID,
		"number":      ms.Number,
		"title":       ms.Title,
		"description": ms.Description,
		"state":       strings.ToUpper(ms.State),
		"dueOn":       dueOn,
	}
}

func commentToGQL(c *Comment, st *Store) map[string]interface{} {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return commentToGQLLocked(c, st)
}

func commentToGQLLocked(c *Comment, st *Store) map[string]interface{} {
	var author map[string]interface{}
	if u, ok := st.Users[c.AuthorID]; ok {
		author = userToGraphQL(u)
	}
	var editor map[string]interface{}
	var lastEditedAt interface{}
	if c.LastEditedAt != nil {
		lastEditedAt = c.LastEditedAt.Format(time.RFC3339)
		if u, ok := st.Users[c.EditorID]; ok {
			editor = userToGraphQL(u)
		}
	}
	return map[string]interface{}{
		"_dbID":               c.ID,
		"nodeID":              c.NodeID,
		"body":                c.Body,
		"url":                 commentURLLocked(c, st),
		"authorID":            c.AuthorID,
		"createdAt":           c.CreatedAt.Format(time.RFC3339),
		"updatedAt":           c.UpdatedAt.Format(time.RFC3339),
		"author":              author,
		"authorAssociation":   commentAuthorAssociationLocked(c, st),
		"includesCreatedEdit": c.LastEditedAt != nil,
		"lastEditedAt":        lastEditedAt,
		"editor":              editor,
		"isMinimized":         c.MinimizedReason != "",
		"isPinned":            c.Pinned,
		"minimizedReason":     nilStr(c.MinimizedReason),
		"reactionGroups":      reactionGroupsForGraphQL(st.Reactions, "issue_comment", c.ID),
	}
}

func commentAuthorAssociationLocked(comment *Comment, st *Store) string {
	repoID := 0
	switch comment.ParentType {
	case "pull_request":
		if pull := st.PullRequests[comment.IssueID]; pull != nil {
			repoID = pull.RepoID
		}
	default:
		if issue := st.Issues[comment.IssueID]; issue != nil {
			repoID = issue.RepoID
		}
	}
	return authorAssociationForRepoLocked(st, repoID, comment.AuthorID)
}

func commentURLLocked(comment *Comment, st *Store) string {
	repoID, number, lane := 0, 0, "issues"
	switch comment.ParentType {
	case "pull_request":
		if pull := st.PullRequests[comment.IssueID]; pull != nil {
			repoID, number, lane = pull.RepoID, pull.Number, "pull"
		}
	default:
		if issue := st.Issues[comment.IssueID]; issue != nil {
			repoID, number = issue.RepoID, issue.Number
		}
	}
	if repo := st.Repos[repoID]; repo != nil && number > 0 {
		return externalURL(fmt.Sprintf("/%s/%s/%d#issuecomment-%d", repo.FullName, lane, number, comment.ID))
	}
	return externalURL(fmt.Sprintf("/comments/%d", comment.ID))
}

func authorAssociationForRepoLocked(st *Store, repoID, authorID int) string {
	repo := st.Repos[repoID]
	author := st.Users[authorID]
	if repo == nil || author == nil {
		return "NONE"
	}
	if repo.OwnerType == "User" && repo.OwnerID == authorID {
		return "OWNER"
	}
	if repo.OwnerType == "Organization" {
		owner, _, _ := strings.Cut(repo.FullName, "/")
		if membership := st.Memberships[membershipKey(owner, authorID)]; membership != nil && membership.State == "active" {
			return "MEMBER"
		}
	}
	if _, ok := st.RepoCollaborators[repo.FullName][author.Login]; ok {
		return "COLLABORATOR"
	}
	return "NONE"
}

// nilStr returns nil for empty strings (so nullable GraphQL String fields
// resolve to null rather than ""), or the string itself.
func nilStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nilStrPtr(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}

// reactionGroupsForGraphQL returns a GraphQL-shaped `[ReactionGroup]` list
// for the given parent, querying the real ReactionStore so per-content
// totalCount values reflect actual reactions. Used by Issue, IssueComment,
// and any other reactable type's `reactionGroups` field.
func reactionGroupsForGraphQL(rs *ReactionStore, parentType string, parentID int) []map[string]interface{} {
	counts := map[string]int{
		"+1": 0, "-1": 0, "laugh": 0, "confused": 0,
		"heart": 0, "hooray": 0, "rocket": 0, "eyes": 0,
	}
	if rs != nil && parentID != 0 {
		for _, r := range rs.ListReactions(parentType, parentID, "") {
			counts[r.Content]++
		}
	}
	// Order matches real GitHub's GraphQL response.
	mapping := [...]struct{ rest, gql string }{
		{"+1", "THUMBS_UP"},
		{"-1", "THUMBS_DOWN"},
		{"laugh", "LAUGH"},
		{"hooray", "HOORAY"},
		{"confused", "CONFUSED"},
		{"heart", "HEART"},
		{"rocket", "ROCKET"},
		{"eyes", "EYES"},
	}
	out := make([]map[string]interface{}, 0, len(mapping))
	for _, m := range mapping {
		out = append(out, map[string]interface{}{
			"content": m.gql,
			"users":   map[string]interface{}{"totalCount": counts[m.rest]},
		})
	}
	return out
}

// --- Node ID lookup helpers ---

// decodeNodeDBID extracts the trailing database id from a GraphQL node ID of the
// form "<prefix><digits>" (e.g. "R_kgDO00000123", prefix "R_kgDO"). It returns
// false when the id lacks that prefix or does not end in digits — so a legacy or
// foreign-shaped id (e.g. the "U_bleephub_<login>" identifiers) falls through to
// a scan rather than being mis-resolved. Callers pair the O(1) map lookup with a
// node-id equality check, keeping behavior identical to the old full scan
// (GQL-024).
func decodeNodeDBID(nodeID, prefix string) (int, bool) {
	rest, ok := strings.CutPrefix(nodeID, prefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return id, true
}

func findRepoByNodeID(st *Store, nodeID string) *Repo {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if id, ok := decodeNodeDBID(nodeID, "R_kgDO"); ok {
		if r := st.Repos[id]; r != nil && r.NodeID == nodeID {
			return r
		}
	}
	for _, r := range st.Repos {
		if r.NodeID == nodeID {
			return r
		}
	}
	return nil
}

func findIssueByNodeID(st *Store, nodeID string) *Issue {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if id, ok := decodeNodeDBID(nodeID, "I_kgDO"); ok {
		if i := st.Issues[id]; i != nil && i.NodeID == nodeID {
			return i
		}
	}
	for _, i := range st.Issues {
		if i.NodeID == nodeID {
			return i
		}
	}
	return nil
}

func findLabelByNodeID(st *Store, nodeID string) *IssueLabel {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if id, ok := decodeNodeDBID(nodeID, "LA_kgDO"); ok {
		if l := st.Labels[id]; l != nil && l.NodeID == nodeID {
			return l
		}
	}
	for _, l := range st.Labels {
		if l.NodeID == nodeID {
			return l
		}
	}
	return nil
}

func findMilestoneByNodeID(st *Store, nodeID string) *Milestone {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if id, ok := decodeNodeDBID(nodeID, "MI_kgDO"); ok {
		if ms := st.Milestones[id]; ms != nil && ms.NodeID == nodeID {
			return ms
		}
	}
	for _, ms := range st.Milestones {
		if ms.NodeID == nodeID {
			return ms
		}
	}
	return nil
}

// resolveGQLLabelIDs maps a mutation's labelIds onto store ids. A nil argument
// — absent, or an explicit null — means "leave the labels alone" and yields a
// nil result; an id that names no label of repoID is refused rather than
// dropped, because dropping it reported success for a label never applied.
func resolveGQLLabelIDs(st *Store, repoID int, raw interface{}) (*[]int, error) {
	entries, ok := raw.([]interface{})
	if !ok {
		return nil, nil
	}
	ids := make([]int, 0, len(entries))
	for _, entry := range entries {
		nodeID := fmt.Sprintf("%v", entry)
		l := findLabelByNodeID(st, nodeID)
		if l == nil || l.RepoID != repoID {
			return nil, gqlMissingNode("Label", nodeID)
		}
		ids = append(ids, l.ID)
	}
	return &ids, nil
}

// resolveGQLAssigneeIDs is resolveGQLLabelIDs for assigneeIds.
func resolveGQLAssigneeIDs(st *Store, raw interface{}) (*[]int, error) {
	entries, ok := raw.([]interface{})
	if !ok {
		return nil, nil
	}
	ids := make([]int, 0, len(entries))
	for _, entry := range entries {
		nodeID := fmt.Sprintf("%v", entry)
		u := findUserByNodeID(st, nodeID)
		if u == nil {
			return nil, gqlMissingNode("User", nodeID)
		}
		ids = append(ids, u.ID)
	}
	return &ids, nil
}

// resolveGQLMilestoneID maps a mutation's milestone argument onto a store id.
// An absent member leaves the milestone alone; an explicit null clears it.
func resolveGQLMilestoneID(st *Store, repoID int, input map[string]interface{}, key string) (*int, error) {
	raw, present := input[key]
	if !present {
		return nil, nil
	}
	nodeID, ok := raw.(string)
	if !ok || nodeID == "" {
		cleared := 0
		return &cleared, nil
	}
	ms := findMilestoneByNodeID(st, nodeID)
	if ms == nil || ms.RepoID != repoID {
		return nil, gqlMissingNode("Milestone", nodeID)
	}
	id := ms.ID
	return &id, nil
}

// applyIssueState moves an issue between OPEN and CLOSED, keeping ClosedAt and
// StateReason consistent with the transition the way the REST handler does.
func applyIssueState(i *Issue, state string) {
	if state == "CLOSED" {
		i.State = "CLOSED"
		if i.ClosedAt == nil {
			now := time.Now()
			i.ClosedAt = &now
		}
		if i.StateReason == "" {
			i.StateReason = "COMPLETED"
		}
		return
	}
	i.State = "OPEN"
	i.ClosedAt = nil
	i.StateReason = ""
}

func findUserByNodeID(st *Store, nodeID string) *User {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if id, ok := decodeNodeDBID(nodeID, "U_kgDO"); ok {
		if u := st.Users[id]; u != nil && u.NodeID == nodeID {
			return u
		}
	}
	for _, u := range st.Users {
		if u.NodeID == nodeID {
			return u
		}
	}
	return nil
}

// paginateIssuesGQL implements Relay-style cursor pagination for issues.
func paginateIssuesGQL(issues []*Issue, st *Store, first int, after string) map[string]interface{} {
	return paginateGQL(issues, first, after, func(i *Issue) map[string]interface{} {
		return issueToGQL(i, st)
	}, func(i *Issue) string { return i.NodeID })
}

// Some GraphQL fields queried by gh CLI are not mutable through the REST
// surfaces Bleephub implements today. Those fields resolve to their persisted
// value when modeled, or to the GitHub-shaped zero value when the feature is
// absent.
// projectV2ItemConnectionType returns a singleton wiring for the
// ProjectV2 connection on Issue + PullRequest. Real lookups against
// the ProjectV2Store; resolvers read from the source map populated by
// projectItemsForGraphQL.
type graphQLTypeRegistry struct {
	pageInfo                         *graphql.Object
	scalars                          map[string]*graphql.Scalar
	enums                            map[string]*graphql.Enum
	actor                            *graphql.Interface
	repositoryOwner                  *graphql.Interface
	issueOrder                       *graphql.InputObject
	projectV2Type                    *graphql.Object
	projectV2FieldTypeMemo           *graphql.Object
	projectV2FieldConnectionMemo     *graphql.Object
	projectV2ViewTypeMemo            *graphql.Object
	projectV2ViewConnectionMemo      *graphql.Object
	projectV2ItemTypeMemo            *graphql.Object
	projectV2ItemConnectionTypeMemo  *graphql.Object
	projectV2ItemsFieldAdded         bool
	projectV2SingleSelectValueMemo   *graphql.Object
	projectV2TextValueMemo           *graphql.Object
	projectV2NumberValueMemo         *graphql.Object
	projectV2DateValueMemo           *graphql.Object
	projectV2IterationValueMemo      *graphql.Object
	projectV2ItemFieldValueUnionMemo *graphql.Union
}

func (s *Server) projectV2GraphQLTypes() *graphql.Object {
	if s.graphqlTypes.projectV2Type != nil {
		return s.graphqlTypes.projectV2Type
	}
	s.graphqlTypes.projectV2Type = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2",
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return src["nodeID"], nil
				},
			},
			"number": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"title":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"closed": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"public": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"url":    &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("URI"))},
		},
	})
	s.graphqlTypes.projectV2Type.AddFieldConfig("fields", &graphql.Field{
		Type: graphql.NewNonNull(s.projectV2FieldConnectionType()),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			projectID, err := projectV2SourceID(p.Source)
			if err != nil {
				return nil, err
			}
			fields := s.store.ProjectsV2.FieldsForProject(projectID)
			nodes := make([]map[string]interface{}, 0, len(fields))
			for _, f := range fields {
				nodes = append(nodes, projectV2FieldToGQL(f))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
	s.graphqlTypes.projectV2Type.AddFieldConfig("views", &graphql.Field{
		Type: graphql.NewNonNull(s.projectV2ViewConnectionType()),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			projectID, err := projectV2SourceID(p.Source)
			if err != nil {
				return nil, err
			}
			views := s.store.ProjectsV2.ViewsForProject(projectID)
			nodes := make([]map[string]interface{}, 0, len(views))
			for _, v := range views {
				nodes = append(nodes, projectV2ViewToGQL(v))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
	return s.graphqlTypes.projectV2Type
}

func (s *Server) ensureProjectV2ItemsField() {
	if s.graphqlTypes.projectV2Type == nil || s.graphqlTypes.projectV2ItemConnectionTypeMemo == nil || s.graphqlTypes.projectV2ItemsFieldAdded {
		return
	}
	s.graphqlTypes.projectV2Type.AddFieldConfig("items", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.projectV2ItemConnectionTypeMemo),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			projectID, err := projectV2SourceID(p.Source)
			if err != nil {
				return nil, err
			}
			items := s.store.ProjectsV2.ListItemsForProject(projectID)
			nodes := make([]map[string]interface{}, 0, len(items))
			for _, it := range items {
				nodes = append(nodes, projectV2ItemToGQL(it, s.store))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
	s.graphqlTypes.projectV2ItemsFieldAdded = true
}

func (s *Server) projectV2FieldConnectionType() *graphql.Object {
	if s.graphqlTypes.projectV2FieldConnectionMemo != nil {
		return s.graphqlTypes.projectV2FieldConnectionMemo
	}
	optionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2SingleSelectFieldOption",
		Fields: graphql.Fields{
			"id":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"name": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"color": &graphql.Field{Type: graphql.NewNonNull(s.graphQLEnum(
				"ProjectV2SingleSelectFieldOptionColor",
				"BLUE", "GRAY", "GREEN", "ORANGE", "PINK", "PURPLE", "RED", "YELLOW",
			))},
			"description": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	iterationType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2Iteration",
		Fields: graphql.Fields{
			"id":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"title":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"startDate": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"duration":  &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
	iterationConfigurationType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2IterationConfiguration",
		Fields: graphql.Fields{
			"startDate":  &graphql.Field{Type: graphql.String},
			"duration":   &graphql.Field{Type: graphql.Int},
			"iterations": &graphql.Field{Type: graphql.NewList(iterationType)},
		},
	})
	s.graphqlTypes.projectV2FieldTypeMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2Field",
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return src["nodeID"], nil
				},
			},
			"name": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"dataType": &graphql.Field{Type: graphql.NewNonNull(s.graphQLEnum(
				"ProjectV2FieldType",
				"ASSIGNEES", "DATE", "ITERATION", "LABELS", "LINKED_PULL_REQUESTS",
				"MILESTONE", "NUMBER", "REPOSITORY", "REVIEWERS", "SINGLE_SELECT",
				"TEXT", "TITLE", "TRACKED_BY", "TRACKS",
			))},
			"options":                &graphql.Field{Type: graphql.NewList(optionType)},
			"iterationConfiguration": &graphql.Field{Type: iterationConfigurationType},
			"createdAt":              &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime"))},
			"updatedAt":              &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime"))},
		},
	})
	s.graphqlTypes.projectV2FieldConnectionMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2FieldConnection",
		Fields: graphql.Fields{
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"nodes":      &graphql.Field{Type: graphql.NewList(s.graphqlTypes.projectV2FieldTypeMemo)},
			"edges":      &graphql.Field{Type: graphql.NewList(graphql.NewObject(graphql.ObjectConfig{Name: "ProjectV2FieldEdge", Fields: graphql.Fields{"node": &graphql.Field{Type: s.graphqlTypes.projectV2FieldTypeMemo}, "cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)}}}))},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})
	return s.graphqlTypes.projectV2FieldConnectionMemo
}

func (s *Server) projectV2ViewConnectionType() *graphql.Object {
	if s.graphqlTypes.projectV2ViewConnectionMemo != nil {
		return s.graphqlTypes.projectV2ViewConnectionMemo
	}
	s.graphqlTypes.projectV2ViewTypeMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2View",
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return src["nodeID"], nil
				},
			},
			"number": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"name":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"layout": &graphql.Field{
				Type: graphql.NewNonNull(s.graphQLEnum("ProjectV2ViewLayout", "BOARD_LAYOUT", "ROADMAP_LAYOUT", "TABLE_LAYOUT")),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					layout, _ := src["layout"].(string)
					layout = strings.ToUpper(layout)
					if !strings.HasSuffix(layout, "_LAYOUT") {
						layout += "_LAYOUT"
					}
					return layout, nil
				},
			},
			"filter":    &graphql.Field{Type: graphql.String},
			"createdAt": &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime"))},
			"updatedAt": &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime"))},
			"visibleFieldIds": &graphql.Field{
				Type: graphql.NewList(graphql.NewNonNull(graphql.Int)),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return src["visibleFieldIds"], nil
				},
			},
		},
	})
	s.graphqlTypes.projectV2ViewConnectionMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ViewConnection",
		Fields: graphql.Fields{
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"nodes":      &graphql.Field{Type: graphql.NewList(s.graphqlTypes.projectV2ViewTypeMemo)},
			"edges":      &graphql.Field{Type: graphql.NewList(graphql.NewObject(graphql.ObjectConfig{Name: "ProjectV2ViewEdge", Fields: graphql.Fields{"node": &graphql.Field{Type: s.graphqlTypes.projectV2ViewTypeMemo}, "cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)}}}))},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})
	return s.graphqlTypes.projectV2ViewConnectionMemo
}

func (s *Server) projectV2ItemConnectionType() *graphql.Object {
	if s.graphqlTypes.projectV2ItemConnectionTypeMemo != nil {
		return s.graphqlTypes.projectV2ItemConnectionTypeMemo
	}
	projectV2Type := s.projectV2GraphQLTypes()
	s.graphqlTypes.projectV2SingleSelectValueMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemFieldSingleSelectValue",
		Fields: graphql.Fields{
			"optionId": &graphql.Field{
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return src["optionId"], nil
				},
			},
			"name": &graphql.Field{
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return src["name"], nil
				},
			},
		},
	})
	s.graphqlTypes.projectV2TextValueMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemFieldTextValue",
		Fields: graphql.Fields{
			"text": &graphql.Field{
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return src["text"], nil
				},
			},
		},
	})
	s.graphqlTypes.projectV2NumberValueMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemFieldNumberValue",
		Fields: graphql.Fields{
			"number": &graphql.Field{
				Type: graphql.Float,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return src["number"], nil
				},
			},
		},
	})
	s.graphqlTypes.projectV2DateValueMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemFieldDateValue",
		Fields: graphql.Fields{
			"date": &graphql.Field{
				Type: s.graphQLStringScalar("Date"),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return src["date"], nil
				},
			},
		},
	})
	s.graphqlTypes.projectV2IterationValueMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemFieldIterationValue",
		Fields: graphql.Fields{
			"iterationId": &graphql.Field{
				Type: graphql.NewNonNull(graphql.String),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return src["iterationId"], nil
				},
			},
			"title": &graphql.Field{
				Type: graphql.NewNonNull(graphql.String),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return src["title"], nil
				},
			},
			"startDate": &graphql.Field{
				Type: graphql.NewNonNull(s.graphQLStringScalar("Date")),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return src["startDate"], nil
				},
			},
			"duration": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Int),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return src["duration"], nil
				},
			},
		},
	})
	s.graphqlTypes.projectV2ItemFieldValueUnionMemo = graphql.NewUnion(graphql.UnionConfig{
		Name:  "ProjectV2ItemFieldValue",
		Types: []*graphql.Object{s.graphqlTypes.projectV2SingleSelectValueMemo, s.graphqlTypes.projectV2TextValueMemo, s.graphqlTypes.projectV2NumberValueMemo, s.graphqlTypes.projectV2DateValueMemo, s.graphqlTypes.projectV2IterationValueMemo},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			src, _ := p.Value.(map[string]interface{})
			switch src["kind"] {
			case string(ProjectV2FieldText):
				return s.graphqlTypes.projectV2TextValueMemo
			case string(ProjectV2FieldNumber):
				return s.graphqlTypes.projectV2NumberValueMemo
			case string(ProjectV2FieldDate):
				return s.graphqlTypes.projectV2DateValueMemo
			case string(ProjectV2FieldIteration):
				return s.graphqlTypes.projectV2IterationValueMemo
			default:
				return s.graphqlTypes.projectV2SingleSelectValueMemo
			}
		},
	})
	s.graphqlTypes.projectV2ItemTypeMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2Item",
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return src["nodeID"], nil
				},
			},
			"project": &graphql.Field{
				Type: graphql.NewNonNull(projectV2Type),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return src["project"], nil
				},
			},
			// fieldValueByName — looks up the named field on the item's
			// project, returns the stored value (nil when unset).
			"fieldValueByName": &graphql.Field{
				Type: s.graphqlTypes.projectV2ItemFieldValueUnionMemo,
				Args: graphql.FieldConfigArgument{
					"name": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					name, _ := p.Args["name"].(string)
					byName, _ := src["fieldValuesByName"].(map[string]interface{})
					if byName == nil {
						return nil, nil
					}
					v, ok := byName[name]
					if !ok {
						return nil, nil
					}
					return v, nil
				},
			},
		},
	})
	projectV2ItemEdgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemEdge",
		Fields: graphql.Fields{
			"node":   &graphql.Field{Type: s.graphqlTypes.projectV2ItemTypeMemo},
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	s.graphqlTypes.projectV2ItemConnectionTypeMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2ItemConnection",
		Fields: graphql.Fields{
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"nodes":      &graphql.Field{Type: graphql.NewList(s.graphqlTypes.projectV2ItemTypeMemo)},
			"edges":      &graphql.Field{Type: graphql.NewList(projectV2ItemEdgeType)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})
	s.ensureProjectV2ItemsField()
	return s.graphqlTypes.projectV2ItemConnectionTypeMemo
}

// projectV2ItemToGQL builds the GraphQL source map for a single project
// item, embedding the parent project's map so ProjectV2Item.project
// resolves cleanly without a second lookup. Field values are
// pre-resolved into fieldValuesByName so the fieldValueByName(name:)
// resolver is a direct map lookup.
func projectV2ItemToGQL(it *ProjectV2Item, st *Store) map[string]interface{} {
	var projectMap map[string]interface{}
	if p := st.ProjectsV2.GetProject(it.ProjectID); p != nil {
		projectMap = projectV2ToGQL(p)
	}
	byName := map[string]interface{}{}
	for fieldID, val := range it.FieldValues {
		field := st.ProjectsV2.GetField(fieldID)
		if field == nil {
			continue
		}
		byName[field.Name] = projectV2FieldValueToGQL(val, field)
	}
	return map[string]interface{}{
		"nodeID":            it.NodeID,
		"project":           projectMap,
		"fieldValuesByName": byName,
	}
}

// projectV2FieldValueToGQL renders a persisted ProjectV2 field value as
// the matching GraphQL union source map.
func projectV2FieldValueToGQL(v *ProjectV2ItemFieldValue, f *ProjectV2Field) map[string]interface{} {
	out := map[string]interface{}{"kind": string(f.DataType)}
	switch f.DataType {
	case ProjectV2FieldText:
		out["text"] = v.TextValue
	case ProjectV2FieldNumber:
		out["number"] = v.NumberValue
	case ProjectV2FieldDate:
		out["date"] = v.DateValue
	case ProjectV2FieldIteration:
		out["iterationId"] = v.IterationID
		if f.Iteration != nil {
			for _, it := range f.Iteration.Iterations {
				if it.ID == v.IterationID {
					out["title"] = it.Title
					out["startDate"] = it.StartDate
					out["duration"] = it.Duration
					break
				}
			}
		}
	default:
		out["optionId"] = v.OptionID
		out["name"] = v.OptionName
	}
	return out
}

// projectV2ToGQL renders a project as a GraphQL source map. The store is not
// embedded in the map: resolvers reach it through their *Server closure, so a
// live *Store never flows through the resolver graph as an untyped entry.
func projectV2ToGQL(p *ProjectV2) map[string]interface{} {
	return map[string]interface{}{
		"id":     p.ID,
		"nodeID": p.NodeID,
		"number": p.Number,
		"title":  p.Title,
		"closed": p.Closed,
		"public": p.Public,
		"url":    p.URL,
	}
}

func projectV2SourceID(source interface{}) (int, error) {
	src, ok := source.(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("resolve source: unexpected type %T", source)
	}
	id, ok := src["id"].(int)
	if !ok || id == 0 {
		return 0, fmt.Errorf("project source missing id")
	}
	return id, nil
}

func projectV2FieldToGQL(f *ProjectV2Field) map[string]interface{} {
	options := make([]map[string]interface{}, 0, len(f.Options))
	for _, opt := range f.Options {
		options = append(options, map[string]interface{}{
			"id":          opt.ID,
			"name":        opt.Name,
			"color":       opt.Color,
			"description": opt.Description,
		})
	}
	var iteration map[string]interface{}
	if f.Iteration != nil {
		iterations := make([]map[string]interface{}, 0, len(f.Iteration.Iterations))
		for _, it := range f.Iteration.Iterations {
			iterations = append(iterations, map[string]interface{}{
				"id":        it.ID,
				"title":     it.Title,
				"startDate": it.StartDate,
				"duration":  it.Duration,
			})
		}
		iteration = map[string]interface{}{
			"startDate":  f.Iteration.StartDate,
			"duration":   f.Iteration.Duration,
			"iterations": iterations,
		}
	}
	return map[string]interface{}{
		"nodeID":                 f.NodeID,
		"name":                   f.Name,
		"dataType":               string(f.DataType),
		"options":                options,
		"iterationConfiguration": iteration,
		"createdAt":              f.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":              f.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func projectV2ViewToGQL(v *ProjectV2View) map[string]interface{} {
	var filter interface{}
	if v.Filter != nil {
		filter = *v.Filter
	}
	visible := append([]int(nil), v.VisibleFields...)
	return map[string]interface{}{
		"nodeID":          v.NodeID,
		"number":          v.Number,
		"name":            v.Name,
		"layout":          v.Layout,
		"filter":          filter,
		"visibleFieldIds": visible,
		"createdAt":       v.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":       v.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// projectItemsConnectionForIssue returns the source map for the
// Issue.projectItems / PullRequest.projectItems connection.
func projectItemsConnectionForIssue(st *Store, issueID int, args map[string]interface{}) map[string]interface{} {
	items := st.ProjectsV2.ListItemsForIssue(issueID)
	nodes := make([]map[string]interface{}, 0, len(items))
	for _, it := range items {
		nodes = append(nodes, projectV2ItemToGQL(it, st))
	}
	return paginateGQLMaps(nodes, args)
}

// emitIssueStateChange mirrors the REST issue-update path for a GraphQL-driven
// close/reopen: it records the timeline event and delivers the issues webhook,
// but only when the state actually transitioned (so `on: issues` workflows fire
// for the gh CLI, which mutates over GraphQL).
func (s *Server) emitIssueStateChange(issue *Issue, user *User, previousState, action string) {
	if issue == nil || user == nil {
		return
	}
	switch action {
	case "closed":
		if previousState == "CLOSED" {
			return
		}
	case "reopened":
		if previousState == "OPEN" {
			return
		}
	default:
		return
	}
	repo := s.store.GetRepoByID(issue.RepoID)
	if repo == nil {
		return
	}
	s.store.RecordIssueEvent(repo.ID, issue.ID, user.ID, action, nil)
	s.emitWebhookEvent(repo.FullName, "issues", action, buildIssuesPayload(s.store, repo, issue, user, action))
}

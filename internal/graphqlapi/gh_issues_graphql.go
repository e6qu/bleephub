package graphqlapi

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// addIssueFieldsToSchema adds Issue types, queries, and mutations to the schema.
func (s *Resolver) addIssueFieldsToSchema(userType, repoType, mutationType, queryType *graphql.Object, nodeInterface *graphql.Interface) (*graphql.Object, *graphql.Object) {
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	lockReasonEnum := s.graphQLEnum("LockReason", "OFF_TOPIC", "RESOLVED", "SPAM", "TOO_HEATED")
	issueStateReasonEnum := s.graphQLEnum("IssueStateReason", "COMPLETED", "NOT_PLANNED", "REOPENED")
	issueStateEnum := s.sharedEnum("IssueState", "OPEN", "CLOSED")
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
	// Label types (shared with PullRequest via the registry)
	labelConnectionType := s.gqlLabelConnectionType()

	// Milestone type
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
	s.graphqlTypes.milestone = issueMilestoneType

	milestoneEdgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "MilestoneEdge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: issueMilestoneType},
		},
	})
	milestoneConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "MilestoneConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(issueMilestoneType)},
			"edges":      &graphql.Field{Type: graphql.NewList(milestoneEdgeType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})

	// Reaction group type (shared via the registry)
	reactionGroupType := s.gqlReactionGroupType()

	// Comment types (shared with PullRequest via the registry)
	issueCommentType := s.gqlIssueCommentType()
	commentConnectionType := s.gqlIssueCommentConnectionType()

	// Assignee connection (shared UserConnection via the registry)

	assigneeConnectionType := s.gqlUserConnectionType(userType)

	// Memoized: issue-type and issue-field mutations return these same objects,
	// and graphql-go refuses a schema holding two types of one name.
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

	s.graphqlTypes.issueType = issueTypeMetaType

	subIssuesSummaryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "SubIssuesSummary",
		Fields: graphql.Fields{
			"total":            &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"completed":        &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"percentCompleted": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})

	issueFieldValueConnectionType := s.issueFieldValueGraphQLConnectionType()

	// Issue type
	issueType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Issue",
		// Claim every interface at construction: graphql-go memoizes the interface
		// list once, so one added later can never gain Issue as a possible type.
		Interfaces: []*graphql.Interface{
			nodeInterface, s.gqlLockableInterface(), s.gqlLabelableInterface(),
			s.graphqlTypes.reactable, s.uniformResourceLocatableInterface(),
			s.gqlClosableInterface(), s.gqlAssignableInterface(),
		},
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
			"resourcePath":     &graphql.Field{Type: graphql.NewNonNull(uri)},
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
				// The full Relay set the Assignable interface contract requires.
				Args: relayConnectionArgs(),
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
						// Untyped nil: graphql-go's NonNull checks fire even on a
						// nil-valued map[string]interface{}.
						return nil, nil
					}
					return m, nil
				},
			},
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

	// Registered for interface ResolveType dispatch.
	s.graphqlTypes.issue = issueType
	s.addReactableFields(issueType, "issue")

	// parent / subIssues are self-referential: added after issueType exists and
	// resolved lazily from the sub-issue store — embedding full issue maps
	// eagerly would recurse parent↔child forever.
	issueType.AddFieldConfig("parent", &graphql.Field{
		Type: issueType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			i, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			issueID, _ := i["databaseId"].(int)
			s.store.Mu.RLock()
			parentID, hasParent := s.store.SubIssueParent[issueID]
			s.store.Mu.RUnlock()
			if !hasParent {
				return nil, nil
			}
			parent := s.store.GetIssue(parentID)
			if parent == nil {
				return nil, nil
			}
			return issueToGQL(parent, s.store), nil
		},
	})
	issueType.AddFieldConfig("subIssues", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlIssueConnectionType(issueType)),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			i, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			issueID, _ := i["databaseId"].(int)
			s.store.Mu.RLock()
			childIDs := append([]int(nil), s.store.SubIssueLists[issueID]...)
			s.store.Mu.RUnlock()
			nodes := make([]map[string]interface{}, 0, len(childIDs))
			for _, childID := range childIDs {
				if child := s.store.GetIssue(childID); child != nil {
					nodes = append(nodes, issueToGQL(child, s.store))
				}
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
	issueType.AddFieldConfig("repository", &graphql.Field{
		Type: graphql.NewNonNull(repoType),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			i, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			issueID, _ := i["databaseId"].(int)
			issue := s.store.GetIssue(issueID)
			if issue == nil {
				return nil, fmt.Errorf("issue %d not found", issueID)
			}
			repo := s.store.GetRepoByID(issue.RepoID)
			if repo == nil {
				return nil, fmt.Errorf("repository %d not found", issue.RepoID)
			}
			return repoToGraphQL(s.store, s.store.SnapRepo(repo)), nil
		},
	})

	// Issue connection (shared with PullRequest via the registry)
	issueConnectionType := s.gqlIssueConnectionType(issueType)

	// Issue filters input
	issueFieldValueFilterInput := s.mutationInput("IssueFieldValueFilter", graphql.InputObjectConfigFieldMap{
		"dateValue":               gqlString(),
		"fieldId":                 gqlID(),
		"fieldName":               gqlString(),
		"multiSelectOptionIds":    gqlListOf(graphql.ID),
		"multiSelectOptionValues": gqlListOf(graphql.String),
		"numberValue":             gqlInputOf(graphql.Float),
		"singleSelectOptionId":    gqlID(),
		"singleSelectOptionValue": gqlString(),
		"textValue":               gqlString(),
	})
	issueFiltersInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "IssueFilters",
		Fields: graphql.InputObjectConfigFieldMap{
			"assignee":         &graphql.InputObjectFieldConfig{Type: graphql.String},
			"createdBy":        &graphql.InputObjectFieldConfig{Type: graphql.String},
			"mentioned":        &graphql.InputObjectFieldConfig{Type: graphql.String},
			"labels":           &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"states":           &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(issueStateEnum))},
			"issueFieldValues": gqlListOf(issueFieldValueFilterInput),
			"milestone":        gqlString(),
			"milestoneNumber":  gqlString(),
			"since":            gqlInputOf(dateTime),
			"type":             gqlString(),
			"viewerSubscribed": gqlBool(),
		},
	})

	// Add fields to Repository type

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
			// bleephub models pull/push/admin only, never MAINTAIN/TRIAGE; null for no access.
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
			issues := make([]*store.Issue, 0, len(storedIssues))
			for _, issue := range storedIssues {
				issues = append(issues, s.store.SnapIssue(issue))
			}

			if states, ok := p.Args["states"].([]interface{}); ok && len(states) > 0 {
				stateMap := make(map[string]bool)
				for _, st := range states {
					stateMap[fmt.Sprintf("%v", st)] = true
				}
				var filtered []*store.Issue
				for _, i := range issues {
					if stateMap[i.State] {
						filtered = append(filtered, i)
					}
				}
				issues = filtered
			}

			if labelNames, ok := p.Args["labels"].([]interface{}); ok && len(labelNames) > 0 {
				var names []string
				for _, ln := range labelNames {
					names = append(names, fmt.Sprintf("%v", ln))
				}
				var filtered []*store.Issue
				for _, i := range issues {
					if store.IssueHasAllLabels(s.store, i, names, repoID) {
						filtered = append(filtered, i)
					}
				}
				issues = filtered
			}

			if filterBy, ok := p.Args["filterBy"].(map[string]interface{}); ok {
				if assignee, ok := filterBy["assignee"].(string); ok && assignee != "" {
					u := s.store.LookupUserByLogin(assignee)
					var filtered []*store.Issue
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
					// An unresolved login matches no issue; keeping the old list
					// would silently widen a typo to every issue.
					issues = filtered
				}
				if creator, ok := filterBy["createdBy"].(string); ok && creator != "" {
					u := s.store.LookupUserByLogin(creator)
					var filtered []*store.Issue
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
					var filtered []*store.Issue
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
					var filtered []*store.Issue
					for _, issue := range issues {
						if store.IssueHasAllLabels(s.store, issue, names, repoID) {
							filtered = append(filtered, issue)
						}
					}
					issues = filtered
				}
				if mentioned, ok := filterBy["mentioned"].(string); ok && mentioned != "" {
					needle := "@" + strings.ToLower(mentioned)
					var filtered []*store.Issue
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
				// GitHub returns a typed NOT_FOUND error, not bare null.
				return nil, &ghNotFoundError{
					message: fmt.Sprintf("Could not resolve to an Issue with the number of %d.", number),
				}
			}
			return issueToGQL(issue, s.store), nil
		},
	})

	// issueOrPullRequest is defined in addPullRequestFieldsToSchema, after the
	// PullRequest type exists, so it can return an Issue|PullRequest union.

	repoType.AddFieldConfig("labels", &graphql.Field{
		Type: labelConnectionType,
		Args: graphql.FieldConfigArgument{
			"first":  &graphql.ArgumentConfig{Type: graphql.Int},
			"last":   &graphql.ArgumentConfig{Type: graphql.Int},
			"after":  &graphql.ArgumentConfig{Type: graphql.String},
			"before": &graphql.ArgumentConfig{Type: graphql.String},
			"query":  &graphql.ArgumentConfig{Type: graphql.String},
			// gh sends literal enum names, so field/direction must be enums.
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

			if q, ok := p.Args["query"].(string); ok && q != "" {
				q = strings.ToLower(q)
				var filtered []*store.IssueLabel
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
				if len(states) == 1 {
					state = strings.ToLower(fmt.Sprintf("%v", states[0]))
				}
			}

			milestones := s.store.ListMilestones(repoID, state)

			if q, ok := p.Args["query"].(string); ok && q != "" {
				q = strings.ToLower(q)
				var filtered []*store.Milestone
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
			return paginateGQL(milestones, first, after, milestoneToGQL, func(m *store.Milestone) string { return m.NodeID }), nil
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
			usersByID := map[int]*store.User{}
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
			users := make([]*store.User, 0, len(usersByID))
			for _, user := range usersByID {
				users = append(users, user)
			}

			if q, ok := p.Args["query"].(string); ok && q != "" {
				q = strings.ToLower(q)
				var filtered []*store.User
				for _, u := range users {
					if strings.Contains(strings.ToLower(u.Login), q) || strings.Contains(strings.ToLower(u.Name), q) {
						filtered = append(filtered, u)
					}
				}
				users = filtered
			}

			// Sort by ID: map iteration above is nondeterministic; cursor
			// pagination must be stable across pages.
			sort.Slice(users, func(a, b int) bool { return users[a].ID < users[b].ID })

			first := 0
			if f, ok := intArg(p.Args, "first"); ok {
				first = f
			}
			after, _ := p.Args["after"].(string)
			return paginateGQL(users, first, after, userToGraphQL, func(u *store.User) string { return u.NodeID }), nil
		},
	})

	// Mutations

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
			// gh's IssueCreate always serializes these, so the input must declare
			// them or variable coercion rejects the mutation. projectIds add the
			// issue to those ProjectV2 boards; issueTemplate is an accepted no-op.
			"projectIds":      &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.ID))},
			"issueTemplate":   &graphql.InputObjectFieldConfig{Type: graphql.String},
			"parentIssueId":   &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"projectV2Ids":    &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.ID))},
			"agentAssignment": &graphql.InputObjectFieldConfig{Type: s.gqlAgentAssignmentInput()},
			"issueFields":     &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(s.gqlIssueFieldCreateOrUpdateInput()))},
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
			user := s.ghUserFromContext(p.Context)

			input, _ := p.Args["input"].(map[string]interface{})
			repoNodeID, _ := input["repositoryId"].(string)
			title, _ := input["title"].(string)
			body, _ := input["body"].(string)

			repo := store.FindRepoByNodeID(s.store, repoNodeID)
			if repo == nil {
				return nil, gqlMissingNode("Repository", repoNodeID)
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
				it := store.FindIssueTypeByNodeID(s.store, itNodeID)
				if it == nil || s.store.GetAssignableIssueTypeForRepo(repo, it.ID) == nil {
					return nil, gqlMissingNode("IssueType", itNodeID)
				}
				issueTypeID = it.ID
			}

			issue := s.store.CreateIssue(repo.ID, user.ID, title, body, labelIDs, assigneeIDs, milestoneID)
			if issue == nil {
				return nil, fmt.Errorf("issue creation failed")
			}
			if issueTypeID > 0 {
				s.store.UpdateIssue(issue.ID, func(i *store.Issue) {
					i.IssueTypeID = issueTypeID
				})
				issue = s.store.GetIssue(issue.ID)
			}

			// Add the new issue to each named ProjectV2 board.
			if raw, ok := input["projectIds"].([]interface{}); ok {
				for _, v := range raw {
					nodeID, _ := v.(string)
					if nodeID == "" {
						continue
					}
					proj := s.store.ProjectsV2.LookupProjectByNodeID(nodeID)
					if proj == nil {
						return nil, gqlMissingNode("ProjectV2", nodeID)
					}
					s.store.ProjectsV2.AddItem(proj.ID, "Issue", issue.ID, user.ID)
				}
			}

			// Deliver the issues/opened webhook, matching the REST create path, so
			// `on: issues` workflows fire for GraphQL-created issues.
			s.emitWebhookEvent(repo.FullName, "issues", "opened", s.buildIssuesPayload(repo, issue, user, "opened"))

			return map[string]interface{}{
				"issue": issueToGQL(issue, s.store),
			}, nil
		},
	})

	closeIssueInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CloseIssueInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"issueId":          &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"stateReason":      &graphql.InputObjectFieldConfig{Type: issueClosedStateReasonEnum},
			"confidence":       &graphql.InputObjectFieldConfig{Type: s.sharedEnum("IssueEventConfidenceLevel", "HIGH", "LOW", "MEDIUM")},
			"duplicateIssueId": &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"isSuggestion":     &graphql.InputObjectFieldConfig{Type: graphql.Boolean},
			"rationale":        &graphql.InputObjectFieldConfig{Type: graphql.String},
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
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			issueNodeID, _ := input["issueId"].(string)
			stateReason, _ := input["stateReason"].(string)
			if stateReason == "" {
				stateReason = "COMPLETED"
			}

			issue := store.FindIssueByNodeID(s.store, issueNodeID)
			if issue == nil {
				return nil, gqlMissingNodeType("Issue")
			}
			previousState := issue.State

			s.store.UpdateIssue(issue.ID, func(i *store.Issue) {
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
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			issueNodeID, _ := input["issueId"].(string)

			issue := store.FindIssueByNodeID(s.store, issueNodeID)
			if issue == nil {
				return nil, gqlMissingNodeType("Issue")
			}
			previousState := issue.State

			s.store.UpdateIssue(issue.ID, func(i *store.Issue) {
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

	// Pinned issues (pinIssue / unpinIssue, Repository.pinnedIssues)

	pinnedIssueType := graphql.NewObject(graphql.ObjectConfig{
		Name: "PinnedIssue",
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					n, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return n["nodeID"], nil
				},
			},
			"databaseId": &graphql.Field{Type: graphql.Int},
			"fullDatabaseId": &graphql.Field{
				Type: s.graphQLStringScalar("BigInt"),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, _ := p.Source.(map[string]interface{})
					return strconv.Itoa(srcInt(src, "databaseId")), nil
				},
			},
			"issue":      &graphql.Field{Type: graphql.NewNonNull(issueType)},
			"pinnedBy":   &graphql.Field{Type: graphql.NewNonNull(s.graphqlTypes.actor)},
			"repository": &graphql.Field{Type: graphql.NewNonNull(repoType)},
		},
	})

	pinnedIssueEdgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "PinnedIssueEdge",
		Fields: graphql.Fields{
			"node":   &graphql.Field{Type: pinnedIssueType},
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})

	pinnedIssueConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "PinnedIssueConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(pinnedIssueType)},
			"edges":      &graphql.Field{Type: graphql.NewList(pinnedIssueEdgeType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})

	repoType.AddFieldConfig("pinnedIssues", &graphql.Field{
		Type: pinnedIssueConnectionType,
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, _ := repo["databaseId"].(int)
			issues := s.store.ListPinnedIssues(repoID)
			nodes := make([]map[string]interface{}, 0, len(issues))
			for _, issue := range issues {
				pinner := s.store.GetUserByID(issue.PinnedByID)
				if pinner == nil {
					// The pinner's account can be gone; fall back to the author to
					// keep the non-null Actor contract.
					pinner = s.store.GetUserByID(issue.AuthorID)
				}
				var pinnedBy map[string]interface{}
				if pinner != nil {
					pinnedBy = userToGraphQL(pinner)
				}
				nodes = append(nodes, map[string]interface{}{
					"nodeID":     pinnedIssueNodeID(issue.ID),
					"databaseId": issue.ID,
					"issue":      issueToGQL(issue, s.store),
					"pinnedBy":   optionalObject(pinnedBy),
					"repository": repo,
				})
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})

	pinIssueInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "PinIssueInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"issueId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})

	pinIssuePayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "PinIssuePayload",
		Fields: graphql.Fields{
			"issue": &graphql.Field{Type: issueType},
		},
	})

	s.registerMutation(mutationType, "pinIssue", &graphql.Field{
		Type: pinIssuePayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(pinIssueInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			issueNodeID, _ := input["issueId"].(string)

			issue := store.FindIssueByNodeID(s.store, issueNodeID)
			if issue == nil {
				return nil, gqlMissingNodeType("Issue")
			}
			alreadyPinned := issue.PinnedAt != nil
			if err := s.store.PinIssue(issue.ID, user.ID); err != nil {
				return nil, err
			}
			updated := s.store.GetIssue(issue.ID)
			if !alreadyPinned {
				if repo := s.store.GetRepoByID(updated.RepoID); repo != nil {
					s.emitWebhookEvent(repo.FullName, "issues", "pinned", s.buildIssuesPayload(repo, updated, user, "pinned"))
				}
			}
			return map[string]interface{}{
				"issue": issueToGQL(updated, s.store),
			}, nil
		},
	})

	unpinIssueInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UnpinIssueInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"issueId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})

	unpinIssuePayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "UnpinIssuePayload",
		Fields: graphql.Fields{
			"id":    &graphql.Field{Type: graphql.ID},
			"issue": &graphql.Field{Type: issueType},
		},
	})

	s.registerMutation(mutationType, "unpinIssue", &graphql.Field{
		Type: unpinIssuePayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(unpinIssueInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			issueNodeID, _ := input["issueId"].(string)

			issue := store.FindIssueByNodeID(s.store, issueNodeID)
			if issue == nil {
				return nil, gqlMissingNodeType("Issue")
			}
			wasPinned := s.store.UnpinIssue(issue.ID, user.ID)
			updated := s.store.GetIssue(issue.ID)
			var unpinnedID interface{}
			if wasPinned {
				unpinnedID = pinnedIssueNodeID(updated.ID)
				if repo := s.store.GetRepoByID(updated.RepoID); repo != nil {
					s.emitWebhookEvent(repo.FullName, "issues", "unpinned", s.buildIssuesPayload(repo, updated, user, "unpinned"))
				}
			}
			return map[string]interface{}{
				"id":    unpinnedID,
				"issue": issueToGQL(updated, s.store),
			}, nil
		},
	})

	// transferIssue / deleteIssue

	transferIssueInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "TransferIssueInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"issueId":               &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"repositoryId":          &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"createLabelsIfMissing": &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: false},
		},
	})

	transferIssuePayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "TransferIssuePayload",
		Fields: graphql.Fields{
			"issue": &graphql.Field{Type: issueType},
		},
	})

	s.registerMutation(mutationType, "transferIssue", &graphql.Field{
		Type: transferIssuePayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(transferIssueInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			issueNodeID, _ := input["issueId"].(string)
			repoNodeID, _ := input["repositoryId"].(string)
			createLabelsIfMissing, _ := input["createLabelsIfMissing"].(bool)

			issue := store.FindIssueByNodeID(s.store, issueNodeID)
			if issue == nil {
				return nil, gqlMissingNodeType("Issue")
			}
			target := store.FindRepoByNodeID(s.store, repoNodeID)
			if target == nil {
				return nil, gqlMissingNode("Repository", repoNodeID)
			}
			source := s.store.GetRepoByID(issue.RepoID)
			if source == nil {
				return nil, gqlMissingNodeType("Repository")
			}
			if target.ID == source.ID {
				return nil, fmt.Errorf("issue is already in %s", target.FullName)
			}
			// An issue transfers only between repos owned by the same user or org.
			if target.OwnerID != source.OwnerID {
				return nil, fmt.Errorf("issues can only be transferred between repositories owned by the same user or organization")
			}
			moved := s.store.TransferIssue(issue.ID, target.ID, user.ID, createLabelsIfMissing)
			if moved == nil {
				return nil, gqlMissingNodeType("Issue")
			}
			// issues/transferred fires on the repository the issue left.
			s.emitWebhookEvent(source.FullName, "issues", "transferred", s.buildIssuesPayload(source, moved, user, "transferred"))
			return map[string]interface{}{
				"issue": issueToGQL(moved, s.store),
			}, nil
		},
	})

	deleteIssueInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "DeleteIssueInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"issueId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
		},
	})

	deleteIssuePayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DeleteIssuePayload",
		Fields: graphql.Fields{
			"repository": &graphql.Field{Type: repoType},
		},
	})

	s.registerMutation(mutationType, "deleteIssue", &graphql.Field{
		Type: deleteIssuePayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(deleteIssueInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			issueNodeID, _ := input["issueId"].(string)

			issue := store.FindIssueByNodeID(s.store, issueNodeID)
			if issue == nil {
				return nil, gqlMissingNodeType("Issue")
			}
			repo := s.store.GetRepoByID(issue.RepoID)
			// An enterprise can forbid issue deletion outright, whatever repo
			// standing the member holds.
			if err := s.enterprisePolicyRefusal(p, repo, func(policy store.EnterprisePolicy) string {
				return policy.MembersCanDeleteIssues
			}, "Deleting issues is disabled by an enterprise policy."); err != nil {
				return nil, err
			}
			// Render the payload before the rows disappear.
			var payload map[string]interface{}
			if repo != nil {
				payload = s.buildIssuesPayload(repo, issue, user, "deleted")
			}
			if !s.store.DeleteIssue(issue.ID) {
				return nil, gqlMissingNodeType("Issue")
			}
			if repo != nil {
				s.emitWebhookEvent(repo.FullName, "issues", "deleted", payload)
			}
			result := map[string]interface{}{"repository": nil}
			if repo != nil {
				result["repository"] = repoToGraphQL(s.store, s.store.SnapRepo(repo))
			}
			return result, nil
		},
	})

	addCommentInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "AddCommentInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"subjectId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"body":      &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		},
	})

	commentEdgeType := s.gqlIssueCommentEdgeType()
	_ = issueCommentType

	addCommentPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "AddCommentPayload",
		Fields: graphql.Fields{
			"commentEdge": &graphql.Field{Type: commentEdgeType},
			"subject":     &graphql.Field{Type: nodeInterface},
		},
	})
	// timelineEdge is added later, once the timeline family builds
	// IssueTimelineItemEdge.
	s.stashNamedObject(addCommentPayloadType)

	s.registerMutation(mutationType, "addComment", &graphql.Field{
		Type: addCommentPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(addCommentInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)

			input, _ := p.Args["input"].(map[string]interface{})
			subjectNodeID, _ := input["subjectId"].(string)
			body, _ := input["body"].(string)

			// A PR is an issue, so subjectId may be either; resolve both.
			if issue := store.FindIssueByNodeID(s.store, subjectNodeID); issue != nil {
				comment := s.store.CreateComment(issue.ID, user.ID, body)
				if comment == nil {
					return nil, fmt.Errorf("comment creation failed")
				}
				return map[string]interface{}{
					"commentEdge": map[string]interface{}{"node": commentToGQL(comment, s.store)},
					"subject":     issueToGQL(issue, s.store),
				}, nil
			}
			if pr := store.FindPullRequestByNodeID(s.store, subjectNodeID); pr != nil {
				comment := s.store.CreateCommentFor("pull_request", pr.ID, user.ID, body)
				if comment == nil {
					return nil, fmt.Errorf("comment creation failed")
				}
				return map[string]interface{}{
					"commentEdge": map[string]interface{}{"node": commentToGQL(comment, s.store)},
					"subject":     pullRequestToGQL(pr, s.store),
				}, nil
			}
			return nil, gqlMissingNode("node", subjectNodeID)
		},
	})

	// The agent-triage input family on updateIssue. AgentAssignmentInput and
	// AssigneeUpdateInput are memoized (the assignment mutations name the same
	// two), so this reuses whichever installer ran first.
	confidenceEnum := s.sharedEnum("IssueEventConfidenceLevel", "HIGH", "LOW", "MEDIUM")
	agentAssignmentInput := s.mutationInput("AgentAssignmentInput", graphql.InputObjectConfigFieldMap{
		"baseRef":            gqlString(),
		"customAgent":        gqlString(),
		"customInstructions": gqlString(),
		"targetRepositoryId": gqlID(),
	})
	assigneeUpdateInput := s.mutationInput("AssigneeUpdateInput", graphql.InputObjectConfigFieldMap{
		"actorId":    gqlNonNullID(),
		"confidence": gqlInputOf(confidenceEnum),
		"rationale":  gqlString(),
		"suggest":    gqlBool(),
	})
	issueTypeUpdateInput := s.mutationInput("IssueTypeUpdateInput", graphql.InputObjectConfigFieldMap{
		"confidence":  gqlInputOf(confidenceEnum),
		"issueTypeId": gqlID(),
		"rationale":   gqlString(),
		"suggest":     gqlBool(),
	})
	labelUpdateInput := s.gqlLabelUpdateInput()
	issueStateUpdateInput := s.mutationInput("IssueStateUpdateInput", graphql.InputObjectConfigFieldMap{
		"confidence":       gqlInputOf(confidenceEnum),
		"duplicateIssueId": gqlID(),
		"rationale":        gqlString(),
		"stateReason":      gqlInputOf(issueClosedStateReasonEnum),
		"suggest":          gqlBool(),
		"value":            gqlNonNullInputOf(issueStateEnum),
	})
	issueFieldUpdateOperationEnum := s.sharedEnum("IssueFieldUpdateOperation", "ADD", "CLEAR", "REMOVE", "SET")
	issueFieldUpdateInput := s.mutationInput("IssueFieldUpdateInput", graphql.InputObjectConfigFieldMap{
		"fieldName": gqlNonNullString(),
		"operation": gqlNonNullInputOf(issueFieldUpdateOperationEnum),
		"value":     gqlString(),
	})
	updateIssueInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UpdateIssueInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"id":    &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"title": &graphql.InputObjectFieldConfig{Type: graphql.String},
			"body":  &graphql.InputObjectFieldConfig{Type: graphql.String},
			// IssueState enum, not String: a free-form string was written to the
			// store verbatim, so `state: "banana"` became the issue's state.
			"state":             &graphql.InputObjectFieldConfig{Type: issueStateEnum},
			"milestoneId":       &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"labelIds":          &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.ID))},
			"assigneeIds":       &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.ID))},
			"issueTypeId":       &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"agentAssignment":   gqlInputOf(agentAssignmentInput),
			"assignees":         gqlListOf(assigneeUpdateInput),
			"issueFieldUpdates": gqlListOf(issueFieldUpdateInput),
			"issueType":         gqlInputOf(issueTypeUpdateInput),
			"labels":            gqlListOf(labelUpdateInput),
			"projectIds":        gqlListOf(graphql.ID),
			"stateInput":        gqlInputOf(issueStateUpdateInput),
		},
	})

	updateIssuePayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "UpdateIssuePayload",
		Fields: graphql.Fields{
			"issue": &graphql.Field{Type: issueType},
			"actor": s.mutationActorField(),
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

			issue := store.FindIssueByNodeID(s.store, issueNodeID)
			if issue == nil {
				return nil, gqlMissingNodeType("Issue")
			}
			repo := s.store.GetRepoByID(issue.RepoID)
			if repo == nil {
				return nil, gqlMissingNodeType("Repository")
			}
			var issueTypeID *int
			if raw, present := input["issueTypeId"]; present {
				if itNodeID, ok := raw.(string); ok && itNodeID != "" {
					it := store.FindIssueTypeByNodeID(s.store, itNodeID)
					if it == nil || s.store.GetAssignableIssueTypeForRepo(repo, it.ID) == nil {
						return nil, gqlMissingNode("IssueType", itNodeID)
					}
					resolved := it.ID
					issueTypeID = &resolved
				} else {
					cleared := 0
					issueTypeID = &cleared
				}
			}
			// Triage — labels, assignees, milestone — needs push even for the author.
			triage := false
			for _, key := range []string{"labelIds", "assigneeIds", "milestoneId"} {
				if _, present := input[key]; present {
					triage = true
				}
			}
			if triage && !s.viewerHasRepoPermission(p.Context, repo, store.ScopeIssues, store.PermWrite) {
				return nil, fmt.Errorf("must have push access to Repository")
			}
			// Re-check the enum here to catch a caller that reached the resolver
			// another way, never letting an unknown word become the state.
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

			// FindIssueByNodeID returns the live row that UpdateIssue mutates in
			// place; snapshot the before-values the webhook fan-out diffs against.
			before := s.store.GetIssue(issue.ID)
			if before == nil {
				return nil, gqlMissingNodeType("Issue")
			}
			previousState := before.State
			s.store.UpdateIssue(issue.ID, func(i *store.Issue) {
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
			// Deliver the same per-change fan-out the REST PATCH does.
			change := store.SubjectChange{
				LabelsFrom:    before.LabelIDs,
				LabelsTo:      labelIDs,
				AssigneesFrom: before.AssigneeIDs,
				AssigneesTo:   assigneeIDs,
				MilestoneFrom: before.MilestoneID,
				MilestoneTo:   milestoneID,
			}
			if v, ok := input["title"].(string); ok && v != before.Title {
				change.TitleFrom = &before.Title
			}
			if v, ok := input["body"].(string); ok && v != before.Body {
				change.BodyFrom = &before.Body
			}
			user := s.ghUserFromContext(p.Context)
			if change.TitleFrom != nil && user != nil {
				// Record the `renamed` timeline event here too, or the history
				// would depend on whether the edit came via GraphQL or REST.
				s.store.RecordIssueEvent(repo.ID, issue.ID, user.ID, "renamed", map[string]interface{}{
					"rename_from": *change.TitleFrom,
					"rename_to":   updated.Title,
				})
			}
			s.emitIssueChanges(repo, updated, user, change)
			// Route through the shared helper so it records the same timeline
			// event closeIssue/reopenIssue do.
			switch newState {
			case "CLOSED":
				s.emitIssueStateChange(updated, user, previousState, "closed")
			case "OPEN":
				s.emitIssueStateChange(updated, user, previousState, "reopened")
			}
			return map[string]interface{}{
				"issue": issueToGQL(updated, s.store),
			}, nil
		},
	})

	// IssueType's residual members are installed after both the IssueType meta
	// object and the IssueFields union exist.
	s.addIssueTypeResidueFields()

	return issueType, issueMilestoneType
}

// GraphQL converter helpers

func (s *Resolver) issueFieldValueGraphQLConnectionType() *graphql.Object {
	// Shared enums: the issue-field mutations name the same three on their inputs.
	dataTypeEnum := s.sharedEnum("IssueFieldDataType",
		"DATE", "MULTI_SELECT", "NUMBER", "SINGLE_SELECT", "TEXT")
	visibilityEnum := s.sharedEnum("IssueFieldVisibility", "ALL", "ORG_ONLY")
	colorEnum := s.sharedEnum("IssueFieldSingleSelectOptionColor",
		"BLUE", "GRAY", "GREEN", "ORANGE", "PINK", "PURPLE", "RED", "YELLOW")

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

	// fieldUnion and valueUnion are memoized: the issue-field mutations return
	// them, and graphql-go refuses two unions of one name.
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

	s.graphqlTypes.issueFieldsUnion = fieldUnion

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
	s.graphqlTypes.issueFieldValueUnion = valueUnion

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

func issueToGQL(issue *store.Issue, st *store.Store) map[string]interface{} {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	labelNodes := make([]map[string]interface{}, 0)
	for _, lid := range issue.LabelIDs {
		if l, ok := st.Labels[lid]; ok {
			labelNodes = append(labelNodes, labelToGQL(l))
		}
	}

	assigneeNodes := make([]map[string]interface{}, 0)
	for _, aid := range issue.AssigneeIDs {
		if u, ok := st.Users[aid]; ok {
			assigneeNodes = append(assigneeNodes, userToGraphQL(u))
		}
	}

	var milestone map[string]interface{}
	if issue.MilestoneID > 0 {
		if ms, ok := st.Milestones[issue.MilestoneID]; ok {
			milestone = milestoneToGQL(ms)
		}
	}
	var issueType map[string]interface{}
	if it := st.IssueTypeForIssueLocked(issue); it != nil {
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

	// Resolve comments through the by-parent index to avoid scanning every
	// comment in the store once per issue.
	indexed := st.CommentsByParent[store.CommentCountKey("issue", issue.ID)]
	commentNodes := make([]map[string]interface{}, 0, len(indexed))
	for _, c := range indexed {
		commentNodes = append(commentNodes, commentToGQLLocked(c, st))
	}
	// Index order is nondeterministic across a reload; sort oldest-first for
	// stable cursor pagination.
	sortGQLNodesByCreatedAt(commentNodes)

	repo := st.Repos[issue.RepoID]
	url, resourcePath := "", ""
	if repo != nil {
		resourcePath = "/" + repo.FullName + "/issues/" + strconv.Itoa(issue.Number)
		url = externalURL(resourcePath)
	}

	// parent/subIssues resolve lazily; only the summary counts are computed here.
	totalSubIssues := 0
	completedSubIssues := 0
	for _, childID := range st.SubIssueLists[issue.ID] {
		child := st.Issues[childID]
		if child == nil {
			continue
		}
		totalSubIssues++
		if child.State == "CLOSED" {
			completedSubIssues++
		}
	}
	percentCompleted := 0
	if totalSubIssues > 0 {
		percentCompleted = completedSubIssues * 100 / totalSubIssues
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
		"authorID":         issue.AuthorID,
		"repoID":           issue.RepoID,
		"duplicateOfID":    issue.DuplicateOfID,
		"number":           issue.Number,
		"title":            issue.Title,
		"body":             issue.Body,
		"state":            issue.State,
		"stateReason":      stateReason,
		"url":              url,
		"resourcePath":     resourcePath,
		"createdAt":        issue.CreatedAt.Format(time.RFC3339),
		"updatedAt":        issue.UpdatedAt.Format(time.RFC3339),
		"closedAt":         closedAt,
		"isPinned":         issue.PinnedAt != nil,
		"locked":           issue.Locked,
		"activeLockReason": graphQLLockReason(string(issue.ActiveLockReason)),
		"author":           optionalRendered(st.Users[issue.AuthorID], userToGraphQL),
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
		"milestone": optionalObject(milestone),
		"issueType": optionalObject(issueType),
		"subIssuesSummary": map[string]interface{}{
			"total":            totalSubIssues,
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
		"reactionGroups": reactionGroupsForGraphQL(st.Reactions, "issue", issue.ID, 0),
	}
}

func graphQLLockReason(reason string) interface{} {
	if reason == "" {
		return nil
	}
	return strings.ToUpper(strings.ReplaceAll(reason, "-", "_"))
}

func (s *Resolver) gqlPageInfoType() *graphql.Object {
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

// gqlLabelType returns the shared Label object type (memoized); PRs and issues
// share one Label, as on GitHub.
func (s *Resolver) gqlLabelType() *graphql.Object {
	if s.graphqlTypes.labelType != nil {
		return s.graphqlTypes.labelType
	}
	s.graphqlTypes.labelType = graphql.NewObject(graphql.ObjectConfig{
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
	return s.graphqlTypes.labelType
}

// gqlLabelConnectionType returns the shared LabelConnection type (memoized).
func (s *Resolver) gqlLabelConnectionType() *graphql.Object {
	if s.graphqlTypes.labelConnection != nil {
		return s.graphqlTypes.labelConnection
	}
	labelEdge := graphql.NewObject(graphql.ObjectConfig{
		Name: "LabelEdge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: s.gqlLabelType()},
		},
	})
	s.graphqlTypes.labelConnection = graphql.NewObject(graphql.ObjectConfig{
		Name: "LabelConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(s.gqlLabelType())},
			"edges":      &graphql.Field{Type: graphql.NewList(labelEdge)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})
	return s.graphqlTypes.labelConnection
}

// gqlUserConnectionType returns the shared UserConnection type (memoized).
func (s *Resolver) gqlUserConnectionType(userType *graphql.Object) *graphql.Object {
	if s.graphqlTypes.userConnection != nil {
		return s.graphqlTypes.userConnection
	}
	s.graphqlTypes.userConnection = graphql.NewObject(graphql.ObjectConfig{
		Name: "UserConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(userType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})
	return s.graphqlTypes.userConnection
}

// gqlReactionGroupType returns the shared ReactionGroup type (memoized). Source
// maps come from reactionGroupsForGraphQL.
func (s *Resolver) gqlReactionGroupType() *graphql.Object {
	if s.graphqlTypes.reactionGroup != nil {
		return s.graphqlTypes.reactionGroup
	}
	reactionContentEnum := s.graphQLEnum(
		"ReactionContent",
		"CONFUSED", "EYES", "HEART", "HOORAY", "LAUGH", "ROCKET", "THUMBS_DOWN", "THUMBS_UP",
	)
	dateTime := s.graphQLStringScalar("DateTime")
	// A FieldsThunk defers field resolution until schema assembly, so this can be
	// built while the Reactable interface that references it is still assembling.
	s.graphqlTypes.reactionGroup = graphql.NewObject(graphql.ObjectConfig{
		Name: "ReactionGroup",
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			userType := s.graphqlTypes.user
			reactorUnion := graphql.NewUnion(graphql.UnionConfig{
				Name:  "Reactor",
				Types: []*graphql.Object{userType},
				ResolveType: func(graphql.ResolveTypeParams) *graphql.Object {
					return userType
				},
			})
			reactorEdge := graphql.NewObject(graphql.ObjectConfig{
				Name: "ReactorEdge",
				Fields: graphql.Fields{
					"cursor":    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
					"node":      &graphql.Field{Type: graphql.NewNonNull(reactorUnion)},
					"reactedAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
				},
			})
			reactorConn := graphql.NewObject(graphql.ObjectConfig{
				Name: "ReactorConnection",
				Fields: graphql.Fields{
					"edges":      &graphql.Field{Type: graphql.NewList(reactorEdge)},
					"nodes":      &graphql.Field{Type: graphql.NewList(reactorUnion)},
					"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
					"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
				},
			})
			reactingUserEdge := graphql.NewObject(graphql.ObjectConfig{
				Name: "ReactingUserEdge",
				Fields: graphql.Fields{
					"cursor":    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
					"node":      &graphql.Field{Type: graphql.NewNonNull(userType)},
					"reactedAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
				},
			})
			reactingUserConn := graphql.NewObject(graphql.ObjectConfig{
				Name: "ReactingUserConnection",
				Fields: graphql.Fields{
					"edges":      &graphql.Field{Type: graphql.NewList(reactingUserEdge)},
					"nodes":      &graphql.Field{Type: graphql.NewList(userType)},
					"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
					"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
				},
			})
			return graphql.Fields{
				"content":   &graphql.Field{Type: graphql.NewNonNull(reactionContentEnum)},
				"createdAt": &graphql.Field{Type: dateTime, Resolve: nilResolver},
				"viewerHasReacted": &graphql.Field{
					Type: graphql.NewNonNull(graphql.Boolean),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						m, _ := p.Source.(map[string]interface{})
						v, _ := m["viewerHasReacted"].(bool)
						return v, nil
					},
				},
				"subject": &graphql.Field{
					Type: graphql.NewNonNull(s.graphqlTypes.reactable),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						m, _ := p.Source.(map[string]interface{})
						return s.reactableSubjectSource(srcStr(m, "parentType"), srcInt(m, "parentID")), nil
					},
				},
				"reactors": &graphql.Field{
					Type: graphql.NewNonNull(reactorConn),
					Args: reactionConnectionArgs(),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						m, _ := p.Source.(map[string]interface{})
						return s.reactorConnectionFor(srcStr(m, "parentType"), srcInt(m, "parentID"), srcStr(m, "restContent"), p.Args), nil
					},
				},
				"users": &graphql.Field{
					Type: graphql.NewNonNull(reactingUserConn),
					Args: reactionConnectionArgs(),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						m, _ := p.Source.(map[string]interface{})
						return s.reactorConnectionFor(srcStr(m, "parentType"), srcInt(m, "parentID"), srcStr(m, "restContent"), p.Args), nil
					},
				},
			}
		}),
	})
	return s.graphqlTypes.reactionGroup
}

// gqlIssueCommentType returns the shared IssueComment object type (memoized).
// PR conversation comments live in the issue-comment table and serve through
// this one type, as on GitHub. Resolvers read commentToGQLLocked's source maps.
func (s *Resolver) gqlIssueCommentType() *graphql.Object {
	if s.graphqlTypes.issueComment != nil {
		return s.graphqlTypes.issueComment
	}
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	commentAuthorAssociationEnum := s.graphQLEnum(
		"CommentAuthorAssociation",
		"COLLABORATOR", "CONTRIBUTOR", "FIRST_TIMER", "FIRST_TIME_CONTRIBUTOR",
		"MANNEQUIN", "MEMBER", "NONE", "OWNER",
	)
	s.graphqlTypes.issueComment = graphql.NewObject(graphql.ObjectConfig{
		Name:       "IssueComment",
		Interfaces: []*graphql.Interface{s.gqlMinimizableInterface(), s.graphqlTypes.reactable},
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
			// Defaults: bleephub models no edit history or moderation.
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
			"viewerCanMinimize": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, _ := p.Source.(map[string]interface{})
					can, _ := s.commentMinimizePerms(p, c)
					return can, nil
				},
			},
			"viewerCanUnminimize": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, _ := p.Source.(map[string]interface{})
					_, can := s.commentMinimizePerms(p, c)
					return can, nil
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
				Type: graphql.NewList(graphql.NewNonNull(s.gqlReactionGroupType())),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return c["reactionGroups"], nil
				},
			},
			"viewerDidAuthor": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					c, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					viewer := s.ghUserFromContext(p.Context)
					authorID, _ := c["authorID"].(int)
					return viewer != nil && authorID == viewer.ID, nil
				},
			},
		},
	})
	s.addReactableFields(s.graphqlTypes.issueComment, "issue_comment")
	return s.graphqlTypes.issueComment
}

// gqlIssueCommentConnectionType returns the shared IssueCommentConnection type
// (memoized).
func (s *Resolver) gqlIssueCommentConnectionType() *graphql.Object {
	if s.graphqlTypes.issueCommentConnection != nil {
		return s.graphqlTypes.issueCommentConnection
	}
	s.graphqlTypes.issueCommentConnection = graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueCommentConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(s.gqlIssueCommentType())},
			"edges":      &graphql.Field{Type: graphql.NewList(s.gqlIssueCommentEdgeType())},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})
	return s.graphqlTypes.issueCommentConnection
}

// gqlAgentAssignmentInput is GitHub's AgentAssignmentInput (memoized); the
// agent-triage mutations and CreateIssueInput.agentAssignment name one type.
func (s *Resolver) gqlAgentAssignmentInput() *graphql.InputObject {
	return s.mutationInput("AgentAssignmentInput", graphql.InputObjectConfigFieldMap{
		"baseRef":            gqlString(),
		"customAgent":        gqlString(),
		"customInstructions": gqlString(),
		"targetRepositoryId": gqlID(),
	})
}

// gqlIssueFieldCreateOrUpdateInput is GitHub's IssueFieldCreateOrUpdateInput
// (memoized); the issue-field mutations and CreateIssueInput.issueFields name
// one type.
func (s *Resolver) gqlIssueFieldCreateOrUpdateInput() *graphql.InputObject {
	return s.mutationInput("IssueFieldCreateOrUpdateInput", graphql.InputObjectConfigFieldMap{
		"fieldId":              gqlNonNullID(),
		"textValue":            gqlString(),
		"dateValue":            gqlString(),
		"numberValue":          gqlInputOf(graphql.Float),
		"singleSelectOptionId": gqlID(),
		"multiSelectOptionIds": gqlListOf(graphql.ID),
		"delete":               gqlBool(),
		"suggest":              gqlBool(),
		"rationale":            gqlString(),
		"confidence":           gqlInputOf(s.sharedEnum("IssueEventConfidenceLevel", "HIGH", "LOW", "MEDIUM")),
	})
}

// gqlIssueCommentEdgeType returns the shared IssueCommentEdge (memoized), used
// by IssueCommentConnection.edges and AddCommentPayload.commentEdge.
func (s *Resolver) gqlIssueCommentEdgeType() *graphql.Object {
	return s.mutationObject("IssueCommentEdge", graphql.Fields{
		"node":   &graphql.Field{Type: s.gqlIssueCommentType()},
		"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	})
}

// gqlIssueConnectionType returns the shared IssueConnection type (memoized).
// Used by Repository.issues and PullRequest.closingIssuesReferences.
func (s *Resolver) gqlIssueConnectionType(issueType *graphql.Object) *graphql.Object {
	if s.graphqlTypes.issueConnection != nil {
		return s.graphqlTypes.issueConnection
	}
	issueEdgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueEdge",
		Fields: graphql.Fields{
			"node":   &graphql.Field{Type: issueType},
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	s.graphqlTypes.issueConnection = graphql.NewObject(graphql.ObjectConfig{
		Name: "IssueConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(issueType)},
			"edges":      &graphql.Field{Type: graphql.NewList(issueEdgeType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})
	return s.graphqlTypes.issueConnection
}

func (s *Resolver) graphQLEnum(name string, values ...string) *graphql.Enum {
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

func (s *Resolver) gqlIssueOrderType(fieldType, directionType graphql.Input) *graphql.InputObject {
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

// pinnedIssueNodeID renders the PinnedIssue global id. The pin row has no store
// identity of its own, so the issue's database id is the discriminator.
func pinnedIssueNodeID(issueID int) string {
	return fmt.Sprintf("PI_kgDO%08d", issueID)
}

func relayConnectionArgs() graphql.FieldConfigArgument {
	return graphql.FieldConfigArgument{
		"first":  &graphql.ArgumentConfig{Type: graphql.Int},
		"after":  &graphql.ArgumentConfig{Type: graphql.String},
		"last":   &graphql.ArgumentConfig{Type: graphql.Int},
		"before": &graphql.ArgumentConfig{Type: graphql.String},
	}
}

func issueFieldValuesConnectionLocked(st *store.Store, issue *store.Issue) map[string]interface{} {
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
	// Return the full node set; the field resolver windows it via
	// repaginateConnection. Pre-paginating here clamped totalCount to 100 and hid
	// the remainder (GQL-022).
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

func issueFieldsOrgLocked(st *store.Store, repo *store.Repo) string {
	orgLogin, _, _ := strings.Cut(repo.FullName, "/")
	if st.OrgsByLogin[orgLogin] == nil {
		return ""
	}
	return orgLogin
}

func issueFieldValueToGQLLocked(field *store.IssueField, issueID int, value interface{}) map[string]interface{} {
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
		names := store.ToStringSlice(value)
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

func issueFieldToGQLLocked(field *store.IssueField) map[string]interface{} {
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

func issueFieldOptionsToGQL(options []*store.IssueFieldOption) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(options))
	for _, opt := range options {
		out = append(out, issueFieldOptionToGQL(opt))
	}
	return out
}

func issueFieldOptionToGQL(opt *store.IssueFieldOption) map[string]interface{} {
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

func issueFieldOptionByName(field *store.IssueField, name string) *store.IssueFieldOption {
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

func labelToGQL(l *store.IssueLabel) map[string]interface{} {
	return map[string]interface{}{
		"nodeID":      l.NodeID,
		"_dbID":       l.ID,
		"repoID":      l.RepoID,
		"name":        l.Name,
		"description": l.Description,
		"color":       l.Color,
		"isDefault":   l.Default,
		"createdAt":   l.CreatedAt.Format(time.RFC3339),
	}
}

func milestoneToGQL(ms *store.Milestone) map[string]interface{} {
	var dueOn interface{}
	if ms.DueOn != nil {
		dueOn = ms.DueOn.Format(time.RFC3339)
	}
	var closedAt interface{}
	if ms.ClosedAt != nil {
		closedAt = ms.ClosedAt.Format(time.RFC3339)
	}
	return map[string]interface{}{
		"nodeID":      ms.NodeID,
		"_dbID":       ms.ID,
		"repoID":      ms.RepoID,
		"creatorID":   ms.CreatorID,
		"number":      ms.Number,
		"title":       ms.Title,
		"description": ms.Description,
		"state":       strings.ToUpper(string(ms.State)),
		"dueOn":       dueOn,
		"closedAt":    closedAt,
		"createdAt":   ms.CreatedAt.Format(time.RFC3339),
		"updatedAt":   ms.UpdatedAt.Format(time.RFC3339),
	}
}

func commentToGQL(c *store.Comment, st *store.Store) map[string]interface{} {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return commentToGQLLocked(c, st)
}

func commentToGQLLocked(c *store.Comment, st *store.Store) map[string]interface{} {
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
		"author":              optionalRendered(st.Users[c.AuthorID], userToGraphQL),
		"authorAssociation":   commentAuthorAssociationLocked(c, st),
		"includesCreatedEdit": c.LastEditedAt != nil,
		"lastEditedAt":        lastEditedAt,
		"editor":              optionalObject(editor),
		"isMinimized":         c.MinimizedReason != "",
		"isPinned":            c.Pinned,
		"minimizedReason":     nilStr(c.MinimizedReason),
		"reactionGroups":      reactionGroupsForGraphQL(st.Reactions, "issue_comment", c.ID, 0),
		"repoID":              commentRepoIDLocked(c, st),
	}
}

// commentMinimizePerms reports whether the viewer may minimize/unminimize a
// comment: a maintainer may do both; an author may additionally hide their own.
func (s *Resolver) commentMinimizePerms(p graphql.ResolveParams, src map[string]interface{}) (canMinimize, canUnminimize bool) {
	viewer := s.ghUserFromContext(p.Context)
	if viewer == nil || src == nil {
		return false, false
	}
	repoID, _ := src["repoID"].(int)
	repo := s.store.GetRepoByID(repoID)
	canPush := repo != nil && s.viewerCanPushRepo(p.Context, repo)
	authorID, _ := src["authorID"].(int)
	return canPush || viewer.ID == authorID, canPush
}

// commentRepoIDLocked resolves a comment's repository through its parent issue
// or pull request.
func commentRepoIDLocked(comment *store.Comment, st *store.Store) int {
	switch comment.ParentType {
	case "pull_request":
		if pull := st.PullRequests[comment.IssueID]; pull != nil {
			return pull.RepoID
		}
	default:
		if issue := st.Issues[comment.IssueID]; issue != nil {
			return issue.RepoID
		}
	}
	return 0
}

func commentAuthorAssociationLocked(comment *store.Comment, st *store.Store) string {
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

func commentURLLocked(comment *store.Comment, st *store.Store) string {
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

func authorAssociationForRepoLocked(st *store.Store, repoID, authorID int) string {
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
		if membership := st.Memberships[store.MembershipKey(owner, authorID)]; membership != nil && membership.State == "active" {
			return "MEMBER"
		}
	}
	if _, ok := st.RepoCollaborators[repo.FullName][author.Login]; ok {
		return "COLLABORATOR"
	}
	return "NONE"
}

// nilStr returns nil for an empty string, else the string.
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

// reactionGroupsForGraphQL returns a GraphQL-shaped `[ReactionGroup]` for a
// parent, with per-content totalCount from the ReactionStore.
func reactionGroupsForGraphQL(rs *store.ReactionStore, parentType string, parentID int, viewerID int) []map[string]interface{} {
	counts := map[string]int{
		"+1": 0, "-1": 0, "laugh": 0, "confused": 0,
		"heart": 0, "hooray": 0, "rocket": 0, "eyes": 0,
	}
	viewerReacted := map[string]bool{}
	if rs != nil && parentID != 0 {
		for _, r := range rs.ListReactions(parentType, parentID, "") {
			counts[r.Content]++
			if viewerID != 0 && r.UserID == viewerID {
				viewerReacted[r.Content] = true
			}
		}
	}
	// Order matches GitHub's GraphQL response.
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
			"content":          m.gql,
			"restContent":      m.rest,
			"parentType":       parentType,
			"parentID":         parentID,
			"users":            map[string]interface{}{"totalCount": counts[m.rest]},
			"viewerHasReacted": viewerReacted[m.rest],
		})
	}
	return out
}

// Node ID lookup helpers

// resolveGQLLabelIDs maps a mutation's labelIds onto store ids. A nil argument
// leaves the labels alone; an id naming no label of repoID is refused, not
// dropped (dropping reported success for a label never applied).
func resolveGQLLabelIDs(st *store.Store, repoID int, raw interface{}) (*[]int, error) {
	entries, ok := raw.([]interface{})
	if !ok {
		return nil, nil
	}
	ids := make([]int, 0, len(entries))
	for _, entry := range entries {
		nodeID := fmt.Sprintf("%v", entry)
		l := store.FindLabelByNodeID(st, nodeID)
		if l == nil || l.RepoID != repoID {
			return nil, gqlMissingNode("Label", nodeID)
		}
		if l.Archived {
			return nil, fmt.Errorf("label %q is archived", l.Name)
		}
		ids = append(ids, l.ID)
	}
	return &ids, nil
}

// resolveGQLAssigneeIDs is resolveGQLLabelIDs for assigneeIds.
func resolveGQLAssigneeIDs(st *store.Store, raw interface{}) (*[]int, error) {
	entries, ok := raw.([]interface{})
	if !ok {
		return nil, nil
	}
	ids := make([]int, 0, len(entries))
	for _, entry := range entries {
		nodeID := fmt.Sprintf("%v", entry)
		u := store.FindUserByNodeID(st, nodeID)
		if u == nil {
			return nil, gqlMissingNode("User", nodeID)
		}
		ids = append(ids, u.ID)
	}
	return &ids, nil
}

// resolveGQLMilestoneID maps a mutation's milestone argument onto a store id.
// Absent leaves the milestone alone; explicit null clears it.
func resolveGQLMilestoneID(st *store.Store, repoID int, input map[string]interface{}, key string) (*int, error) {
	raw, present := input[key]
	if !present {
		return nil, nil
	}
	nodeID, ok := raw.(string)
	if !ok || nodeID == "" {
		cleared := 0
		return &cleared, nil
	}
	ms := store.FindMilestoneByNodeID(st, nodeID)
	if ms == nil || ms.RepoID != repoID {
		return nil, gqlMissingNode("Milestone", nodeID)
	}
	id := ms.ID
	return &id, nil
}

// applyIssueState moves an issue between OPEN and CLOSED, keeping ClosedAt and
// StateReason consistent, as the REST handler does.
func applyIssueState(i *store.Issue, state string) {
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

// paginateIssuesGQL implements Relay-style cursor pagination for issues.
func paginateIssuesGQL(issues []*store.Issue, st *store.Store, first int, after string) map[string]interface{} {
	return paginateGQL(issues, first, after, func(i *store.Issue) map[string]interface{} {
		return issueToGQL(i, st)
	}, func(i *store.Issue) string { return i.NodeID })
}

// graphQLTypeRegistry memoizes every shared GraphQL type: graphql-go refuses a
// schema holding two types of one name, so each is built once and reused.
type graphQLTypeRegistry struct {
	issueType            *graphql.Object
	issueFieldsUnion     *graphql.Union
	issueFieldValueUnion *graphql.Union

	pageInfo                         *graphql.Object
	scalars                          map[string]*graphql.Scalar
	enums                            map[string]*graphql.Enum
	actor                            *graphql.Interface
	repositoryOwner                  *graphql.Interface
	issueOrder                       *graphql.InputObject
	labelType                        *graphql.Object
	labelConnection                  *graphql.Object
	userConnection                   *graphql.Object
	issueConnection                  *graphql.Object
	issueComment                     *graphql.Object
	issueCommentConnection           *graphql.Object
	reactionGroup                    *graphql.Object
	reaction                         *graphql.Object
	reactionConnection               *graphql.Object
	reactable                        *graphql.Interface
	userContentEdit                  *graphql.Object
	userContentEditConnection        *graphql.Object
	issueEventRationale              *graphql.Object
	repositoryTopicConnection        *graphql.Object
	assigneeConnection               *graphql.Object
	assignee                         *graphql.Union
	hovercard                        *graphql.Object
	pullRequestReview                *graphql.Object
	pullRequestReviewComment         *graphql.Object
	commitComment                    *graphql.Object
	release                          *graphql.Object
	ref                              *graphql.Object
	license                          *graphql.Object
	organization                     *graphql.Object
	lockable                         *graphql.Interface
	minimizable                      *graphql.Interface
	votable                          *graphql.Interface
	issue                            *graphql.Object
	pullRequest                      *graphql.Object
	discussion                       *graphql.Object
	discussionComment                *graphql.Object
	projectV2Type                    *graphql.Object
	projectV2FieldTypeMemo           *graphql.Object
	projectV2SingleSelectFieldMemo   *graphql.Object
	projectV2IterationFieldMemo      *graphql.Object
	projectV2FieldConfigUnionMemo    *graphql.Union
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
	// The Projects v2 read surface (gh_projects_v2_types_graphql.go).
	projectV2OwnerInterface   *graphql.Interface
	projectV2StatusUpdateType *graphql.Object
	projectV2WorkflowType     *graphql.Object
	projectV2DraftIssueType   *graphql.Object
	projectV2OrderInput       *graphql.InputObject
	projectV2OptionInput      *graphql.InputObject
	projectV2MultiOptionInput *graphql.InputObject
	// Query.resource's return interface and the node types it dispatches to.
	projectV2OrderInputs     map[string]*graphql.InputObject
	uniformResourceLocatable *graphql.Interface
	resourceNodeTypes        map[string]*graphql.Object
	// Shared types the Projects v2 field values name (LABELS, MILESTONE, ...).
	milestone              *graphql.Object
	pullRequestConnection  *graphql.Object
	requestedReviewerUnion *graphql.Union
	// The ProjectV2ItemFieldValue union's remaining members and shared interface.
	projectV2ValueCommonInterface *graphql.Interface
	projectV2MultiSelectValueMemo *graphql.Object
	// The enterprise account family (gh_enterprise_graphql.go).
	organizationConnection        *graphql.Object
	enterprise                    *graphql.Object
	enterpriseUserAccount         *graphql.Object
	enterpriseAdminInvitation     *graphql.Object
	enterpriseMemberInvite        *graphql.Object
	enterpriseIdentityProvide     *graphql.Object
	ipAllowListEntry              *graphql.Object
	ipAllowListOwner              *graphql.Union
	projectV2LabelValueMemo       *graphql.Object
	projectV2MilestoneValueMemo   *graphql.Object
	projectV2RepositoryValueMemo  *graphql.Object
	projectV2UserValueMemo        *graphql.Object
	projectV2ReviewerValueMemo    *graphql.Object
	projectV2PullRequestValueMemo *graphql.Object
	projectV2MultiSelectOption    *graphql.Object
	projectV2MultiSelectFieldMemo *graphql.Object
	projectV2Connections          map[string]*graphql.Object
	// The shared RepositoryConnection and Team objects.
	repositoryConnection *graphql.Object
	team                 *graphql.Object
	// The Copilot endpoints object (gh_copilot_graphql.go).
	copilotEndpoints *graphql.Object

	// The GitHub Marketplace type graph (gh_marketplace_graphql.go).
	marketplace *marketplaceTypeRegistry

	// The GitHub Sponsors type graph (gh_sponsors_graphql.go).
	sponsors *sponsorsTypeRegistry

	node              *graphql.Interface
	user              *graphql.Object
	repository        *graphql.Object
	gitObject         *graphql.Interface
	gitObjectTypes    map[string]*graphql.Object
	gitActor          *graphql.Object
	language          *graphql.Object
	commit            *graphql.Object
	commitEdge        *graphql.Object
	commitConnections map[string]*graphql.Object
	tree              *graphql.Object
	treeEntry         *graphql.Object
	blob              *graphql.Object
	tag               *graphql.Object
	refConnection     *graphql.Object
	// BranchProtectionRule (gh_branch_protection_graphql.go).
	branchProtectionRule *graphql.Object
	// The check-rollup members and the RequirableByPullRequest interface.
	statusContext           *graphql.Object
	checkRun                *graphql.Object
	checkSuite              *graphql.Object
	statusCheckRollup       *graphql.Object
	requirableByPullRequest *graphql.Interface
	// The Actions run graph: WorkflowRun and Workflow. Minted as stubs where the
	// CheckSuite rollup references them, finished by addActionsFamilyFields.
	workflowRun *graphql.Object
	workflow    *graphql.Object
	// The deployment graph (gh_repos_deployments_graphql.go).
	deployment        *graphql.Object
	deploymentStatus  *graphql.Object
	environment       *graphql.Object
	pinnedEnvironment *graphql.Object
	// EnvironmentConnection and DeploymentReviewerConnection, shared with the
	// Actions run graph's DeploymentReview/DeploymentRequest.
	environmentConnection        *graphql.Object
	deploymentReviewerConnection *graphql.Object
	// Labelable, the interface the label mutations return their subject behind.
	labelable      *graphql.Interface
	gist           *graphql.Object
	gistFile       *graphql.Object
	gistEdge       *graphql.Object
	gistConnection *graphql.Object
	// The issue/pull-request timeline family (gh_timeline_types_graphql.go),
	// plus three shared types its unions name.
	timeline                *timelineTypeRegistry
	pullRequestCommit       *graphql.Object
	pullRequestReviewThread *graphql.Object
	issueOrPullRequest      *graphql.Union
	// The Repository/User/Organization account-surface family
	// (gh_account_surface_graphql.go), with its own registry.
	accountSurface *accountSurfaceTypes
}

func (s *Resolver) projectV2GraphQLTypes() *graphql.Object {
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
		Args: orderedConnectionArgs(s.projectV2FieldOrderInput()),
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
			projectV2SortNodes(nodes, p.Args, map[string]string{"NAME": "name", "CREATED_AT": "createdAt"})
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
	s.graphqlTypes.projectV2Type.AddFieldConfig("views", &graphql.Field{
		Type: graphql.NewNonNull(s.projectV2ViewConnectionType()),
		Args: orderedConnectionArgs(s.projectV2ViewOrderInput()),
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
			projectV2SortNodes(nodes, p.Args, map[string]string{"NAME": "name", "CREATED_AT": "createdAt"})
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
	return s.graphqlTypes.projectV2Type
}

func (s *Resolver) ensureProjectV2ItemsField() {
	if s.graphqlTypes.projectV2Type == nil || s.graphqlTypes.projectV2ItemConnectionTypeMemo == nil || s.graphqlTypes.projectV2ItemsFieldAdded {
		return
	}
	s.graphqlTypes.projectV2Type.AddFieldConfig("items", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.projectV2ItemConnectionTypeMemo),
		Args: s.projectV2ItemConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			projectID, err := projectV2SourceID(p.Source)
			if err != nil {
				return nil, err
			}
			items := projectV2ApplyItemFilters(s.store, s.store.ProjectsV2.ListItemsForProject(projectID), p.Args)
			nodes := make([]map[string]interface{}, 0, len(items))
			for _, it := range items {
				// A readable project can hold items whose content lives in a
				// private repo; don't leak that issue/PR to a viewer who cannot
				// read the repo (mirrors the REST projectV2ItemContentJSON gate).
				redact := it.ContentType != "DraftIssue" &&
					!s.viewerCanReadProjectContent(p.Context, it.ContentType, it.ContentID)
				nodes = append(nodes, projectV2ItemNode(it, s.store, redact))
			}
			projectV2SortNodes(nodes, p.Args, nil)
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
	s.graphqlTypes.projectV2ItemsFieldAdded = true
}

// projectV2FieldConnectionType builds the typed field-configuration surface: the
// ProjectV2FieldCommon interface, its concrete types, the
// ProjectV2FieldConfiguration union and its connection. Source maps come from
// projectV2FieldToGQL, whose "dataType" key drives ResolveType.
func (s *Resolver) projectV2FieldConnectionType() *graphql.Object {
	if s.graphqlTypes.projectV2FieldConnectionMemo != nil {
		return s.graphqlTypes.projectV2FieldConnectionMemo
	}
	dateTime := s.graphQLStringScalar("DateTime")
	date := s.graphQLStringScalar("Date")
	fieldTypeEnum := s.graphQLEnum(
		"ProjectV2FieldType",
		"ASSIGNEES", "CLOSED", "CREATED", "DATE", "ISSUE_TYPE", "ITERATION",
		"LABELS", "LINKED_PULL_REQUESTS", "MILESTONE", "MULTI_SELECT", "NUMBER",
		"PARENT_ISSUE", "REPOSITORY", "REVIEWERS", "SINGLE_SELECT",
		"SUB_ISSUES_PROGRESS", "TEXT", "TITLE", "TRACKED_BY", "TRACKS", "UPDATED",
	)
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
	// Official ProjectV2Iteration is an INPUT_OBJECT; the object flavor is this.
	iterationType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2IterationFieldIteration",
		Fields: graphql.Fields{
			"id":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"title":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"startDate": &graphql.Field{Type: graphql.NewNonNull(date)},
			"duration":  &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
	iterationConfigurationType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2IterationFieldConfiguration",
		Fields: graphql.Fields{
			"duration":            &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"startDay":            &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"iterations":          &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(iterationType)))},
			"completedIterations": &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(iterationType)))},
		},
	})
	resolveFieldConfigType := func(value interface{}) *graphql.Object {
		src, _ := value.(map[string]interface{})
		switch src["dataType"] {
		case string(store.ProjectV2FieldSingleSelect):
			return s.graphqlTypes.projectV2SingleSelectFieldMemo
		case string(store.ProjectV2FieldMultiSelect):
			return s.graphqlTypes.projectV2MultiSelectFieldMemo
		case string(store.ProjectV2FieldIteration):
			return s.graphqlTypes.projectV2IterationFieldMemo
		default:
			return s.graphqlTypes.projectV2FieldTypeMemo
		}
	}
	fieldCommonInterface := graphql.NewInterface(graphql.InterfaceConfig{
		Name: "ProjectV2FieldCommon",
		Fields: graphql.Fields{
			"id":        &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"name":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"dataType":  &graphql.Field{Type: graphql.NewNonNull(fieldTypeEnum)},
			"createdAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
		},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			return resolveFieldConfigType(p.Value)
		},
	})
	commonFields := func() graphql.Fields {
		return graphql.Fields{
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
			"name":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"dataType":  &graphql.Field{Type: graphql.NewNonNull(fieldTypeEnum)},
			"createdAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
		}
	}
	s.graphqlTypes.projectV2FieldTypeMemo = graphql.NewObject(graphql.ObjectConfig{
		Name:       "ProjectV2Field",
		Interfaces: []*graphql.Interface{fieldCommonInterface},
		Fields:     commonFields(),
	})
	singleSelectFields := commonFields()
	singleSelectFields["options"] = &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(optionType))),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			return src["options"], nil
		},
	}
	s.graphqlTypes.projectV2SingleSelectFieldMemo = graphql.NewObject(graphql.ObjectConfig{
		Name:       "ProjectV2SingleSelectField",
		Interfaces: []*graphql.Interface{fieldCommonInterface},
		Fields:     singleSelectFields,
	})
	multiSelectFields := commonFields()
	multiSelectFields["multiSelectOptions"] = &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(s.projectV2MultiSelectOptionType()))),
		Args: graphql.FieldConfigArgument{
			"names": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			options, _ := src["options"].([]map[string]interface{})
			names, _ := p.Args["names"].([]interface{})
			if len(names) == 0 {
				return options, nil
			}
			wanted := make(map[string]bool, len(names))
			for _, name := range names {
				if value, ok := name.(string); ok {
					wanted[value] = true
				}
			}
			filtered := make([]map[string]interface{}, 0, len(options))
			for _, option := range options {
				if name, _ := option["name"].(string); wanted[name] {
					filtered = append(filtered, option)
				}
			}
			return filtered, nil
		},
	}
	s.graphqlTypes.projectV2MultiSelectFieldMemo = graphql.NewObject(graphql.ObjectConfig{
		Name:       "ProjectV2MultiSelectField",
		Interfaces: []*graphql.Interface{fieldCommonInterface},
		Fields:     multiSelectFields,
	})
	iterationFields := commonFields()
	iterationFields["configuration"] = &graphql.Field{
		Type: graphql.NewNonNull(iterationConfigurationType),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			return src["configuration"], nil
		},
	}
	s.graphqlTypes.projectV2IterationFieldMemo = graphql.NewObject(graphql.ObjectConfig{
		Name:       "ProjectV2IterationField",
		Interfaces: []*graphql.Interface{fieldCommonInterface},
		Fields:     iterationFields,
	})
	s.graphqlTypes.projectV2FieldConfigUnionMemo = graphql.NewUnion(graphql.UnionConfig{
		Name: "ProjectV2FieldConfiguration",
		Types: []*graphql.Object{
			s.graphqlTypes.projectV2FieldTypeMemo,
			s.graphqlTypes.projectV2IterationFieldMemo,
			s.graphqlTypes.projectV2MultiSelectFieldMemo,
			s.graphqlTypes.projectV2SingleSelectFieldMemo,
		},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			return resolveFieldConfigType(p.Value)
		},
	})
	s.graphqlTypes.projectV2FieldConnectionMemo = graphql.NewObject(graphql.ObjectConfig{
		Name: "ProjectV2FieldConfigurationConnection",
		Fields: graphql.Fields{
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"nodes":      &graphql.Field{Type: graphql.NewList(s.graphqlTypes.projectV2FieldConfigUnionMemo)},
			"edges":      &graphql.Field{Type: graphql.NewList(graphql.NewObject(graphql.ObjectConfig{Name: "ProjectV2FieldConfigurationEdge", Fields: graphql.Fields{"node": &graphql.Field{Type: s.graphqlTypes.projectV2FieldConfigUnionMemo}, "cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)}}}))},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})
	return s.graphqlTypes.projectV2FieldConnectionMemo
}

// projectV2FieldConfigurationUnion returns the union, building the field wiring
// that owns the memo on first use.
func (s *Resolver) projectV2FieldConfigurationUnion() *graphql.Union {
	s.projectV2FieldConnectionType()
	return s.graphqlTypes.projectV2FieldConfigUnionMemo
}

func (s *Resolver) projectV2ViewConnectionType() *graphql.Object {
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
			// visibleFieldIds feeds this resolver via the source map; it is not
			// schema-visible.
			"fields": &graphql.Field{
				Type: s.projectV2FieldConnectionType(),
				Args: relayConnectionArgs(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					fieldIDs, _ := src["visibleFieldIds"].([]int)
					nodes := make([]map[string]interface{}, 0, len(fieldIDs))
					for _, fieldID := range fieldIDs {
						if f := s.store.ProjectsV2.GetField(fieldID); f != nil {
							nodes = append(nodes, projectV2FieldToGQL(f))
						}
					}
					return paginateGQLMaps(nodes, p.Args), nil
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

// projectV2ItemType returns the shared ProjectV2Item object type, building the
// connection wiring that owns the memo on first use.
func (s *Resolver) projectV2ItemType() *graphql.Object {
	s.projectV2ItemConnectionType()
	return s.graphqlTypes.projectV2ItemTypeMemo
}

func (s *Resolver) projectV2ItemConnectionType() *graphql.Object {
	if s.graphqlTypes.projectV2ItemConnectionTypeMemo != nil {
		return s.graphqlTypes.projectV2ItemConnectionTypeMemo
	}
	projectV2Type := s.projectV2GraphQLTypes()
	// The whole ProjectV2ItemFieldValue union — twelve members plus the
	// ProjectV2ItemFieldValueCommon interface — is built in one pass, because
	// graphql-go fixes a union's members and an object's interfaces when the
	// type is created.
	s.projectV2FieldValueTypes()
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

// projectV2ItemToGQL builds a project item's source map, embedding the parent
// project's map and pre-resolving field values into fieldValuesByName.
func projectV2ItemToGQL(it *store.ProjectV2Item, st *store.Store) map[string]interface{} {
	return projectV2ItemNode(it, st, false)
}

// projectV2ItemNode renders a project item's source map. When redactContent is
// true the item's content (the issue/PR) and every content-derived built-in
// field value are hidden — for a viewer who may read the project but not the
// private repository the content lives in. Only stored (custom) field values,
// which are project metadata, survive. Mirrors the REST gate in
// projectV2ItemContentJSON (gh_projects_v2_rest.go).
func projectV2ItemNode(it *store.ProjectV2Item, st *store.Store, redactContent bool) map[string]interface{} {
	if it == nil {
		return nil
	}
	var projectMap map[string]interface{}
	if p := st.ProjectsV2.GetProject(it.ProjectID); p != nil {
		projectMap = projectV2ToGQL(st, p)
	}
	byName := map[string]interface{}{}
	// Consider every field, not only those with a stored value: the built-in
	// columns (labels, assignees, repository, milestone) are read off the
	// content. Ordered by field id for a connection stable across requests.
	values := make([]map[string]interface{}, 0, len(it.FieldValues))
	for _, field := range st.ProjectsV2.FieldsForProject(it.ProjectID) {
		var rendered map[string]interface{}
		if stored := it.FieldValues[field.ID]; stored != nil {
			rendered = projectV2FieldValueToGQL(stored, field)
			rendered["field"] = projectV2FieldToGQL(field)
		} else if redactContent {
			// A content-derived built-in value would leak the private issue/PR.
			continue
		} else {
			rendered = projectV2BuiltInFieldValue(st, it, field)
		}
		if rendered == nil {
			continue
		}
		rendered["itemNodeID"] = it.NodeID
		// A field value has no row of its own; its identity is (item, field).
		rendered["valueNodeID"] = fmt.Sprintf("PVTFV_kgDO%08d%08d", it.ID, field.ID)
		rendered["databaseId"] = field.ID
		rendered["createdAt"] = it.CreatedAt.UTC().Format(time.RFC3339)
		rendered["updatedAt"] = it.UpdatedAt.UTC().Format(time.RFC3339)
		if creator := st.GetUserByID(it.CreatorID); creator != nil {
			rendered["creator"] = userToGraphQL(creator)
		}
		byName[field.Name] = rendered
		values = append(values, rendered)
	}
	out := map[string]interface{}{
		"nodeID":            it.NodeID,
		"id":                it.ID,
		"databaseId":        it.ID,
		"fullDatabaseId":    it.ID,
		"project":           optionalObject(projectMap),
		"fieldValuesByName": byName,
		"fieldValues":       values,
		"type":              projectV2ItemTypeEnum(it.ContentType),
		"isArchived":        it.ArchivedAt != nil,
		"createdAt":         it.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":         it.UpdatedAt.UTC().Format(time.RFC3339),
		"content":           redactableItemContent(st, it, redactContent),
	}
	out["creator"] = optionalRendered(st.GetUserByID(it.CreatorID), userToGraphQL)
	return out
}

func projectV2ItemTypeEnum(contentType string) string {
	switch contentType {
	case "Issue":
		return "ISSUE"
	case "PullRequest":
		return "PULL_REQUEST"
	default:
		return "DRAFT_ISSUE"
	}
}

// redactableItemContent renders the item's content, or null when redactContent
// is set — the viewer may read the project but not the private repo the content
// lives in, so the issue/PR is hidden.
func redactableItemContent(st *store.Store, it *store.ProjectV2Item, redactContent bool) interface{} {
	if redactContent {
		return nil
	}
	return optionalObject(projectV2ItemContentToGQL(st, it))
}

// projectV2ItemContentToGQL renders the issue, PR or draft issue an item points
// at. Content that no longer exists resolves to null (the field is nullable).
func projectV2ItemContentToGQL(st *store.Store, it *store.ProjectV2Item) map[string]interface{} {
	switch it.ContentType {
	case "Issue":
		issue := st.GetIssue(it.ContentID)
		if issue == nil {
			return nil
		}
		content := issueToGQL(issue, st)
		content["__typename"] = "Issue"
		return content
	case "PullRequest":
		pr := st.GetPullRequest(it.ContentID)
		if pr == nil {
			return nil
		}
		content := pullRequestToGQL(pr, st)
		content["__typename"] = "PullRequest"
		return content
	default:
		content := map[string]interface{}{
			"__typename": "DraftIssue",
			// A draft has no row of its own; the item's node id identifies it.
			"nodeID":    it.NodeID,
			"title":     it.DraftTitle,
			"body":      it.DraftBody,
			"bodyText":  it.DraftBody,
			"bodyHTML":  it.DraftBody,
			"createdAt": it.CreatedAt.UTC().Format(time.RFC3339),
			"updatedAt": it.UpdatedAt.UTC().Format(time.RFC3339),
		}
		if creator := st.GetUserByID(it.CreatorID); creator != nil {
			content["creator"] = userToGraphQL(creator)
		}
		return content
	}
}

// projectV2FieldValueToGQL renders a persisted field value as its union source map.
func projectV2FieldValueToGQL(v *store.ProjectV2ItemFieldValue, f *store.ProjectV2Field) map[string]interface{} {
	out := map[string]interface{}{"kind": string(f.DataType)}
	switch f.DataType {
	case store.ProjectV2FieldText:
		out["text"] = v.TextValue
	case store.ProjectV2FieldNumber:
		out["number"] = v.NumberValue
	case store.ProjectV2FieldDate:
		out["date"] = v.DateValue
	case store.ProjectV2FieldIteration:
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
	case store.ProjectV2FieldMultiSelect:
		selections := make([]map[string]interface{}, 0, len(v.OptionIDs))
		for i, id := range v.OptionIDs {
			name := ""
			if i < len(v.OptionNames) {
				name = v.OptionNames[i]
			}
			option := map[string]interface{}{"id": id, "name": name, "description": "", "color": "GRAY"}
			// Colour and description live on the field's option, not the value.
			for _, defined := range f.Options {
				if defined.ID == id {
					option["name"] = defined.Name
					option["description"] = defined.Description
					option["color"] = defined.Color
					break
				}
			}
			selections = append(selections, option)
		}
		out["options"] = selections
		out["value"] = strings.Join(v.OptionNames, ", ")
	default:
		out["optionId"] = v.OptionID
		out["name"] = v.OptionName
	}
	return out
}

// projectV2ToGQL renders a project as a GraphQL source map. No live *Store is
// embedded; resolvers reach it through their closure. Built once here so a
// project answers the same fields however it was reached.
func projectV2ToGQL(st *store.Store, p *store.ProjectV2) map[string]interface{} {
	return projectV2ToGQLFull(st, p)
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

func projectV2FieldToGQL(f *store.ProjectV2Field) map[string]interface{} {
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
		// Split past iterations into completedIterations and report the start
		// day-of-week (1=Monday … 7=Sunday), as GitHub does.
		now := time.Now()
		active := make([]map[string]interface{}, 0, len(f.Iteration.Iterations))
		completed := make([]map[string]interface{}, 0)
		for _, it := range f.Iteration.Iterations {
			node := map[string]interface{}{
				"id":        it.ID,
				"title":     it.Title,
				"startDate": it.StartDate,
				"duration":  it.Duration,
			}
			if start, err := time.Parse("2006-01-02", it.StartDate); err == nil &&
				start.AddDate(0, 0, it.Duration).Before(now) {
				completed = append(completed, node)
				continue
			}
			active = append(active, node)
		}
		startDay := 1
		if start, err := time.Parse("2006-01-02", f.Iteration.StartDate); err == nil {
			startDay = (int(start.Weekday())+6)%7 + 1
		}
		iteration = map[string]interface{}{
			"duration":            f.Iteration.Duration,
			"startDay":            startDay,
			"iterations":          active,
			"completedIterations": completed,
		}
	}
	return map[string]interface{}{
		"nodeID":        f.NodeID,
		"name":          f.Name,
		"dataType":      string(f.DataType),
		"options":       options,
		"configuration": optionalObject(iteration),
		"createdAt":     f.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":     f.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func projectV2ViewToGQL(v *store.ProjectV2View) map[string]interface{} {
	var filter interface{}
	if v.Filter != nil {
		filter = *v.Filter
	}
	sortBy := make([]map[string]interface{}, 0, len(v.SortBy))
	for _, entry := range v.SortBy {
		sortBy = append(sortBy, map[string]interface{}{
			"fieldID":   entry.FieldID,
			"direction": entry.Direction,
		})
	}
	return map[string]interface{}{
		"nodeID":                  v.NodeID,
		"projectID":               v.ProjectID,
		"databaseId":              v.ID,
		"number":                  v.Number,
		"name":                    v.Name,
		"layout":                  v.Layout,
		"filter":                  filter,
		"visibleFieldIds":         append([]int(nil), v.VisibleFields...),
		"groupByFieldIds":         append([]int(nil), v.GroupBy...),
		"verticalGroupByFieldIds": append([]int(nil), v.VerticalGroupBy...),
		"sortBy":                  sortBy,
		"createdAt":               v.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":               v.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// projectItemsConnectionForIssue returns the source map for the Issue/PullRequest
// projectItems connection.
func projectItemsConnectionForIssue(st *store.Store, issueID int, args map[string]interface{}) map[string]interface{} {
	items := st.ProjectsV2.ListItemsForIssue(issueID)
	nodes := make([]map[string]interface{}, 0, len(items))
	for _, it := range items {
		nodes = append(nodes, projectV2ItemToGQL(it, st))
	}
	return paginateGQLMaps(nodes, args)
}

// emitIssueStateChange records the timeline event and delivers the issues
// webhook for a GraphQL-driven close/reopen, only when the state transitioned.
func (s *Resolver) emitIssueStateChange(issue *store.Issue, user *store.User, previousState, action string) {
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
	s.emitWebhookEvent(repo.FullName, "issues", action, s.buildIssuesPayload(repo, issue, user, action))
}

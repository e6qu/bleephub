package graphqlapi

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/graphql-go/graphql"
)

// addDiscussionFieldsToSchema adds Discussion, DiscussionCategory,
// DiscussionComment types and their connections/mutations to the GraphQL schema.
func (s *Resolver) addDiscussionFieldsToSchema(userType, repoType, mutationType *graphql.Object) {
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	htmlScalar := s.graphQLStringScalar("HTML")

	// --- Reaction types ---
	// The shared ReactionGroup from the registry (issues, PRs, and
	// discussions all expose GitHub's one ReactionGroup type).
	discussionReactionGroupType := s.gqlReactionGroupType()

	// Reaction/ReactionConnection/ReactionEdge bear GitHub's real type names;
	// bleephub implements the subset of fields its reaction store models.
	discussionReactionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Reaction",
		Fields: graphql.Fields{
			"id": &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"content": &graphql.Field{
				Type: graphql.NewNonNull(s.graphQLEnum(
					"ReactionContent",
					"CONFUSED", "EYES", "HEART", "HOORAY", "LAUGH", "ROCKET", "THUMBS_DOWN", "THUMBS_UP",
				)),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					r, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("reaction source: unexpected type %T", p.Source)
					}
					content, _ := r["content"].(string)
					return reactionContentToGraphQL(content), nil
				},
			},
			"user": &graphql.Field{
				Type: userType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					r, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("reaction source: unexpected type %T", p.Source)
					}
					return r["user"], nil
				},
			},
		},
	})

	discussionReactionConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ReactionConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(discussionReactionType)},
			"edges":      &graphql.Field{Type: graphql.NewList(graphql.NewObject(graphql.ObjectConfig{Name: "ReactionEdge", Fields: graphql.Fields{"node": &graphql.Field{Type: discussionReactionType}, "cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)}}}))},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})

	// --- Category type ---
	discussionCategoryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DiscussionCategory",
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					cat, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("category source: unexpected type %T", p.Source)
					}
					return cat["nodeID"], nil
				},
			},
			"name":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"emoji":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"description":  &graphql.Field{Type: graphql.String},
			"isAnswerable": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"createdAt":    &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":    &graphql.Field{Type: graphql.NewNonNull(dateTime)},
		},
	})

	discussionCategoryEdgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DiscussionCategoryEdge",
		Fields: graphql.Fields{
			"node":   &graphql.Field{Type: discussionCategoryType},
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})

	discussionCategoryConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DiscussionCategoryConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(discussionCategoryType)},
			"edges":      &graphql.Field{Type: graphql.NewList(discussionCategoryEdgeType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})

	// --- Comment and discussion types (forward-declared for recursion) ---
	var discussionType *graphql.Object
	var discussionCommentType *graphql.Object
	var discussionCommentConnectionType *graphql.Object

	discussionCommentType = graphql.NewObject(graphql.ObjectConfig{
		Name:       "DiscussionComment",
		Interfaces: []*graphql.Interface{s.gqlMinimizableInterface()},
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				// Minimizable contract — bleephub doesn't model discussion
				// comment moderation, so these resolve GitHub's zero values.
				"isMinimized": &graphql.Field{
					Type: graphql.NewNonNull(graphql.Boolean),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						c, _ := p.Source.(map[string]interface{})
						minimized, _ := c["isMinimized"].(bool)
						return minimized, nil
					},
				},
				"minimizedReason": &graphql.Field{
					Type: graphql.String,
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						c, _ := p.Source.(map[string]interface{})
						return c["minimizedReason"], nil
					},
				},
				"id": &graphql.Field{
					Type: graphql.NewNonNull(graphql.ID),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						c, ok := p.Source.(map[string]interface{})
						if !ok {
							return nil, fmt.Errorf("comment source: unexpected type %T", p.Source)
						}
						return c["nodeID"], nil
					},
				},
				"discussion": &graphql.Field{
					Type: discussionType,
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						c, ok := p.Source.(map[string]interface{})
						if !ok {
							return nil, fmt.Errorf("comment source: unexpected type %T", p.Source)
						}
						discussionID, _ := c["discussionID"].(int)
						d := s.store.GetDiscussion(discussionID)
						if d == nil {
							return nil, nil
						}
						return discussionToGQL(d, s.store), nil
					},
				},
				"author": &graphql.Field{
					Type: s.graphqlTypes.actor,
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						c, ok := p.Source.(map[string]interface{})
						if !ok {
							return nil, fmt.Errorf("comment source: unexpected type %T", p.Source)
						}
						return c["author"], nil
					},
				},
				"body":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"bodyHTML":     &graphql.Field{Type: graphql.NewNonNull(htmlScalar)},
				"bodyText":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"createdAt":    &graphql.Field{Type: graphql.NewNonNull(dateTime)},
				"updatedAt":    &graphql.Field{Type: graphql.NewNonNull(dateTime)},
				"lastEditedAt": &graphql.Field{Type: dateTime},
				"isAnswer":     &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
				"reactionGroups": &graphql.Field{
					Type: graphql.NewList(graphql.NewNonNull(discussionReactionGroupType)),
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						c, ok := p.Source.(map[string]interface{})
						if !ok {
							return nil, fmt.Errorf("comment source: unexpected type %T", p.Source)
						}
						commentID, _ := c["databaseId"].(int)
						return reactionGroupsForGraphQL(s.store.Reactions, "discussion_comment", commentID), nil
					},
				},
				"reactions": &graphql.Field{
					Type: graphql.NewNonNull(discussionReactionConnectionType),
					Args: graphql.FieldConfigArgument{
						"first":  &graphql.ArgumentConfig{Type: graphql.Int},
						"last":   &graphql.ArgumentConfig{Type: graphql.Int},
						"after":  &graphql.ArgumentConfig{Type: graphql.String},
						"before": &graphql.ArgumentConfig{Type: graphql.String},
					},
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						c, ok := p.Source.(map[string]interface{})
						if !ok {
							return nil, fmt.Errorf("comment source: unexpected type %T", p.Source)
						}
						commentID, _ := c["databaseId"].(int)
						return discussionReactionConnection(s.store, "discussion_comment", commentID, p.Args), nil
					},
				},
				"replies": &graphql.Field{
					Type: graphql.NewNonNull(discussionCommentConnectionType),
					Args: graphql.FieldConfigArgument{
						"first":  &graphql.ArgumentConfig{Type: graphql.Int},
						"last":   &graphql.ArgumentConfig{Type: graphql.Int},
						"after":  &graphql.ArgumentConfig{Type: graphql.String},
						"before": &graphql.ArgumentConfig{Type: graphql.String},
					},
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						c, ok := p.Source.(map[string]interface{})
						if !ok {
							return nil, fmt.Errorf("comment source: unexpected type %T", p.Source)
						}
						discussionID, _ := c["discussionID"].(int)
						commentID, _ := c["databaseId"].(int)
						replies := s.store.ListDiscussionComments(discussionID, commentID)
						nodes := make([]map[string]interface{}, 0, len(replies))
						for _, r := range replies {
							nodes = append(nodes, discussionCommentToGQL(r, s.store))
						}
						return paginateGQLMaps(nodes, p.Args), nil
					},
				},
			}
		}),
	})

	discussionCommentEdgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DiscussionCommentEdge",
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"node":   &graphql.Field{Type: discussionCommentType},
				"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			}
		}),
	})

	discussionCommentConnectionType = graphql.NewObject(graphql.ObjectConfig{
		Name: "DiscussionCommentConnection",
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"nodes":      &graphql.Field{Type: graphql.NewList(discussionCommentType)},
				"edges":      &graphql.Field{Type: graphql.NewList(discussionCommentEdgeType)},
				"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
				"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
			}
		}),
	})

	discussionType = graphql.NewObject(graphql.ObjectConfig{
		Name:       "Discussion",
		Interfaces: []*graphql.Interface{s.gqlLockableInterface()},
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					d, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("discussion source: unexpected type %T", p.Source)
					}
					return d["nodeID"], nil
				},
			},
			"number": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"title":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"body":   &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"bodyHTML": &graphql.Field{
				Type: graphql.NewNonNull(htmlScalar),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					d, ok := p.Source.(map[string]interface{})
					if !ok {
						return "", nil
					}
					body, _ := d["body"].(string)
					return discussionBodyToHTML(body), nil
				},
			},
			"bodyText": &graphql.Field{
				Type: graphql.NewNonNull(graphql.String),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					d, ok := p.Source.(map[string]interface{})
					if !ok {
						return "", nil
					}
					body, _ := d["body"].(string)
					return discussionBodyToText(body), nil
				},
			},
			"author": &graphql.Field{
				Type: s.graphqlTypes.actor,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					d, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("discussion source: unexpected type %T", p.Source)
					}
					return d["author"], nil
				},
			},
			"category": &graphql.Field{
				Type: graphql.NewNonNull(discussionCategoryType),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					d, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("discussion source: unexpected type %T", p.Source)
					}
					return d["category"], nil
				},
			},
			"createdAt":        &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":        &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"lastEditedAt":     &graphql.Field{Type: dateTime},
			"locked":           &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"activeLockReason": &graphql.Field{Type: s.graphQLEnum("LockReason", "OFF_TOPIC", "RESOLVED", "SPAM", "TOO_HEATED")},
			"publishedAt":      &graphql.Field{Type: dateTime},
			"url":              &graphql.Field{Type: graphql.NewNonNull(uri)},
			"viewerCanDelete": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					d, ok := p.Source.(map[string]interface{})
					if !ok {
						return false, nil
					}
					viewer := s.ghUserFromContext(p.Context)
					if viewer == nil {
						return false, nil
					}
					authorID, _ := d["authorID"].(int)
					if authorID == viewer.ID {
						return true, nil
					}
					repoID, _ := d["repoID"].(int)
					repo := s.store.GetRepoByID(repoID)
					return s.viewerMayActOnRepo(p.Context, repo, scopeDiscussions, permWrite, permAdmin), nil
				},
			},
			"viewerCanUpdate": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					d, ok := p.Source.(map[string]interface{})
					if !ok {
						return false, nil
					}
					viewer := s.ghUserFromContext(p.Context)
					if viewer == nil {
						return false, nil
					}
					authorID, _ := d["authorID"].(int)
					if authorID == viewer.ID {
						return true, nil
					}
					repoID, _ := d["repoID"].(int)
					repo := s.store.GetRepoByID(repoID)
					return s.viewerMayActOnRepo(p.Context, repo, scopeDiscussions, permWrite, permAdmin), nil
				},
			},
			"viewerCanReact": &graphql.Field{
				Type: graphql.NewNonNull(graphql.Boolean),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					d, ok := p.Source.(map[string]interface{})
					if !ok {
						return false, nil
					}
					viewer := s.ghUserFromContext(p.Context)
					if viewer == nil {
						return false, nil
					}
					repoID, _ := d["repoID"].(int)
					repo := s.store.GetRepoByID(repoID)
					return repo != nil && s.viewerCanReadRepo(p.Context, repo), nil
				},
			},
			"reactionGroups": &graphql.Field{
				Type: graphql.NewList(graphql.NewNonNull(discussionReactionGroupType)),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					d, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("discussion source: unexpected type %T", p.Source)
					}
					discussionID, _ := d["databaseId"].(int)
					return reactionGroupsForGraphQL(s.store.Reactions, "discussion", discussionID), nil
				},
			},
			"reactions": &graphql.Field{
				Type: graphql.NewNonNull(discussionReactionConnectionType),
				Args: graphql.FieldConfigArgument{
					"first":  &graphql.ArgumentConfig{Type: graphql.Int},
					"last":   &graphql.ArgumentConfig{Type: graphql.Int},
					"after":  &graphql.ArgumentConfig{Type: graphql.String},
					"before": &graphql.ArgumentConfig{Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					d, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("discussion source: unexpected type %T", p.Source)
					}
					discussionID, _ := d["databaseId"].(int)
					return discussionReactionConnection(s.store, "discussion", discussionID, p.Args), nil
				},
			},
			"comments": &graphql.Field{
				Type: graphql.NewNonNull(discussionCommentConnectionType),
				Args: graphql.FieldConfigArgument{
					"first":  &graphql.ArgumentConfig{Type: graphql.Int},
					"last":   &graphql.ArgumentConfig{Type: graphql.Int},
					"after":  &graphql.ArgumentConfig{Type: graphql.String},
					"before": &graphql.ArgumentConfig{Type: graphql.String},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					d, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("discussion source: unexpected type %T", p.Source)
					}
					discussionID, _ := d["databaseId"].(int)
					comments := s.store.ListDiscussionComments(discussionID, 0)
					nodes := make([]map[string]interface{}, 0, len(comments))
					for _, c := range comments {
						nodes = append(nodes, discussionCommentToGQL(c, s.store))
					}
					return paginateGQLMaps(nodes, p.Args), nil
				},
			},
		},
	})

	// Registered for interface ResolveType dispatch (Lockable / Minimizable).
	s.graphqlTypes.discussion = discussionType
	s.graphqlTypes.discussionComment = discussionCommentType

	discussionEdgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DiscussionEdge",
		Fields: graphql.Fields{
			"node":   &graphql.Field{Type: discussionType},
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})

	discussionConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DiscussionConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(discussionType)},
			"edges":      &graphql.Field{Type: graphql.NewList(discussionEdgeType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})

	// --- Enums ---
	discussionOrderFieldEnum := graphql.NewEnum(graphql.EnumConfig{
		Name: "DiscussionOrderField",
		Values: graphql.EnumValueConfigMap{
			"CREATED_AT": &graphql.EnumValueConfig{Value: "CREATED_AT"},
			"UPDATED_AT": &graphql.EnumValueConfig{Value: "UPDATED_AT"},
		},
	})

	discussionOrderDirectionEnum := s.graphQLEnum("OrderDirection", "ASC", "DESC")

	// --- Repository fields ---
	repoType.AddFieldConfig("discussionCategories", &graphql.Field{
		Type: graphql.NewNonNull(discussionCategoryConnectionType),
		Args: graphql.FieldConfigArgument{
			"first":  &graphql.ArgumentConfig{Type: graphql.Int},
			"last":   &graphql.ArgumentConfig{Type: graphql.Int},
			"after":  &graphql.ArgumentConfig{Type: graphql.String},
			"before": &graphql.ArgumentConfig{Type: graphql.String},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, _ := repo["databaseId"].(int)
			cats := s.store.ListDiscussionCategories(repoID)
			nodes := make([]map[string]interface{}, 0, len(cats))
			for _, cat := range cats {
				nodes = append(nodes, discussionCategoryToGQL(cat))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})

	repoType.AddFieldConfig("discussions", &graphql.Field{
		Type: graphql.NewNonNull(discussionConnectionType),
		Args: graphql.FieldConfigArgument{
			"first":      &graphql.ArgumentConfig{Type: graphql.Int},
			"last":       &graphql.ArgumentConfig{Type: graphql.Int},
			"after":      &graphql.ArgumentConfig{Type: graphql.String},
			"before":     &graphql.ArgumentConfig{Type: graphql.String},
			"categoryId": &graphql.ArgumentConfig{Type: graphql.ID},
			"orderBy": &graphql.ArgumentConfig{Type: graphql.NewInputObject(graphql.InputObjectConfig{
				Name: "DiscussionOrder",
				Fields: graphql.InputObjectConfigFieldMap{
					"field":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(discussionOrderFieldEnum)},
					"direction": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(discussionOrderDirectionEnum)},
				},
			})},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoID, _ := repo["databaseId"].(int)

			categoryID := 0
			if catNodeID, ok := p.Args["categoryId"].(string); ok && catNodeID != "" {
				cat := findDiscussionCategoryByNodeID(s.store, catNodeID)
				if cat == nil || cat.RepoID != repoID {
					return paginateGQLMaps(nil, p.Args), nil
				}
				categoryID = cat.ID
			}

			discussions := s.store.ListDiscussions(repoID, categoryID)

			orderField, direction := "CREATED_AT", "DESC"
			if orderBy, ok := p.Args["orderBy"].(map[string]interface{}); ok {
				if f, ok := orderBy["field"].(string); ok && f != "" {
					orderField = f
				}
				if d, ok := orderBy["direction"].(string); ok && d != "" {
					direction = d
				}
			}
			sort.SliceStable(discussions, func(a, b int) bool {
				var less bool
				if orderField == "UPDATED_AT" {
					less = discussions[a].UpdatedAt.Before(discussions[b].UpdatedAt)
				} else {
					less = discussions[a].CreatedAt.Before(discussions[b].CreatedAt)
				}
				if direction == "DESC" {
					return !less
				}
				return less
			})

			nodes := make([]map[string]interface{}, 0, len(discussions))
			for _, d := range discussions {
				nodes = append(nodes, discussionToGQL(d, s.store))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})

	repoType.AddFieldConfig("discussion", &graphql.Field{
		Type: discussionType,
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
			d := s.store.GetDiscussionByNumber(repoID, number)
			if d == nil {
				return nil, &ghNotFoundError{message: fmt.Sprintf("Could not resolve to a Discussion with the number of %d.", number)}
			}
			return discussionToGQL(d, s.store), nil
		},
	})

	// --- Mutations ---
	createDiscussionInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreateDiscussionInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"repositoryId":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"categoryId":       &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"title":            &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"body":             &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})

	createDiscussionPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "CreateDiscussionPayload",
		Fields: graphql.Fields{
			"discussion":       &graphql.Field{Type: discussionType},
			"clientMutationId": &graphql.Field{Type: graphql.String},
		},
	})

	updateDiscussionInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UpdateDiscussionInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"discussionId":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"title":            &graphql.InputObjectFieldConfig{Type: graphql.String},
			"body":             &graphql.InputObjectFieldConfig{Type: graphql.String},
			"categoryId":       &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})

	updateDiscussionPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "UpdateDiscussionPayload",
		Fields: graphql.Fields{
			"discussion":       &graphql.Field{Type: discussionType},
			"clientMutationId": &graphql.Field{Type: graphql.String},
		},
	})

	deleteDiscussionInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "DeleteDiscussionInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"id":               &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})

	deleteDiscussionPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DeleteDiscussionPayload",
		Fields: graphql.Fields{
			"discussion":       &graphql.Field{Type: discussionType},
			"clientMutationId": &graphql.Field{Type: graphql.String},
		},
	})

	addDiscussionCommentInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "AddDiscussionCommentInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"discussionId":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"body":             &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"replyToId":        &graphql.InputObjectFieldConfig{Type: graphql.ID},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})

	addDiscussionCommentPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "AddDiscussionCommentPayload",
		Fields: graphql.Fields{
			"comment":          &graphql.Field{Type: discussionCommentType},
			"clientMutationId": &graphql.Field{Type: graphql.String},
		},
	})

	updateDiscussionCommentInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UpdateDiscussionCommentInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"commentId":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"body":             &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})

	updateDiscussionCommentPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "UpdateDiscussionCommentPayload",
		Fields: graphql.Fields{
			"comment":          &graphql.Field{Type: discussionCommentType},
			"clientMutationId": &graphql.Field{Type: graphql.String},
		},
	})

	deleteDiscussionCommentInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "DeleteDiscussionCommentInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"id":               &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})

	deleteDiscussionCommentPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DeleteDiscussionCommentPayload",
		Fields: graphql.Fields{
			"comment":          &graphql.Field{Type: discussionCommentType},
			"clientMutationId": &graphql.Field{Type: graphql.String},
		},
	})

	markDiscussionCommentAsAnswerInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "MarkDiscussionCommentAsAnswerInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"id":               &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})

	markDiscussionCommentAsAnswerPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "MarkDiscussionCommentAsAnswerPayload",
		Fields: graphql.Fields{
			"discussion":       &graphql.Field{Type: discussionType},
			"clientMutationId": &graphql.Field{Type: graphql.String},
		},
	})

	unmarkDiscussionCommentAsAnswerInputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "UnmarkDiscussionCommentAsAnswerInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"id":               &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		},
	})

	unmarkDiscussionCommentAsAnswerPayloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "UnmarkDiscussionCommentAsAnswerPayload",
		Fields: graphql.Fields{
			"discussion":       &graphql.Field{Type: discussionType},
			"clientMutationId": &graphql.Field{Type: graphql.String},
		},
	})

	s.registerMutation(mutationType, "createDiscussion", &graphql.Field{
		Type: createDiscussionPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(createDiscussionInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			repoNodeID, _ := input["repositoryId"].(string)
			categoryNodeID, _ := input["categoryId"].(string)
			title, _ := input["title"].(string)
			body, _ := input["body"].(string)

			repo := findRepoByNodeID(s.store, repoNodeID)
			if repo == nil {
				return nil, fmt.Errorf("could not resolve to a Repository with the global id of '%s'", repoNodeID)
			}
			cat := findDiscussionCategoryByNodeID(s.store, categoryNodeID)
			if cat == nil || cat.RepoID != repo.ID {
				return nil, fmt.Errorf("could not resolve to a DiscussionCategory with the global id of '%s'", categoryNodeID)
			}
			if title == "" {
				return nil, fmt.Errorf("title is required")
			}
			d := s.store.CreateDiscussion(repo.ID, cat.ID, user.ID, title, body)
			// `discussion` (created) fires so `on: discussion` workflows run (ACT-026).
			s.emitWebhookEvent(repo.FullName, "discussion", "created", map[string]interface{}{
				"action":     "created",
				"discussion": map[string]interface{}{"number": d.Number, "title": d.Title, "body": d.Body},
				"repository": s.repoPayload(repo),
				"sender":     userToJSON(user),
			})
			return map[string]interface{}{
				"discussion":       discussionToGQL(d, s.store),
				"clientMutationId": input["clientMutationId"],
			}, nil
		},
	})

	s.registerMutation(mutationType, "updateDiscussion", &graphql.Field{
		Type: updateDiscussionPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(updateDiscussionInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			discussionNodeID, _ := input["discussionId"].(string)
			d := findDiscussionByNodeID(s.store, discussionNodeID)
			if d == nil {
				return nil, fmt.Errorf("could not resolve to a Discussion")
			}
			s.store.UpdateDiscussion(d.ID, func(disc *Discussion) {
				if v, ok := input["title"].(string); ok {
					disc.Title = v
				}
				if v, ok := input["body"].(string); ok {
					disc.Body = v
				}
				if v, ok := input["categoryId"].(string); ok && v != "" {
					if cat := findDiscussionCategoryByNodeID(s.store, v); cat != nil && cat.RepoID == disc.RepoID {
						disc.CategoryID = cat.ID
					}
				}
			})
			return map[string]interface{}{
				"discussion":       discussionToGQL(s.store.GetDiscussion(d.ID), s.store),
				"clientMutationId": input["clientMutationId"],
			}, nil
		},
	})

	s.registerMutation(mutationType, "deleteDiscussion", &graphql.Field{
		Type: deleteDiscussionPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(deleteDiscussionInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			discussionNodeID, _ := input["id"].(string)
			d := findDiscussionByNodeID(s.store, discussionNodeID)
			if d == nil {
				return nil, fmt.Errorf("could not resolve to a Discussion")
			}
			source := discussionToGQL(d, s.store)
			s.store.DeleteDiscussion(d.ID)
			return map[string]interface{}{
				"discussion":       source,
				"clientMutationId": input["clientMutationId"],
			}, nil
		},
	})

	s.registerMutation(mutationType, "addDiscussionComment", &graphql.Field{
		Type: addDiscussionCommentPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(addDiscussionCommentInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			user := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			discussionNodeID, _ := input["discussionId"].(string)
			body, _ := input["body"].(string)
			d := findDiscussionByNodeID(s.store, discussionNodeID)
			if d == nil {
				return nil, fmt.Errorf("could not resolve to a Discussion")
			}
			parentID := 0
			if replyToID, ok := input["replyToId"].(string); ok && replyToID != "" {
				if parent := findDiscussionCommentByNodeID(s.store, replyToID); parent != nil && parent.DiscussionID == d.ID {
					parentID = parent.ID
				} else {
					return nil, fmt.Errorf("could not resolve replyToId to a comment on this discussion")
				}
			}
			c := s.store.CreateDiscussionComment(d.ID, user.ID, body, parentID)
			// `discussion_comment` (created) fires so `on: discussion_comment`
			// workflows run (ACT-026).
			if repo := s.store.GetRepoByID(d.RepoID); repo != nil {
				s.emitWebhookEvent(repo.FullName, "discussion_comment", "created", map[string]interface{}{
					"action":     "created",
					"comment":    map[string]interface{}{"id": c.ID, "body": c.Body},
					"discussion": map[string]interface{}{"number": d.Number, "title": d.Title},
					"repository": s.repoPayload(repo),
					"sender":     userToJSON(user),
				})
			}
			return map[string]interface{}{
				"comment":          discussionCommentToGQL(c, s.store),
				"clientMutationId": input["clientMutationId"],
			}, nil
		},
	})

	s.registerMutation(mutationType, "updateDiscussionComment", &graphql.Field{
		Type: updateDiscussionCommentPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(updateDiscussionCommentInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			commentNodeID, _ := input["commentId"].(string)
			body, _ := input["body"].(string)
			c := findDiscussionCommentByNodeID(s.store, commentNodeID)
			if c == nil {
				return nil, fmt.Errorf("could not resolve to a DiscussionComment")
			}
			s.store.UpdateDiscussionComment(c.ID, func(cc *DiscussionComment) {
				cc.Body = body
			})
			return map[string]interface{}{
				"comment":          discussionCommentToGQL(s.store.GetDiscussionComment(c.ID), s.store),
				"clientMutationId": input["clientMutationId"],
			}, nil
		},
	})

	s.registerMutation(mutationType, "deleteDiscussionComment", &graphql.Field{
		Type: deleteDiscussionCommentPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(deleteDiscussionCommentInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			commentNodeID, _ := input["id"].(string)
			c := findDiscussionCommentByNodeID(s.store, commentNodeID)
			if c == nil {
				return nil, fmt.Errorf("could not resolve to a DiscussionComment")
			}
			s.store.DeleteDiscussionComment(c.ID)
			return map[string]interface{}{
				"comment":          discussionCommentToGQL(c, s.store),
				"clientMutationId": input["clientMutationId"],
			}, nil
		},
	})

	s.registerMutation(mutationType, "markDiscussionCommentAsAnswer", &graphql.Field{
		Type: markDiscussionCommentAsAnswerPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(markDiscussionCommentAsAnswerInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			commentNodeID, _ := input["id"].(string)
			c := findDiscussionCommentByNodeID(s.store, commentNodeID)
			if c == nil {
				return nil, fmt.Errorf("could not resolve to a DiscussionComment")
			}
			d := s.store.GetDiscussion(c.DiscussionID)
			if d == nil {
				return nil, fmt.Errorf("could not resolve to a Discussion")
			}
			cat := s.store.GetDiscussionCategory(d.CategoryID)
			if cat == nil || !cat.IsAnswerable {
				return nil, fmt.Errorf("this discussion's category does not support answers")
			}
			s.store.MarkDiscussionCommentAsAnswer(c.ID)
			return map[string]interface{}{
				"discussion":       discussionToGQL(s.store.GetDiscussion(d.ID), s.store),
				"clientMutationId": input["clientMutationId"],
			}, nil
		},
	})

	s.registerMutation(mutationType, "unmarkDiscussionCommentAsAnswer", &graphql.Field{
		Type: unmarkDiscussionCommentAsAnswerPayloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(unmarkDiscussionCommentAsAnswerInputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			commentNodeID, _ := input["id"].(string)
			c := findDiscussionCommentByNodeID(s.store, commentNodeID)
			if c == nil {
				return nil, fmt.Errorf("could not resolve to a DiscussionComment")
			}
			d := s.store.GetDiscussion(c.DiscussionID)
			if d == nil {
				return nil, fmt.Errorf("could not resolve to a Discussion")
			}
			s.store.UnmarkDiscussionCommentAsAnswer(c.ID)
			return map[string]interface{}{
				"discussion":       discussionToGQL(s.store.GetDiscussion(d.ID), s.store),
				"clientMutationId": input["clientMutationId"],
			}, nil
		},
	})
}

// --- GraphQL converters ---

func discussionCategoryToGQL(cat *DiscussionCategory) map[string]interface{} {
	return map[string]interface{}{
		"nodeID":       cat.NodeID,
		"name":         cat.Name,
		"emoji":        cat.Emoji,
		"description":  cat.Description,
		"isAnswerable": cat.IsAnswerable,
		"createdAt":    cat.CreatedAt.Format(time.RFC3339),
		"updatedAt":    cat.UpdatedAt.Format(time.RFC3339),
	}
}

func discussionToGQL(d *Discussion, st *Store) map[string]interface{} {
	// A comment can outlive its soft-deleted discussion, and several callers pass
	// a re-fetched GetDiscussion result straight through; guarding here keeps a
	// nil discussion from panicking on d.RepoID below rather than relying on
	// every call site to check first.
	if d == nil {
		return nil
	}

	st.Mu.RLock()
	defer st.Mu.RUnlock()

	repo := st.Repos[d.RepoID]
	url := ""
	if repo != nil {
		url = externalURL("/" + repo.FullName + "/discussions/" + strconv.Itoa(d.Number))
	}

	var author map[string]interface{}
	if u, ok := st.Users[d.AuthorID]; ok {
		author = userToGraphQL(u)
	}

	var category map[string]interface{}
	if cat, ok := st.DiscussionCategories[d.CategoryID]; ok {
		category = discussionCategoryToGQL(cat)
	}

	var lastEditedAt interface{}
	if d.LastEditedAt != nil {
		lastEditedAt = d.LastEditedAt.Format(time.RFC3339)
	}

	var publishedAt interface{}
	if d.PublishedAt != nil {
		publishedAt = d.PublishedAt.Format(time.RFC3339)
	}

	return map[string]interface{}{
		"nodeID":           d.NodeID,
		"databaseId":       d.ID,
		"repoID":           d.RepoID,
		"number":           d.Number,
		"title":            d.Title,
		"body":             d.Body,
		"bodyHTML":         discussionBodyToHTML(d.Body),
		"bodyText":         discussionBodyToText(d.Body),
		"author":           author,
		"authorID":         d.AuthorID,
		"category":         category,
		"createdAt":        d.CreatedAt.Format(time.RFC3339),
		"updatedAt":        d.UpdatedAt.Format(time.RFC3339),
		"lastEditedAt":     lastEditedAt,
		"locked":           d.Locked,
		"activeLockReason": graphQLLockReason(d.LockedReason),
		"publishedAt":      publishedAt,
		"url":              url,
	}
}

func discussionCommentToGQL(c *DiscussionComment, st *Store) map[string]interface{} {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	var author map[string]interface{}
	if u, ok := st.Users[c.AuthorID]; ok {
		author = userToGraphQL(u)
	}

	var lastEditedAt interface{}
	if c.LastEditedAt != nil {
		lastEditedAt = c.LastEditedAt.Format(time.RFC3339)
	}

	return map[string]interface{}{
		"nodeID":       c.NodeID,
		"databaseId":   c.ID,
		"discussionID": c.DiscussionID,
		"author":       author,
		"body":         c.Body,
		"bodyHTML":     discussionBodyToHTML(c.Body),
		"bodyText":     discussionBodyToText(c.Body),
		"createdAt":    c.CreatedAt.Format(time.RFC3339),
		"updatedAt":    c.UpdatedAt.Format(time.RFC3339),
		"lastEditedAt": lastEditedAt,
		"isAnswer":     c.IsAnswer,
	}
}

func discussionBodyToHTML(body string) string {
	if body == "" {
		return ""
	}
	var rendered bytes.Buffer
	if err := markdownModeRenderer.Convert([]byte(body), &rendered); err != nil {
		return ""
	}
	return rendered.String()
}

func discussionBodyToText(body string) string {
	return strings.TrimSpace(body)
}

func discussionReactionConnection(st *Store, parentType string, parentID int, args map[string]interface{}) map[string]interface{} {
	reactions := st.Reactions.ListReactions(parentType, parentID, "")
	nodes := make([]map[string]interface{}, 0, len(reactions))
	for _, r := range reactions {
		var userMap map[string]interface{}
		if u := st.GetUserByID(r.UserID); u != nil {
			userMap = userToGraphQL(u)
		}
		nodes = append(nodes, map[string]interface{}{
			"id":      fmt.Sprintf("REA_kgDO%08d", r.ID),
			"content": r.Content,
			"user":    userMap,
		})
	}
	return paginateGQLMaps(nodes, args)
}

func reactionContentToGraphQL(content string) string {
	switch content {
	case "+1":
		return "THUMBS_UP"
	case "-1":
		return "THUMBS_DOWN"
	case "laugh":
		return "LAUGH"
	case "hooray":
		return "HOORAY"
	case "confused":
		return "CONFUSED"
	case "heart":
		return "HEART"
	case "rocket":
		return "ROCKET"
	case "eyes":
		return "EYES"
	}
	return content
}

// paginateGQLMaps implements Relay pagination over pre-converted node maps,
// supporting first/last/after/before.
// gqlNodeIdentity returns a stable identity for a rendered GraphQL node so its
// connection cursor survives inserts. Prefer the global node id.
func gqlNodeIdentity(n map[string]interface{}) string {
	for _, key := range []string{"nodeID", "id", "databaseId"} {
		switch v := n[key].(type) {
		case string:
			if v != "" {
				return v
			}
		case int:
			return strconv.Itoa(v)
		}
	}
	return ""
}

// resolveConnectionIndex returns the current position of the node a cursor
// identifies. If the cursor carries an identity that still exists, its live
// index is used (stable across inserts before it); otherwise the cursor's
// recorded index is the fallback.
// paginateGQLMaps windows an already-rendered node slice. It wraps each node
// in a lazy item whose render is a no-op (the node is already built) and
// delegates to the shared paginateGQLItems windowing, so eager and lazy callers
// share one cursor implementation.
func paginateGQLMaps(nodes []map[string]interface{}, args map[string]interface{}) map[string]interface{} {
	items := make([]gqlConnItem, len(nodes))
	for i, n := range nodes {
		n := n
		items[i] = gqlConnItem{identity: gqlNodeIdentity(n), render: func() map[string]interface{} { return n }}
	}
	return paginateGQLItems(items, args)
}

// --- Node ID lookup helpers ---

package graphqlapi

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// Milestone, Label, Discussion and DiscussionComment field surfaces, plus the
// small metadata connection types (RepositoryTopic, Language, Reactor).
// Companion to gh_issue_fields_graphql.go.

// --- shared render helpers -------------------------------------------------

// repoNodeSource renders a repository as its GraphQL source map.
func (s *Resolver) repoNodeSource(repoID int) map[string]interface{} {
	repo := s.store.GetRepoByID(repoID)
	if repo == nil {
		return nil
	}
	return repoToGraphQL(s.store, s.store.SnapRepo(repo))
}

// fullPageInfoResolver answers the all-false pageInfo for a fully materialised
// connection.
func fullPageInfoResolver(graphql.ResolveParams) (interface{}, error) {
	return map[string]interface{}{
		"hasNextPage": false, "hasPreviousPage": false,
		"startCursor": nil, "endCursor": nil,
	}, nil
}

func languageNodeID(name string) string {
	return "LA_" + base64.RawURLEncoding.EncodeToString([]byte("Language"+name))
}

func repositoryTopicNodeID(name string) string {
	return "RT_" + base64.RawURLEncoding.EncodeToString([]byte("RepositoryTopic"+name))
}

// languageConnectionNodes derives the node list from an edges-only language
// connection source.
func languageConnectionNodes(source interface{}) []interface{} {
	src, _ := source.(map[string]interface{})
	edges, _ := src["edges"].([]interface{})
	nodes := make([]interface{}, 0, len(edges))
	for _, e := range edges {
		if em, ok := e.(map[string]interface{}); ok {
			nodes = append(nodes, em["node"])
		}
	}
	return nodes
}

// languageConnectionTotalSize sums the byte sizes across a language
// connection's edges.
func languageConnectionTotalSize(source interface{}) int {
	src, _ := source.(map[string]interface{})
	edges, _ := src["edges"].([]interface{})
	total := 0
	for _, e := range edges {
		em, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		switch n := em["size"].(type) {
		case int:
			total += n
		case int64:
			total += int(n)
		}
	}
	return total
}

// gqlRepositoryTopicConnectionType builds RepositoryTopicConnection.
func (s *Resolver) gqlRepositoryTopicConnectionType() *graphql.Object {
	if s.graphqlTypes.repositoryTopicConnection != nil {
		return s.graphqlTypes.repositoryTopicConnection
	}
	uri := s.graphQLStringScalar("URI")
	topicName := func(src interface{}) string {
		m, _ := src.(map[string]interface{})
		return srcStr(m, "name")
	}
	repositoryTopic := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryTopic",
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return repositoryTopicNodeID(topicName(p.Source)), nil
				},
			},
			"topic": &graphql.Field{Type: graphql.NewNonNull(s.gqlTopicType())},
			"resourcePath": &graphql.Field{
				Type: graphql.NewNonNull(uri),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return "/topics/" + url.PathEscape(topicName(p.Source)), nil
				},
			},
			"url": &graphql.Field{
				Type: graphql.NewNonNull(uri),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return externalURL("/topics/" + url.PathEscape(topicName(p.Source))), nil
				},
			},
		},
	})
	repositoryTopicEdge := graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryTopicEdge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: repositoryTopic},
		},
	})
	s.graphqlTypes.repositoryTopicConnection = graphql.NewObject(graphql.ObjectConfig{
		Name: "RepositoryTopicConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(repositoryTopic)},
			"edges":      &graphql.Field{Type: graphql.NewList(repositoryTopicEdge)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})
	return s.graphqlTypes.repositoryTopicConnection
}

// reactorConnectionFor builds the connection ReactionGroup.reactors and
// ReactionGroup.users share: the users who reacted with the group's emoji,
// each edge carrying its reactedAt instant.
func (s *Resolver) reactorConnectionFor(parentType string, parentID int, restContent string, args map[string]interface{}) map[string]interface{} {
	reactions := s.store.Reactions.ListReactions(parentType, parentID, restContent)
	sort.SliceStable(reactions, func(a, b int) bool {
		if !reactions[a].CreatedAt.Equal(reactions[b].CreatedAt) {
			return reactions[a].CreatedAt.Before(reactions[b].CreatedAt)
		}
		return reactions[a].ID < reactions[b].ID
	})
	total := len(reactions)

	start := 0
	if after, ok := args["after"].(string); ok && after != "" {
		start = decodeCursor(after) + 1
	}
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end := total
	first := 30
	if n, ok := intArg(args, "first"); ok && n >= 0 {
		if n > 100 {
			n = 100
		}
		first = n
	}
	if end-start > first {
		end = start + first
	}

	nodes := make([]map[string]interface{}, 0, end-start)
	edges := make([]map[string]interface{}, 0, end-start)
	for i := start; i < end; i++ {
		r := reactions[i]
		u := s.store.GetUserByID(r.UserID)
		if u == nil {
			continue
		}
		node := userToGraphQL(u)
		nodes = append(nodes, node)
		edges = append(edges, map[string]interface{}{
			"node":      node,
			"cursor":    encodeCursor(i),
			"reactedAt": r.CreatedAt.Format(time.RFC3339),
		})
	}
	var startCursor, endCursor interface{}
	if len(edges) > 0 {
		startCursor = edges[0]["cursor"]
		endCursor = edges[len(edges)-1]["cursor"]
	}
	return map[string]interface{}{
		"nodes":      nodes,
		"edges":      edges,
		"totalCount": total,
		"pageInfo": map[string]interface{}{
			"hasNextPage":     end < total,
			"hasPreviousPage": start > 0,
			"startCursor":     startCursor,
			"endCursor":       endCursor,
		},
	}
}

// --- Milestone -------------------------------------------------------------

func (s *Resolver) enrichMilestoneType() {
	milestoneType := s.graphqlTypes.milestone
	uri := s.graphQLStringScalar("URI")
	dateTime := s.graphQLStringScalar("DateTime")
	actor := s.graphqlTypes.actor
	repoType := s.graphqlTypes.repository
	issueConn := s.graphqlTypes.issueConnection
	prConn := s.graphqlTypes.pullRequestConnection

	milestonePath := func(src map[string]interface{}) string {
		repo := s.store.GetRepoByID(srcInt(src, "repoID"))
		if repo == nil {
			return ""
		}
		return "/" + repo.FullName + "/milestone/" + strconv.Itoa(srcInt(src, "number"))
	}
	// counts returns (open, closed) issue counts for the milestone.
	counts := func(src map[string]interface{}) (int, int) {
		repoID := srcInt(src, "repoID")
		msID := srcInt(src, "_dbID")
		open, closed := 0, 0
		for _, iss := range s.store.ListIssues(repoID, "") {
			if iss.MilestoneID != msID {
				continue
			}
			if iss.State == "CLOSED" {
				closed++
			} else {
				open++
			}
		}
		return open, closed
	}
	canWrite := func(p graphql.ResolveParams, src map[string]interface{}) bool {
		return s.viewerMayActOnRepo(p.Context, s.store.GetRepoByID(srcInt(src, "repoID")), store.ScopeIssues, store.PermWrite, store.PermAdmin)
	}
	writeBool := func() *graphql.Field {
		return &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return canWrite(p, src), nil
			},
		}
	}

	fields := graphql.Fields{
		"closed": &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return srcStr(src, "state") == "CLOSED", nil
			},
		},
		"closedAt":  &graphql.Field{Type: dateTime},
		"createdAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
		"updatedAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
		"creator": &graphql.Field{
			Type: actor,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return optionalRendered(s.store.GetUserByID(srcInt(src, "creatorID")), userToGraphQL), nil
			},
		},
		"descriptionHTML": &graphql.Field{
			Type: graphql.String,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				desc := srcStr(src, "description")
				if desc == "" {
					return nil, nil
				}
				return discussionBodyToHTML(desc), nil
			},
		},
		"openIssueCount": &graphql.Field{
			Type: graphql.NewNonNull(graphql.Int),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				open, _ := counts(src)
				return open, nil
			},
		},
		"closedIssueCount": &graphql.Field{
			Type: graphql.NewNonNull(graphql.Int),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				_, closed := counts(src)
				return closed, nil
			},
		},
		"progressPercentage": &graphql.Field{
			Type: graphql.NewNonNull(graphql.Float),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				open, closed := counts(src)
				if open+closed == 0 {
					return float64(0), nil
				}
				return float64(closed) * 100 / float64(open+closed), nil
			},
		},
		"repository": &graphql.Field{
			Type: graphql.NewNonNull(repoType),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				repo := s.repoNodeSource(srcInt(src, "repoID"))
				if repo == nil {
					return nil, fmt.Errorf("milestone repository %d not found", srcInt(src, "repoID"))
				}
				return repo, nil
			},
		},
		"resourcePath": &graphql.Field{
			Type: graphql.NewNonNull(uri),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return milestonePath(src), nil
			},
		},
		"url": &graphql.Field{
			Type: graphql.NewNonNull(uri),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return externalURL(milestonePath(src)), nil
			},
		},
		"issues": &graphql.Field{
			Type: graphql.NewNonNull(issueConn),
			Args: relayConnectionArgs(),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return s.milestoneIssueConnection(srcInt(src, "repoID"), srcInt(src, "_dbID"), p.Args), nil
			},
		},
		"pullRequests": &graphql.Field{
			Type: graphql.NewNonNull(prConn),
			Args: relayConnectionArgs(),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return s.milestonePullRequestConnection(srcInt(src, "repoID"), srcInt(src, "_dbID"), p.Args), nil
			},
		},
		"viewerCanClose":  writeBool(),
		"viewerCanReopen": writeBool(),
	}
	for name, f := range fields {
		milestoneType.AddFieldConfig(name, f)
	}
}

func (s *Resolver) milestoneIssueConnection(repoID, msID int, args map[string]interface{}) map[string]interface{} {
	nodes := []map[string]interface{}{}
	for _, iss := range s.store.ListIssues(repoID, "") {
		if iss.MilestoneID == msID {
			nodes = append(nodes, issueToGQL(iss, s.store))
		}
	}
	sortGQLNodesByCreatedAt(nodes)
	return paginateGQLMaps(nodes, args)
}

func (s *Resolver) milestonePullRequestConnection(repoID, msID int, args map[string]interface{}) map[string]interface{} {
	nodes := []map[string]interface{}{}
	for _, pr := range s.store.ListPullRequests(repoID, "") {
		if pr.MilestoneID == msID {
			nodes = append(nodes, pullRequestToGQL(pr, s.store))
		}
	}
	sortGQLNodesByCreatedAt(nodes)
	return paginateGQLMaps(nodes, args)
}

// --- Label -----------------------------------------------------------------

func (s *Resolver) enrichLabelType() {
	labelType := s.graphqlTypes.labelType
	uri := s.graphQLStringScalar("URI")
	dateTime := s.graphQLStringScalar("DateTime")
	repoType := s.graphqlTypes.repository
	issueConn := s.graphqlTypes.issueConnection
	prConn := s.graphqlTypes.pullRequestConnection

	labelPath := func(src map[string]interface{}) string {
		repo := s.store.GetRepoByID(srcInt(src, "repoID"))
		if repo == nil {
			return ""
		}
		return "/" + repo.FullName + "/labels/" + url.PathEscape(srcStr(src, "name"))
	}

	fields := graphql.Fields{
		"createdAt": &graphql.Field{Type: dateTime},
		// No label last-updated instant is recorded (null).
		"updatedAt": &graphql.Field{Type: dateTime, Resolve: nilResolver},
		"isDefault": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		"repository": &graphql.Field{
			Type: graphql.NewNonNull(repoType),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				repo := s.repoNodeSource(srcInt(src, "repoID"))
				if repo == nil {
					return nil, fmt.Errorf("label repository %d not found", srcInt(src, "repoID"))
				}
				return repo, nil
			},
		},
		"resourcePath": &graphql.Field{
			Type: graphql.NewNonNull(uri),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return labelPath(src), nil
			},
		},
		"url": &graphql.Field{
			Type: graphql.NewNonNull(uri),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return externalURL(labelPath(src)), nil
			},
		},
		"issues": &graphql.Field{
			Type: graphql.NewNonNull(issueConn),
			Args: relayConnectionArgs(),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return s.labelIssueConnection(srcInt(src, "repoID"), srcInt(src, "_dbID"), p.Args), nil
			},
		},
		"pullRequests": &graphql.Field{
			Type: graphql.NewNonNull(prConn),
			Args: relayConnectionArgs(),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return s.labelPullRequestConnection(srcInt(src, "repoID"), srcInt(src, "_dbID"), p.Args), nil
			},
		},
	}
	for name, f := range fields {
		labelType.AddFieldConfig(name, f)
	}
}

func containsInt(ids []int, want int) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func (s *Resolver) labelIssueConnection(repoID, labelID int, args map[string]interface{}) map[string]interface{} {
	nodes := []map[string]interface{}{}
	for _, iss := range s.store.ListIssues(repoID, "") {
		if containsInt(iss.LabelIDs, labelID) {
			nodes = append(nodes, issueToGQL(iss, s.store))
		}
	}
	sortGQLNodesByCreatedAt(nodes)
	return paginateGQLMaps(nodes, args)
}

func (s *Resolver) labelPullRequestConnection(repoID, labelID int, args map[string]interface{}) map[string]interface{} {
	nodes := []map[string]interface{}{}
	for _, pr := range s.store.ListPullRequests(repoID, "") {
		if containsInt(pr.LabelIDs, labelID) {
			nodes = append(nodes, pullRequestToGQL(pr, s.store))
		}
	}
	sortGQLNodesByCreatedAt(nodes)
	return paginateGQLMaps(nodes, args)
}

// --- Discussion ------------------------------------------------------------

func (s *Resolver) enrichDiscussionType() {
	discussionType := s.graphqlTypes.discussion
	uri := s.graphQLStringScalar("URI")
	dateTime := s.graphQLStringScalar("DateTime")
	actor := s.graphqlTypes.actor
	repoType := s.graphqlTypes.repository
	labelConn := s.graphqlTypes.labelConnection
	discussionComment := s.graphqlTypes.discussionComment
	authorAssocEnum := s.graphQLEnum("CommentAuthorAssociation",
		"COLLABORATOR", "CONTRIBUTOR", "FIRST_TIMER", "FIRST_TIME_CONTRIBUTOR",
		"MANNEQUIN", "MEMBER", "NONE", "OWNER")
	subState := s.graphQLEnum("SubscriptionState", "IGNORED", "SUBSCRIBED", "UNSUBSCRIBED")

	discussionPath := func(src map[string]interface{}) string {
		repo := s.store.GetRepoByID(srcInt(src, "repoID"))
		if repo == nil {
			return ""
		}
		return "/" + repo.FullName + "/discussions/" + strconv.Itoa(srcInt(src, "number"))
	}
	canWriteDisc := func(p graphql.ResolveParams, src map[string]interface{}) bool {
		v := s.ghUserFromContext(p.Context)
		if v == nil {
			return false
		}
		if srcInt(src, "authorID") == v.ID {
			return true
		}
		return s.viewerMayActOnRepo(p.Context, s.store.GetRepoByID(srcInt(src, "repoID")), store.ScopeDiscussions, store.PermWrite, store.PermAdmin)
	}
	discBool := func(fn func(p graphql.ResolveParams, src map[string]interface{}) bool) *graphql.Field {
		return &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return fn(p, src), nil
			},
		}
	}

	fields := graphql.Fields{
		"authorAssociation": &graphql.Field{
			Type: graphql.NewNonNull(authorAssocEnum),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				s.store.Mu.RLock()
				defer s.store.Mu.RUnlock()
				return authorAssociationForRepoLocked(s.store, srcInt(src, "repoID"), srcInt(src, "authorID")), nil
			},
		},
		"createdViaEmail": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: falseResolver},
		"editor":          &graphql.Field{Type: actor, Resolve: nilResolver},
		"includesCreatedEdit": &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return src["lastEditedAt"] != nil, nil
			},
		},
		"isAnswered": &graphql.Field{
			Type: graphql.Boolean,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return s.discussionAnswerComment(srcInt(src, "databaseId")) != nil, nil
			},
		},
		"answer": &graphql.Field{
			Type: discussionComment,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				ans := s.discussionAnswerComment(srcInt(src, "databaseId"))
				if ans == nil {
					return nil, nil
				}
				return discussionCommentToGQL(ans, s.store), nil
			},
		},
		// Who chose an answer and when are unrecorded (null even when answered).
		"answerChosenAt": &graphql.Field{Type: dateTime, Resolve: nilResolver},
		"answerChosenBy": &graphql.Field{Type: actor, Resolve: nilResolver},
		"repository": &graphql.Field{
			Type: graphql.NewNonNull(repoType),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				repo := s.repoNodeSource(srcInt(src, "repoID"))
				if repo == nil {
					return nil, fmt.Errorf("discussion repository %d not found", srcInt(src, "repoID"))
				}
				return repo, nil
			},
		},
		"resourcePath": &graphql.Field{
			Type: graphql.NewNonNull(uri),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return discussionPath(src), nil
			},
		},
		// Discussions carry no labels (real empty connection).
		"labels": &graphql.Field{
			Type:    labelConn,
			Args:    relayConnectionArgs(),
			Resolve: func(graphql.ResolveParams) (interface{}, error) { return emptyGQLConnection(), nil },
		},
		"userContentEdits": &graphql.Field{
			Type:    s.gqlUserContentEditConnectionType(),
			Args:    relayConnectionArgs(),
			Resolve: func(graphql.ResolveParams) (interface{}, error) { return emptyUserContentEditConnection(), nil },
		},
		"viewerSubscription": &graphql.Field{Type: subState, Resolve: nilResolver},
		"viewerCanClose":     discBool(canWriteDisc),
		"viewerCanReopen":    discBool(canWriteDisc),
		"viewerCanLabel":     discBool(canWriteDisc),
		"viewerCanSubscribe": discBool(func(p graphql.ResolveParams, src map[string]interface{}) bool {
			repo := s.store.GetRepoByID(srcInt(src, "repoID"))
			return s.ghUserFromContext(p.Context) != nil && repo != nil && s.viewerCanReadRepo(p.Context, repo)
		}),
		"viewerDidAuthor": discBool(func(p graphql.ResolveParams, src map[string]interface{}) bool {
			v := s.ghUserFromContext(p.Context)
			return v != nil && srcInt(src, "authorID") == v.ID
		}),
	}
	for name, f := range fields {
		discussionType.AddFieldConfig(name, f)
	}
}

// discussionAnswerComment returns the comment marked as a discussion's answer.
func (s *Resolver) discussionAnswerComment(discussionID int) *store.DiscussionComment {
	for _, c := range s.store.ListDiscussionComments(discussionID, 0) {
		if c.IsAnswer {
			return c
		}
	}
	return nil
}

// --- DiscussionComment -----------------------------------------------------

// discussionCommentExtraFields returns the remaining DiscussionComment fields.
// The type is built from a FieldsThunk that AddFieldConfig cannot extend, so
// the thunk merges these in directly.
func (s *Resolver) discussionCommentExtraFields() graphql.Fields {
	uri := s.graphQLStringScalar("URI")
	dateTime := s.graphQLStringScalar("DateTime")
	actor := s.graphqlTypes.actor
	authorAssocEnum := s.graphQLEnum("CommentAuthorAssociation",
		"COLLABORATOR", "CONTRIBUTOR", "FIRST_TIMER", "FIRST_TIME_CONTRIBUTOR",
		"MANNEQUIN", "MEMBER", "NONE", "OWNER")

	commentRepo := func(src map[string]interface{}) *store.Repo {
		d := s.store.GetDiscussion(srcInt(src, "discussionID"))
		if d == nil {
			return nil
		}
		return s.store.GetRepoByID(d.RepoID)
	}
	commentPath := func(src map[string]interface{}) string {
		d := s.store.GetDiscussion(srcInt(src, "discussionID"))
		if d == nil {
			return ""
		}
		repo := s.store.GetRepoByID(d.RepoID)
		if repo == nil {
			return ""
		}
		return fmt.Sprintf("/%s/discussions/%d#discussioncomment-%d", repo.FullName, d.Number, srcInt(src, "databaseId"))
	}
	canWrite := func(p graphql.ResolveParams, src map[string]interface{}) bool {
		v := s.ghUserFromContext(p.Context)
		if v == nil {
			return false
		}
		if srcInt(src, "authorID") == v.ID {
			return true
		}
		return s.viewerMayActOnRepo(p.Context, commentRepo(src), store.ScopeDiscussions, store.PermWrite, store.PermAdmin)
	}
	writeOnly := func(p graphql.ResolveParams, src map[string]interface{}) bool {
		return s.viewerMayActOnRepo(p.Context, commentRepo(src), store.ScopeDiscussions, store.PermWrite, store.PermAdmin)
	}
	cBool := func(fn func(p graphql.ResolveParams, src map[string]interface{}) bool) *graphql.Field {
		return &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return fn(p, src), nil
			},
		}
	}
	isAnswer := func(src map[string]interface{}) bool {
		v, _ := src["isAnswer"].(bool)
		return v
	}
	answerable := func(src map[string]interface{}) bool {
		d := s.store.GetDiscussion(srcInt(src, "discussionID"))
		if d == nil {
			return false
		}
		cat := s.store.DiscussionCategories[d.CategoryID]
		return cat != nil && cat.IsAnswerable
	}

	return graphql.Fields{
		"authorAssociation": &graphql.Field{
			Type: graphql.NewNonNull(authorAssocEnum),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				repo := commentRepo(src)
				repoID := 0
				if repo != nil {
					repoID = repo.ID
				}
				s.store.Mu.RLock()
				defer s.store.Mu.RUnlock()
				return authorAssociationForRepoLocked(s.store, repoID, srcInt(src, "authorID")), nil
			},
		},
		"createdViaEmail": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: falseResolver},
		// No comment deletion timestamp is recorded (null).
		"deletedAt": &graphql.Field{Type: dateTime, Resolve: nilResolver},
		"editor":    &graphql.Field{Type: actor, Resolve: nilResolver},
		"includesCreatedEdit": &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return src["lastEditedAt"] != nil, nil
			},
		},
		"publishedAt": &graphql.Field{
			Type: dateTime,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return src["createdAt"], nil
			},
		},
		"replyTo": &graphql.Field{
			Type: s.graphqlTypes.discussionComment,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				parentID := srcInt(src, "parentID")
				if parentID == 0 {
					return nil, nil
				}
				parent := s.store.GetDiscussionComment(parentID)
				if parent == nil {
					return nil, nil
				}
				return discussionCommentToGQL(parent, s.store), nil
			},
		},
		"resourcePath": &graphql.Field{
			Type: graphql.NewNonNull(uri),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return commentPath(src), nil
			},
		},
		"url": &graphql.Field{
			Type: graphql.NewNonNull(uri),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return externalURL(commentPath(src)), nil
			},
		},
		"userContentEdits": &graphql.Field{
			Type:    s.gqlUserContentEditConnectionType(),
			Args:    relayConnectionArgs(),
			Resolve: func(graphql.ResolveParams) (interface{}, error) { return emptyUserContentEditConnection(), nil },
		},
		"viewerCanUpdate":     cBool(canWrite),
		"viewerCanDelete":     cBool(canWrite),
		"viewerCanMinimize":   cBool(writeOnly),
		"viewerCanUnminimize": cBool(writeOnly),
		"viewerCanMarkAsAnswer": cBool(func(p graphql.ResolveParams, src map[string]interface{}) bool {
			return writeOnly(p, src) && answerable(src) && !isAnswer(src)
		}),
		"viewerCanUnmarkAsAnswer": cBool(func(p graphql.ResolveParams, src map[string]interface{}) bool {
			return writeOnly(p, src) && isAnswer(src)
		}),
		"viewerDidAuthor": cBool(func(p graphql.ResolveParams, src map[string]interface{}) bool {
			v := s.ghUserFromContext(p.Context)
			return v != nil && srcInt(src, "authorID") == v.ID
		}),
		"viewerCannotUpdateReasons": &graphql.Field{
			Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(s.commentCannotUpdateReasonEnum()))),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return cannotUpdateReasons(s.ghUserFromContext(p.Context), canWrite(p, src)), nil
			},
		},
	}
}

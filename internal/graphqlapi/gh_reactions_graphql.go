package graphqlapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// initReactionGraphQLTypes creates the shared reaction types — Reaction,
// ReactionConnection and the Reactable interface — and registers them, so they
// are available when the concrete subject types (built earlier or later) declare
// they implement Reactable. Called once from initGraphQLSchema after userType
// exists. The Reactable interface's ResolveType reads the type registry lazily
// at query time, so it may reference concrete types not yet built.
func (s *Resolver) initReactionGraphQLTypes(userType *graphql.Object) {
	reactionContentEnum := s.graphQLEnum(
		"ReactionContent",
		"CONFUSED", "EYES", "HEART", "HOORAY", "LAUGH", "ROCKET", "THUMBS_DOWN", "THUMBS_UP",
	)
	reactionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Reaction",
		Fields: graphql.Fields{
			"id": &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"content": &graphql.Field{
				Type: graphql.NewNonNull(reactionContentEnum),
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
	s.graphqlTypes.reaction = reactionType

	s.graphqlTypes.reactionConnection = graphql.NewObject(graphql.ObjectConfig{
		Name: "ReactionConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(reactionType)},
			"edges":      &graphql.Field{Type: graphql.NewList(graphql.NewObject(graphql.ObjectConfig{Name: "ReactionEdge", Fields: graphql.Fields{"node": &graphql.Field{Type: reactionType}, "cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)}}}))},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})

	// The Reactable interface: GitHub's shared reaction contract implemented by
	// the nine concrete subject types. ResolveType discriminates on the source
	// node-id prefix (more-specific prefixes first), reading the registry at
	// query time once every concrete type is built.
	s.graphqlTypes.reactable = graphql.NewInterface(graphql.InterfaceConfig{
		Name: "Reactable",
		Fields: graphql.Fields{
			"id":             &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"databaseId":     &graphql.Field{Type: graphql.Int},
			"reactionGroups": &graphql.Field{Type: graphql.NewList(graphql.NewNonNull(s.gqlReactionGroupType()))},
			"reactions":      &graphql.Field{Type: graphql.NewNonNull(s.graphqlTypes.reactionConnection), Args: reactionConnectionArgs()},
			"viewerCanReact": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
		},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			nodeID, _ := source["nodeID"].(string)
			t := s.graphqlTypes
			switch {
			case strings.HasPrefix(nodeID, "PRRC_"):
				return t.pullRequestReviewComment
			case strings.HasPrefix(nodeID, "PRR_"):
				return t.pullRequestReview
			case strings.HasPrefix(nodeID, "PR_"):
				return t.pullRequest
			case strings.HasPrefix(nodeID, "DC_"):
				return t.discussionComment
			case strings.HasPrefix(nodeID, "D_"):
				return t.discussion
			case strings.HasPrefix(nodeID, "IC_"):
				return t.issueComment
			case strings.HasPrefix(nodeID, "CC_"):
				return t.commitComment
			case strings.HasPrefix(nodeID, "RE_"):
				return t.release
			default:
				return t.issue
			}
		},
	})

	// CommitComment is the one Reactable subject with no pre-existing GraphQL
	// type; create it now that the interface exists.
	s.initCommitCommentType()
}

// initCommitCommentType creates the CommitComment GraphQL type — the ninth
// Reactable concrete type, which had no GraphQL type before. It implements
// Reactable and carries the subset of official fields bleephub models. Called
// from initReactionGraphQLTypes after the Reactable interface exists.
func (s *Resolver) initCommitCommentType() {
	dateTime := s.graphQLStringScalar("DateTime")
	s.graphqlTypes.commitComment = graphql.NewObject(graphql.ObjectConfig{
		Name:       "CommitComment",
		Interfaces: []*graphql.Interface{s.graphqlTypes.reactable},
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, _ := p.Source.(map[string]interface{})
					return src["nodeID"], nil
				},
			},
			// body/createdAt/author/path resolve via the default map resolver
			// (field name == source key). Signatures match GitHub's CommitComment.
			"body":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"path":      &graphql.Field{Type: graphql.String},
			"createdAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"author":    &graphql.Field{Type: s.graphqlTypes.actor},
		},
	})
	s.addReactableFields(s.graphqlTypes.commitComment, "commit_comment")
}

// commitCommentToGQL renders a store commit comment as its GraphQL source map.
func commitCommentToGQL(c *store.CommitComment, st *store.Store) map[string]interface{} {
	var author map[string]interface{}
	if u := st.GetUserByID(c.AuthorID); u != nil {
		author = userToGraphQL(u)
	}
	return map[string]interface{}{
		"nodeID":     c.NodeID,
		"databaseId": c.ID,
		"body":       c.Body,
		"path":       c.Path,
		"createdAt":  c.CreatedAt.Format(time.RFC3339),
		"author":     author,
	}
}

// reactionConnectionArgs is the shared Relay pagination argument set used by
// every reactions: ReactionConnection! field (a subset of GitHub's args).
func reactionConnectionArgs() graphql.FieldConfigArgument {
	return graphql.FieldConfigArgument{
		"first":  &graphql.ArgumentConfig{Type: graphql.Int},
		"last":   &graphql.ArgumentConfig{Type: graphql.Int},
		"after":  &graphql.ArgumentConfig{Type: graphql.String},
		"before": &graphql.ArgumentConfig{Type: graphql.String},
	}
}

// addReactableFields attaches the reactions + viewerCanReact fields (bound to
// the subject's store parentType) to a concrete type so it satisfies Reactable.
// The type's config must also declare Interfaces: [reactable].
func (s *Resolver) addReactableFields(obj *graphql.Object, parentType string) {
	for name, f := range s.reactableFields(parentType) {
		obj.AddFieldConfig(name, f)
	}
}

// reactableDBID extracts a concrete subject's database id from its GraphQL
// source map, accepting either the public "databaseId" or the internal "_dbID".
func reactableDBID(src map[string]interface{}) int {
	if id, ok := src["databaseId"].(int); ok && id != 0 {
		return id
	}
	id, _ := src["_dbID"].(int)
	return id
}

// reactableFields returns the full Reactable interface field set (id,
// databaseId, reactionGroups, reactions, viewerCanReact) a concrete type adds to
// satisfy the interface, bound to its store parentType. addReactableFields
// installs them via AddFieldConfig, replacing any equivalent per-type field so
// every reactable type satisfies the interface uniformly. Source maps carry
// "nodeID" and either "databaseId" or "_dbID".
func (s *Resolver) reactableFields(parentType string) graphql.Fields {
	return graphql.Fields{
		"databaseId": &graphql.Field{
			Type: graphql.Int,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return reactableDBID(src), nil
			},
		},
		"reactionGroups": &graphql.Field{
			Type: graphql.NewList(graphql.NewNonNull(s.gqlReactionGroupType())),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return reactionGroupsForGraphQL(s.store.Reactions, parentType, reactableDBID(src), s.viewerReactorID(p.Context)), nil
			},
		},
		"reactions": &graphql.Field{
			Type: graphql.NewNonNull(s.graphqlTypes.reactionConnection),
			Args: reactionConnectionArgs(),
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				return discussionReactionConnection(s.store, parentType, reactableDBID(src), p.Args), nil
			},
		},
		"viewerCanReact": &graphql.Field{
			Type: graphql.NewNonNull(graphql.Boolean),
			// An authenticated viewer can react to a subject they can see; the
			// subject was reached through a readable connection, so authentication
			// is the operative check (mirrors the reaction mutation's read gate).
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return s.ghUserFromContext(p.Context) != nil, nil
			},
		},
	}
}

// viewerReactorID returns the signed-in viewer's user ID, or 0 when anonymous —
// used to compute reactionGroups.viewerHasReacted.
func (s *Resolver) viewerReactorID(ctx context.Context) int {
	if v := s.ghUserFromContext(ctx); v != nil {
		return v.ID
	}
	return 0
}

// graphQLToReactionContent maps the GraphQL ReactionContent enum back to the
// store's canonical content strings (the inverse of reactionContentToGraphQL).
func graphQLToReactionContent(enum string) string {
	switch enum {
	case "THUMBS_UP":
		return "+1"
	case "THUMBS_DOWN":
		return "-1"
	case "LAUGH":
		return "laugh"
	case "HOORAY":
		return "hooray"
	case "CONFUSED":
		return "confused"
	case "HEART":
		return "heart"
	case "ROCKET":
		return "rocket"
	case "EYES":
		return "eyes"
	}
	return ""
}

// reactableSubject is a resolved Reactable node: the store (parentType,
// databaseID) the reaction attaches to, plus the owning repo and author used
// for authorization.
type reactableSubject struct {
	parentType string
	parentID   int
	repo       *store.Repo
	authorID   int
}

// lookupReactableSubject decodes a Reactable node id to its store subject.
// GitHub's AddReactionInput.subjectId accepts all nine Reactable concrete types
// — Issue, PullRequest, PullRequestReview, Discussion, DiscussionComment,
// IssueComment, CommitComment, PullRequestReviewComment and Release — and
// bleephub resolves every one. The parentType strings mirror the REST reaction
// handlers so both API surfaces key the same store rows.
func (s *Resolver) lookupReactableSubject(nodeID string) (reactableSubject, bool) {
	if d := store.FindDiscussionByNodeID(s.store, nodeID); d != nil {
		return reactableSubject{"discussion", d.ID, s.store.GetRepoByID(d.RepoID), d.AuthorID}, true
	}
	if c := store.FindDiscussionCommentByNodeID(s.store, nodeID); c != nil {
		rs := reactableSubject{parentType: "discussion_comment", parentID: c.ID, authorID: c.AuthorID}
		if d := s.store.GetDiscussion(c.DiscussionID); d != nil {
			rs.repo = s.store.GetRepoByID(d.RepoID)
		}
		return rs, true
	}
	if i := store.FindIssueByNodeID(s.store, nodeID); i != nil {
		return reactableSubject{"issue", i.ID, s.store.GetRepoByID(i.RepoID), i.AuthorID}, true
	}
	if pr := store.FindPullRequestByNodeID(s.store, nodeID); pr != nil {
		return reactableSubject{"pull_request", pr.ID, s.store.GetRepoByID(pr.RepoID), pr.AuthorID}, true
	}
	if rv := store.FindReviewByNodeID(s.store, nodeID); rv != nil {
		rs := reactableSubject{parentType: "pull_request_review", parentID: rv.ID, authorID: rv.AuthorID}
		if pr := s.store.GetPullRequest(rv.PRID); pr != nil {
			rs.repo = s.store.GetRepoByID(pr.RepoID)
		}
		return rs, true
	}
	if c := store.FindIssueCommentByNodeID(s.store, nodeID); c != nil {
		rs := reactableSubject{parentType: "issue_comment", parentID: c.ID, authorID: c.AuthorID}
		if c.ParentType == "pull_request" {
			if pr := s.store.GetPullRequest(c.IssueID); pr != nil {
				rs.repo = s.store.GetRepoByID(pr.RepoID)
			}
		} else if iss := s.store.GetIssue(c.IssueID); iss != nil {
			rs.repo = s.store.GetRepoByID(iss.RepoID)
		}
		return rs, true
	}
	if c := store.FindCommitCommentByNodeID(s.store, nodeID); c != nil {
		return reactableSubject{"commit_comment", c.ID, s.store.GetRepoByID(c.RepoID), c.AuthorID}, true
	}
	if c := store.FindPullRequestReviewCommentByNodeID(s.store, nodeID); c != nil {
		rs := reactableSubject{parentType: "pull_request_review_comment", parentID: c.ID, authorID: c.AuthorID}
		if pr := s.store.GetPullRequest(c.PullRequestID); pr != nil {
			rs.repo = s.store.GetRepoByID(pr.RepoID)
		}
		return rs, true
	}
	if r := store.FindReleaseByNodeID(s.store, nodeID); r != nil {
		return reactableSubject{"release", r.ID, s.store.GetRepoByID(r.RepoID), r.AuthorID}, true
	}
	return reactableSubject{}, false
}

// reactableSubjectSource builds the concrete GraphQL source map for a reacted
// subject so the addReaction/removeReaction payload's subject: Reactable field
// resolves to a fully-populated concrete type. Returns nil if the subject no
// longer exists. Each branch calls the subject type's *ToGQL source builder;
// the Reactable interface's ResolveType then discriminates on the nodeID.
func (s *Resolver) reactableSubjectSource(parentType string, parentID int) map[string]interface{} {
	switch parentType {
	case "issue":
		if i := s.store.GetIssue(parentID); i != nil {
			return issueToGQL(i, s.store)
		}
	case "pull_request":
		if pr := s.store.GetPullRequest(parentID); pr != nil {
			return pullRequestToGQL(pr, s.store)
		}
	case "pull_request_review":
		if rv := s.store.GetPullRequestReview(parentID); rv != nil {
			return prReviewToGQL(rv, s.store)
		}
	case "discussion":
		if d := s.store.GetDiscussion(parentID); d != nil {
			return discussionToGQL(d, s.store)
		}
	case "discussion_comment":
		if c := s.store.GetDiscussionComment(parentID); c != nil {
			return discussionCommentToGQL(c, s.store)
		}
	case "issue_comment":
		if c := s.store.GetComment(parentID); c != nil {
			return commentToGQL(c, s.store)
		}
	case "commit_comment":
		if c := s.store.CommitComments.Get(parentID); c != nil {
			return commitCommentToGQL(c, s.store)
		}
	case "pull_request_review_comment":
		if c := s.store.PRReviewComments.Get(parentID); c != nil {
			return prReviewCommentToGQL(c, s.store)
		}
	case "release":
		if r := s.store.Releases.Get(parentID); r != nil {
			repoFullName := ""
			if repo := s.store.GetRepoByID(r.RepoID); repo != nil {
				repoFullName = repo.FullName
			}
			return releaseToGQL(r, 0, repoFullName, false)
		}
	}
	return nil
}

// resolveReactableSubject decodes a Reactable node id to its store
// (parentType, databaseID). ok=false if the id is not a supported subject.
func (s *Resolver) resolveReactableSubject(nodeID string) (string, int, bool) {
	rs, ok := s.lookupReactableSubject(nodeID)
	return rs.parentType, rs.parentID, ok
}

// reactableSubjectScope maps a resolved reaction subject to the app permission
// scope GitHub requires: issue reactions need issues:write, pull-request and
// review reactions need pull_requests:write, discussion reactions need
// discussions:write — never one blanket scope.
func reactableSubjectScope(parentType string) store.PermScope {
	switch parentType {
	case "issue", "issue_comment":
		return store.ScopeIssues
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		return store.ScopePullRequests
	case "commit_comment", "release":
		return store.ScopeContents
	default:
		return store.ScopeDiscussions
	}
}

// reactableScope resolves the subject in the input and returns its required
// scope, for the subject-typed scopeFor hook on the reaction authz rows.
func reactableScope(key string) func(*Resolver, map[string]interface{}) store.PermScope {
	return func(s *Resolver, input map[string]interface{}) store.PermScope {
		nodeID, _ := input[key].(string)
		if rs, ok := s.lookupReactableSubject(nodeID); ok {
			return reactableSubjectScope(rs.parentType)
		}
		return store.ScopeDiscussions
	}
}

// mutationTargetReactable authorizes a reaction against the repo owning the
// resolved subject.
func mutationTargetReactable(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		// `missing` stays set so the "can't read the repo" path in authorize
		// returns a non-nil error rather than silently authorizing.
		target := mutationTarget{missing: gqlMissingNode("Reactable", nodeID)}
		if rs, ok := s.lookupReactableSubject(nodeID); ok {
			target.repo = rs.repo
			target.authorID = rs.authorID
		}
		return target
	}
}

// addReactionMutationsToSchema registers addReaction/removeReaction. GitHub
// exposes reactions only over GraphQL; the discussion/comment reaction READ
// side (reactionGroups) already existed — this adds the write side.
func (s *Resolver) addReactionMutationsToSchema(mutationType *graphql.Object) {
	reactionContentEnum := s.graphQLEnum(
		"ReactionContent",
		"CONFUSED", "EYES", "HEART", "HOORAY", "LAUGH", "ROCKET", "THUMBS_DOWN", "THUMBS_UP",
	)
	// Payloads expose clientMutationId (registerMutation supplies it) plus
	// reactionGroups — the subject's reaction groups after the mutation, which
	// GitHub returns as [ReactionGroup!] and clients commonly select instead of
	// refetching the subject. The remaining official fields (reaction: Reaction,
	// subject: Reactable) still need a node-ID-bearing reaction and a Reactable
	// interface resolver; they stay absent (a subset the ratchet accepts).
	reactionGroupType := s.gqlReactionGroupType()
	reactionType := s.graphqlTypes.reaction
	inputFields := func() graphql.InputObjectConfigFieldMap {
		return graphql.InputObjectConfigFieldMap{
			"subjectId":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"content":          &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(reactionContentEnum)},
			"clientMutationId": &graphql.InputObjectFieldConfig{Type: graphql.String},
		}
	}
	payloadFields := func() graphql.Fields {
		return graphql.Fields{
			"clientMutationId": &graphql.Field{Type: graphql.String},
			"reaction": &graphql.Field{
				Type: reactionType,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, _ := p.Source.(map[string]interface{})
					return src["reaction"], nil
				},
			},
			"reactionGroups": &graphql.Field{
				Type: graphql.NewList(graphql.NewNonNull(reactionGroupType)),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, _ := p.Source.(map[string]interface{})
					parentType, _ := src["parentType"].(string)
					parentID, _ := src["parentID"].(int)
					return reactionGroupsForGraphQL(s.store.Reactions, parentType, parentID, s.viewerReactorID(p.Context)), nil
				},
			},
			"subject": &graphql.Field{
				Type: s.graphqlTypes.reactable,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, _ := p.Source.(map[string]interface{})
					parentType, _ := src["parentType"].(string)
					parentID, _ := src["parentID"].(int)
					if subj := s.reactableSubjectSource(parentType, parentID); subj != nil {
						return subj, nil
					}
					return nil, nil
				},
			},
		}
	}
	addInput := graphql.NewInputObject(graphql.InputObjectConfig{Name: "AddReactionInput", Fields: inputFields()})
	addPayload := graphql.NewObject(graphql.ObjectConfig{Name: "AddReactionPayload", Fields: payloadFields()})
	removeInput := graphql.NewInputObject(graphql.InputObjectConfig{Name: "RemoveReactionInput", Fields: inputFields()})
	removePayload := graphql.NewObject(graphql.ObjectConfig{Name: "RemoveReactionPayload", Fields: payloadFields()})

	s.registerMutation(mutationType, "addReaction", &graphql.Field{
		Type: addPayload,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(addInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			viewer := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			subjectID, _ := input["subjectId"].(string)
			contentEnum, _ := input["content"].(string)
			parentType, parentID, ok := s.resolveReactableSubject(subjectID)
			if !ok {
				return nil, gqlMissingNode("Reactable", subjectID)
			}
			created, _, err := s.store.Reactions.AddReaction(parentType, parentID, viewer.ID, graphQLToReactionContent(contentEnum))
			if err != nil {
				return nil, err
			}
			out := map[string]interface{}{"parentType": parentType, "parentID": parentID}
			if created != nil {
				out["reaction"] = reactionNodeToGraphQL(s.store, created)
			}
			return out, nil
		},
	})

	s.registerMutation(mutationType, "removeReaction", &graphql.Field{
		Type: removePayload,
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(removeInput)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			viewer := s.ghUserFromContext(p.Context)
			input, _ := p.Args["input"].(map[string]interface{})
			subjectID, _ := input["subjectId"].(string)
			contentEnum, _ := input["content"].(string)
			parentType, parentID, ok := s.resolveReactableSubject(subjectID)
			if !ok {
				return nil, gqlMissingNode("Reactable", subjectID)
			}
			content := graphQLToReactionContent(contentEnum)
			out := map[string]interface{}{"parentType": parentType, "parentID": parentID}
			for _, r := range s.store.Reactions.ListReactions(parentType, parentID, content) {
				if r.UserID == viewer.ID {
					// The payload's reaction is the one that was removed.
					out["reaction"] = reactionNodeToGraphQL(s.store, r)
					s.store.Reactions.DeleteReactionByUser(parentType, parentID, r.ID, viewer.ID)
					break
				}
			}
			return out, nil
		},
	})
}

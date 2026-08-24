package graphqlapi

import (
	"context"
	"fmt"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// The shared Comment/UpdatableComment/Minimizable/Deletable trait fields GitHub
// puts on every concrete comment type. CommitComment, IssueComment, GistComment
// and PullRequestReviewComment each carry the same content projections
// (bodyHTML/bodyText), the same edit-history surface (editor/lastEditedAt/
// includesCreatedEdit/userContentEdits), the same minimize surface, and the
// same viewer-permission family. Rendering them from one place keeps the four
// types answering identically and truthfully:
//
//   - bleephub records no per-edit diff history, so userContentEdits is a real
//     empty connection (never invented edits) and, on the types that do not
//     model edits at all, editor/lastEditedAt/includesCreatedEdit resolve to
//     GitHub's zero values.
//   - createdViaEmail is legitimately always false: bleephub has no
//     email-reply ingestion path, so no comment was ever created via one.
//   - the viewerCan* family answers from real repository (or gist) permission,
//     so a viewer without write access — or a stranger to a private repo — is
//     told the truth (false) rather than leaked a capability they do not hold.
//
// The authz-dependent fields resolve the backing store record from the source
// map's node id rather than trusting pre-baked source keys, so every producer
// of a comment source (the *ToGQL builders and the review-thread inline
// builder) gains these fields without being touched.

// commentAuthz is the repository (or gist) and author backing a comment source,
// resolved once per field so the viewerCan* family and the repository/parent
// back-references answer from real ownership. repo is nil for gist comments,
// which carry gistOwnerID instead.
type commentAuthz struct {
	repo        *store.Repo
	authorID    int
	gistOwnerID int
	isGist      bool
}

// ---- shared enums -----------------------------------------------------------

// commentAuthorAssociationEnum is GitHub's CommentAuthorAssociation, memoized by
// graphQLEnum so the four comment types share the one enum the IssueComment type
// first minted.
func (s *Resolver) commentAuthorAssociationEnum() *graphql.Enum {
	return s.graphQLEnum(
		"CommentAuthorAssociation",
		"COLLABORATOR", "CONTRIBUTOR", "FIRST_TIMER", "FIRST_TIME_CONTRIBUTOR",
		"MANNEQUIN", "MEMBER", "NONE", "OWNER",
	)
}

// gqlCommentCannotUpdateReasonEnum is GitHub's CommentCannotUpdateReason, the
// enum viewerCannotUpdateReasons returns. graphQLEnum memoizes it by name, so it
// is the single shared enum regardless of which family first references it.
func (s *Resolver) gqlCommentCannotUpdateReasonEnum() *graphql.Enum {
	return s.graphQLEnum(
		"CommentCannotUpdateReason",
		"ARCHIVED", "DENIED", "INSUFFICIENT_ACCESS", "LOCKED",
		"LOGIN_REQUIRED", "MAINTENANCE", "VERIFIED_EMAIL_REQUIRED",
	)
}

// ---- viewer permission logic ------------------------------------------------

// commentViewerIsAuthor reports whether the signed-in viewer wrote the comment.
func (s *Resolver) commentViewerIsAuthor(ctx context.Context, az commentAuthz) bool {
	v := s.ghUserFromContext(ctx)
	return v != nil && az.authorID != 0 && v.ID == az.authorID
}

// commentViewerCanModerate reports whether the viewer holds the standing that
// lets them act on somebody else's comment: repository write for a repo comment,
// gist ownership for a gist comment.
func (s *Resolver) commentViewerCanModerate(ctx context.Context, az commentAuthz) bool {
	if az.isGist {
		v := s.ghUserFromContext(ctx)
		return v != nil && az.gistOwnerID != 0 && v.ID == az.gistOwnerID
	}
	return az.repo != nil && s.viewerCanPushRepo(ctx, az.repo)
}

// commentViewerCanUpdate reports whether the viewer may edit the comment: its
// author, or a moderator of the surrounding repository/gist.
func (s *Resolver) commentViewerCanUpdate(ctx context.Context, az commentAuthz) bool {
	return s.commentViewerIsAuthor(ctx, az) || s.commentViewerCanModerate(ctx, az)
}

// commentCannotUpdateReasons is the real reason list behind viewerCanUpdate: an
// empty list when the viewer may update, LOGIN_REQUIRED for an anonymous
// viewer, INSUFFICIENT_ACCESS for a signed-in viewer without the standing.
func (s *Resolver) commentCannotUpdateReasons(ctx context.Context, az commentAuthz) []interface{} {
	if s.ghUserFromContext(ctx) == nil {
		return []interface{}{"LOGIN_REQUIRED"}
	}
	if s.commentViewerCanUpdate(ctx, az) {
		return []interface{}{}
	}
	return []interface{}{"INSUFFICIENT_ACCESS"}
}

// repoCommentAssociation computes a repo comment's authorAssociation under the
// store lock authorAssociationForRepoLocked requires.
func (s *Resolver) repoCommentAssociation(repoID, authorID int) string {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return authorAssociationForRepoLocked(s.store, repoID, authorID)
}

// ---- source helpers ---------------------------------------------------------

func commentSourceMap(source interface{}) map[string]interface{} {
	m, _ := source.(map[string]interface{})
	return m
}

func commentSourceString(source interface{}, key string) string {
	v, _ := commentSourceMap(source)[key].(string)
	return v
}

// ---- content-trait field constructors --------------------------------------

// commentBodyHTMLField renders the markdown body to HTML (HTML!).
func (s *Resolver) commentBodyHTMLField() *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(s.graphQLStringScalar("HTML")),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return discussionBodyToHTML(commentSourceString(p.Source, "body")), nil
		},
	}
}

// commentBodyTextField renders the markdown body to plain text (String!).
func (s *Resolver) commentBodyTextField() *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.String),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return bodyText(commentSourceString(p.Source, "body")), nil
		},
	}
}

// commentCreatedViaEmailField is GitHub's createdViaEmail: always false, because
// bleephub has no email-reply ingestion path.
func commentCreatedViaEmailField() *graphql.Field {
	return &graphql.Field{
		Type:    graphql.NewNonNull(graphql.Boolean),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return false, nil },
	}
}

// commentPublishedAtField is publishedAt: the moment a published comment became
// visible, which for bleephub — where a comment is public the instant it is
// created — equals createdAt.
func (s *Resolver) commentPublishedAtField() *graphql.Field {
	return &graphql.Field{
		Type: s.graphQLStringScalar("DateTime"),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			if v := commentSourceString(p.Source, "createdAt"); v != "" {
				return v, nil
			}
			return nil, nil
		},
	}
}

// commentUserContentEditsField is the edit-history connection. bleephub records
// no per-edit diff history, so it is a real, well-formed empty connection.
func (s *Resolver) commentUserContentEditsField() *graphql.Field {
	return &graphql.Field{
		Type: s.gqlUserContentEditConnectionType(),
		Args: graphql.FieldConfigArgument{
			"after":  &graphql.ArgumentConfig{Type: graphql.String},
			"before": &graphql.ArgumentConfig{Type: graphql.String},
			"first":  &graphql.ArgumentConfig{Type: graphql.Int},
			"last":   &graphql.ArgumentConfig{Type: graphql.Int},
		},
		Resolve: func(graphql.ResolveParams) (interface{}, error) {
			return emptyUserContentEditConnection(), nil
		},
	}
}

// commentZeroBoolField is a Boolean! that resolves false — the truthful answer
// for an edit/minimize flag on a type bleephub does not model that state for.
func commentZeroBoolField() *graphql.Field {
	return &graphql.Field{
		Type:    graphql.NewNonNull(graphql.Boolean),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return false, nil },
	}
}

// commentNullField is a nullable member that resolves null — the truthful
// answer for an editor / lastEditedAt / minimizedReason a type does not track.
func commentNullField(t graphql.Output) *graphql.Field {
	return &graphql.Field{
		Type:    t,
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	}
}

// commentViewerBoolField resolves a viewerCan*/viewerDidAuthor predicate against
// the real authz context extracted from the source.
func (s *Resolver) commentViewerBoolField(extract func(interface{}) commentAuthz, pred func(context.Context, commentAuthz) bool) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return pred(p.Context, extract(p.Source)), nil
		},
	}
}

// commentCannotUpdateReasonsField resolves the real
// [CommentCannotUpdateReason!]! list.
func (s *Resolver) commentCannotUpdateReasonsField(extract func(interface{}) commentAuthz) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(s.gqlCommentCannotUpdateReasonEnum()))),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.commentCannotUpdateReasons(p.Context, extract(p.Source)), nil
		},
	}
}

// commentAuthorAssociationField resolves authorAssociation from the real repo
// (or gist) membership.
func (s *Resolver) commentAuthorAssociationField(extract func(interface{}) commentAuthz) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(s.commentAuthorAssociationEnum()),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			az := extract(p.Source)
			if az.isGist {
				if az.gistOwnerID != 0 && az.authorID == az.gistOwnerID {
					return "OWNER", nil
				}
				return "NONE", nil
			}
			if az.repo == nil {
				return "NONE", nil
			}
			return s.repoCommentAssociation(az.repo.ID, az.authorID), nil
		},
	}
}

// commentRepositoryField resolves the repository: Repository! back-reference,
// gated so a private repo never leaks to a viewer without read access.
func (s *Resolver) commentRepositoryField(extract func(interface{}) commentAuthz) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.repository),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			az := extract(p.Source)
			if az.repo == nil {
				return nil, nil
			}
			if az.repo.Private && !s.viewerCanReadRepo(p.Context, az.repo) {
				return nil, nil
			}
			return repoToGraphQL(s.store, az.repo), nil
		},
	}
}

// commentPathField resolves a URI! whose value is the given path builder's
// result (resourcePath is the path, url is externalURL of the same path).
func (s *Resolver) commentPathField(build func(interface{}) string, external bool) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(s.graphQLStringScalar("URI")),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			path := build(p.Source)
			if external {
				return externalURL(path), nil
			}
			return path, nil
		},
	}
}

// ============================================================================
// CommitComment
// ============================================================================

// commitCommentAuthz resolves a commit comment's repo + author from its node id.
func (s *Resolver) commitCommentAuthz(source interface{}) commentAuthz {
	c := store.FindCommitCommentByNodeID(s.store, commentSourceString(source, "nodeID"))
	if c == nil {
		return commentAuthz{}
	}
	return commentAuthz{repo: s.store.GetRepoByID(c.RepoID), authorID: c.AuthorID}
}

// commitCommentResourcePath builds /{owner}/{repo}/commit/{sha}#commitcomment-{id}.
func (s *Resolver) commitCommentResourcePath(source interface{}) string {
	c := store.FindCommitCommentByNodeID(s.store, commentSourceString(source, "nodeID"))
	if c == nil {
		return ""
	}
	repo := s.store.GetRepoByID(c.RepoID)
	if repo == nil {
		return ""
	}
	return fmt.Sprintf("/%s/commit/%s#commitcomment-%d", repo.FullName, c.CommitID, c.ID)
}

// enrichCommitCommentType installs the 23 Comment-trait fields CommitComment was
// missing.
func (s *Resolver) enrichCommitCommentType() {
	obj := s.graphqlTypes.commitComment
	if obj == nil {
		return
	}
	extract := s.commitCommentAuthz
	dateTime := s.graphQLStringScalar("DateTime")

	obj.AddFieldConfig("bodyHTML", s.commentBodyHTMLField())
	obj.AddFieldConfig("bodyText", s.commentBodyTextField())
	obj.AddFieldConfig("createdViaEmail", commentCreatedViaEmailField())
	obj.AddFieldConfig("publishedAt", s.commentPublishedAtField())
	obj.AddFieldConfig("userContentEdits", s.commentUserContentEditsField())
	obj.AddFieldConfig("authorAssociation", s.commentAuthorAssociationField(extract))
	// bleephub does not model commit-comment edits or minimization.
	obj.AddFieldConfig("editor", commentNullField(s.graphqlTypes.actor))
	obj.AddFieldConfig("lastEditedAt", commentNullField(dateTime))
	obj.AddFieldConfig("includesCreatedEdit", commentZeroBoolField())
	obj.AddFieldConfig("isMinimized", commentZeroBoolField())
	obj.AddFieldConfig("minimizedReason", commentNullField(graphql.String))
	// updatedAt and position come off the store record.
	obj.AddFieldConfig("updatedAt", &graphql.Field{
		Type: graphql.NewNonNull(dateTime),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			c := store.FindCommitCommentByNodeID(s.store, commentSourceString(p.Source, "nodeID"))
			if c == nil {
				return commentSourceString(p.Source, "createdAt"), nil
			}
			return c.UpdatedAt.Format(time.RFC3339), nil
		},
	})
	obj.AddFieldConfig("position", &graphql.Field{
		Type: graphql.Int,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			c := store.FindCommitCommentByNodeID(s.store, commentSourceString(p.Source, "nodeID"))
			if c == nil || c.Position == nil {
				return nil, nil
			}
			return *c.Position, nil
		},
	})
	obj.AddFieldConfig("commit", &graphql.Field{
		Type: s.graphqlTypes.commit,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			c := store.FindCommitCommentByNodeID(s.store, commentSourceString(p.Source, "nodeID"))
			if c == nil {
				return nil, nil
			}
			repo := s.store.GetRepoByID(c.RepoID)
			if repo == nil {
				return nil, nil
			}
			return optionalObject(s.timelineCommitSource(repo, c.CommitID)), nil
		},
	})
	obj.AddFieldConfig("repository", s.commentRepositoryField(extract))
	obj.AddFieldConfig("resourcePath", s.commentPathField(s.commitCommentResourcePath, false))
	obj.AddFieldConfig("url", s.commentPathField(s.commitCommentResourcePath, true))
	obj.AddFieldConfig("viewerDidAuthor", s.commentViewerBoolField(extract, s.commentViewerIsAuthor))
	obj.AddFieldConfig("viewerCanUpdate", s.commentViewerBoolField(extract, s.commentViewerCanUpdate))
	obj.AddFieldConfig("viewerCanDelete", s.commentViewerBoolField(extract, s.commentViewerCanUpdate))
	obj.AddFieldConfig("viewerCanMinimize", s.commentViewerBoolField(extract, s.commentViewerCanModerate))
	obj.AddFieldConfig("viewerCanUnminimize", s.commentViewerBoolField(extract, s.commentViewerCanModerate))
	obj.AddFieldConfig("viewerCannotUpdateReasons", s.commentCannotUpdateReasonsField(extract))
}

// ============================================================================
// IssueComment
// ============================================================================

// issueCommentContext resolves an issue comment's record and its repo.
func (s *Resolver) issueCommentContext(source interface{}) (*store.Comment, *store.Repo) {
	c := store.FindIssueCommentByNodeID(s.store, commentSourceString(source, "nodeID"))
	if c == nil {
		return nil, nil
	}
	var repo *store.Repo
	if c.ParentType == "pull_request" {
		if pr := s.store.GetPullRequest(c.IssueID); pr != nil {
			repo = s.store.GetRepoByID(pr.RepoID)
		}
	} else if iss := s.store.GetIssue(c.IssueID); iss != nil {
		repo = s.store.GetRepoByID(iss.RepoID)
	}
	return c, repo
}

func (s *Resolver) issueCommentAuthz(source interface{}) commentAuthz {
	c, repo := s.issueCommentContext(source)
	if c == nil {
		return commentAuthz{}
	}
	return commentAuthz{repo: repo, authorID: c.AuthorID}
}

func (s *Resolver) issueCommentResourcePath(source interface{}) string {
	c, repo := s.issueCommentContext(source)
	if c == nil || repo == nil {
		return ""
	}
	number, lane := 0, "issues"
	if c.ParentType == "pull_request" {
		if pr := s.store.GetPullRequest(c.IssueID); pr != nil {
			number, lane = pr.Number, "pull"
		}
	} else if iss := s.store.GetIssue(c.IssueID); iss != nil {
		number = iss.Number
	}
	if number == 0 {
		return ""
	}
	return fmt.Sprintf("/%s/%s/%d#issuecomment-%d", repo.FullName, lane, number, c.ID)
}

// enrichIssueCommentType installs the 19 fields IssueComment was missing. The
// edit/minimize surface already lives on the type (from the Comment store
// record), so only the content projections, back-references, the pin surface
// and the viewer family are added here.
func (s *Resolver) enrichIssueCommentType() {
	obj := s.graphqlTypes.issueComment
	if obj == nil {
		return
	}
	extract := s.issueCommentAuthz
	dateTime := s.graphQLStringScalar("DateTime")

	obj.AddFieldConfig("bodyHTML", s.commentBodyHTMLField())
	obj.AddFieldConfig("bodyText", s.commentBodyTextField())
	obj.AddFieldConfig("createdViaEmail", commentCreatedViaEmailField())
	obj.AddFieldConfig("publishedAt", s.commentPublishedAtField())
	obj.AddFieldConfig("userContentEdits", s.commentUserContentEditsField())
	obj.AddFieldConfig("fullDatabaseId", &graphql.Field{
		Type: s.graphQLStringScalar("BigInt"),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			id := reactableDBID(commentSourceMap(p.Source))
			if id == 0 {
				return nil, nil
			}
			return id, nil
		},
	})
	obj.AddFieldConfig("repository", s.commentRepositoryField(extract))
	obj.AddFieldConfig("resourcePath", s.commentPathField(s.issueCommentResourcePath, false))
	obj.AddFieldConfig("issue", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.issue),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			c, _ := s.issueCommentContext(p.Source)
			if c == nil || c.ParentType == "pull_request" {
				// A pull-request conversation comment has no backing Issue row
				// in bleephub (PRs and issues share only the per-repo number
				// counter), so there is nothing truthful to return here.
				return nil, nil
			}
			iss := s.store.GetIssue(c.IssueID)
			return optionalRendered(iss, func(i *store.Issue) map[string]interface{} { return issueToGQL(i, s.store) }), nil
		},
	})
	obj.AddFieldConfig("pullRequest", &graphql.Field{
		Type: s.graphqlTypes.pullRequest,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			c, _ := s.issueCommentContext(p.Source)
			if c == nil || c.ParentType != "pull_request" {
				return nil, nil
			}
			pr := s.store.GetPullRequest(c.IssueID)
			if pr == nil {
				return nil, nil
			}
			return pullRequestToGQL(pr, s.store), nil
		},
	})
	// bleephub records the pin flag (isPinned already resolves it) but not who
	// pinned or when, so pinnedAt/pinnedBy are the truthful null.
	obj.AddFieldConfig("pinnedAt", commentNullField(dateTime))
	obj.AddFieldConfig("pinnedBy", commentNullField(s.graphqlTypes.user))
	obj.AddFieldConfig("viewerDidAuthor", s.commentViewerBoolField(extract, s.commentViewerIsAuthor))
	obj.AddFieldConfig("viewerCanUpdate", s.commentViewerBoolField(extract, s.commentViewerCanUpdate))
	obj.AddFieldConfig("viewerCanDelete", s.commentViewerBoolField(extract, s.commentViewerCanUpdate))
	obj.AddFieldConfig("viewerCanMinimize", s.commentViewerBoolField(extract, s.commentViewerCanModerate))
	obj.AddFieldConfig("viewerCanUnminimize", s.commentViewerBoolField(extract, s.commentViewerCanModerate))
	obj.AddFieldConfig("viewerCanPin", s.commentViewerBoolField(extract, s.commentViewerCanModerate))
	obj.AddFieldConfig("viewerCanUnpin", s.commentViewerBoolField(extract, s.commentViewerCanModerate))
	obj.AddFieldConfig("viewerCannotUpdateReasons", s.commentCannotUpdateReasonsField(extract))
}

// ============================================================================
// PullRequestReviewComment
// ============================================================================

func (s *Resolver) prrcContext(source interface{}) (*store.PRReviewComment, *store.PullRequest, *store.Repo) {
	c := store.FindPullRequestReviewCommentByNodeID(s.store, commentSourceString(source, "nodeID"))
	if c == nil {
		return nil, nil, nil
	}
	pr := s.store.GetPullRequest(c.PullRequestID)
	var repo *store.Repo
	if pr != nil {
		repo = s.store.GetRepoByID(pr.RepoID)
	}
	return c, pr, repo
}

func (s *Resolver) prrcAuthz(source interface{}) commentAuthz {
	c, _, repo := s.prrcContext(source)
	if c == nil {
		return commentAuthz{}
	}
	return commentAuthz{repo: repo, authorID: c.AuthorID}
}

func (s *Resolver) prrcResourcePath(source interface{}) string {
	c, pr, repo := s.prrcContext(source)
	if c == nil || pr == nil || repo == nil {
		return ""
	}
	return fmt.Sprintf("/%s/pull/%d#discussion_r%d", repo.FullName, pr.Number, c.ID)
}

// enrichPullRequestReviewCommentType installs the 33 fields the review-comment
// type was missing: the Comment trait, the diff-position family, the commit and
// pull-request back-references, and the viewer family.
func (s *Resolver) enrichPullRequestReviewCommentType() {
	obj := s.graphqlTypes.pullRequestReviewComment
	if obj == nil {
		return
	}
	extract := s.prrcAuthz
	dateTime := s.graphQLStringScalar("DateTime")

	obj.AddFieldConfig("bodyHTML", s.commentBodyHTMLField())
	obj.AddFieldConfig("bodyText", s.commentBodyTextField())
	obj.AddFieldConfig("createdViaEmail", commentCreatedViaEmailField())
	obj.AddFieldConfig("publishedAt", s.commentPublishedAtField())
	obj.AddFieldConfig("userContentEdits", s.commentUserContentEditsField())
	obj.AddFieldConfig("authorAssociation", s.commentAuthorAssociationField(extract))
	// bleephub does not model review-comment edits or minimization.
	obj.AddFieldConfig("editor", commentNullField(s.graphqlTypes.actor))
	obj.AddFieldConfig("lastEditedAt", commentNullField(dateTime))
	obj.AddFieldConfig("includesCreatedEdit", commentZeroBoolField())
	obj.AddFieldConfig("isMinimized", commentZeroBoolField())
	obj.AddFieldConfig("minimizedReason", commentNullField(graphql.String))
	obj.AddFieldConfig("fullDatabaseId", &graphql.Field{
		Type: s.graphQLStringScalar("BigInt"),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			id := reactableDBID(commentSourceMap(p.Source))
			if id == 0 {
				return nil, nil
			}
			return id, nil
		},
	})
	obj.AddFieldConfig("draftedAt", &graphql.Field{
		Type: graphql.NewNonNull(dateTime),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			// A submitted comment was drafted when it was created.
			if v := commentSourceString(p.Source, "createdAt"); v != "" {
				return v, nil
			}
			return nil, nil
		},
	})
	obj.AddFieldConfig("startLine", s.prrcIntPtrField(func(c *store.PRReviewComment) *int { return c.StartLine }))
	obj.AddFieldConfig("originalLine", s.prrcIntPtrField(func(c *store.PRReviewComment) *int { return c.OriginalLine }))
	obj.AddFieldConfig("originalStartLine", s.prrcIntPtrField(func(c *store.PRReviewComment) *int { return c.OriginalStartLine }))
	obj.AddFieldConfig("originalPosition", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Int),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			c, _, _ := s.prrcContext(p.Source)
			if c == nil || c.OriginalPosition == nil {
				return 0, nil
			}
			return *c.OriginalPosition, nil
		},
	})
	obj.AddFieldConfig("outdated", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			c, _, _ := s.prrcContext(p.Source)
			// A comment whose diff position no longer maps to the current diff
			// is outdated; bleephub clears Position when that happens.
			return c != nil && c.Position == nil, nil
		},
	})
	obj.AddFieldConfig("subjectType", &graphql.Field{
		Type: graphql.NewNonNull(s.sharedEnum("PullRequestReviewThreadSubjectType", "FILE", "LINE")),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			c, _, _ := s.prrcContext(p.Source)
			if c != nil && c.Line == nil {
				return "FILE", nil
			}
			return "LINE", nil
		},
	})
	obj.AddFieldConfig("commit", s.prrcCommitField(func(c *store.PRReviewComment) string { return c.CommitID }))
	obj.AddFieldConfig("originalCommit", s.prrcCommitField(func(c *store.PRReviewComment) string { return c.OriginalCommitID }))
	obj.AddFieldConfig("replyTo", &graphql.Field{
		Type: obj,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			c, _, _ := s.prrcContext(p.Source)
			if c == nil || c.InReplyToID == 0 {
				return nil, nil
			}
			parent := s.store.PRReviewComments.Get(c.InReplyToID)
			if parent == nil {
				return nil, nil
			}
			return optionalObject(prReviewCommentToGQL(parent, s.store)), nil
		},
	})
	obj.AddFieldConfig("pullRequest", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.pullRequest),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			_, pr, _ := s.prrcContext(p.Source)
			if pr == nil {
				return nil, nil
			}
			return pullRequestToGQL(pr, s.store), nil
		},
	})
	obj.AddFieldConfig("pullRequestReview", &graphql.Field{
		Type: s.graphqlTypes.pullRequestReview,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			c, _, _ := s.prrcContext(p.Source)
			if c == nil || c.ReviewID == 0 {
				return nil, nil
			}
			rv := s.store.GetPullRequestReview(c.ReviewID)
			if rv == nil {
				return nil, nil
			}
			return prReviewToGQL(rv, s.store), nil
		},
	})
	obj.AddFieldConfig("repository", s.commentRepositoryField(extract))
	obj.AddFieldConfig("resourcePath", s.commentPathField(s.prrcResourcePath, false))
	obj.AddFieldConfig("url", s.commentPathField(s.prrcResourcePath, true))
	obj.AddFieldConfig("viewerDidAuthor", s.commentViewerBoolField(extract, s.commentViewerIsAuthor))
	obj.AddFieldConfig("viewerCanUpdate", s.commentViewerBoolField(extract, s.commentViewerCanUpdate))
	obj.AddFieldConfig("viewerCanDelete", s.commentViewerBoolField(extract, s.commentViewerCanUpdate))
	obj.AddFieldConfig("viewerCanMinimize", s.commentViewerBoolField(extract, s.commentViewerCanModerate))
	obj.AddFieldConfig("viewerCanUnminimize", s.commentViewerBoolField(extract, s.commentViewerCanModerate))
	obj.AddFieldConfig("viewerCannotUpdateReasons", s.commentCannotUpdateReasonsField(extract))
}

// prrcIntPtrField resolves a nullable Int off the review-comment record.
func (s *Resolver) prrcIntPtrField(pick func(*store.PRReviewComment) *int) *graphql.Field {
	return &graphql.Field{
		Type: graphql.Int,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			c, _, _ := s.prrcContext(p.Source)
			if c == nil {
				return nil, nil
			}
			if v := pick(c); v != nil {
				return *v, nil
			}
			return nil, nil
		},
	}
}

// prrcCommitField resolves a nullable Commit off a sha picked from the record.
func (s *Resolver) prrcCommitField(pick func(*store.PRReviewComment) string) *graphql.Field {
	return &graphql.Field{
		Type: s.graphqlTypes.commit,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			c, _, repo := s.prrcContext(p.Source)
			if c == nil || repo == nil {
				return nil, nil
			}
			return optionalObject(s.timelineCommitSource(repo, pick(c))), nil
		},
	}
}

// ============================================================================
// GistComment
// ============================================================================

// addGistCommentFields installs the 19 Comment-trait fields GistComment was
// missing. It is called from gqlGistCommentType, after the gist and repository
// families exist, and reads the gistID/authorID keys the gist-comment source
// carries.
func (s *Resolver) addGistCommentFields(obj *graphql.Object) {
	extract := s.gistCommentAuthz
	dateTime := s.graphQLStringScalar("DateTime")

	obj.AddFieldConfig("bodyHTML", s.commentBodyHTMLField())
	obj.AddFieldConfig("bodyText", s.commentBodyTextField())
	obj.AddFieldConfig("createdViaEmail", commentCreatedViaEmailField())
	obj.AddFieldConfig("publishedAt", s.commentPublishedAtField())
	obj.AddFieldConfig("userContentEdits", s.commentUserContentEditsField())
	obj.AddFieldConfig("authorAssociation", s.commentAuthorAssociationField(extract))
	obj.AddFieldConfig("databaseId", &graphql.Field{
		Type: graphql.Int,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			id := reactableDBID(commentSourceMap(p.Source))
			if id == 0 {
				return nil, nil
			}
			return id, nil
		},
	})
	// bleephub does not model gist-comment edits or minimization.
	obj.AddFieldConfig("editor", commentNullField(s.graphqlTypes.actor))
	obj.AddFieldConfig("lastEditedAt", commentNullField(dateTime))
	obj.AddFieldConfig("includesCreatedEdit", commentZeroBoolField())
	obj.AddFieldConfig("isMinimized", commentZeroBoolField())
	obj.AddFieldConfig("minimizedReason", commentNullField(graphql.String))
	obj.AddFieldConfig("gist", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlGistType()),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			gistID := commentSourceString(p.Source, "gistID")
			gist := s.store.GetGist(gistID)
			if gist == nil {
				return nil, nil
			}
			owner := optionalRendered(s.store.GetUserByID(gist.OwnerID), userToGraphQL)
			ownerMap, _ := owner.(map[string]interface{})
			return gistToGQL(gist, ownerMap), nil
		},
	})
	obj.AddFieldConfig("viewerDidAuthor", s.commentViewerBoolField(extract, s.commentViewerIsAuthor))
	obj.AddFieldConfig("viewerCanUpdate", s.commentViewerBoolField(extract, s.commentViewerCanUpdate))
	obj.AddFieldConfig("viewerCanDelete", s.commentViewerBoolField(extract, s.commentViewerCanUpdate))
	obj.AddFieldConfig("viewerCanMinimize", s.commentViewerBoolField(extract, s.commentViewerCanModerate))
	obj.AddFieldConfig("viewerCanUnminimize", s.commentViewerBoolField(extract, s.commentViewerCanModerate))
	obj.AddFieldConfig("viewerCannotUpdateReasons", s.commentCannotUpdateReasonsField(extract))
}

// gistCommentAuthz resolves a gist comment's owner + author from the gistID and
// authorID keys the source carries.
func (s *Resolver) gistCommentAuthz(source interface{}) commentAuthz {
	m := commentSourceMap(source)
	authorID, _ := m["authorID"].(int)
	az := commentAuthz{isGist: true, authorID: authorID}
	if gist := s.store.GetGist(commentSourceString(source, "gistID")); gist != nil {
		az.gistOwnerID = gist.OwnerID
	}
	return az
}

// enrichCommentTypes installs the shared Comment-trait, back-reference and
// viewer-permission fields on the comment types built before the repository,
// issue and pull-request families existed. GistComment is enriched in place by
// gqlGistCommentType, which runs after those families. Called once from
// initGraphQLSchema after every referenced type is assembled.
func (s *Resolver) enrichCommentTypes() {
	s.enrichCommitCommentType()
	s.enrichIssueCommentType()
	s.enrichPullRequestReviewCommentType()
}

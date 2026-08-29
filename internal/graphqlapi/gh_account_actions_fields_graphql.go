package graphqlapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/graphql-go/graphql"
)

// addAccountActionsFields hangs the residual Commit, Release and status-rollup
// members onto the already-built types. Runs after every type family is
// assembled.
func (s *Resolver) addAccountActionsFields() {
	s.addCommitAccountFields()
	s.addStatusContextFields()
	s.addStatusCheckRollupFields()
	s.addReleaseAccountFields()
}

// Commit

func (s *Resolver) addCommitAccountFields() {
	commitType := s.graphqlTypes.commit
	if commitType == nil {
		return
	}
	html := s.graphQLStringScalar("HTML")
	dateTime := s.graphQLStringScalar("DateTime")

	commitType.AddFieldConfig("messageHeadlineHTML", &graphql.Field{
		Type:    graphql.NewNonNull(html),
		Resolve: commitStringSourceHTML("messageHeadline"),
	})
	commitType.AddFieldConfig("messageBodyHTML", &graphql.Field{
		Type:    graphql.NewNonNull(html),
		Resolve: commitStringSourceHTML("messageBody"),
	})
	// No commit is authored through a web editor here, so the honest value is false.
	commitType.AddFieldConfig("committedViaWeb", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.Boolean),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return false, nil },
	})
	// pushedDate is removed upstream (null). The deprecation reason must match the
	// vendored SDL exactly for the schema-parity check.
	commitType.AddFieldConfig("pushedDate", &graphql.Field{
		Type:              dateTime,
		DeprecationReason: "`pushedDate` is no longer supported. Removal on 2023-07-01 UTC.",
		Resolve:           func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})
	// No commit is attributed to an app acting on an org's behalf (null).
	commitType.AddFieldConfig("onBehalfOf", &graphql.Field{
		Type:    s.graphqlTypes.organization,
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})
}

// commitStringSourceHTML renders a commit source string member as HTML.
func commitStringSourceHTML(key string) graphql.FieldResolveFn {
	return func(p graphql.ResolveParams) (interface{}, error) {
		src, ok := p.Source.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
		}
		text, _ := src[key].(string)
		return discussionBodyToHTML(text), nil
	}
}

// StatusContext

func (s *Resolver) addStatusContextFields() {
	statusContextType := s.graphqlTypes.statusContext
	if statusContextType == nil {
		return
	}
	uri := s.graphQLStringScalar("URI")
	dateTime := s.graphQLStringScalar("DateTime")

	statusContextType.AddFieldConfig("id", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.ID),
		Resolve: statusContextSourceField("nodeID"),
	})
	statusContextType.AddFieldConfig("updatedAt", &graphql.Field{
		Type:    graphql.NewNonNull(dateTime),
		Resolve: statusContextSourceField("updatedAt"),
	})
	statusContextType.AddFieldConfig("creator", &graphql.Field{
		Type:    s.graphqlTypes.actor,
		Resolve: s.statusContextCreator,
	})
	statusContextType.AddFieldConfig("avatarUrl", &graphql.Field{
		Type: uri,
		Args: graphql.FieldConfigArgument{
			"size": &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 40},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			creator, err := s.statusContextCreator(p)
			if err != nil || creator == nil {
				return nil, err
			}
			user, _ := creator.(map[string]interface{})
			if user == nil {
				return nil, nil
			}
			return user["avatarUrl"], nil
		},
	})
	statusContextType.AddFieldConfig("commit", &graphql.Field{
		Type: s.graphqlTypes.commit,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.commitSourceFromStatus(p.Context, p.Source), nil
		},
	})
}

// statusContextCreator resolves the account that posted the commit status.
func (s *Resolver) statusContextCreator(p graphql.ResolveParams) (interface{}, error) {
	src, ok := p.Source.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
	}
	creatorID, _ := src["creatorID"].(int)
	if creatorID == 0 {
		return nil, nil
	}
	return optionalRendered(s.store.GetUserByID(creatorID), userToGraphQL), nil
}

func statusContextSourceField(key string) graphql.FieldResolveFn {
	return func(p graphql.ResolveParams) (interface{}, error) {
		src, ok := p.Source.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
		}
		return src[key], nil
	}
}

// StatusCheckRollup

func (s *Resolver) addStatusCheckRollupFields() {
	rollupType := s.graphqlTypes.statusCheckRollup
	if rollupType == nil {
		return
	}
	rollupType.AddFieldConfig("id", &graphql.Field{
		Type: graphql.NewNonNull(graphql.ID),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			repoKey, _ := src["repoKey"].(string)
			sha, _ := src["sha"].(string)
			// GitHub's opaque id is not reproducible; derive a stable one from the commit.
			return "SCR_" + base64.RawURLEncoding.EncodeToString([]byte(repoKey+":"+sha)), nil
		},
	})
	rollupType.AddFieldConfig("commit", &graphql.Field{
		Type: s.graphqlTypes.commit,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.commitSourceFromStatus(p.Context, p.Source), nil
		},
	})
}

// commitSourceFromStatus builds the Commit source a StatusContext or
// StatusCheckRollup points back at, applying the standard private-repo
// visibility gate.
func (s *Resolver) commitSourceFromStatus(ctx context.Context, source interface{}) interface{} {
	src, ok := source.(map[string]interface{})
	if !ok {
		return nil
	}
	repoKey, _ := src["repoKey"].(string)
	sha, _ := src["sha"].(string)
	return s.commitSourceForRepoSHA(ctx, repoKey, sha)
}

// commitSourceForRepoSHA resolves (repoFullName, sha) to a Commit source map,
// or nil when the repository is unreadable.
func (s *Resolver) commitSourceForRepoSHA(ctx context.Context, repoFullName, sha string) interface{} {
	if repoFullName == "" || sha == "" {
		return nil
	}
	owner, name, ok := store.SplitRepoFullName(repoFullName)
	if !ok {
		return nil
	}
	repo := s.store.GetRepo(owner, name)
	if repo == nil || (repo.Private && !s.viewerCanReadRepo(ctx, repo)) {
		return nil
	}
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		return nil
	}
	commit, err := object.GetCommit(stor, plumbing.NewHash(sha))
	if err != nil {
		// The status names a sha the git store no longer has; answer the
		// minimal object source so id/oid still resolve.
		return gitObjectSourceFields("Commit", repo.FullName, sha)
	}
	return gitCommitSource(commit, s.store, repo.FullName)
}

// Release

func (s *Resolver) addReleaseAccountFields() {
	releaseType := s.graphqlTypes.release
	if releaseType == nil {
		return
	}
	html := s.graphQLStringScalar("HTML")
	uri := s.graphQLStringScalar("URI")
	dateTime := s.graphQLStringScalar("DateTime")

	releaseType.AddFieldConfig("author", &graphql.Field{
		Type: s.graphqlTypes.user,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			authorID := releaseSourceInt(p.Source, "authorID")
			if authorID == 0 {
				return nil, nil
			}
			return optionalRendered(s.store.GetUserByID(authorID), userToGraphQL), nil
		},
	})
	releaseType.AddFieldConfig("descriptionHTML", &graphql.Field{
		Type: html,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return nilStr(discussionBodyToHTML(releaseSourceString(p.Source, "body"))), nil
		},
	})
	releaseType.AddFieldConfig("shortDescriptionHTML", &graphql.Field{
		Type: html,
		Args: graphql.FieldConfigArgument{
			"limit": &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 200},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			limit := 200
			if n, ok := intArg(p.Args, "limit"); ok {
				limit = n
			}
			body := releaseSourceString(p.Source, "body")
			short := firstLine(body)
			if limit >= 0 && len(short) > limit {
				short = short[:limit]
			}
			return nilStr(discussionBodyToHTML(short)), nil
		},
	})
	releaseType.AddFieldConfig("updatedAt", &graphql.Field{
		Type: graphql.NewNonNull(dateTime),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return releaseSourceString(p.Source, "updatedAt"), nil
		},
	})
	releaseType.AddFieldConfig("resourcePath", &graphql.Field{
		Type: graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			full := releaseSourceString(p.Source, "repoFullName")
			tag := releaseSourceString(p.Source, "tagName")
			return "/" + full + "/releases/tag/" + tag, nil
		},
	})
	releaseType.AddFieldConfig("repository", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.repository),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repoID := releaseSourceInt(p.Source, "repoID")
			repo := s.store.GetRepoByID(repoID)
			if repo == nil {
				return nil, fmt.Errorf("release repository %d not found", repoID)
			}
			return repoToGraphQL(s.store, repo), nil
		},
	})
	releaseType.AddFieldConfig("tag", &graphql.Field{
		Type: s.graphqlTypes.ref,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			full := releaseSourceString(p.Source, "repoFullName")
			tag := releaseSourceString(p.Source, "tagName")
			oid := s.releaseTagOID(p.Context, full, tag)
			if oid == "" {
				return nil, nil
			}
			return gitRefSource(full, "refs/tags/"+tag, oid), nil
		},
	})
	releaseType.AddFieldConfig("tagCommit", &graphql.Field{
		Type: s.graphqlTypes.commit,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			full := releaseSourceString(p.Source, "repoFullName")
			tag := releaseSourceString(p.Source, "tagName")
			oid := s.releaseTagCommitOID(p.Context, full, tag)
			if oid == "" {
				return nil, nil
			}
			return s.commitSourceForRepoSHA(p.Context, full, oid), nil
		},
	})
	releaseType.AddFieldConfig("mentions", &graphql.Field{
		Type: s.gqlUserConnectionType(s.graphqlTypes.user),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			body := releaseSourceString(p.Source, "body")
			users := s.bodyMentionedUsers(body)
			nodes := make([]map[string]interface{}, 0, len(users))
			for _, u := range users {
				nodes = append(nodes, userToGraphQL(u))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})

	s.addReleaseAssetsField(releaseType, uri, dateTime)
}

// addReleaseAssetsField builds the ReleaseAsset object graph and hangs
// Release.releaseAssets off it.
func (s *Resolver) addReleaseAssetsField(releaseType *graphql.Object, uri, dateTime *graphql.Scalar) {
	assetType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ReleaseAsset",
		Fields: graphql.Fields{
			"id":            &graphql.Field{Type: graphql.NewNonNull(graphql.ID), Resolve: releaseAssetField("nodeID")},
			"name":          &graphql.Field{Type: graphql.NewNonNull(graphql.String), Resolve: releaseAssetField("name")},
			"contentType":   &graphql.Field{Type: graphql.NewNonNull(graphql.String), Resolve: releaseAssetField("contentType")},
			"size":          &graphql.Field{Type: graphql.NewNonNull(graphql.Int), Resolve: releaseAssetField("size")},
			"downloadCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int), Resolve: releaseAssetField("downloadCount")},
			"digest":        &graphql.Field{Type: graphql.String, Resolve: releaseAssetField("digest")},
			"downloadUrl":   &graphql.Field{Type: graphql.NewNonNull(uri), Resolve: releaseAssetField("downloadUrl")},
			"url":           &graphql.Field{Type: graphql.NewNonNull(uri), Resolve: releaseAssetField("url")},
			"createdAt":     &graphql.Field{Type: graphql.NewNonNull(dateTime), Resolve: releaseAssetField("createdAt")},
			"updatedAt":     &graphql.Field{Type: graphql.NewNonNull(dateTime), Resolve: releaseAssetField("updatedAt")},
			"uploadedBy": &graphql.Field{
				Type:    graphql.NewNonNull(s.graphqlTypes.user),
				Resolve: releaseAssetField("uploadedBy"),
			},
			"release": &graphql.Field{
				Type:    releaseType,
				Resolve: releaseAssetField("release"),
			},
		},
	})
	assetEdgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ReleaseAssetEdge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: assetType},
		},
	})
	assetConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "ReleaseAssetConnection",
		Fields: graphql.Fields{
			"edges":      &graphql.Field{Type: graphql.NewList(assetEdgeType)},
			"nodes":      &graphql.Field{Type: graphql.NewList(assetType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})

	args := relayConnectionArgs()
	args["name"] = &graphql.ArgumentConfig{Type: graphql.String}
	releaseType.AddFieldConfig("releaseAssets", &graphql.Field{
		Type: graphql.NewNonNull(assetConnectionType),
		Args: args,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			releaseID := releaseSourceInt(p.Source, "databaseId")
			releaseSource, _ := p.Source.(map[string]interface{})
			full := releaseSourceString(p.Source, "repoFullName")
			assets := s.store.Releases.ListReleaseAssets(releaseID)
			nameFilter, _ := p.Args["name"].(string)
			items := make([]gqlConnItem, 0, len(assets))
			for _, a := range assets {
				if nameFilter != "" && a.Name != nameFilter {
					continue
				}
				asset := a
				items = append(items, gqlConnItem{
					identity: asset.NodeID,
					render:   func() map[string]interface{} { return s.releaseAssetSource(asset, full, releaseSource) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})
}

// releaseAssetSource renders a store release asset as the ReleaseAsset source
// map, mirroring the REST asset URL shape.
func (s *Resolver) releaseAssetSource(a *store.ReleaseAsset, repoFullName string, release map[string]interface{}) map[string]interface{} {
	downloadURL := externalURL(fmt.Sprintf("/api/v3/repos/%s/releases/assets/%d", repoFullName, a.ID))
	var uploader interface{}
	if u := s.store.GetUserByID(a.UploaderID); u != nil {
		uploader = userToGraphQL(u)
	}
	return map[string]interface{}{
		"nodeID":        a.NodeID,
		"name":          a.Name,
		"contentType":   a.ContentType,
		"size":          a.Size,
		"downloadCount": a.DownloadCount,
		"digest":        nilStr(a.Digest),
		"downloadUrl":   downloadURL,
		"url":           downloadURL,
		"createdAt":     a.CreatedAt.UTC().Format(time.RFC3339),
		"updatedAt":     a.UpdatedAt.UTC().Format(time.RFC3339),
		"uploadedBy":    uploader,
		"release":       optionalObject(release),
	}
}

func releaseAssetField(key string) graphql.FieldResolveFn {
	return func(p graphql.ResolveParams) (interface{}, error) {
		src, ok := p.Source.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
		}
		return src[key], nil
	}
}

// releaseTagOID resolves the git object id the release's tag points at, or ""
// when the tag is absent (e.g. a draft release whose tag does not yet exist).
func (s *Resolver) releaseTagOID(ctx context.Context, repoFullName, tag string) string {
	stor := s.readableGitStorage(ctx, repoFullName)
	if stor == nil || tag == "" {
		return ""
	}
	ref, err := stor.Reference(plumbing.NewTagReferenceName(tag))
	if err != nil {
		return ""
	}
	return ref.Hash().String()
}

// releaseTagCommitOID resolves the commit the release's tag points at,
// dereferencing an annotated tag to its target commit.
func (s *Resolver) releaseTagCommitOID(ctx context.Context, repoFullName, tag string) string {
	stor := s.readableGitStorage(ctx, repoFullName)
	if stor == nil || tag == "" {
		return ""
	}
	ref, err := stor.Reference(plumbing.NewTagReferenceName(tag))
	if err != nil {
		return ""
	}
	hash := ref.Hash()
	if commit, err := object.GetCommit(stor, hash); err == nil {
		return commit.Hash.String()
	}
	if tagObj, err := object.GetTag(stor, hash); err == nil {
		if commit, err := tagObj.Commit(); err == nil {
			return commit.Hash.String()
		}
	}
	return ""
}

// readableGitStorage returns the git storage for repoFullName when the viewer
// may read it, applying the private-repo visibility gate.
func (s *Resolver) readableGitStorage(ctx context.Context, repoFullName string) gitStorage.Storer {
	owner, name, ok := store.SplitRepoFullName(repoFullName)
	if !ok {
		return nil
	}
	repo := s.store.GetRepo(owner, name)
	if repo == nil || (repo.Private && !s.viewerCanReadRepo(ctx, repo)) {
		return nil
	}
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		return nil
	}
	return stor
}

// bodyMentionedUsers resolves the real accounts a markdown body @-mentions, in
// first-appearance order and de-duplicated.
func (s *Resolver) bodyMentionedUsers(body string) []*store.User {
	if body == "" {
		return nil
	}
	seen := map[string]bool{}
	var users []*store.User
	for _, match := range mentionLoginPattern.FindAllStringSubmatch(body, -1) {
		login := strings.ToLower(match[1])
		if seen[login] {
			continue
		}
		seen[login] = true
		if u := s.store.LookupUserByLogin(login); u != nil {
			users = append(users, u)
		}
	}
	return users
}

// mentionLoginPattern matches an @login honoring GitHub's login grammar
// (alphanumerics and single hyphens, up to 39 characters).
var mentionLoginPattern = regexp.MustCompile(`(?:^|[^a-zA-Z0-9_/])@([a-zA-Z0-9](?:-?[a-zA-Z0-9]){0,38})`)

func firstLine(body string) string {
	body = strings.TrimSpace(body)
	if idx := strings.IndexByte(body, '\n'); idx >= 0 {
		return strings.TrimSpace(body[:idx])
	}
	return body
}

func releaseSourceString(source interface{}, key string) string {
	src, _ := source.(map[string]interface{})
	v, _ := src[key].(string)
	return v
}

func releaseSourceInt(source interface{}, key string) int {
	src, _ := source.(map[string]interface{})
	v, _ := src[key].(int)
	return v
}

package graphqlapi

import (
	"context"
	"encoding/base64"
	"fmt"
	pathpkg "path"
	"sort"
	"strings"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/graphql-go/graphql"
)

// The git object graph: GitObject and its four implementations (Commit, Tree,
// Blob, Tag), the tree entries linking them, and the Repository entry fields
// (object, ref, refs). All resolve out of git storage through internal/store,
// so this surface and the REST one answer the same revisions.

// --- source maps -----------------------------------------------------------

// gitObjectSourceFields are the GitObject members every implementation carries.
// The graph is walked lazily so selecting one blob does not read the whole tree.
func gitObjectSourceFields(typename, repoFullName, oid string) map[string]interface{} {
	return map[string]interface{}{
		"__typename":   typename,
		"oid":          oid,
		"repoFullName": repoFullName,
	}
}

// gitObjectSource renders the source map for an object whose type is read from
// storage, or nil when the object is absent.
func gitObjectSource(stor gitStorage.Storer, st *store.Store, repoFullName string, hash plumbing.Hash) map[string]interface{} {
	objectType, err := store.GitObjectTypeOf(stor, hash)
	if err != nil {
		return nil
	}
	return gitObjectSourceOfType(stor, st, repoFullName, hash, objectType)
}

func gitObjectSourceOfType(stor gitStorage.Storer, st *store.Store, repoFullName string, hash plumbing.Hash, objectType plumbing.ObjectType) map[string]interface{} {
	oid := hash.String()
	switch objectType {
	case plumbing.CommitObject:
		commit, err := object.GetCommit(stor, hash)
		if err != nil {
			return nil
		}
		return gitCommitSource(commit, st, repoFullName)
	case plumbing.TreeObject:
		return gitObjectSourceFields("Tree", repoFullName, oid)
	case plumbing.BlobObject:
		return gitObjectSourceFields("Blob", repoFullName, oid)
	case plumbing.TagObject:
		tag, err := object.GetTag(stor, hash)
		if err != nil {
			return nil
		}
		source := gitObjectSourceFields("Tag", repoFullName, oid)
		source["name"] = tag.Name
		source["message"] = tag.Message
		source["targetOID"] = tag.Target.String()
		source["tagger"] = gitActorSource(st, tag.Tagger)
		return source
	default:
		return nil
	}
}

// gitCommitSource renders a commit as the shared Commit source map, the same
// shape the pull-request commit lists produce.
func gitCommitSource(commit *object.Commit, st *store.Store, repoFullName string) map[string]interface{} {
	source := gitObjectSourceFields("Commit", repoFullName, commit.Hash.String())
	source["message"] = commit.Message
	source["messageHeadline"] = strings.SplitN(commit.Message, "\n", 2)[0]
	source["messageBody"] = commitMessageBody(commit.Message)
	source["committedDate"] = commit.Committer.When.UTC().Format(time.RFC3339)
	source["authoredDate"] = commit.Author.When.UTC().Format(time.RFC3339)
	source["author"] = gitActorSource(st, commit.Author)
	source["committer"] = gitActorSource(st, commit.Committer)
	return source
}

// gitActorSource renders a git signature as GitActor. GitActor.user must be an
// untyped nil (not a User shell) when no account owns the email, or its
// non-null id would abort the query; optionalRendered keeps it so.
func gitActorSource(st *store.Store, signature object.Signature) map[string]interface{} {
	actor := map[string]interface{}{
		"name":  signature.Name,
		"email": signature.Email,
		"date":  signature.When.UTC().Format(time.RFC3339),
	}
	if st != nil {
		actor["user"] = optionalRendered(st.ResolveUserBySignature(signature.Name, signature.Email), userToGraphQL)
	}
	return actor
}

// gitRefSource renders a reference as the Ref type's source map.
func gitRefSource(repoFullName, qualifiedName, targetOID string) map[string]interface{} {
	prefix, name := splitGitRefName(qualifiedName)
	return map[string]interface{}{
		"name":          name,
		"prefix":        prefix,
		"qualifiedName": qualifiedName,
		"repoFullName":  repoFullName,
		"targetOID":     targetOID,
	}
}

// splitGitRefName splits a qualified reference into its prefix (`refs/heads/`,
// `refs/tags/`, …) and short name.
func splitGitRefName(qualifiedName string) (string, string) {
	if index := strings.LastIndex(qualifiedName, "/"); index >= 0 {
		for _, prefix := range []string{"refs/heads/", "refs/tags/", "refs/pull/", "refs/remotes/"} {
			if strings.HasPrefix(qualifiedName, prefix) {
				return prefix, strings.TrimPrefix(qualifiedName, prefix)
			}
		}
		return qualifiedName[:index+1], qualifiedName[index+1:]
	}
	return "", qualifiedName
}

// --- storage access --------------------------------------------------------

// gitSourceRepo resolves the repository a git-object source belongs to. A
// private repo the viewer cannot read resolves to nothing rather than leaking
// its object graph.
func (s *Resolver) gitSourceRepo(ctx context.Context, source interface{}) (*store.Repo, gitStorage.Storer, error) {
	src, ok := source.(map[string]interface{})
	if !ok {
		return nil, nil, fmt.Errorf("resolve source: unexpected type %T", source)
	}
	fullName, _ := src["repoFullName"].(string)
	owner, name, ok := store.SplitRepoFullName(fullName)
	if !ok {
		return nil, nil, nil
	}
	repo := s.store.GetRepo(owner, name)
	if repo == nil || (repo.Private && !s.viewerCanReadRepo(ctx, repo)) {
		return nil, nil, nil
	}
	return repo, s.store.GetGitStorage(owner, name), nil
}

// gitSourceString reads a string member of a git-object source map.
func gitSourceString(source interface{}, key string) (string, error) {
	src, ok := source.(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("resolve source: unexpected type %T", source)
	}
	value, _ := src[key].(string)
	return value, nil
}

// gitSourceCommit re-reads the commit a Commit source names.
func (s *Resolver) gitSourceCommit(p graphql.ResolveParams) (*store.Repo, gitStorage.Storer, *object.Commit, error) {
	repo, stor, err := s.gitSourceRepo(p.Context, p.Source)
	if err != nil || repo == nil || stor == nil {
		return nil, nil, nil, err
	}
	oid, err := gitSourceString(p.Source, "oid")
	if err != nil {
		return nil, nil, nil, err
	}
	commit, err := object.GetCommit(stor, plumbing.NewHash(oid))
	if err != nil {
		return repo, stor, nil, nil
	}
	return repo, stor, commit, nil
}

// --- shared leaf types -----------------------------------------------------

// gqlGitActorType returns the shared GitActor object type (memoized).
func (s *Resolver) gqlGitActorType() *graphql.Object {
	if s.graphqlTypes.gitActor != nil {
		return s.graphqlTypes.gitActor
	}
	uri := s.graphQLStringScalar("URI")
	s.graphqlTypes.gitActor = graphql.NewObject(graphql.ObjectConfig{
		Name: "GitActor",
		Fields: graphql.Fields{
			"name":  &graphql.Field{Type: graphql.String},
			"email": &graphql.Field{Type: graphql.String},
			"date":  &graphql.Field{Type: s.graphQLStringScalar("GitTimestamp")},
			"avatarUrl": &graphql.Field{
				Type: graphql.NewNonNull(uri),
				Args: graphql.FieldConfigArgument{"size": &graphql.ArgumentConfig{Type: graphql.Int}},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					actor, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					if user, ok := actor["user"].(map[string]interface{}); ok {
						if avatar, ok := user["avatarUrl"].(string); ok && avatar != "" {
							return avatar, nil
						}
					}
					email, _ := actor["email"].(string)
					return externalURL("/avatars/" + gitActorAvatarKey(email)), nil
				},
			},
			"user": &graphql.Field{
				Type: s.graphqlTypes.user,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					actor, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return actor["user"], nil
				},
			},
		},
	})
	return s.graphqlTypes.gitActor
}

// gitActorAvatarKey is the avatar path segment for a signature with no
// matching account, mirroring GitHub's identicon key derived from the email.
func gitActorAvatarKey(email string) string {
	if email == "" {
		return "git"
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strings.ToLower(email)))
}

// gqlLanguageType returns the shared Language object type (memoized).
func (s *Resolver) gqlLanguageType() *graphql.Object {
	if s.graphqlTypes.language != nil {
		return s.graphqlTypes.language
	}
	s.graphqlTypes.language = graphql.NewObject(graphql.ObjectConfig{
		Name: "Language",
		Fields: graphql.Fields{
			"name": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, _ := p.Source.(map[string]interface{})
					name, _ := src["name"].(string)
					return languageNodeID(name), nil
				},
			},
			// No Linguist color data; color is a truthful null.
			"color": &graphql.Field{Type: graphql.String, Resolve: nilResolver},
		},
	})
	return s.graphqlTypes.language
}

// --- the GitObject interface and its implementations -----------------------

// gqlGitObjectInterface returns the GitObject interface (memoized). ResolveType
// reads the type registry lazily so the four implementations may build after it.
func (s *Resolver) gqlGitObjectInterface() *graphql.Interface {
	if s.graphqlTypes.gitObject != nil {
		return s.graphqlTypes.gitObject
	}
	uri := s.graphQLStringScalar("URI")
	gitObjectID := s.graphQLStringScalar("GitObjectID")
	s.graphqlTypes.gitObjectTypes = map[string]*graphql.Object{}
	s.graphqlTypes.gitObject = graphql.NewInterface(graphql.InterfaceConfig{
		Name: "GitObject",
		Fields: graphql.Fields{
			"abbreviatedOid":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"commitResourcePath": &graphql.Field{Type: graphql.NewNonNull(uri)},
			"commitUrl":          &graphql.Field{Type: graphql.NewNonNull(uri)},
			"id":                 &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"oid":                &graphql.Field{Type: graphql.NewNonNull(gitObjectID)},
			"repository":         &graphql.Field{Type: graphql.NewNonNull(s.graphqlTypes.repository)},
		},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			name, _ := source["__typename"].(string)
			return s.graphqlTypes.gitObjectTypes[name]
		},
	})
	return s.graphqlTypes.gitObject
}

// addGitObjectFields installs the six GitObject members on a concrete
// implementation. All derive from the oid plus the repository, so one
// definition serves Commit, Tree, Blob and Tag.
func (s *Resolver) addGitObjectFields(objectType *graphql.Object, nodeIDPrefix string) {
	uri := s.graphQLStringScalar("URI")
	gitObjectID := s.graphQLStringScalar("GitObjectID")
	objectType.AddFieldConfig("oid", &graphql.Field{Type: graphql.NewNonNull(gitObjectID)})
	objectType.AddFieldConfig("abbreviatedOid", &graphql.Field{
		Type: graphql.NewNonNull(graphql.String),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			oid, err := gitSourceString(p.Source, "oid")
			if err != nil {
				return nil, err
			}
			return store.AbbreviatedGitOID(oid), nil
		},
	})
	objectType.AddFieldConfig("commitUrl", &graphql.Field{
		Type: graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			path, err := gitObjectResourcePath(p.Source)
			if err != nil {
				return nil, err
			}
			return externalURL(path), nil
		},
	})
	objectType.AddFieldConfig("commitResourcePath", &graphql.Field{
		Type:    graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { return gitObjectResourcePath(p.Source) },
	})
	objectType.AddFieldConfig("id", &graphql.Field{
		Type: graphql.NewNonNull(graphql.ID),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, _, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil {
				return nil, err
			}
			oid, err := gitSourceString(p.Source, "oid")
			if err != nil {
				return nil, err
			}
			return store.GitObjectNodeID(nodeIDPrefix, repo.ID, oid), nil
		},
	})
	objectType.AddFieldConfig("repository", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.repository),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, _, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil {
				return nil, err
			}
			return repoToGraphQL(s.store, s.store.SnapRepo(repo)), nil
		},
	})
}

func gitObjectResourcePath(source interface{}) (string, error) {
	fullName, err := gitSourceString(source, "repoFullName")
	if err != nil {
		return "", err
	}
	oid, err := gitSourceString(source, "oid")
	if err != nil {
		return "", err
	}
	return "/" + fullName + "/commit/" + oid, nil
}

// gqlCommitType returns the one shared Commit object type (memoized); the
// pull-request commit lists add their check-rollup and authors fields to it.
func (s *Resolver) gqlCommitType() *graphql.Object {
	if s.graphqlTypes.commit != nil {
		return s.graphqlTypes.commit
	}
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	gitActorType := s.gqlGitActorType()

	commitType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "Commit",
		Interfaces: []*graphql.Interface{s.gqlGitObjectInterface(), s.graphqlTypes.node},
		Fields: graphql.Fields{
			"messageHeadline": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"messageBody":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"committedDate":   &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"authoredDate":    &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"message": &graphql.Field{
				Type: graphql.NewNonNull(graphql.String),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					if message, ok := src["message"].(string); ok {
						return message, nil
					}
					headline, _ := src["messageHeadline"].(string)
					body, _ := src["messageBody"].(string)
					if body == "" {
						return headline, nil
					}
					return headline + "\n\n" + body, nil
				},
			},
			"author":    gitActorField(gitActorType, "author"),
			"committer": gitActorField(gitActorType, "committer"),
		},
	})
	s.graphqlTypes.commit = commitType
	s.graphqlTypes.gitObjectTypes["Commit"] = commitType
	s.addGitObjectFields(commitType, store.GitCommitNodeIDPrefix)

	commitType.AddFieldConfig("url", &graphql.Field{
		Type: graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			path, err := gitObjectResourcePath(p.Source)
			if err != nil {
				return nil, err
			}
			return externalURL(path), nil
		},
	})
	commitType.AddFieldConfig("resourcePath", &graphql.Field{
		Type:    graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) { return gitObjectResourcePath(p.Source) },
	})
	commitType.AddFieldConfig("treeUrl", &graphql.Field{
		Type: graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			path, err := gitCommitTreeResourcePath(p)
			if err != nil || path == nil {
				return nil, err
			}
			return externalURL(path.(string)), nil
		},
	})
	commitType.AddFieldConfig("treeResourcePath", &graphql.Field{
		Type:    graphql.NewNonNull(uri),
		Resolve: gitCommitTreeResourcePath,
	})
	commitType.AddFieldConfig("tree", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlTreeType()),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, _, commit, err := s.gitSourceCommit(p)
			if err != nil || repo == nil || commit == nil {
				return nil, err
			}
			return gitObjectSourceFields("Tree", repo.FullName, commit.TreeHash.String()), nil
		},
	})
	commitType.AddFieldConfig("parents", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlCommitConnectionType("CommitConnection")),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, stor, commit, err := s.gitSourceCommit(p)
			if err != nil || repo == nil || commit == nil {
				return emptyGitConnection(), err
			}
			items := make([]gqlConnItem, 0, commit.NumParents())
			for _, parent := range commit.ParentHashes {
				hash := parent
				items = append(items, gqlConnItem{
					identity: hash.String(),
					render: func() map[string]interface{} {
						parentCommit, err := object.GetCommit(stor, hash)
						if err != nil {
							return gitObjectSourceFields("Commit", repo.FullName, hash.String())
						}
						return gitCommitSource(parentCommit, s.store, repo.FullName)
					},
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})
	commitType.AddFieldConfig("file", &graphql.Field{
		Type: s.gqlTreeEntryType(),
		Args: graphql.FieldConfigArgument{
			"path": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, stor, commit, err := s.gitSourceCommit(p)
			if err != nil || repo == nil || commit == nil {
				return nil, err
			}
			tree, err := commit.Tree()
			if err != nil {
				return nil, nil
			}
			path, _ := p.Args["path"].(string)
			entry, err := store.GitTreeEntryAtPath(tree, path)
			if err != nil {
				return nil, nil
			}
			return gitTreeEntrySource(stor, repo.FullName, path, *entry), nil
		},
	})
	commitType.AddFieldConfig("authoredByCommitter", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			author, _ := src["author"].(map[string]interface{})
			committer, _ := src["committer"].(map[string]interface{})
			if author == nil || committer == nil {
				return false, nil
			}
			return author["name"] == committer["name"] && author["email"] == committer["email"], nil
		},
	})
	commitType.AddFieldConfig("tarballUrl", gitCommitArchiveField(uri, "legacy.tar.gz"))
	commitType.AddFieldConfig("zipballUrl", gitCommitArchiveField(uri, "legacy.zip"))
	s.addCommitDiffStatFields(commitType)
	s.addCommitHistoryField(commitType)
	return commitType
}

// gitCommitArchiveField renders a commit's source-archive URL, on the same
// path the REST tarball/zipball redirects target.
func gitCommitArchiveField(uri *graphql.Scalar, format string) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(uri),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			fullName, err := gitSourceString(p.Source, "repoFullName")
			if err != nil {
				return nil, err
			}
			oid, err := gitSourceString(p.Source, "oid")
			if err != nil {
				return nil, err
			}
			return externalURL("/" + fullName + "/" + format + "/" + oid), nil
		},
	}
}

// addCommitDiffStatFields installs the line/file counts a commit introduced,
// computed against its first parent.
func (s *Resolver) addCommitDiffStatFields(commitType *graphql.Object) {
	stat := func(pick func(additions, deletions, changedFiles int) int) func(graphql.ResolveParams) (interface{}, error) {
		return func(p graphql.ResolveParams) (interface{}, error) {
			repo, _, commit, err := s.gitSourceCommit(p)
			if err != nil || repo == nil || commit == nil {
				return 0, err
			}
			additions, deletions, changedFiles, err := store.GitCommitDiffStats(commit)
			if err != nil {
				return 0, nil
			}
			return pick(additions, deletions, changedFiles), nil
		}
	}
	commitType.AddFieldConfig("additions", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.Int),
		Resolve: stat(func(additions, _, _ int) int { return additions }),
	})
	commitType.AddFieldConfig("deletions", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.Int),
		Resolve: stat(func(_, deletions, _ int) int { return deletions }),
	})
	commitType.AddFieldConfig("changedFiles", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.Int),
		Resolve: stat(func(_, _, changedFiles int) int { return changedFiles }),
	})
	commitType.AddFieldConfig("changedFilesIfAvailable", &graphql.Field{
		Type:    graphql.Int,
		Resolve: stat(func(_, _, changedFiles int) int { return changedFiles }),
	})
}

// gitActorField reads an already-rendered GitActor out of a source map.
func gitActorField(gitActorType *graphql.Object, key string) *graphql.Field {
	return &graphql.Field{
		Type: gitActorType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			return src[key], nil
		},
	}
}

func gitCommitTreeResourcePath(p graphql.ResolveParams) (interface{}, error) {
	fullName, err := gitSourceString(p.Source, "repoFullName")
	if err != nil {
		return nil, err
	}
	oid, err := gitSourceString(p.Source, "oid")
	if err != nil {
		return nil, err
	}
	return "/" + fullName + "/tree/" + oid, nil
}

func emptyGitConnection() map[string]interface{} {
	return map[string]interface{}{
		"nodes":      []map[string]interface{}{},
		"edges":      []map[string]interface{}{},
		"totalCount": 0,
		"pageInfo": map[string]interface{}{
			"hasNextPage": false, "hasPreviousPage": false,
			"startCursor": nil, "endCursor": nil,
		},
	}
}

// gqlCommitConnectionType builds CommitConnection and CommitHistoryConnection,
// same-shaped on GitHub.
func (s *Resolver) gqlCommitConnectionType(name string) *graphql.Object {
	if s.graphqlTypes.commitConnections == nil {
		s.graphqlTypes.commitConnections = map[string]*graphql.Object{}
	}
	if existing := s.graphqlTypes.commitConnections[name]; existing != nil {
		return existing
	}
	if s.graphqlTypes.commitEdge == nil {
		s.graphqlTypes.commitEdge = graphql.NewObject(graphql.ObjectConfig{
			Name: "CommitEdge",
			Fields: graphql.Fields{
				"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
				"node":   &graphql.Field{Type: s.gqlCommitType()},
			},
		})
	}
	connection := graphql.NewObject(graphql.ObjectConfig{
		Name: name,
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(s.gqlCommitType())},
			"edges":      &graphql.Field{Type: graphql.NewList(s.graphqlTypes.commitEdge)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})
	s.graphqlTypes.commitConnections[name] = connection
	return connection
}

// addCommitHistoryField installs Commit.history — the `git log` ancestry walk
// with GitHub's path, author, since and until filters.
func (s *Resolver) addCommitHistoryField(commitType *graphql.Object) {
	gitTimestamp := s.graphQLStringScalar("GitTimestamp")
	commitAuthorInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CommitAuthor",
		Fields: graphql.InputObjectConfigFieldMap{
			"emails": &graphql.InputObjectFieldConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			"id":     &graphql.InputObjectFieldConfig{Type: graphql.ID},
		},
	})
	args := relayConnectionArgs()
	args["path"] = &graphql.ArgumentConfig{Type: graphql.String}
	args["author"] = &graphql.ArgumentConfig{Type: commitAuthorInput}
	args["since"] = &graphql.ArgumentConfig{Type: gitTimestamp}
	args["until"] = &graphql.ArgumentConfig{Type: gitTimestamp}

	commitType.AddFieldConfig("history", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlCommitConnectionType("CommitHistoryConnection")),
		Args: args,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, _, commit, err := s.gitSourceCommit(p)
			if err != nil || repo == nil || commit == nil {
				return emptyGitConnection(), err
			}
			filter, err := s.commitHistoryFilter(p.Args)
			if err != nil {
				return nil, err
			}
			commits, err := gitCommitHistory(commit, s.store, filter)
			if err != nil {
				return emptyGitConnection(), nil
			}
			items := make([]gqlConnItem, 0, len(commits))
			for _, c := range commits {
				historyCommit := c
				items = append(items, gqlConnItem{
					identity: historyCommit.Hash.String(),
					render: func() map[string]interface{} {
						return gitCommitSource(historyCommit, s.store, repo.FullName)
					},
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})
}

// commitHistoryFilter is the resolved form of Commit.history's arguments.
type commitHistoryFilter struct {
	path   string
	emails []string
	logins []string
	since  *time.Time
	until  *time.Time
}

func (s *Resolver) commitHistoryFilter(args map[string]interface{}) (commitHistoryFilter, error) {
	filter := commitHistoryFilter{}
	filter.path = strings.Trim(gitArgString(args, "path"), "/")
	if since, ok := gitArgTime(args, "since"); ok {
		filter.since = &since
	}
	if until, ok := gitArgTime(args, "until"); ok {
		filter.until = &until
	}
	author, _ := args["author"].(map[string]interface{})
	if author == nil {
		return filter, nil
	}
	if raw, ok := author["emails"].([]interface{}); ok {
		for _, value := range raw {
			if email, ok := value.(string); ok && email != "" {
				filter.emails = append(filter.emails, strings.ToLower(email))
			}
		}
	}
	// GitHub documents the user id as taking precedence over the emails.
	if nodeID, ok := author["id"].(string); ok && nodeID != "" {
		user := store.FindUserByNodeID(s.store, nodeID)
		if user == nil {
			return commitHistoryFilter{}, gqlMissingNode("User", nodeID)
		}
		filter.emails = nil
		filter.logins = []string{strings.ToLower(user.Login)}
		if user.Email != "" {
			filter.emails = []string{strings.ToLower(user.Email)}
		}
	}
	return filter, nil
}

func (f commitHistoryFilter) matches(st *store.Store, commit *object.Commit) (bool, error) {
	when := commit.Committer.When.UTC()
	if f.since != nil && when.Before(*f.since) {
		return false, nil
	}
	if f.until != nil && when.After(*f.until) {
		return false, nil
	}
	if len(f.emails) > 0 || len(f.logins) > 0 {
		if !f.matchesAuthor(st, commit) {
			return false, nil
		}
	}
	if f.path != "" {
		touches, err := store.CommitTouchesPath(commit, f.path)
		if err != nil {
			return false, err
		}
		if !touches {
			return false, nil
		}
	}
	return true, nil
}

func (f commitHistoryFilter) matchesAuthor(st *store.Store, commit *object.Commit) bool {
	email := strings.ToLower(commit.Author.Email)
	for _, candidate := range f.emails {
		if candidate == email {
			return true
		}
	}
	if len(f.logins) == 0 {
		return false
	}
	user := st.ResolveUserBySignature(commit.Author.Name, commit.Author.Email)
	if user == nil {
		return false
	}
	login := strings.ToLower(user.Login)
	for _, candidate := range f.logins {
		if candidate == login {
			return true
		}
	}
	return false
}

// gitCommitHistory walks the ancestry in `git log` order, keeping commits the
// filter accepts. Same walk as the REST commit listing.
func gitCommitHistory(head *object.Commit, st *store.Store, filter commitHistoryFilter) ([]*object.Commit, error) {
	iter := object.NewCommitPreorderIter(head, nil, nil)
	defer iter.Close()
	var commits []*object.Commit
	err := iter.ForEach(func(commit *object.Commit) error {
		keep, err := filter.matches(st, commit)
		if err != nil {
			return err
		}
		if keep {
			commits = append(commits, commit)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return commits, nil
}

func gitArgString(args map[string]interface{}, key string) string {
	value, _ := args[key].(string)
	return value
}

func gitArgTime(args map[string]interface{}, key string) (time.Time, bool) {
	raw, _ := args[key].(string)
	if raw == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

// gqlTreeType returns the Tree object type (memoized).
func (s *Resolver) gqlTreeType() *graphql.Object {
	if s.graphqlTypes.tree != nil {
		return s.graphqlTypes.tree
	}
	treeType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "Tree",
		Interfaces: []*graphql.Interface{s.gqlGitObjectInterface(), s.graphqlTypes.node},
		Fields:     graphql.Fields{},
	})
	s.graphqlTypes.tree = treeType
	s.graphqlTypes.gitObjectTypes["Tree"] = treeType
	s.addGitObjectFields(treeType, store.GitTreeNodeIDPrefix)
	treeType.AddFieldConfig("entries", &graphql.Field{
		Type: graphql.NewList(graphql.NewNonNull(s.gqlTreeEntryType())),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, stor, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil || stor == nil {
				return nil, err
			}
			oid, err := gitSourceString(p.Source, "oid")
			if err != nil {
				return nil, err
			}
			basePath, _ := p.Source.(map[string]interface{})["treePath"].(string)
			tree, err := object.GetTree(stor, plumbing.NewHash(oid))
			if err != nil {
				return nil, nil
			}
			entries := make([]interface{}, 0, len(tree.Entries))
			for _, entry := range tree.Entries {
				path := entry.Name
				if basePath != "" {
					path = basePath + "/" + entry.Name
				}
				entries = append(entries, gitTreeEntrySource(stor, repo.FullName, path, entry))
			}
			return entries, nil
		},
	})
	return treeType
}

// gitTreeEntrySource renders one tree entry. size and lineCount resolve lazily
// so listing a directory does not read every blob.
func gitTreeEntrySource(stor gitStorage.Storer, repoFullName, path string, entry object.TreeEntry) map[string]interface{} {
	return map[string]interface{}{
		"name":         entry.Name,
		"path":         path,
		"mode":         int(entry.Mode),
		"type":         store.GitTreeEntryType(entry.Mode),
		"oid":          entry.Hash.String(),
		"repoFullName": repoFullName,
	}
}

// gqlTreeEntryType returns the TreeEntry object type (memoized).
func (s *Resolver) gqlTreeEntryType() *graphql.Object {
	if s.graphqlTypes.treeEntry != nil {
		return s.graphqlTypes.treeEntry
	}
	base64String := s.graphQLStringScalar("Base64String")
	gitObjectID := s.graphQLStringScalar("GitObjectID")
	entryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "TreeEntry",
		Fields: graphql.Fields{
			"name": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"path": &graphql.Field{Type: graphql.String},
			"mode": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"type": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"oid":  &graphql.Field{Type: graphql.NewNonNull(gitObjectID)},
			"nameRaw": &graphql.Field{
				Type: graphql.NewNonNull(base64String),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					name, err := gitSourceString(p.Source, "name")
					if err != nil {
						return nil, err
					}
					return base64.StdEncoding.EncodeToString([]byte(name)), nil
				},
			},
			"pathRaw": &graphql.Field{
				Type: base64String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					path, err := gitSourceString(p.Source, "path")
					if err != nil {
						return nil, err
					}
					return base64.StdEncoding.EncodeToString([]byte(path)), nil
				},
			},
			"extension": &graphql.Field{
				Type: graphql.String,
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					name, err := gitSourceString(p.Source, "name")
					if err != nil {
						return nil, err
					}
					extension := pathpkg.Ext(name)
					if extension == "" {
						return nil, nil
					}
					return extension, nil
				},
			},
		},
	})
	s.graphqlTypes.treeEntry = entryType
	entryType.AddFieldConfig("language", &graphql.Field{
		Type: s.gqlLanguageType(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			name, err := gitSourceString(p.Source, "name")
			if err != nil {
				return nil, err
			}
			language, ok := store.LanguageForFilename(name)
			if !ok {
				return nil, nil
			}
			return map[string]interface{}{"name": language}, nil
		},
	})
	entryType.AddFieldConfig("repository", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.repository),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, _, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil {
				return nil, err
			}
			return repoToGraphQL(s.store, s.store.SnapRepo(repo)), nil
		},
	})
	entryType.AddFieldConfig("size", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Int),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			blob, err := s.gitTreeEntryBlob(p)
			if err != nil || blob == nil {
				// A tree or gitlink entry has no byte size; GitHub reports 0.
				return 0, err
			}
			return int(blob.Size), nil
		},
	})
	entryType.AddFieldConfig("lineCount", &graphql.Field{
		Type: graphql.Int,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			blob, err := s.gitTreeEntryBlob(p)
			if err != nil || blob == nil {
				return nil, err
			}
			content, ok, err := s.gitSourceBlobContent(p)
			if err != nil || !ok || store.GitBlobIsBinary(content) {
				return nil, err
			}
			return gitTextLineCount(content), nil
		},
	})
	entryType.AddFieldConfig("object", &graphql.Field{
		Type: s.gqlGitObjectInterface(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, stor, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil || stor == nil {
				return nil, err
			}
			oid, err := gitSourceString(p.Source, "oid")
			if err != nil {
				return nil, err
			}
			source := gitObjectSource(stor, s.store, repo.FullName, plumbing.NewHash(oid))
			if source == nil {
				// A gitlink names a commit in the submodule's repository,
				// not contained here.
				return nil, nil
			}
			if source["__typename"] == "Tree" {
				// Carry the path down so nested-tree entries keep their full
				// repository-relative paths.
				if path, _ := gitSourceString(p.Source, "path"); path != "" {
					source["treePath"] = path
				}
			}
			return source, nil
		},
	})
	return entryType
}

// gitTreeEntryBlob reads the blob a tree entry names, or nil when it is not a
// blob.
func (s *Resolver) gitTreeEntryBlob(p graphql.ResolveParams) (*object.Blob, error) {
	repo, stor, err := s.gitSourceRepo(p.Context, p.Source)
	if err != nil || repo == nil || stor == nil {
		return nil, err
	}
	entryType, err := gitSourceString(p.Source, "type")
	if err != nil {
		return nil, err
	}
	if entryType != "blob" {
		return nil, nil
	}
	oid, err := gitSourceString(p.Source, "oid")
	if err != nil {
		return nil, err
	}
	blob, err := object.GetBlob(stor, plumbing.NewHash(oid))
	if err != nil {
		return nil, nil
	}
	return blob, nil
}

func gitTextLineCount(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	lines := strings.Count(string(content), "\n")
	if !strings.HasSuffix(string(content), "\n") {
		lines++
	}
	return lines
}

// gqlBlobType returns the Blob object type (memoized).
func (s *Resolver) gqlBlobType() *graphql.Object {
	if s.graphqlTypes.blob != nil {
		return s.graphqlTypes.blob
	}
	blobType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "Blob",
		Interfaces: []*graphql.Interface{s.gqlGitObjectInterface(), s.graphqlTypes.node},
		Fields:     graphql.Fields{},
	})
	s.graphqlTypes.blob = blobType
	s.graphqlTypes.gitObjectTypes["Blob"] = blobType
	s.addGitObjectFields(blobType, store.GitBlobNodeIDPrefix)
	blobType.AddFieldConfig("byteSize", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Int),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			blob, err := s.gitSourceBlob(p)
			if err != nil || blob == nil {
				return 0, err
			}
			return int(blob.Size), nil
		},
	})
	blobType.AddFieldConfig("isBinary", &graphql.Field{
		Type: graphql.Boolean,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			content, ok, err := s.gitSourceBlobContent(p)
			if err != nil || !ok {
				return nil, err
			}
			return store.GitBlobIsBinary(content), nil
		},
	})
	blobType.AddFieldConfig("isTruncated", &graphql.Field{
		// Blobs are served whole, never a partial read.
		Type:    graphql.NewNonNull(graphql.Boolean),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return false, nil },
	})
	blobType.AddFieldConfig("text", &graphql.Field{
		Type: graphql.String,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			content, ok, err := s.gitSourceBlobContent(p)
			if err != nil || !ok {
				return nil, err
			}
			if store.GitBlobIsBinary(content) {
				// Null text is how clients detect binary content.
				return nil, nil
			}
			return string(content), nil
		},
	})
	return blobType
}

func (s *Resolver) gitSourceBlob(p graphql.ResolveParams) (*object.Blob, error) {
	repo, stor, err := s.gitSourceRepo(p.Context, p.Source)
	if err != nil || repo == nil || stor == nil {
		return nil, err
	}
	oid, err := gitSourceString(p.Source, "oid")
	if err != nil {
		return nil, err
	}
	blob, err := object.GetBlob(stor, plumbing.NewHash(oid))
	if err != nil {
		return nil, nil
	}
	return blob, nil
}

func (s *Resolver) gitSourceBlobContent(p graphql.ResolveParams) ([]byte, bool, error) {
	repo, stor, err := s.gitSourceRepo(p.Context, p.Source)
	if err != nil || repo == nil || stor == nil {
		return nil, false, err
	}
	oid, err := gitSourceString(p.Source, "oid")
	if err != nil {
		return nil, false, err
	}
	content, err := store.ReadGitBlob(stor, plumbing.NewHash(oid))
	if err != nil {
		return nil, false, nil
	}
	return content, true, nil
}

// gqlTagType returns the Tag object type (memoized).
func (s *Resolver) gqlTagType() *graphql.Object {
	if s.graphqlTypes.tag != nil {
		return s.graphqlTypes.tag
	}
	tagType := graphql.NewObject(graphql.ObjectConfig{
		Name:       "Tag",
		Interfaces: []*graphql.Interface{s.gqlGitObjectInterface(), s.graphqlTypes.node},
		Fields: graphql.Fields{
			"name":    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"message": &graphql.Field{Type: graphql.String},
			"tagger":  gitActorField(s.gqlGitActorType(), "tagger"),
		},
	})
	s.graphqlTypes.tag = tagType
	s.graphqlTypes.gitObjectTypes["Tag"] = tagType
	s.addGitObjectFields(tagType, store.GitTagNodeIDPrefix)
	tagType.AddFieldConfig("target", &graphql.Field{
		Type: graphql.NewNonNull(s.gqlGitObjectInterface()),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, stor, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil || stor == nil {
				return nil, err
			}
			targetOID, err := gitSourceString(p.Source, "targetOID")
			if err != nil {
				return nil, err
			}
			return optionalObject(gitObjectSource(stor, s.store, repo.FullName, plumbing.NewHash(targetOID))), nil
		},
	})
	return tagType
}

// --- Ref -------------------------------------------------------------------

// addGitRefFields installs the Ref members reaching into the object graph: id,
// repository, and target.
func (s *Resolver) addGitRefFields(refType *graphql.Object) {
	refType.AddFieldConfig("id", &graphql.Field{
		Type: graphql.NewNonNull(graphql.ID),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, _, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil {
				return nil, err
			}
			qualified, err := gitRefQualifiedName(p.Source)
			if err != nil {
				return nil, err
			}
			return store.GitObjectNodeID(store.GitRefNodeIDPrefix, repo.ID, qualified), nil
		},
	})
	refType.AddFieldConfig("repository", &graphql.Field{
		Type: graphql.NewNonNull(s.graphqlTypes.repository),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, _, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil {
				return nil, err
			}
			return repoToGraphQL(s.store, s.store.SnapRepo(repo)), nil
		},
	})
	refType.AddFieldConfig("target", &graphql.Field{
		Type: s.gqlGitObjectInterface(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, stor, err := s.gitSourceRepo(p.Context, p.Source)
			if err != nil || repo == nil || stor == nil {
				return nil, err
			}
			src, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			oid, _ := src["targetOID"].(string)
			if oid == "" {
				qualified, err := gitRefQualifiedName(p.Source)
				if err != nil {
					return nil, err
				}
				hash, found, err := store.ResolveGitObjectReference(stor, qualified)
				if err != nil || !found {
					return nil, nil
				}
				oid = hash.String()
			}
			return optionalObject(gitObjectSource(stor, s.store, repo.FullName, plumbing.NewHash(oid))), nil
		},
	})
}

// gitRefQualifiedName is a Ref source's qualified name, recomposed from prefix
// and name when the producer did not record it.
func gitRefQualifiedName(source interface{}) (string, error) {
	src, ok := source.(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("resolve source: unexpected type %T", source)
	}
	if qualified, ok := src["qualifiedName"].(string); ok && qualified != "" {
		return qualified, nil
	}
	prefix, _ := src["prefix"].(string)
	name, _ := src["name"].(string)
	return prefix + name, nil
}

// gqlRefConnectionType returns the RefConnection object type (memoized).
func (s *Resolver) gqlRefConnectionType() *graphql.Object {
	if s.graphqlTypes.refConnection != nil {
		return s.graphqlTypes.refConnection
	}
	refEdgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "RefEdge",
		Fields: graphql.Fields{
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"node":   &graphql.Field{Type: s.gqlRefType()},
		},
	})
	s.graphqlTypes.refConnection = graphql.NewObject(graphql.ObjectConfig{
		Name: "RefConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(s.gqlRefType())},
			"edges":      &graphql.Field{Type: graphql.NewList(refEdgeType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})
	return s.graphqlTypes.refConnection
}

// --- Repository entry points ----------------------------------------------

// addGitObjectFieldsToRepository installs Repository.object, .ref and .refs.
func (s *Resolver) addGitObjectFieldsToRepository(repoType *graphql.Object) {
	// Build all four implementations up front: ResolveType dispatches through
	// their registry, and `... on Blob` only validates once the type is in the
	// schema.
	s.gqlCommitType()
	s.gqlTreeType()
	s.gqlBlobType()
	s.gqlTagType()

	gitObjectID := s.graphQLStringScalar("GitObjectID")
	orderDirectionEnum := s.graphQLEnum("OrderDirection", "ASC", "DESC")
	refOrderFieldEnum := s.graphQLEnum("RefOrderField", "ALPHABETICAL", "TAG_COMMIT_DATE")
	refOrderInput := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "RefOrder",
		Fields: graphql.InputObjectConfigFieldMap{
			"direction": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(orderDirectionEnum)},
			"field":     &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(refOrderFieldEnum)},
		},
	})

	repoType.AddFieldConfig("object", &graphql.Field{
		Type: s.gqlGitObjectInterface(),
		Args: graphql.FieldConfigArgument{
			"expression": &graphql.ArgumentConfig{Type: graphql.String},
			"oid":        &graphql.ArgumentConfig{Type: gitObjectID},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, stor, err := s.repositorySourceGitStorage(p)
			if err != nil || repo == nil || stor == nil {
				return nil, err
			}
			expression, _ := p.Args["expression"].(string)
			if oid, ok := p.Args["oid"].(string); ok && oid != "" {
				expression = oid
			}
			if expression == "" {
				return nil, nil
			}
			revision, err := store.ResolveGitRevision(stor, expression)
			if err != nil {
				// An unresolvable revision is null on GitHub, not an error.
				return nil, nil
			}
			source := gitObjectSourceOfType(stor, s.store, repo.FullName, revision.Hash, revision.Type)
			if source != nil && revision.Type == plumbing.TreeObject {
				// A tree reached through `<rev>:<path>` keeps that path so its
				// entries report repository-relative paths.
				if _, path, hasPath := strings.Cut(expression, ":"); hasPath && path != "" {
					source["treePath"] = path
				}
			}
			return source, nil
		},
	})

	repoType.AddFieldConfig("ref", &graphql.Field{
		Type: s.gqlRefType(),
		Args: graphql.FieldConfigArgument{
			"qualifiedName": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, stor, err := s.repositorySourceGitStorage(p)
			if err != nil || repo == nil || stor == nil {
				return nil, err
			}
			qualifiedName, _ := p.Args["qualifiedName"].(string)
			reference := lookupGitReference(stor, qualifiedName)
			if reference == nil {
				return nil, nil
			}
			hash, err := store.ResolvedReferenceHash(stor, reference, map[plumbing.ReferenceName]bool{})
			if err != nil {
				return nil, nil
			}
			return s.decorateRefSource(repo, gitRefSource(repo.FullName, reference.Name().String(), hash.String())), nil
		},
	})

	repoType.AddFieldConfig("refs", &graphql.Field{
		Type: s.gqlRefConnectionType(),
		Args: graphql.FieldConfigArgument{
			"after":     &graphql.ArgumentConfig{Type: graphql.String},
			"before":    &graphql.ArgumentConfig{Type: graphql.String},
			"direction": &graphql.ArgumentConfig{Type: orderDirectionEnum},
			"first":     &graphql.ArgumentConfig{Type: graphql.Int},
			"last":      &graphql.ArgumentConfig{Type: graphql.Int},
			"orderBy":   &graphql.ArgumentConfig{Type: refOrderInput},
			"query":     &graphql.ArgumentConfig{Type: graphql.String},
			"refPrefix": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, stor, err := s.repositorySourceGitStorage(p)
			if err != nil || repo == nil || stor == nil {
				return emptyGitConnection(), err
			}
			refPrefix, _ := p.Args["refPrefix"].(string)
			refs, err := store.ListGitReferences(stor, refPrefix)
			if err != nil {
				return emptyGitConnection(), nil
			}
			if query, ok := p.Args["query"].(string); ok && query != "" {
				refs = filterGitReferences(refs, refPrefix, query)
			}
			sortGitReferences(stor, refs, p.Args)
			items := make([]gqlConnItem, 0, len(refs))
			for _, reference := range refs {
				ref := reference
				items = append(items, gqlConnItem{
					identity: ref.Name().String(),
					render: func() map[string]interface{} {
						hash, err := store.ResolvedReferenceHash(stor, ref, map[plumbing.ReferenceName]bool{})
						oid := ""
						if err == nil {
							oid = hash.String()
						}
						return s.decorateRefSource(repo, gitRefSource(repo.FullName, ref.Name().String(), oid))
					},
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})
}

// decorateRefSource attaches a Ref's branch-protection rule, so a ref reached
// through refs()/ref() answers branchProtectionRule like the PR base ref.
func (s *Resolver) decorateRefSource(repo *store.Repo, ref map[string]interface{}) map[string]interface{} {
	prefix, _ := ref["prefix"].(string)
	name, _ := ref["name"].(string)
	if prefix != "refs/heads/" || name == "" {
		return ref
	}
	if rule := s.branchProtectionRuleForPR(repo, name); rule != nil {
		ref["branchProtectionRule"] = rule
	}
	return ref
}

// repositorySourceGitStorage resolves the repository a Repository source
// describes plus its git storage, under the usual visibility gate.
func (s *Resolver) repositorySourceGitStorage(p graphql.ResolveParams) (*store.Repo, gitStorage.Storer, error) {
	src, ok := p.Source.(map[string]interface{})
	if !ok {
		return nil, nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
	}
	fullName, _ := src["nameWithOwner"].(string)
	owner, name, ok := store.SplitRepoFullName(fullName)
	if !ok {
		return nil, nil, nil
	}
	repo := s.store.GetRepo(owner, name)
	if repo == nil || (repo.Private && !s.viewerCanReadRepo(p.Context, repo)) {
		return nil, nil, nil
	}
	return repo, s.store.GetGitStorage(owner, name), nil
}

// lookupGitReference implements Repository.ref(qualifiedName:)'s documented
// order: a qualified match first, then branch and tag short names.
func lookupGitReference(stor gitStorage.Storer, qualifiedName string) *plumbing.Reference {
	if qualifiedName == "" {
		return nil
	}
	candidates := make([]plumbing.ReferenceName, 0, 4)
	if strings.HasPrefix(qualifiedName, "refs/") {
		candidates = append(candidates, plumbing.ReferenceName(qualifiedName))
	} else {
		if strings.HasPrefix(qualifiedName, "heads/") || strings.HasPrefix(qualifiedName, "tags/") {
			candidates = append(candidates, plumbing.ReferenceName("refs/"+qualifiedName))
		}
		candidates = append(candidates,
			plumbing.NewBranchReferenceName(qualifiedName),
			plumbing.NewTagReferenceName(qualifiedName))
	}
	for _, name := range candidates {
		if ref, err := stor.Reference(name); err == nil && ref != nil {
			return ref
		}
	}
	return nil
}

func filterGitReferences(refs []*plumbing.Reference, refPrefix, query string) []*plumbing.Reference {
	needle := strings.ToLower(query)
	kept := refs[:0]
	for _, ref := range refs {
		short := strings.TrimPrefix(ref.Name().String(), refPrefix)
		if strings.Contains(strings.ToLower(short), needle) {
			kept = append(kept, ref)
		}
	}
	return kept
}

// sortGitReferences applies RefOrder (and the deprecated direction argument).
// ALPHABETICAL is ListGitReferences' existing order; TAG_COMMIT_DATE orders by
// the commit date each ref peels to.
func sortGitReferences(stor gitStorage.Storer, refs []*plumbing.Reference, args map[string]interface{}) {
	field, direction := "ALPHABETICAL", "ASC"
	if orderBy, ok := args["orderBy"].(map[string]interface{}); ok {
		if value, ok := orderBy["field"].(string); ok && value != "" {
			field = value
		}
		if value, ok := orderBy["direction"].(string); ok && value != "" {
			direction = value
		}
	}
	if value, ok := args["direction"].(string); ok && value != "" {
		direction = value
	}
	if field == "TAG_COMMIT_DATE" {
		dates := map[string]time.Time{}
		for _, ref := range refs {
			dates[ref.Name().String()] = gitReferenceCommitDate(stor, ref)
		}
		sort.SliceStable(refs, func(a, b int) bool {
			left, right := dates[refs[a].Name().String()], dates[refs[b].Name().String()]
			if left.Equal(right) {
				return refs[a].Name().String() < refs[b].Name().String()
			}
			return left.Before(right)
		})
	}
	if direction == "DESC" {
		for i, j := 0, len(refs)-1; i < j; i, j = i+1, j-1 {
			refs[i], refs[j] = refs[j], refs[i]
		}
	}
}

func gitReferenceCommitDate(stor gitStorage.Storer, ref *plumbing.Reference) time.Time {
	hash, err := store.ResolvedReferenceHash(stor, ref, map[plumbing.ReferenceName]bool{})
	if err != nil {
		return time.Time{}
	}
	peeled, err := store.PeelGitTagObjects(stor, hash)
	if err != nil {
		return time.Time{}
	}
	commit, err := object.GetCommit(stor, peeled)
	if err != nil {
		return time.Time{}
	}
	return commit.Committer.When.UTC()
}

// gitObjectNodeByID resolves a git object or ref global id to its source map
// for Query.node, refusing repositories the viewer cannot read.
func (s *Resolver) gitObjectNodeByID(ctx context.Context, nodeID string) interface{} {
	prefix, repoID, value, ok := store.ParseGitObjectNodeID(nodeID)
	if !ok {
		return nil
	}
	repo := s.store.GetRepoByID(repoID)
	if repo == nil || (repo.Private && !s.viewerCanReadRepo(ctx, repo)) {
		return nil
	}
	owner, name, ok := store.SplitRepoFullName(repo.FullName)
	if !ok {
		return nil
	}
	stor := s.store.GetGitStorage(owner, name)
	if stor == nil {
		return nil
	}
	if prefix == store.GitRefNodeIDPrefix {
		reference := lookupGitReference(stor, value)
		if reference == nil {
			return nil
		}
		hash, err := store.ResolvedReferenceHash(stor, reference, map[plumbing.ReferenceName]bool{})
		if err != nil {
			return nil
		}
		source := s.decorateRefSource(repo, gitRefSource(repo.FullName, reference.Name().String(), hash.String()))
		source["__typename"] = "Ref"
		return source
	}
	if !store.ValidGitObjectID(value) {
		return nil
	}
	hash := plumbing.NewHash(value)
	objectType, err := store.GitObjectTypeOf(stor, hash)
	if err != nil || store.GitObjectNodeIDPrefixForType(objectType) != prefix {
		return nil
	}
	return gitObjectSourceOfType(stor, s.store, repo.FullName, hash, objectType)
}

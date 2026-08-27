package graphqlapi

// The final residue pass: the members GitHub declares that bleephub already
// holds the data for but had not yet exposed over GraphQL.
//
//   - the Package object graph (repository, statistics, version(s),
//     latestVersion) and its PackageVersion/PackageFile sub-objects, plus
//     Repository.packages reusing the one shared PackageConnection;
//   - the Assignable interface's actor connections (assignedActors /
//     suggestedActors), which Issue and PullRequest already carry;
//   - Query.topic; and
//   - Ref.rules.
//
// It is driven by one call at the end of addAccountSurfaceFieldsToSchema
// (Query.topic is the exception — that one root field is wired from the schema
// builder because it needs the queryType). Every type it names is already
// assembled by then: the PackageConnection User/Organization.packages built,
// the shared AssigneeConnection, the RepositoryRuleConnection, Repository, Ref
// and Release.

import (
	"sort"
	"time"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// addFinalResidueFields installs the residue members. Wired by a single call
// at the end of addAccountSurfaceFieldsToSchema.
func (s *Resolver) addFinalResidueFields(types *accountSurfaceTypes) {
	s.addPackageResidueFields(types)
	s.addRepositoryPackagesField(types)
	s.addAssignableActorInterfaceFields()
	s.addRefRulesField()
}

// --- Package ----------------------------------------------------------------

// packageSourceMap renders a store package into the Package source shape. The
// store id and owner identity are threaded so the residue resolvers can look up
// the package's versions and, for a repository-owned package, its repository.
func packageSourceMap(pkg *store.Package, kind string) map[string]interface{} {
	return map[string]interface{}{
		"id":          pkg.NodeID,
		"name":        pkg.Name,
		"packageType": kind,
		"_pkgStoreID": pkg.ID,
		"_ownerType":  pkg.OwnerType,
		"_ownerKey":   pkg.OwnerKey,
	}
}

// packageVersionSourceMap renders a store package version into the
// PackageVersion source shape. The parent package source is threaded as the
// back-reference PackageVersion.package returns.
func packageVersionSourceMap(v *store.PackageVersion, pkgSource map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"id":              v.NodeID,
		"version":         v.Version,
		"_versionStoreID": v.ID,
		"_package":        optionalObject(pkgSource),
	}
}

// packageFileSourceMap renders a store package file into the PackageFile source
// shape. versionSource is threaded as the PackageFile.packageVersion
// back-reference. GitHub's md5/sha1/sha256 checksums are unmodeled by the store
// and render null; updatedAt (DateTime!) is the file's immutable upload time.
func packageFileSourceMap(f *store.PackageFile, versionSource map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{
		"id":             f.NodeID,
		"name":           f.Name,
		"size":           int(f.Size),
		"updatedAt":      f.UpdatedAt.UTC().Format(time.RFC3339),
		"packageVersion": optionalObject(versionSource),
	}
	// url is URI (nullable): prefer the download URL, else the API url; leave it
	// null when the store recorded neither rather than emit an empty string.
	switch {
	case f.DownloadURL != "":
		out["url"] = f.DownloadURL
	case f.URL != "":
		out["url"] = f.URL
	}
	return out
}

// addPackageResidueFields completes GitHub's Package object with the members
// the account surface had not claimed. The Package type itself is memoized by
// gqlPackageType(); these fields are hung on it once its version sub-graph is
// buildable.
func (s *Resolver) addPackageResidueFields(types *accountSurfaceTypes) {
	pkgType := s.gqlPackageType()
	versionType := s.gqlPackageVersionType()
	versionConnection := s.gqlConnectionType("PackageVersion", versionType)

	packageStoreID := func(source interface{}) (int, bool) {
		src, ok := source.(map[string]interface{})
		if !ok {
			return 0, false
		}
		id, ok := src["_pkgStoreID"].(int)
		return id, ok
	}

	// repository: Repository — the repository a repository-owned package
	// belongs to; null for a user- or organization-owned package.
	pkgType.AddFieldConfig("repository", &graphql.Field{
		Type: s.graphqlTypes.repository,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, nil
			}
			if ownerType, _ := src["_ownerType"].(string); ownerType != "Repository" {
				return nil, nil
			}
			ownerKey, _ := src["_ownerKey"].(string)
			owner, name, ok := store.SplitRepoFullName(ownerKey)
			if !ok {
				return nil, nil
			}
			return optionalRendered(s.store.GetRepo(owner, name), func(r *store.Repo) map[string]interface{} {
				return repoToGraphQL(s.store, r)
			}), nil
		},
	})

	// statistics: PackageStatistics — bleephub records no package download
	// counter, so the total is a truthful zero.
	pkgType.AddFieldConfig("statistics", &graphql.Field{
		Type: s.gqlPackageStatisticsType(),
		Resolve: func(graphql.ResolveParams) (interface{}, error) {
			return map[string]interface{}{"downloadsTotalCount": 0}, nil
		},
	})

	// version(version: String!): PackageVersion — the named version, or null.
	pkgType.AddFieldConfig("version", &graphql.Field{
		Type: versionType,
		Args: graphql.FieldConfigArgument{
			"version": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			id, ok := packageStoreID(p.Source)
			if !ok {
				return nil, nil
			}
			want, _ := p.Args["version"].(string)
			src, _ := p.Source.(map[string]interface{})
			for _, v := range s.store.ListPackageVersions(id, false) {
				if v.Version == want {
					return packageVersionSourceMap(v, src), nil
				}
			}
			return nil, nil
		},
	})

	// latestVersion: PackageVersion — the newest version (ListPackageVersions
	// is newest-first), or null when the package has none.
	pkgType.AddFieldConfig("latestVersion", &graphql.Field{
		Type: versionType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			id, ok := packageStoreID(p.Source)
			if !ok {
				return nil, nil
			}
			versions := s.store.ListPackageVersions(id, false)
			if len(versions) == 0 {
				return nil, nil
			}
			src, _ := p.Source.(map[string]interface{})
			return packageVersionSourceMap(versions[0], src), nil
		},
	})

	// versions: PackageVersionConnection! — the package's versions.
	pkgType.AddFieldConfig("versions", &graphql.Field{
		Type: graphql.NewNonNull(versionConnection),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			id, ok := packageStoreID(p.Source)
			if !ok {
				return paginateGQLItems(nil, p.Args), nil
			}
			src, _ := p.Source.(map[string]interface{})
			var items []gqlConnItem
			for _, v := range s.store.ListPackageVersions(id, false) {
				v := v
				items = append(items, gqlConnItem{
					identity: v.NodeID,
					render:   func() map[string]interface{} { return packageVersionSourceMap(v, src) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})
}

// gqlPackageVersionType is GitHub's PackageVersion object. It is minted lazily
// because it names PackageFile, which names PackageVersion back — a cycle a
// thunk resolves. bleephub backs id/version from the store row; preRelease is a
// truthful false, platform/readme/summary are unmodeled (null), release is null
// (no package-version→release link is recorded), statistics are a truthful zero
// and the file list is truthful-empty (the store's PackageFile row carries no
// updatedAt for the non-null field GitHub declares — reported).
func (s *Resolver) gqlPackageVersionType() *graphql.Object {
	return s.mutationObjectLazy("PackageVersion", func() graphql.Fields {
		return graphql.Fields{
			"id":         &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"version":    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"platform":   &graphql.Field{Type: graphql.String},
			"readme":     &graphql.Field{Type: graphql.String},
			"summary":    &graphql.Field{Type: graphql.String},
			"release":    &graphql.Field{Type: s.graphqlTypes.release},
			"preRelease": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean), Resolve: falseFieldResolver},
			"package": &graphql.Field{
				Type: s.gqlPackageType(),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, nil
					}
					pkg, _ := src["_package"].(map[string]interface{})
					return optionalObject(pkg), nil
				},
			},
			"statistics": &graphql.Field{
				Type: s.gqlPackageVersionStatisticsType(),
				Resolve: func(graphql.ResolveParams) (interface{}, error) {
					return map[string]interface{}{"downloadsTotalCount": 0}, nil
				},
			},
			"files": &graphql.Field{
				Type: graphql.NewNonNull(s.gqlConnectionType("PackageFile", s.gqlPackageFileType())),
				Args: connectionArgs(nil),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					src, ok := p.Source.(map[string]interface{})
					if !ok {
						return paginateGQLItems(nil, p.Args), nil
					}
					versionID, ok := src["_versionStoreID"].(int)
					if !ok {
						return paginateGQLItems(nil, p.Args), nil
					}
					files := s.store.ListPackageFiles(versionID)
					var items []gqlConnItem
					for i := range files {
						f := files[i]
						items = append(items, gqlConnItem{
							identity: f.NodeID,
							render:   func() map[string]interface{} { return packageFileSourceMap(f, src) },
						})
					}
					return paginateGQLItems(items, p.Args), nil
				},
			},
		}
	})
}

// gqlPackageStatisticsType / gqlPackageVersionStatisticsType are GitHub's two
// download-count statistics objects.
func (s *Resolver) gqlPackageStatisticsType() *graphql.Object {
	return s.mutationObject("PackageStatistics", graphql.Fields{
		"downloadsTotalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	})
}

func (s *Resolver) gqlPackageVersionStatisticsType() *graphql.Object {
	return s.mutationObject("PackageVersionStatistics", graphql.Fields{
		"downloadsTotalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	})
}

// gqlPackageFileType is GitHub's PackageFile object, nodes of the
// PackageVersion.files connection. id/name/size/updatedAt/url come from the
// stored package file; md5/sha1/sha256 are checksums the store does not model
// and render null.
func (s *Resolver) gqlPackageFileType() *graphql.Object {
	return s.mutationObject("PackageFile", graphql.Fields{
		"id":             &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
		"md5":            &graphql.Field{Type: graphql.String},
		"name":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		"packageVersion": &graphql.Field{Type: s.gqlPackageVersionType()},
		"sha1":           &graphql.Field{Type: graphql.String},
		"sha256":         &graphql.Field{Type: graphql.String},
		"size":           &graphql.Field{Type: graphql.Int},
		"updatedAt":      &graphql.Field{Type: graphql.NewNonNull(s.graphQLStringScalar("DateTime"))},
		"url":            &graphql.Field{Type: s.graphQLStringScalar("URI")},
	})
}

// addRepositoryPackagesField installs Repository.packages, backed by the store's
// packages for the repository's owner key and rendered over the one shared
// PackageConnection User/Organization.packages already built.
func (s *Resolver) addRepositoryPackagesField(types *accountSurfaceTypes) {
	repoType := types.repository
	packageConnection := s.namedObject("PackageConnection")
	if repoType == nil || packageConnection == nil {
		return
	}
	repoType.AddFieldConfig("packages", &graphql.Field{
		Type: graphql.NewNonNull(packageConnection),
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromSource(p.Source)
			if err != nil {
				return nil, err
			}
			packages := s.store.ListPackages(repo.FullName)
			sort.Slice(packages, func(i, j int) bool { return packages[i].ID < packages[j].ID })
			var items []gqlConnItem
			for i := range packages {
				pkg := packages[i]
				if pkg.Deleted {
					continue
				}
				kind, ok := graphqlPackageTypeName(pkg.PackageType)
				if !ok {
					// A container package has no PackageType enum member and is
					// not representable on this surface (the REST packages API
					// serves it), matching User/Organization.packages.
					continue
				}
				items = append(items, gqlConnItem{
					identity: pkg.NodeID,
					render:   func() map[string]interface{} { return packageSourceMap(pkg, kind) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})
}

// --- Assignable interface ---------------------------------------------------

// addAssignableActorInterfaceFields adds the actor connections GitHub's
// Assignable interface declares. Issue and PullRequest already carry
// assignedActors/suggestedActors over the same shared AssigneeConnection, so
// this only completes the interface itself.
func (s *Resolver) addAssignableActorInterfaceFields() {
	iface := s.gqlAssignableInterface()
	assigneeConnection := s.sharedAssigneeConnectionType(s.graphqlTypes.user)
	iface.AddFieldConfig("assignedActors", &graphql.Field{
		Type: graphql.NewNonNull(assigneeConnection),
		Args: relayConnectionArgs(),
	})
	iface.AddFieldConfig("suggestedActors", &graphql.Field{
		Type: graphql.NewNonNull(assigneeConnection),
		Args: withArg(relayConnectionArgs(), "query", graphql.String),
	})
}

// --- Query.topic ------------------------------------------------------------

// addQueryTopicField installs Query.topic(name:), resolving a topic name onto
// the Topic object; Topic.repositories then resolves from the repositories that
// actually carry the topic.
func (s *Resolver) addQueryTopicField(queryType *graphql.Object) {
	queryType.AddFieldConfig("topic", &graphql.Field{
		Type: s.gqlTopicType(),
		Args: graphql.FieldConfigArgument{
			"name": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			name, _ := p.Args["name"].(string)
			if name == "" {
				return nil, nil
			}
			return map[string]interface{}{"name": name}, nil
		},
	})
}

// --- Ref.rules --------------------------------------------------------------

// addRefRulesField installs Ref.rules over the shared RepositoryRuleConnection.
// Attributing the rules that apply to a ref requires evaluating each ruleset's
// ref-name conditions against the branch, whose match predicate is unexported,
// so the connection is truthful-empty rather than a fabricated set (reported).
func (s *Resolver) addRefRulesField() {
	refType := s.graphqlTypes.ref
	ruleConnection := s.namedObject("RepositoryRuleConnection")
	if refType == nil || ruleConnection == nil {
		return
	}
	refType.AddFieldConfig("rules", &graphql.Field{
		Type: ruleConnection,
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return paginateGQLItems(nil, p.Args), nil
		},
	})
}

// falseFieldResolver answers a truthful false for a non-null Boolean member
// whose source never carries a value.
func falseFieldResolver(graphql.ResolveParams) (interface{}, error) { return false, nil }

package graphqlapi

// The final residue pass: members backed by data already held but not yet
// exposed over GraphQL — the Package object graph and Repository.packages, the
// Assignable interface's actor connections, Query.topic and Ref.rules. Driven
// by one call at the end of addAccountSurfaceFieldsToSchema (Query.topic wired
// separately, from the schema builder, because it needs the queryType).

import (
	"sort"
	"time"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// addFinalResidueFields installs the residue members.
func (s *Resolver) addFinalResidueFields(types *accountSurfaceTypes) {
	s.addPackageResidueFields(types)
	s.addRepositoryPackagesField(types)
	s.addAssignableActorInterfaceFields()
	s.addRefRulesField()
}

// --- Package ----------------------------------------------------------------

// packageSourceMap renders a store package into the Package source shape,
// threading the store id and owner identity for version/repository lookups.
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
// PackageVersion source shape, threading the parent package as the
// PackageVersion.package back-reference.
func packageVersionSourceMap(v *store.PackageVersion, pkgSource map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"id":              v.NodeID,
		"version":         v.Version,
		"_versionStoreID": v.ID,
		"_package":        optionalObject(pkgSource),
	}
}

// packageFileSourceMap renders a store package file into the PackageFile source
// shape, threading versionSource as the PackageFile.packageVersion
// back-reference. md5/sha1/sha256 checksums are unmodeled and render null.
func packageFileSourceMap(f *store.PackageFile, versionSource map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{
		"id":             f.NodeID,
		"name":           f.Name,
		"size":           int(f.Size),
		"updatedAt":      f.UpdatedAt.UTC().Format(time.RFC3339),
		"packageVersion": optionalObject(versionSource),
	}
	// Prefer the download URL, else the API url; leave null when neither exists.
	switch {
	case f.DownloadURL != "":
		out["url"] = f.DownloadURL
	case f.URL != "":
		out["url"] = f.URL
	}
	return out
}

// addPackageResidueFields completes the Package object with the members the
// account surface had not claimed.
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

	// repository is null for a user- or organization-owned package.
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

	// No package download counter is recorded (total is zero).
	pkgType.AddFieldConfig("statistics", &graphql.Field{
		Type: s.gqlPackageStatisticsType(),
		Resolve: func(graphql.ResolveParams) (interface{}, error) {
			return map[string]interface{}{"downloadsTotalCount": 0}, nil
		},
	})

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

	// latestVersion relies on ListPackageVersions returning newest-first.
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

// gqlPackageVersionType is GitHub's PackageVersion object, minted lazily to
// resolve the PackageFile↔PackageVersion cycle. id/version come from the store
// row; preRelease is false, platform/readme/summary/release are unmodeled (null)
// and statistics are zero.
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

// gqlPackageStatisticsType / gqlPackageVersionStatisticsType are the two
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

// gqlPackageFileType is GitHub's PackageFile object. md5/sha1/sha256 are
// unmodeled checksums and render null.
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

// addRepositoryPackagesField installs Repository.packages over the shared
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
					// not representable here (served only via REST), matching
					// User/Organization.packages.
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

// addAssignableActorInterfaceFields completes the Assignable interface with the
// actor connections Issue and PullRequest already carry.
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
// the Topic object.
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
// Attributing rules to a ref needs each ruleset's ref-name match predicate,
// which is unexported, so the connection is empty rather than fabricated.
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

// falseFieldResolver answers false for a non-null Boolean member with no source value.
func falseFieldResolver(graphql.ResolveParams) (interface{}, error) { return false, nil }

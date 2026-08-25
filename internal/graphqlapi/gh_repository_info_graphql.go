package graphqlapi

import "github.com/graphql-go/graphql"

// repositoryInfoInterface is GitHub's RepositoryInfo — the shared
// repository-shape interface a handful of fields (RepositoryInvitation.repository)
// return in place of the full Repository. Repository is its only implementer on
// this instance; every member's type is drawn from the same memoized scalar,
// enum and object instances the Repository object uses, so the interface
// conformance check sees identical types.
func (s *Resolver) repositoryInfoInterface() *graphql.Interface {
	return s.mutationInterface("RepositoryInfo", func() graphql.Fields {
		dt := s.graphQLStringScalar("DateTime")
		uri := s.graphQLStringScalar("URI")
		html := s.graphQLStringScalar("HTML")
		nnBool := func() *graphql.Field { return &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)} }
		nnStr := func() *graphql.Field { return &graphql.Field{Type: graphql.NewNonNull(graphql.String)} }
		nnURI := func() *graphql.Field { return &graphql.Field{Type: graphql.NewNonNull(uri)} }
		return graphql.Fields{
			"archivedAt":                &graphql.Field{Type: dt},
			"createdAt":                 &graphql.Field{Type: graphql.NewNonNull(dt)},
			"description":               &graphql.Field{Type: graphql.String},
			"descriptionHTML":           &graphql.Field{Type: graphql.NewNonNull(html)},
			"forkCount":                 &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"hasDiscussionsEnabled":     nnBool(),
			"hasIssuesEnabled":          nnBool(),
			"hasProjectsEnabled":        nnBool(),
			"hasPullRequestsEnabled":    nnBool(),
			"hasSponsorshipsEnabled":    nnBool(),
			"hasWikiEnabled":            nnBool(),
			"homepageUrl":               &graphql.Field{Type: uri},
			"isArchived":                nnBool(),
			"isFork":                    nnBool(),
			"isInOrganization":          nnBool(),
			"isLocked":                  nnBool(),
			"isMirror":                  nnBool(),
			"isPrivate":                 nnBool(),
			"isTemplate":                nnBool(),
			"issueCreationPolicy":       &graphql.Field{Type: s.sharedEnum("IssueCreationPolicy", "ALL", "COLLABORATORS_ONLY")},
			"licenseInfo":               &graphql.Field{Type: s.gqlLicenseType()},
			"lockReason":                &graphql.Field{Type: s.sharedEnum("RepositoryLockReason", "BILLING", "MIGRATING", "MOVING", "RENAME", "TRADE_RESTRICTION", "TRANSFERRING_OWNERSHIP")},
			"mirrorUrl":                 &graphql.Field{Type: uri},
			"name":                      nnStr(),
			"nameWithOwner":             nnStr(),
			"openGraphImageUrl":         nnURI(),
			"owner":                     &graphql.Field{Type: graphql.NewNonNull(s.graphqlTypes.repositoryOwner)},
			"pullRequestCreationPolicy": &graphql.Field{Type: s.sharedEnum("PullRequestCreationPolicy", "ALL", "COLLABORATORS_ONLY")},
			"pushedAt":                  &graphql.Field{Type: dt},
			"resourcePath":              nnURI(),
			"shortDescriptionHTML": &graphql.Field{
				Type: graphql.NewNonNull(html),
				Args: graphql.FieldConfigArgument{"limit": &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 200}},
			},
			"updatedAt":                nnDateTime(dt),
			"url":                      nnURI(),
			"usesCustomOpenGraphImage": nnBool(),
			"visibility":               &graphql.Field{Type: graphql.NewNonNull(s.sharedEnum("RepositoryVisibility", "PUBLIC", "PRIVATE", "INTERNAL"))},
		}
	}, func(p graphql.ResolveTypeParams) *graphql.Object {
		// Repository is the only implementer.
		return s.graphqlTypes.repository
	})
}

func nnDateTime(dt *graphql.Scalar) *graphql.Field {
	return &graphql.Field{Type: graphql.NewNonNull(dt)}
}

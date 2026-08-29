package graphqlapi

// User.contributionsCollection: the ContributionsCollection type graph and the
// aggregate computed from store data by store.ComputeContributions. Memoization
// goes through the shared mutationObject/mutationUnion/sharedEnum helpers.

import (
	"fmt"
	"sort"
	"time"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// GitHub's calendar green ramp, indexed by ContributionLevel (0=NONE … 4).
var contributionColorRamp = []string{"#ebedf0", "#9be9a8", "#40c463", "#30a14e", "#216e39"}

var contributionLevelNames = []string{"NONE", "FIRST_QUARTILE", "SECOND_QUARTILE", "THIRD_QUARTILE", "FOURTH_QUARTILE"}

// enums & inputs

func (s *Resolver) contributionLevelEnum() *graphql.Enum {
	return s.sharedEnum("ContributionLevel", "FIRST_QUARTILE", "FOURTH_QUARTILE", "NONE", "SECOND_QUARTILE", "THIRD_QUARTILE")
}

func (s *Resolver) contributionOrderInput() *graphql.InputObject {
	return s.mutationInput("ContributionOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(s.sharedEnum("OrderDirection", "ASC", "DESC")),
	})
}

func (s *Resolver) commitContributionOrderInput() *graphql.InputObject {
	return s.mutationInput("CommitContributionOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(s.sharedEnum("OrderDirection", "ASC", "DESC")),
		"field":     gqlNonNullInputOf(s.sharedEnum("CommitContributionOrderField", "COMMIT_COUNT", "OCCURRED_AT")),
	})
}

// the Contribution interface & its implementers

func (s *Resolver) contributionInterface() *graphql.Interface {
	return s.mutationInterface("Contribution", func() graphql.Fields {
		dateTime := s.graphQLStringScalar("DateTime")
		uri := s.graphQLStringScalar("URI")
		return graphql.Fields{
			"isRestricted": gqlNonNull(graphql.Boolean),
			"occurredAt":   gqlNonNull(dateTime),
			"resourcePath": gqlNonNull(uri),
			"url":          gqlNonNull(uri),
			"user":         gqlNonNull(s.graphqlTypes.user),
		}
	}, func(p graphql.ResolveTypeParams) *graphql.Object {
		return s.contributionObjectFor(p.Value)
	})
}

// contributionObjectFor dispatches a source map to its concrete object by the
// __typename tag every renderer writes.
func (s *Resolver) contributionObjectFor(value interface{}) *graphql.Object {
	src, _ := value.(map[string]interface{})
	switch src["__typename"] {
	case "CreatedIssueContribution":
		return s.createdIssueContributionType()
	case "CreatedPullRequestContribution":
		return s.createdPullRequestContributionType()
	case "CreatedPullRequestReviewContribution":
		return s.createdPullRequestReviewContributionType()
	case "CreatedRepositoryContribution":
		return s.createdRepositoryContributionType()
	case "CreatedCommitContribution":
		return s.createdCommitContributionType()
	case "JoinedGitHubContribution":
		return s.joinedGitHubContributionType()
	default:
		return s.restrictedContributionType()
	}
}

// contribObject memoizes a Contribution-implementing object. mutationObjectLazy
// cannot set Interfaces, so this mints the object directly and records it in the
// s.mutationObjects registry the shared helpers use.
func (s *Resolver) contribObject(name string, build func() graphql.Fields) *graphql.Object {
	if existing := s.memoizedMutationObject(name); existing != nil {
		return existing
	}
	obj := graphql.NewObject(graphql.ObjectConfig{
		Name:       name,
		Interfaces: []*graphql.Interface{s.contributionInterface()},
		Fields:     graphql.FieldsThunk(build),
	})
	s.mutationObjects[name] = obj
	return obj
}

// contributionCommonFields is the five-field Contribution shape every
// implementer declares, with the exact interface types.
func (s *Resolver) contributionCommonFields() graphql.Fields {
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	return graphql.Fields{
		"isRestricted": gqlNonNull(graphql.Boolean),
		"occurredAt":   gqlNonNull(dateTime),
		"resourcePath": gqlNonNull(uri),
		"url":          gqlNonNull(uri),
		"user":         gqlNonNull(s.graphqlTypes.user),
	}
}

func (s *Resolver) createdIssueContributionType() *graphql.Object {
	return s.contribObject("CreatedIssueContribution", func() graphql.Fields {
		f := s.contributionCommonFields()
		f["issue"] = gqlNonNull(s.graphqlTypes.issue)
		return f
	})
}

func (s *Resolver) createdPullRequestContributionType() *graphql.Object {
	return s.contribObject("CreatedPullRequestContribution", func() graphql.Fields {
		f := s.contributionCommonFields()
		f["pullRequest"] = gqlNonNull(s.graphqlTypes.pullRequest)
		return f
	})
}

func (s *Resolver) createdPullRequestReviewContributionType() *graphql.Object {
	return s.contribObject("CreatedPullRequestReviewContribution", func() graphql.Fields {
		f := s.contributionCommonFields()
		f["pullRequest"] = gqlNonNull(s.graphqlTypes.pullRequest)
		f["pullRequestReview"] = gqlNonNull(s.graphqlTypes.pullRequestReview)
		f["repository"] = gqlNonNull(s.graphqlTypes.repository)
		return f
	})
}

func (s *Resolver) createdRepositoryContributionType() *graphql.Object {
	return s.contribObject("CreatedRepositoryContribution", func() graphql.Fields {
		f := s.contributionCommonFields()
		f["repository"] = gqlNonNull(s.graphqlTypes.repository)
		return f
	})
}

func (s *Resolver) createdCommitContributionType() *graphql.Object {
	return s.contribObject("CreatedCommitContribution", func() graphql.Fields {
		f := s.contributionCommonFields()
		f["commitCount"] = gqlNonNull(graphql.Int)
		f["repository"] = gqlNonNull(s.graphqlTypes.repository)
		return f
	})
}

func (s *Resolver) joinedGitHubContributionType() *graphql.Object {
	return s.contribObject("JoinedGitHubContribution", func() graphql.Fields {
		return s.contributionCommonFields()
	})
}

func (s *Resolver) restrictedContributionType() *graphql.Object {
	return s.contribObject("RestrictedContribution", func() graphql.Fields {
		return s.contributionCommonFields()
	})
}

// unions

func (s *Resolver) createdIssueOrRestrictedUnion() *graphql.Union {
	return s.mutationUnion("CreatedIssueOrRestrictedContribution", func() []*graphql.Object {
		return []*graphql.Object{s.createdIssueContributionType(), s.restrictedContributionType()}
	}, func(p graphql.ResolveTypeParams) *graphql.Object {
		return s.contributionObjectFor(p.Value)
	})
}

func (s *Resolver) createdPullRequestOrRestrictedUnion() *graphql.Union {
	return s.mutationUnion("CreatedPullRequestOrRestrictedContribution", func() []*graphql.Object {
		return []*graphql.Object{s.createdPullRequestContributionType(), s.restrictedContributionType()}
	}, func(p graphql.ResolveTypeParams) *graphql.Object {
		return s.contributionObjectFor(p.Value)
	})
}

func (s *Resolver) createdRepositoryOrRestrictedUnion() *graphql.Union {
	return s.mutationUnion("CreatedRepositoryOrRestrictedContribution", func() []*graphql.Object {
		return []*graphql.Object{s.createdRepositoryContributionType(), s.restrictedContributionType()}
	}, func(p graphql.ResolveTypeParams) *graphql.Object {
		return s.contributionObjectFor(p.Value)
	})
}

// connections

func (s *Resolver) contributionConnectionType(name string, node *graphql.Object) *graphql.Object {
	return s.mutationObject(name, graphql.Fields{
		// GitHub declares these edges as [Edge] (nullable elements), not [Edge!].
		"edges":      &graphql.Field{Type: graphql.NewList(s.contributionEdgeType(name[:len(name)-len("Connection")]+"Edge", node))},
		"nodes":      &graphql.Field{Type: graphql.NewList(node)},
		"pageInfo":   gqlNonNull(s.gqlPageInfoType()),
		"totalCount": gqlNonNull(graphql.Int),
	})
}

func (s *Resolver) contributionEdgeType(name string, node *graphql.Object) *graphql.Object {
	return s.mutationObject(name, graphql.Fields{
		"cursor": gqlNonNull(graphql.String),
		"node":   &graphql.Field{Type: node},
	})
}

func (s *Resolver) createdIssueContributionConnection() *graphql.Object {
	return s.contributionConnectionType("CreatedIssueContributionConnection", s.createdIssueContributionType())
}

func (s *Resolver) createdPullRequestContributionConnection() *graphql.Object {
	return s.contributionConnectionType("CreatedPullRequestContributionConnection", s.createdPullRequestContributionType())
}

func (s *Resolver) createdPullRequestReviewContributionConnection() *graphql.Object {
	return s.contributionConnectionType("CreatedPullRequestReviewContributionConnection", s.createdPullRequestReviewContributionType())
}

func (s *Resolver) createdRepositoryContributionConnection() *graphql.Object {
	return s.contributionConnectionType("CreatedRepositoryContributionConnection", s.createdRepositoryContributionType())
}

func (s *Resolver) createdCommitContributionConnection() *graphql.Object {
	return s.contributionConnectionType("CreatedCommitContributionConnection", s.createdCommitContributionType())
}

// *ByRepository groupings

// contributionsConnectionField is the `contributions(...)` connection every
// *ByRepository object declares, paginating the group's pre-rendered node list.
func (s *Resolver) contributionsConnectionField(connection *graphql.Object, order graphql.Input) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(connection),
		Args: graphql.FieldConfigArgument{
			"after":   &graphql.ArgumentConfig{Type: graphql.String},
			"before":  &graphql.ArgumentConfig{Type: graphql.String},
			"first":   &graphql.ArgumentConfig{Type: graphql.Int},
			"last":    &graphql.ArgumentConfig{Type: graphql.Int},
			"orderBy": &graphql.ArgumentConfig{Type: order},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, _ := p.Source.(map[string]interface{})
			nodes, _ := src["_contribNodes"].([]map[string]interface{})
			return paginateContributionNodes(nodes, p.Args), nil
		},
	}
}

func (s *Resolver) commitContributionsByRepositoryType() *graphql.Object {
	return s.mutationObjectLazy("CommitContributionsByRepository", func() graphql.Fields {
		uri := s.graphQLStringScalar("URI")
		return graphql.Fields{
			"contributions": s.contributionsConnectionField(s.createdCommitContributionConnection(), s.commitContributionOrderInput()),
			"repository":    gqlNonNull(s.graphqlTypes.repository),
			"resourcePath":  gqlNonNull(uri),
			"url":           gqlNonNull(uri),
		}
	})
}

func (s *Resolver) issueContributionsByRepositoryType() *graphql.Object {
	return s.mutationObjectLazy("IssueContributionsByRepository", func() graphql.Fields {
		return graphql.Fields{
			"contributions": s.contributionsConnectionField(s.createdIssueContributionConnection(), s.contributionOrderInput()),
			"repository":    gqlNonNull(s.graphqlTypes.repository),
		}
	})
}

func (s *Resolver) pullRequestContributionsByRepositoryType() *graphql.Object {
	return s.mutationObjectLazy("PullRequestContributionsByRepository", func() graphql.Fields {
		return graphql.Fields{
			"contributions": s.contributionsConnectionField(s.createdPullRequestContributionConnection(), s.contributionOrderInput()),
			"repository":    gqlNonNull(s.graphqlTypes.repository),
		}
	})
}

func (s *Resolver) pullRequestReviewContributionsByRepositoryType() *graphql.Object {
	return s.mutationObjectLazy("PullRequestReviewContributionsByRepository", func() graphql.Fields {
		return graphql.Fields{
			"contributions": s.contributionsConnectionField(s.createdPullRequestReviewContributionConnection(), s.contributionOrderInput()),
			"repository":    gqlNonNull(s.graphqlTypes.repository),
		}
	})
}

// calendar

func (s *Resolver) contributionCalendarType() *graphql.Object {
	return s.mutationObject("ContributionCalendar", graphql.Fields{
		"colors":             gqlNonNull(graphql.NewList(graphql.NewNonNull(graphql.String))),
		"isHalloween":        gqlNonNull(graphql.Boolean),
		"months":             gqlNonNull(graphql.NewList(graphql.NewNonNull(s.contributionCalendarMonthType()))),
		"totalContributions": gqlNonNull(graphql.Int),
		"weeks":              gqlNonNull(graphql.NewList(graphql.NewNonNull(s.contributionCalendarWeekType()))),
	})
}

func (s *Resolver) contributionCalendarWeekType() *graphql.Object {
	date := s.graphQLStringScalar("Date")
	return s.mutationObject("ContributionCalendarWeek", graphql.Fields{
		"contributionDays": gqlNonNull(graphql.NewList(graphql.NewNonNull(s.contributionCalendarDayType()))),
		"firstDay":         gqlNonNull(date),
	})
}

func (s *Resolver) contributionCalendarDayType() *graphql.Object {
	date := s.graphQLStringScalar("Date")
	return s.mutationObject("ContributionCalendarDay", graphql.Fields{
		"color":             gqlNonNull(graphql.String),
		"contributionCount": gqlNonNull(graphql.Int),
		"contributionLevel": gqlNonNull(s.contributionLevelEnum()),
		"date":              gqlNonNull(date),
		"weekday":           gqlNonNull(graphql.Int),
	})
}

func (s *Resolver) contributionCalendarMonthType() *graphql.Object {
	date := s.graphQLStringScalar("Date")
	return s.mutationObject("ContributionCalendarMonth", graphql.Fields{
		"firstDay":   gqlNonNull(date),
		"name":       gqlNonNull(graphql.String),
		"totalWeeks": gqlNonNull(graphql.Int),
		"year":       gqlNonNull(graphql.Int),
	})
}

// the collection

func (s *Resolver) gqlContributionsCollectionType() *graphql.Object {
	return s.mutationObjectLazy("ContributionsCollection", func() graphql.Fields {
		dateTime := s.graphQLStringScalar("DateTime")
		date := s.graphQLStringScalar("Date")

		fields := graphql.Fields{
			"contributionCalendar":                               gqlNonNull(s.contributionCalendarType()),
			"contributionYears":                                  gqlNonNull(graphql.NewList(graphql.NewNonNull(graphql.Int))),
			"doesEndInCurrentMonth":                              gqlNonNull(graphql.Boolean),
			"earliestRestrictedContributionDate":                 gqlField(date),
			"endedAt":                                            gqlNonNull(dateTime),
			"hasActivityInThePast":                               gqlNonNull(graphql.Boolean),
			"hasAnyContributions":                                gqlNonNull(graphql.Boolean),
			"hasAnyRestrictedContributions":                      gqlNonNull(graphql.Boolean),
			"isSingleDay":                                        gqlNonNull(graphql.Boolean),
			"joinedGitHubContribution":                           gqlField(s.joinedGitHubContributionType()),
			"latestRestrictedContributionDate":                   gqlField(date),
			"mostRecentCollectionWithActivity":                   gqlField(s.gqlContributionsCollectionType()),
			"mostRecentCollectionWithoutActivity":                gqlField(s.gqlContributionsCollectionType()),
			"popularIssueContribution":                           gqlField(s.createdIssueContributionType()),
			"popularPullRequestContribution":                     gqlField(s.createdPullRequestContributionType()),
			"restrictedContributionsCount":                       gqlNonNull(graphql.Int),
			"startedAt":                                          gqlNonNull(dateTime),
			"totalCommitContributions":                           gqlNonNull(graphql.Int),
			"totalPullRequestReviewContributions":                gqlNonNull(graphql.Int),
			"totalRepositoriesWithContributedCommits":            gqlNonNull(graphql.Int),
			"totalRepositoriesWithContributedPullRequestReviews": gqlNonNull(graphql.Int),
			"user": gqlNonNull(s.graphqlTypes.user),

			"firstIssueContribution":       gqlField(s.createdIssueOrRestrictedUnion()),
			"firstPullRequestContribution": gqlField(s.createdPullRequestOrRestrictedUnion()),
			"firstRepositoryContribution":  gqlField(s.createdRepositoryOrRestrictedUnion()),
		}

		fields["commitContributionsByRepository"] = &graphql.Field{
			Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(s.commitContributionsByRepositoryType()))),
			Args: graphql.FieldConfigArgument{
				"maxRepositories": &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 25},
			},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return limitByRepository(sourceGroups(p.Source, "_commitByRepo"), maxReposArg(p.Args)), nil
			},
		}

		fields["issueContributions"] = s.excludableConnectionField(s.createdIssueContributionConnection(), "_issueNodes", true)
		fields["pullRequestContributions"] = s.excludableConnectionField(s.createdPullRequestContributionConnection(), "_prNodes", true)
		fields["pullRequestReviewContributions"] = s.plainConnectionField(s.createdPullRequestReviewContributionConnection(), "_reviewNodes")
		fields["repositoryContributions"] = s.excludableConnectionField(s.createdRepositoryContributionConnection(), "_repoNodes", false)

		fields["issueContributionsByRepository"] = s.byRepositoryField(s.issueContributionsByRepositoryType(), "_issueByRepo", true)
		fields["pullRequestContributionsByRepository"] = s.byRepositoryField(s.pullRequestContributionsByRepositoryType(), "_prByRepo", true)
		fields["pullRequestReviewContributionsByRepository"] = s.byRepositoryMaxOnlyField(s.pullRequestReviewContributionsByRepositoryType(), "_reviewByRepo")

		fields["totalIssueContributions"] = s.excludableCountField("_issueNodes")
		fields["totalPullRequestContributions"] = s.excludableCountField("_prNodes")
		fields["totalRepositoriesWithContributedIssues"] = s.excludableRepoCountField("_issueNodes")
		fields["totalRepositoriesWithContributedPullRequests"] = s.excludableRepoCountField("_prNodes")
		fields["totalRepositoryContributions"] = s.excludeFirstCountField("_repoNodes")

		return fields
	})
}

// collection field builders

func excludeArgs() graphql.FieldConfigArgument {
	return graphql.FieldConfigArgument{
		"excludeFirst":   &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false},
		"excludePopular": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false},
	}
}

func contributionConnArgs(extra graphql.FieldConfigArgument, order graphql.Input) graphql.FieldConfigArgument {
	args := graphql.FieldConfigArgument{
		"after":   &graphql.ArgumentConfig{Type: graphql.String},
		"before":  &graphql.ArgumentConfig{Type: graphql.String},
		"first":   &graphql.ArgumentConfig{Type: graphql.Int},
		"last":    &graphql.ArgumentConfig{Type: graphql.Int},
		"orderBy": &graphql.ArgumentConfig{Type: order},
	}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// excludableConnectionField honors the excludeFirst/excludePopular arguments;
// repository carries excludeFirst only (popular==false).
func (s *Resolver) excludableConnectionField(connection *graphql.Object, key string, popular bool) *graphql.Field {
	extra := graphql.FieldConfigArgument{"excludeFirst": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false}}
	if popular {
		extra["excludePopular"] = &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false}
	}
	return &graphql.Field{
		Type: graphql.NewNonNull(connection),
		Args: contributionConnArgs(extra, s.contributionOrderInput()),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			nodes := filterExcluded(sourceNodes(p.Source, key), p.Source, p.Args)
			return paginateContributionNodes(nodes, p.Args), nil
		},
	}
}

func (s *Resolver) plainConnectionField(connection *graphql.Object, key string) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(connection),
		Args: contributionConnArgs(nil, s.contributionOrderInput()),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return paginateContributionNodes(sourceNodes(p.Source, key), p.Args), nil
		},
	}
}

func (s *Resolver) byRepositoryField(object *graphql.Object, key string, popular bool) *graphql.Field {
	extra := graphql.FieldConfigArgument{
		"excludeFirst":    &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false},
		"maxRepositories": &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 25},
	}
	if popular {
		extra["excludePopular"] = &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false}
	}
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(object))),
		Args: extra,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			groups := excludeFromGroups(sourceGroups(p.Source, key), p.Source, p.Args)
			return limitByRepository(groups, maxReposArg(p.Args)), nil
		},
	}
}

func (s *Resolver) byRepositoryMaxOnlyField(object *graphql.Object, key string) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(object))),
		Args: graphql.FieldConfigArgument{
			"maxRepositories": &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 25},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return limitByRepository(sourceGroups(p.Source, key), maxReposArg(p.Args)), nil
		},
	}
}

func (s *Resolver) excludableCountField(key string) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.Int),
		Args: excludeArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return len(filterExcluded(sourceNodes(p.Source, key), p.Source, p.Args)), nil
		},
	}
}

func (s *Resolver) excludeFirstCountField(key string) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.Int),
		Args: graphql.FieldConfigArgument{"excludeFirst": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return len(filterExcluded(sourceNodes(p.Source, key), p.Source, p.Args)), nil
		},
	}
}

func (s *Resolver) excludableRepoCountField(key string) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewNonNull(graphql.Int),
		Args: excludeArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			nodes := filterExcluded(sourceNodes(p.Source, key), p.Source, p.Args)
			repos := map[interface{}]bool{}
			for _, n := range nodes {
				repos[n["_repoID"]] = true
			}
			return len(repos), nil
		},
	}
}

// source-map access & filtering

func sourceNodes(source interface{}, key string) []map[string]interface{} {
	src, _ := source.(map[string]interface{})
	nodes, _ := src[key].([]map[string]interface{})
	return nodes
}

func sourceGroups(source interface{}, key string) []map[string]interface{} {
	return sourceNodes(source, key)
}

func maxReposArg(args map[string]interface{}) int {
	if n, ok := intArg(args, "maxRepositories"); ok {
		return n
	}
	return 25
}

// filterExcluded drops the first-ever and/or most-commented node per the
// excludeFirst/excludePopular arguments.
func filterExcluded(nodes []map[string]interface{}, source interface{}, args map[string]interface{}) []map[string]interface{} {
	excludeFirst, _ := args["excludeFirst"].(bool)
	excludePopular, _ := args["excludePopular"].(bool)
	if !excludeFirst && !excludePopular {
		return nodes
	}
	src, _ := source.(map[string]interface{})
	firstID, popularID := src[firstKeyFor(nodes)], src[popularKeyFor(nodes)]
	out := make([]map[string]interface{}, 0, len(nodes))
	for _, n := range nodes {
		if excludeFirst && firstID != nil && n["_dbID"] == firstID {
			continue
		}
		if excludePopular && popularID != nil && n["_dbID"] == popularID {
			continue
		}
		out = append(out, n)
	}
	return out
}

// firstKeyFor / popularKeyFor name the source keys holding the first/popular
// node id, keyed by the list's contribution __typename.
func firstKeyFor(nodes []map[string]interface{}) string {
	return "_first" + contributionKind(nodes)
}
func popularKeyFor(nodes []map[string]interface{}) string {
	return "_popular" + contributionKind(nodes)
}
func contributionKind(nodes []map[string]interface{}) string {
	if len(nodes) == 0 {
		return ""
	}
	switch nodes[0]["__typename"] {
	case "CreatedIssueContribution":
		return "Issue"
	case "CreatedPullRequestContribution":
		return "PR"
	case "CreatedRepositoryContribution":
		return "Repo"
	}
	return ""
}

// excludeFromGroups applies exclude arguments per-group, dropping groups that
// become empty.
func excludeFromGroups(groups []map[string]interface{}, source interface{}, args map[string]interface{}) []map[string]interface{} {
	excludeFirst, _ := args["excludeFirst"].(bool)
	excludePopular, _ := args["excludePopular"].(bool)
	if !excludeFirst && !excludePopular {
		return groups
	}
	out := make([]map[string]interface{}, 0, len(groups))
	for _, g := range groups {
		nodes, _ := g["_contribNodes"].([]map[string]interface{})
		filtered := filterExcluded(nodes, source, args)
		if len(filtered) == 0 {
			continue
		}
		clone := map[string]interface{}{}
		for k, v := range g {
			clone[k] = v
		}
		clone["_contribNodes"] = filtered
		out = append(out, clone)
	}
	return out
}

func limitByRepository(groups []map[string]interface{}, max int) []interface{} {
	if max < 0 {
		max = 0
	}
	if max > len(groups) {
		max = len(groups)
	}
	out := make([]interface{}, 0, max)
	for i := 0; i < max; i++ {
		out = append(out, groups[i])
	}
	return out
}

// paginateContributionNodes windows a pre-rendered node list into a Relay
// connection source.
func paginateContributionNodes(nodes []map[string]interface{}, args map[string]interface{}) map[string]interface{} {
	items := make([]gqlConnItem, len(nodes))
	for i := range nodes {
		node := nodes[i]
		items[i] = gqlConnItem{
			identity: fmt.Sprint(node["_dbID"]),
			render:   func() map[string]interface{} { return node },
		}
	}
	return paginateGQLItems(items, args)
}

// the aggregate source

// contributionsCollectionSource computes the aggregate for the window and
// returns the ContributionsCollection source map. A zero from or to means the
// trailing 52 weeks ending at the store's current time.
func (s *Resolver) contributionsCollectionSource(userID int, from, to time.Time, orgID int) map[string]interface{} {
	if from.IsZero() || to.IsZero() {
		to = s.store.CurrentTime()
		from = to.AddDate(0, 0, -7*52)
	}
	data := s.store.ComputeContributions(userID, from, to, orgID)

	userSource := map[string]interface{}{}
	if data.User != nil {
		userSource = userToGraphQL(data.User)
	}

	issueNodes := s.renderIssueContributions(data, userSource)
	prNodes := s.renderPullRequestContributions(data, userSource)
	reviewNodes := s.renderReviewContributions(data, userSource)
	repoNodes := s.renderRepositoryContributions(data, userSource)

	source := map[string]interface{}{
		"user":      userSource,
		"startedAt": from.UTC().Format(time.RFC3339),
		"endedAt":   to.UTC().Format(time.RFC3339),

		"totalCommitContributions":                           data.TotalCommits,
		"totalPullRequestReviewContributions":                len(reviewNodes),
		"totalRepositoriesWithContributedCommits":            len(data.ReposWithCommits),
		"totalRepositoriesWithContributedPullRequestReviews": len(data.ReposWithReviews),

		"hasAnyContributions":           data.TotalCommits+len(issueNodes)+len(prNodes)+len(reviewNodes)+len(repoNodes) > 0,
		"hasAnyRestrictedContributions": false,
		"restrictedContributionsCount":  0,

		"contributionYears":    data.ContributionYears,
		"hasActivityInThePast": data.HasActivityInThePast,

		"doesEndInCurrentMonth": to.UTC().Year() == data.Now.UTC().Year() && to.UTC().Month() == data.Now.UTC().Month(),
		"isSingleDay":           store.ContributionDate(from) == store.ContributionDate(to),

		"earliestRestrictedContributionDate":  nil,
		"latestRestrictedContributionDate":    nil,
		"mostRecentCollectionWithActivity":    nil,
		"mostRecentCollectionWithoutActivity": nil,

		"contributionCalendar": s.renderContributionCalendar(data, from, to),

		// Pre-rendered node lists the connection/byRepository/count resolvers
		// window and filter.
		"_issueNodes":  issueNodes,
		"_prNodes":     prNodes,
		"_reviewNodes": reviewNodes,
		"_repoNodes":   repoNodes,

		"_issueByRepo":  groupByRepository(issueNodes, s.store),
		"_prByRepo":     groupByRepository(prNodes, s.store),
		"_reviewByRepo": groupByRepository(reviewNodes, s.store),
		"_commitByRepo": s.renderCommitContributionsByRepository(data, userSource),
	}

	// first-ever / popular ids for the exclude arguments.
	source["_firstIssue"] = firstDBID(data.FirstIssue, from, to, func() int { return data.FirstIssue.ID })
	source["_firstPR"] = firstDBID(data.FirstPR, from, to, func() int { return data.FirstPR.ID })
	source["_firstRepo"] = firstDBID(data.FirstRepo, from, to, func() int { return data.FirstRepo.ID })
	if data.PopularIssue != nil {
		source["_popularIssue"] = data.PopularIssue.ID
	}
	if data.PopularPR != nil {
		source["_popularPR"] = data.PopularPR.ID
	}

	source["firstIssueContribution"] = optionalObject(s.renderFirstIssue(data, userSource, from, to))
	source["firstPullRequestContribution"] = optionalObject(s.renderFirstPullRequest(data, userSource, from, to))
	source["firstRepositoryContribution"] = optionalObject(s.renderFirstRepository(data, userSource, from, to))

	source["popularIssueContribution"] = optionalObject(s.renderPopularIssue(data, userSource))
	source["popularPullRequestContribution"] = optionalObject(s.renderPopularPullRequest(data, userSource))

	// Non-null only when the join falls in the window.
	source["joinedGitHubContribution"] = optionalObject(s.renderJoinedGitHub(data, userSource, from, to))

	return source
}

func firstDBID[T any](record *T, from, to time.Time, id func() int) interface{} {
	if record == nil {
		return nil
	}
	return id()
}

// contribution node renderers

func (s *Resolver) baseContribution(typename, resourcePath string, occurredAt time.Time, userSource map[string]interface{}, dbID int) map[string]interface{} {
	return map[string]interface{}{
		"__typename":   typename,
		"isRestricted": false,
		"occurredAt":   occurredAt.UTC().Format(time.RFC3339),
		"resourcePath": resourcePath,
		"url":          externalURL(resourcePath),
		"user":         userSource,
		"_dbID":        dbID,
	}
}

func (s *Resolver) renderIssueContributions(data *store.ContributionData, userSource map[string]interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(data.Issues))
	for _, issue := range data.Issues {
		repo := s.store.GetRepoByID(issue.RepoID)
		path := fmt.Sprintf("/%s/issues/%d", repoFullName(repo), issue.Number)
		node := s.baseContribution("CreatedIssueContribution", path, issue.CreatedAt, userSource, issue.ID)
		node["issue"] = issueToGQL(issue, s.store)
		node["_repoID"] = issue.RepoID
		node["_repo"] = repo
		out = append(out, node)
	}
	return out
}

func (s *Resolver) renderPullRequestContributions(data *store.ContributionData, userSource map[string]interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(data.PullRequests))
	for _, pr := range data.PullRequests {
		repo := s.store.GetRepoByID(pr.RepoID)
		path := fmt.Sprintf("/%s/pull/%d", repoFullName(repo), pr.Number)
		node := s.baseContribution("CreatedPullRequestContribution", path, pr.CreatedAt, userSource, pr.ID)
		node["pullRequest"] = pullRequestToGQL(pr, s.store)
		node["_repoID"] = pr.RepoID
		node["_repo"] = repo
		out = append(out, node)
	}
	return out
}

func (s *Resolver) renderReviewContributions(data *store.ContributionData, userSource map[string]interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(data.Reviews))
	for _, review := range data.Reviews {
		repoID := data.ReviewRepoID[review.ID]
		repo := s.store.GetRepoByID(repoID)
		pr := s.store.GetPullRequest(review.PRID)
		occurred := review.CreatedAt
		if review.SubmittedAt != nil {
			occurred = *review.SubmittedAt
		}
		number := 0
		if pr != nil {
			number = pr.Number
		}
		path := fmt.Sprintf("/%s/pull/%d", repoFullName(repo), number)
		node := s.baseContribution("CreatedPullRequestReviewContribution", path, occurred, userSource, review.ID)
		node["pullRequestReview"] = prReviewToGQL(review, s.store)
		node["repository"] = optionalRendered(repo, func(r *store.Repo) map[string]interface{} { return repoToGraphQL(s.store, r) })
		if pr != nil {
			node["pullRequest"] = pullRequestToGQL(pr, s.store)
		}
		node["_repoID"] = repoID
		node["_repo"] = repo
		out = append(out, node)
	}
	return out
}

func (s *Resolver) renderRepositoryContributions(data *store.ContributionData, userSource map[string]interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(data.Repos))
	for _, repo := range data.Repos {
		path := "/" + repo.FullName
		node := s.baseContribution("CreatedRepositoryContribution", path, repo.CreatedAt, userSource, repo.ID)
		node["repository"] = repoToGraphQL(s.store, repo)
		node["_repoID"] = repo.ID
		node["_repo"] = repo
		out = append(out, node)
	}
	return out
}

func (s *Resolver) renderCommitContributionsByRepository(data *store.ContributionData, userSource map[string]interface{}) []map[string]interface{} {
	byRepo := map[int][]store.CommitContributionDay{}
	order := []int{}
	for _, day := range data.CommitDays {
		if _, seen := byRepo[day.RepoID]; !seen {
			order = append(order, day.RepoID)
		}
		byRepo[day.RepoID] = append(byRepo[day.RepoID], day)
	}
	login := ""
	if data.User != nil {
		login = data.User.Login
	}
	groups := make([]map[string]interface{}, 0, len(order))
	for _, repoID := range order {
		repo := s.store.GetRepoByID(repoID)
		days := byRepo[repoID]
		nodes := make([]map[string]interface{}, 0, len(days))
		total := 0
		for _, day := range days {
			total += day.Count
			path := fmt.Sprintf("/%s/commits?author=%s", repoFullName(repo), login)
			node := s.baseContribution("CreatedCommitContribution", path, day.OccurredAt, userSource, dayID(repoID, day.Date))
			node["commitCount"] = day.Count
			node["repository"] = optionalRendered(repo, func(r *store.Repo) map[string]interface{} { return repoToGraphQL(s.store, r) })
			node["_repoID"] = repoID
			nodes = append(nodes, node)
		}
		path := fmt.Sprintf("/%s/commits?author=%s", repoFullName(repo), login)
		groups = append(groups, map[string]interface{}{
			"repository":    optionalRendered(repo, func(r *store.Repo) map[string]interface{} { return repoToGraphQL(s.store, r) }),
			"resourcePath":  path,
			"url":           externalURL(path),
			"_contribNodes": nodes,
			"_dbID":         repoID,
			"_repoID":       repoID,
			"_commitTotal":  total,
			"_contribCount": len(nodes),
		})
	}
	// Most commits first, then repo id for stability.
	sort.SliceStable(groups, func(a, b int) bool {
		ca, _ := groups[a]["_commitTotal"].(int)
		cb, _ := groups[b]["_commitTotal"].(int)
		if ca != cb {
			return ca > cb
		}
		ida, _ := groups[a]["_repoID"].(int)
		idb, _ := groups[b]["_repoID"].(int)
		return ida < idb
	})
	return groups
}

// groupByRepository groups rendered nodes into *ByRepository source maps,
// ordered by contribution count descending then repository id.
func groupByRepository(nodes []map[string]interface{}, st *store.Store) []map[string]interface{} {
	byRepo := map[int][]map[string]interface{}{}
	order := []int{}
	for _, node := range nodes {
		repoID, _ := node["_repoID"].(int)
		if _, seen := byRepo[repoID]; !seen {
			order = append(order, repoID)
		}
		byRepo[repoID] = append(byRepo[repoID], node)
	}
	groups := make([]map[string]interface{}, 0, len(order))
	for _, repoID := range order {
		repo := st.GetRepoByID(repoID)
		groups = append(groups, map[string]interface{}{
			"repository":    optionalRendered(repo, func(r *store.Repo) map[string]interface{} { return repoToGraphQL(st, r) }),
			"_contribNodes": byRepo[repoID],
			"_dbID":         repoID,
			"_repoID":       repoID,
			"_contribCount": len(byRepo[repoID]),
		})
	}
	sort.SliceStable(groups, func(a, b int) bool {
		ca, _ := groups[a]["_contribCount"].(int)
		cb, _ := groups[b]["_contribCount"].(int)
		if ca != cb {
			return ca > cb
		}
		return groups[a]["_repoID"].(int) < groups[b]["_repoID"].(int)
	})
	return groups
}

// first / popular / joined renderers

func (s *Resolver) renderFirstIssue(data *store.ContributionData, userSource map[string]interface{}, from, to time.Time) map[string]interface{} {
	if data.FirstIssue == nil || data.FirstIssue.CreatedAt.Before(from) || data.FirstIssue.CreatedAt.After(to) {
		return nil
	}
	repo := s.store.GetRepoByID(data.FirstIssue.RepoID)
	path := fmt.Sprintf("/%s/issues/%d", repoFullName(repo), data.FirstIssue.Number)
	node := s.baseContribution("CreatedIssueContribution", path, data.FirstIssue.CreatedAt, userSource, data.FirstIssue.ID)
	node["issue"] = issueToGQL(data.FirstIssue, s.store)
	return node
}

func (s *Resolver) renderFirstPullRequest(data *store.ContributionData, userSource map[string]interface{}, from, to time.Time) map[string]interface{} {
	if data.FirstPR == nil || data.FirstPR.CreatedAt.Before(from) || data.FirstPR.CreatedAt.After(to) {
		return nil
	}
	repo := s.store.GetRepoByID(data.FirstPR.RepoID)
	path := fmt.Sprintf("/%s/pull/%d", repoFullName(repo), data.FirstPR.Number)
	node := s.baseContribution("CreatedPullRequestContribution", path, data.FirstPR.CreatedAt, userSource, data.FirstPR.ID)
	node["pullRequest"] = pullRequestToGQL(data.FirstPR, s.store)
	return node
}

func (s *Resolver) renderFirstRepository(data *store.ContributionData, userSource map[string]interface{}, from, to time.Time) map[string]interface{} {
	if data.FirstRepo == nil || data.FirstRepo.CreatedAt.Before(from) || data.FirstRepo.CreatedAt.After(to) {
		return nil
	}
	path := "/" + data.FirstRepo.FullName
	node := s.baseContribution("CreatedRepositoryContribution", path, data.FirstRepo.CreatedAt, userSource, data.FirstRepo.ID)
	node["repository"] = repoToGraphQL(s.store, data.FirstRepo)
	return node
}

func (s *Resolver) renderPopularIssue(data *store.ContributionData, userSource map[string]interface{}) map[string]interface{} {
	if data.PopularIssue == nil {
		return nil
	}
	repo := s.store.GetRepoByID(data.PopularIssue.RepoID)
	path := fmt.Sprintf("/%s/issues/%d", repoFullName(repo), data.PopularIssue.Number)
	node := s.baseContribution("CreatedIssueContribution", path, data.PopularIssue.CreatedAt, userSource, data.PopularIssue.ID)
	node["issue"] = issueToGQL(data.PopularIssue, s.store)
	return node
}

func (s *Resolver) renderPopularPullRequest(data *store.ContributionData, userSource map[string]interface{}) map[string]interface{} {
	if data.PopularPR == nil {
		return nil
	}
	repo := s.store.GetRepoByID(data.PopularPR.RepoID)
	path := fmt.Sprintf("/%s/pull/%d", repoFullName(repo), data.PopularPR.Number)
	node := s.baseContribution("CreatedPullRequestContribution", path, data.PopularPR.CreatedAt, userSource, data.PopularPR.ID)
	node["pullRequest"] = pullRequestToGQL(data.PopularPR, s.store)
	return node
}

func (s *Resolver) renderJoinedGitHub(data *store.ContributionData, userSource map[string]interface{}, from, to time.Time) map[string]interface{} {
	if data.User == nil || data.User.CreatedAt.Before(from) || data.User.CreatedAt.After(to) {
		return nil
	}
	path := "/" + data.User.Login
	return s.baseContribution("JoinedGitHubContribution", path, data.User.CreatedAt, userSource, data.User.ID)
}

// calendar rendering

func (s *Resolver) renderContributionCalendar(data *store.ContributionData, from, to time.Time) map[string]interface{} {
	// Positive day counts determine the quartile thresholds.
	positive := make([]int, 0, len(data.DayCounts))
	total := 0
	for _, c := range data.DayCounts {
		total += c
		if c > 0 {
			positive = append(positive, c)
		}
	}
	q1, q2, q3 := quartiles(positive)

	// The calendar spans whole Sunday-started weeks covering [from, to].
	start := weekStart(from)
	end := weekStart(to).AddDate(0, 0, 6)

	weeks := []map[string]interface{}{}
	months := []map[string]interface{}{}
	monthWeeks := map[string]int{}
	monthOrder := []string{}

	for weekDay := start; !weekDay.After(end); weekDay = weekDay.AddDate(0, 0, 7) {
		days := make([]map[string]interface{}, 0, 7)
		for d := 0; d < 7; d++ {
			day := weekDay.AddDate(0, 0, d)
			count := data.DayCounts[store.ContributionDate(day)]
			level := contributionLevel(count, q1, q2, q3)
			days = append(days, map[string]interface{}{
				"color":             contributionColorRamp[level],
				"contributionCount": count,
				"contributionLevel": contributionLevelNames[level],
				"date":              store.ContributionDate(day),
				"weekday":           int(day.Weekday()),
			})
		}
		weeks = append(weeks, map[string]interface{}{
			"contributionDays": days,
			"firstDay":         store.ContributionDate(weekDay),
		})
		mk := weekDay.Format("2006-01")
		if _, seen := monthWeeks[mk]; !seen {
			monthOrder = append(monthOrder, mk)
		}
		monthWeeks[mk]++
	}

	for _, mk := range monthOrder {
		first, _ := time.Parse("2006-01", mk)
		months = append(months, map[string]interface{}{
			"firstDay":   first.Format("2006-01-02"),
			"name":       first.Month().String(),
			"totalWeeks": monthWeeks[mk],
			"year":       first.Year(),
		})
	}

	return map[string]interface{}{
		"colors":             append([]string(nil), contributionColorRamp...),
		"isHalloween":        false,
		"months":             months,
		"totalContributions": total,
		"weeks":              weeks,
	}
}

// small helpers

func repoFullName(repo *store.Repo) string {
	if repo == nil {
		return ""
	}
	return repo.FullName
}

// dayID is a stable numeric identity for a commit-day node: repository id
// combined with the day's ordinal.
func dayID(repoID int, date time.Time) int {
	return repoID*1_000_000 + int(date.UTC().Unix()/86400)
}

// weekStart returns the Sunday (UTC) on or before t at midnight.
func weekStart(t time.Time) time.Time {
	u := t.UTC()
	day := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	return day.AddDate(0, 0, -int(day.Weekday()))
}

// quartiles returns the 25th/50th/75th percentile thresholds bucketing a day
// into a ContributionLevel.
func quartiles(values []int) (q1, q2, q3 int) {
	if len(values) == 0 {
		return 0, 0, 0
	}
	sorted := append([]int(nil), values...)
	sort.Ints(sorted)
	pick := func(p float64) int {
		idx := int(p * float64(len(sorted)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return sorted[idx]
	}
	return pick(0.25), pick(0.50), pick(0.75)
}

// contributionLevel buckets a day's count into 0..4 (NONE..FOURTH_QUARTILE).
func contributionLevel(count, q1, q2, q3 int) int {
	if count <= 0 {
		return 0
	}
	switch {
	case count <= q1:
		return 1
	case count <= q2:
		return 2
	case count <= q3:
		return 3
	default:
		return 4
	}
}

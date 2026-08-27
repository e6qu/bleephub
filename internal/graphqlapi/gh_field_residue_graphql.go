package graphqlapi

import (
	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// addIssueTypeResidueFields adds the remaining IssueType members. The source
// map carries only id/name/description/color, so the store row is re-read by
// node id for state and issue filtering. Called from addIssueFieldsToSchema.
func (s *Resolver) addIssueTypeResidueFields() {
	issueTypeMeta := s.graphqlTypes.issueType
	if issueTypeMeta == nil {
		return
	}

	// A missing store row is truthfully not enabled.
	issueTypeMeta.AddFieldConfig("isEnabled", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, _ := p.Source.(map[string]interface{})
			nodeID, _ := src["id"].(string)
			it := store.FindIssueTypeByNodeID(s.store, nodeID)
			return it != nil && it.IsEnabled, nil
		},
	})

	// bleephub does not model private issue types (a deprecated GitHub concept).
	issueTypeMeta.AddFieldConfig("isPrivate", &graphql.Field{
		Type:    graphql.NewNonNull(graphql.Boolean),
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return false, nil },
	})

	// The repo's issues (repositoryId argument) filtered by this issue-type id;
	// empty when the type or repo is unresolved.
	if issueObj := s.graphqlTypes.issue; issueObj != nil {
		issueTypeMeta.AddFieldConfig("issues", &graphql.Field{
			Type: graphql.NewNonNull(s.gqlIssueConnectionType(issueObj)),
			Args: graphql.FieldConfigArgument{
				"repositoryId": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)},
				"first":        &graphql.ArgumentConfig{Type: graphql.Int},
				"after":        &graphql.ArgumentConfig{Type: graphql.String},
				"last":         &graphql.ArgumentConfig{Type: graphql.Int},
				"before":       &graphql.ArgumentConfig{Type: graphql.String},
				"states":       &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(s.sharedEnum("IssueState", "OPEN", "CLOSED")))},
				"labels":       &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))},
			},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				first := 30
				if f, ok := intArg(p.Args, "first"); ok && f > 0 {
					first = f
				}
				after, _ := p.Args["after"].(string)

				src, _ := p.Source.(map[string]interface{})
				nodeID, _ := src["id"].(string)
				it := store.FindIssueTypeByNodeID(s.store, nodeID)
				repoNodeID, _ := p.Args["repositoryId"].(string)
				repo := store.FindRepoByNodeID(s.store, repoNodeID)
				if it == nil || repo == nil {
					return paginateIssuesGQL(nil, s.store, first, after), nil
				}
				var issues []*store.Issue
				for _, stored := range s.store.ListIssues(repo.ID, "") {
					snap := s.store.SnapIssue(stored)
					if snap.IssueTypeID == it.ID {
						issues = append(issues, snap)
					}
				}
				return paginateIssuesGQL(issues, s.store, first, after), nil
			},
		})
	}

	// bleephub models no per-type pinned-field ordering, so the list is empty.
	if fieldUnion := s.graphqlTypes.issueFieldsUnion; fieldUnion != nil {
		issueTypeMeta.AddFieldConfig("pinnedFields", &graphql.Field{
			Type: graphql.NewList(graphql.NewNonNull(fieldUnion)),
			Resolve: func(graphql.ResolveParams) (interface{}, error) {
				return []interface{}{}, nil
			},
		})
	}
}

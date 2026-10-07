package graphqlapi

import (
	"sort"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// Issue.blockedBy, Issue.blocking and Issue.relatesTo list the issues linked to
// one issue, from the dependency and relates-to records the REST endpoints and
// the add/remove mutations write. Each lists only issues the viewer can read.

type issueLink struct {
	issue    *store.Issue
	addedAt  time.Time
	position int
}

func (s *Resolver) issueDependencyOrderInput() *graphql.InputObject {
	return s.mutationInput("IssueDependencyOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(s.sharedEnum("OrderDirection", "ASC", "DESC")),
		"field":     gqlNonNullInputOf(s.sharedEnum("IssueDependencyOrderField", "CREATED_AT", "DEPENDENCY_ADDED_AT")),
	})
}

func (s *Resolver) issueRelatesToOrderInput() *graphql.InputObject {
	return s.mutationInput("IssueRelatesToOrder", graphql.InputObjectConfigFieldMap{
		"direction": gqlNonNullInputOf(s.sharedEnum("OrderDirection", "ASC", "DESC")),
		"field":     gqlNonNullInputOf(s.sharedEnum("IssueRelatesToOrderField", "CREATED_AT", "RELATES_TO_ADDED_AT")),
	})
}

// issueLinksField is a connection of linked issues ordered by orderBy:
// CREATED_AT orders by the linked issue's creation, the other value by when the
// link was made, which falls back to the order links were recorded in.
func (s *Resolver) issueLinksField(issueConn *graphql.Object, order *graphql.InputObject, addedField string, links func(issueID int) []issueLink) *graphql.Field {
	args := relayConnectionArgs()
	args["orderBy"] = &graphql.ArgumentConfig{
		Type:         order,
		DefaultValue: map[string]interface{}{"field": addedField, "direction": "DESC"},
	}
	return &graphql.Field{
		Type: graphql.NewNonNull(issueConn),
		Args: args,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			src, _ := p.Source.(map[string]interface{})
			issueID, _ := src["databaseId"].(int)
			linked := s.readableIssueLinks(p, links(issueID))
			orderBy, _ := p.Args["orderBy"].(map[string]interface{})
			byCreation := orderBy["field"] == "CREATED_AT"
			descending := orderBy["direction"] != "ASC"
			sort.SliceStable(linked, func(i, j int) bool {
				a, b := linked[i], linked[j]
				if byCreation {
					if !a.issue.CreatedAt.Equal(b.issue.CreatedAt) {
						return a.issue.CreatedAt.Before(b.issue.CreatedAt) != descending
					}
				} else if !a.addedAt.Equal(b.addedAt) {
					return a.addedAt.Before(b.addedAt) != descending
				}
				return (a.position < b.position) != descending
			})
			nodes := make([]map[string]interface{}, 0, len(linked))
			for _, link := range linked {
				nodes = append(nodes, issueToGQL(link.issue, s.store))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	}
}

func (s *Resolver) readableIssueLinks(p graphql.ResolveParams, links []issueLink) []issueLink {
	out := make([]issueLink, 0, len(links))
	for _, link := range links {
		if link.issue == nil {
			continue
		}
		repo := s.store.GetRepoByID(link.issue.RepoID)
		if repo == nil || !s.authz.ViewerCanReadRepo(p.Context, repo) {
			continue
		}
		out = append(out, link)
	}
	return out
}

func (s *Resolver) blockedByLinks(issueID int) []issueLink {
	ids := s.store.ListIssueBlockedBy(issueID)
	out := make([]issueLink, 0, len(ids))
	for i, id := range ids {
		out = append(out, issueLink{issue: s.store.GetIssue(id), addedAt: s.store.BlockedByAddedAt(issueID, id), position: i})
	}
	return out
}

func (s *Resolver) blockingLinks(issueID int) []issueLink {
	ids := s.store.ListIssueBlocking(issueID)
	sort.Ints(ids)
	out := make([]issueLink, 0, len(ids))
	for i, id := range ids {
		out = append(out, issueLink{issue: s.store.GetIssue(id), addedAt: s.store.BlockedByAddedAt(id, issueID), position: i})
	}
	return out
}

func (s *Resolver) relatesToLinks(issueID int) []issueLink {
	ids := s.store.ListIssueRelatesTo(issueID)
	out := make([]issueLink, 0, len(ids))
	for i, id := range ids {
		out = append(out, issueLink{issue: s.store.GetIssue(id), position: i})
	}
	return out
}

// issueDependenciesSummary counts the readable linked issues: all of them for
// the totals, the open ones for blockedBy and blocking.
func (s *Resolver) issueDependenciesSummary(p graphql.ResolveParams) map[string]interface{} {
	src, _ := p.Source.(map[string]interface{})
	issueID, _ := src["databaseId"].(int)
	count := func(links []issueLink) (open, total int) {
		for _, link := range s.readableIssueLinks(p, links) {
			total++
			if link.issue.State == "OPEN" {
				open++
			}
		}
		return open, total
	}
	blockedBy, totalBlockedBy := count(s.blockedByLinks(issueID))
	blocking, totalBlocking := count(s.blockingLinks(issueID))
	return map[string]interface{}{
		"blockedBy": blockedBy, "blocking": blocking,
		"totalBlockedBy": totalBlockedBy, "totalBlocking": totalBlocking,
	}
}

package graphqlapi

import (
	"fmt"
	"sort"
	"time"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/graphql-go/graphql"
)

// addOrgFieldsToSchema adds Organization types, queries, and viewer.organizations to the schema.
func (s *Resolver) addOrgFieldsToSchema(userType, queryType *graphql.Object, nodeInterface *graphql.Interface) *graphql.Object {
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	orgType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Organization",
		// ProjectV2Owner is declared at construction for the reason the User
		// type declares it: graphql-go memoizes an object's interface list on
		// first read, so it cannot be added afterwards.
		Interfaces: []*graphql.Interface{
			nodeInterface, s.graphqlTypes.repositoryOwner,
			s.projectV2OwnerInterfaceType(),
			// ProjectOwner (classic projects), for the same memoization reason.
			s.projectOwnerInterfaceType(),
			// Sponsorable is declared here for the same reason
			// ProjectV2Owner is: graphql-go memoizes the interface list.
			s.sponsorableInterfaceType(),
		},
		Fields: graphql.Fields{
			"id": &graphql.Field{
				Type: graphql.NewNonNull(graphql.ID),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					o, ok := p.Source.(map[string]interface{})
					if !ok {
						return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
					}
					return o["nodeID"], nil
				},
			},
			"databaseId":   &graphql.Field{Type: graphql.Int},
			"login":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"name":         &graphql.Field{Type: graphql.String},
			"description":  &graphql.Field{Type: graphql.String},
			"email":        &graphql.Field{Type: graphql.String},
			"url":          &graphql.Field{Type: graphql.NewNonNull(uri)},
			"resourcePath": &graphql.Field{Type: graphql.NewNonNull(uri)},
			"avatarUrl": &graphql.Field{
				Type: graphql.NewNonNull(uri),
				Args: graphql.FieldConfigArgument{"size": &graphql.ArgumentConfig{Type: graphql.Int}},
			},
			"createdAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
		},
	})
	// Registered so later schema families (Team.organization in the pull
	// request file) reuse the one Organization type instead of forking it.
	s.graphqlTypes.organization = orgType

	orgEdgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: "OrganizationEdge",
		Fields: graphql.Fields{
			"node":   &graphql.Field{Type: orgType},
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})

	orgConnectionType := graphql.NewObject(graphql.ObjectConfig{
		Name: "OrganizationConnection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(orgType)},
			"edges":      &graphql.Field{Type: graphql.NewList(orgEdgeType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(s.gqlPageInfoType())},
		},
	})

	// Registered so the enterprise family's policy-override connections
	// return the one OrganizationConnection the rest of the schema uses.
	s.graphqlTypes.organizationConnection = orgConnectionType

	// Add organizations field to User type (for viewer.organizations)
	userType.AddFieldConfig("organizations", &graphql.Field{
		Type: graphql.NewNonNull(orgConnectionType),
		Args: relayConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			u, ok := p.Source.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
			}
			dbID, _ := u["databaseId"].(int)

			orgs := s.store.ListOrgsByUser(dbID)

			// Sort by creation time (newest first)
			sort.Slice(orgs, func(i, j int) bool {
				if !orgs[i].CreatedAt.Equal(orgs[j].CreatedAt) {
					return orgs[i].CreatedAt.After(orgs[j].CreatedAt)
				}
				return orgs[i].ID > orgs[j].ID
			})

			nodes := make([]map[string]interface{}, 0, len(orgs))
			for _, org := range orgs {
				nodes = append(nodes, orgToGraphQL(org))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})

	// Add organization query to queryType
	queryType.AddFieldConfig("organization", &graphql.Field{
		Type: orgType,
		Args: graphql.FieldConfigArgument{
			"login": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			login, _ := p.Args["login"].(string)
			org := s.store.GetOrg(login)
			if org == nil {
				return nil, &ghNotFoundError{
					message: fmt.Sprintf("Could not resolve to an Organization with the login of '%s'.", login),
				}
			}
			return orgToGraphQL(org), nil
		},
	})
	return orgType
}

// orgToGraphQL converts an Org to a map for GraphQL resolvers.
func orgToGraphQL(org *store.Org) map[string]interface{} {
	return map[string]interface{}{
		"nodeID":       org.NodeID,
		"databaseId":   org.ID,
		"login":        org.Login,
		"name":         org.Name,
		"description":  org.Description,
		"email":        org.Email,
		"url":          externalURL("/" + org.Login),
		"resourcePath": "/" + org.Login,
		"avatarUrl":    org.AvatarURL,
		"createdAt":    org.CreatedAt.Format(time.RFC3339),
		"updatedAt":    org.UpdatedAt.Format(time.RFC3339),
	}
}

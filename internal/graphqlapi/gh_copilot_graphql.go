package graphqlapi

// User.copilotEndpoints — the service endpoints a Copilot client resolves
// before connecting. bleephub runs no completion model but serves its own
// URLs. GitHub scopes the field to the authenticated user (null otherwise).

import (
	"strings"

	"github.com/graphql-go/graphql"
)

func (s *Resolver) copilotEndpointsType() *graphql.Object {
	if s.graphqlTypes.copilotEndpoints != nil {
		return s.graphqlTypes.copilotEndpoints
	}
	s.graphqlTypes.copilotEndpoints = graphql.NewObject(graphql.ObjectConfig{
		Name: "CopilotEndpoints",
		Fields: graphql.Fields{
			"api":           &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"exp":           &graphql.Field{Type: graphql.String},
			"originTracker": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"proxy":         &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"telemetry":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	return s.graphqlTypes.copilotEndpoints
}

// addCopilotFieldsToSchema installs User.copilotEndpoints.
func (s *Resolver) addCopilotFieldsToSchema(userType *graphql.Object) {
	userType.AddFieldConfig("copilotEndpoints", &graphql.Field{
		Type: s.copilotEndpointsType(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			viewer := s.ghUserFromContext(p.Context)
			login := sponsorsAccountLogin(p.Source)
			// Scoped to the viewer: not served through another account's profile.
			if viewer == nil || !strings.EqualFold(viewer.Login, login) {
				return nil, nil
			}
			return map[string]interface{}{
				"api":           externalURL("/copilot/api"),
				"exp":           externalURL("/copilot/experiments"),
				"originTracker": externalURL("/copilot/origin-tracker"),
				"proxy":         externalURL("/copilot/proxy"),
				"telemetry":     externalURL("/copilot/telemetry"),
			}, nil
		},
	})
}

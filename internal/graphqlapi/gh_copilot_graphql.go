package graphqlapi

// GitHub Copilot — the GraphQL surface: User.copilotEndpoints, the service
// endpoints a Copilot client resolves before it connects.
//
// bleephub runs no code-completion model, but the endpoints are real
// configuration rather than a model: they are where a client would send
// completions, chat, telemetry and experiment traffic, and they are this
// instance's own URLs. GitHub scopes the field to the authenticated user,
// so it answers null for anybody else's profile.

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
			// A client's Copilot endpoints are its own; GitHub does not hand
			// one account's out through another's profile.
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

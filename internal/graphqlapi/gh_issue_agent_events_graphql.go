package graphqlapi

import "github.com/graphql-go/graphql"

// addIssueAgentEvents declares the six agent-triage timeline events: an issue
// field or issue type added, changed or removed. bleephub records none of them;
// they are declared so the timeline unions and their fragments validate.
func (s *Resolver) addIssueAgentEvents(reg *timelineTypeRegistry, dateTime *graphql.Scalar) {
	actor := s.graphqlTypes.actor
	issueFields := s.graphqlTypes.issueFieldsUnion
	issueType := s.graphqlTypes.issueType
	intent := reg.updateIntent

	timelineOption := s.mutationObject("IssueFieldTimelineOption", graphql.Fields{
		"color": &graphql.Field{Type: graphql.String},
		"name":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
	})
	optionList := graphql.NewList(graphql.NewNonNull(timelineOption))

	base := func(extra graphql.Fields) graphql.Fields {
		f := graphql.Fields{
			"actor":     &graphql.Field{Type: actor},
			"createdAt": &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"id":        &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"intent":    &graphql.Field{Type: intent},
		}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}

	s.mutationObject("IssueFieldAddedEvent", base(graphql.Fields{
		"color":      &graphql.Field{Type: graphql.String},
		"issueField": &graphql.Field{Type: issueFields},
		"options":    &graphql.Field{Type: optionList},
		"value":      &graphql.Field{Type: graphql.String},
	}))
	s.mutationObject("IssueFieldChangedEvent", base(graphql.Fields{
		"issueField":      &graphql.Field{Type: issueFields},
		"newColor":        &graphql.Field{Type: graphql.String},
		"newOptions":      &graphql.Field{Type: optionList},
		"newValue":        &graphql.Field{Type: graphql.String},
		"previousColor":   &graphql.Field{Type: graphql.String},
		"previousOptions": &graphql.Field{Type: optionList},
		"previousValue":   &graphql.Field{Type: graphql.String},
	}))
	s.mutationObject("IssueFieldRemovedEvent", base(graphql.Fields{
		"issueField": &graphql.Field{Type: issueFields},
		"options":    &graphql.Field{Type: optionList},
	}))
	s.mutationObject("IssueTypeAddedEvent", base(graphql.Fields{
		"issueType": &graphql.Field{Type: issueType},
	}))
	s.mutationObject("IssueTypeChangedEvent", base(graphql.Fields{
		"issueType":     &graphql.Field{Type: issueType},
		"prevIssueType": &graphql.Field{Type: issueType},
	}))
	s.mutationObject("IssueTypeRemovedEvent", base(graphql.Fields{
		"issueType": &graphql.Field{Type: issueType},
	}))
}

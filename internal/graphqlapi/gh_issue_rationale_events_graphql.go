package graphqlapi

import "github.com/graphql-go/graphql"

// addIssueEventWithRationaleUnion builds IssueEventWithRationale, the union
// IssueEventRationale.issueEvent returns. bleephub records no agent-triage
// rationale events, so the field resolves null; the union and its six agent
// events exist only for schema fidelity. Each event names IssueEventRationale,
// which in turn names this union — a cycle the union's memoization and the
// eventRationale object's post-hoc AddFieldConfig resolve.
func (s *Resolver) addIssueEventWithRationaleUnion(reg *timelineTypeRegistry, dateTime *graphql.Scalar) {
	actor := s.graphqlTypes.actor
	issueFields := s.graphqlTypes.issueFieldsUnion
	issueType := s.graphqlTypes.issueType
	intent := reg.updateIntent
	rationale := reg.eventRationale

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
			"rationale": &graphql.Field{Type: rationale},
		}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}

	fieldAdded := s.mutationObject("IssueFieldAddedEvent", base(graphql.Fields{
		"color":      &graphql.Field{Type: graphql.String},
		"issueField": &graphql.Field{Type: issueFields},
		"options":    &graphql.Field{Type: optionList},
		"value":      &graphql.Field{Type: graphql.String},
	}))
	fieldChanged := s.mutationObject("IssueFieldChangedEvent", base(graphql.Fields{
		"issueField":      &graphql.Field{Type: issueFields},
		"newColor":        &graphql.Field{Type: graphql.String},
		"newOptions":      &graphql.Field{Type: optionList},
		"newValue":        &graphql.Field{Type: graphql.String},
		"previousColor":   &graphql.Field{Type: graphql.String},
		"previousOptions": &graphql.Field{Type: optionList},
		"previousValue":   &graphql.Field{Type: graphql.String},
	}))
	fieldRemoved := s.mutationObject("IssueFieldRemovedEvent", base(graphql.Fields{
		"issueField": &graphql.Field{Type: issueFields},
		"options":    &graphql.Field{Type: optionList},
	}))
	typeAdded := s.mutationObject("IssueTypeAddedEvent", base(graphql.Fields{
		"issueType": &graphql.Field{Type: issueType},
	}))
	typeChanged := s.mutationObject("IssueTypeChangedEvent", base(graphql.Fields{
		"issueType":     &graphql.Field{Type: issueType},
		"prevIssueType": &graphql.Field{Type: issueType},
	}))
	typeRemoved := s.mutationObject("IssueTypeRemovedEvent", base(graphql.Fields{
		"issueType": &graphql.Field{Type: issueType},
	}))

	members := []*graphql.Object{
		reg.byName["ClosedEvent"], fieldAdded, fieldChanged, fieldRemoved,
		typeAdded, typeChanged, typeRemoved, reg.byName["LabeledEvent"], reg.byName["UnlabeledEvent"],
	}
	union := s.mutationUnion("IssueEventWithRationale", func() []*graphql.Object { return members }, func(p graphql.ResolveTypeParams) *graphql.Object {
		source, _ := p.Value.(map[string]interface{})
		if name, _ := source["__typename"].(string); name != "" {
			return s.namedObject(name)
		}
		return nil
	})

	rationale.AddFieldConfig("issueEvent", &graphql.Field{
		Type:    union,
		Resolve: func(graphql.ResolveParams) (interface{}, error) { return nil, nil },
	})
}

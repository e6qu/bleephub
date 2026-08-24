package graphqlapi

// The issue mutation surface: comment editing and moderation, assignment,
// sub-issues and dependencies, issue types, custom issue fields, the duplicate
// relation, and the pending-suggestion queue an agent proposes triage through.
//
// Every one writes through the store primitive the equivalent REST route
// writes through — UpdateCommentBody, AddIssueAssignees, AddSubIssue,
// CreateIssueType, SetIssueFieldValues, PerformIssueSuggestion — so a change
// made over GraphQL and the same change made over REST leave the same records
// and the same timeline behind.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

func (s *Resolver) addIssueSurfaceMutations(mutationType *graphql.Object) {
	issueType := s.graphqlTypes.issue
	issueCommentType := s.graphqlTypes.issueComment
	assignable := s.gqlAssignableInterface()
	issueOrPullRequest := s.graphqlTypes.issueOrPullRequest
	issueTypeObject := s.graphqlTypes.issueType
	issueFieldsUnion := s.graphqlTypes.issueFieldsUnion
	issueFieldValueUnion := s.graphqlTypes.issueFieldValueUnion
	confidence := s.sharedEnum("IssueEventConfidenceLevel", "HIGH", "LOW", "MEDIUM")

	// --- issue comments -----------------------------------------------------

	s.registerMutation(mutationType, "updateIssueComment", &graphql.Field{
		Type: s.mutationPayload("UpdateIssueCommentPayload", graphql.Fields{
			"issueComment": gqlField(issueCommentType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateIssueCommentInput", graphql.InputObjectConfigFieldMap{
				"id":   gqlNonNullID(),
				"body": gqlNonNullString(),
			})),
		}},
		Resolve: s.resolveUpdateIssueComment,
	})

	s.registerMutation(mutationType, "deleteIssueComment", &graphql.Field{
		Type: s.mutationPayload("DeleteIssueCommentPayload", graphql.Fields{}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("DeleteIssueCommentInput", graphql.InputObjectConfigFieldMap{
				"id": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveDeleteIssueComment,
	})

	pinComment := func(name, payloadName, inputName string, pinned bool) {
		s.registerMutation(mutationType, name, &graphql.Field{
			Type: s.mutationPayload(payloadName, graphql.Fields{
				"issueComment": gqlField(issueCommentType),
			}),
			Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
				Type: graphql.NewNonNull(s.mutationInput(inputName, graphql.InputObjectConfigFieldMap{
					"issueCommentId": gqlNonNullID(),
				})),
			}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return s.resolveIssueCommentPin(p, pinned)
			},
		})
	}
	pinComment("pinIssueComment", "PinIssueCommentPayload", "PinIssueCommentInput", true)
	pinComment("unpinIssueComment", "UnpinIssueCommentPayload", "UnpinIssueCommentInput", false)

	// --- assignment ---------------------------------------------------------

	agentAssignment := s.mutationInput("AgentAssignmentInput", graphql.InputObjectConfigFieldMap{
		"baseRef":            gqlString(),
		"customAgent":        gqlString(),
		"customInstructions": gqlString(),
		"targetRepositoryId": gqlID(),
	})
	assigneeUpdate := s.mutationInput("AssigneeUpdateInput", graphql.InputObjectConfigFieldMap{
		"actorId":    gqlNonNullID(),
		"confidence": gqlInputOf(confidence),
		"rationale":  gqlString(),
		"suggest":    gqlBool(),
	})

	s.registerMutation(mutationType, "addAssigneesToAssignable", &graphql.Field{
		Type: s.mutationPayload("AddAssigneesToAssignablePayload", graphql.Fields{
			"assignable": gqlField(assignable),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("AddAssigneesToAssignableInput", graphql.InputObjectConfigFieldMap{
				"assignableId":    gqlNonNullID(),
				"assigneeIds":     gqlListOf(graphql.ID),
				"assignees":       gqlListOf(assigneeUpdate),
				"agentAssignment": gqlInputOf(agentAssignment),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.resolveAssigneeChange(p, assigneeChangeAdd)
		},
	})

	s.registerMutation(mutationType, "removeAssigneesFromAssignable", &graphql.Field{
		Type: s.mutationPayload("RemoveAssigneesFromAssignablePayload", graphql.Fields{
			"assignable": gqlField(assignable),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("RemoveAssigneesFromAssignableInput", graphql.InputObjectConfigFieldMap{
				"assignableId": gqlNonNullID(),
				"assigneeIds":  gqlNonNullListOf(graphql.ID),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.resolveAssigneeChange(p, assigneeChangeRemove)
		},
	})

	s.registerMutation(mutationType, "replaceActorsForAssignable", &graphql.Field{
		Type: s.mutationPayload("ReplaceActorsForAssignablePayload", graphql.Fields{
			"assignable": gqlField(assignable),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("ReplaceActorsForAssignableInput", graphql.InputObjectConfigFieldMap{
				"assignableId":    gqlNonNullID(),
				"actorIds":        gqlListOf(graphql.ID),
				"actorLogins":     gqlListOf(graphql.String),
				"assignees":       gqlListOf(assigneeUpdate),
				"agentAssignment": gqlInputOf(agentAssignment),
			})),
		}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.resolveAssigneeChange(p, assigneeChangeReplace)
		},
	})

	// --- sub-issues and dependencies ---------------------------------------

	s.registerMutation(mutationType, "addSubIssue", &graphql.Field{
		Type: s.mutationPayload("AddSubIssuePayload", graphql.Fields{
			"issue":    gqlField(issueType),
			"subIssue": gqlField(issueType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("AddSubIssueInput", graphql.InputObjectConfigFieldMap{
				"issueId":       gqlNonNullID(),
				"subIssueId":    gqlID(),
				"subIssueUrl":   gqlString(),
				"replaceParent": gqlBool(),
			})),
		}},
		Resolve: s.resolveAddSubIssue,
	})

	s.registerMutation(mutationType, "removeSubIssue", &graphql.Field{
		Type: s.mutationPayload("RemoveSubIssuePayload", graphql.Fields{
			"issue":    gqlField(issueType),
			"subIssue": gqlField(issueType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("RemoveSubIssueInput", graphql.InputObjectConfigFieldMap{
				"issueId":    gqlNonNullID(),
				"subIssueId": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveRemoveSubIssue,
	})

	s.registerMutation(mutationType, "reprioritizeSubIssue", &graphql.Field{
		Type: s.mutationPayload("ReprioritizeSubIssuePayload", graphql.Fields{
			"issue": gqlField(issueType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("ReprioritizeSubIssueInput", graphql.InputObjectConfigFieldMap{
				"issueId":    gqlNonNullID(),
				"subIssueId": gqlNonNullID(),
				"afterId":    gqlID(),
				"beforeId":   gqlID(),
			})),
		}},
		Resolve: s.resolveReprioritizeSubIssue,
	})

	blockedBy := func(name, payloadName, inputName string, add bool) {
		s.registerMutation(mutationType, name, &graphql.Field{
			Type: s.mutationPayload(payloadName, graphql.Fields{
				"issue":         gqlField(issueType),
				"blockingIssue": gqlField(issueType),
			}),
			Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
				Type: graphql.NewNonNull(s.mutationInput(inputName, graphql.InputObjectConfigFieldMap{
					"issueId":         gqlNonNullID(),
					"blockingIssueId": gqlNonNullID(),
				})),
			}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return s.resolveBlockedBy(p, add)
			},
		})
	}
	blockedBy("addBlockedBy", "AddBlockedByPayload", "AddBlockedByInput", true)
	blockedBy("removeBlockedBy", "RemoveBlockedByPayload", "RemoveBlockedByInput", false)

	// --- the duplicate relation --------------------------------------------

	s.registerMutation(mutationType, "unmarkIssueAsDuplicate", &graphql.Field{
		Type: s.mutationPayload("UnmarkIssueAsDuplicatePayload", graphql.Fields{
			"duplicate": gqlField(issueOrPullRequest),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UnmarkIssueAsDuplicateInput", graphql.InputObjectConfigFieldMap{
				"canonicalId": gqlNonNullID(),
				"duplicateId": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveUnmarkIssueAsDuplicate,
	})

	// --- issue types --------------------------------------------------------

	issueTypeColor := s.sharedEnum("IssueTypeColor",
		"BLUE", "GRAY", "GREEN", "ORANGE", "PINK", "PURPLE", "RED", "YELLOW")

	s.registerMutation(mutationType, "createIssueType", &graphql.Field{
		Type: s.mutationPayload("CreateIssueTypePayload", graphql.Fields{
			"issueType": gqlField(issueTypeObject),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("CreateIssueTypeInput", graphql.InputObjectConfigFieldMap{
				"ownerId":     gqlNonNullID(),
				"name":        gqlNonNullString(),
				"description": gqlString(),
				"color":       gqlInputOf(issueTypeColor),
				"isEnabled":   gqlNonNullBool(),
			})),
		}},
		Resolve: s.resolveCreateIssueType,
	})

	s.registerMutation(mutationType, "updateIssueType", &graphql.Field{
		Type: s.mutationPayload("UpdateIssueTypePayload", graphql.Fields{
			"issueType": gqlField(issueTypeObject),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateIssueTypeInput", graphql.InputObjectConfigFieldMap{
				"issueTypeId": gqlNonNullID(),
				"name":        gqlString(),
				"description": gqlString(),
				"color":       gqlInputOf(issueTypeColor),
				"isEnabled":   gqlBool(),
			})),
		}},
		Resolve: s.resolveUpdateIssueType,
	})

	s.registerMutation(mutationType, "deleteIssueType", &graphql.Field{
		Type: s.mutationPayload("DeleteIssueTypePayload", graphql.Fields{
			"deletedIssueTypeId": gqlField(graphql.ID),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("DeleteIssueTypeInput", graphql.InputObjectConfigFieldMap{
				"issueTypeId": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveDeleteIssueType,
	})

	s.registerMutation(mutationType, "updateIssueIssueType", &graphql.Field{
		Type: s.mutationPayload("UpdateIssueIssueTypePayload", graphql.Fields{
			"issue": gqlField(issueType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateIssueIssueTypeInput", graphql.InputObjectConfigFieldMap{
				"issueId":     gqlNonNullID(),
				"issueTypeId": gqlID(),
			})),
		}},
		Resolve: s.resolveUpdateIssueIssueType,
	})

	// --- custom issue fields ------------------------------------------------

	optionInput := s.mutationInput("IssueFieldSingleSelectOptionInput", graphql.InputObjectConfigFieldMap{
		"name":        gqlNonNullString(),
		"description": gqlString(),
		"priority":    gqlNonNullInt(),
		"color": gqlNonNullInputOf(s.sharedEnum("IssueFieldSingleSelectOptionColor",
			"BLUE", "GRAY", "GREEN", "ORANGE", "PINK", "PURPLE", "RED", "YELLOW")),
	})
	visibility := s.sharedEnum("IssueFieldVisibility", "ALL", "ORG_ONLY")

	s.registerMutation(mutationType, "createIssueField", &graphql.Field{
		Type: s.mutationPayload("CreateIssueFieldPayload", graphql.Fields{
			"issueField": gqlField(issueFieldsUnion),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("CreateIssueFieldInput", graphql.InputObjectConfigFieldMap{
				"ownerId":     gqlNonNullID(),
				"name":        gqlNonNullString(),
				"description": gqlString(),
				"dataType": gqlNonNullInputOf(s.sharedEnum("IssueFieldDataType",
					"DATE", "MULTI_SELECT", "NUMBER", "SINGLE_SELECT", "TEXT")),
				"visibility": gqlInputOf(visibility),
				"options":    gqlListOf(optionInput),
			})),
		}},
		Resolve: s.resolveCreateIssueField,
	})

	s.registerMutation(mutationType, "updateIssueField", &graphql.Field{
		Type: s.mutationPayload("UpdateIssueFieldPayload", graphql.Fields{
			"issueField": gqlField(issueFieldsUnion),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateIssueFieldInput", graphql.InputObjectConfigFieldMap{
				"id":          gqlNonNullID(),
				"name":        gqlString(),
				"description": gqlString(),
				"visibility":  gqlInputOf(visibility),
				"options":     gqlListOf(optionInput),
			})),
		}},
		Resolve: s.resolveUpdateIssueField,
	})

	s.registerMutation(mutationType, "deleteIssueField", &graphql.Field{
		Type: s.mutationPayload("DeleteIssueFieldPayload", graphql.Fields{
			"issueField": gqlField(issueFieldsUnion),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("DeleteIssueFieldInput", graphql.InputObjectConfigFieldMap{
				"fieldId": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveDeleteIssueField,
	})

	fieldValueInput := s.mutationInput("IssueFieldCreateOrUpdateInput", graphql.InputObjectConfigFieldMap{
		"fieldId":              gqlNonNullID(),
		"textValue":            gqlString(),
		"dateValue":            gqlString(),
		"numberValue":          gqlInputOf(graphql.Float),
		"singleSelectOptionId": gqlID(),
		"multiSelectOptionIds": gqlListOf(graphql.ID),
		"delete":               gqlBool(),
		"suggest":              gqlBool(),
		"rationale":            gqlString(),
		"confidence":           gqlInputOf(confidence),
	})

	fieldValueMutation := func(name, payloadName, inputName string) {
		s.registerMutation(mutationType, name, &graphql.Field{
			Type: s.mutationPayload(payloadName, graphql.Fields{
				"issue":           gqlField(issueType),
				"issueFieldValue": gqlField(issueFieldValueUnion),
			}),
			Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
				Type: graphql.NewNonNull(s.mutationInput(inputName, graphql.InputObjectConfigFieldMap{
					"issueId":    gqlNonNullID(),
					"issueField": gqlNonNullInputOf(fieldValueInput),
				})),
			}},
			Resolve: s.resolveWriteIssueFieldValue,
		})
	}
	fieldValueMutation("createIssueFieldValue", "CreateIssueFieldValuePayload", "CreateIssueFieldValueInput")
	fieldValueMutation("updateIssueFieldValue", "UpdateIssueFieldValuePayload", "UpdateIssueFieldValueInput")

	s.registerMutation(mutationType, "setIssueFieldValue", &graphql.Field{
		Type: s.mutationPayload("SetIssueFieldValuePayload", graphql.Fields{
			"issue":            gqlField(issueType),
			"issueFieldValues": gqlFieldListOf(issueFieldValueUnion),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("SetIssueFieldValueInput", graphql.InputObjectConfigFieldMap{
				"issueId":     gqlNonNullID(),
				"issueFields": gqlNonNullListOf(fieldValueInput),
			})),
		}},
		Resolve: s.resolveSetIssueFieldValue,
	})

	s.registerMutation(mutationType, "deleteIssueFieldValue", &graphql.Field{
		Type: s.mutationPayload("DeleteIssueFieldValuePayload", graphql.Fields{
			"issue":   gqlField(issueType),
			"success": gqlField(graphql.Boolean),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("DeleteIssueFieldValueInput", graphql.InputObjectConfigFieldMap{
				"issueId": gqlNonNullID(),
				"fieldId": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveDeleteIssueFieldValue,
	})

	// --- the pending-suggestion queue ---------------------------------------

	suggestionRef := s.mutationInput("PendingIssueSuggestionRef", graphql.InputObjectConfigFieldMap{
		"kind": gqlNonNullInputOf(s.sharedEnum("PendingIssueSuggestionKind",
			"ASSIGNEE", "CLOSE", "FIELD", "LABEL", "TYPE")),
		"assigneeId":   gqlID(),
		"issueFieldId": gqlID(),
		"issueTypeId":  gqlID(),
		"labelId":      gqlID(),
	})

	suggestionMutation := func(name, payloadName, inputName string, apply bool) {
		s.registerMutation(mutationType, name, &graphql.Field{
			Type: s.mutationPayload(payloadName, graphql.Fields{
				"issue": gqlField(issueType),
			}),
			Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
				Type: graphql.NewNonNull(s.mutationInput(inputName, graphql.InputObjectConfigFieldMap{
					"issueId":     gqlNonNullID(),
					"actorId":     gqlNonNullID(),
					"suggestions": gqlNonNullListOf(suggestionRef),
				})),
			}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return s.resolvePendingIssueSuggestions(p, apply)
			},
		})
	}
	suggestionMutation("applyPendingIssueSuggestions", "ApplyPendingIssueSuggestionsPayload",
		"ApplyPendingIssueSuggestionsInput", true)
	suggestionMutation("rejectPendingIssueSuggestions", "RejectPendingIssueSuggestionsPayload",
		"RejectPendingIssueSuggestionsInput", false)
}

// --- issue comments ---------------------------------------------------------

func (s *Resolver) resolveUpdateIssueComment(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "id")
	comment := store.FindIssueCommentByNodeID(s.store, nodeID)
	if comment == nil {
		return nil, gqlMissingNode("IssueComment", nodeID)
	}
	body, _ := gqlInputString(input, "body")
	user := s.ghUserFromContext(p.Context)
	updated := s.store.UpdateCommentBody(comment.ID, user.ID, body)
	if updated == nil {
		return nil, gqlMissingNodeType("IssueComment")
	}
	s.emitIssueCommentEvent(updated, user, "edited")
	return map[string]interface{}{"issueComment": optionalObject(commentToGQL(updated, s.store))}, nil
}

func (s *Resolver) resolveDeleteIssueComment(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "id")
	comment := store.FindIssueCommentByNodeID(s.store, nodeID)
	if comment == nil {
		return nil, gqlMissingNode("IssueComment", nodeID)
	}
	// The webhook body carries the comment as it was, so it is snapshotted
	// before the row is destroyed.
	deleted := s.store.GetComment(comment.ID)
	if deleted == nil {
		return nil, gqlMissingNodeType("IssueComment")
	}
	if !s.store.DeleteComment(deleted.ID) {
		return nil, gqlMissingNodeType("IssueComment")
	}
	s.emitIssueCommentEvent(deleted, s.ghUserFromContext(p.Context), "deleted")
	return map[string]interface{}{}, nil
}

func (s *Resolver) resolveIssueCommentPin(p graphql.ResolveParams, pinned bool) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "issueCommentId")
	comment := store.FindIssueCommentByNodeID(s.store, nodeID)
	if comment == nil {
		return nil, gqlMissingNode("IssueComment", nodeID)
	}
	if pinned {
		s.store.PinIssueComment(comment.ID)
	} else {
		s.store.UnpinIssueComment(comment.ID)
	}
	updated := s.store.GetIssueComment(comment.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("IssueComment")
	}
	return map[string]interface{}{"issueComment": optionalObject(commentToGQL(updated, s.store))}, nil
}

// emitIssueCommentEvent delivers the issue_comment webhook the REST comment
// routes deliver. A comment on a pull request is an issue comment on GitHub
// and fires the same event, which is why the subject is looked up either way.
func (s *Resolver) emitIssueCommentEvent(comment *store.Comment, sender *store.User, action string) {
	repo, subject := s.commentSubject(comment)
	if repo == nil || subject == nil {
		return
	}
	payload := map[string]interface{}{
		"action":     action,
		"comment":    map[string]interface{}{"id": comment.ID, "node_id": comment.NodeID, "body": comment.Body},
		"repository": s.repoPayload(repo),
		"sender":     s.senderPayload(sender),
	}
	if issue, ok := subject.(*store.Issue); ok {
		payload["issue"] = s.buildIssuesPayload(repo, issue, sender, action)["issue"]
	} else if pr, ok := subject.(*store.PullRequest); ok {
		payload["issue"] = s.buildPullRequestPayload(repo, pr, sender, action)["pull_request"]
	}
	s.emitWebhookEvent(repo.FullName, "issue_comment", action, payload)
}

// commentSubject resolves the repository and the issue or pull request a
// comment hangs off.
func (s *Resolver) commentSubject(comment *store.Comment) (*store.Repo, interface{}) {
	if comment == nil {
		return nil, nil
	}
	if comment.ParentType == "pull_request" {
		pr := s.store.GetPullRequest(comment.IssueID)
		if pr == nil {
			return nil, nil
		}
		return s.store.GetRepoByID(pr.RepoID), pr
	}
	issue := s.store.GetIssue(comment.IssueID)
	if issue == nil {
		return nil, nil
	}
	return s.store.GetRepoByID(issue.RepoID), issue
}

// --- assignment -------------------------------------------------------------

type assigneeChangeMode int

const (
	assigneeChangeAdd assigneeChangeMode = iota
	assigneeChangeRemove
	assigneeChangeReplace
)

// resolveAssigneeChange is the body the three assignment mutations share. The
// named actors are resolved to accounts, combined with the subject's current
// assignees according to the mode, and written through the subject's update
// primitive so the assigned/unassigned timeline events and the webhook
// fan-out are the ones the REST assignee routes produce.
//
// An assignee offered with `suggest: true` is not assigned: it is recorded as
// a pending suggestion for applyPendingIssueSuggestions to perform, which is
// what the flag means on GitHub.
func (s *Resolver) resolveAssigneeChange(p graphql.ResolveParams, mode assigneeChangeMode) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "assignableId")
	user := s.ghUserFromContext(p.Context)

	named, suggested, err := s.assigneesFromInput(input, mode)
	if err != nil {
		return nil, err
	}

	if issue := store.FindIssueByNodeID(s.store, nodeID); issue != nil {
		repo := s.store.GetRepoByID(issue.RepoID)
		before := s.store.GetIssue(issue.ID)
		if repo == nil || before == nil {
			return nil, gqlMissingNodeType("Issue")
		}
		for _, actorID := range suggested {
			s.recordAssigneeSuggestion(repo, before, user, actorID)
		}
		next := combineAssignees(before.AssigneeIDs, named, mode)
		s.store.UpdateIssue(before.ID, func(i *store.Issue) { i.AssigneeIDs = next })
		updated := s.store.GetIssue(before.ID)
		if updated == nil {
			return nil, gqlMissingNodeType("Issue")
		}
		s.emitIssueChanges(repo, updated, user, store.SubjectChange{
			AssigneesFrom: before.AssigneeIDs,
			AssigneesTo:   &next,
		})
		return map[string]interface{}{"assignable": issueToGQL(updated, s.store)}, nil
	}

	if pr := store.FindPullRequestByNodeID(s.store, nodeID); pr != nil {
		repo := s.store.GetRepoByID(pr.RepoID)
		before := s.store.GetPullRequest(pr.ID)
		if repo == nil || before == nil {
			return nil, gqlMissingNodeType("PullRequest")
		}
		next := combineAssignees(before.AssigneeIDs, named, mode)
		s.store.UpdatePullRequest(before.ID, func(item *store.PullRequest) { item.AssigneeIDs = next })
		updated := s.store.GetPullRequest(before.ID)
		if updated == nil {
			return nil, gqlMissingNodeType("PullRequest")
		}
		s.emitPullRequestChanges(repo, updated, user, store.SubjectChange{
			AssigneesFrom: before.AssigneeIDs,
			AssigneesTo:   &next,
		})
		return map[string]interface{}{"assignable": pullRequestToGQL(updated, s.store)}, nil
	}

	return nil, gqlMissingNode("Assignable", nodeID)
}

// assigneesFromInput resolves every way the three mutations name an actor —
// assigneeIds, actorIds, actorLogins and the assignees[] objects — into the
// account ids to apply and the ones merely suggested.
func (s *Resolver) assigneesFromInput(input map[string]interface{}, mode assigneeChangeMode) (apply, suggest []int, err error) {
	appendActor := func(nodeID string, suggested bool) error {
		actor := store.FindUserByNodeID(s.store, nodeID)
		if actor == nil {
			return gqlMissingNode("Actor", nodeID)
		}
		if suggested {
			suggest = append(suggest, actor.ID)
			return nil
		}
		apply = append(apply, actor.ID)
		return nil
	}

	for _, key := range []string{"assigneeIds", "actorIds"} {
		ids, _ := gqlInputStrings(input, key)
		for _, id := range ids {
			if err := appendActor(id, false); err != nil {
				return nil, nil, err
			}
		}
	}
	for _, login := range mustStrings(input, "actorLogins") {
		actor := s.store.LookupUserByLogin(login)
		if actor == nil {
			return nil, nil, gqlMissingNodeType("Actor")
		}
		apply = append(apply, actor.ID)
	}
	for _, assignee := range gqlInputObjects(input, "assignees") {
		actorID, _ := gqlInputString(assignee, "actorId")
		suggested, _ := gqlInputBool(assignee, "suggest")
		if err := appendActor(actorID, suggested); err != nil {
			return nil, nil, err
		}
	}
	if mode == assigneeChangeRemove && len(apply) == 0 {
		return nil, nil, fmt.Errorf("assigneeIds names no account")
	}
	return apply, suggest, nil
}

func mustStrings(input map[string]interface{}, key string) []string {
	values, _ := gqlInputStrings(input, key)
	return values
}

// recordAssigneeSuggestion queues a proposed assignment for the pending
// suggestion queue rather than performing it.
func (s *Resolver) recordAssigneeSuggestion(repo *store.Repo, issue *store.Issue, actor *store.User, assigneeID int) {
	target := assigneeID
	var actorID *int
	if actor != nil {
		id := actor.ID
		actorID = &id
	}
	s.store.CreateIssueSuggestion(repo.FullName, issue.ID, store.IssueSuggestion{
		Action:   "add_assignee",
		TargetID: &target,
		ActorID:  actorID,
	})
}

func combineAssignees(existing, named []int, mode assigneeChangeMode) []int {
	switch mode {
	case assigneeChangeRemove:
		drop := make(map[int]bool, len(named))
		for _, id := range named {
			drop[id] = true
		}
		out := make([]int, 0, len(existing))
		for _, id := range existing {
			if !drop[id] {
				out = append(out, id)
			}
		}
		return out
	case assigneeChangeReplace:
		return dedupeInts(named)
	default:
		return dedupeInts(append(append([]int(nil), existing...), named...))
	}
}

func dedupeInts(values []int) []int {
	seen := make(map[int]bool, len(values))
	out := make([]int, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// --- sub-issues and dependencies --------------------------------------------

func (s *Resolver) resolveAddSubIssue(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	parent, err := s.issueFromInput(input, "issueId")
	if err != nil {
		return nil, err
	}
	child, err := s.subIssueFromInput(input)
	if err != nil {
		return nil, err
	}
	replace, _ := gqlInputBool(input, "replaceParent")
	if err := s.store.AddSubIssue(parent.ID, child.ID, replace); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"issue":    optionalRenderedIssue(s.store.GetIssue(parent.ID), s.store),
		"subIssue": optionalRenderedIssue(s.store.GetIssue(child.ID), s.store),
	}, nil
}

func (s *Resolver) resolveRemoveSubIssue(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	parent, err := s.issueFromInput(input, "issueId")
	if err != nil {
		return nil, err
	}
	child, err := s.issueFromInput(input, "subIssueId")
	if err != nil {
		return nil, err
	}
	if err := s.store.RemoveSubIssue(parent.ID, child.ID); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"issue":    optionalRenderedIssue(s.store.GetIssue(parent.ID), s.store),
		"subIssue": optionalRenderedIssue(s.store.GetIssue(child.ID), s.store),
	}, nil
}

func (s *Resolver) resolveReprioritizeSubIssue(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	parent, err := s.issueFromInput(input, "issueId")
	if err != nil {
		return nil, err
	}
	child, err := s.issueFromInput(input, "subIssueId")
	if err != nil {
		return nil, err
	}
	after, err := s.optionalIssueID(input, "afterId")
	if err != nil {
		return nil, err
	}
	before, err := s.optionalIssueID(input, "beforeId")
	if err != nil {
		return nil, err
	}
	if err := s.store.ReprioritizeSubIssue(parent.ID, child.ID, after, before); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"issue": optionalRenderedIssue(s.store.GetIssue(parent.ID), s.store),
	}, nil
}

func (s *Resolver) resolveBlockedBy(p graphql.ResolveParams, add bool) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	issue, err := s.issueFromInput(input, "issueId")
	if err != nil {
		return nil, err
	}
	blocker, err := s.issueFromInput(input, "blockingIssueId")
	if err != nil {
		return nil, err
	}
	if issue.ID == blocker.ID {
		return nil, fmt.Errorf("an issue cannot block itself")
	}
	if add {
		s.store.AddIssueBlockedBy(issue.ID, blocker.ID)
	} else {
		s.store.RemoveIssueBlockedBy(issue.ID, blocker.ID)
	}
	return map[string]interface{}{
		"issue":         optionalRenderedIssue(s.store.GetIssue(issue.ID), s.store),
		"blockingIssue": optionalRenderedIssue(s.store.GetIssue(blocker.ID), s.store),
	}, nil
}

// --- the duplicate relation --------------------------------------------------

func (s *Resolver) resolveUnmarkIssueAsDuplicate(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	canonical, err := s.issueFromInput(input, "canonicalId")
	if err != nil {
		return nil, err
	}
	duplicate, err := s.issueFromInput(input, "duplicateId")
	if err != nil {
		return nil, err
	}
	before := s.store.GetIssue(duplicate.ID)
	if before == nil {
		return nil, gqlMissingNodeType("Issue")
	}
	if before.DuplicateOfID != canonical.ID {
		return nil, fmt.Errorf("the issue is not marked as a duplicate of that issue")
	}
	s.store.UpdateIssue(duplicate.ID, func(i *store.Issue) { i.DuplicateOfID = 0 })
	updated := s.store.GetIssue(duplicate.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("Issue")
	}
	repo := s.store.GetRepoByID(updated.RepoID)
	user := s.ghUserFromContext(p.Context)
	if repo != nil && user != nil {
		s.store.RecordIssueEvent(repo.ID, updated.ID, user.ID, "unmarked_as_duplicate", map[string]interface{}{})
	}
	return map[string]interface{}{"duplicate": issueToGQL(updated, s.store)}, nil
}

// --- issue types --------------------------------------------------------------

func (s *Resolver) resolveCreateIssueType(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	ownerID, _ := gqlInputString(input, "ownerId")
	org := s.orgByNodeID(ownerID)
	if org == nil {
		return nil, gqlMissingNode("Organization", ownerID)
	}
	name, _ := gqlInputString(input, "name")
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("Name can't be blank")
	}
	description := optionalStringPtr(input, "description")
	color := optionalLowerStringPtr(input, "color")
	enabled, _ := gqlInputBool(input, "isEnabled")

	issueType := s.store.CreateIssueType(org.Login, name, description, color, enabled)
	if issueType == nil {
		return nil, fmt.Errorf("Name has already been taken")
	}
	return map[string]interface{}{"issueType": optionalObject(issueTypeToGQL(issueType))}, nil
}

func (s *Resolver) resolveUpdateIssueType(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "issueTypeId")
	existing := store.FindIssueTypeByNodeID(s.store, nodeID)
	if existing == nil {
		return nil, gqlMissingNode("IssueType", nodeID)
	}
	orgLogin := existing.OrgLogin
	name, renaming := gqlInputString(input, "name")
	if !renaming {
		name = existing.Name
	}
	description := optionalStringPtr(input, "description")
	if description == nil {
		description = existing.Description
	}
	color := optionalLowerStringPtr(input, "color")
	if color == nil {
		color = existing.Color
	}
	enabled, toggling := gqlInputBool(input, "isEnabled")
	if !toggling {
		enabled = existing.IsEnabled
	}

	updated := s.store.UpdateIssueType(orgLogin, existing.ID, name, description, color, enabled)
	if updated == nil {
		return nil, fmt.Errorf("Name has already been taken")
	}
	return map[string]interface{}{"issueType": optionalObject(issueTypeToGQL(updated))}, nil
}

func (s *Resolver) resolveDeleteIssueType(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "issueTypeId")
	existing := store.FindIssueTypeByNodeID(s.store, nodeID)
	if existing == nil {
		return nil, gqlMissingNode("IssueType", nodeID)
	}
	orgLogin := existing.OrgLogin
	if !s.store.DeleteIssueType(orgLogin, existing.ID) {
		return nil, gqlMissingNodeType("IssueType")
	}
	return map[string]interface{}{"deletedIssueTypeId": nodeID}, nil
}

func (s *Resolver) resolveUpdateIssueIssueType(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	issue, err := s.issueFromInput(input, "issueId")
	if err != nil {
		return nil, err
	}
	repo := s.store.GetRepoByID(issue.RepoID)
	if repo == nil {
		return nil, gqlMissingNodeType("Repository")
	}
	typeID := 0
	if nodeID, ok := gqlInputString(input, "issueTypeId"); ok && nodeID != "" {
		issueType := store.FindIssueTypeByNodeID(s.store, nodeID)
		if issueType == nil || s.store.GetAssignableIssueTypeForRepo(repo, issueType.ID) == nil {
			return nil, gqlMissingNode("IssueType", nodeID)
		}
		typeID = issueType.ID
	}
	s.store.UpdateIssue(issue.ID, func(i *store.Issue) { i.IssueTypeID = typeID })
	updated := s.store.GetIssue(issue.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("Issue")
	}
	s.emitIssueChanges(repo, updated, s.ghUserFromContext(p.Context), store.SubjectChange{})
	return map[string]interface{}{"issue": issueToGQL(updated, s.store)}, nil
}

// --- custom issue fields ------------------------------------------------------

func (s *Resolver) resolveCreateIssueField(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	ownerID, _ := gqlInputString(input, "ownerId")
	org := s.orgByNodeID(ownerID)
	if org == nil {
		return nil, gqlMissingNode("Organization", ownerID)
	}
	name, _ := gqlInputString(input, "name")
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("Name can't be blank")
	}
	dataType, _ := gqlInputString(input, "dataType")
	visibility, _ := gqlInputString(input, "visibility")
	field := s.store.CreateIssueField(org.Login, name,
		optionalStringPtr(input, "description"),
		strings.ToLower(dataType), issueFieldVisibilityStored(visibility),
		issueFieldOptionRequests(input))
	if field == nil {
		return nil, fmt.Errorf("Name has already been taken")
	}
	return map[string]interface{}{"issueField": optionalObject(s.issueFieldToGQL(field))}, nil
}

func (s *Resolver) resolveUpdateIssueField(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "id")
	org, field := s.issueFieldByNodeID(nodeID)
	if field == nil {
		return nil, gqlMissingNode("IssueFields", nodeID)
	}
	var namePtr, descriptionPtr, visibilityPtr *string
	if name, ok := gqlInputString(input, "name"); ok {
		namePtr = &name
	}
	descriptionPtr = optionalStringPtr(input, "description")
	if visibility, ok := gqlInputString(input, "visibility"); ok {
		stored := issueFieldVisibilityStored(visibility)
		visibilityPtr = &stored
	}
	updated := s.store.UpdateIssueField(org, field.ID, namePtr, descriptionPtr, visibilityPtr,
		issueFieldOptionRequests(input))
	if updated == nil {
		return nil, gqlMissingNodeType("IssueFields")
	}
	return map[string]interface{}{"issueField": optionalObject(s.issueFieldToGQL(updated))}, nil
}

func (s *Resolver) resolveDeleteIssueField(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "fieldId")
	org, field := s.issueFieldByNodeID(nodeID)
	if field == nil {
		return nil, gqlMissingNode("IssueFields", nodeID)
	}
	// The payload carries the field as it was, so it is rendered before the
	// definition is destroyed.
	rendered := s.issueFieldToGQL(field)
	if !s.store.DeleteIssueField(org, field.ID) {
		return nil, gqlMissingNodeType("IssueFields")
	}
	return map[string]interface{}{"issueField": optionalObject(rendered)}, nil
}

func (s *Resolver) resolveWriteIssueFieldValue(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	issue, repo, err := s.issueAndRepoFromInput(input, "issueId")
	if err != nil {
		return nil, err
	}
	spec, ok := gqlInputObject(input, "issueField")
	if !ok {
		return nil, fmt.Errorf("issueField is required")
	}
	values, rendered, err := s.issueFieldWrite(repo, issue, spec, p)
	if err != nil {
		return nil, err
	}
	if len(values) > 0 {
		s.store.AddIssueFieldValues(issue.ID, values)
	}
	updated := s.store.GetIssue(issue.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("Issue")
	}
	payload := map[string]interface{}{"issue": issueToGQL(updated, s.store)}
	if len(rendered) > 0 {
		payload["issueFieldValue"] = rendered[0]
	} else {
		payload["issueFieldValue"] = nil
	}
	return payload, nil
}

func (s *Resolver) resolveSetIssueFieldValue(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	issue, repo, err := s.issueAndRepoFromInput(input, "issueId")
	if err != nil {
		return nil, err
	}
	values := map[int]interface{}{}
	var rendered []interface{}
	for _, spec := range gqlInputObjects(input, "issueFields") {
		written, renderedOne, err := s.issueFieldWrite(repo, issue, spec, p)
		if err != nil {
			return nil, err
		}
		for fieldID, value := range written {
			values[fieldID] = value
		}
		rendered = append(rendered, renderedOne...)
	}
	// setIssueFieldValue replaces the issue's field values with exactly what
	// it names, which is what SetIssueFieldValues writes.
	s.store.SetIssueFieldValues(issue.ID, values)
	updated := s.store.GetIssue(issue.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("Issue")
	}
	return map[string]interface{}{
		"issue":            issueToGQL(updated, s.store),
		"issueFieldValues": rendered,
	}, nil
}

func (s *Resolver) resolveDeleteIssueFieldValue(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	issue, _, err := s.issueAndRepoFromInput(input, "issueId")
	if err != nil {
		return nil, err
	}
	nodeID, _ := gqlInputString(input, "fieldId")
	_, field := s.issueFieldByNodeID(nodeID)
	if field == nil {
		return nil, gqlMissingNode("IssueFields", nodeID)
	}
	removed := s.store.DeleteIssueFieldValue(issue.ID, field.ID)
	updated := s.store.GetIssue(issue.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("Issue")
	}
	return map[string]interface{}{
		"issue":   issueToGQL(updated, s.store),
		"success": removed,
	}, nil
}

// issueFieldWrite turns one IssueFieldCreateOrUpdateInput into the store value
// to write and the payload member to answer with. A member carrying
// `suggest: true` writes nothing: it queues a pending suggestion, which is
// what the flag means.
func (s *Resolver) issueFieldWrite(repo *store.Repo, issue *store.Issue, spec map[string]interface{}, p graphql.ResolveParams) (map[int]interface{}, []interface{}, error) {
	nodeID, _ := gqlInputString(spec, "fieldId")
	org, field := s.issueFieldByNodeID(nodeID)
	if field == nil || org == "" {
		return nil, nil, gqlMissingNode("IssueFields", nodeID)
	}
	if remove, _ := gqlInputBool(spec, "delete"); remove {
		s.store.DeleteIssueFieldValue(issue.ID, field.ID)
		return nil, nil, nil
	}
	value, err := issueFieldValueFromInput(field, spec)
	if err != nil {
		return nil, nil, err
	}
	if suggest, _ := gqlInputBool(spec, "suggest"); suggest {
		target := field.ID
		var actorID *int
		if user := s.ghUserFromContext(p.Context); user != nil {
			id := user.ID
			actorID = &id
		}
		s.store.CreateIssueSuggestion(repo.FullName, issue.ID, store.IssueSuggestion{
			Action:      "add_field",
			TargetID:    &target,
			TargetValue: value,
			ActorID:     actorID,
		})
		return nil, nil, nil
	}
	rendered := s.issueFieldValueToGQL(field, issue.ID, value)
	return map[int]interface{}{field.ID: value}, []interface{}{rendered}, nil
}

// issueFieldValueFromInput picks the member of the input that matches the
// field's data type, refusing one that names a value of the wrong shape.
func issueFieldValueFromInput(field *store.IssueField, spec map[string]interface{}) (interface{}, error) {
	switch field.DataType {
	case "date":
		value, ok := gqlInputString(spec, "dateValue")
		if !ok {
			return nil, fmt.Errorf("dateValue is required for a DATE field")
		}
		return value, nil
	case "number":
		value, ok := spec["numberValue"].(float64)
		if !ok {
			return nil, fmt.Errorf("numberValue is required for a NUMBER field")
		}
		return value, nil
	case "single_select":
		optionID, ok := gqlInputString(spec, "singleSelectOptionId")
		if !ok {
			return nil, fmt.Errorf("singleSelectOptionId is required for a SINGLE_SELECT field")
		}
		option := issueFieldOptionByNodeID(field, optionID)
		if option == nil {
			return nil, gqlMissingNode("IssueFieldSingleSelectOption", optionID)
		}
		return option.Name, nil
	case "multi_select":
		ids, _ := gqlInputStrings(spec, "multiSelectOptionIds")
		names := make([]string, 0, len(ids))
		for _, id := range ids {
			option := issueFieldOptionByNodeID(field, id)
			if option == nil {
				return nil, gqlMissingNode("IssueFieldSingleSelectOption", id)
			}
			names = append(names, option.Name)
		}
		return names, nil
	default:
		value, ok := gqlInputString(spec, "textValue")
		if !ok {
			return nil, fmt.Errorf("textValue is required for a TEXT field")
		}
		return value, nil
	}
}

func issueFieldOptionByNodeID(field *store.IssueField, nodeID string) *store.IssueFieldOption {
	for _, option := range field.Options {
		if issueFieldOptionNodeID(option.ID) == nodeID {
			return option
		}
	}
	return nil
}

// issueFieldOptionRequests reads the options[] member into the shape the store
// primitives take. An absent list leaves the field's options alone.
func issueFieldOptionRequests(input map[string]interface{}) []store.IssueFieldOptionRequest {
	specs := gqlInputObjects(input, "options")
	if len(specs) == 0 {
		return nil
	}
	out := make([]store.IssueFieldOptionRequest, 0, len(specs))
	for _, spec := range specs {
		name, _ := gqlInputString(spec, "name")
		color, _ := gqlInputString(spec, "color")
		priority, _ := gqlInputInt(spec, "priority")
		lowerColor := strings.ToLower(color)
		request := store.IssueFieldOptionRequest{
			Name:     &name,
			Color:    &lowerColor,
			Priority: &priority,
		}
		if description, ok := gqlInputString(spec, "description"); ok {
			request.Description = &description
		}
		out = append(out, request)
	}
	return out
}

func issueFieldVisibilityStored(value string) string {
	if value == "ORG_ONLY" {
		return "org_only"
	}
	return "all"
}

// issueFieldByNodeID resolves an IssueFields node id to the organization that
// owns it and the definition itself.
func (s *Resolver) issueFieldByNodeID(nodeID string) (string, *store.IssueField) {
	if nodeID == "" {
		return "", nil
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for org, fields := range s.store.OrgIssueFields {
		for _, field := range fields {
			if field.NodeID == nodeID {
				clone := *field
				clone.Options = append([]*store.IssueFieldOption(nil), field.Options...)
				return org, &clone
			}
		}
	}
	return "", nil
}

// issueTypeToGQL renders an issue type into the shared IssueType source shape
// the read surface uses, so a type read back after a mutation is the same
// object Issue.issueType resolves.
func issueTypeToGQL(issueType *store.IssueType) map[string]interface{} {
	if issueType == nil {
		return nil
	}
	color := "GRAY"
	if issueType.Color != nil && *issueType.Color != "" {
		color = strings.ToUpper(*issueType.Color)
	}
	return map[string]interface{}{
		"id":          issueType.NodeID,
		"name":        issueType.Name,
		"description": nilStrPtr(issueType.Description),
		"color":       color,
	}
}

func (s *Resolver) issueFieldToGQL(field *store.IssueField) map[string]interface{} {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return issueFieldToGQLLocked(field)
}

func (s *Resolver) issueFieldValueToGQL(field *store.IssueField, issueID int, value interface{}) map[string]interface{} {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return issueFieldValueToGQLLocked(field, issueID, value)
}

// --- the pending-suggestion queue ---------------------------------------------

func (s *Resolver) resolvePendingIssueSuggestions(p graphql.ResolveParams, apply bool) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	issue, repo, err := s.issueAndRepoFromInput(input, "issueId")
	if err != nil {
		return nil, err
	}
	actorNodeID, _ := gqlInputString(input, "actorId")
	actor := store.FindUserByNodeID(s.store, actorNodeID)
	if actor == nil {
		return nil, gqlMissingNode("Actor", actorNodeID)
	}
	user := s.ghUserFromContext(p.Context)

	refs := gqlInputObjects(input, "suggestions")
	if len(refs) == 0 {
		return nil, fmt.Errorf("suggestions names nothing")
	}
	for _, ref := range refs {
		suggestion, err := s.pendingSuggestionForRef(repo, issue, actor, ref)
		if err != nil {
			return nil, err
		}
		state := "dismissed"
		var eventID *int
		if apply {
			state = "approved"
			event, err := s.store.PerformIssueSuggestion(repo, issue, suggestion, user.ID)
			if err != nil {
				return nil, err
			}
			eventID = &event.ID
		}
		if s.store.ResolveIssueSuggestion(repo.FullName, issue.ID, suggestion.ID, user.ID, state, eventID) == nil {
			return nil, fmt.Errorf("the suggestion is no longer pending")
		}
	}
	updated := s.store.GetIssue(issue.ID)
	if updated == nil {
		return nil, gqlMissingNodeType("Issue")
	}
	return map[string]interface{}{"issue": issueToGQL(updated, s.store)}, nil
}

// pendingSuggestionForRef finds the queued suggestion a
// PendingIssueSuggestionRef addresses: the actor's pending suggestion of that
// kind, aimed at the record the ref names.
func (s *Resolver) pendingSuggestionForRef(repo *store.Repo, issue *store.Issue, actor *store.User, ref map[string]interface{}) (*store.IssueSuggestion, error) {
	kind, _ := gqlInputString(ref, "kind")
	action, idKey := pendingSuggestionAction(kind)
	if action == "" {
		return nil, fmt.Errorf("kind %q is not a suggestion kind", kind)
	}
	wantTarget := 0
	if idKey != "" {
		nodeID, ok := gqlInputString(ref, idKey)
		if !ok || nodeID == "" {
			return nil, fmt.Errorf("%s is required for a %s suggestion", idKey, kind)
		}
		resolved, err := s.suggestionTargetID(idKey, nodeID)
		if err != nil {
			return nil, err
		}
		wantTarget = resolved
	}
	for _, suggestion := range s.store.ListIssueSuggestions(repo.FullName, issue.ID) {
		if suggestion.State != "pending" || suggestion.Action != action {
			continue
		}
		if suggestion.ActorID == nil || *suggestion.ActorID != actor.ID {
			continue
		}
		if idKey != "" && (suggestion.TargetID == nil || *suggestion.TargetID != wantTarget) {
			continue
		}
		return suggestion, nil
	}
	return nil, gqlMissingNodeType("IssueSuggestion")
}

func pendingSuggestionAction(kind string) (action, idKey string) {
	switch kind {
	case "ASSIGNEE":
		return "add_assignee", "assigneeId"
	case "CLOSE":
		return "close_issue", ""
	case "FIELD":
		return "add_field", "issueFieldId"
	case "LABEL":
		return "add_label", "labelId"
	case "TYPE":
		return "set_type", "issueTypeId"
	}
	return "", ""
}

// suggestionTargetID resolves the node id a suggestion ref names to the
// database id the queued suggestion records.
func (s *Resolver) suggestionTargetID(idKey, nodeID string) (int, error) {
	switch idKey {
	case "assigneeId":
		if actor := store.FindUserByNodeID(s.store, nodeID); actor != nil {
			return actor.ID, nil
		}
		return 0, gqlMissingNode("Actor", nodeID)
	case "labelId":
		if label := store.FindLabelByNodeID(s.store, nodeID); label != nil {
			return label.ID, nil
		}
		return 0, gqlMissingNode("Label", nodeID)
	case "issueTypeId":
		if issueType := store.FindIssueTypeByNodeID(s.store, nodeID); issueType != nil {
			return issueType.ID, nil
		}
		return 0, gqlMissingNode("IssueType", nodeID)
	case "issueFieldId":
		if _, field := s.issueFieldByNodeID(nodeID); field != nil {
			return field.ID, nil
		}
		return 0, gqlMissingNode("IssueFields", nodeID)
	}
	return 0, gqlMissingNodeType("Node")
}

// --- shared helpers -----------------------------------------------------------

// issueFromInput resolves the issue a mutation input names.
func (s *Resolver) issueFromInput(input map[string]interface{}, key string) (*store.Issue, error) {
	nodeID, _ := gqlInputString(input, key)
	issue := store.FindIssueByNodeID(s.store, nodeID)
	if issue == nil {
		return nil, gqlMissingNode("Issue", nodeID)
	}
	// The node finder hands back the live row; a detached snapshot is what a
	// caller may read fields off (STORE-021).
	snapshot := s.store.GetIssue(issue.ID)
	if snapshot == nil {
		return nil, gqlMissingNodeType("Issue")
	}
	return snapshot, nil
}

func (s *Resolver) issueAndRepoFromInput(input map[string]interface{}, key string) (*store.Issue, *store.Repo, error) {
	issue, err := s.issueFromInput(input, key)
	if err != nil {
		return nil, nil, err
	}
	repo := s.store.GetRepoByID(issue.RepoID)
	if repo == nil {
		return nil, nil, gqlMissingNodeType("Repository")
	}
	return issue, repo, nil
}

// subIssueFromInput resolves addSubIssue's child, which GitHub lets a client
// name either by node id or by the issue's web URL.
func (s *Resolver) subIssueFromInput(input map[string]interface{}) (*store.Issue, error) {
	if nodeID, ok := gqlInputString(input, "subIssueId"); ok && nodeID != "" {
		return s.issueFromInput(input, "subIssueId")
	}
	url, ok := gqlInputString(input, "subIssueUrl")
	if !ok || url == "" {
		return nil, fmt.Errorf("one of subIssueId or subIssueUrl is required")
	}
	issue := s.issueByWebURL(url)
	if issue == nil {
		return nil, gqlMissingNodeType("Issue")
	}
	return issue, nil
}

// issueByWebURL resolves ".../{owner}/{repo}/issues/{number}" to its issue.
func (s *Resolver) issueByWebURL(url string) *store.Issue {
	trimmed := strings.TrimSuffix(url, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 4 {
		return nil
	}
	number, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || number <= 0 || parts[len(parts)-2] != "issues" {
		return nil
	}
	repo := s.store.GetRepo(parts[len(parts)-4], parts[len(parts)-3])
	if repo == nil {
		return nil
	}
	return s.store.GetIssueByNumber(repo.ID, number)
}

func (s *Resolver) optionalIssueID(input map[string]interface{}, key string) (*int, error) {
	nodeID, ok := gqlInputString(input, key)
	if !ok || nodeID == "" {
		return nil, nil
	}
	issue := store.FindIssueByNodeID(s.store, nodeID)
	if issue == nil {
		return nil, gqlMissingNode("Issue", nodeID)
	}
	id := issue.ID
	return &id, nil
}

// optionalRenderedIssue renders an issue that may be absent into the value a
// nilable payload member may safely hold.
func optionalRenderedIssue(issue *store.Issue, st *store.Store) interface{} {
	if issue == nil {
		return nil
	}
	return issueToGQL(issue, st)
}

// optionalStringPtr reads an optional String member as the pointer the store
// primitives use to distinguish "leave alone" from "set".
func optionalStringPtr(input map[string]interface{}, key string) *string {
	value, ok := gqlInputString(input, key)
	if !ok {
		return nil
	}
	return &value
}

func optionalLowerStringPtr(input map[string]interface{}, key string) *string {
	value, ok := gqlInputString(input, key)
	if !ok {
		return nil
	}
	lower := strings.ToLower(value)
	return &lower
}

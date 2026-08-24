package graphqlapi

// The account-scoped mutation surface: the follow graph, the profile status,
// the repository lists a user keeps on their stars page, and the
// notification-delivery restriction an organization or enterprise sets.
//
// These mutations name no repository, so their policy rows do not go through
// repoRule. Each names either the viewer's own account — in which case the
// entitlement is the credential's grant over that account, which is what
// refuses an app installed elsewhere — or an organization or enterprise, in
// which case it is ownership of that account, which is what makes the refusal
// cross-tenant.

import (
	"fmt"
	"strings"
	"time"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// --- authorization rules ----------------------------------------------------

// viewerAccountRule is the policy for a mutation that changes the viewer's own
// account. Authentication is asked by the registrar, so what is left is the
// credential's grant: a user-to-server token of an app installed nowhere, or a
// fine-grained token belonging to somebody else, holds no such grant and is
// refused where the bearer's own session would be served.
type viewerAccountRule struct {
	scope store.PermScope
}

func (r viewerAccountRule) check() error {
	if r.scope == "" {
		return fmt.Errorf("no permission scope")
	}
	return nil
}

func (r viewerAccountRule) authorize(s *Resolver, p graphql.ResolveParams, _ map[string]interface{}) error {
	viewer := s.ghUserFromContext(p.Context)
	if viewer == nil {
		return fmt.Errorf("authentication required")
	}
	if !s.credentialGrantsAccount(p.Context, store.AnyAccount, viewer.Login, r.scope, store.PermWrite) {
		return &ghForbiddenError{message: "resource not accessible by integration"}
	}
	return nil
}

// userListRule is viewerAccountRule for the two mutations that name an
// existing list: the list has to be the viewer's own. Somebody else's list is
// answered as though it did not exist, so the mutation is not a way to learn
// that a private list exists.
type userListRule struct {
	idKey string
}

func (r userListRule) check() error {
	if r.idKey == "" {
		return fmt.Errorf("no list id input key")
	}
	return nil
}

func (r userListRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	viewer := s.ghUserFromContext(p.Context)
	if viewer == nil {
		return fmt.Errorf("authentication required")
	}
	nodeID, _ := input[r.idKey].(string)
	list := store.FindUserListByNodeID(s.store, nodeID)
	if list == nil || list.UserID != viewer.ID {
		return gqlMissingNode("UserList", nodeID)
	}
	if !s.credentialGrantsAccount(p.Context, store.AnyAccount, viewer.Login, store.ScopeMetadata, store.PermWrite) {
		return &ghForbiddenError{message: "resource not accessible by integration"}
	}
	return nil
}

// notificationRestrictionRule is the policy for
// updateNotificationRestrictionSetting, whose owner is an enterprise or an
// organization. Either way the entitlement is ownership of that account, so
// owning one organization never authorizes the write against another.
type notificationRestrictionRule struct{}

func (notificationRestrictionRule) check() error { return nil }

func (notificationRestrictionRule) authorize(s *Resolver, p graphql.ResolveParams, input map[string]interface{}) error {
	nodeID, _ := input["ownerId"].(string)
	viewer := s.ghUserFromContext(p.Context)
	if enterprise := store.FindEnterpriseByNodeID(s.store, nodeID); enterprise != nil {
		if !s.store.IsEnterpriseOwner(enterprise.ID, viewer) {
			return enterpriseOwnerRequired()
		}
		return nil
	}
	org := s.orgByNodeID(nodeID)
	if org == nil {
		return gqlMissingNode("VerifiableDomainOwner", nodeID)
	}
	if !s.viewerCanAdminAccount(p.Context, org.Login) {
		return &ghForbiddenError{message: "You must be an owner of the organization to perform this action."}
	}
	if !s.credentialGrantsAccount(p.Context, store.OrganizationAccount, org.Login, store.ScopeOrgAdministration, store.PermWrite) {
		return &ghForbiddenError{message: "resource not accessible by integration"}
	}
	return nil
}

// --- schema -----------------------------------------------------------------

func (s *Resolver) addAccountActivityMutations(mutationType *graphql.Object) {
	userType := s.graphqlTypes.user
	orgType := s.graphqlTypes.organization
	userStatusType := s.gqlUserStatusType()
	userListType := s.gqlUserListType()
	dateTime := s.graphQLStringScalar("DateTime")

	// --- the follow graph --------------------------------------------------

	follow := func(name, payloadName, inputName, idKey string, payloadField string, payloadType *graphql.Object, following bool) {
		s.registerMutation(mutationType, name, &graphql.Field{
			Type: s.mutationPayload(payloadName, graphql.Fields{
				payloadField: gqlField(payloadType),
			}),
			Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
				Type: graphql.NewNonNull(s.mutationInput(inputName, graphql.InputObjectConfigFieldMap{
					idKey: gqlNonNullID(),
				})),
			}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				return s.resolveFollow(p, idKey, payloadField, following)
			},
		})
	}
	follow("followUser", "FollowUserPayload", "FollowUserInput", "userId", "user", userType, true)
	follow("unfollowUser", "UnfollowUserPayload", "UnfollowUserInput", "userId", "user", userType, false)
	follow("followOrganization", "FollowOrganizationPayload", "FollowOrganizationInput", "organizationId", "organization", orgType, true)
	follow("unfollowOrganization", "UnfollowOrganizationPayload", "UnfollowOrganizationInput", "organizationId", "organization", orgType, false)

	// --- the profile status ------------------------------------------------

	s.registerMutation(mutationType, "changeUserStatus", &graphql.Field{
		Type: s.mutationPayload("ChangeUserStatusPayload", graphql.Fields{
			"status": gqlField(userStatusType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("ChangeUserStatusInput", graphql.InputObjectConfigFieldMap{
				"emoji":               gqlString(),
				"message":             gqlString(),
				"expiresAt":           gqlInputOf(dateTime),
				"limitedAvailability": &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: false},
				"organizationId":      gqlID(),
			})),
		}},
		Resolve: s.resolveChangeUserStatus,
	})

	// --- the stars-page lists ----------------------------------------------

	s.registerMutation(mutationType, "createUserList", &graphql.Field{
		Type: s.mutationPayload("CreateUserListPayload", graphql.Fields{
			"list":   gqlField(userListType),
			"viewer": gqlField(userType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("CreateUserListInput", graphql.InputObjectConfigFieldMap{
				"name":        gqlNonNullString(),
				"description": gqlString(),
				"isPrivate":   &graphql.InputObjectFieldConfig{Type: graphql.Boolean, DefaultValue: false},
			})),
		}},
		Resolve: s.resolveCreateUserList,
	})

	s.registerMutation(mutationType, "updateUserList", &graphql.Field{
		Type: s.mutationPayload("UpdateUserListPayload", graphql.Fields{
			"list": gqlField(userListType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateUserListInput", graphql.InputObjectConfigFieldMap{
				"listId":      gqlNonNullID(),
				"name":        gqlString(),
				"description": gqlString(),
				"isPrivate":   gqlBool(),
			})),
		}},
		Resolve: s.resolveUpdateUserList,
	})

	s.registerMutation(mutationType, "deleteUserList", &graphql.Field{
		Type: s.mutationPayload("DeleteUserListPayload", graphql.Fields{
			"user": gqlField(userType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("DeleteUserListInput", graphql.InputObjectConfigFieldMap{
				"listId": gqlNonNullID(),
			})),
		}},
		Resolve: s.resolveDeleteUserList,
	})

	s.registerMutation(mutationType, "updateUserListsForItem", &graphql.Field{
		Type: s.mutationPayload("UpdateUserListsForItemPayload", graphql.Fields{
			"item":  gqlField(s.gqlUserListItemsUnion()),
			"lists": gqlFieldListOf(userListType),
			"user":  gqlField(userType),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateUserListsForItemInput", graphql.InputObjectConfigFieldMap{
				"itemId":           gqlNonNullID(),
				"listIds":          gqlNonNullListOf(graphql.ID),
				"suggestedListIds": gqlListOf(graphql.ID),
			})),
		}},
		Resolve: s.resolveUpdateUserListsForItem,
	})

	// --- notification delivery ---------------------------------------------

	s.registerMutation(mutationType, "updateNotificationRestrictionSetting", &graphql.Field{
		Type: s.mutationPayload("UpdateNotificationRestrictionSettingPayload", graphql.Fields{
			"owner": gqlField(s.gqlVerifiableDomainOwnerUnion()),
		}),
		Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{
			Type: graphql.NewNonNull(s.mutationInput("UpdateNotificationRestrictionSettingInput", graphql.InputObjectConfigFieldMap{
				"ownerId": gqlNonNullID(),
				"settingValue": gqlNonNullInputOf(
					s.sharedEnum("NotificationRestrictionSettingValue", "DISABLED", "ENABLED")),
			})),
		}},
		Resolve: s.resolveUpdateNotificationRestrictionSetting,
	})
}

// --- types ------------------------------------------------------------------

func (s *Resolver) gqlUserStatusType() *graphql.Object {
	dateTime := s.graphQLStringScalar("DateTime")
	html := s.graphQLStringScalar("HTML")
	return s.mutationObject("UserStatus", graphql.Fields{
		"id":                           gqlNonNull(graphql.ID),
		"emoji":                        gqlField(graphql.String),
		"emojiHTML":                    gqlField(html),
		"message":                      gqlField(graphql.String),
		"indicatesLimitedAvailability": gqlNonNull(graphql.Boolean),
		"expiresAt":                    gqlField(dateTime),
		"createdAt":                    gqlNonNull(dateTime),
		"updatedAt":                    gqlNonNull(dateTime),
		"organization":                 gqlField(s.graphqlTypes.organization),
		"user":                         gqlNonNull(s.graphqlTypes.user),
	})
}

func (s *Resolver) userStatusToGQL(status *store.UserStatus) map[string]interface{} {
	if status == nil {
		return nil
	}
	var org map[string]interface{}
	if status.OrganizationID != 0 {
		org = optionalRenderedOrg(s.store.GetOrgByID(status.OrganizationID))
	}
	var emojiHTML interface{}
	if status.Emoji != "" {
		emojiHTML = "<g-emoji>" + status.Emoji + "</g-emoji>"
	}
	return map[string]interface{}{
		"id":                           store.UserStatusNodeID(status.UserID),
		"emoji":                        nilStr(status.Emoji),
		"emojiHTML":                    emojiHTML,
		"message":                      nilStr(status.Message),
		"indicatesLimitedAvailability": status.LimitedAvailability,
		"expiresAt":                    nullableTimePtr(status.ExpiresAt),
		"createdAt":                    status.CreatedAt.Format(time.RFC3339),
		"updatedAt":                    status.UpdatedAt.Format(time.RFC3339),
		"organization":                 optionalObject(org),
		"user":                         optionalRendered(s.store.GetUserByID(status.UserID), userToGraphQL),
	}
}

func optionalRenderedOrg(org *store.Org) map[string]interface{} {
	if org == nil {
		return nil
	}
	return orgToGraphQL(org)
}

func (s *Resolver) gqlUserListType() *graphql.Object {
	dateTime := s.graphQLStringScalar("DateTime")
	return s.mutationObject("UserList", graphql.Fields{
		"id":          gqlNonNull(graphql.ID),
		"name":        gqlNonNull(graphql.String),
		"slug":        gqlNonNull(graphql.String),
		"description": gqlField(graphql.String),
		"isPrivate":   gqlNonNull(graphql.Boolean),
		"createdAt":   gqlNonNull(dateTime),
		"updatedAt":   gqlNonNull(dateTime),
		"lastAddedAt": gqlNonNull(dateTime),
		"user":        gqlNonNull(s.graphqlTypes.user),
		"items": &graphql.Field{
			Type: graphql.NewNonNull(s.gqlUserListItemsConnectionType()),
			Args: graphql.FieldConfigArgument{
				"first":  &graphql.ArgumentConfig{Type: graphql.Int},
				"last":   &graphql.ArgumentConfig{Type: graphql.Int},
				"after":  &graphql.ArgumentConfig{Type: graphql.String},
				"before": &graphql.ArgumentConfig{Type: graphql.String},
			},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				source, _ := p.Source.(map[string]interface{})
				return repaginateConnection(source["items"], p.Args), nil
			},
		},
	})
}

func (s *Resolver) gqlUserListItemsUnion() *graphql.Union {
	return s.mutationUnion("UserListItems",
		func() []*graphql.Object { return []*graphql.Object{s.graphqlTypes.repository} },
		func(graphql.ResolveTypeParams) *graphql.Object { return s.graphqlTypes.repository })
}

func (s *Resolver) gqlUserListItemsConnectionType() *graphql.Object {
	edge := s.mutationObject("UserListItemsEdge", graphql.Fields{
		"cursor": gqlNonNull(graphql.String),
		"node":   gqlField(s.gqlUserListItemsUnion()),
	})
	return s.mutationObject("UserListItemsConnection", graphql.Fields{
		"edges":      gqlField(graphql.NewList(edge)),
		"nodes":      gqlField(graphql.NewList(s.gqlUserListItemsUnion())),
		"totalCount": gqlNonNull(graphql.Int),
		"pageInfo":   gqlNonNull(s.gqlPageInfoType()),
	})
}

// gqlVerifiableDomainOwnerUnion is the Enterprise | Organization union the
// verifiable-domain and notification-restriction payloads return.
func (s *Resolver) gqlVerifiableDomainOwnerUnion() *graphql.Union {
	return s.mutationUnion("VerifiableDomainOwner",
		func() []*graphql.Object {
			return []*graphql.Object{s.graphqlTypes.enterprise, s.graphqlTypes.organization}
		},
		func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			if _, isEnterprise := source["slug"]; isEnterprise {
				return s.graphqlTypes.enterprise
			}
			return s.graphqlTypes.organization
		})
}

func (s *Resolver) userListToGQL(list *store.UserList) map[string]interface{} {
	if list == nil {
		return nil
	}
	items := make([]map[string]interface{}, 0, len(list.RepoIDs))
	for _, repoID := range list.RepoIDs {
		repo := s.store.GetRepoByID(repoID)
		if repo == nil {
			continue
		}
		items = append(items, repoToGraphQL(s.store, repo))
	}
	// GitHub's lastAddedAt is non-null; a list nothing has been added to
	// answers with its own creation time, which is when it last changed size.
	lastAdded := list.CreatedAt
	if list.LastAddedAt != nil {
		lastAdded = *list.LastAddedAt
	}
	return map[string]interface{}{
		"id":          list.NodeID,
		"name":        list.Name,
		"slug":        list.Slug,
		"description": nilStr(list.Description),
		"isPrivate":   list.IsPrivate,
		"createdAt":   list.CreatedAt.Format(time.RFC3339),
		"updatedAt":   list.UpdatedAt.Format(time.RFC3339),
		"lastAddedAt": lastAdded.Format(time.RFC3339),
		"user":        optionalRendered(s.store.GetUserByID(list.UserID), userToGraphQL),
		"items":       gqlConnectionSource(items),
	}
}

// --- resolvers --------------------------------------------------------------

func (s *Resolver) resolveFollow(p graphql.ResolveParams, idKey, payloadField string, following bool) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, idKey)
	viewer := s.ghUserFromContext(p.Context)

	if idKey == "userId" {
		target := store.FindUserByNodeID(s.store, nodeID)
		if target == nil {
			return nil, gqlMissingNode("User", nodeID)
		}
		if target.ID == viewer.ID {
			return nil, fmt.Errorf("an account cannot follow itself")
		}
		s.store.SetFollow(viewer.Login, target.Login, following)
		return map[string]interface{}{payloadField: optionalRendered(s.store.GetUserByID(target.ID), userToGraphQL)}, nil
	}

	org := s.orgByNodeID(nodeID)
	if org == nil {
		return nil, gqlMissingNode("Organization", nodeID)
	}
	s.store.SetFollow(viewer.Login, org.Login, following)
	return map[string]interface{}{payloadField: optionalObject(orgToGraphQL(org))}, nil
}

func (s *Resolver) resolveChangeUserStatus(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	viewer := s.ghUserFromContext(p.Context)

	status := store.UserStatus{}
	status.Emoji, _ = gqlInputString(input, "emoji")
	status.Message, _ = gqlInputString(input, "message")
	status.LimitedAvailability, _ = gqlInputBool(input, "limitedAvailability")
	if expires, ok := gqlInputString(input, "expiresAt"); ok && expires != "" {
		parsed, err := time.Parse(time.RFC3339, expires)
		if err != nil {
			return nil, fmt.Errorf("expiresAt is not an RFC 3339 timestamp")
		}
		utc := parsed.UTC()
		status.ExpiresAt = &utc
	}
	if orgNodeID, ok := gqlInputString(input, "organizationId"); ok && orgNodeID != "" {
		org := s.orgByNodeID(orgNodeID)
		if org == nil {
			return nil, gqlMissingNode("Organization", orgNodeID)
		}
		// A status scoped to an organization is shown to that organization's
		// members, so only a member may set one.
		if !s.viewerIsOrgMember(p.Context, org.Login) {
			return nil, &ghForbiddenError{message: "You must be a member of the organization to scope a status to it."}
		}
		status.OrganizationID = org.ID
	}

	stored := s.store.SetUserStatus(viewer.ID, status)
	return map[string]interface{}{"status": optionalObject(s.userStatusToGQL(stored))}, nil
}

func (s *Resolver) resolveCreateUserList(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	viewer := s.ghUserFromContext(p.Context)
	name, _ := gqlInputString(input, "name")
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("Name can't be blank")
	}
	description, _ := gqlInputString(input, "description")
	private, _ := gqlInputBool(input, "isPrivate")

	list := s.store.CreateUserList(viewer.ID, name, description, private)
	if list == nil {
		return nil, fmt.Errorf("Name has already been taken")
	}
	return map[string]interface{}{
		"list":   optionalObject(s.userListToGQL(list)),
		"viewer": optionalRendered(s.store.GetUserByID(viewer.ID), userToGraphQL),
	}, nil
}

func (s *Resolver) resolveUpdateUserList(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "listId")
	found := store.FindUserListByNodeID(s.store, nodeID)
	if found == nil {
		return nil, gqlMissingNode("UserList", nodeID)
	}
	name, renaming := gqlInputString(input, "name")
	description, redescribing := gqlInputString(input, "description")
	private, reprivatizing := gqlInputBool(input, "isPrivate")

	updated := s.store.UpdateUserList(found.ID, func(l *store.UserList) {
		if renaming {
			l.Name = name
		}
		if redescribing {
			l.Description = description
		}
		if reprivatizing {
			l.IsPrivate = private
		}
	})
	if updated == nil {
		return nil, fmt.Errorf("Name has already been taken")
	}
	return map[string]interface{}{"list": optionalObject(s.userListToGQL(updated))}, nil
}

func (s *Resolver) resolveDeleteUserList(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "listId")
	found := store.FindUserListByNodeID(s.store, nodeID)
	if found == nil {
		return nil, gqlMissingNode("UserList", nodeID)
	}
	ownerID := found.UserID
	if !s.store.DeleteUserList(found.ID) {
		return nil, gqlMissingNodeType("UserList")
	}
	return map[string]interface{}{
		"user": optionalRendered(s.store.GetUserByID(ownerID), userToGraphQL),
	}, nil
}

func (s *Resolver) resolveUpdateUserListsForItem(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	viewer := s.ghUserFromContext(p.Context)
	itemID, _ := gqlInputString(input, "itemId")
	repo := store.FindRepoByNodeID(s.store, itemID)
	if repo == nil || !s.viewerCanReadRepo(p.Context, repo) {
		return nil, gqlMissingNode("Repository", itemID)
	}
	wanted, _ := gqlInputStrings(input, "listIds")
	listIDs := make([]int, 0, len(wanted))
	for _, nodeID := range wanted {
		list := store.FindUserListByNodeID(s.store, nodeID)
		if list == nil || list.UserID != viewer.ID {
			return nil, gqlMissingNode("UserList", nodeID)
		}
		listIDs = append(listIDs, list.ID)
	}

	lists := s.store.SetUserListsForRepo(viewer.ID, repo.ID, listIDs)
	rendered := make([]interface{}, 0, len(lists))
	for _, list := range lists {
		rendered = append(rendered, s.userListToGQL(list))
	}
	return map[string]interface{}{
		"item":  optionalObject(repoToGraphQL(s.store, repo)),
		"lists": rendered,
		"user":  optionalRendered(s.store.GetUserByID(viewer.ID), userToGraphQL),
	}, nil
}

func (s *Resolver) resolveUpdateNotificationRestrictionSetting(p graphql.ResolveParams) (interface{}, error) {
	input, _ := p.Args["input"].(map[string]interface{})
	nodeID, _ := gqlInputString(input, "ownerId")
	setting, _ := gqlInputString(input, "settingValue")
	enabled := setting == "ENABLED"

	if enterprise := store.FindEnterpriseByNodeID(s.store, nodeID); enterprise != nil {
		policyValue := store.EnterprisePolicyDisabled
		if enabled {
			policyValue = store.EnterprisePolicyEnabled
		}
		updated := s.store.UpdateEnterprisePolicy(enterprise.ID, func(policy *store.EnterprisePolicy) {
			policy.NotificationDeliveryRestrictionEnabled = policyValue
		})
		if updated == nil {
			return nil, gqlMissingNodeType("Enterprise")
		}
		return map[string]interface{}{"owner": optionalObject(enterpriseToGraphQL(updated))}, nil
	}

	org := s.orgByNodeID(nodeID)
	if org == nil {
		return nil, gqlMissingNode("VerifiableDomainOwner", nodeID)
	}
	if !s.store.UpdateOrg(org.Login, func(o *store.Org) {
		o.NotificationDeliveryRestrictionEnabled = enabled
	}) {
		return nil, gqlMissingNodeType("Organization")
	}
	updated := s.store.GetOrg(org.Login)
	if updated == nil {
		return nil, gqlMissingNodeType("Organization")
	}
	return map[string]interface{}{"owner": optionalObject(orgToGraphQL(updated))}, nil
}

package graphqlapi

// The remaining Team members beyond the roster/repository core: pending
// invitations, member statuses, visible ProjectsV2, review-request delegation,
// and the deprecated subscription pair.

import (
	"sort"
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// addTeamExtraFields completes the Team type.
func (s *Resolver) addTeamExtraFields(types *accountSurfaceTypes) {
	teamType := s.graphqlTypes.team

	s.addTeamInvitationsField(teamType, types)
	s.addTeamMemberStatusesField(teamType, types)
	s.addTeamProjectsV2Fields(teamType)
	s.addTeamReviewDelegationFields(teamType)
	s.addTeamSubscriptionFields(teamType)
}

// addTeamInvitationsField wires Team.invitations: the org's pending invitations
// naming this team. The connection reuses the enterprise family's
// OrganizationInvitation node so it is not declared twice.
func (s *Resolver) addTeamInvitationsField(teamType *graphql.Object, types *accountSurfaceTypes) {
	invitationNode := s.reachOrganizationInvitationType()
	// Guard against the enterprise family changing shape: a nil node still builds
	// (the field then resolves empty).
	var connection *graphql.Object
	if invitationNode != nil {
		connection = s.accountConnectionType(types, "OrganizationInvitation", invitationNode, false, nil)
	} else {
		connection = s.accountConnectionType(types, "OrganizationInvitation",
			graphql.NewObject(graphql.ObjectConfig{
				Name:   "OrganizationInvitation",
				Fields: graphql.Fields{"id": &graphql.Field{Type: graphql.NewNonNull(graphql.ID)}},
			}), false, nil)
	}

	teamType.AddFieldConfig("invitations", &graphql.Field{
		Type: connection,
		Args: connectionArgs(nil),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			team, org, err := s.teamFromSource(p.Source)
			if err != nil || team == nil {
				return paginateGQLItems(nil, p.Args), err
			}
			// Membership-administration data: an org outsider must not enumerate it.
			if !s.viewerIsOrgMember(p.Context, org.Login) {
				return paginateGQLItems(nil, p.Args), nil
			}
			invitations := s.store.ListPendingOrgInvitationsForTeam(org.Login, team.ID)
			sort.Slice(invitations, func(i, j int) bool {
				return invitations[i].CreatedAt.Before(invitations[j].CreatedAt)
			})
			items := make([]gqlConnItem, 0, len(invitations))
			for i := range invitations {
				inv := invitations[i]
				items = append(items, gqlConnItem{
					identity: inv.NodeID,
					render:   func() map[string]interface{} { return s.orgInvitationToGQL(inv, org) },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})
}

// reachOrganizationInvitationType reaches the enterprise family's
// OrganizationInvitation object without re-declaring it, via
// Enterprise.ownerInfo -> failedInvitations -> nodes (built by now).
func (s *Resolver) reachOrganizationInvitationType() *graphql.Object {
	enterprise := s.graphqlTypes.enterprise
	if enterprise == nil {
		return nil
	}
	ownerInfo := namedObjectFromField(enterprise, "ownerInfo")
	failedInvitations := namedObjectFromField(ownerInfo, "failedInvitations")
	return namedObjectFromField(failedInvitations, "nodes")
}

// orgInvitationToGQL renders a pending invitation as the OrganizationInvitation
// source map the enterprise node's resolvers read by key.
func (s *Resolver) orgInvitationToGQL(inv *store.OrgInvitation, org *store.Org) map[string]interface{} {
	invitationType := "USER"
	if inv.UserID == 0 {
		invitationType = "EMAIL"
	}
	source := "MEMBER"
	switch strings.ToLower(inv.Source) {
	case "member":
		source = "MEMBER"
	case "scim":
		source = "SCIM"
	default:
		source = "UNKNOWN"
	}
	role := "DIRECT_MEMBER"
	switch strings.ToLower(inv.Role) {
	case "admin":
		role = "ADMIN"
	case "billing_manager":
		role = "BILLING_MANAGER"
	case "reinstate":
		role = "REINSTATE"
	case "direct_member", "":
		role = "DIRECT_MEMBER"
	}
	var invitee interface{}
	if inv.UserID != 0 {
		invitee = optionalRendered(s.store.GetUserByID(inv.UserID), userToGraphQL)
	}
	var inviter interface{}
	if inviterUser := s.store.GetUserByID(inv.InviterID); inviterUser != nil {
		inviter = userToGraphQL(inviterUser)
	}
	return map[string]interface{}{
		"id":               inv.NodeID,
		"email":            nilStr(inv.Email),
		"createdAt":        inv.CreatedAt.UTC().Format(rfc3339),
		"invitationSource": source,
		"invitationType":   invitationType,
		"invitee":          invitee,
		"inviter":          inviter,
		"inviterActor":     inviter,
		"organization":     orgToGraphQL(org),
		"role":             role,
	}
}

// addTeamMemberStatusesField wires Team.memberStatuses, reusing the org
// surface's UserStatusConnection and UserStatus types.
func (s *Resolver) addTeamMemberStatusesField(teamType *graphql.Object, types *accountSurfaceTypes) {
	connection := s.accountConnectionType(types, "UserStatus", s.gqlUserStatusType(), false, nil)
	teamType.AddFieldConfig("memberStatuses", &graphql.Field{
		Type: graphql.NewNonNull(connection),
		Args: connectionArgs(graphql.FieldConfigArgument{
			"orderBy": &graphql.ArgumentConfig{
				Type:         s.gqlOrderInput(types, "UserStatusOrder", "UserStatusOrderField", "UPDATED_AT"),
				DefaultValue: map[string]interface{}{"field": "UPDATED_AT", "direction": "DESC"},
			},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			team, org, err := s.teamFromSource(p.Source)
			if err != nil || team == nil {
				return paginateGQLItems(nil, p.Args), err
			}
			// A secret team's roster is visible only to org owners and its own
			// members, not to every org member.
			if !s.viewerCanSeeTeam(p.Context, org, team) {
				return paginateGQLItems(nil, p.Args), nil
			}
			members := s.store.ListTeamMembers(org.Login, team.Slug)
			statuses := make([]*store.UserStatus, 0, len(members))
			for i := range members {
				if status := s.store.GetUserStatus(members[i].ID); status != nil {
					statuses = append(statuses, status)
				}
			}
			direction := "DESC"
			if orderBy, ok := p.Args["orderBy"].(map[string]interface{}); ok {
				if d, ok := orderBy["direction"].(string); ok && d != "" {
					direction = d
				}
			}
			sort.SliceStable(statuses, func(i, j int) bool {
				if direction == "ASC" {
					return statuses[i].UpdatedAt.Before(statuses[j].UpdatedAt)
				}
				return statuses[i].UpdatedAt.After(statuses[j].UpdatedAt)
			})
			items := make([]gqlConnItem, 0, len(statuses))
			for i := range statuses {
				status := statuses[i]
				rendered := s.userStatusToGQL(status)
				items = append(items, gqlConnItem{
					identity: store.UserStatusNodeID(status.UserID),
					render:   func() map[string]interface{} { return rendered },
				})
			}
			return paginateGQLItems(items, p.Args), nil
		},
	})
}

// addTeamProjectsV2Fields wires Team.projectV2 / Team.projectsV2 through the
// team's organization (bleephub models no team-to-project association), as
// Repository.projectsV2 resolves through the repository's owner.
func (s *Resolver) addTeamProjectsV2Fields(teamType *graphql.Object) {
	projectType := s.projectV2GraphQLTypes()
	connection := s.gqlConnectionType("ProjectV2", projectType)

	teamType.AddFieldConfig("projectsV2", &graphql.Field{
		Type: graphql.NewNonNull(connection),
		Args: s.projectV2ConnectionArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			_, org, err := s.teamFromSource(p.Source)
			if err != nil || org == nil {
				return s.projectV2Connection(p, 0, "Organization"), err
			}
			return s.projectV2Connection(p, org.ID, "Organization"), nil
		},
	})
	teamType.AddFieldConfig("projectV2", &graphql.Field{
		Type: projectType,
		Args: graphql.FieldConfigArgument{
			"number": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			_, org, err := s.teamFromSource(p.Source)
			if err != nil || org == nil {
				return nil, err
			}
			number, _ := p.Args["number"].(int)
			return optionalObject(s.projectV2ByNumber(p, org.ID, "Organization", number)), nil
		},
	})
}

// addTeamReviewDelegationFields wires the review-request delegation members off
// the team's TeamReviewAssignment; a nil assignment is "not enabled" (false / null).
func (s *Resolver) addTeamReviewDelegationFields(teamType *graphql.Object) {
	assignment := func(p graphql.ResolveParams) *store.TeamReviewAssignment {
		team, _, err := s.teamFromSource(p.Source)
		if err != nil || team == nil {
			return nil
		}
		return team.ReviewAssignment
	}

	teamType.AddFieldConfig("reviewRequestDelegationEnabled", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			a := assignment(p)
			return a != nil && a.Enabled, nil
		},
	})
	teamType.AddFieldConfig("reviewRequestDelegationNotifyTeam", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			a := assignment(p)
			return a != nil && a.NotifyTeam, nil
		},
	})
	teamType.AddFieldConfig("reviewRequestDelegationMemberCount", &graphql.Field{
		Type: graphql.Int,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			if a := assignment(p); a != nil {
				return a.TeamMemberCount, nil
			}
			return nil, nil
		},
	})
	teamType.AddFieldConfig("reviewRequestDelegationAlgorithm", &graphql.Field{
		Type: s.sharedEnum("TeamReviewAssignmentAlgorithm", "ROUND_ROBIN", "LOAD_BALANCE"),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			a := assignment(p)
			if a == nil || a.Algorithm == "" {
				return nil, nil
			}
			return a.Algorithm, nil
		},
	})
}

// addTeamSubscriptionFields wires the deprecated Subscribable pair. No per-team
// subscription is modeled: a present viewer may subscribe and reads the default
// UNSUBSCRIBED; an anonymous viewer gets false / null.
func (s *Resolver) addTeamSubscriptionFields(teamType *graphql.Object) {
	subState := s.sharedEnum("SubscriptionState", "IGNORED", "SUBSCRIBED", "UNSUBSCRIBED")
	teamType.AddFieldConfig("viewerCanSubscribe", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return s.ghUserFromContext(p.Context) != nil, nil
		},
	})
	teamType.AddFieldConfig("viewerSubscription", &graphql.Field{
		Type: subState,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			if s.ghUserFromContext(p.Context) == nil {
				return nil, nil
			}
			return "UNSUBSCRIBED", nil
		},
	})
}

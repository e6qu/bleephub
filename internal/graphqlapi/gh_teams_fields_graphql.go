package graphqlapi

// The remaining Team members GitHub declares beyond the roster/repository
// core: the pending-invitation and member-status connections, the team's
// visible ProjectsV2, the review-request delegation settings, and the
// (deprecated) subscription pair. They live here rather than in
// gh_teams_graphql.go to keep that file's shape stable; addTeamFields wires
// this installer with a single call.

import (
	"sort"
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// addTeamExtraFields completes the Team type, driving it to GitHub parity.
func (s *Resolver) addTeamExtraFields(types *accountSurfaceTypes) {
	teamType := s.graphqlTypes.team

	s.addTeamInvitationsField(teamType, types)
	s.addTeamMemberStatusesField(teamType, types)
	s.addTeamProjectsV2Fields(teamType)
	s.addTeamReviewDelegationFields(teamType)
	s.addTeamSubscriptionFields(teamType)
}

// addTeamInvitationsField wires Team.invitations: the organization's pending
// invitations that name this team. GitHub returns these as an
// OrganizationInvitationConnection whose node is the shared
// OrganizationInvitation object the enterprise family already minted; the
// connection type itself is new here, built over that reused node so the two
// families do not declare the node twice.
func (s *Resolver) addTeamInvitationsField(teamType *graphql.Object, types *accountSurfaceTypes) {
	invitationNode := s.reachOrganizationInvitationType()
	// A missing node type would mean the enterprise family changed shape; guard
	// so the schema still builds (the field then resolves truthfully empty).
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
			// Pending invitations are membership-administration data; an
			// outsider to the organization must not enumerate them.
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

// reachOrganizationInvitationType reaches the OrganizationInvitation object the
// enterprise family builds, without re-declaring it. The enterprise surface is
// installed before the account surface, so the path
// Enterprise.ownerInfo -> failedInvitations -> nodes is populated by now.
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
// source map the enterprise node's default resolvers read by key.
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

// addTeamMemberStatusesField wires Team.memberStatuses: the status messages the
// team's members have set. It reuses the org surface's UserStatusConnection and
// UserStatus types so both name the same connection.
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
			// A secret team's roster (and thus whose statuses these are) is
			// visible only within the organization.
			if team.Privacy == store.TeamPrivacySecret && !s.viewerIsOrgMember(p.Context, org.Login) {
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

// addTeamProjectsV2Fields wires Team.projectV2 / Team.projectsV2. A team's
// Projects tab on github.com lists its owning organization's projects (bleephub
// models no team-to-project collaborator association of its own), so both
// resolve through the team's organization exactly as Repository.projectsV2
// resolves through the repository's owner.
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

// addTeamReviewDelegationFields wires the four review-request delegation
// members off the team's stored TeamReviewAssignment. An unconfigured team
// (nil assignment) is GitHub's "not enabled": false / null.
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

// addTeamSubscriptionFields wires the (deprecated) Subscribable pair. bleephub
// models no per-team notification subscription, so a present viewer may
// subscribe (viewerCanSubscribe) and reads the default UNSUBSCRIBED state; an
// anonymous viewer gets false / null.
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

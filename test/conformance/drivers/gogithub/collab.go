package main

import (
	"fmt"
	"net/http"

	github "github.com/google/go-github/v88/github"
)

// runCollaborators drives the whole invitation lifecycle, which needs a second
// account: an invitation nobody can accept proves very little. The second
// account is provisioned through GitHub Enterprise Server's site-admin API,
// the only route through which a client can make one.
func runCollaborators(client *github.Client, rec *recorder, set *fixtureSet, guest *principal) {
	const domain = "collaborators"
	sc := newScratch(client, set.owner, "conformance-collaborators")
	if !sc.ok() {
		skipAll(rec, domain, "POST /user/repos", "the collaborator repository fixture could not be provisioned",
			"repos.addCollaborator", "repos.listInvitations", "repos.updateInvitation",
			"user.acceptRepositoryInvitation", "repos.isCollaborator", "repos.getCollaboratorPermissionLevel",
			"repos.listCollaborators (affiliation)", "repos.removeCollaborator")
		return
	}
	if !guest.ok() {
		skipAll(rec, domain, "POST /repos/{owner}/{repo}/collaborators/{username}",
			"a second account could not be provisioned through the site-admin API",
			"repos.addCollaborator", "repos.listInvitations", "repos.updateInvitation",
			"user.acceptRepositoryInvitation", "repos.isCollaborator", "repos.getCollaboratorPermissionLevel",
			"repos.listCollaborators (affiliation)", "repos.removeCollaborator")
		return
	}

	var invitationID int64
	rec.check(domain, "repos.addCollaborator", "PUT /repos/{owner}/{repo}/collaborators/{username}", func() error {
		invitation, resp, err := client.Repositories.AddCollaborator(ctx, sc.owner, sc.repo, guest.login,
			&github.RepositoryAddCollaboratorOptions{Permission: "push"})
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusCreated, "adding a collaborator"); err != nil {
			return err
		}
		if invitation == nil {
			return deviate("an invitation object", "nothing",
				"adding a collaborator returns no invitation, so a client cannot report what it created")
		}
		invitationID = invitation.GetID()
		if invitationID == 0 {
			return deviate("a non-zero invitation id", "0", "the invitation has no id")
		}
		if invitation.GetInvitee().GetLogin() != guest.login {
			return deviate(guest.login, invitation.GetInvitee().GetLogin(), "the invitation names the wrong invitee")
		}
		if invitation.GetInviter().GetLogin() == "" {
			return deviate("inviter.login populated", "empty", "the invitation has no inviter")
		}
		if invitation.GetRepo().GetFullName() == "" {
			return deviate("repository populated", "empty", "the invitation does not say which repository it is for")
		}
		return nil
	})

	rec.check(domain, "repos.listInvitations", "GET /repos/{owner}/{repo}/invitations", func() error {
		invitations, _, err := client.Repositories.ListInvitations(ctx, sc.owner, sc.repo, nil)
		if err != nil {
			return err
		}
		if len(invitations) != 1 {
			return deviate("exactly the one pending invitation", fmt.Sprintf("%d", len(invitations)),
				"the repository invitation listing does not show the pending invitation")
		}
		if invitations[0].GetPermissions() == "" {
			return deviate("permissions populated", "empty",
				"a pending invitation does not say what access it grants")
		}
		return nil
	})

	rec.check(domain, "user.listRepositoryInvitations", "GET /user/repository_invitations", func() error {
		invitations, _, err := guest.client.Users.ListInvitations(ctx, nil)
		if err != nil {
			return err
		}
		if len(invitations) != 1 {
			return deviate("the one invitation addressed to this account", fmt.Sprintf("%d", len(invitations)),
				"the invitee cannot see the invitation addressed to them")
		}
		return nil
	})

	if invitationID != 0 {
		rec.check(domain, "repos.updateInvitation", "PATCH /repos/{owner}/{repo}/invitations/{invitation_id}", func() error {
			invitation, _, err := client.Repositories.UpdateInvitation(ctx, sc.owner, sc.repo, invitationID, "admin")
			if err != nil {
				return err
			}
			if invitation.GetPermissions() != "admin" {
				return deviate("admin", invitation.GetPermissions(),
					"changing an invitation's permission did not take effect")
			}
			return nil
		})

		rec.check(domain, "user.acceptRepositoryInvitation", "PATCH /user/repository_invitations/{invitation_id}", func() error {
			resp, err := guest.client.Users.AcceptInvitation(ctx, invitationID)
			if err != nil {
				return err
			}
			return wantStatus(resp, http.StatusNoContent, "accepting an invitation")
		})
	}

	rec.check(domain, "repos.isCollaborator", "GET /repos/{owner}/{repo}/collaborators/{username}", func() error {
		isCollaborator, _, err := client.Repositories.IsCollaborator(ctx, sc.owner, sc.repo, guest.login)
		if err != nil {
			return err
		}
		if !isCollaborator {
			return deviate("204 (is a collaborator)", "404",
				"the accepted invitation did not make the invitee a collaborator")
		}
		return nil
	})

	rec.check(domain, "repos.getCollaboratorPermissionLevel",
		"GET /repos/{owner}/{repo}/collaborators/{username}/permission", func() error {
			level, _, err := client.Repositories.GetPermissionLevel(ctx, sc.owner, sc.repo, guest.login)
			if err != nil {
				return err
			}
			if level.GetPermission() != "admin" {
				return deviate("admin", level.GetPermission(),
					"the permission level does not reflect the accepted invitation's grant")
			}
			if level.GetUser().GetLogin() != guest.login {
				return deviate(guest.login, level.GetUser().GetLogin(),
					"the permission response names the wrong user")
			}
			return nil
		})

	rec.check(domain, "repos.listCollaborators (affiliation)",
		"GET /repos/{owner}/{repo}/collaborators?affiliation=direct", func() error {
			collaborators, _, err := client.Repositories.ListCollaborators(ctx, sc.owner, sc.repo,
				&github.ListCollaboratorsOptions{Affiliation: "direct"})
			if err != nil {
				return err
			}
			found := false
			for _, collaborator := range collaborators {
				if collaborator.GetLogin() == guest.login {
					found = true
					if collaborator.GetPermissions() == nil {
						return deviate("a permissions map on each collaborator", "absent",
							"a listed collaborator carries no permissions map, which every client renders")
					}
					if collaborator.GetRoleName() == "" {
						return deviate("role_name populated", "empty",
							"a listed collaborator has no role_name")
					}
				}
			}
			if !found {
				return deviate("the direct collaborator in the listing", "absent",
					"the affiliation filter excludes a direct collaborator")
			}
			return nil
		})

	rec.check(domain, "repos.removeCollaborator", "DELETE /repos/{owner}/{repo}/collaborators/{username}", func() error {
		resp, err := client.Repositories.RemoveCollaborator(ctx, sc.owner, sc.repo, guest.login)
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusNoContent, "removing a collaborator"); err != nil {
			return err
		}
		isCollaborator, _, err := client.Repositories.IsCollaborator(ctx, sc.owner, sc.repo, guest.login)
		if err != nil {
			return err
		}
		if isCollaborator {
			return deviate("no longer a collaborator", "still a collaborator", "the removal did not take effect")
		}
		return nil
	})
}

// runTeams covers the team surface: creation, membership, repository grants,
// nesting and discussions.
func runTeams(client *github.Client, rec *recorder, set *fixtureSet, guest *principal) {
	const domain = "teams"
	if set.org == "" {
		skipAll(rec, domain, "POST /orgs/{org}/teams", "the organization fixture is unavailable",
			"teams.createTeam", "teams.getTeamBySlug", "teams.editTeam", "teams.addTeamMembership",
			"teams.getTeamMembership", "teams.listTeamMembers", "teams.addTeamRepo", "teams.listTeamRepos",
			"teams.deleteTeam")
		return
	}

	const slug = "conformance-squad"
	rec.check(domain, "teams.createTeam", "POST /orgs/{org}/teams", func() error {
		team, _, err := client.Teams.CreateTeam(ctx, set.org, github.NewTeam{
			Name:        "Conformance Squad",
			Description: github.Ptr("created by the conformance harness"),
			Privacy:     github.Ptr("closed"),
		})
		if err != nil {
			return err
		}
		if team.GetSlug() == "" {
			return deviate("a slug", "empty", "the created team has no slug, which every later call keys on")
		}
		if team.GetPrivacy() != "closed" {
			return deviate("closed", team.GetPrivacy(), "team privacy does not round trip")
		}
		if team.GetNodeID() == "" {
			return deviate("node_id populated", "empty", "a team carries no node_id")
		}
		return nil
	})

	rec.check(domain, "teams.getTeamBySlug", "GET /orgs/{org}/teams/{team_slug}", func() error {
		team, _, err := client.Teams.GetTeamBySlug(ctx, set.org, slug)
		if err != nil {
			return err
		}
		if team.GetName() != "Conformance Squad" {
			return deviate("Conformance Squad", team.GetName(), "the team name is wrong")
		}
		if team.GetMembersURL() == "" || team.GetRepositoriesURL() == "" {
			return deviate("members_url and repositories_url populated", "empty",
				"a team omits the hypermedia clients follow to its members and repositories")
		}
		return nil
	})

	rec.check(domain, "teams.editTeam", "PATCH /orgs/{org}/teams/{team_slug}", func() error {
		team, _, err := client.Teams.EditTeamBySlug(ctx, set.org, slug, github.NewTeam{
			Name:        "Conformance Squad",
			Description: github.Ptr("edited by the conformance harness"),
		}, false)
		if err != nil {
			return err
		}
		if team.GetDescription() != "edited by the conformance harness" {
			return deviate("the new description", team.GetDescription(), "the team edit did not persist")
		}
		return nil
	})

	rec.check(domain, "teams.listTeams", "GET /orgs/{org}/teams", func() error {
		teams, _, err := client.Teams.ListTeams(ctx, set.org, nil)
		if err != nil {
			return err
		}
		for _, team := range teams {
			if team.GetSlug() == slug {
				return nil
			}
		}
		return deviate("the created team in the listing", "absent", "the team listing omits the created team")
	})

	if guest.ok() {
		rec.check(domain, "teams.addTeamMembership", "PUT /orgs/{org}/teams/{team_slug}/memberships/{username}", func() error {
			membership, _, err := client.Teams.AddTeamMembershipBySlug(ctx, set.org, slug, guest.login,
				&github.TeamAddTeamMembershipOptions{Role: "maintainer"})
			if err != nil {
				return err
			}
			if membership.GetRole() != "maintainer" {
				return deviate("maintainer", membership.GetRole(), "the team role does not round trip")
			}
			if membership.GetState() == "" {
				return deviate("state populated", "empty",
					"a team membership has no state, so a client cannot tell active from pending")
			}
			return nil
		})

		rec.check(domain, "teams.getTeamMembership", "GET /orgs/{org}/teams/{team_slug}/memberships/{username}", func() error {
			membership, _, err := client.Teams.GetTeamMembershipBySlug(ctx, set.org, slug, guest.login)
			if err != nil {
				return err
			}
			if membership.GetRole() != "maintainer" {
				return deviate("maintainer", membership.GetRole(), "the stored team role is wrong")
			}
			return nil
		})

		rec.check(domain, "teams.listTeamMembers (role filter)",
			"GET /orgs/{org}/teams/{team_slug}/members?role=maintainer", func() error {
				members, _, err := client.Teams.ListTeamMembersBySlug(ctx, set.org, slug,
					&github.TeamListTeamMembersOptions{Role: "maintainer"})
				if err != nil {
					return err
				}
				for _, member := range members {
					if member.GetLogin() == guest.login {
						return nil
					}
				}
				return deviate("the maintainer in the listing", "absent",
					"the role filter excludes a maintainer that was just added")
			})

		rec.check(domain, "teams.removeTeamMembership", "DELETE /orgs/{org}/teams/{team_slug}/memberships/{username}", func() error {
			resp, err := client.Teams.RemoveTeamMembershipBySlug(ctx, set.org, slug, guest.login)
			if err != nil {
				return err
			}
			if err := wantStatus(resp, http.StatusNoContent, "removing a team membership"); err != nil {
				return err
			}
			_, _, err = client.Teams.GetTeamMembershipBySlug(ctx, set.org, slug, guest.login)
			return wantHTTPError(err, http.StatusNotFound, "reading a removed team membership")
		})
	} else {
		skipAll(rec, domain, "PUT /orgs/{org}/teams/{team_slug}/memberships/{username}",
			"a second account could not be provisioned",
			"teams.addTeamMembership", "teams.getTeamMembership",
			"teams.listTeamMembers (role filter)", "teams.removeTeamMembership")
	}

	if set.orgRepo != "" {
		rec.check(domain, "teams.addTeamRepo", "PUT /orgs/{org}/teams/{team_slug}/repos/{owner}/{repo}", func() error {
			resp, err := client.Teams.AddTeamRepoBySlug(ctx, set.org, slug, set.org, set.orgRepo,
				&github.TeamAddTeamRepoOptions{Permission: "push"})
			if err != nil {
				return err
			}
			return wantStatus(resp, http.StatusNoContent, "granting a team access to a repository")
		})

		rec.check(domain, "teams.isTeamRepo", "GET /orgs/{org}/teams/{team_slug}/repos/{owner}/{repo}", func() error {
			repository, _, err := client.Teams.IsTeamRepoBySlug(ctx, set.org, slug, set.org, set.orgRepo)
			if err != nil {
				return err
			}
			if repository.GetPermissions() == nil {
				return deviate("a permissions map", "absent",
					"the team-repository check omits the permissions map that says what the grant is")
			}
			return nil
		})

		rec.check(domain, "teams.listTeamRepos", "GET /orgs/{org}/teams/{team_slug}/repos", func() error {
			repositories, _, err := client.Teams.ListTeamReposBySlug(ctx, set.org, slug, nil)
			if err != nil {
				return err
			}
			if len(repositories) != 1 {
				return deviate("the one repository granted to the team", fmt.Sprintf("%d", len(repositories)),
					"the team's repository listing is wrong")
			}
			return nil
		})

		rec.check(domain, "teams.removeTeamRepo", "DELETE /orgs/{org}/teams/{team_slug}/repos/{owner}/{repo}", func() error {
			resp, err := client.Teams.RemoveTeamRepoBySlug(ctx, set.org, slug, set.org, set.orgRepo)
			if err != nil {
				return err
			}
			return wantStatus(resp, http.StatusNoContent, "revoking a team's repository access")
		})
	} else {
		skipAll(rec, domain, "PUT /orgs/{org}/teams/{team_slug}/repos/{owner}/{repo}",
			"no organization repository fixture exists",
			"teams.addTeamRepo", "teams.isTeamRepo", "teams.listTeamRepos", "teams.removeTeamRepo")
	}

	rec.check(domain, "teams.createChildTeam", "POST /orgs/{org}/teams with parent_team_id", func() error {
		parent, _, err := client.Teams.GetTeamBySlug(ctx, set.org, slug)
		if err != nil {
			return err
		}
		child, _, err := client.Teams.CreateTeam(ctx, set.org, github.NewTeam{
			Name:         "Conformance Cadets",
			ParentTeamID: github.Ptr(parent.GetID()),
		})
		if err != nil {
			return err
		}
		if child.GetParent() == nil || child.GetParent().GetID() != parent.GetID() {
			return deviate("parent set to the parent team", "absent",
				"a nested team does not report its parent, so a client cannot render the hierarchy")
		}
		children, _, err := client.Teams.ListChildTeamsByParentSlug(ctx, set.org, slug, nil)
		if err != nil {
			return err
		}
		if len(children) != 1 {
			return deviate("one child team", fmt.Sprintf("%d", len(children)),
				"the child-team listing does not show the nested team")
		}
		return nil
	})

	rec.check(domain, "teams.deleteTeam", "DELETE /orgs/{org}/teams/{team_slug}", func() error {
		resp, err := client.Teams.DeleteTeamBySlug(ctx, set.org, "conformance-cadets")
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusNoContent, "deleting a team"); err != nil {
			return err
		}
		_, _, err = client.Teams.GetTeamBySlug(ctx, set.org, "conformance-cadets")
		return wantHTTPError(err, http.StatusNotFound, "reading a deleted team")
	})
}

// runOrgMembership covers organization membership: invitations, roles,
// publicity and the outside-collaborator distinction.
func runOrgMembership(client *github.Client, rec *recorder, set *fixtureSet, guest *principal) {
	const domain = "orgs"
	if set.org == "" || !guest.ok() {
		skipAll(rec, domain, "PUT /orgs/{org}/memberships/{username}",
			"the organization fixture or the second account is unavailable",
			"orgs.editOrgMembership", "orgs.getOrgMembership", "orgs.isMember",
			"orgs.listMembers (role filter)", "orgs.editOrgMembershipForAuthenticatedUser",
			"orgs.publicizeMembership / concealMembership",
			"orgs.createOrgInvitation", "orgs.listPendingOrgInvitations", "orgs.removeMember")
		return
	}

	rec.check(domain, "orgs.editOrgMembership", "PUT /orgs/{org}/memberships/{username}", func() error {
		membership, _, err := client.Organizations.EditOrgMembership(ctx, guest.login, set.org,
			&github.Membership{Role: github.Ptr("member")})
		if err != nil {
			return err
		}
		if membership.GetRole() != "member" {
			return deviate("member", membership.GetRole(), "the organization role does not round trip")
		}
		if membership.GetOrganization().GetLogin() != set.org {
			return deviate(set.org, membership.GetOrganization().GetLogin(),
				"the membership does not carry the organization it belongs to")
		}
		if membership.GetUser().GetLogin() != guest.login {
			return deviate(guest.login, membership.GetUser().GetLogin(), "the membership names the wrong user")
		}
		return nil
	})

	rec.check(domain, "orgs.getOrgMembership", "GET /orgs/{org}/memberships/{username}", func() error {
		membership, _, err := client.Organizations.GetOrgMembership(ctx, guest.login, set.org)
		if err != nil {
			return err
		}
		switch membership.GetState() {
		case "active", "pending":
			return nil
		}
		return deviate("active or pending", membership.GetState(),
			"the membership state is not one of the documented values")
	})

	rec.check(domain, "orgs.isMember", "GET /orgs/{org}/members/{username}", func() error {
		// A pending membership is deliberately NOT a member: GitHub answers 404
		// until the invitation is accepted. What is asserted is that the two
		// resources agree, because a client that reads one and acts on the
		// other is exactly what a disagreement would break.
		membership, _, err := client.Organizations.GetOrgMembership(ctx, guest.login, set.org)
		if err != nil {
			return err
		}
		isMember, _, err := client.Organizations.IsMember(ctx, set.org, guest.login)
		if err != nil {
			return err
		}
		if (membership.GetState() == "active") != isMember {
			return deviate(fmt.Sprintf("membership state %q and member check to agree", membership.GetState()),
				fmt.Sprintf("member check says %v", isMember),
				"the membership resource and the member check disagree")
		}
		return nil
	})

	rec.check(domain, "orgs.listMembers (role filter)", "GET /orgs/{org}/members?role=admin", func() error {
		admins, _, err := client.Organizations.ListMembers(ctx, set.org, &github.ListMembersOptions{Role: "admin"})
		if err != nil {
			return err
		}
		for _, member := range admins {
			if member.GetLogin() == guest.login {
				return deviate("the plain member excluded from role=admin", "present",
					"the role filter is ignored, so a client cannot list owners")
			}
		}
		return nil
	})

	rec.check(domain, "orgs.editOrgMembershipForAuthenticatedUser", "PATCH /user/memberships/orgs/{org}", func() error {
		// This is the only documented way an invitee accepts an organization
		// invitation through the API; without it every membership stays pending
		// and nothing downstream of acceptance can be exercised at all.
		membership, _, err := guest.client.Organizations.EditOrgMembership(ctx, "", set.org,
			&github.Membership{State: github.Ptr("active")})
		if err != nil {
			return err
		}
		if membership.GetState() != "active" {
			return deviate("active", membership.GetState(),
				"accepting an organization invitation did not activate the membership")
		}
		return nil
	})

	rec.check(domain, "orgs.publicizeMembership / concealMembership",
		"PUT and DELETE /orgs/{org}/public_members/{username}", func() error {
			membership, _, err := client.Organizations.GetOrgMembership(ctx, guest.login, set.org)
			if err != nil {
				return err
			}
			if membership.GetState() != "active" {
				return deviate("an active membership to publicise", membership.GetState(),
					"the membership never became active, so publicity cannot be exercised")
			}
			// GitHub refuses to let one account publicise another's membership,
			// so this is done with the guest's own credential.
			if _, err := guest.client.Organizations.PublicizeMembership(ctx, set.org, guest.login); err != nil {
				return err
			}
			isPublic, _, err := client.Organizations.IsPublicMember(ctx, set.org, guest.login)
			if err != nil {
				return err
			}
			if !isPublic {
				return deviate("a public member", "not public", "publicising a membership did not take effect")
			}
			if _, err := guest.client.Organizations.ConcealMembership(ctx, set.org, guest.login); err != nil {
				return err
			}
			isPublic, _, err = client.Organizations.IsPublicMember(ctx, set.org, guest.login)
			if err != nil {
				return err
			}
			if isPublic {
				return deviate("no longer a public member", "still public", "concealing a membership did not take effect")
			}
			return nil
		})

	rec.check(domain, "orgs.listPendingOrgInvitations", "GET /orgs/{org}/invitations", func() error {
		invitations, _, err := client.Organizations.ListPendingOrgInvitations(ctx, set.org, nil)
		if err != nil {
			return err
		}
		if invitations == nil {
			return deviate("an invitation array", "nil", "the pending-invitation listing did not decode")
		}
		return nil
	})

	rec.check(domain, "orgs.listFailedOrgInvitations", "GET /orgs/{org}/failed_invitations", func() error {
		invitations, _, err := client.Organizations.ListFailedOrgInvitations(ctx, set.org, nil)
		if err != nil {
			return err
		}
		if invitations == nil {
			return deviate("an invitation array", "nil", "the failed-invitation listing did not decode")
		}
		return nil
	})

	rec.check(domain, "orgs.listOutsideCollaborators", "GET /orgs/{org}/outside_collaborators", func() error {
		collaborators, _, err := client.Organizations.ListOutsideCollaborators(ctx, set.org, nil)
		if err != nil {
			return err
		}
		if collaborators == nil {
			return deviate("a user array", "nil", "the outside-collaborator listing did not decode")
		}
		return nil
	})

	rec.check(domain, "orgs.listOrgMemberships", "GET /user/memberships/orgs", func() error {
		memberships, _, err := guest.client.Organizations.ListOrgMemberships(ctx, nil)
		if err != nil {
			return err
		}
		for _, membership := range memberships {
			if membership.GetOrganization().GetLogin() == set.org {
				return nil
			}
		}
		return deviate("the organization the account was just added to", "absent",
			"an account cannot see its own organization membership")
	})

	rec.check(domain, "orgs.removeMember", "DELETE /orgs/{org}/members/{username}", func() error {
		resp, err := client.Organizations.RemoveMember(ctx, set.org, guest.login)
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusNoContent, "removing an organization member"); err != nil {
			return err
		}
		isMember, _, err := client.Organizations.IsMember(ctx, set.org, guest.login)
		if err != nil {
			return err
		}
		if isMember {
			return deviate("no longer a member", "still a member", "the removal did not take effect")
		}
		return nil
	})
}

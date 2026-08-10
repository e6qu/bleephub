package bleephub

import (
	"testing"
	"time"
)

// TestIssueGetsAreDetached pins STORE-021 for the issue getters: GetIssue and
// GetIssueByNumber must deep-copy AssigneeIDs/LabelIDs and ClosedAt.
func TestIssueGetsAreDetached(t *testing.T) {
	s := newTestServer()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "issue-detach", "", false)
	iss := s.store.CreateIssue(repo.ID, admin.ID, "bug", "body", []int{1}, []int{admin.ID}, 0)

	got := s.store.GetIssue(iss.ID)
	got.Title = "hacked"
	got.LabelIDs = append(got.LabelIDs, 99999)
	got.AssigneeIDs[0] = 99999

	fresh := s.store.GetIssueByNumber(repo.ID, iss.Number)
	if fresh.Title == "hacked" {
		t.Fatalf("issue title mutated through the getter: %q", fresh.Title)
	}
	if len(fresh.LabelIDs) != 1 || fresh.LabelIDs[0] != 1 {
		t.Fatalf("issue LabelIDs mutated through the getter: %v", fresh.LabelIDs)
	}
	if fresh.AssigneeIDs[0] != admin.ID {
		t.Fatalf("issue AssigneeIDs mutated through the getter: %v", fresh.AssigneeIDs)
	}
}

// TestPullRequestReviewGetIsDetached pins STORE-021 for the PR-review getter.
func TestPullRequestReviewGetIsDetached(t *testing.T) {
	s := newTestServer()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "review-detach", "", false)
	s.store.mu.Lock()
	prID := s.store.NextPR
	s.store.PullRequests[prID] = &PullRequest{ID: prID, Number: 1, RepoID: repo.ID, State: "OPEN"}
	s.store.NextPR++
	s.store.mu.Unlock()
	review := s.store.CreatePRReview(prID, admin.ID, "APPROVED", "lgtm")
	if review == nil {
		t.Fatal("create review failed")
	}

	got := s.store.GetPullRequestReview(review.ID)
	got.Body = "hacked"
	if got.SubmittedAt != nil {
		*got.SubmittedAt = got.SubmittedAt.Add(999 * time.Hour)
	}

	fresh := s.store.GetPullRequestReview(review.ID)
	if fresh.Body == "hacked" {
		t.Fatalf("review body mutated through the getter: %q", fresh.Body)
	}
	if fresh.SubmittedAt == nil || !fresh.SubmittedAt.Equal(*review.SubmittedAt) {
		t.Fatal("review SubmittedAt mutated through the getter")
	}
}

// TestOrgGetsAreDetached pins STORE-021 for the org getters: GetOrg and
// GetOrgByID must copy the org, including its MembersCanCreateRepositories bool
// pointer.
func TestOrgGetsAreDetached(t *testing.T) {
	s := newTestServer()
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "org-detach", "Name", "")
	allowed := true
	s.store.UpdateOrg(org.Login, func(o *Org) {
		o.MembersCanCreateRepositories = &allowed
		o.Description = "orig"
	})

	got := s.store.GetOrg(org.Login)
	got.Description = "hacked"
	*got.MembersCanCreateRepositories = false

	fresh := s.store.GetOrgByID(org.ID)
	if fresh.Description == "hacked" {
		t.Fatalf("org description mutated through the getter: %q", fresh.Description)
	}
	if fresh.MembersCanCreateRepositories == nil || !*fresh.MembersCanCreateRepositories {
		t.Fatalf("org bool pointer mutated through the getter: %v", fresh.MembersCanCreateRepositories)
	}
}

// TestTeamGetsAreDetached pins STORE-021 for the team getters: GetTeam and
// GetTeamByID must deep-copy the MemberIDs/RepoPermissions reference fields.
func TestTeamGetsAreDetached(t *testing.T) {
	s := newTestServer()
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "team-detach-org", "T", "")
	team := s.store.CreateTeam(org.Login, "Crew", TeamOptions{})
	s.store.SetTeamMembership(org.Login, team.Slug, admin.ID, TeamRoleMember)

	got := s.store.GetTeam(org.Login, team.Slug)
	got.Name = "hacked"
	got.MemberIDs = append(got.MemberIDs, 99999)
	fresh := s.store.GetTeamByID(team.ID)
	if fresh.Name == "hacked" {
		t.Fatalf("team name mutated through the getter: %q", fresh.Name)
	}
	for _, id := range fresh.MemberIDs {
		if id == 99999 {
			t.Fatalf("team MemberIDs mutated through the getter: %v", fresh.MemberIDs)
		}
	}
}

// TestMembershipGetIsDetached pins STORE-021 for the org-membership getter.
func TestMembershipGetIsDetached(t *testing.T) {
	s := newTestServer()
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "mem-detach-org", "M", "")
	u := &User{ID: s.store.NextUser, Login: "mem-detach-user", Type: "User"}
	s.store.mu.Lock()
	s.store.Users[u.ID] = u
	s.store.UsersByLogin[u.Login] = u
	s.store.NextUser++
	s.store.mu.Unlock()
	s.store.SetMembership(org.Login, u.ID, OrgRoleAdmin, MembershipStateActive)

	got := s.store.GetMembership(org.Login, u.ID)
	got.State = MembershipStatePending
	if fresh := s.store.GetMembership(org.Login, u.ID); fresh.State != MembershipStateActive {
		t.Fatalf("membership mutated through the getter: state=%v", fresh.State)
	}
}

// TestMilestoneAndLabelGetsAreDetached pins STORE-021 for the issues family:
// GetMilestone (with its DueOn time pointer) and GetLabel must return copies.
func TestMilestoneAndLabelGetsAreDetached(t *testing.T) {
	s := newTestServer()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, "ml-detach", "", false)
	due := fixedTestTime.Add(48 * time.Hour)
	ms := s.store.CreateMilestone(repo.ID, admin.ID, "v1", "desc", "open", &due)
	lbl := s.store.CreateLabel(repo.ID, "bug", "d", "ff0000")

	m := s.store.GetMilestone(ms.ID)
	m.Title = "hacked"
	*m.DueOn = m.DueOn.Add(999 * time.Hour)
	if fresh := s.store.GetMilestone(ms.ID); fresh.Title == "hacked" || !fresh.DueOn.Equal(due) {
		t.Fatalf("milestone mutated through the getter: title=%q dueOn=%v", fresh.Title, fresh.DueOn)
	}

	l := s.store.GetLabel(lbl.ID)
	l.Color = "000000"
	if fresh := s.store.GetLabel(lbl.ID); fresh.Color != "ff0000" {
		t.Fatalf("label mutated through the getter: %q", fresh.Color)
	}
}

// TestOrgInvitationAndInteractionLimitGetsAreDetached pins STORE-021 for the
// teams/people family: GetOrgInvitation and GetOrgInteractionLimit must return
// copies, so a reader mutating the result (or its TeamIDs slice) cannot corrupt
// the stored row.
func TestOrgInvitationAndInteractionLimitGetsAreDetached(t *testing.T) {
	s := newTestServer()
	st := s.store
	admin := st.UsersByLogin["admin"]
	org := st.CreateOrg(admin, "detach-org", "Detach", "")
	invitee := &User{ID: st.NextUser, Login: "detach-invitee", Type: "User"}
	st.mu.Lock()
	st.Users[invitee.ID] = invitee
	st.UsersByLogin[invitee.Login] = invitee
	st.NextUser++
	st.mu.Unlock()

	inv, msg := st.CreateOrgInvitation(org, admin, invitee, "", "direct_member", []int{7, 8})
	if inv == nil {
		t.Fatalf("create invitation: %s", msg)
	}

	got := st.GetOrgInvitation(org.Login, inv.ID)
	got.Role = "admin"
	got.TeamIDs[0] = 999
	again := st.GetOrgInvitation(org.Login, inv.ID)
	if again.Role == "admin" || again.TeamIDs[0] != 7 {
		t.Fatalf("invitation mutated through the getter: role=%q teamIDs=%v", again.Role, again.TeamIDs)
	}

	st.SetOrgInteractionLimit(org.Login, "existing_users", fixedTestTime.Add(24*time.Hour))
	lim := st.GetOrgInteractionLimit(org.Login)
	lim.Limit = "hacked"
	if fresh := st.GetOrgInteractionLimit(org.Login); fresh.Limit != "existing_users" {
		t.Fatalf("interaction limit mutated through the getter: %q", fresh.Limit)
	}
}

// TestOrgInvitationReadsAreNonMutating covers the invitation half of STORE-034:
// the invitation list/get endpoints used to take the exclusive lock and run the
// reconcile state machine — durable deletes and MustPut writes — on a GET, so a
// read was non-idempotent and a persist blip faulted it. Reads are now pure; the
// state machine (including the durable membership side effects) runs on the
// background reconciler instead.
func TestOrgInvitationReadsAreNonMutating(t *testing.T) {
	s := newTestServer()
	st := s.store
	admin := st.UsersByLogin["admin"]
	org := st.CreateOrg(admin, "inv-pure-org", "Invitations", "")

	invitee := &User{ID: st.NextUser, Login: "invitee-pure", Type: "User"}
	st.mu.Lock()
	st.Users[invitee.ID] = invitee
	st.UsersByLogin[invitee.Login] = invitee
	st.NextUser++
	st.mu.Unlock()

	inv, msg := st.CreateOrgInvitation(org, admin, invitee, "", "direct_member", nil)
	if inv == nil {
		t.Fatalf("create invitation: %s", msg)
	}

	// Age it past the 7-day TTL.
	st.mu.Lock()
	st.OrgInvitations[inv.ID].CreatedAt = st.currentTime().Add(-8 * 24 * time.Hour)
	st.mu.Unlock()

	// Reads must not reconcile durably: the invitation stays un-failed and the
	// pending membership survives a GET.
	_ = st.ListPendingOrgInvitations(org.Login)
	_ = st.GetOrgInvitation(org.Login, inv.ID)
	st.mu.RLock()
	failedAfterRead := st.OrgInvitations[inv.ID].FailedAt
	_, membershipAfterRead := st.Memberships[membershipKey(org.Login, invitee.ID)]
	st.mu.RUnlock()
	if failedAfterRead != nil {
		t.Fatal("STORE-034: a read durably marked the invitation failed")
	}
	if !membershipAfterRead {
		t.Fatal("STORE-034: a read durably dropped the pending membership")
	}

	// The background reconciler applies the expiry durably.
	st.ReconcileAllOrgInvitations(st.currentTime())
	st.mu.RLock()
	failedAfter := st.OrgInvitations[inv.ID].FailedAt
	_, membershipAfter := st.Memberships[membershipKey(org.Login, invitee.ID)]
	st.mu.RUnlock()
	if failedAfter == nil {
		t.Fatal("reconciler did not mark the expired invitation failed")
	}
	if membershipAfter {
		t.Fatal("reconciler did not drop the expired pending membership")
	}
}

package bleephub

import (
	"testing"
	"time"
)

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

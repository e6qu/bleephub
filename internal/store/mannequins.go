package store

import (
	"fmt"
	"strconv"
	"time"
)

// Mannequins are the placeholder identities an import creates for authors who
// have no account on this instance: github's model for "someone wrote this,
// but nobody here is them yet". An attribution invitation asks a real account
// to claim one; claiming is the target's act, so the invitation records the
// ask rather than performing the reattribution.

type Mannequin struct {
	ID         int       `json:"id"`
	NodeID     string    `json:"node_id"`
	OrgID      int       `json:"org_id"`
	Login      string    `json:"login"`
	Email      string    `json:"email"`
	ClaimantID int       `json:"claimant_id"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type AttributionInvitation struct {
	ID           int       `json:"id"`
	OrgID        int       `json:"org_id"`
	SourceNodeID string    `json:"source_node_id"`
	TargetNodeID string    `json:"target_node_id"`
	CreatedAt    time.Time `json:"created_at"`
}

// EnsureMannequin answers the org's mannequin for login, minting one when the
// import meets that author for the first time. Idempotent by (org, login), so
// a resumed migration does not multiply placeholders.
func (st *Store) EnsureMannequin(orgID int, login, email string) *Mannequin {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if login == "" {
		return nil
	}
	for _, m := range st.Mannequins {
		if m.OrgID == orgID && m.Login == login {
			return cloneMannequin(m)
		}
	}
	now := st.CurrentTime()
	m := &Mannequin{
		ID:        st.NextMannequinID,
		NodeID:    fmt.Sprintf("M_kgDO%08d", st.NextMannequinID),
		OrgID:     orgID,
		Login:     login,
		Email:     email,
		CreatedAt: now,
		UpdatedAt: now,
	}
	st.NextMannequinID++
	st.Mannequins[m.ID] = m
	if st.Persist != nil {
		st.Persist.MustPut("mannequins", strconv.Itoa(m.ID), m)
	}
	return cloneMannequin(m)
}

// FindMannequinByNodeID resolves a mannequin's global id to the live row.
func FindMannequinByNodeID(st *Store, nodeID string) *Mannequin {
	if nodeID == "" {
		return nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, m := range st.Mannequins {
		if m.NodeID == nodeID {
			return m
		}
	}
	return nil
}

// ListMannequins answers an org's mannequins as detached snapshots.
func (st *Store) ListMannequins(orgID int) []*Mannequin {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*Mannequin
	for _, m := range st.Mannequins {
		if m.OrgID == orgID {
			out = append(out, cloneMannequin(m))
		}
	}
	return out
}

// CreateAttributionInvitation records the ask that source's work be claimed by
// target. One open invitation per source: github refuses a second invitation
// while one is pending, because two accounts cannot both be the author.
func (st *Store) CreateAttributionInvitation(orgID int, sourceNodeID, targetNodeID string) (*AttributionInvitation, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	for _, inv := range st.AttributionInvitations {
		if inv.SourceNodeID == sourceNodeID {
			return nil, fmt.Errorf("an attribution invitation for this mannequin already exists")
		}
	}
	inv := &AttributionInvitation{
		ID:           st.NextAttributionInvitationID,
		OrgID:        orgID,
		SourceNodeID: sourceNodeID,
		TargetNodeID: targetNodeID,
		CreatedAt:    st.CurrentTime(),
	}
	st.NextAttributionInvitationID++
	st.AttributionInvitations[inv.ID] = inv
	if st.Persist != nil {
		st.Persist.MustPut("attribution_invitations", strconv.Itoa(inv.ID), inv)
	}
	out := *inv
	return &out, nil
}

func cloneMannequin(m *Mannequin) *Mannequin {
	if m == nil {
		return nil
	}
	out := *m
	return &out
}

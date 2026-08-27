package store

import (
	"fmt"
	"strconv"
	"time"
)

// Mannequins are placeholder identities an import creates for authors with no
// account here. An attribution invitation only records the ask; claiming is
// the target's own act.

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

// EnsureMannequin returns the org's mannequin for login, minting one if
// absent. Idempotent by (org, login) so a resumed migration does not duplicate.
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

// FindMannequinByNodeID returns the live row (not a snapshot) for a node ID.
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

// ListMannequins returns an org's mannequins as detached snapshots.
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

// CreateAttributionInvitation records that source's work be claimed by target.
// GitHub allows only one open invitation per source.
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

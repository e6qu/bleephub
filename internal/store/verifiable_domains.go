package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// Verifiable domains implement the domain-verification lifecycle: an owner
// adds a domain, gets a DNS token, then verifies or approves it. Enterprise
// rows feed Enterprise.VerifiedDomains, which the notification-delivery
// restriction and the /ui-data verified-domains routes read.

// VerifiableDomainNodeIDPrefix is the node-id prefix for VerifiableDomain rows.
const VerifiableDomainNodeIDPrefix = "VD_kgDN"

// GitHub's VerifiableDomainOwner union admits an enterprise or an organization.
const (
	VerifiableDomainOwnerEnterprise   = "Enterprise"
	VerifiableDomainOwnerOrganization = "Organization"
)

// VerifiableDomainTokenTTL matches GitHub's seven-day DNS TXT token expiry.
const VerifiableDomainTokenTTL = 7 * 24 * time.Hour

// VerifiableDomain is one domain on an owner's verification ledger. OwnerType
// ("Enterprise" or "Organization") disambiguates OwnerID, which is drawn from
// separate id sequences.
type VerifiableDomain struct {
	ID        int    `json:"id"`
	NodeID    string `json:"node_id"`
	OwnerType string `json:"owner_type"`
	OwnerID   int    `json:"owner_id"`
	// Domain is stored normalized (NormalizeVerifiedDomain).
	Domain            string    `json:"domain"`
	VerificationToken string    `json:"verification_token"`
	TokenExpiresAt    time.Time `json:"token_expires_at"`
	IsVerified        bool      `json:"is_verified"`
	IsApproved        bool      `json:"is_approved"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func snapshotVerifiableDomain(d *VerifiableDomain) *VerifiableDomain {
	if d == nil {
		return nil
	}
	c := *d
	return &c
}

func (st *Store) persistVerifiableDomainLocked(d *VerifiableDomain) {
	if st.Persist != nil {
		st.Persist.MustPut("verifiable_domains", strconv.Itoa(d.ID), d)
	}
}

// newVerifiableDomainToken mints the DNS challenge value from the CSPRNG.
func newVerifiableDomainToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Errorf("verifiable domain token: %w", err))
	}
	return "bleephub-domain-verification=" + hex.EncodeToString(buf)
}

// CreateVerifiableDomain adds a domain with a fresh token, erroring when it
// does not normalize or the owner already carries it.
func (st *Store) CreateVerifiableDomain(ownerType string, ownerID int, domain string) (*VerifiableDomain, error) {
	cleaned := NormalizeVerifiedDomain(domain)
	if cleaned == "" {
		return nil, fmt.Errorf("%q is not a valid domain", domain)
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()
	for _, existing := range st.VerifiableDomains {
		if existing.OwnerType == ownerType && existing.OwnerID == ownerID && existing.Domain == cleaned {
			//lint:ignore ST1005 GitHub API parity requires this exact upstream message.
			return nil, fmt.Errorf("Domain has already been added")
		}
	}
	now := st.CurrentTime()
	id := st.NextVerifiableDomainID
	st.NextVerifiableDomainID++
	row := &VerifiableDomain{
		ID:                id,
		NodeID:            fmt.Sprintf("%s%08d", VerifiableDomainNodeIDPrefix, id),
		OwnerType:         ownerType,
		OwnerID:           ownerID,
		Domain:            cleaned,
		VerificationToken: newVerifiableDomainToken(),
		TokenExpiresAt:    now.Add(VerifiableDomainTokenTTL),
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	st.VerifiableDomains[id] = row
	st.persistVerifiableDomainLocked(row)
	return snapshotVerifiableDomain(row), nil
}

// FindVerifiableDomainByNodeID resolves a domain global id to the LIVE row.
func FindVerifiableDomainByNodeID(st *Store, nodeID string) *VerifiableDomain {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if id, ok := DecodeNodeDBID(nodeID, VerifiableDomainNodeIDPrefix); ok {
		if d := st.VerifiableDomains[id]; d != nil && d.NodeID == nodeID {
			return d
		}
	}
	return nil
}

// VerifyVerifiableDomain marks the domain verified (the stand-in for a DNS TXT
// lookup), erroring when the token has expired.
func (st *Store) VerifyVerifiableDomain(id int) (*VerifiableDomain, error) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	row := st.VerifiableDomains[id]
	if row == nil {
		return nil, fmt.Errorf("verifiable domain not found")
	}
	if st.CurrentTime().After(row.TokenExpiresAt) {
		return nil, fmt.Errorf("the verification token has expired; regenerate it and update the DNS record before verifying")
	}
	row.IsVerified = true
	row.UpdatedAt = st.CurrentTime()
	st.persistVerifiableDomainLocked(row)
	st.resyncEnterpriseVerifiedDomainsLocked(row)
	return snapshotVerifiableDomain(row), nil
}

// ApproveVerifiableDomain marks the domain approved: an owner vouching for it
// without DNS verification.
func (st *Store) ApproveVerifiableDomain(id int) *VerifiableDomain {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	row := st.VerifiableDomains[id]
	if row == nil {
		return nil
	}
	row.IsApproved = true
	row.UpdatedAt = st.CurrentTime()
	st.persistVerifiableDomainLocked(row)
	st.resyncEnterpriseVerifiedDomainsLocked(row)
	return snapshotVerifiableDomain(row)
}

// RegenerateVerifiableDomainToken replaces the token and restarts its expiry,
// leaving verified/approved status intact — the recovery path from an expired
// token.
func (st *Store) RegenerateVerifiableDomainToken(id int) *VerifiableDomain {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	row := st.VerifiableDomains[id]
	if row == nil {
		return nil
	}
	now := st.CurrentTime()
	row.VerificationToken = newVerifiableDomainToken()
	row.TokenExpiresAt = now.Add(VerifiableDomainTokenTTL)
	row.UpdatedAt = now
	st.persistVerifiableDomainLocked(row)
	return snapshotVerifiableDomain(row)
}

// DeleteVerifiableDomain removes a domain, returning a detached snapshot of
// what was removed, or nil.
func (st *Store) DeleteVerifiableDomain(id int) *VerifiableDomain {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	row := st.VerifiableDomains[id]
	if row == nil {
		return nil
	}
	removed := snapshotVerifiableDomain(row)
	delete(st.VerifiableDomains, id)
	if st.Persist != nil {
		st.Persist.MustDelete("verifiable_domains", strconv.Itoa(id))
	}
	st.resyncEnterpriseVerifiedDomainsLocked(removed)
	return removed
}

// ListVerifiableDomains returns detached snapshots of one owner's domains,
// ordered by database id.
func (st *Store) ListVerifiableDomains(ownerType string, ownerID int) []*VerifiableDomain {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*VerifiableDomain
	for _, row := range st.VerifiableDomains {
		if row.OwnerType == ownerType && row.OwnerID == ownerID {
			out = append(out, snapshotVerifiableDomain(row))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// resyncEnterpriseVerifiedDomainsLocked recomputes the enterprise's flat
// VerifiedDomains list from its verified-or-approved rows; organization-owned
// rows do not feed it. Callers hold st.Mu.
func (st *Store) resyncEnterpriseVerifiedDomainsLocked(changed *VerifiableDomain) {
	if changed == nil || changed.OwnerType != VerifiableDomainOwnerEnterprise {
		return
	}
	e := st.Enterprises[changed.OwnerID]
	if e == nil {
		return
	}
	var names []string
	for _, row := range st.VerifiableDomains {
		if row.OwnerType == VerifiableDomainOwnerEnterprise && row.OwnerID == e.ID && (row.IsVerified || row.IsApproved) {
			names = append(names, row.Domain)
		}
	}
	sort.Strings(names)
	e.VerifiedDomains = names
	e.UpdatedAt = st.CurrentTime()
	st.persistEnterpriseLocked(e)
}

// reconcileEnterpriseDomainRowsLocked reconciles the domain-row ledger with a
// flat list from SetEnterpriseVerifiedDomains: missing names gain a verified
// row, dropped names lose theirs. Callers hold st.Mu.
func (st *Store) reconcileEnterpriseDomainRowsLocked(enterpriseID int, names []string) {
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	now := st.CurrentTime()
	for id, row := range st.VerifiableDomains {
		if row.OwnerType != VerifiableDomainOwnerEnterprise || row.OwnerID != enterpriseID {
			continue
		}
		if wanted[row.Domain] {
			delete(wanted, row.Domain)
			if !row.IsVerified {
				row.IsVerified = true
				row.UpdatedAt = now
				st.persistVerifiableDomainLocked(row)
			}
			continue
		}
		delete(st.VerifiableDomains, id)
		if st.Persist != nil {
			st.Persist.MustDelete("verifiable_domains", strconv.Itoa(id))
		}
	}
	for name := range wanted {
		id := st.NextVerifiableDomainID
		st.NextVerifiableDomainID++
		row := &VerifiableDomain{
			ID:                id,
			NodeID:            fmt.Sprintf("%s%08d", VerifiableDomainNodeIDPrefix, id),
			OwnerType:         VerifiableDomainOwnerEnterprise,
			OwnerID:           enterpriseID,
			Domain:            name,
			VerificationToken: newVerifiableDomainToken(),
			TokenExpiresAt:    now.Add(VerifiableDomainTokenTTL),
			IsVerified:        true,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		st.VerifiableDomains[id] = row
		st.persistVerifiableDomainLocked(row)
	}
}

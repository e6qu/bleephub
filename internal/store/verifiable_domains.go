package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// Verifiable domains are the GraphQL-first domain-verification lifecycle:
// an enterprise or organization adds a domain, receives a DNS verification
// token with an expiry, and then verifies (the TXT record was found) or
// approves (an owner vouches for it without DNS) the domain. The enterprise
// half backs the same Enterprise.VerifiedDomains list the notification
// delivery restriction and the /ui-data verified-domains routes read, so a
// domain verified over GraphQL restricts notification delivery exactly as
// one written through the UI surface does.

// VerifiableDomainNodeIDPrefix is the node-id prefix for VerifiableDomain
// rows, following the "<prefix><zero-padded id>" convention the other
// enterprise-account records use.
const VerifiableDomainNodeIDPrefix = "VD_kgDN"

// Verifiable domain owner types. GitHub's VerifiableDomainOwner union admits
// an enterprise or an organization.
const (
	VerifiableDomainOwnerEnterprise   = "Enterprise"
	VerifiableDomainOwnerOrganization = "Organization"
)

// VerifiableDomainTokenTTL is how long a verification token stays usable —
// GitHub's DNS TXT verification token expires after seven days.
const VerifiableDomainTokenTTL = 7 * 24 * time.Hour

// VerifiableDomain is one domain on an owner's verification ledger. Owner is
// an enterprise or an organization; OwnerType distinguishes them because
// their ids are drawn from different sequences.
type VerifiableDomain struct {
	ID     int    `json:"id"`
	NodeID string `json:"node_id"`
	// OwnerType is "Enterprise" or "Organization".
	OwnerType string `json:"owner_type"`
	OwnerID   int    `json:"owner_id"`
	// Domain is stored normalized: lower case, no leading "@", no
	// trailing dot (NormalizeVerifiedDomain).
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

// newVerifiableDomainToken mints the random challenge value the owner is
// asked to publish in DNS. It is a credential-shaped value, so it comes from
// the system's CSPRNG like the API tokens do.
func newVerifiableDomainToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Errorf("verifiable domain token: %w", err))
	}
	return "bleephub-domain-verification=" + hex.EncodeToString(buf)
}

// CreateVerifiableDomain adds a domain to an owner's ledger with a fresh
// verification token. It returns nil and an error when the domain does not
// normalize to a domain at all or the owner already carries it.
func (st *Store) CreateVerifiableDomain(ownerType string, ownerID int, domain string) (*VerifiableDomain, error) {
	cleaned := NormalizeVerifiedDomain(domain)
	if cleaned == "" {
		return nil, fmt.Errorf("%q is not a valid domain", domain)
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()
	for _, existing := range st.VerifiableDomains {
		if existing.OwnerType == ownerType && existing.OwnerID == ownerID && existing.Domain == cleaned {
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

// VerifyVerifiableDomain marks the domain verified — the simulator's stand-in
// for the DNS TXT lookup finding the token — unless the token has expired,
// in which case the caller must regenerate it first.
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

// ApproveVerifiableDomain marks the domain approved: an owner vouching for a
// domain they cannot complete DNS verification for.
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

// RegenerateVerifiableDomainToken replaces the domain's verification token
// and restarts its expiry window. Verified or approved status is not
// revoked — regenerating is how an owner recovers from an expired token.
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

// DeleteVerifiableDomain removes a domain and returns a detached snapshot of
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
// VerifiedDomains list — the one the notification-delivery restriction and
// the /ui-data verified-domains surface read — from the domain rows that are
// verified or approved. Rows owned by organizations do not feed it. Callers
// hold st.Mu.
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

// reconcileEnterpriseDomainRowsLocked makes the domain-row ledger agree with
// a flat verified-domain list written through SetEnterpriseVerifiedDomains:
// names without a row gain a verified one, and rows whose names left the
// list are dropped. Callers hold st.Mu.
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

package store

import "time"

func (st *Store) HasPagesSite(repoID int) bool {
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	return st.Misc.PagesByRepo[repoID] != nil
}

type GPGKeyEmail struct {
	Email    string `json:"email"`
	Verified bool   `json:"verified"`
	Primary  bool   `json:"primary"`
}

type MarketplacePendingChange struct {
	PlanID        int       `json:"plan_id,omitempty"`
	BillingCycle  string    `json:"billing_cycle,omitempty"`
	UnitCount     *int      `json:"unit_count,omitempty"`
	EffectiveDate time.Time `json:"effective_date"`
	Cancellation  bool      `json:"cancellation,omitempty"`
	ActorID       int       `json:"actor_id"`
}

type PagesBuildErr struct {
	Message *string `json:"message"`
}

type PagesHTTPSCertificate struct {
	State       string   `json:"state"`
	Description string   `json:"description"`
	Domains     []string `json:"domains"`
	ExpiresAt   *string  `json:"expires_at"`
}

type PagesPusher struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

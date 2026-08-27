package store

import (
	"crypto/rsa"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// UserKey json tags shape the persisted row (responses go through
// userKeyToJSON); UserID must round-trip to rebuild KeysByUser.
type UserKey struct {
	ID        int       `json:"id"`
	Key       string    `json:"key"`
	Title     string    `json:"title"`
	Verified  bool      `json:"verified"`
	UserID    int       `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
	// parsed caches the public key for SSH Git auth. Not persisted; rebuilt on load.
	parsed ssh.PublicKey
}

// CacheParsedKey parses a user key's authorized-key text into k.parsed. An
// unparseable key stays registered (listed, deletable) but can never
// authenticate.
func CacheParsedKey(k *UserKey) error {
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k.Key))
	if err != nil {
		return fmt.Errorf("user key %d: %w", k.ID, err)
	}
	k.parsed = parsed
	return nil
}

type PagesSite struct {
	CNAME                string                 `json:"cname"`
	URL                  string                 `json:"url"`
	HTMLURL              string                 `json:"html_url"`
	Status               string                 `json:"status"`
	Source               map[string]interface{} `json:"source"`
	Public               bool                   `json:"public"`
	Custom404            bool                   `json:"custom_404"`
	ProtectedDomainState *string                `json:"protected_domain_state"`
	BuildType            *string                `json:"build_type"`
	HTTPSCertificate     *PagesHTTPSCertificate `json:"https_certificate,omitempty"`
	HTTPSEnforced        bool                   `json:"https_enforced"`
}

type GPGKey struct {
	ID                int           `json:"id"`
	PrimaryKeyID      int           `json:"primary_key_id"`
	KeyID             string        `json:"key_id"`
	RawKey            string        `json:"raw_key"`
	PublicKey         string        `json:"public_key"`
	Name              string        `json:"name,omitempty"`
	Emails            []GPGKeyEmail `json:"emails"`
	CanSign           bool          `json:"can_sign"`
	CanEncryptComms   bool          `json:"can_encrypt_comms"`
	CanEncryptStorage bool          `json:"can_encrypt_storage"`
	CanCertify        bool          `json:"can_certify"`
	Revoked           bool          `json:"revoked"`
	CreatedAt         time.Time     `json:"created_at"`
	ExpiresAt         *time.Time    `json:"expires_at,omitempty"`
	UserID            int           `json:"-"`
}

type PagesBuild struct {
	// ID routes GET .../pages/builds/{build_id}. Not serialized: GitHub's build
	// object has no top-level id, only the trailing segment of url.
	ID        int64          `json:"-"`
	URL       string         `json:"url"`
	Status    string         `json:"status"`
	Pusher    *PagesPusher   `json:"pusher"`
	Commit    string         `json:"commit"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	Duration  int            `json:"duration"`
	Error     *PagesBuildErr `json:"error"`
}

func pagesBuildIDFromURL(url string) int64 {
	idx := strings.LastIndex(url, "/")
	if idx < 0 || idx == len(url)-1 {
		return 0
	}
	id, err := strconv.ParseInt(url[idx+1:], 10, 64)
	if err != nil {
		return 0
	}
	return id
}

type AuditEntry struct {
	ID        int64                  `json:"_document_id"`
	Timestamp string                 `json:"@timestamp"`
	Action    string                 `json:"action"`
	Actor     string                 `json:"actor"`
	Org       string                 `json:"org,omitempty"`
	Data      map[string]interface{} `json:"data,omitempty"`
	Version   string                 `json:"version"`
}

// ListOrgAuditEntries returns detached snapshots (STORE-021) of the audit
// entries the org's log surfaces: org-scoped entries plus the org-less
// instance-wide entries GitHub also lists. Mirrors handleOrgAuditLog's filter so
// REST and GraphQL answer from the same rows.
func (st *Store) ListOrgAuditEntries(org string) []*AuditEntry {
	st.Misc.Mu.RLock()
	defer st.Misc.Mu.RUnlock()
	out := make([]*AuditEntry, 0, len(st.Misc.AuditLog))
	for _, e := range st.Misc.AuditLog {
		if e.Org != "" && e.Org != org {
			continue
		}
		out = append(out, cloneAuditEntry(e))
	}
	return out
}

func cloneAuditEntry(e *AuditEntry) *AuditEntry {
	if e == nil {
		return nil
	}
	clone := *e
	if e.Data != nil {
		data := make(map[string]interface{}, len(e.Data))
		for k, v := range e.Data {
			data[k] = v
		}
		clone.Data = data
	}
	return &clone
}

type AuditLogEvent struct {
	ID         int64                  `json:"id"`
	Timestamp  string                 `json:"timestamp"`
	Actor      string                 `json:"actor"`
	Action     string                 `json:"action"`
	TargetType string                 `json:"target_type"`
	TargetID   string                 `json:"target_id"`
	Org        string                 `json:"org,omitempty"`
	Details    map[string]interface{} `json:"details,omitempty"`
	CreatedAt  time.Time              `json:"-"`
}

type MarketplacePlan struct {
	ID                  int      `json:"id"`
	ListingSlug         string   `json:"listing_slug"`
	Number              int      `json:"number"`
	Name                string   `json:"name"`
	Description         string   `json:"description"`
	MonthlyPriceInCents int      `json:"monthly_price_in_cents"`
	YearlyPriceInCents  int      `json:"yearly_price_in_cents"`
	PriceModel          string   `json:"price_model"`
	HasFreeTrial        bool     `json:"has_free_trial"`
	UnitName            string   `json:"unit_name"`
	State               string   `json:"state"`
	Bullets             []string `json:"bullets"`
}

type MarketplaceListing struct {
	Slug               string    `json:"slug"`
	Name               string    `json:"name"`
	Description        string    `json:"description"`
	FullDescription    string    `json:"full_description"`
	SetupURL           string    `json:"setup_url,omitempty"`
	InstallationURL    string    `json:"installation_url,omitempty"`
	GitHubAppID        int       `json:"github_app_id,omitempty"`
	OAuthAppClientID   string    `json:"oauth_app_client_id,omitempty"`
	WebhookURL         string    `json:"webhook_url,omitempty"`
	WebhookSecret      string    `json:"webhook_secret,omitempty"`
	WebhookContentType string    `json:"webhook_content_type,omitempty"`
	WebhookActive      bool      `json:"webhook_active"`
	WebhookID          int       `json:"webhook_id,omitempty"`
	Published          bool      `json:"published"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type MarketplacePurchase struct {
	ListingSlug   string     `json:"listing_slug"`
	AccountID     int        `json:"account_id"`
	AccountType   string     `json:"account_type"`
	BillingCycle  string     `json:"billing_cycle"`
	PlanID        int        `json:"plan_id"`
	PlanName      string     `json:"plan_name"`
	OnFreeTrial   bool       `json:"on_free_trial"`
	FreeTrialEnds *time.Time `json:"free_trial_ends_on,omitempty"`
	// Members surfaced by GET /user/marketplace_purchases.
	UnitCount       *int                      `json:"unit_count,omitempty"`
	NextBillingDate *time.Time                `json:"next_billing_date,omitempty"`
	UpdatedAt       *time.Time                `json:"updated_at,omitempty"`
	InstallationID  *int                      `json:"installation_id,omitempty"`
	PendingChange   *MarketplacePendingChange `json:"pending_change,omitempty"`
}

type MiscStore struct {
	Mu               sync.RWMutex                 `json:"-"`
	UserKeys         map[int]*UserKey             `json:"-"`
	KeysByUser       map[int][]*UserKey           `json:"-"`
	GpgKeys          map[int]*GPGKey              `json:"-"`
	GpgKeysByUser    map[int][]*GPGKey            `json:"-"`
	Follows          map[string]map[string]bool   `json:"-"`
	PagesByRepo      map[int]*PagesSite           `json:"-"`
	PagesBuilds      map[string][]*PagesBuild     `json:"-"`
	BranchProtection map[string]*BranchProtection `json:"-"`
	// BranchProtectionPatterns holds web-only fnmatch pattern rules per repo ID,
	// consulted when no exact-name rule matches (served under /ui-data).
	BranchProtectionPatterns map[int][]*BranchProtectionPatternRule `json:"-"`
	// BranchProtectionExtras holds a rule's GraphQL-only members (deployment
	// requirements, force-push bypass actors, creator), keyed by BpKey. Kept
	// outside BranchProtection, whose JSON is GitHub's REST protection shape.
	BranchProtectionExtras    map[string]*BranchProtectionRuleExtras `json:"-"`
	AuditLog                  []*AuditEntry                          `json:"-"`
	AuditLogEvents            []*AuditLogEvent                       `json:"-"`
	marketplaceListings       map[string]*MarketplaceListing
	marketplacePlans          map[int]*MarketplacePlan
	MarketplacePurchases      map[string]*MarketplacePurchase `json:"-"`
	marketplaceDeliveries     map[string][]*WebhookDelivery
	nextMarketplaceDeliveryID int
	nextMarketplacePlanID     int
	// OidcClaimKeys maps an OIDC-subject-customization scope ("repo:owner/name"
	// or "org:login") to its include_claim_keys, per-scope to prevent cross-tenant
	// clobbering.
	OidcClaimKeys       map[string][]string  `json:"-"`
	NextKeyID           int                  `json:"-"`
	NextGPGKeyID        int                  `json:"-"`
	NextPagesBuildID    int64                `json:"-"`
	NextAuditID         int64                `json:"-"`
	NextAdminAuditID    int64                `json:"-"`
	OidcKey             *rsa.PrivateKey      `json:"-"`
	Persist             *Persistence         `json:"-"`
	blockedUsers        map[int]map[int]bool // userID -> targetID -> blocked
	socialAccounts      map[int][]map[string]interface{}
	sshSigningKeys      map[int][]map[string]interface{}
	nextSSHSigningKeyID int
}

func newMiscStore() *MiscStore {
	return &MiscStore{
		UserKeys:                  map[int]*UserKey{},
		KeysByUser:                map[int][]*UserKey{},
		GpgKeys:                   map[int]*GPGKey{},
		GpgKeysByUser:             map[int][]*GPGKey{},
		Follows:                   map[string]map[string]bool{},
		PagesByRepo:               map[int]*PagesSite{},
		PagesBuilds:               map[string][]*PagesBuild{},
		BranchProtection:          map[string]*BranchProtection{},
		BranchProtectionPatterns:  map[int][]*BranchProtectionPatternRule{},
		BranchProtectionExtras:    map[string]*BranchProtectionRuleExtras{},
		marketplaceListings:       map[string]*MarketplaceListing{},
		marketplacePlans:          map[int]*MarketplacePlan{},
		MarketplacePurchases:      map[string]*MarketplacePurchase{},
		marketplaceDeliveries:     map[string][]*WebhookDelivery{},
		nextMarketplaceDeliveryID: 1,
		nextMarketplacePlanID:     1,
		AuditLogEvents:            []*AuditLogEvent{},
		NextKeyID:                 1,
		NextGPGKeyID:              1,
		NextPagesBuildID:          1,
		blockedUsers:              map[int]map[int]bool{},
		socialAccounts:            map[int][]map[string]interface{}{},
		sshSigningKeys:            map[int][]map[string]interface{}{},
		nextSSHSigningKeyID:       1,
	}
}

func BpKey(repoID int, branch string) string {
	return strconv.Itoa(repoID) + ":" + branch
}

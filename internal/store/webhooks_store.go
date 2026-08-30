package store

import (
	"strconv"
	"time"
)

// MaxHookDeliveries caps retained per-hook delivery history, mirroring GitHub's ≈30-day window and bounding unbounded growth.
const MaxHookDeliveries = 500

// Webhook represents a GitHub repository webhook.
//
// Secret is persisted (deliveries must keep signing X-Hub-Signature-256 after a restart) but never marshaled to clients
// (hookToJSON omits it). RepoKey stays json:"-": it equals the persistence bucket key ("owner/name"), which the loader backfills on reload.
type Webhook struct {
	ID          int      `json:"id"`
	URL         string   `json:"config_url"`
	Secret      string   `json:"secret"`
	ContentType string   `json:"content_type"`
	InsecureSSL string   `json:"insecure_ssl"`
	Events      []string `json:"events"`
	Active      bool     `json:"active"`
	RepoKey     string   `json:"-"`
	// OrgLogin marks an org-level hook; like RepoKey it equals the bucket key and the loader backfills it.
	// Exactly one of RepoKey/OrgLogin is set (both empty = app-level pseudo-hook).
	OrgLogin string `json:"-"`
	// MarketplaceSlug marks a Marketplace webhook; listing persistence owns its configuration.
	MarketplaceSlug string `json:"-"`
	// Global marks an appliance-wide GHES webhook owned by EnterpriseSettings.
	Global bool `json:"-"`
	// LastResponse is the outcome of the most recent delivery; nil until one occurs (rendered "unused").
	LastResponse *HookLastResponse `json:"last_response,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// HookLastResponse is the outcome of a webhook's most recent delivery.
type HookLastResponse struct {
	Code    int    `json:"code"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// CloneWebhook detaches every mutable child from the store-owned hook. Callers must hold st.Mu when cloning a Store-owned hook.
func CloneWebhook(h *Webhook) *Webhook {
	if h == nil {
		return nil
	}
	snapshot := *h
	snapshot.Events = append([]string(nil), h.Events...)
	if h.LastResponse != nil {
		lastResponse := *h.LastResponse
		snapshot.LastResponse = &lastResponse
	}
	return &snapshot
}

// DeliveryRequest holds the request details of a webhook delivery.
type DeliveryRequest struct {
	Headers map[string]string `json:"headers"`
	Payload interface{}       `json:"payload"`
}

// DeliveryResponse holds the response details of a webhook delivery.
type DeliveryResponse struct {
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
}

// WebhookDelivery records a single delivery attempt for a webhook.
type WebhookDelivery struct {
	ID             int               `json:"id"`
	HookID         int               `json:"hook_id"`
	AppID          int               `json:"app_id,omitempty"`
	InstallationID int               `json:"installation_id,omitempty"`
	RepositoryID   int               `json:"repository_id,omitempty"`
	TargetURL      string            `json:"url"`
	GUID           string            `json:"guid"`
	Event          string            `json:"event"`
	Action         string            `json:"action"`
	StatusCode     int               `json:"status_code"`
	Duration       float64           `json:"duration"`
	Request        *DeliveryRequest  `json:"request"`
	Response       *DeliveryResponse `json:"response"`
	Redelivery     bool              `json:"redelivery"`
	DeliveredAt    time.Time         `json:"delivered_at"`
	ThrottledAt    *time.Time        `json:"throttled_at"`
}

// CreateHook creates a new webhook for a repository.
func (st *Store) CreateHook(repoKey, url, secret, contentType, insecureSSL string, events []string, active bool) *Webhook {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)

	if st.Hooks == nil {
		st.Hooks = make(map[string][]*Webhook)
	}

	if contentType == "" {
		contentType = "form"
	}
	if insecureSSL == "" {
		insecureSSL = "0"
	}

	now := time.Now()
	hook := &Webhook{
		ID:          st.NextHookID,
		URL:         url,
		Secret:      secret,
		ContentType: contentType,
		InsecureSSL: insecureSSL,
		Events:      events,
		Active:      active,
		RepoKey:     repoKey,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	st.NextHookID++
	st.Hooks[repoKey] = append(st.Hooks[repoKey], hook)
	if st.Persist != nil {
		st.Persist.MustPut("hooks", repoKey, st.Hooks[repoKey])
	}
	return hook
}

// GetHook returns a webhook by repo key and hook ID, or nil.
func (st *Store) GetHook(repoKey string, hookID int) *Webhook {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)

	for _, h := range st.Hooks[repoKey] {
		if h.ID == hookID {
			return CloneWebhook(h)
		}
	}
	return nil
}

// ListHooks returns all webhooks for a repository.
func (st *Store) ListHooks(repoKey string) []*Webhook {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)

	hooks := st.Hooks[repoKey]
	out := make([]*Webhook, len(hooks))
	for i, hook := range hooks {
		out[i] = CloneWebhook(hook)
	}
	return snapshotWebhooks(out)
}

// UpdateHook updates a webhook in place. Returns false if not found.
func (st *Store) UpdateHook(repoKey string, hookID int, fn func(h *Webhook)) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)

	for _, h := range st.Hooks[repoKey] {
		if h.ID == hookID {
			fn(h)
			h.UpdatedAt = time.Now()
			if st.Persist != nil {
				st.Persist.MustPut("hooks", repoKey, st.Hooks[repoKey])
			}
			return true
		}
	}
	return false
}

// DeleteHook removes a webhook. Returns false if not found.
func (st *Store) DeleteHook(repoKey string, hookID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)

	hooks := st.Hooks[repoKey]
	for i, h := range hooks {
		if h.ID == hookID {
			st.Hooks[repoKey] = append(hooks[:i], hooks[i+1:]...)
			if st.Persist != nil {
				if len(st.Hooks[repoKey]) > 0 {
					st.Persist.MustPut("hooks", repoKey, st.Hooks[repoKey])
				} else {
					st.Persist.MustDelete("hooks", repoKey)
				}
			}
			return true
		}
	}
	return false
}

// AddDelivery records a webhook delivery.
func (st *Store) AddDelivery(delivery *WebhookDelivery) {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	if st.HookDeliveries == nil {
		st.HookDeliveries = make(map[int][]*WebhookDelivery)
	}

	delivery.ID = st.NextDeliveryID
	st.NextDeliveryID++
	list := append(st.HookDeliveries[delivery.HookID], delivery)
	// Keep only the newest MaxHookDeliveries (see the const).
	if len(list) > MaxHookDeliveries {
		list = list[len(list)-MaxHookDeliveries:]
	}
	st.HookDeliveries[delivery.HookID] = list
	if st.Persist != nil {
		st.Persist.MustPut("hook_deliveries", strconv.Itoa(delivery.HookID), list)
	}
}

// HookLastResp reads the hook's last_response under the lock. A direct h.LastResponse read races SetHookLastResponse
// on the async deliverWebhook goroutine, so every JSON-rendering path must use this.
func (st *Store) HookLastResp(h *Webhook) *HookLastResponse {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	return h.LastResponse
}

// SetHookLastResponse records the outcome of a hook's most recent delivery.
func (st *Store) SetHookLastResponse(repoKey string, hookID int, lr *HookLastResponse) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	repoKey = st.canonicalRepoKeyLocked(repoKey)
	for _, h := range st.Hooks[repoKey] {
		if h.ID == hookID {
			h.LastResponse = lr
			if st.Persist != nil {
				st.Persist.MustPut("hooks", repoKey, st.Hooks[repoKey])
			}
			return
		}
	}
}

// ListDeliveries returns the hook's deliveries newest-first. Rows are shared live, not cloned: they are write-once and
// carry large payloads (STORE-021 exception, as for ListAppDeliveries).
func (st *Store) ListDeliveries(hookID int) []*WebhookDelivery {
	st.Mu.RLock()
	defer st.Mu.RUnlock()

	deliveries := st.HookDeliveries[hookID]
	out := make([]*WebhookDelivery, len(deliveries))
	copy(out, deliveries)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

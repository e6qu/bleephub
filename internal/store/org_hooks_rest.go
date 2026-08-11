package store

import "time"

// CreateOrgHook creates a webhook on an organization.
func (st *Store) CreateOrgHook(orgLogin, url, secret, contentType, insecureSSL string, events []string, active bool) *Webhook {
	st.Mu.Lock()
	defer st.Mu.Unlock()

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
		OrgLogin:    orgLogin,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	st.NextHookID++
	st.OrgHooks[orgLogin] = append(st.OrgHooks[orgLogin], hook)
	if st.Persist != nil {
		st.Persist.MustPut("org_hooks", orgLogin, st.OrgHooks[orgLogin])
	}
	return hook
}

// GetOrgHook returns an org webhook by org login and hook ID, or nil.
func (st *Store) GetOrgHook(orgLogin string, hookID int) *Webhook {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, h := range st.OrgHooks[orgLogin] {
		if h.ID == hookID {
			return CloneWebhook(h)
		}
	}
	return nil
}

// ListOrgHooks returns all webhooks on an organization.
func (st *Store) ListOrgHooks(orgLogin string) []*Webhook {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	hooks := st.OrgHooks[orgLogin]
	out := make([]*Webhook, len(hooks))
	for i, hook := range hooks {
		out[i] = CloneWebhook(hook)
	}
	return snapshotWebhooks(out)
}

// UpdateOrgHook updates an org webhook in place. Returns false if not found.
func (st *Store) UpdateOrgHook(orgLogin string, hookID int, fn func(h *Webhook)) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	for _, h := range st.OrgHooks[orgLogin] {
		if h.ID == hookID {
			fn(h)
			h.UpdatedAt = time.Now()
			if st.Persist != nil {
				st.Persist.MustPut("org_hooks", orgLogin, st.OrgHooks[orgLogin])
			}
			return true
		}
	}
	return false
}

// DeleteOrgHook removes an org webhook. Returns false if not found.
func (st *Store) DeleteOrgHook(orgLogin string, hookID int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	hooks := st.OrgHooks[orgLogin]
	for i, h := range hooks {
		if h.ID == hookID {
			st.OrgHooks[orgLogin] = append(hooks[:i], hooks[i+1:]...)
			if st.Persist != nil {
				if len(st.OrgHooks[orgLogin]) > 0 {
					st.Persist.MustPut("org_hooks", orgLogin, st.OrgHooks[orgLogin])
				} else {
					st.Persist.MustDelete("org_hooks", orgLogin)
				}
			}
			return true
		}
	}
	return false
}

// SetOrgHookLastResponse records the outcome of an org hook's most recent delivery.
func (st *Store) SetOrgHookLastResponse(orgLogin string, hookID int, lr *HookLastResponse) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	for _, h := range st.OrgHooks[orgLogin] {
		if h.ID == hookID {
			h.LastResponse = lr
			if st.Persist != nil {
				st.Persist.MustPut("org_hooks", orgLogin, st.OrgHooks[orgLogin])
			}
			return
		}
	}
}

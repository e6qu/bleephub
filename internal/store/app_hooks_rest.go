package store

import "strconv"

// AddAppDelivery records an app-level webhook delivery on the App's queue.
func (st *Store) AddAppDelivery(appID int, d *WebhookDelivery) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if st.AppHookDeliveries == nil {
		st.AppHookDeliveries = make(map[int][]*WebhookDelivery)
	}
	d.ID = st.NextDeliveryID
	st.NextDeliveryID++
	list := append(st.AppHookDeliveries[appID], d)
	if len(list) > MaxHookDeliveries {
		list = list[len(list)-MaxHookDeliveries:]
	}
	st.AppHookDeliveries[appID] = list
	if st.Persist != nil {
		st.Persist.MustPut("app_hook_deliveries", strconv.Itoa(appID), list)
	}
}

// ListAppDeliveries returns app-level deliveries newest-first.
// ListAppDeliveries returns the app's deliveries. Like GetAppDelivery it hands
// back live rows deliberately: deliveries are write-once (append-only, never
// mutated) so a reader never races a writer, and each carries full payloads
// cloning would needlessly copy (STORE-021 documented exception).
func (st *Store) ListAppDeliveries(appID int) []*WebhookDelivery {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	src := st.AppHookDeliveries[appID]
	out := make([]*WebhookDelivery, len(src))
	copy(out, src)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// GetAppDelivery returns a single app-level delivery.
// GetAppDelivery returns a stored delivery by id, or nil.
//
// This intentionally returns the live row rather than a snapshot: deliveries are
// write-once — AddAppDelivery appends a record and nothing mutates it afterward
// (a redelivery appends a brand-new record), so a reader never races a writer —
// and a WebhookDelivery carries the full request/response payloads that cloning
// on every read would needlessly copy.
func (st *Store) GetAppDelivery(appID, deliveryID int) *WebhookDelivery {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, d := range st.AppHookDeliveries[appID] {
		if d.ID == deliveryID {
			return d
		}
	}
	return nil
}

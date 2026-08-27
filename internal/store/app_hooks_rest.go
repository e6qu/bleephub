package store

import "strconv"

// AddAppDelivery records an app-level webhook delivery.
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

// ListAppDeliveries returns app deliveries newest-first as live rows: they are
// write-once, so a reader never races a writer (STORE-021 exception).
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

// GetAppDelivery returns a delivery by id, or nil. Returns the live row:
// deliveries are write-once, so a reader never races a writer (STORE-021 exception).
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

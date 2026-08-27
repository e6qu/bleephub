package store

// SnapshotHook copies a hook's config under the store lock. Hook edits mutate
// the stored *Webhook in place, so a delivery must be addressed and signed by
// its config as of queue time, not race a concurrent PATCH mid-flight.
func (st *Store) SnapshotHook(h *Webhook) *Webhook {
	if h == nil {
		return nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	snapshot := CloneWebhook(h)
	snapshot.LastResponse = nil
	return snapshot
}

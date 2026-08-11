package store

// SnapshotHook copies the configuration a delivery reads, under the store
// lock. Hook edits mutate the stored *Webhook in place, so a delivery holding
// the shared pointer races every PATCH of the hook it is delivering — and a
// delivery is addressed and signed by the configuration as of the moment it
// was queued, not by whatever the hook becomes mid-flight.
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

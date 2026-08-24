package store

import "sort"

// listGlobalAdvisories returns every advisory in the global database view,
// i.e. every repository advisory that has been published.
func (st *Store) ListGlobalAdvisories() []*SecurityAdvisory {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*SecurityAdvisory
	for _, a := range st.SecurityAdvisories {
		if a.PublishedAt == nil {
			continue
		}
		if a.State != "published" && a.State != "withdrawn" {
			continue
		}
		// A detached snapshot, not the live row (STORE-021): alert derivation
		// walks this list while holding no lock and reads the vulnerability
		// slice off each advisory, which a concurrent advisory update would
		// otherwise be rewriting underneath it.
		out = append(out, cloneSecurityAdvisory(a))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotSlice(out)
}

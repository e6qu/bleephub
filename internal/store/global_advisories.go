package store

import "sort"

// ListGlobalAdvisories returns every published repository advisory.
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
		// Detached snapshot, not the live row (STORE-021): alert derivation
		// walks this list lock-free and reads each advisory's vulnerability
		// slice, which a concurrent update would otherwise be rewriting.
		out = append(out, cloneSecurityAdvisory(a))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return snapshotSlice(out)
}

package store

// LookupAgentByClientID returns the agent whose Authorization.ClientID matches,
// or nil if no agent has registered with that ClientID. Agent count is bounded
// by the number of registered runners, so the linear scan is fine.
func (st *Store) LookupAgentByClientID(clientID string) *Agent {
	if clientID == "" {
		return nil
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, a := range st.Agents {
		if a.Authorization != nil && a.Authorization.ClientID == clientID {
			return a
		}
	}
	return nil
}

// SetLabels replaces all custom labels on an agent while preserving system
// (read-only) labels. Names supplied that are system labels are treated as
// system labels.
func (a *Agent) SetLabels(names []string) {
	custom := []Label{}
	for _, l := range a.Labels {
		if l.Type == "system" {
			custom = append(custom, l)
		}
	}
	nextID := a.nextLabelID()
	for _, name := range names {
		custom = append(custom, Label{
			ID:   nextID,
			Name: name,
			Type: a.labelTypeForName(name),
		})
		nextID++
	}
	a.Labels = custom
}

// AddLabels appends custom labels, deduplicating by name.
func (a *Agent) AddLabels(names []string) {
	have := map[string]bool{}
	for _, l := range a.Labels {
		have[l.Name] = true
	}
	for _, name := range names {
		if have[name] {
			continue
		}
		a.Labels = append(a.Labels, Label{
			ID:   a.nextLabelID(),
			Name: name,
			Type: a.labelTypeForName(name),
		})
		have[name] = true
	}
}

// RemoveLabels removes custom labels by name; system labels are never removed.
func (a *Agent) RemoveLabels(names []string) {
	drop := map[string]bool{}
	for _, n := range names {
		drop[n] = true
	}
	kept := a.Labels[:0:0]
	for _, l := range a.Labels {
		if l.Type == "system" || !drop[l.Name] {
			kept = append(kept, l)
		}
	}
	a.Labels = kept
}

// ClearLabels removes every custom label, leaving system labels in place.
func (a *Agent) ClearLabels() {
	kept := a.Labels[:0:0]
	for _, l := range a.Labels {
		if l.Type == "system" {
			kept = append(kept, l)
		}
	}
	a.Labels = kept
}

func (a *Agent) labelTypeForName(name string) string {
	for _, l := range a.Labels {
		if l.Name == name {
			return l.Type
		}
	}
	return "custom"
}

func (a *Agent) nextLabelID() int {
	max := 0
	for _, l := range a.Labels {
		if l.ID > max {
			max = l.ID
		}
	}
	return max + 1
}

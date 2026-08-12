package store

// Store types and methods for GitHub Copilot Spaces — interactive AI
// workspaces owned by a user or an organization, each carrying
// collaborators (users or teams, with a role) and attached resources.

import (
	"sort"
	"strconv"
	"time"
)

// CopilotSpace is one GitHub Copilot Space. Number identifies the space
// within its owner; ID is globally unique.
type CopilotSpace struct {
	ID                  int64                       `json:"id"`
	Number              int                         `json:"number"`
	OwnerType           string                      `json:"owner_type"` // "User" | "Organization"
	OwnerLogin          string                      `json:"owner_login"`
	Name                string                      `json:"name"`
	Description         string                      `json:"description"`
	GeneralInstructions string                      `json:"general_instructions"`
	BaseRole            string                      `json:"base_role"`
	CreatorID           int                         `json:"creator_id"`
	Collaborators       []*CopilotSpaceCollaborator `json:"collaborators"`
	Resources           []*CopilotSpaceResource     `json:"resources"`
	NextResourceID      int                         `json:"next_resource_id"`
	CreatedAt           time.Time                   `json:"created_at"`
	UpdatedAt           time.Time                   `json:"updated_at"`
}

// CopilotSpaceCollaborator grants a user or a team a role on a space.
type CopilotSpaceCollaborator struct {
	ActorType string `json:"actor_type"` // "User" | "Team"
	UserID    int    `json:"user_id"`    // set when ActorType == "User"
	TeamID    int    `json:"team_id"`    // set when ActorType == "Team"
	Role      string `json:"role"`       // reader | writer | admin
}

// CopilotSpaceResource is a resource attached to a space.
type CopilotSpaceResource struct {
	ID           int                    `json:"id"`
	ResourceType string                 `json:"resource_type"`
	Metadata     map[string]interface{} `json:"metadata"`
	CreatedAt    time.Time              `json:"created_at"`
	UpdatedAt    time.Time              `json:"updated_at"`
}

// CreateCopilotSpace creates a space for the owner, numbering it after
// the owner's highest existing space so numbers are never reused within
// an owner while IDs stay globally unique.
func (st *Store) CreateCopilotSpace(ownerType, ownerLogin string, creatorID int, name, description, instructions, baseRole string) *CopilotSpace {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	number := 0
	for _, sp := range st.CopilotSpaces {
		if sp.OwnerType == ownerType && sp.OwnerLogin == ownerLogin && sp.Number > number {
			number = sp.Number
		}
	}
	now := st.CurrentTime()
	space := &CopilotSpace{
		ID:                  st.NextCopilotSpaceID,
		Number:              number + 1,
		OwnerType:           ownerType,
		OwnerLogin:          ownerLogin,
		Name:                name,
		Description:         description,
		GeneralInstructions: instructions,
		BaseRole:            baseRole,
		CreatorID:           creatorID,
		Collaborators:       []*CopilotSpaceCollaborator{},
		Resources:           []*CopilotSpaceResource{},
		NextResourceID:      1,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	st.NextCopilotSpaceID++
	st.CopilotSpaces[space.ID] = space
	st.persistCopilotSpaceLocked(space)
	return space
}

func (st *Store) persistCopilotSpaceLocked(space *CopilotSpace) {
	if st.Persist != nil {
		st.Persist.MustPut("copilot_spaces", strconv.FormatInt(space.ID, 10), space)
	}
}

// SaveCopilotSpace bumps the space's UpdatedAt and persists it after
// the caller mutated its fields, collaborators, or resources.
func (st *Store) SaveCopilotSpace(space *CopilotSpace) {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	space.UpdatedAt = st.CurrentTime()
	// The caller mutated a detached snapshot from GetCopilotSpace; publish it as
	// the live row (copy-on-write) so the update is visible in memory, not just
	// persisted.
	st.CopilotSpaces[space.ID] = space
	st.persistCopilotSpaceLocked(space)
}

// cloneCopilotSpace deep-copies a space so a getter caller holds a snapshot
// detached from the stored row: its Collaborators and Resources slices (and each
// resource's Metadata map) are copied, so a reader can neither race SaveCopilotSpace's
// in-place UpdatedAt bump nor mutate the stored space's members/resources.
func cloneCopilotSpace(sp *CopilotSpace) *CopilotSpace {
	if sp == nil {
		return nil
	}
	clone := *sp
	if sp.Collaborators != nil {
		clone.Collaborators = make([]*CopilotSpaceCollaborator, len(sp.Collaborators))
		for i, c := range sp.Collaborators {
			cc := *c
			clone.Collaborators[i] = &cc
		}
	}
	if sp.Resources != nil {
		clone.Resources = make([]*CopilotSpaceResource, len(sp.Resources))
		for i, r := range sp.Resources {
			rc := *r
			if r.Metadata != nil {
				rc.Metadata = make(map[string]interface{}, len(r.Metadata))
				for k, v := range r.Metadata {
					rc.Metadata[k] = v
				}
			}
			clone.Resources[i] = &rc
		}
	}
	return &clone
}

// GetCopilotSpace returns the owner's space with the given number, or nil.
func (st *Store) GetCopilotSpace(ownerType, ownerLogin string, number int) *CopilotSpace {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	for _, sp := range st.CopilotSpaces {
		if sp.OwnerType == ownerType && sp.OwnerLogin == ownerLogin && sp.Number == number {
			return cloneCopilotSpace(sp)
		}
	}
	return nil
}

// ListCopilotSpaces returns the owner's spaces sorted by number.
func (st *Store) ListCopilotSpaces(ownerType, ownerLogin string) []*CopilotSpace {
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	var out []*CopilotSpace
	for _, sp := range st.CopilotSpaces {
		if sp.OwnerType == ownerType && sp.OwnerLogin == ownerLogin {
			out = append(out, cloneCopilotSpace(sp))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// DeleteCopilotSpace removes a space. Returns true if it existed.
func (st *Store) DeleteCopilotSpace(id int64) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if _, ok := st.CopilotSpaces[id]; !ok {
		return false
	}
	delete(st.CopilotSpaces, id)
	if st.Persist != nil {
		st.Persist.MustDelete("copilot_spaces", strconv.FormatInt(id, 10))
	}
	return true
}

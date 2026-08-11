package store

import "time"

// OrgCustomRepositoryRole is an organization-defined repository role.
type OrgCustomRepositoryRole struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	BaseRole    string    `json:"base_role"`
	Permissions []string  `json:"permissions"`
	OrgLogin    string    `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// OrgCustomOrganizationRole is an organization-defined organization role.
type OrgCustomOrganizationRole struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	BaseRole    *string   `json:"base_role"`
	Permissions []string  `json:"permissions"`
	OrgLogin    string    `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (st *Store) ReserveOrgCustomRoleIDLocked() int {
	id := st.NextOrgCustomRoleID
	if st.Persist != nil {
		reserved, err := st.Persist.AllocateCounterValue("next_org_custom_role_id", int64(id))
		if err != nil {
			panic(&PersistenceFailure{Op: "counter", Bucket: "counters", Key: "next_org_custom_role_id", Err: err})
		}
		id = int(reserved)
	}
	st.NextOrgCustomRoleID = id + 1
	return id
}

package store

import "time"

type OrgExternalIdentityGroup struct {
	ID          string    `json:"group_id"`
	NumericID   int       `json:"numeric_id"`
	Name        string    `json:"group_name"`
	Description string    `json:"group_description"`
	MemberIDs   []int     `json:"member_ids"`
	UpdatedAt   time.Time `json:"updated_at"`
}

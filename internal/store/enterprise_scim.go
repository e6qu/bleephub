package store

import "time"

type EnterpriseSCIMUser struct {
	Schemas     []string              `json:"schemas"`
	ID          string                `json:"id"`
	ExternalID  string                `json:"externalId,omitempty"`
	UserName    string                `json:"userName"`
	Name        EnterpriseSCIMName    `json:"name,omitempty"`
	DisplayName string                `json:"displayName,omitempty"`
	Active      bool                  `json:"active"`
	Emails      []EnterpriseSCIMEmail `json:"emails,omitempty"`
	UserID      int                   `json:"user_id"`
	CreatedAt   time.Time             `json:"created_at"`
	UpdatedAt   time.Time             `json:"updated_at"`
}

type EnterpriseSCIMGroup struct {
	Schemas     []string               `json:"schemas"`
	ID          string                 `json:"id"`
	ExternalID  string                 `json:"externalId,omitempty"`
	DisplayName string                 `json:"displayName"`
	Members     []EnterpriseSCIMMember `json:"members"`
	TeamID      int                    `json:"team_id"`
	CreatedAt   time.Time              `json:"created_at"`
	UpdatedAt   time.Time              `json:"updated_at"`
}

type EnterpriseSCIMEmail struct {
	Value   string `json:"value"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

type EnterpriseSCIMMember struct {
	Value   string `json:"value"`
	Display string `json:"display,omitempty"`
}

type EnterpriseSCIMName struct {
	GivenName  string `json:"givenName,omitempty"`
	FamilyName string `json:"familyName,omitempty"`
	Formatted  string `json:"formatted,omitempty"`
}

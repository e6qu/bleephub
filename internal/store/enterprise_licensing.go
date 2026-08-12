package store

import "time"

type VisualStudioSubscription struct {
	SubscriptionID string `json:"subscription_id"`
	Email          string `json:"email"`
	Username       string `json:"username,omitempty"`
	ManualMatch    bool   `json:"manual_match"`
}

type EnterpriseInnerSourceSyncJob struct {
	ID        string                            `json:"id"`
	Status    string                            `json:"status"`
	Processed int                               `json:"processed"`
	Created   int                               `json:"created"`
	Updated   int                               `json:"updated"`
	Withdrawn int                               `json:"withdrawn"`
	Errors    int                               `json:"errors"`
	Results   []EnterpriseInnerSourceSyncResult `json:"results"`
	CreatedAt time.Time                         `json:"created_at"`
	UpdatedAt time.Time                         `json:"updated_at"`
}

type EnterpriseInnerSourceSyncResult struct {
	ExternalID string `json:"external_id"`
	Status     string `json:"status"`
	GHSAID     string `json:"ghsa_id,omitempty"`
}

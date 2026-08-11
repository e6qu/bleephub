package store

import "time"

// EnterpriseCostCenter is a durable enhanced-billing cost allocation. A
// resource belongs to at most one active cost center; adding it elsewhere
// atomically reassigns it, matching GitHub's response contract.
type EnterpriseCostCenter struct {
	ID                  string                         `json:"id"`
	Name                string                         `json:"name"`
	State               string                         `json:"state"`
	Resources           []EnterpriseCostCenterResource `json:"resources"`
	AICreditPoolEnabled bool                           `json:"ai_credit_pool_enabled"`
	CreatedAt           time.Time                      `json:"created_at"`
	UpdatedAt           time.Time                      `json:"updated_at"`
}

// EnterpriseBillingReport records one asynchronous usage export request.
// Pending reports become completed when read, which gives clients a
// deterministic lifecycle without a wall-clock-dependent background task.
type EnterpriseBillingReport struct {
	ID           string    `json:"id"`
	ReportType   string    `json:"report_type"`
	StartDate    string    `json:"start_date"`
	EndDate      string    `json:"end_date"`
	Status       string    `json:"status"`
	DownloadURLs []string  `json:"download_urls,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	Actor        string    `json:"actor"`
}

func CloneBudget(b *OrgBudget) *OrgBudget {
	if b == nil {
		return nil
	}
	copy := *b
	copy.BudgetAlerting.AlertRecipients = append([]string(nil), b.BudgetAlerting.AlertRecipients...)
	return &copy
}

type EnterpriseCostCenterResource struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

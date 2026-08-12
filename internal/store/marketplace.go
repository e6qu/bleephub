package store

type MarketplaceBuyerAccount struct {
	Id          int    `json:"-"`
	Login       string `json:"-"`
	AccountType string `json:"-"`
}

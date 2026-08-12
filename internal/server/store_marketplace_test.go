package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func TestMarketplaceStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)
	persistence, err := store.NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewStore()
	if err := st.SetPersistence(persistence); err != nil {
		t.Fatal(err)
	}
	st.SeedDefaultUser()
	admin := st.LookupUserByLogin("admin")
	app, err := st.CreateAppE(admin.ID, "Persistent Marketplace App", "", map[string]string{"contents": "read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := fixedTestTime.UTC()
	listing := &store.MarketplaceListing{
		Slug: app.Slug, Name: app.Name, Description: "Persistent listing", SetupURL: "https://example.test/setup",
		GitHubAppID: app.ID, WebhookURL: "https://example.test/hook", WebhookContentType: "json", WebhookActive: true,
		Published: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.SaveMarketplaceListing(listing); err != nil {
		t.Fatal(err)
	}
	plan, err := st.CreateMarketplacePlan(&store.MarketplacePlan{
		ListingSlug: listing.Slug, Name: "Team", Description: "Persistent plan", PriceModel: "FLAT_RATE",
		MonthlyPriceInCents: 900, YearlyPriceInCents: 9000, State: "published",
	})
	if err != nil {
		t.Fatal(err)
	}
	nextBilling := now.AddDate(0, 1, 0)
	purchase := &store.MarketplacePurchase{
		ListingSlug: listing.Slug, AccountID: admin.ID, AccountType: "User", BillingCycle: "monthly",
		PlanID: plan.ID, PlanName: plan.Name, NextBillingDate: &nextBilling, UpdatedAt: &now,
		PendingChange: &store.MarketplacePendingChange{EffectiveDate: nextBilling, Cancellation: true, ActorID: admin.ID},
	}
	installation, created, err := st.CreateMarketplacePurchase(listing,
		store.MarketplaceBuyerAccount{Id: admin.ID, Login: admin.Login, AccountType: "User"}, purchase)
	if err != nil || !created || installation == nil {
		t.Fatalf("create persisted Marketplace purchase: installation=%#v created=%v err=%v", installation, created, err)
	}
	if err := persistence.Close(); err != nil {
		t.Fatal(err)
	}

	persistence, err = store.NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	defer persistence.Close()
	reloaded := store.NewStore()
	if err := reloaded.SetPersistence(persistence); err != nil {
		t.Fatal(err)
	}
	gotListing := reloaded.GetMarketplaceListing(listing.Slug)
	gotPlan := reloaded.GetMarketplacePlanForListing(listing.Slug, plan.ID)
	gotPurchase := reloaded.GetMarketplacePurchase(listing.Slug, "User", admin.ID)
	gotInstallation := reloaded.GetInstallation(installation.ID)
	if gotListing == nil || gotPlan == nil || gotPurchase == nil || gotInstallation == nil {
		t.Fatalf("reloaded Marketplace state: listing=%#v plan=%#v purchase=%#v installation=%#v", gotListing, gotPlan, gotPurchase, gotInstallation)
	}
	if gotPurchase.PendingChange == nil || !gotPurchase.PendingChange.Cancellation || gotPurchase.InstallationID == nil || *gotPurchase.InstallationID != installation.ID {
		t.Fatalf("reloaded Marketplace purchase = %#v", gotPurchase)
	}
}

func TestMarketplacePurchaseStorageFailureLeavesNoInstallationOrPurchase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLEEPHUB_PERSIST", "true")
	t.Setenv("BLEEPHUB_DATA_DIR", dir)
	persistence, err := store.NewPersistence()
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewStore()
	if err := st.SetPersistence(persistence); err != nil {
		t.Fatal(err)
	}
	st.SeedDefaultUser()
	admin := st.LookupUserByLogin("admin")
	app, err := st.CreateAppE(admin.ID, "Atomic Marketplace App", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := fixedTestTime.UTC()
	listing := &store.MarketplaceListing{Slug: app.Slug, Name: app.Name, Description: "Atomic listing", GitHubAppID: app.ID, CreatedAt: now, UpdatedAt: now}
	if err := st.SaveMarketplaceListing(listing); err != nil {
		t.Fatal(err)
	}
	plan, err := st.CreateMarketplacePlan(&store.MarketplacePlan{ListingSlug: listing.Slug, Name: "Free", PriceModel: "FREE", State: "published"})
	if err != nil {
		t.Fatal(err)
	}
	if err := persistence.Close(); err != nil {
		t.Fatal(err)
	}

	purchase := &store.MarketplacePurchase{ListingSlug: listing.Slug, AccountID: admin.ID, AccountType: "User", BillingCycle: "monthly", PlanID: plan.ID, PlanName: plan.Name, UpdatedAt: &now}
	if _, _, err := st.CreateMarketplacePurchase(listing, store.MarketplaceBuyerAccount{Id: admin.ID, Login: admin.Login, AccountType: "User"}, purchase); err == nil {
		t.Fatal("Marketplace purchase unexpectedly succeeded after durable storage closed")
	}
	if st.GetMarketplacePurchase(listing.Slug, "User", admin.ID) != nil {
		t.Fatal("failed Marketplace purchase mutated subscription memory")
	}
	if got := st.ListAppInstallations(app.ID); len(got) != 0 {
		t.Fatalf("failed Marketplace purchase left installations: %#v", got)
	}
}

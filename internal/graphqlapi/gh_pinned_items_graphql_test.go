package graphqlapi

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestProfilePinnedItemsReportTheOwnersChoice: pins come back in the owner's
// order, the pinnable set spans repos and gists, and the remaining count counts
// down from GitHub's limit of six.
func TestProfilePinnedItemsReportTheOwnersChoice(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.store.UsersByLogin["admin"]
	first := h.store.CreateRepo(owner, "alpha", "", false)
	second := h.store.CreateRepo(owner, "beta", "", false)
	if first == nil || second == nil {
		t.Fatal("repositories not created")
	}
	if gist, err := h.store.CreateGistE(owner, "snippet", true, map[string]*store.GistFile{
		"a.txt": {Filename: "a.txt", Content: "hello"},
	}); err != nil || gist == nil {
		t.Fatalf("gist not created: %v", err)
	}
	// The owner pinned beta first, so beta must come first.
	if pinned, ok := h.store.SetPinnedRepos(owner.ID, []string{"admin/beta", "admin/alpha"}); !ok || len(pinned) != 2 {
		t.Fatalf("pins not recorded: %#v", pinned)
	}

	data := h.query(owner, `{
	  user(login:"admin") {
	    pinnedItems(first:10) { totalCount nodes { ... on Repository { nameWithOwner } } }
	    pinnableItems(first:10) {
	      totalCount
	      nodes { ... on Repository { nameWithOwner } ... on Gist { name } }
	    }
	    pinnableRepositoriesOnly: pinnableItems(first:10, types:[REPOSITORY]) { totalCount }
	    anyPinnableItems(type: GIST)
	    pinnedItemsRemaining
	    viewerCanChangePinnedItems
	    itemShowcase { hasPinnedItems items(first:10) { totalCount } }
	  }
	}`, nil)

	order := []string{}
	for _, raw := range at(t, data, "user", "pinnedItems", "nodes").([]interface{}) {
		node, _ := raw.(map[string]interface{})
		name, _ := node["nameWithOwner"].(string)
		order = append(order, name)
	}
	if len(order) != 2 || order[0] != "admin/beta" || order[1] != "admin/alpha" {
		t.Errorf("pinnedItems = %#v, want the owner's order [admin/beta admin/alpha]", order)
	}
	if got := at(t, data, "user", "pinnableItems", "totalCount"); got != float64(3) {
		t.Errorf("pinnableItems totalCount = %v, want the two repositories plus the gist", got)
	}
	if got := at(t, data, "user", "pinnableRepositoriesOnly", "totalCount"); got != float64(2) {
		t.Errorf("pinnableItems(types:[REPOSITORY]) totalCount = %v, want 2", got)
	}
	if got := at(t, data, "user", "anyPinnableItems"); got != true {
		t.Errorf("anyPinnableItems(type:GIST) = %v, want true", got)
	}
	if got := at(t, data, "user", "pinnedItemsRemaining"); got != float64(store.MaxPinnedRepos-2) {
		t.Errorf("pinnedItemsRemaining = %v, want %d", got, store.MaxPinnedRepos-2)
	}
	if got := at(t, data, "user", "viewerCanChangePinnedItems"); got != true {
		t.Errorf("owner viewerCanChangePinnedItems = %v, want true", got)
	}
	if got := at(t, data, "user", "itemShowcase", "hasPinnedItems"); got != true {
		t.Errorf("itemShowcase.hasPinnedItems = %v, want true", got)
	}
	if got := at(t, data, "user", "itemShowcase", "items", "totalCount"); got != float64(2) {
		t.Errorf("itemShowcase.items totalCount = %v, want 2", got)
	}
}

// TestPinnedItemsHidePrivateContentAndAdminRights: a pinned private repository
// and a secret gist stay hidden through a profile, and only the account itself
// may change its pins.
func TestPinnedItemsHidePrivateContentAndAdminRights(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.store.UsersByLogin["admin"]
	if h.store.CreateRepo(owner, "public-pin", "", false) == nil {
		t.Fatal("public repository not created")
	}
	if h.store.CreateRepo(owner, "private-pin", "", true) == nil {
		t.Fatal("private repository not created")
	}
	if gist, err := h.store.CreateGistE(owner, "secret", false, map[string]*store.GistFile{
		"s.txt": {Filename: "s.txt", Content: "shh"},
	}); err != nil || gist == nil {
		t.Fatalf("secret gist not created: %v", err)
	}
	if pinned, ok := h.store.SetPinnedRepos(owner.ID, []string{"admin/private-pin", "admin/public-pin"}); !ok || len(pinned) != 2 {
		t.Fatalf("pins not recorded: %#v", pinned)
	}

	document := `{
	  user(login:"admin") {
	    pinnedItems(first:10) { totalCount nodes { ... on Repository { nameWithOwner } } }
	    pinnableItems(first:10) { totalCount }
	    viewerCanChangePinnedItems
	  }
	}`

	ownerView := h.query(owner, document, nil)
	if got := at(t, ownerView, "user", "pinnedItems", "totalCount"); got != float64(2) {
		t.Errorf("owner pinnedItems totalCount = %v, want both pins", got)
	}

	strangerView := h.query(h.user("browser"), document, nil)
	names := []string{}
	for _, raw := range at(t, strangerView, "user", "pinnedItems", "nodes").([]interface{}) {
		node, _ := raw.(map[string]interface{})
		name, _ := node["nameWithOwner"].(string)
		names = append(names, name)
	}
	if len(names) != 1 || names[0] != "admin/public-pin" {
		t.Errorf("stranger pinnedItems = %#v, want only the public repository", names)
	}
	if got := at(t, strangerView, "user", "pinnableItems", "totalCount"); got != float64(1) {
		t.Errorf("stranger pinnableItems totalCount = %v, want only the public repository", got)
	}
	if got := at(t, strangerView, "user", "viewerCanChangePinnedItems"); got != false {
		t.Errorf("stranger viewerCanChangePinnedItems = %v, want false", got)
	}
}

// TestOrganizationPinnedItemsFollowOwnership: an org's pins come from the
// organization row, and only an owner may change them.
func TestOrganizationPinnedItemsFollowOwnership(t *testing.T) {
	h := newAccountHarness(t)
	admin := h.store.UsersByLogin["admin"]
	org := h.store.CreateOrg(admin, "pinorg", "Pin Org", "")
	if org == nil {
		t.Fatal("organization not created")
	}
	if h.store.CreateOrgRepo(org, admin, "showcase", "", false) == nil {
		t.Fatal("organization repository not created")
	}
	if pinned, ok := h.store.SetOrgPinnedRepos("pinorg", []string{"pinorg/showcase"}); !ok || len(pinned) != 1 {
		t.Fatalf("organization pins not recorded: %#v", pinned)
	}

	document := `{
	  organization(login:"pinorg") {
	    pinnedItems(first:10) { totalCount nodes { ... on Repository { nameWithOwner } } }
	    itemShowcase { hasPinnedItems }
	    viewerCanChangePinnedItems
	  }
	}`

	ownerView := h.query(admin, document, nil)
	if got := at(t, ownerView, "organization", "pinnedItems", "totalCount"); got != float64(1) {
		t.Errorf("owner pinnedItems totalCount = %v, want 1", got)
	}
	if got := at(t, ownerView, "organization", "viewerCanChangePinnedItems"); got != true {
		t.Errorf("owner viewerCanChangePinnedItems = %v, want true", got)
	}

	strangerView := h.query(h.user("pinstranger"), document, nil)
	if got := at(t, strangerView, "organization", "pinnedItems", "totalCount"); got != float64(1) {
		t.Errorf("stranger pinnedItems totalCount = %v, want the public pin", got)
	}
	if got := at(t, strangerView, "organization", "viewerCanChangePinnedItems"); got != false {
		t.Errorf("stranger viewerCanChangePinnedItems = %v, want false", got)
	}
}

package bleephub

import "testing"

// STORE-028: deleting an organization must cascade its directly-owned packages
// (reclaiming their file bytes) and cancel its Marketplace purchases, not just
// its repositories/teams/memberships.
func TestDeleteOrgCascadesPackagesAndMarketplace(t *testing.T) {
	st := NewStore()
	bs := &flakyByteStore{blobs: map[string][]byte{}, failOn: 0}
	st.ObjectByteStore = bs
	st.SeedDefaultUser()
	admin := st.LookupUserByLogin("admin")
	org := st.CreateOrg(admin, "acme", "Acme", "")
	if org == nil {
		t.Fatal("CreateOrg returned nil")
	}

	// An npm package owned directly by the org (not by a repo), with one version
	// and one file whose bytes live in the object store.
	st.Mu.Lock()
	pkg := &Package{ID: 1, Name: "lib", PackageType: "npm", OwnerType: "Organization", OwnerKey: "acme"}
	st.Packages[1] = pkg
	st.PackagesByOwnerKey["acme"] = map[string]*Package{packageKey("npm", "lib"): pkg}
	ver := &PackageVersion{ID: 10, PackageID: 1}
	st.PackageVersions[10] = ver
	st.PackageVersionsByPackage[1] = map[int]*PackageVersion{10: ver}
	file := &PackageFile{ID: 100, VersionID: 10, StoragePath: "objkey-1"}
	st.PackageFiles[100] = file
	st.PackageFilesByVersion[10] = map[int]*PackageFile{100: file}
	st.Mu.Unlock()
	bs.blobs["objkey-1"] = []byte("payload")

	// A Marketplace purchase held by the org.
	mkey := marketplacePurchaseKey("plan-slug", "Organization", org.ID)
	st.Misc.Mu.Lock()
	st.Misc.MarketplacePurchases[mkey] = &MarketplacePurchase{ListingSlug: "plan-slug", AccountType: "Organization", AccountID: org.ID}
	st.Misc.Mu.Unlock()

	if !st.DeleteOrg("acme") {
		t.Fatal("DeleteOrg returned false")
	}

	st.Mu.RLock()
	_, pkgAlive := st.Packages[1]
	ownerIndex := st.PackagesByOwnerKey["acme"]
	_, verAlive := st.PackageVersions[10]
	_, fileAlive := st.PackageFiles[100]
	st.Mu.RUnlock()
	if pkgAlive {
		t.Error("org-owned package row survived org deletion")
	}
	if ownerIndex != nil {
		t.Error("PackagesByOwnerKey entry for the org was not cleared")
	}
	if verAlive {
		t.Error("package version survived org deletion")
	}
	if fileAlive {
		t.Error("package file survived org deletion")
	}
	if _, ok := bs.blobs["objkey-1"]; ok {
		t.Error("package file bytes were not reclaimed from the object store")
	}

	st.Misc.Mu.RLock()
	_, purchaseAlive := st.Misc.MarketplacePurchases[mkey]
	st.Misc.Mu.RUnlock()
	if purchaseAlive {
		t.Error("org Marketplace purchase was not cancelled")
	}
}

func TestDeleteUserCascadesOwnedResources(t *testing.T) {
	st := NewStore()
	bs := &flakyByteStore{blobs: map[string][]byte{}, failOn: 0}
	st.ObjectByteStore = bs

	user := &User{ID: 2, Login: "bob", Type: "User"}
	st.Mu.Lock()
	st.Users[2] = user
	st.UsersByLogin["bob"] = user
	// A package owned directly by the user, with a version + file in the store.
	pkg := &Package{ID: 1, Name: "cli", PackageType: "npm", OwnerType: "User", OwnerKey: "bob"}
	st.Packages[1] = pkg
	st.PackagesByOwnerKey["bob"] = map[string]*Package{packageKey("npm", "cli"): pkg}
	ver := &PackageVersion{ID: 10, PackageID: 1}
	st.PackageVersions[10] = ver
	st.PackageVersionsByPackage[1] = map[int]*PackageVersion{10: ver}
	file := &PackageFile{ID: 100, VersionID: 10, StoragePath: "user-objkey"}
	st.PackageFiles[100] = file
	st.PackageFilesByVersion[10] = map[int]*PackageFile{100: file}
	st.Memberships["9:2"] = &Membership{OrgID: 9, UserID: 2, State: MembershipStateActive}
	st.Mu.Unlock()
	bs.blobs["user-objkey"] = []byte("payload")
	mkey := marketplacePurchaseKey("plan-slug", "User", 2)
	st.Misc.Mu.Lock()
	st.Misc.MarketplacePurchases[mkey] = &MarketplacePurchase{ListingSlug: "plan-slug", AccountType: "User", AccountID: 2}
	st.Misc.Mu.Unlock()

	st.Mu.Lock()
	_, userIntent, err := st.DeleteUserOwnedResourcesLocked(user)
	st.Mu.Unlock()
	if err != nil {
		t.Fatalf("deleteUserOwnedResourcesLocked: %v", err)
	}
	if err := st.CleanupDeletedRepo(userIntent); err != nil {
		t.Fatalf("cleanupDeletedRepo: %v", err)
	}

	st.Mu.RLock()
	_, pkgAlive := st.Packages[1]
	ownerIndex := st.PackagesByOwnerKey["bob"]
	_, memAlive := st.Memberships["9:2"]
	st.Mu.RUnlock()
	if pkgAlive {
		t.Error("user-owned package row survived user deletion")
	}
	if ownerIndex != nil {
		t.Error("PackagesByOwnerKey entry for the user was not cleared")
	}
	if memAlive {
		t.Error("user's org membership survived user deletion")
	}
	if _, ok := bs.blobs["user-objkey"]; ok {
		t.Error("user package bytes were not reclaimed from the object store")
	}
	st.Misc.Mu.RLock()
	_, purchaseAlive := st.Misc.MarketplacePurchases[mkey]
	st.Misc.Mu.RUnlock()
	if purchaseAlive {
		t.Error("user Marketplace purchase was not cancelled")
	}
}

package store

import "testing"

// TestSecretScanningPushProtectionHonorsRepoToggle pins that the repository's own
// push-protection toggle actually enables protection — for user- and org-owned
// repos alike. It was previously ignored (only an org per-pattern config counted),
// so a repo with push protection enabled did not block secrets on push.
func TestSecretScanningPushProtectionHonorsRepoToggle(t *testing.T) {
	st := NewStore()

	userOn := &Repo{ID: 1, OwnerType: "User", OwnerID: 1, SecretScanningPushProtectionEnabled: true}
	if !st.SecretScanningPushProtectionEnabled(userOn, "ghp") {
		t.Fatal("a user repo with the push-protection toggle must be protected")
	}
	orgOn := &Repo{ID: 2, OwnerType: "Organization", OwnerID: 5, SecretScanningPushProtectionEnabled: true}
	if !st.SecretScanningPushProtectionEnabled(orgOn, "ghp") {
		t.Fatal("an org repo with the push-protection toggle must be protected")
	}
	off := &Repo{ID: 3, OwnerType: "User", OwnerID: 1}
	if st.SecretScanningPushProtectionEnabled(off, "ghp") {
		t.Fatal("a repo without the toggle must not be protected")
	}
}

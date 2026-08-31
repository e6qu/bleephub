package bleephub

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/e6qu/bleephub/internal/store"
)

// TestOrgMembershipHiddenFromNonMember pins the fix for the membership-read
// leak: GET /orgs/{org}/memberships/{username} passed a plain
// requirePerm(Members:read) gate, which never checks that the caller belongs to
// the org — so any read-scoped token was an oracle for another org's private
// membership (role + state). A non-member must get a 404, exactly like the
// sibling member-check and team-membership endpoints.
func TestOrgMembershipHiddenFromNonMember(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	srv.createOrg(t, "leak-org")
	outsider, _ := srv.newUser(t, "leak-outsider")
	outsiderToken := srv.store.CreateToken(outsider.ID, "read:org").Value

	// A non-member's read-scoped token must not learn the owner's membership.
	resp := srv.get(t, "/api/v3/orgs/leak-org/memberships/admin", outsiderToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("non-member membership read = %d, want 404 (must not leak)", resp.StatusCode)
	}

	// The org owner still reads memberships normally.
	ok := srv.get(t, "/api/v3/orgs/leak-org/memberships/admin", defaultToken)
	if ok.StatusCode != http.StatusOK {
		ok.Body.Close()
		t.Fatalf("owner membership read = %d, want 200", ok.StatusCode)
	}
	m := decodeJSON(t, ok)
	if m["state"] != "active" {
		t.Fatalf("owner membership state = %v, want active", m["state"])
	}
}

// TestCompareReportsBaseSHAForRemovedFile pins the fix for a removed file's blob
// sha in the compare response. plumbing.Hash is a [20]byte, so ch.To.Hash.String()
// is always 40 hex chars — never "" — so the old `if sha == ""` fallback never
// fired and a deleted file reported 40 zeros instead of its base-side blob sha.
func TestCompareReportsBaseSHAForRemovedFile(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "compare-removed"})
	stor := s.store.GetGitStorage("admin", "compare-removed")
	if stor == nil {
		t.Fatal("no git storage")
	}
	sig := repoSignature("t", "t@t")
	base, err := initRepoWithFiles(stor, "main", "init", map[string]string{
		"README.md":  "# hello",
		"doomed.txt": "delete me\n",
	}, sig)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	if err := stor.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), base)); err != nil {
		t.Fatalf("set feature ref: %v", err)
	}
	if _, err := deleteFileCommit(stor, "feature", "doomed.txt", "remove doomed", sig, base, nil); err != nil {
		t.Fatalf("delete commit: %v", err)
	}

	// Compute the base-side blob sha of the removed file for an exact assertion.
	baseCommit, err := object.GetCommit(stor, base)
	if err != nil {
		t.Fatalf("base commit: %v", err)
	}
	baseTree, err := baseCommit.Tree()
	if err != nil {
		t.Fatalf("base tree: %v", err)
	}
	entry, err := baseTree.FindEntry("doomed.txt")
	if err != nil {
		t.Fatalf("find doomed.txt: %v", err)
	}
	wantSHA := entry.Hash.String()

	resp := s.get(t, "/api/v3/repos/admin/compare-removed/compare/main...feature", defaultToken)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("compare = %d, want 200", resp.StatusCode)
	}
	var got map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	files, _ := got["files"].([]interface{})
	var removed map[string]interface{}
	for _, f := range files {
		fm := f.(map[string]interface{})
		if fm["filename"] == "doomed.txt" {
			removed = fm
		}
	}
	if removed == nil {
		t.Fatalf("removed file not present in compare files: %v", files)
	}
	if removed["status"] != "removed" {
		t.Fatalf("doomed.txt status = %v, want removed", removed["status"])
	}
	sha, _ := removed["sha"].(string)
	if sha == strings.Repeat("0", 40) {
		t.Fatal("removed file reported the 40-zero hash instead of its base blob sha")
	}
	if sha != wantSHA {
		t.Fatalf("removed file sha = %q, want base blob sha %q", sha, wantSHA)
	}
}

// TestSCIMSingleUserGETAllowsReadScope pins the fix that the single-user SCIM
// GET reads (Members:read), matching the collection GET and GitHub. It was
// over-gated on Members:write, so a read:org token was refused.
func TestSCIMSingleUserGETAllowsReadScope(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	srv.createOrg(t, "scim-scope-org")
	base := "/api/v3/scim/v2/organizations/scim-scope-org/Users"
	created := srv.post(t, base, defaultToken, map[string]interface{}{
		"schemas":    []string{scimUserSchema},
		"externalId": "dir-1",
		"userName":   "scim.scope.user",
		"active":     true,
		"emails":     []map[string]interface{}{{"value": "scim.scope.user@example.test", "primary": true}},
	})
	if created.StatusCode != http.StatusCreated {
		created.Body.Close()
		t.Fatalf("create SCIM user = %d", created.StatusCode)
	}
	user := decodeJSON(t, created)
	id := user["id"].(string)

	// The org owner's read-only token (read:org grants Members:read, not write)
	// must be able to GET the single SCIM user.
	admin := srv.store.LookupUserByLogin("admin")
	readToken := srv.store.CreateToken(admin.ID, "read:org").Value
	resp := srv.get(t, base+"/"+id, readToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("single SCIM user GET with read:org = %d, want 200", resp.StatusCode)
	}
}

// TestReapExpiredRateLimitWindows pins the fix for the unbounded s.rateLimits
// map: without a reaper it accumulated one permanent entry per
// identity×resource forever (every token/session/IP a scanner sprayed leaked
// memory). Past-reset and nil windows must be deleted; live windows kept.
func TestReapExpiredRateLimitWindows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	server := &Server{rateLimits: map[string]*apiRateWindow{
		"live\x1fcore":    {Limit: 5000, Used: 3, Reset: now.Add(time.Hour)},
		"expired\x1fcore": {Limit: 5000, Used: 9, Reset: now.Add(-time.Minute)},
		"exact\x1fcore":   {Limit: 5000, Used: 1, Reset: now}, // reset==now → no longer before → reaped
		"nilwin\x1fcore":  nil,
	}}
	server.reapExpiredRateLimitWindows(now)
	if _, ok := server.rateLimits["live\x1fcore"]; !ok {
		t.Fatal("a live window was reaped")
	}
	for _, k := range []string{"expired\x1fcore", "exact\x1fcore", "nilwin\x1fcore"} {
		if _, ok := server.rateLimits[k]; ok {
			t.Errorf("expired/nil window %q survived the reap", k)
		}
	}
	if len(server.rateLimits) != 1 {
		t.Fatalf("windows remaining = %d, want 1", len(server.rateLimits))
	}
}

// TestRateLimitBucketsAreIndependent pins the fix that dependency_sbom and
// copilot_usage_records are reported buckets in their own right. Absent from
// apiRateResourceLimits they collapsed onto the core key, so /rate_limit
// reported their used/remaining bleeding from unrelated core traffic.
func TestRateLimitBucketsAreIndependent(t *testing.T) {
	t.Parallel()
	for _, res := range []string{"dependency_sbom", "copilot_usage_records"} {
		if _, ok := apiRateResourceLimits[res]; !ok {
			t.Errorf("%q missing from apiRateResourceLimits — collapses onto the core window", res)
		}
	}
}

// TestReapExpiredEnterpriseCopilotSeats pins the fix that expired enterprise
// Copilot seats are deleted AND persisted on the background tick, so the read
// path no longer has to persist (STORE-034). Previously the GET expired seats
// in memory without persisting, so they reappeared after a restart.
func TestReapExpiredEnterpriseCopilotSeats(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	now := srv.store.CurrentTime()
	yesterday := now.Add(-24 * time.Hour).Format("2006-01-02")
	tomorrow := now.Add(24 * time.Hour).Format("2006-01-02")

	srv.store.Mu.Lock()
	if srv.store.EnterpriseSettings.EnterpriseCopilotSeats == nil {
		srv.store.EnterpriseSettings.EnterpriseCopilotSeats = map[string]*store.CopilotSeat{}
	}
	srv.store.EnterpriseSettings.EnterpriseCopilotSeats["user:1"] = &store.CopilotSeat{
		UserID: 1, PendingCancellationDate: yesterday, CreatedAt: now, UpdatedAt: now,
	}
	srv.store.EnterpriseSettings.EnterpriseCopilotSeats["user:2"] = &store.CopilotSeat{
		UserID: 2, PendingCancellationDate: tomorrow, CreatedAt: now, UpdatedAt: now,
	}
	srv.store.EnterpriseSettings.EnterpriseCopilotSeats["user:3"] = &store.CopilotSeat{
		UserID: 3, CreatedAt: now, UpdatedAt: now,
	}
	srv.store.Mu.Unlock()

	srv.reapExpiredEnterpriseCopilotSeats(now)

	srv.store.Mu.Lock()
	defer srv.store.Mu.Unlock()
	seats := srv.store.EnterpriseSettings.EnterpriseCopilotSeats
	if _, ok := seats["user:1"]; ok {
		t.Error("seat whose cancellation took effect yesterday was not reaped")
	}
	if _, ok := seats["user:2"]; !ok {
		t.Error("seat with a future cancellation date was wrongly reaped")
	}
	if _, ok := seats["user:3"]; !ok {
		t.Error("active seat with no cancellation was wrongly reaped")
	}
}

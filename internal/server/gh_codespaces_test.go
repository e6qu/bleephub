package bleephub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

const codespaceTestImage = "alpine:latest"

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func TestCodespaces_WorkspaceRuntimeWithoutDockerCLI(t *testing.T) {
	// No t.Parallel(): t.Setenv("PATH", …) below cannot run in a parallel test.
	s := newIsolatedServer(t)
	repo := s.createTestCodespaceRepo(t, "cs-workspace-runtime")
	t.Setenv("PATH", t.TempDir())

	resp := s.post(t, "/api/v3/user/codespaces", defaultToken, map[string]any{
		"repository_id": repo.ID,
		"machine":       "basicLinux32",
		"display_name":  "Workspace Codespace",
	})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create without Docker = %d %s, want 201", resp.StatusCode, body)
	}
	created := decodeJSON(t, resp)
	name := created["name"].(string)
	cs := s.store.GetCodespaceByName(name)
	t.Cleanup(func() {
		if live := s.store.GetCodespaceByName(name); live != nil {
			_, _ = s.store.DeleteCodespace(live.ID)
		}
	})
	if created["state"] != "Available" || cs == nil || cs.Runtime != "workspace" || cs.ContainerID != "" {
		t.Fatalf("workspace codespace = %+v", cs)
	}

	resp = s.post(t, "/api/v3/user/codespaces/"+name+"/stop", defaultToken, map[string]any{})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("stop workspace codespace = %d %s, want 200", resp.StatusCode, body)
	}
	_ = decodeJSON(t, resp)
	if state := s.store.RefreshCodespaceState(cs.ID); state != "Shutdown" {
		t.Fatalf("state after stop = %q, want Shutdown", state)
	}

	resp = s.post(t, "/api/v3/user/codespaces/"+name+"/start", defaultToken, map[string]any{})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("start workspace codespace = %d %s, want 200", resp.StatusCode, body)
	}
	_ = decodeJSON(t, resp)
	if state := s.store.RefreshCodespaceState(cs.ID); state != "Available" {
		t.Fatalf("state after start = %q, want Available", state)
	}
}

func TestCodespaces_UserCreateListGetDelete(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestCodespaceRepo(t, "cs-user-repo")

	// Create via user endpoint.
	resp := s.post(t, "/api/v3/user/codespaces", defaultToken, map[string]any{
		"repository_id": repo.ID,
		"machine":       "basicLinux32",
		"display_name":  "User Codespace",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create user codespace: %d %s", resp.StatusCode, b)
	}
	created := decodeJSON(t, resp)
	name := created["name"].(string)
	t.Cleanup(func() {
		if cs := s.store.GetCodespaceByName(name); cs != nil {
			_, _ = s.store.DeleteCodespace(cs.ID)
		}
		s.cleanupCodespaceContainer(t, name)
	})
	if created["state"] != "Available" {
		t.Fatalf("created state = %v, want Available", created["state"])
	}
	if created["display_name"] != "User Codespace" {
		t.Fatalf("unexpected display_name: %v", created["display_name"])
	}

	// List user codespaces.
	resp = s.get(t, "/api/v3/user/codespaces", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list user codespaces: %d %s", resp.StatusCode, b)
	}
	var listResp struct {
		Codespaces []map[string]any `json:"codespaces"`
		TotalCount int              `json:"total_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	resp.Body.Close()
	found := false
	for _, cs := range listResp.Codespaces {
		if cs["name"].(string) == name {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("created codespace not in list: %v", listResp.Codespaces)
	}

	// Get user codespace.
	resp = s.get(t, "/api/v3/user/codespaces/"+name, defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("get user codespace: %d %s", resp.StatusCode, b)
	}
	got := decodeJSON(t, resp)
	if got["name"].(string) != name {
		t.Fatalf("unexpected name: %v", got["name"])
	}

	// Patch.
	resp = s.patch(t, "/api/v3/user/codespaces/"+name, defaultToken, map[string]any{
		"display_name": "Renamed",
	})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("patch user codespace: %d %s", resp.StatusCode, b)
	}
	patched := decodeJSON(t, resp)
	if patched["display_name"] != "Renamed" {
		t.Fatalf("patch did not update display_name: %v", patched["display_name"])
	}

	// Delete.
	resp = s.delete(t, "/api/v3/user/codespaces/"+name, defaultToken)
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("delete user codespace: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	// Ensure container removed.
	ctx, cancel := contextWithTimeout(10 * time.Second)
	defer cancel()
	if out, _ := store.RunDockerCLI(ctx, "ps", "-a", "--filter", "name="+store.CodespaceContainerName(name), "--format", "{{.Names}}"); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("container still exists after delete")
	}
}

func TestCodespaces_RepoCreateStartStopDelete(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestCodespaceRepo(t, "cs-repo-repo")

	resp := s.post(t, fmt.Sprintf("/api/v3/repos/%s/codespaces", repo.FullName), defaultToken, map[string]any{
		"machine":      "basicLinux32",
		"display_name": "Repo Codespace",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create repo codespace: %d %s", resp.StatusCode, b)
	}
	created := decodeJSON(t, resp)
	name := created["name"].(string)
	t.Cleanup(func() {
		if cs := s.store.GetCodespaceByName(name); cs != nil {
			_, _ = s.store.DeleteCodespace(cs.ID)
		}
		s.cleanupCodespaceContainer(t, name)
	})

	// Individual codespaces are addressed through the user-scoped operations,
	// including when they were created from a repository.
	resp = s.post(t, fmt.Sprintf("/api/v3/user/codespaces/%s/start", name), defaultToken, nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("start repo codespace: %d %s", resp.StatusCode, b)
	}
	started := decodeJSON(t, resp)
	resp.Body.Close()
	if started["state"] != "Available" {
		t.Fatalf("start state = %v, want Available", started["state"])
	}

	resp = s.post(t, fmt.Sprintf("/api/v3/user/codespaces/%s/stop", name), defaultToken, nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("stop repo codespace: %d %s", resp.StatusCode, b)
	}
	stopped := decodeJSON(t, resp)
	resp.Body.Close()
	if stopped["state"] != "Shutdown" {
		t.Fatalf("stop state = %v, want Shutdown", stopped["state"])
	}

	// Delete.
	resp = s.delete(t, fmt.Sprintf("/api/v3/user/codespaces/%s", name), defaultToken)
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("delete repo codespace: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

func TestCodespaces_MachinesList(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestCodespaceRepo(t, "cs-machines-repo")
	resp := s.get(t, fmt.Sprintf("/api/v3/repos/%s/codespaces/machines", repo.FullName), defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list machines: %d %s", resp.StatusCode, b)
	}
	var m struct {
		Machines   []map[string]any `json:"machines"`
		TotalCount int              `json:"total_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode machines: %v", err)
	}
	resp.Body.Close()
	if m.TotalCount == 0 {
		t.Fatal("expected machines")
	}
}

func TestCodespaces_UserSecretsCRUD(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	// Fetch public key.
	resp := s.get(t, "/api/v3/user/codespaces/secrets/public-key", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("get public key: %d %s", resp.StatusCode, b)
	}
	pk := decodeJSON(t, resp)
	keyID := pk["key_id"].(string)

	// Encrypt a dummy value.
	plain := "secret-value"
	enc, _, err := s.store.SealSecretValue(plain)
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}

	// Put secret.
	resp = s.put(t, "/api/v3/user/codespaces/secrets/MY_SECRET", defaultToken, map[string]any{
		"encrypted_value": enc,
		"key_id":          keyID,
	})
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("put user secret: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	// List.
	resp = s.get(t, "/api/v3/user/codespaces/secrets", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list user secrets: %d %s", resp.StatusCode, b)
	}
	var listResp struct {
		Secrets    []map[string]any `json:"secrets"`
		TotalCount int              `json:"total_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode secrets: %v", err)
	}
	resp.Body.Close()
	found := false
	for _, s := range listResp.Secrets {
		if s["name"].(string) == "MY_SECRET" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("secret not in list")
	}

	// Get.
	resp = s.get(t, "/api/v3/user/codespaces/secrets/MY_SECRET", defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("get user secret: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	// Delete.
	resp = s.delete(t, "/api/v3/user/codespaces/secrets/MY_SECRET", defaultToken)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("delete user secret: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

func TestCodespaces_RepoSecretsCRUD(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createTestCodespaceRepo(t, "cs-repo-secrets")
	resp := s.get(t, fmt.Sprintf("/api/v3/repos/%s/codespaces/secrets/public-key", repo.FullName), defaultToken)
	pk := decodeJSON(t, resp)
	keyID := pk["key_id"].(string)
	enc, _, _ := s.store.SealSecretValue("repo-secret")

	resp = s.put(t, fmt.Sprintf("/api/v3/repos/%s/codespaces/secrets/REPO_SECRET", repo.FullName), defaultToken, map[string]any{
		"encrypted_value": enc,
		"key_id":          keyID,
	})
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("put repo secret: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	resp = s.get(t, fmt.Sprintf("/api/v3/repos/%s/codespaces/secrets/REPO_SECRET", repo.FullName), defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("get repo secret: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	resp = s.delete(t, fmt.Sprintf("/api/v3/repos/%s/codespaces/secrets/REPO_SECRET", repo.FullName), defaultToken)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("delete repo secret: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

func TestCodespaces_OrgSecretsCRUD(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "cs-secrets-org", "Codespaces Secrets Org", "")

	resp := s.get(t, fmt.Sprintf("/api/v3/orgs/%s/codespaces/secrets/public-key", org.Login), defaultToken)
	pk := decodeJSON(t, resp)
	keyID := pk["key_id"].(string)
	enc, _, _ := s.store.SealSecretValue("org-secret")

	resp = s.put(t, fmt.Sprintf("/api/v3/orgs/%s/codespaces/secrets/ORG_SECRET", org.Login), defaultToken, map[string]any{
		"encrypted_value": enc,
		"key_id":          keyID,
		"visibility":      "all",
	})
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("put org secret: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	resp = s.get(t, fmt.Sprintf("/api/v3/orgs/%s/codespaces/secrets/ORG_SECRET", org.Login), defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("get org secret: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	resp = s.get(t, fmt.Sprintf("/api/v3/orgs/%s/codespaces/secrets/ORG_SECRET/repositories", org.Login), defaultToken)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list org secret repos: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	resp = s.delete(t, fmt.Sprintf("/api/v3/orgs/%s/codespaces/secrets/ORG_SECRET", org.Login), defaultToken)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("delete org secret: %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

func TestCodespaces_404Cases(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	s.createTestCodespaceRepo(t, "cs-404-repo")

	resp := s.get(t, "/api/v3/user/codespaces/no-such-codespace", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 404, got %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	resp = s.get(t, "/api/v3/repos/no-owner/no-repo/codespaces", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 404 repo, got %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

// runDockerCLI is already defined in store_codespaces.go.

// TestCodespaces_OrgMemberAdministration exercises the org-owner view of
// a member's codespaces on the organization's repositories:
// list → stop (200 with the codespace body) → delete (202).
func TestCodespaces_OrgMemberAdministration(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	s.createOrg(t, "cs-admin-org")
	org := s.store.GetOrg("cs-admin-org")
	repo := s.store.CreateOrgRepo(org, admin, "cs-admin-repo", "org codespace repo", false)
	if repo == nil {
		t.Fatal("create org repo")
	}
	stor := s.store.GitStorages[repo.FullName]
	if _, err := initRepoWithFiles(stor, repo.DefaultBranch, "init", map[string]string{
		".devcontainer/devcontainer.json": fmt.Sprintf(`{"image":%q}`, codespaceTestImage),
	}, repoSignature(admin.Login, "bleephub@local")); err != nil {
		t.Fatalf("init repo files: %v", err)
	}

	_, memberToken := s.newUser(t, "cs-org-member")
	s.activateOrgMember(t, "cs-admin-org", "cs-org-member", memberToken)

	created := s.post(t, "/api/v3/user/codespaces", memberToken, map[string]any{
		"repository_id": repo.ID,
		"machine":       "basicLinux32",
	})
	if created.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(created.Body)
		created.Body.Close()
		t.Fatalf("member creates codespace: %d %s", created.StatusCode, b)
	}
	name := decodeJSON(t, created)["name"].(string)
	t.Cleanup(func() { s.cleanupCodespaceContainer(t, name) })

	list := s.get(t, "/api/v3/orgs/cs-admin-org/members/cs-org-member/codespaces", defaultToken)
	if list.StatusCode != http.StatusOK {
		list.Body.Close()
		t.Fatalf("org member codespaces list: %d", list.StatusCode)
	}
	listing := decodeJSON(t, list)
	if listing["total_count"] != float64(1) {
		t.Fatalf("total_count = %v, want 1", listing["total_count"])
	}
	first := listing["codespaces"].([]interface{})[0].(map[string]interface{})
	if first["name"] != name {
		t.Fatalf("listed codespace = %v", first)
	}
	if _, has := first["html_url"]; has {
		t.Fatalf("org-scoped codespace carries undocumented html_url: %v", first)
	}

	stopped := s.post(t, "/api/v3/orgs/cs-admin-org/members/cs-org-member/codespaces/"+name+"/stop", defaultToken, nil)
	if stopped.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(stopped.Body)
		stopped.Body.Close()
		t.Fatalf("org member codespace stop: %d %s", stopped.StatusCode, b)
	}
	if body := decodeJSON(t, stopped); body["name"] != name {
		t.Fatalf("stop response = %v", body)
	}

	// The member and codespace must both resolve within the org.
	resp := s.get(t, "/api/v3/orgs/cs-admin-org/members/nobody-here/codespaces", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Fatalf("unknown member list: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = s.delete(t, "/api/v3/orgs/cs-admin-org/members/cs-org-member/codespaces/no-such-codespace", defaultToken)
	if resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Fatalf("unknown codespace delete: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// Only org owners administer member codespaces.
	resp = s.get(t, "/api/v3/orgs/cs-admin-org/members/cs-org-member/codespaces", memberToken)
	if resp.StatusCode != http.StatusForbidden {
		resp.Body.Close()
		t.Fatalf("member lists own org codespaces: %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	deleted := s.delete(t, "/api/v3/orgs/cs-admin-org/members/cs-org-member/codespaces/"+name, defaultToken)
	if deleted.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(deleted.Body)
		deleted.Body.Close()
		t.Fatalf("org member codespace delete: %d %s", deleted.StatusCode, b)
	}
	deleted.Body.Close()
	after := decodeJSON(t, s.get(t, "/api/v3/orgs/cs-admin-org/members/cs-org-member/codespaces", defaultToken))
	if after["total_count"] != float64(0) {
		t.Fatalf("codespaces after delete = %v", after)
	}
}

func TestCodespaces_OrgList(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "cs-list-org", "Codespaces List Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}

	// Honest empty list before any member codespace exists... except that
	// the admin may own codespaces created by earlier tests, so assert
	// against membership: every returned codespace belongs to an org member.
	repo := s.createTestCodespaceRepo(t, "cs-org-list-repo")
	resp := s.post(t, "/api/v3/user/codespaces", defaultToken, map[string]any{
		"repository_id": repo.ID,
		"machine":       "basicLinux32",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create codespace: %d %s", resp.StatusCode, b)
	}
	created := decodeJSON(t, resp)
	name, _ := created["name"].(string)
	defer s.cleanupCodespaceContainer(t, name)

	resp = s.get(t, "/api/v3/orgs/cs-list-org/codespaces", defaultToken)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("list org codespaces: %d", resp.StatusCode)
	}
	var listResp struct {
		TotalCount int              `json:"total_count"`
		Codespaces []map[string]any `json:"codespaces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode org codespaces: %v", err)
	}
	resp.Body.Close()
	if listResp.TotalCount != len(listResp.Codespaces) {
		t.Fatalf("total_count %d != len(codespaces) %d", listResp.TotalCount, len(listResp.Codespaces))
	}
	found := false
	for _, cs := range listResp.Codespaces {
		if cs["name"] == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("member codespace %s missing from org list", name)
	}

	// Non-admin callers are forbidden.
	outsider := s.createTestUser(t, "cs-outsider")
	s.store.Tokens["ghp_cs_outsider"] = &store.Token{Value: "ghp_cs_outsider", UserID: outsider.ID}
	resp = s.get(t, "/api/v3/orgs/cs-list-org/codespaces", "ghp_cs_outsider")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin org codespaces: %d, want 403", resp.StatusCode)
	}

	cs := s.store.GetCodespaceByName(name)
	if cs == nil {
		t.Fatalf("created codespace %s disappeared before cleanup", name)
	}
	workspace := cs.WorkspaceMount
	resp = s.delete(t, "/api/v3/user/codespaces/"+name, defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete codespace: %d, want 202", resp.StatusCode)
	}
	if workspace != "" {
		if _, err := os.Stat(workspace); !os.IsNotExist(err) {
			t.Fatalf("deleted codespace workspace still exists at %q: %v", workspace, err)
		}
	}
}

func TestCodespaces_OrgAccessControls(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "cs-access-org", "Codespaces Access Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}
	member := s.createTestUser(t, "cs-access-member")
	s.store.SetMembership(org.Login, member.ID, store.OrgRoleMember, store.MembershipStateActive)
	member2 := s.createTestUser(t, "cs-access-member2")
	s.store.SetMembership(org.Login, member2.ID, store.OrgRoleMember, store.MembershipStateActive)

	// Invalid visibility.
	resp := s.put(t, "/api/v3/orgs/cs-access-org/codespaces/access", defaultToken, map[string]any{
		"visibility": "everyone-on-earth",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid visibility: %d, want 422", resp.StatusCode)
	}

	// Usernames that are neither members nor collaborators are rejected.
	resp = s.put(t, "/api/v3/orgs/cs-access-org/codespaces/access", defaultToken, map[string]any{
		"visibility":         "selected_members",
		"selected_usernames": []string{"total-stranger"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("stranger username: %d, want 400", resp.StatusCode)
	}

	// Set selected_members with one member.
	resp = s.put(t, "/api/v3/orgs/cs-access-org/codespaces/access", defaultToken, map[string]any{
		"visibility":         "selected_members",
		"selected_usernames": []string{"cs-access-member"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set access: %d, want 204", resp.StatusCode)
	}

	// Add and remove selected users.
	resp = s.post(t, "/api/v3/orgs/cs-access-org/codespaces/access/selected_users", defaultToken, map[string]any{
		"selected_usernames": []string{"cs-access-member2"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("add selected user: %d, want 204", resp.StatusCode)
	}
	access := s.store.OrgCodespacesAccess["cs-access-org"]
	if access == nil || len(access.SelectedUsernames) != 2 {
		t.Fatalf("selected usernames after add: %+v", access)
	}
	resp = s.do(t, "DELETE", "/api/v3/orgs/cs-access-org/codespaces/access/selected_users", defaultToken, map[string]interface{}{
		"selected_usernames": []interface{}{"cs-access-member"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove selected user: %d, want 204", resp.StatusCode)
	}
	access = s.store.OrgCodespacesAccess["cs-access-org"]
	if access == nil || len(access.SelectedUsernames) != 1 || access.SelectedUsernames[0] != "cs-access-member2" {
		t.Fatalf("selected usernames after remove: %+v", access)
	}

	// Disabling access makes the selected-users endpoints invalid.
	resp = s.put(t, "/api/v3/orgs/cs-access-org/codespaces/access", defaultToken, map[string]any{
		"visibility": "disabled",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("disable access: %d, want 204", resp.StatusCode)
	}
	resp = s.post(t, "/api/v3/orgs/cs-access-org/codespaces/access/selected_users", defaultToken, map[string]any{
		"selected_usernames": []string{"cs-access-member"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("add selected user while disabled: %d, want 422", resp.StatusCode)
	}
}

func TestCodespaces_OrgSecretSelectedRepoAddRemove(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "cs-secret-repo-org", "Codespaces Secret Repo Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}
	r1 := s.store.CreateOrgRepo(org, admin, "cs-secret-repo-1", "", false)
	r2 := s.store.CreateOrgRepo(org, admin, "cs-secret-repo-2", "", false)
	if r1 == nil || r2 == nil {
		t.Fatal("create org repos failed")
	}

	enc, keyID := s.sealForServer(t, "org codespace secret value")
	resp := s.put(t, "/api/v3/orgs/cs-secret-repo-org/codespaces/secrets/CS_SELECTED", defaultToken, map[string]any{
		"encrypted_value":         enc,
		"key_id":                  keyID,
		"visibility":              "selected",
		"selected_repository_ids": []int{r1.ID},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put org codespace secret: %d, want 204", resp.StatusCode)
	}

	// Add the second repository.
	resp = s.put(t, fmt.Sprintf("/api/v3/orgs/cs-secret-repo-org/codespaces/secrets/CS_SELECTED/repositories/%d", r2.ID), defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("add selected repo: %d, want 204", resp.StatusCode)
	}
	resp = s.get(t, "/api/v3/orgs/cs-secret-repo-org/codespaces/secrets/CS_SELECTED/repositories", defaultToken)
	repos := decodeJSON(t, resp)
	if repos["total_count"] != float64(2) {
		t.Fatalf("selected repos after add = %v, want 2", repos["total_count"])
	}

	// Remove the first repository.
	resp = s.delete(t, fmt.Sprintf("/api/v3/orgs/cs-secret-repo-org/codespaces/secrets/CS_SELECTED/repositories/%d", r1.ID), defaultToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove selected repo: %d, want 204", resp.StatusCode)
	}
	resp = s.get(t, "/api/v3/orgs/cs-secret-repo-org/codespaces/secrets/CS_SELECTED/repositories", defaultToken)
	repos = decodeJSON(t, resp)
	if repos["total_count"] != float64(1) {
		t.Fatalf("selected repos after remove = %v, want 1", repos["total_count"])
	}

	// A secret with visibility all conflicts.
	enc, keyID = s.sealForServer(t, "all visibility value")
	resp = s.put(t, "/api/v3/orgs/cs-secret-repo-org/codespaces/secrets/CS_ALL", defaultToken, map[string]any{
		"encrypted_value": enc,
		"key_id":          keyID,
		"visibility":      "all",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put all-visibility secret: %d, want 204", resp.StatusCode)
	}
	resp = s.put(t, fmt.Sprintf("/api/v3/orgs/cs-secret-repo-org/codespaces/secrets/CS_ALL/repositories/%d", r1.ID), defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("add repo to all-visibility secret: %d, want 409", resp.StatusCode)
	}

	// Unknown secret.
	resp = s.put(t, fmt.Sprintf("/api/v3/orgs/cs-secret-repo-org/codespaces/secrets/NO_SUCH/repositories/%d", r1.ID), defaultToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown secret add repo: %d, want 404", resp.StatusCode)
	}
}

func createPaginationTestCodespace(t *testing.T, s *Server, path string, body map[string]any) string {
	t.Helper()
	w := pagedJSONRequest(t, s, http.MethodPost, path, defaultToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create codespace: %d %s", w.Code, w.Body.String())
	}
	var cs map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &cs); err != nil {
		t.Fatalf("decode codespace: %v", err)
	}
	return cs["name"].(string)
}

func createPaginationCodespaceRepo(t *testing.T, s *Server, name string) *store.Repo {
	t.Helper()
	admin := s.store.UsersByLogin["admin"]
	repo := s.store.CreateRepo(admin, name, "codespace test repo", false)
	if repo == nil {
		t.Fatalf("failed to create repo %s", name)
	}
	stor := s.store.GitStorages[repo.FullName]
	if _, err := initRepoWithFiles(stor, repo.DefaultBranch, "init", map[string]string{
		".devcontainer/devcontainer.json": fmt.Sprintf(`{"image":"%s"}`, codespaceTestImage),
	}, repoSignature(admin.Login, "bleephub@local")); err != nil {
		t.Fatalf("init repo files: %v", err)
	}
	return repo
}

func assertCodespaceWrapperPage(t *testing.T, s *Server, path, key string, wantItems, wantTotal int, wantNext bool) map[string]interface{} {
	t.Helper()
	w := tokenRequest(s, http.MethodGet, path, defaultToken)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s, want 200", path, w.Code, w.Body.String())
	}
	link := w.Header().Get("Link")
	var listing map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil {
		t.Fatalf("GET %s decode: %v", path, err)
	}
	if got := int(listing["total_count"].(float64)); got != wantTotal {
		t.Fatalf("GET %s total_count = %d, want %d", path, got, wantTotal)
	}
	items, ok := listing[key].([]interface{})
	if !ok {
		t.Fatalf("GET %s missing %q array: %v", path, key, listing)
	}
	if len(items) != wantItems {
		t.Fatalf("GET %s returned %d %s, want %d", path, len(items), key, wantItems)
	}
	if hasNext := strings.Contains(link, `rel="next"`); hasNext != wantNext {
		t.Fatalf("GET %s Link = %q, want rel=\"next\" presence %v", path, link, wantNext)
	}
	return listing
}

func putPaginationSealedSecret(t *testing.T, s *Server, path, plain string, want int) {
	t.Helper()
	enc, keyID, err := s.store.SealSecretValue(plain)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	w := pagedJSONRequest(t, s, http.MethodPut, path, defaultToken, map[string]any{
		"encrypted_value": enc,
		"key_id":          keyID,
	})
	if w.Code != want {
		t.Fatalf("put %s: %d %s, want %d", path, w.Code, w.Body.String(), want)
	}
}

func TestCodespaces_UserListPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	repo := createPaginationCodespaceRepo(t, s, "cs-pg-user-repo")
	t.Setenv("PATH", t.TempDir())

	base := tokenRequest(s, http.MethodGet, "/api/v3/user/codespaces?per_page=100", defaultToken)
	var baseListing map[string]interface{}
	if err := json.Unmarshal(base.Body.Bytes(), &baseListing); err != nil {
		t.Fatalf("decode base codespaces listing: %v", err)
	}
	total := int(baseListing["total_count"].(float64)) + 2

	first := createPaginationTestCodespace(t, s, "/api/v3/user/codespaces", map[string]any{
		"repository_id": repo.ID,
		"machine":       "basicLinux32",
	})
	second := createPaginationTestCodespace(t, s, "/api/v3/user/codespaces", map[string]any{
		"repository_id": repo.ID,
		"machine":       "basicLinux32",
	})

	page1 := assertCodespaceWrapperPage(t, s, "/api/v3/user/codespaces?per_page=1", "codespaces", 1, total, true)
	page2 := assertCodespaceWrapperPage(t, s, "/api/v3/user/codespaces?per_page=1&page=2", "codespaces", 1, total, false)
	name1 := page1["codespaces"].([]interface{})[0].(map[string]interface{})["name"]
	name2 := page2["codespaces"].([]interface{})[0].(map[string]interface{})["name"]
	if name1 == name2 {
		t.Fatalf("pages 1 and 2 returned the same codespace %v", name1)
	}
	seen := map[string]bool{first: true, second: true}
	if !seen[name1.(string)] || !seen[name2.(string)] {
		t.Fatalf("paginated names %v, %v not in seeded set %v", name1, name2, seen)
	}
}

func TestCodespaces_RepoListPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	repo := createPaginationCodespaceRepo(t, s, "cs-pg-repo-repo")
	t.Setenv("PATH", t.TempDir())

	createPaginationTestCodespace(t, s, fmt.Sprintf("/api/v3/repos/%s/codespaces", repo.FullName), map[string]any{
		"machine": "basicLinux32",
	})
	createPaginationTestCodespace(t, s, fmt.Sprintf("/api/v3/repos/%s/codespaces", repo.FullName), map[string]any{
		"machine": "basicLinux32",
	})

	assertCodespaceWrapperPage(t, s, fmt.Sprintf("/api/v3/repos/%s/codespaces?per_page=1", repo.FullName), "codespaces", 1, 2, true)
	assertCodespaceWrapperPage(t, s, fmt.Sprintf("/api/v3/repos/%s/codespaces?per_page=1&page=2", repo.FullName), "codespaces", 1, 2, false)
}

func TestCodespaces_UserSecretsListPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()

	base := tokenRequest(s, http.MethodGet, "/api/v3/user/codespaces/secrets?per_page=100", defaultToken)
	var baseListing map[string]interface{}
	if err := json.Unmarshal(base.Body.Bytes(), &baseListing); err != nil {
		t.Fatalf("decode base secrets listing: %v", err)
	}
	total := int(baseListing["total_count"].(float64)) + 2

	for _, name := range []string{"PG_SECRET_ONE", "PG_SECRET_TWO"} {
		putPaginationSealedSecret(t, s, "/api/v3/user/codespaces/secrets/"+name, "value-"+name, http.StatusNoContent)
	}

	assertCodespaceWrapperPage(t, s, "/api/v3/user/codespaces/secrets?per_page=1", "secrets", 1, total, true)
	assertCodespaceWrapperPage(t, s, "/api/v3/user/codespaces/secrets?per_page=1&page=2", "secrets", 1, total, false)
}

func TestCodespaces_RepoSecretsListPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	repo := createPaginationCodespaceRepo(t, s, "cs-pg-repo-secrets")
	for _, name := range []string{"PG_REPO_SECRET_ONE", "PG_REPO_SECRET_TWO"} {
		putPaginationSealedSecret(t, s, fmt.Sprintf("/api/v3/repos/%s/codespaces/secrets/%s", repo.FullName, name), "value-"+name, http.StatusNoContent)
	}

	assertCodespaceWrapperPage(t, s, fmt.Sprintf("/api/v3/repos/%s/codespaces/secrets?per_page=1", repo.FullName), "secrets", 1, 2, true)
	assertCodespaceWrapperPage(t, s, fmt.Sprintf("/api/v3/repos/%s/codespaces/secrets?per_page=1&page=2", repo.FullName), "secrets", 1, 2, false)
}

func TestCodespaces_OrgSecretReposListPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	admin := s.store.UsersByLogin["admin"]
	org := s.store.CreateOrg(admin, "cs-pg-secret-repo-org", "Codespaces Pagination Secret Repo Org", "")
	if org == nil {
		t.Fatal("create org failed")
	}
	r1 := s.store.CreateOrgRepo(org, admin, "cs-pg-secret-repo-1", "", false)
	r2 := s.store.CreateOrgRepo(org, admin, "cs-pg-secret-repo-2", "", false)
	if r1 == nil || r2 == nil {
		t.Fatal("create org repos failed")
	}

	enc, keyID, err := s.store.SealSecretValue("paginated org secret value")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	w := pagedJSONRequest(t, s, http.MethodPut, "/api/v3/orgs/cs-pg-secret-repo-org/codespaces/secrets/PG_SELECTED", defaultToken, map[string]any{
		"encrypted_value":         enc,
		"key_id":                  keyID,
		"visibility":              "selected",
		"selected_repository_ids": []int{r1.ID, r2.ID},
	})
	if w.Code != http.StatusNoContent {
		t.Fatalf("put org codespace secret: %d, want 204", w.Code)
	}

	page1 := assertCodespaceWrapperPage(t, s, "/api/v3/orgs/cs-pg-secret-repo-org/codespaces/secrets/PG_SELECTED/repositories?per_page=1", "repositories", 1, 2, true)
	page2 := assertCodespaceWrapperPage(t, s, "/api/v3/orgs/cs-pg-secret-repo-org/codespaces/secrets/PG_SELECTED/repositories?per_page=1&page=2", "repositories", 1, 2, false)
	id1 := page1["repositories"].([]interface{})[0].(map[string]interface{})["id"]
	id2 := page2["repositories"].([]interface{})[0].(map[string]interface{})["id"]
	if id1 == id2 {
		t.Fatalf("pages 1 and 2 returned the same repository %v", id1)
	}
}

func TestCodespaces_RepoDevcontainersPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	repo := createPaginationCodespaceRepo(t, s, "cs-pg-devcontainers")
	assertCodespaceWrapperPage(t, s, fmt.Sprintf("/api/v3/repos/%s/codespaces/devcontainers?per_page=1", repo.FullName), "devcontainers", 0, 0, false)
}

package bleephub

import (
	"net/http"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// ACT-014: GITHUB_TOKEN's REST access is scoped to the workflow's repository and
// the least-privilege permission set its `permissions:` block resolves to.

func TestResolveJobTokenPermissions(t *testing.T) {
	s := newTestServer()
	wfWith := func(p store.PermissionDef) *store.Workflow {
		return &store.Workflow{RepoFullName: "admin/r", Permissions: p}
	}
	jdWith := func(p store.PermissionDef) *store.JobDef { return &store.JobDef{Permissions: p} }

	// Undeclared → the repo default level (read) across the standard scope set,
	// and nothing outside it.
	got := s.resolveJobTokenPermissions(wfWith(nil), jdWith(nil))
	if got["contents"] != "read" || got["issues"] != "read" || got["metadata"] != "read" {
		t.Fatalf("default grant = %v", got)
	}
	if _, ok := got["administration"]; ok {
		t.Fatalf("default granted a non-standard scope: %v", got)
	}

	// A declared workflow block replaces the default: only the listed scopes.
	got = s.resolveJobTokenPermissions(wfWith(store.PermissionDef{"issues": "write"}), jdWith(nil))
	if got["issues"] != "write" || got["metadata"] != "read" {
		t.Fatalf("declared block = %v", got)
	}
	if _, ok := got["contents"]; ok {
		t.Fatalf("declared block leaked contents: %v", got)
	}

	// A job block fully overrides the workflow block.
	got = s.resolveJobTokenPermissions(wfWith(store.PermissionDef{"issues": "write"}), jdWith(store.PermissionDef{"contents": "read"}))
	if got["contents"] != "read" {
		t.Fatalf("job override lost contents: %v", got)
	}
	if _, ok := got["issues"]; ok {
		t.Fatalf("job override kept the workflow's issues: %v", got)
	}

	// write-all grants write across the standard set.
	got = s.resolveJobTokenPermissions(wfWith(store.PermissionDef{"*": "write"}), jdWith(nil))
	if got["contents"] != "write" || got["pull_requests"] != "write" {
		t.Fatalf("write-all = %v", got)
	}

	// An explicit empty block yields metadata:read only.
	got = s.resolveJobTokenPermissions(wfWith(store.PermissionDef{}), jdWith(nil))
	if len(got) != 1 || got["metadata"] != "read" {
		t.Fatalf("empty block = %v", got)
	}

	// A `none` value drops the scope; hyphenated keys normalize to permScope form.
	got = s.resolveJobTokenPermissions(wfWith(store.PermissionDef{"pull-requests": "write", "contents": "none"}), jdWith(nil))
	if got["pull_requests"] != "write" {
		t.Fatalf("hyphen normalization = %v", got)
	}
	if _, ok := got["contents"]; ok {
		t.Fatalf("none value not dropped: %v", got)
	}

	// A repo whose default workflow permission is write grants write by default.
	s.store.SetRepoActionsPermissions("admin/rw", &store.RepoActionsPermissions{WorkflowPermissions: &store.WorkflowPermissions{DefaultWorkflowPermissions: "write"}})
	got = s.resolveJobTokenPermissions(&store.Workflow{RepoFullName: "admin/rw"}, jdWith(nil))
	if got["contents"] != "write" {
		t.Fatalf("repo write-default = %v", got)
	}
}

func TestGithubTokenIsRepoScopedLeastPrivilege(t *testing.T) {
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	s.store.CreateRepo(admin, "act014", "", false)
	s.store.CreateRepo(admin, "other", "", false)

	dispatch := func(repoPath string, perms map[string]string) int {
		token := makeJobJWT("scope-"+repoPath, "admin/act014", perms)
		resp := s.do(t, http.MethodPost, "/api/v3/repos/"+repoPath+"/dispatches", token, map[string]interface{}{"event_type": "go"})
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// contents:write is granted → the write-gated dispatch passes the gate.
	if code := dispatch("admin/act014", map[string]string{"contents": "write"}); code != http.StatusNoContent {
		t.Fatalf("contents:write token: status=%d, want 204", code)
	}

	// Only contents:read → the contents:write gate forbids it.
	if code := dispatch("admin/act014", map[string]string{"contents": "read"}); code != http.StatusForbidden {
		t.Fatalf("contents:read token on a write gate: status=%d, want 403", code)
	}

	// A block that grants unrelated scopes but not contents is forbidden.
	if code := dispatch("admin/act014", map[string]string{"issues": "write"}); code != http.StatusForbidden {
		t.Fatalf("issues:write token on a contents gate: status=%d, want 403", code)
	}

	// A token minted for admin/act014 cannot act on a different repository even
	// with the right scope — it is bound to its own repo.
	if code := dispatch("admin/other", map[string]string{"contents": "write"}); code != http.StatusForbidden && code != http.StatusNotFound {
		t.Fatalf("cross-repo token: status=%d, want 403/404 (repo-scoped)", code)
	}
}

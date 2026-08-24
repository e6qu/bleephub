package bleephub

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// protectBranchForWriteTests puts a base protection rule (status checks +
// restrictions) on main so the sub-resource endpoints have state to act on.
func protectBranchForWriteTests(t *testing.T, s *isolatedServer, repo string) string {
	t.Helper()
	base := "/api/v3/repos/admin/" + repo + "/branches/main/protection"
	resp := s.put(t, base, defaultToken, map[string]interface{}{
		"required_status_checks": map[string]interface{}{"strict": true, "contexts": []string{"ci"}},
		"restrictions": map[string]interface{}{
			"users": []map[string]interface{}{{"login": "admin", "id": 1, "type": "User"}},
		},
	})
	requireStatus(t, resp, 200)
	return base
}

func TestBranchProtectionRequiredSignatures(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createRepoWriteRepo(t, true)

	// Unprotected branch → 404 on every method.
	resp := s.get(t, "/api/v3/repos/admin/"+repo+"/branches/main/protection/required_signatures", defaultToken)
	requireStatus(t, resp, 404)

	base := protectBranchForWriteTests(t, s, repo)

	resp = s.get(t, base+"/required_signatures", defaultToken)
	data := decodeJSONWithStatus(t, resp, 200)
	if data["enabled"] != false {
		t.Fatalf("enabled = %v, want false before POST", data["enabled"])
	}
	if data["url"] == "" {
		t.Fatal("missing url")
	}

	resp = s.post(t, base+"/required_signatures", defaultToken, nil)
	data = decodeJSONWithStatus(t, resp, 200)
	if data["enabled"] != true {
		t.Fatalf("enabled = %v, want true after POST", data["enabled"])
	}

	// The top-level protection object now carries required_signatures.
	resp = s.get(t, base, defaultToken)
	protection := decodeJSONWithStatus(t, resp, 200)
	rs, _ := protection["required_signatures"].(map[string]interface{})
	if rs == nil || rs["enabled"] != true {
		t.Fatalf("protection.required_signatures = %v, want enabled true", protection["required_signatures"])
	}

	delResp := s.delete(t, base+"/required_signatures", defaultToken)
	requireStatus(t, delResp, 204)
	resp = s.get(t, base+"/required_signatures", defaultToken)
	data = decodeJSONWithStatus(t, resp, 200)
	if data["enabled"] != false {
		t.Fatalf("enabled = %v, want false after DELETE", data["enabled"])
	}
}

func TestBranchProtectionRestrictionsApps(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createRepoWriteRepo(t, true)
	base := protectBranchForWriteTests(t, s, repo)

	admin := s.store.LookupUserByLogin("admin")
	appA := s.store.CreateApp(admin.ID, "Push App A", "app with push access",
		map[string]string{"contents": "write"}, []string{"push"})
	appB := s.store.CreateApp(admin.ID, "Push App B", "second app with push access",
		map[string]string{"contents": "write"}, []string{"push"})

	resp := s.get(t, base+"/restrictions/apps", defaultToken)
	list := decodeJSONWithStatus2xxArray(t, resp, 200)
	if len(list) != 0 {
		t.Fatalf("initial apps = %v, want empty", list)
	}

	// Unknown slugs are rejected.
	resp = s.post(t, base+"/restrictions/apps", defaultToken, map[string]interface{}{"apps": []string{"no-such-app"}})
	requireStatus(t, resp, 422)

	resp = s.post(t, base+"/restrictions/apps", defaultToken, map[string]interface{}{"apps": []string{appA.Slug}})
	list = decodeJSONWithStatus2xxArray(t, resp, 200)
	if len(list) != 1 || list[0]["slug"] != appA.Slug {
		t.Fatalf("apps after POST = %v, want [%s]", list, appA.Slug)
	}

	// PUT replaces the whole list.
	resp = s.put(t, base+"/restrictions/apps", defaultToken, map[string]interface{}{"apps": []string{appB.Slug}})
	list = decodeJSONWithStatus2xxArray(t, resp, 200)
	if len(list) != 1 || list[0]["slug"] != appB.Slug {
		t.Fatalf("apps after PUT = %v, want [%s]", list, appB.Slug)
	}

	// POST appends without duplicating.
	resp = s.post(t, base+"/restrictions/apps", defaultToken, map[string]interface{}{"apps": []string{appA.Slug, appB.Slug}})
	list = decodeJSONWithStatus2xxArray(t, resp, 200)
	if len(list) != 2 {
		t.Fatalf("apps after append = %v, want 2 entries", list)
	}

	resp = s.do(t, "DELETE", base+"/restrictions/apps", defaultToken, map[string]interface{}{"apps": []interface{}{appB.Slug}})
	list = decodeJSONWithStatus2xxArray(t, resp, 200)
	if len(list) != 1 || list[0]["slug"] != appA.Slug {
		t.Fatalf("apps after DELETE = %v, want [%s]", list, appA.Slug)
	}

	resp = s.get(t, base+"/restrictions/apps", defaultToken)
	list = decodeJSONWithStatus2xxArray(t, resp, 200)
	if len(list) != 1 {
		t.Fatalf("apps round-trip = %v, want 1 entry", list)
	}
}

func TestBranchProtectionRequiredStatusChecksPatch(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createRepoWriteRepo(t, true)
	base := protectBranchForWriteTests(t, s, repo)

	resp := s.patch(t, base+"/required_status_checks", defaultToken, map[string]interface{}{
		"strict": false,
		"checks": []map[string]interface{}{{"context": "build", "app_id": -1}},
	})
	data := decodeJSONWithStatus(t, resp, 200)
	if data["strict"] != false {
		t.Fatalf("strict = %v, want false", data["strict"])
	}
	checks, _ := data["checks"].([]interface{})
	if len(checks) != 1 {
		t.Fatalf("checks = %v, want 1 entry", data["checks"])
	}
	check := checks[0].(map[string]interface{})
	if check["context"] != "build" || check["app_id"] != float64(-1) {
		t.Fatalf("check = %v, want context build app_id -1", check)
	}
	// contexts and checks are two views of one set, so replacing the set
	// through `checks` renames it in both views. Leaving the previous "ci"
	// behind in `contexts` would publish a policy whose two halves disagree,
	// and make the /contexts sub-resource contradict the rule itself.
	contexts, _ := data["contexts"].([]interface{})
	if len(contexts) != 1 || contexts[0] != "build" {
		t.Fatalf("contexts = %v, want [build]", data["contexts"])
	}
	if data["contexts_url"] == "" || data["url"] == "" {
		t.Fatal("missing url/contexts_url")
	}

	// The merged rule reads back the same through GET.
	resp = s.get(t, base+"/required_status_checks", defaultToken)
	got := decodeJSONWithStatus(t, resp, 200)
	if got["strict"] != false {
		t.Fatalf("GET strict = %v, want false", got["strict"])
	}
	gotChecks, _ := got["checks"].([]interface{})
	if len(gotChecks) != 1 {
		t.Fatalf("GET checks = %v, want 1 entry", got["checks"])
	}

	// PATCH on a branch without protection is a 404.
	resp = s.patch(t, "/api/v3/repos/admin/"+repo+"/branches/other/protection/required_status_checks",
		defaultToken, map[string]interface{}{"strict": true})
	requireStatus(t, resp, 404)
}

// TestBranchProtectionStatusCheckContextsAndChecksAreOneSet pins that the
// published status-check-policy keeps `contexts` and `checks` in step. They are
// two views of a single set — the legacy list of names, and the same names
// paired with the app allowed to report them — so a write through either view
// has to be visible in both, and the /contexts sub-resource has to agree with
// the rule it belongs to.
func TestBranchProtectionStatusCheckContextsAndChecksAreOneSet(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.createRepoWriteRepo(t, true)
	base := protectBranchForWriteTests(t, s, repo)

	// The PUT named the set through `contexts`; `checks` carries the same name.
	policy := decodeJSONWithStatus(t, s.get(t, base+"/required_status_checks", defaultToken), 200)
	requireContextSet(t, policy, "ci")

	// The sub-resource is a view of that same set, not an independent list.
	contexts := decodeStringArrayWithStatus(t, s.get(t, base+"/required_status_checks/contexts", defaultToken), 200)
	if len(contexts) != 1 || contexts[0] != "ci" {
		t.Fatalf("GET contexts = %v, want [ci]", contexts)
	}

	// POST adds to the set and answers with the whole set, not just the
	// value added — a client that replaced its state with the response would
	// otherwise lose every context it did not name in this call.
	added := decodeStringArrayWithStatus(t, s.post(t, base+"/required_status_checks/contexts", defaultToken, []string{"lint"}), 200)
	if len(added) != 2 || added[0] != "ci" || added[1] != "lint" {
		t.Fatalf("POST contexts = %v, want [ci lint]", added)
	}
	policy = decodeJSONWithStatus(t, s.get(t, base+"/required_status_checks", defaultToken), 200)
	requireContextSet(t, policy, "ci", "lint")

	// PUT replaces the set through the names view; `checks` follows.
	replaced := decodeStringArrayWithStatus(t, s.put(t, base+"/required_status_checks/contexts", defaultToken, []string{"build"}), 200)
	if len(replaced) != 1 || replaced[0] != "build" {
		t.Fatalf("PUT contexts = %v, want [build]", replaced)
	}
	policy = decodeJSONWithStatus(t, s.get(t, base+"/required_status_checks", defaultToken), 200)
	requireContextSet(t, policy, "build")

	// A write through the richer view is equally visible in the names view.
	policy = decodeJSONWithStatus(t, s.patch(t, base+"/required_status_checks", defaultToken,
		map[string]interface{}{"checks": []map[string]interface{}{{"context": "verify", "app_id": 42}}}), 200)
	requireContextSet(t, policy, "verify")
	checks, _ := policy["checks"].([]interface{})
	if len(checks) != 1 {
		t.Fatalf("checks = %v, want 1 entry", policy["checks"])
	}
	if check, _ := checks[0].(map[string]interface{}); check == nil || check["app_id"] != float64(42) {
		t.Fatalf("checks[0] = %v, want app_id 42", checks[0])
	}
	contexts = decodeStringArrayWithStatus(t, s.get(t, base+"/required_status_checks/contexts", defaultToken), 200)
	if len(contexts) != 1 || contexts[0] != "verify" {
		t.Fatalf("GET contexts after checks PATCH = %v, want [verify]", contexts)
	}
}

// requireContextSet asserts that both views of the status-check set — the
// `contexts` names and the `checks` objects — name exactly want, in order.
func requireContextSet(t *testing.T, policy map[string]interface{}, want ...string) {
	t.Helper()
	contexts, _ := policy["contexts"].([]interface{})
	if len(contexts) != len(want) {
		t.Fatalf("contexts = %v, want %v", policy["contexts"], want)
	}
	for i, name := range want {
		if contexts[i] != name {
			t.Fatalf("contexts = %v, want %v", policy["contexts"], want)
		}
	}
	checks, _ := policy["checks"].([]interface{})
	if len(checks) != len(want) {
		t.Fatalf("checks = %v, want the same %d contexts as %v", policy["checks"], len(want), want)
	}
	for i, name := range want {
		check, _ := checks[i].(map[string]interface{})
		if check == nil || check["context"] != name {
			t.Fatalf("checks[%d] = %v, want context %q", i, checks[i], name)
		}
	}
}

// TestBranchProtectionSubResourceDeleteKeepsBranchProtected pins that turning
// one rule off never unprotects the branch. Each of these DELETE endpoints is
// documented to remove only its own rule; dropping the whole protection record
// with the last rule would silently retire every other rule an administrator
// still believes is in force. Only DELETE .../protection unprotects a branch.
func TestBranchProtectionSubResourceDeleteKeepsBranchProtected(t *testing.T) {
	t.Parallel()

	// Each case protects the branch with exactly one rule, so the DELETE under
	// test is removing the only rule there is — the case where a record keyed
	// on "has any rule" collapses.
	cases := []struct {
		name    string
		rule    string
		body    map[string]interface{}
		subPath string
		// enableFirst is a sub-resource POST that has to run before the DELETE
		// under test, for a rule the top-level PUT cannot name.
		enableFirst string
	}{
		{
			name:    "enforce_admins",
			rule:    "enforce_admins",
			body:    map[string]interface{}{"enforce_admins": true},
			subPath: "/enforce_admins",
		},
		{
			name:    "required_status_checks",
			rule:    "required_status_checks",
			body:    map[string]interface{}{"required_status_checks": map[string]interface{}{"strict": true, "contexts": []string{"ci"}}},
			subPath: "/required_status_checks",
		},
		{
			name: "required_pull_request_reviews",
			rule: "required_pull_request_reviews",
			body: map[string]interface{}{
				"required_pull_request_reviews": map[string]interface{}{"required_approving_review_count": 1},
			},
			subPath: "/required_pull_request_reviews",
		},
		{
			name: "restrictions",
			rule: "restrictions",
			body: map[string]interface{}{
				"restrictions": map[string]interface{}{
					"users": []map[string]interface{}{{"login": "admin", "id": 1, "type": "User"}},
				},
			},
			subPath: "/restrictions",
		},
		{
			// required_signatures has no member in the top-level PUT body, so
			// it is turned on through its own sub-resource first.
			name:        "required_signatures",
			rule:        "required_signatures",
			body:        map[string]interface{}{"enforce_admins": true},
			subPath:     "/required_signatures",
			enableFirst: "/required_signatures",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newIsolatedServer(t)
			repo := s.createRepoWriteRepo(t, true)
			base := "/api/v3/repos/admin/" + repo + "/branches/main/protection"
			requireStatus(t, s.put(t, base, defaultToken, tc.body), 200)
			if tc.enableFirst != "" {
				requireStatus(t, s.post(t, base+tc.enableFirst, defaultToken, nil), 200)
			}

			requireStatus(t, s.delete(t, base+tc.subPath, defaultToken), 204)

			// The branch is still protected: the top-level record survives.
			if resp := s.get(t, base, defaultToken); resp.StatusCode != 200 {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				t.Fatalf("GET protection after DELETE %s = %d, want 200 (branch must stay protected): %s",
					tc.subPath, resp.StatusCode, body)
			} else {
				resp.Body.Close()
			}

			// enforce_admins reports the toggle as off rather than answering
			// "Branch not protected" — that distinction is how a client tells
			// a disabled toggle from an unprotected branch.
			data := decodeJSONWithStatus(t, s.get(t, base+"/enforce_admins", defaultToken), 200)
			if tc.rule == "enforce_admins" && data["enabled"] != false {
				t.Fatalf("enforce_admins.enabled = %v, want false after DELETE", data["enabled"])
			}

			// Only DELETE .../protection unprotects the branch.
			requireStatus(t, s.delete(t, base, defaultToken), 204)
			requireStatus(t, s.get(t, base, defaultToken), 404)
		})
	}
}

// decodeStringArrayWithStatus asserts the status and decodes a JSON array of
// strings, the shape the /contexts sub-resource answers with.
func decodeStringArrayWithStatus(t *testing.T, resp *http.Response, want int) []string {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, want, body)
	}
	var out []string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode string array: %v", err)
	}
	return out
}

// decodeJSONWithStatus2xxArray asserts the status and decodes a JSON-array
// response body into a slice of objects.
func decodeJSONWithStatus2xxArray(t *testing.T, resp *http.Response, want int) []map[string]interface{} {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, want, body)
	}
	var out []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode array: %v", err)
	}
	return out
}

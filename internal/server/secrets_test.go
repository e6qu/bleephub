package bleephub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// shared fixtures for the secrets/variables suites

func mustStatus(t *testing.T, resp *http.Response, want int, what string) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: status %d, want %d (body: %s)", what, resp.StatusCode, want, body)
	}
}

// repository secrets

func TestSecretsPublicKey(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	ensureSeededRepo(s.Server, "owner/repo")
	resp := s.get(t, "/api/v3/repos/owner/repo/actions/secrets/public-key", defaultToken)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	kp, err := s.store.ActionsKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if body["key_id"] != kp.KeyID {
		t.Errorf("key_id = %v, want %s", body["key_id"], kp.KeyID)
	}
	if body["key"] != kp.PublicKey {
		t.Errorf("key = %v, want %s", body["key"], kp.PublicKey)
	}
}

func TestSecretsSealedRoundTrip(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "sealed-rt", false)
	path := "/api/v3/repos/" + repo.FullName + "/actions/secrets/ROUND_TRIP"

	mustStatus(t, s.putSealedSecret(t, path, "v1-plain"), 201, "create")

	secrets, _, err := s.actions.CollectJobSecretsAndVars(repo.FullName, "")
	if err != nil {
		t.Fatal(err)
	}
	if secrets["ROUND_TRIP"] != "v1-plain" {
		t.Fatalf("injected = %q, want v1-plain", secrets["ROUND_TRIP"])
	}

	mustStatus(t, s.putSealedSecret(t, path, "v2-plain"), 204, "update")

	secrets, _, err = s.actions.CollectJobSecretsAndVars(repo.FullName, "")
	if err != nil {
		t.Fatal(err)
	}
	if secrets["ROUND_TRIP"] != "v2-plain" {
		t.Fatalf("after update injected = %q, want v2-plain", secrets["ROUND_TRIP"])
	}
}

func TestSecretsPutWrongKeyID422(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	ensureSeededRepo(s.Server, "owner/repo")
	enc, keyID := s.sealForServer(t, "doesnt-matter")
	resp := s.put(t, "/api/v3/repos/owner/repo/actions/secrets/WRONG_KID", defaultToken,
		map[string]interface{}{"encrypted_value": enc, "key_id": keyID + "0"})
	mustStatus(t, resp, 422, "wrong key_id")
}

func TestSecretsPutBadCiphertext422(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	ensureSeededRepo(s.Server, "owner/repo")
	_, keyID := s.sealForServer(t, "x")
	cases := []struct {
		name string
		enc  string
	}{
		{"valid base64, not a sealed box", "Z2FyYmFnZS1ub3QtYS1zZWFsZWQtYm94LWF0LWFsbC1ub3BlCg=="},
		{"not base64", "!!!not-base64!!!"},
		{"empty encrypted_value", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := s.put(t, "/api/v3/repos/owner/repo/actions/secrets/BAD_CT", defaultToken,
				map[string]interface{}{"encrypted_value": tc.enc, "key_id": keyID})
			mustStatus(t, resp, 422, tc.name)
		})
	}
}

func TestSecretsPutMissingKeyID422(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	ensureSeededRepo(s.Server, "owner/repo")
	enc, _ := s.sealForServer(t, "x")
	resp := s.put(t, "/api/v3/repos/owner/repo/actions/secrets/NO_KID", defaultToken,
		map[string]interface{}{"encrypted_value": enc})
	mustStatus(t, resp, 422, "missing key_id")
}

func TestSecretsBadNames422(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	ensureSeededRepo(s.Server, "owner/repo")
	enc, keyID := s.sealForServer(t, "x")
	body := map[string]interface{}{"encrypted_value": enc, "key_id": keyID}
	for _, name := range []string{"1STARTS_WITH_DIGIT", "HAS-DASH", "GITHUB_RESERVED", "github_reserved"} {
		resp := s.put(t, "/api/v3/repos/owner/repo/actions/secrets/"+name, defaultToken, body)
		mustStatus(t, resp, 422, "bad name "+name)
	}
}

func TestSecretsListAndCaseInsensitiveGet(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "sec-list", false)
	base := "/api/v3/repos/" + repo.FullName + "/actions/secrets"
	mustStatus(t, s.putSealedSecret(t, base+"/lower_cased", "v"), 201, "create")

	list := decodeJSON(t, s.get(t, base, defaultToken))
	if int(list["total_count"].(float64)) != 1 {
		t.Fatalf("total_count = %v, want 1", list["total_count"])
	}
	item := list["secrets"].([]interface{})[0].(map[string]interface{})
	if item["name"] != "LOWER_CASED" {
		t.Errorf("name = %v, want LOWER_CASED (uppercased)", item["name"])
	}
	if _, leaked := item["value"]; leaked {
		t.Error("list response exposes value member")
	}

	// Real GitHub treats secret names case-insensitively.
	one := decodeJSON(t, s.get(t, base+"/LOWER_CASED", defaultToken))
	if one["name"] != "LOWER_CASED" {
		t.Errorf("get name = %v", one["name"])
	}
}

func TestSecretsListPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "sec-pg", "", false)
	if repo == nil {
		t.Fatal("create repo failed")
	}
	base := "/api/v3/repos/" + repo.FullName + "/actions/secrets"
	putPaginationSealedSecret(t, s, base+"/AAA_FIRST", "v", http.StatusCreated)
	putPaginationSealedSecret(t, s, base+"/BBB_SECOND", "v", http.StatusCreated)

	resp := tokenRequest(s, http.MethodGet, base+"?per_page=1", defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 1: %d %s", resp.Code, resp.Body.String())
	}
	link := resp.Header().Get("Link")
	var list map[string]interface{}
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode page 1: %v", err)
	}
	if int(list["total_count"].(float64)) != 2 {
		t.Fatalf("total_count = %v, want 2", list["total_count"])
	}
	page1 := list["secrets"].([]interface{})
	if len(page1) != 1 || page1[0].(map[string]interface{})["name"] != "AAA_FIRST" {
		t.Fatalf("page 1 = %v, want [AAA_FIRST]", page1)
	}
	if !strings.Contains(link, `rel="next"`) {
		t.Fatalf("Link = %q, want rel=next", link)
	}

	resp = tokenRequest(s, http.MethodGet, base+"?per_page=1&page=2", defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 2: %d %s", resp.Code, resp.Body.String())
	}
	list = nil
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode page 2: %v", err)
	}
	if int(list["total_count"].(float64)) != 2 {
		t.Fatalf("page 2 total_count = %v, want 2", list["total_count"])
	}
	page2 := list["secrets"].([]interface{})
	if len(page2) != 1 || page2[0].(map[string]interface{})["name"] != "BBB_SECOND" {
		t.Fatalf("page 2 = %v, want [BBB_SECOND]", page2)
	}
}

func TestSecretsValueNotExposed(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "sec-hidden", false)
	path := "/api/v3/repos/" + repo.FullName + "/actions/secrets/HIDDEN_VAL"
	mustStatus(t, s.putSealedSecret(t, path, "top-secret-plaintext"), 201, "create")

	resp := s.get(t, path, defaultToken)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if bytes.Contains(body, []byte("top-secret-plaintext")) {
		t.Error("GET response exposes secret value")
	}
}

func TestSecretsDelete(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "sec-del", false)
	path := "/api/v3/repos/" + repo.FullName + "/actions/secrets/DELETE_ME"
	mustStatus(t, s.putSealedSecret(t, path, "val"), 201, "create")
	mustStatus(t, s.delete(t, path, defaultToken), 204, "delete")
	mustStatus(t, s.get(t, path, defaultToken), 404, "get after delete")
}

func TestSecretsNoAuth401(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	resp, err := http.Get(s.baseURL + "/api/v3/repos/owner/repo/actions/secrets")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestSecretsMissingSecret404(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	mustStatus(t, s.get(t, "/api/v3/repos/nonexist/repo/actions/secrets/NOPE", defaultToken), 404, "missing secret")
}

// organization secrets

func TestOrgSecretsMissingOrg404(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	mustStatus(t, s.get(t, "/api/v3/orgs/no-such-org/actions/secrets", defaultToken), 404, "list")
	mustStatus(t, s.get(t, "/api/v3/orgs/no-such-org/actions/secrets/public-key", defaultToken), 404, "public-key")
	mustStatus(t, s.putSealedSecret(t, "/api/v3/orgs/no-such-org/actions/secrets/X", "v"), 404, "put")
}

func TestOrgSecretsVisibilityRequired422(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.seedTestOrg(t, "secorg-vis")
	enc, keyID := s.sealForServer(t, "v")
	resp := s.put(t, "/api/v3/orgs/"+org.Login+"/actions/secrets/NEEDS_VIS", defaultToken,
		map[string]interface{}{"encrypted_value": enc, "key_id": keyID})
	mustStatus(t, resp, 422, "missing visibility")

	resp = s.put(t, "/api/v3/orgs/"+org.Login+"/actions/secrets/NEEDS_VIS", defaultToken,
		map[string]interface{}{"encrypted_value": enc, "key_id": keyID, "visibility": "everyone"})
	mustStatus(t, resp, 422, "invalid visibility")
}

func TestOrgSecretsLifecycle(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.seedTestOrg(t, "secorg-life")
	selRepo := s.seedOrgRepo(t, org, "sel-target", false)
	base := "/api/v3/orgs/" + org.Login + "/actions/secrets"

	enc, keyID := s.sealForServer(t, "all-value")
	mustStatus(t, s.put(t, base+"/ORG_ALL", defaultToken, map[string]interface{}{
		"encrypted_value": enc, "key_id": keyID, "visibility": "all",
	}), 201, "create all")

	enc, keyID = s.sealForServer(t, "sel-value")
	mustStatus(t, s.put(t, base+"/ORG_SEL", defaultToken, map[string]interface{}{
		"encrypted_value": enc, "key_id": keyID, "visibility": "selected",
		"selected_repository_ids": []int{selRepo.ID},
	}), 201, "create selected")

	// List carries visibility; selected_repositories_url only on selected.
	list := decodeJSON(t, s.get(t, base, defaultToken))
	if int(list["total_count"].(float64)) != 2 {
		t.Fatalf("total_count = %v, want 2", list["total_count"])
	}
	for _, raw := range list["secrets"].([]interface{}) {
		item := raw.(map[string]interface{})
		_, hasURL := item["selected_repositories_url"]
		switch item["name"] {
		case "ORG_ALL":
			if item["visibility"] != "all" || hasURL {
				t.Errorf("ORG_ALL: visibility=%v hasURL=%v", item["visibility"], hasURL)
			}
		case "ORG_SEL":
			if item["visibility"] != "selected" || !hasURL {
				t.Errorf("ORG_SEL: visibility=%v hasURL=%v", item["visibility"], hasURL)
			}
		}
	}

	repos := decodeJSON(t, s.get(t, base+"/ORG_SEL/repositories", defaultToken))
	if int(repos["total_count"].(float64)) != 1 {
		t.Fatalf("repositories total_count = %v, want 1", repos["total_count"])
	}
	first := repos["repositories"].([]interface{})[0].(map[string]interface{})
	if int(first["id"].(float64)) != selRepo.ID {
		t.Errorf("repository id = %v, want %d", first["id"], selRepo.ID)
	}

	// Per-repo add/remove on a non-selected secret conflicts.
	idPath := fmt.Sprintf("%s/ORG_ALL/repositories/%d", base, selRepo.ID)
	mustStatus(t, s.put(t, idPath, defaultToken, nil), 409, "add to visibility=all")
	mustStatus(t, s.delete(t, idPath, defaultToken), 409, "remove from visibility=all")

	other := s.seedOrgRepo(t, org, "sel-other", true)
	addPath := fmt.Sprintf("%s/ORG_SEL/repositories/%d", base, other.ID)
	mustStatus(t, s.put(t, addPath, defaultToken, nil), 204, "add repo")
	repos = decodeJSON(t, s.get(t, base+"/ORG_SEL/repositories", defaultToken))
	if int(repos["total_count"].(float64)) != 2 {
		t.Fatalf("after add total_count = %v, want 2", repos["total_count"])
	}
	mustStatus(t, s.delete(t, addPath, defaultToken), 204, "remove repo")

	// Replace the selected set wholesale.
	mustStatus(t, s.put(t, base+"/ORG_SEL/repositories", defaultToken,
		map[string]interface{}{"selected_repository_ids": []int{other.ID}}), 204, "set repositories")
	repos = decodeJSON(t, s.get(t, base+"/ORG_SEL/repositories", defaultToken))
	if int(repos["total_count"].(float64)) != 1 {
		t.Fatalf("after set total_count = %v, want 1", repos["total_count"])
	}

	// Unknown repository id in the set → 404.
	mustStatus(t, s.put(t, base+"/ORG_SEL/repositories", defaultToken,
		map[string]interface{}{"selected_repository_ids": []int{999999}}), 404, "set unknown repo id")

	mustStatus(t, s.delete(t, base+"/ORG_ALL", defaultToken), 204, "delete")
	mustStatus(t, s.get(t, base+"/ORG_ALL", defaultToken), 404, "get after delete")
	mustStatus(t, s.delete(t, base+"/ORG_ALL", defaultToken), 404, "delete again")
}

func TestOrgSecretsListPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "secorg-pg", "secorg-pg", "")
	if org == nil {
		t.Fatal("create org failed")
	}
	base := "/api/v3/orgs/" + org.Login + "/actions/secrets"

	putOrgPaginationSecret := func(name string) {
		t.Helper()
		enc, keyID, err := s.store.SealSecretValue("v")
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		w := pagedJSONRequest(t, s, http.MethodPut, base+"/"+name, defaultToken, map[string]interface{}{
			"encrypted_value": enc, "key_id": keyID, "visibility": "all",
		})
		if w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s, want 201", name, w.Code, w.Body.String())
		}
	}
	putOrgPaginationSecret("AAA_FIRST")
	putOrgPaginationSecret("BBB_SECOND")

	resp := tokenRequest(s, http.MethodGet, base+"?per_page=1", defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 1: %d %s", resp.Code, resp.Body.String())
	}
	link := resp.Header().Get("Link")
	var list map[string]interface{}
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode page 1: %v", err)
	}
	if int(list["total_count"].(float64)) != 2 {
		t.Fatalf("total_count = %v, want 2", list["total_count"])
	}
	page1 := list["secrets"].([]interface{})
	if len(page1) != 1 || page1[0].(map[string]interface{})["name"] != "AAA_FIRST" {
		t.Fatalf("page 1 = %v, want [AAA_FIRST]", page1)
	}
	if !strings.Contains(link, `rel="next"`) {
		t.Fatalf("Link = %q, want rel=next", link)
	}

	resp = tokenRequest(s, http.MethodGet, base+"?per_page=1&page=2", defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 2: %d %s", resp.Code, resp.Body.String())
	}
	list = nil
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode page 2: %v", err)
	}
	page2 := list["secrets"].([]interface{})
	if len(page2) != 1 || page2[0].(map[string]interface{})["name"] != "BBB_SECOND" {
		t.Fatalf("page 2 = %v, want [BBB_SECOND]", page2)
	}
}

func TestOrgSecretReposListPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	admin := s.store.LookupUserByLogin("admin")
	org := s.store.CreateOrg(admin, "secorg-repo-pg", "secorg-repo-pg", "")
	if org == nil {
		t.Fatal("create org failed")
	}
	repo1 := s.store.CreateOrgRepo(org, admin, "pg-one", "", false)
	repo2 := s.store.CreateOrgRepo(org, admin, "pg-two", "", false)
	if repo1 == nil || repo2 == nil {
		t.Fatal("create org repos failed")
	}
	base := "/api/v3/orgs/" + org.Login + "/actions/secrets"

	enc, keyID, err := s.store.SealSecretValue("v")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	w := pagedJSONRequest(t, s, http.MethodPut, base+"/PG_SEL", defaultToken, map[string]interface{}{
		"encrypted_value": enc, "key_id": keyID, "visibility": "selected",
		"selected_repository_ids": []int{repo1.ID, repo2.ID},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create selected: %d %s, want 201", w.Code, w.Body.String())
	}

	resp := tokenRequest(s, http.MethodGet, base+"/PG_SEL/repositories?per_page=1", defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 1: %d %s", resp.Code, resp.Body.String())
	}
	link := resp.Header().Get("Link")
	var list map[string]interface{}
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode page 1: %v", err)
	}
	if int(list["total_count"].(float64)) != 2 {
		t.Fatalf("total_count = %v, want 2", list["total_count"])
	}
	page1 := list["repositories"].([]interface{})
	if len(page1) != 1 || int(page1[0].(map[string]interface{})["id"].(float64)) != repo1.ID {
		t.Fatalf("page 1 = %v, want [%d]", page1, repo1.ID)
	}
	if !strings.Contains(link, `rel="next"`) {
		t.Fatalf("Link = %q, want rel=next", link)
	}

	resp = tokenRequest(s, http.MethodGet, base+"/PG_SEL/repositories?per_page=1&page=2", defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 2: %d %s", resp.Code, resp.Body.String())
	}
	list = nil
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode page 2: %v", err)
	}
	page2 := list["repositories"].([]interface{})
	if len(page2) != 1 || int(page2[0].(map[string]interface{})["id"].(float64)) != repo2.ID {
		t.Fatalf("page 2 = %v, want [%d]", page2, repo2.ID)
	}
}

// environment secrets

func TestEnvSecretsMissingEnv404(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "env-missing", false)
	base := "/api/v3/repos/" + repo.FullName + "/environments/ghost/secrets"
	mustStatus(t, s.get(t, base, defaultToken), 404, "list")
	mustStatus(t, s.get(t, base+"/public-key", defaultToken), 404, "public-key")
	// Real GitHub does NOT auto-create the environment on secret PUT.
	mustStatus(t, s.putSealedSecret(t, base+"/NEW_SECRET", "v"), 404, "put")
	if env := s.store.Deployments.GetEnvironment(repo.ID, "ghost"); env != nil {
		t.Error("PUT must not auto-create the environment")
	}
}

func TestEnvSecretsLifecycle(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	repo := s.seedRepo(t, "env-life", false)
	s.store.Deployments.UpsertEnvironment(repo.ID, "production")
	base := "/api/v3/repos/" + repo.FullName + "/environments/production/secrets"

	pk := decodeJSON(t, s.get(t, base+"/public-key", defaultToken))
	if pk["key_id"] == "" || pk["key"] == "" {
		t.Fatalf("public-key incomplete: %v", pk)
	}

	mustStatus(t, s.putSealedSecret(t, base+"/ENV_ONLY", "env-plain"), 201, "create")
	mustStatus(t, s.putSealedSecret(t, base+"/ENV_ONLY", "env-plain-2"), 204, "update")

	list := decodeJSON(t, s.get(t, base, defaultToken))
	if int(list["total_count"].(float64)) != 1 {
		t.Fatalf("total_count = %v, want 1", list["total_count"])
	}

	one := decodeJSON(t, s.get(t, base+"/ENV_ONLY", defaultToken))
	if one["name"] != "ENV_ONLY" {
		t.Errorf("name = %v", one["name"])
	}

	secrets, _, err := s.actions.CollectJobSecretsAndVars(repo.FullName, "production")
	if err != nil {
		t.Fatal(err)
	}
	if secrets["ENV_ONLY"] != "env-plain-2" {
		t.Errorf("injected env secret = %q, want env-plain-2", secrets["ENV_ONLY"])
	}

	mustStatus(t, s.delete(t, base+"/ENV_ONLY", defaultToken), 204, "delete")
	mustStatus(t, s.delete(t, base+"/ENV_ONLY", defaultToken), 404, "delete again")
}

func TestEnvSecretsListPagination(t *testing.T) {
	s := newTestServer()
	s.registerRoutes()
	admin := s.store.LookupUserByLogin("admin")
	repo := s.store.CreateRepo(admin, "env-pg", "", false)
	if repo == nil {
		t.Fatal("create repo failed")
	}
	s.store.Deployments.UpsertEnvironment(repo.ID, "staging")
	base := "/api/v3/repos/" + repo.FullName + "/environments/staging/secrets"

	putPaginationSealedSecret(t, s, base+"/AAA_FIRST", "v", http.StatusCreated)
	putPaginationSealedSecret(t, s, base+"/BBB_SECOND", "v", http.StatusCreated)

	resp := tokenRequest(s, http.MethodGet, base+"?per_page=1", defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 1: %d %s", resp.Code, resp.Body.String())
	}
	link := resp.Header().Get("Link")
	var list map[string]interface{}
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode page 1: %v", err)
	}
	if int(list["total_count"].(float64)) != 2 {
		t.Fatalf("total_count = %v, want 2", list["total_count"])
	}
	page1 := list["secrets"].([]interface{})
	if len(page1) != 1 || page1[0].(map[string]interface{})["name"] != "AAA_FIRST" {
		t.Fatalf("page 1 = %v, want [AAA_FIRST]", page1)
	}
	if !strings.Contains(link, `rel="next"`) {
		t.Fatalf("Link = %q, want rel=next", link)
	}

	resp = tokenRequest(s, http.MethodGet, base+"?per_page=1&page=2", defaultToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("page 2: %d %s", resp.Code, resp.Body.String())
	}
	list = nil
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode page 2: %v", err)
	}
	page2 := list["secrets"].([]interface{})
	if len(page2) != 1 || page2[0].(map[string]interface{})["name"] != "BBB_SECOND" {
		t.Fatalf("page 2 = %v, want [BBB_SECOND]", page2)
	}
}

// repo-visible organization secrets

func TestRepoOrganizationSecretsList(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	org := s.seedTestOrg(t, "secorg-repovis")
	pubRepo := s.seedOrgRepo(t, org, "vis-pub", false)
	privRepo := s.seedOrgRepo(t, org, "vis-priv", true)
	base := "/api/v3/orgs/" + org.Login + "/actions/secrets"

	enc, keyID := s.sealForServer(t, "v")
	mustStatus(t, s.put(t, base+"/VIS_ALL", defaultToken, map[string]interface{}{
		"encrypted_value": enc, "key_id": keyID, "visibility": "all",
	}), 201, "create all")
	enc, keyID = s.sealForServer(t, "v")
	mustStatus(t, s.put(t, base+"/VIS_PRIV", defaultToken, map[string]interface{}{
		"encrypted_value": enc, "key_id": keyID, "visibility": "private",
	}), 201, "create private")

	names := func(repo *store.Repo) map[string]bool {
		body := decodeJSON(t, s.get(t, "/api/v3/repos/"+repo.FullName+"/actions/organization-secrets", defaultToken))
		out := map[string]bool{}
		for _, raw := range body["secrets"].([]interface{}) {
			item := raw.(map[string]interface{})
			out[item["name"].(string)] = true
			// The documented repo-level item shape is the plain
			// actions-secret: no visibility member.
			if _, has := item["visibility"]; has {
				t.Errorf("repo-level org secret item leaks visibility: %v", item)
			}
		}
		return out
	}

	pub := names(pubRepo)
	if !pub["VIS_ALL"] || pub["VIS_PRIV"] {
		t.Errorf("public repo sees %v, want VIS_ALL only", pub)
	}
	priv := names(privRepo)
	if !priv["VIS_ALL"] || !priv["VIS_PRIV"] {
		t.Errorf("private repo sees %v, want VIS_ALL and VIS_PRIV", priv)
	}

	mustStatus(t, s.get(t, "/api/v3/repos/admin/no-such-repo/actions/organization-secrets", defaultToken), 404, "missing repo")
}

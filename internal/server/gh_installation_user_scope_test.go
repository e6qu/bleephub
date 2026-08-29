package bleephub

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// loadOperationsClosedToGitHubApps returns the normalized "METHOD /path"
// operations the vendored GitHub description marks unavailable to a GitHub App
// installation token, via `x-github.enabledForGitHubApps: false`.
//
// This is GitHub's own record of which operations refuse an installation token,
// published alongside them, so a refusal gate measured against it cannot drift
// from the contract.
func loadOperationsClosedToGitHubApps(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(vendoredSpecFile)
	if err != nil {
		t.Fatalf("open vendored OpenAPI: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gunzip OpenAPI: %v", err)
	}
	defer gz.Close()

	var doc struct {
		Paths map[string]map[string]struct {
			XGitHub struct {
				EnabledForGitHubApps *bool `json:"enabledForGitHubApps"`
			} `json:"x-github"`
		} `json:"paths"`
	}
	if err := json.NewDecoder(gz).Decode(&doc); err != nil {
		t.Fatalf("decode OpenAPI: %v", err)
	}
	if len(doc.Paths) < 500 {
		t.Fatalf("vendored OpenAPI looks truncated: only %d paths", len(doc.Paths))
	}

	closed := make(map[string]bool)
	for path, methods := range doc.Paths {
		norm := normalizePath(path)
		for method, op := range methods {
			switch method {
			case "get", "post", "put", "patch", "delete", "head":
			default:
				continue
			}
			if op.XGitHub.EnabledForGitHubApps != nil && !*op.XGitHub.EnabledForGitHubApps {
				closed[strings.ToUpper(method)+" "+norm] = true
			}
		}
	}
	// The marker is widespread in the description; an empty result means the
	// field moved or was renamed, not that GitHub opened everything up.
	if len(closed) < 100 {
		t.Fatalf("only %d operations marked enabledForGitHubApps:false — "+
			"the vendored description no longer carries that marker where this gate reads it", len(closed))
	}
	return closed
}

// TestInstallationTokenRefusalMatchesVendoredContract holds the
// installation-token refusal against GitHub's published record of it. Every
// route this server refuses an installation token on must be an operation
// GitHub itself marks `enabledForGitHubApps: false`.
//
// The assertion runs one way on purpose. Refusing an operation GitHub serves
// to installations is a regression this must catch: it takes a working App
// integration and breaks it. Not yet refusing one GitHub closes is a narrower
// gate than the contract allows, which is why the count floor below keeps the
// gate from quietly shrinking to nothing.
func TestInstallationTokenRefusalMatchesVendoredContract(t *testing.T) {
	t.Parallel()
	closed := loadOperationsClosedToGitHubApps(t)
	s := newIsolatedServer(t)

	refused := 0
	for _, pattern := range s.routePatterns {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Fatalf("route pattern %q has no method", pattern)
		}
		if path != "/api/v3/user" && !strings.HasPrefix(path, "/api/v3/user/") {
			continue
		}
		normalized := method + " " + normalizePath(strings.TrimPrefix(path, "/api/v3"))

		if !routeIsUserAccountScoped(pattern) {
			// Declared reachable: the description must actually leave it open,
			// or the exemption is holding a refusal off a route GitHub closes.
			if closed[normalized] {
				t.Errorf("route %q is exempted from the installation-token refusal, but the vendored "+
					"description marks %q as enabledForGitHubApps:false — an installation token reaches "+
					"an operation GitHub closes to it", pattern, normalized)
			}
			continue
		}
		refused++
		if closed[normalized] {
			continue
		}
		// A wildcard that fans out to several real GitHub sub-resources has no
		// single operation of its own to look up; it is refused because every
		// operation it dispatches to is closed, which the dispatch table names.
		if _, isDispatch := dispatchRoutes[normalized]; isDispatch {
			continue
		}
		t.Errorf("route %q refuses an installation token, but the vendored description does not mark "+
			"%q as enabledForGitHubApps:false — refusing an operation GitHub serves to installations "+
			"breaks every App that calls it. Add it to installationReachableUserRoutes.", pattern, normalized)
	}
	// The whole /api/v3/user surface is account-scoped bar a handful of
	// exemptions; a gate matching only a few routes would mean the prefix
	// stopped matching, and the refusal quietly stopped applying.
	if refused < 40 {
		t.Fatalf("only %d account-scoped routes are gated, want at least 40 — "+
			"routeIsUserAccountScoped no longer matches the /api/v3/user surface", refused)
	}
}

// TestInstallationTokenCannotReadTheAuthenticatedUser pins the distinction a
// client depends on to tell an App credential from a person's. An installation
// token holds no account, so the endpoints describing "the authenticated user"
// refuse it rather than describing the App's bot actor as though it were the
// signed-in user.
func TestInstallationTokenCannotReadTheAuthenticatedUser(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	perms := map[string]string{"contents": "write", "metadata": "read"}
	app := s.store.CreateApp(1, "User Scope App", "", perms, nil)
	inst := s.store.CreateInstallation(app.ID, "User", 1, "admin", perms, nil)
	instToken := s.store.CreateInstallationToken(inst.ID, app.ID, perms, nil)

	for _, path := range []string{
		"/api/v3/user",
		"/api/v3/user/repos",
		"/api/v3/user/emails",
		"/api/v3/user/orgs",
		"/api/v3/user/installations",
		"/api/v3/user/keys",
		"/api/v3/user/starred",
	} {
		resp := s.get(t, path, instToken.Token)
		data := decodeJSONWithStatus(t, resp, http.StatusForbidden)
		if data["message"] != "Resource not accessible by integration" {
			t.Errorf("GET %s with an installation token: message = %v, want %q",
				path, data["message"], "Resource not accessible by integration")
		}
	}

	// The bot actor still exists as an author — the refusal is about the
	// credential having no account, not about the App having no identity. A
	// user credential reads the same endpoint fine.
	requireStatus(t, s.get(t, "/api/v3/user", defaultToken), http.StatusOK)
}

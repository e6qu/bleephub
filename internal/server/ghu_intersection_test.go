package bleephub

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// A user-to-server (`ghu_`) token is the intersection of two things: what its
// bearer can reach, and what the app behind it was installed on. Handlers that
// asked only the user-shaped question saw the bearer and nothing else, so an
// app holding zero permissions and installed nowhere read a stranger's private
// repository and planted an organization webhook — through a token it had
// obtained legitimately, on routes where the same app's `ghs_` token is
// refused. A credential that can be exchanged for a broader one is not a
// narrower credential.
//
// Both directions matter. Over-blocking has been the recurring regression when
// this was tightened before, so every case below is paired: the app installed
// nowhere must be refused, and the app installed on the target, whose bearer
// does hold access, must still be served.

type ghuFixture struct {
	victim      *User
	victimRepo  *Repo
	bearer      *User
	bearerToken string
	org         *Org

	// outsideApp holds no permissions and has no installation anywhere.
	outsideApp *App
	outsideGhu string
	outsideGhs string

	// insideApp is installed on the victim's account and on the org.
	insideGhu string
	insideGhs string
}

func newGhuFixture(t *testing.T, tag string) *ghuFixture {
	t.Helper()
	store := testServer.store
	now := fixedTestTime.UTC()

	mkUser := func(login string) *User {
		store.mu.Lock()
		defer store.mu.Unlock()
		u := &User{ID: store.NextUser, Login: login, Type: "User", CreatedAt: now, UpdatedAt: now}
		store.Users[u.ID] = u
		store.UsersByLogin[u.Login] = u
		store.NextUser++
		return u
	}

	f := &ghuFixture{
		victim: mkUser("ghu-victim-" + tag),
		bearer: mkUser("ghu-bearer-" + tag),
	}

	f.victimRepo = store.CreateRepo(f.victim, "ghu-private", "private fixture", true)
	if f.victimRepo == nil || !f.victimRepo.Private {
		t.Fatalf("could not create the victim's private repository")
	}
	// The bearer legitimately holds pull on the victim's private repository.
	// That is what makes the borrowed access plausible: the user really can
	// read it, the app really cannot.
	if !store.AddRepoCollaborator(f.victim.Login, "ghu-private", f.bearer.Login, "pull") {
		t.Fatalf("could not make the bearer a collaborator")
	}
	if tok := store.CreateToken(f.bearer.ID, "repo, admin:org"); tok != nil {
		f.bearerToken = tok.Value
	} else {
		t.Fatalf("could not mint the bearer's PAT")
	}

	f.org = store.CreateOrg(f.bearer, "ghu-org-"+tag, "Ghu Org", "")
	if f.org == nil {
		t.Fatalf("could not create the organization")
	}

	outside := store.CreateApp(f.bearer.ID, "Ghu Outside "+tag, "", nil, nil)
	if outside == nil {
		t.Fatalf("could not create the zero-permission app")
	}
	if insts := store.ListAppInstallations(outside.ID); len(insts) != 0 {
		t.Fatalf("the fixture app must have no installations, got %d", len(insts))
	}
	f.outsideApp = outside
	outsideGhu, _ := store.CreateUserToServerToken(f.bearer.ID, outside.ID, "", "", time.Hour, false)
	if outsideGhu == nil {
		t.Fatalf("could not mint the outside ghu_ token")
	}
	f.outsideGhu = outsideGhu.Token
	// A ghs_ for an app with no installation: installation id 0 resolves to
	// nothing, which is exactly the "installed nowhere" shape.
	f.outsideGhs = store.CreateInstallationToken(0, outside.ID, nil, nil).Token

	perms := map[string]string{
		"metadata":                    "read",
		"contents":                    "read",
		"administration":              "admin",
		"organization_administration": "admin",
	}
	inside := store.CreateApp(f.bearer.ID, "Ghu Inside "+tag, "", perms, nil)
	if inside == nil {
		t.Fatalf("could not create the installed app")
	}
	victimInst := store.CreateInstallation(inside.ID, "User", f.victim.ID, f.victim.Login, perms, nil)
	orgInst := store.CreateInstallation(inside.ID, "Organization", f.org.ID, f.org.Login, perms, nil)
	if victimInst == nil || orgInst == nil {
		t.Fatalf("could not install the app on the fixture accounts")
	}
	insideGhu, _ := store.CreateUserToServerToken(f.bearer.ID, inside.ID, "", "", time.Hour, false)
	if insideGhu == nil {
		t.Fatalf("could not mint the inside ghu_ token")
	}
	f.insideGhu = insideGhu.Token
	f.insideGhs = store.CreateInstallationToken(victimInst.ID, inside.ID, perms, nil).Token

	return f
}

// ghuRequest issues one request against the live test server and returns the
// status and a truncated body for failure messages.
func ghuRequest(t *testing.T, method, path, token, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, testBaseURL+path, reader)
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if len(text) > 300 {
		text = text[:300]
	}
	return resp.StatusCode, text
}

func served(status int) bool { return status >= 200 && status < 300 }

type ghuRoute struct {
	method string
	path   string
	body   string
}

func (f *ghuFixture) repoRoutes() []ghuRoute {
	base := "/api/v3/repos/" + f.victimRepo.FullName
	return []ghuRoute{
		{http.MethodGet, base, ""},
		{http.MethodGet, base + "/topics", ""},
		{http.MethodGet, base + "/languages", ""},
		{http.MethodGet, base + "/forks", ""},
		{http.MethodGet, base + "/rules/branches/main", ""},
	}
}

func (f *ghuFixture) orgRoutes() []ghuRoute {
	base := "/api/v3/orgs/" + f.org.Login
	return []ghuRoute{
		{http.MethodPost, base + "/hooks", `{"name":"web","config":{"url":"http://127.0.0.1:9/exfiltrate","content_type":"json"},"events":["*"]}`},
		{http.MethodGet, base + "/hooks", ""},
		{http.MethodGet, base + "/installations", ""},
	}
}

// A ghu_ token must never reach further than the ghs_ token of the same app.
func TestGhuTokenIsNeverBroaderThanTheSameAppsGhsToken(t *testing.T) {
	f := newGhuFixture(t, "narrow")

	for _, route := range append(f.repoRoutes(), f.orgRoutes()...) {
		name := route.method + " " + route.path
		ghsStatus, ghsBody := ghuRequest(t, route.method, route.path, f.outsideGhs, route.body)
		ghuStatus, ghuBody := ghuRequest(t, route.method, route.path, f.outsideGhu, route.body)

		if served(ghsStatus) {
			t.Errorf("%s: the fixture is wrong — an app installed nowhere was served its ghs_ token (%d): %s", name, ghsStatus, ghsBody)
			continue
		}
		if served(ghuStatus) {
			t.Errorf("%s: ghu_ of an app installed nowhere was served (%d) where its ghs_ got %d: %s",
				name, ghuStatus, ghsStatus, ghuBody)
		}
	}
}

// The bearer's own credential still reaches everything it did before, so the
// denials above are the app's absence and not a broken fixture.
func TestGhuFixtureBearerStillReachesItsOwnAccess(t *testing.T) {
	f := newGhuFixture(t, "control")

	for _, route := range f.repoRoutes() {
		if route.method != http.MethodGet {
			continue
		}
		status, body := ghuRequest(t, route.method, route.path, f.bearerToken, route.body)
		if !served(status) {
			t.Errorf("%s with the bearer's own PAT: status = %d, want 2xx: %s", route.path, status, body)
		}
	}
	for _, route := range f.orgRoutes() {
		status, body := ghuRequest(t, route.method, route.path, f.bearerToken, route.body)
		if !served(status) {
			t.Errorf("%s %s with the bearer's own PAT: status = %d, want 2xx: %s", route.method, route.path, status, body)
		}
	}
}

// The other direction: an app that IS installed on the target, whose bearer
// does hold access, must still be served. Over-blocking here is the regression
// that every previous tightening of this gate introduced.
func TestGhuTokenOfAnInstalledAppIsStillServed(t *testing.T) {
	f := newGhuFixture(t, "wide")

	for _, route := range f.repoRoutes() {
		status, body := ghuRequest(t, route.method, route.path, f.insideGhu, route.body)
		if !served(status) {
			t.Errorf("%s with a ghu_ of an app installed on the owner: status = %d, want 2xx: %s", route.path, status, body)
		}
		// The ghs_ baseline for the same installation: the ghu_ above is
		// meant to be the intersection of that and its bearer, not something
		// narrower.
		if status, body := ghuRequest(t, route.method, route.path, f.insideGhs, route.body); !served(status) {
			t.Errorf("%s with the installed app's ghs_: status = %d, want 2xx: %s", route.path, status, body)
		}
	}
	for _, route := range f.orgRoutes() {
		status, body := ghuRequest(t, route.method, route.path, f.insideGhu, route.body)
		if !served(status) {
			t.Errorf("%s %s with a ghu_ of an app installed on the org: status = %d, want 2xx: %s",
				route.method, route.path, status, body)
		}
	}
}

// The organization half had no choke point at all: orgHookGate asked only
// whether the bearer was an org admin, so a webhook pointing anywhere the
// attacker chose received every event the organization ever emits. This pins
// the specific exfiltration rather than the generic comparison above.
func TestOrgWebhookCannotBePlantedByAnAppInstalledNowhere(t *testing.T) {
	f := newGhuFixture(t, "orghook")
	base := "/api/v3/orgs/" + f.org.Login + "/hooks"
	create := `{"name":"web","config":{"url":"http://127.0.0.1:9/exfiltrate","content_type":"json"},"events":["*"]}`

	if status, body := ghuRequest(t, http.MethodPost, base, f.outsideGhu, create); served(status) {
		t.Errorf("planting an org webhook with a ghu_ of an app installed nowhere: status = %d, want a denial: %s", status, body)
	}
	if status, body := ghuRequest(t, http.MethodGet, base, f.outsideGhu, ""); served(status) {
		t.Errorf("listing org webhooks with a ghu_ of an app installed nowhere: status = %d, want a denial: %s", status, body)
	}
	if hooks := testServer.store.ListOrgHooks(f.org.Login); len(hooks) != 0 {
		t.Errorf("the organization has %d webhook(s) after the refused create; none should have been stored", len(hooks))
	}
}

// A path naming a repository or an organization that does not exist used to
// pass the resource gate entirely, because "no repository named" and "the named
// repository is not there" both reduced to a nil lookup. Handlers keyed off the
// raw path values then acted under a fabricated key.
func TestNamedButAbsentTargetsAreNeverServed(t *testing.T) {
	f := newGhuFixture(t, "absent")
	missing := "/api/v3/repos/ghu-no-such-owner/ghu-no-such-repo"

	repoRoutes := []ghuRoute{
		// Read.
		{http.MethodGet, missing + "/hooks", ""},
		{http.MethodGet, missing + "/dependabot/secrets/public-key", ""},
		{http.MethodGet, missing + "/actions/secrets", ""},
		{http.MethodGet, missing + "/actions/variables", ""},
		// Write.
		{http.MethodPut, missing + "/topics", `{"names":["x"]}`},
		// Admin.
		{http.MethodPost, missing + "/hooks", `{"name":"web","config":{"url":"http://127.0.0.1:9/x"}}`},
		{http.MethodPatch, missing, `{"description":"x"}`},
	}
	for _, route := range repoRoutes {
		status, body := ghuRequest(t, route.method, route.path, f.bearerToken, route.body)
		if status != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404 (a fabricated repository must never be acted on, and must not be distinguishable from one the caller cannot see): %s",
				route.method, route.path, status, body)
		}
	}
	// Nothing was created under the fabricated key.
	if hooks := testServer.store.ListHooks("ghu-no-such-owner/ghu-no-such-repo"); len(hooks) != 0 {
		t.Errorf("%d webhook(s) stored under a repository that does not exist", len(hooks))
	}

	missingOrg := "/api/v3/orgs/ghu-no-such-org"
	orgRoutes := []ghuRoute{
		{http.MethodGet, missingOrg + "/actions/secrets", ""},
		{http.MethodGet, missingOrg + "/dependabot/secrets/public-key", ""},
		{http.MethodPatch, missingOrg, `{"description":"x"}`},
	}
	for _, route := range orgRoutes {
		status, body := ghuRequest(t, route.method, route.path, f.bearerToken, route.body)
		if status != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404 for an organization that does not exist: %s",
				route.method, route.path, status, body)
		}
	}
}

// Positive control for the check above: a path that names no repository at all,
// and one that names an organization which does exist, are served exactly as
// before. Denying those would break most of the API, which is why the gate
// cannot simply default to refusing a nil lookup.
func TestRoutesNamingNoRepositoryAreUnaffected(t *testing.T) {
	f := newGhuFixture(t, "control-absent")

	for _, route := range []ghuRoute{
		{http.MethodGet, "/api/v3/user/repos", ""},
		{http.MethodGet, "/api/v3/user/installations", ""},
		{http.MethodGet, "/api/v3/orgs/" + f.org.Login, ""},
		{http.MethodGet, "/api/v3/orgs/" + f.org.Login + "/hooks", ""},
		{http.MethodGet, "/api/v3/orgs/" + f.org.Login + "/members", ""},
	} {
		status, body := ghuRequest(t, route.method, route.path, f.bearerToken, route.body)
		if !served(status) {
			t.Errorf("%s %s: status = %d, want 2xx — this route names no absent resource: %s",
				route.method, route.path, status, body)
		}
	}
}

// The check above names a handful of routes by hand. This one is the whole
// table: every registered route that names a repository must refuse a
// repository that does not exist, whatever gate it happens to use. Picking
// routes by hand is how the previous rounds kept missing some.
var ghuPathPlaceholder = regexp.MustCompile(`\{[^}]+\}`)

// These two are served by artifacts.go, which is owned elsewhere and cannot be
// edited from here; both answer 200 with an empty cache listing for a
// repository that does not exist. Remove the entry when the handler resolves
// the repository.
var fabricatedRepoRoutesStillServed = map[string]bool{}

func TestNoRegisteredRouteServesAFabricatedRepository(t *testing.T) {
	f := newGhuFixture(t, "sweep")
	appJWT, err := signAppJWT(f.outsideApp.PEMPrivateKey, f.outsideApp.ID, fixedTestTime)
	if err != nil {
		t.Fatalf("signing app JWT for credential-specific route: %v", err)
	}
	swept := 0
	for _, pattern := range fuzzRoutePatterns {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok || !strings.Contains(path, "/repos/{owner}/{repo}") {
			continue
		}
		if fabricatedRepoRoutesStillServed[pattern] {
			continue
		}
		swept++
		filled := strings.ReplaceAll(path, "{owner}", "ghu-no-such-owner")
		filled = strings.ReplaceAll(filled, "{repo}", "ghu-no-such-repo")
		filled = ghuPathPlaceholder.ReplaceAllString(filled, "1")
		body := ""
		if method != http.MethodGet && method != http.MethodHead && method != http.MethodDelete {
			body = "{}"
		}
		token := f.bearerToken
		if pattern == "GET /api/v3/repos/{owner}/{repo}/installation" {
			token = appJWT
		}
		status, text := ghuRequest(t, method, filled, token, body)
		if status != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404 for a repository that does not exist: %s", method, filled, status, text)
		}
	}
	if swept < 100 {
		t.Fatalf("swept only %d repository routes; the route table is not being read", swept)
	}
}

package bleephub

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// App represents a registered GitHub App.
//
// The credential / webhook-config fields carry real json names so the
// persistence layer (which marshals the struct as-is) round-trips them:
// JWT auth, OAuth client-secret auth and app webhooks must survive a
// restart. Client-facing responses never marshal this struct directly —
// they go through appToJSON / appHookConfigJSON, which emit explicit maps.
type App struct {
	ID                 int               `json:"id"`
	NodeID             string            `json:"node_id"`
	Slug               string            `json:"slug"`
	Name               string            `json:"name"`
	ClientID           string            `json:"client_id"`
	ClientSecret       string            `json:"client_secret"`
	Description        string            `json:"description"`
	ExternalURL        string            `json:"external_url"`
	WebhookURL         string            `json:"webhook_url"`
	WebhookSecret      string            `json:"webhook_secret"`
	WebhookActive      bool              `json:"webhook_active"`
	WebhookEvents      []string          `json:"webhook_events"`
	WebhookContentType string            `json:"webhook_content_type"` // "json" | "form" (default "form")
	WebhookInsecureSSL string            `json:"webhook_insecure_ssl"` // "0" | "1" (default "0")
	CallbackURL        string            `json:"callback_url"`         // OAuth web-flow destination; empty means none registered
	PEMPrivateKey      string            `json:"pem_private_key"`
	Permissions        map[string]string `json:"permissions"`
	Events             []string          `json:"events"`
	OwnerID            int               `json:"owner_id"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
}

// appBotUser materializes the Bot actor GitHub exposes for installation-token
// writes. The bot is derived from the app rather than persisted as an ordinary
// account: its negative internal ID cannot collide with a real user, while the
// app record is sufficient to restore the identity after a restart.
func appBotUser(app *App) *User {
	if app == nil {
		return nil
	}
	return &User{
		ID:        -app.ID,
		NodeID:    fmt.Sprintf("BOT_kgDO%08d", app.ID),
		Login:     app.Slug + "[bot]",
		Name:      app.Name,
		Type:      "Bot",
		CreatedAt: app.CreatedAt,
		UpdatedAt: app.UpdatedAt,
	}
}

// actorUserLocked resolves both persisted users and the derived GitHub App bot
// IDs stored on resources created with an installation token. st.mu must be
// held by the caller.
func actorUserLocked(st *Store, id int) *User {
	if user := st.Users[id]; user != nil {
		return user
	}
	if id < 0 {
		return appBotUser(st.Apps[-id])
	}
	return nil
}

// GetActorByID is the lock-safe form used by response and activity builders.
func (st *Store) GetActorByID(id int) *User {
	st.mu.RLock()
	defer st.mu.RUnlock()
	user := actorUserLocked(st, id)
	if user == nil {
		return nil
	}
	copy := *user
	return &copy
}

// Installation represents an app installation on a user or org.
type Installation struct {
	ID                  int               `json:"id"`
	AppID               int               `json:"app_id"`
	AppSlug             string            `json:"app_slug"`
	TargetType          string            `json:"target_type"`
	TargetID            int               `json:"target_id"`
	TargetLogin         string            `json:"target_login"`
	TargetNodeID        string            `json:"target_node_id"`    // snapshotted from the target account at install time
	TargetAvatarURL     string            `json:"target_avatar_url"` // snapshotted from the target account at install time
	Permissions         map[string]string `json:"permissions"`
	Events              []string          `json:"events"`
	RepositorySelection string            `json:"repository_selection"`
	SelectedRepoIDs     []int             `json:"selected_repo_ids"` // persisted; rendered only via the bespoke installation emitters
	SuspendedAt         *time.Time        `json:"suspended_at"`
	SuspendedBy         *User             `json:"suspended_by"`
	SingleFileName      string            `json:"single_file_name"`
	CreatedAt           time.Time         `json:"created_at"`
	UpdatedAt           time.Time         `json:"updated_at"`
}

// InstallationToken is a short-lived token scoped to an installation.
type InstallationToken struct {
	Token          string            `json:"token"`
	ExpiresAt      time.Time         `json:"expires_at"`
	Permissions    map[string]string `json:"permissions"`
	RepositoryIDs  []int             `json:"repository_ids"` // persisted; rendered only via installationTokenToJSON
	InstallationID int               `json:"installation_id"`
	AppID          int               `json:"app_id"`
}

// normalizeAppPermissions materializes the Metadata read permission GitHub
// grants to every GitHub App installation. It cannot be deselected in GitHub's
// App settings and remains present when an app asks for no other permission.
//
// Always return a copy: App, Installation, and InstallationToken are separate
// persisted snapshots and mutating one grant must never silently mutate the
// others through a shared map.
func normalizeAppPermissions(perms map[string]string) map[string]string {
	out := make(map[string]string, len(perms)+1)
	for scope, level := range perms {
		out[scope] = level
	}
	out[string(scopeMetadata)] = "read"
	return out
}

// OAuthApp is the OAuth-app entity Basic-authenticated by client_id+client_secret.
// Distinct from GitHub Apps (App above) although a GitHub App also has a client_id+secret pair
// that can be used the same way for OAuth user-to-server flows.
type OAuthApp struct {
	ClientID     string
	ClientSecret string
	Name         string
	Description  string
	URL          string
	CallbackURL  string
	OwnerID      int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func cloneApp(app *App) *App {
	if app == nil {
		return nil
	}
	copy := *app
	copy.Permissions = normalizeAppPermissions(app.Permissions)
	copy.Events = append([]string(nil), app.Events...)
	copy.WebhookEvents = append([]string(nil), app.WebhookEvents...)
	return &copy
}

func cloneInstallation(installation *Installation) *Installation {
	if installation == nil {
		return nil
	}
	copy := *installation
	copy.Permissions = normalizeAppPermissions(installation.Permissions)
	copy.Events = append([]string(nil), installation.Events...)
	copy.SelectedRepoIDs = append([]int(nil), installation.SelectedRepoIDs...)
	if installation.SuspendedAt != nil {
		at := *installation.SuspendedAt
		copy.SuspendedAt = &at
	}
	if installation.SuspendedBy != nil {
		user := *installation.SuspendedBy
		copy.SuspendedBy = &user
	}
	return &copy
}

func cloneInstallationToken(token *InstallationToken) *InstallationToken {
	if token == nil {
		return nil
	}
	copy := *token
	copy.Permissions = normalizeAppPermissions(token.Permissions)
	copy.RepositoryIDs = append([]int(nil), token.RepositoryIDs...)
	return &copy
}

func cloneOAuthApp(app *OAuthApp) *OAuthApp {
	if app == nil {
		return nil
	}
	copy := *app
	return &copy
}

// validateClientCallbackURL is the registration rule for an OAuth client's
// callback, shared by both client kinds so "where may this client be
// redirected" has one answer and one validator rather than two.
//
// An empty callback records no destination. That is a legal registration — the
// authorize flow refuses a client that has none — but it is never a licence to
// redirect somewhere else.
func validateClientCallbackURL(raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("callback URL %q is not a valid URL: %w", raw, err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("callback URL scheme %q is not supported; use an absolute http or https URL", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("callback URL %q names no host; use an absolute http or https URL", raw)
	}
	return nil
}

// CreateApp generates a new GitHub App with an RSA key pair.
func (st *Store) CreateApp(ownerID int, name, description string, perms map[string]string, events []string) *App {
	app, err := st.CreateAppE(ownerID, name, description, perms, events)
	if err != nil {
		panic(err)
	}
	return app
}

func (st *Store) CreateAppE(ownerID int, name, description string, perms map[string]string, events []string) (*App, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate GitHub App private key: %w", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	clientSecret, err := randomHex(20)
	if err != nil {
		return nil, fmt.Errorf("generate GitHub App client secret: %w", err)
	}
	webhookSecret, err := randomHex(20)
	if err != nil {
		return nil, fmt.Errorf("generate GitHub App webhook secret: %w", err)
	}

	id := st.NextAppID
	st.NextAppID++
	now := st.currentTime()
	slug := slugify(name)

	app := &App{
		ID:                 id,
		NodeID:             fmt.Sprintf("A_kgDO%08d", id),
		Slug:               slug,
		Name:               name,
		ClientID:           fmt.Sprintf("Iv1.%016x", id),
		ClientSecret:       clientSecret,
		Description:        description,
		ExternalURL:        fmt.Sprintf("https://github.com/apps/%s", slug),
		WebhookSecret:      webhookSecret,
		WebhookActive:      true,
		WebhookContentType: "form",
		WebhookInsecureSSL: "0",
		PEMPrivateKey:      string(privPEM),
		Permissions:        normalizeAppPermissions(perms),
		Events:             append([]string(nil), events...),
		OwnerID:            ownerID,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	st.Apps[id] = app
	st.AppsBySlug[slug] = app
	if st.AppsByClientID == nil {
		st.AppsByClientID = make(map[string]*App)
	}
	st.AppsByClientID[app.ClientID] = app
	if st.persist != nil {
		st.persist.MustPut("apps", strconv.Itoa(id), app)
	}
	return cloneApp(app), nil
}

// UpdateApp mutates a registered app under the write lock and persists it.
// Every field-level app edit goes through here so none of them can forget the
// lock or the persistence write.
func (st *Store) UpdateApp(appID int, fn func(a *App)) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	app := st.Apps[appID]
	if app == nil {
		return false
	}
	fn(app)
	app.UpdatedAt = time.Now().UTC()
	if st.persist != nil {
		st.persist.MustPut("apps", strconv.Itoa(appID), app)
	}
	return true
}

// UpdateAppHookConfig mutates the app's hook URL/secret/active flags.
func (st *Store) UpdateAppHookConfig(appID int, fn func(a *App)) bool {
	return st.UpdateApp(appID, fn)
}

// RotateAppClientSecret replaces an app's OAuth client secret and returns the
// new one-time value.
func (st *Store) RotateAppClientSecret(appID int) (string, error) {
	secret, err := randomHex(20)
	if err != nil {
		return "", fmt.Errorf("generate GitHub App client secret: %w", err)
	}
	if !st.UpdateApp(appID, func(a *App) { a.ClientSecret = secret }) {
		return "", fmt.Errorf("GitHub App not found")
	}
	return secret, nil
}

// RotateAppPrivateKey replaces the signing key and returns its one-time PEM.
func (st *Store) RotateAppPrivateKey(appID int) (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", fmt.Errorf("generate GitHub App private key: %w", err)
	}
	privateKey := string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	if !st.UpdateApp(appID, func(a *App) { a.PEMPrivateKey = privateKey }) {
		return "", fmt.Errorf("GitHub App not found")
	}
	return privateKey, nil
}

// DeleteApp removes an app and every credential or installation derived from
// it. Marketplace deletion is handled by the settings layer because that
// store has a separate lock and may refuse deletion while purchases exist.
func (st *Store) DeleteApp(appID int) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	app := st.Apps[appID]
	if app == nil {
		return false
	}
	delete(st.Apps, appID)
	delete(st.AppsBySlug, app.Slug)
	delete(st.AppsByClientID, app.ClientID)
	if st.persist != nil {
		st.persist.MustDelete("apps", strconv.Itoa(appID))
	}
	for code, id := range st.ManifestCodes {
		if id == appID {
			delete(st.ManifestCodes, code)
		}
	}
	for code, authorization := range st.AuthCodes {
		if authorization.ClientID == app.ClientID {
			delete(st.AuthCodes, code)
		}
	}
	for code, device := range st.DeviceCodes {
		if device.AppID == appID || device.ClientID == app.ClientID {
			delete(st.DeviceCodes, code)
		}
	}
	for id, inst := range st.Installations {
		if inst.AppID != appID {
			continue
		}
		delete(st.Installations, id)
		if st.persist != nil {
			st.persist.MustDelete("installations", strconv.Itoa(id))
		}
	}
	for token, installationToken := range st.InstallationTokens {
		if installationToken.AppID != appID {
			continue
		}
		delete(st.InstallationTokens, token)
		if st.persist != nil {
			st.persist.MustDelete("installation_tokens", token)
		}
	}
	for token, userToken := range st.UserToServerTokens {
		if userToken.AppID != appID {
			continue
		}
		delete(st.UserToServerTokens, token)
		if st.persist != nil {
			st.persist.MustDelete("user_to_server_tokens", token)
		}
		if userToken.RefreshTokenValue != "" {
			delete(st.RefreshTokens, userToken.RefreshTokenValue)
			if st.persist != nil {
				st.persist.MustDelete("refresh_tokens", userToken.RefreshTokenValue)
			}
		}
	}
	for token, refresh := range st.RefreshTokens {
		if refresh.AppID != appID {
			continue
		}
		delete(st.RefreshTokens, token)
		if st.persist != nil {
			st.persist.MustDelete("refresh_tokens", token)
		}
	}
	delete(st.AppHookDeliveries, appID)
	if st.persist != nil {
		st.persist.MustDelete("app_hook_deliveries", strconv.Itoa(appID))
	}
	return true
}

// GetApp returns an app by ID, or nil.
func (st *Store) GetApp(id int) *App {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return cloneApp(st.Apps[id])
}

// GetAppBySlug returns an app by slug, or nil.
func (st *Store) GetAppBySlug(slug string) *App {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return cloneApp(st.AppsBySlug[slug])
}

// CreateInstallation creates a new installation for an app.
func (st *Store) CreateInstallation(appID int, targetType string, targetID int, targetLogin string, perms map[string]string, events []string) *Installation {
	st.mu.Lock()
	defer st.mu.Unlock()

	app := st.Apps[appID]
	if app == nil {
		return nil
	}

	id := st.NextInstallationID
	st.NextInstallationID++
	now := st.currentTime()

	// Snapshot the target account's node ID and avatar so the
	// installation's `account` object can be served without a live
	// lookup (both are immutable in bleephub).
	var targetNodeID, targetAvatarURL string
	if u := st.UsersByLogin[targetLogin]; u != nil {
		targetNodeID, targetAvatarURL = u.NodeID, u.AvatarURL
	} else if o := st.OrgsByLogin[targetLogin]; o != nil {
		targetNodeID, targetAvatarURL = o.NodeID, o.AvatarURL
	}

	inst := &Installation{
		ID:                  id,
		AppID:               appID,
		AppSlug:             app.Slug,
		TargetType:          targetType,
		TargetID:            targetID,
		TargetLogin:         targetLogin,
		TargetNodeID:        targetNodeID,
		TargetAvatarURL:     targetAvatarURL,
		Permissions:         normalizeAppPermissions(perms),
		Events:              append([]string(nil), events...),
		RepositorySelection: "all",
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	st.Installations[id] = inst
	if st.persist != nil {
		st.persist.MustPut("installations", strconv.Itoa(id), inst)
	}
	return cloneInstallation(inst)
}

// GetInstallation returns an installation by ID, or nil.
func (st *Store) GetInstallation(id int) *Installation {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return cloneInstallation(st.Installations[id])
}

// ListAppInstallations returns all installations for a given app.
func (st *Store) ListAppInstallations(appID int) []*Installation {
	st.mu.RLock()
	defer st.mu.RUnlock()

	var result []*Installation
	for _, inst := range st.Installations {
		if inst.AppID == appID {
			result = append(result, cloneInstallation(inst))
		}
	}
	return result
}

// CountAppInstallations returns the number of installations for a given app.
func (st *Store) CountAppInstallations(appID int) int {
	st.mu.RLock()
	defer st.mu.RUnlock()
	n := 0
	for _, inst := range st.Installations {
		if inst.AppID == appID {
			n++
		}
	}
	return n
}

// GetRepoInstallation finds an installation by target login.
func (st *Store) GetRepoInstallation(ownerLogin string) *Installation {
	st.mu.RLock()
	defer st.mu.RUnlock()

	for _, inst := range st.Installations {
		if inst.TargetLogin == ownerLogin {
			return cloneInstallation(inst)
		}
	}
	return nil
}

// DeleteInstallation removes an installation by ID.
func (st *Store) DeleteInstallation(id int) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	if _, ok := st.Installations[id]; !ok {
		return false
	}
	delete(st.Installations, id)
	if st.persist != nil {
		st.persist.MustDelete("installations", strconv.Itoa(id))
	}
	return true
}

// persistInstallation writes-through to disk. Caller must hold st.mu.
func (st *Store) persistInstallation(inst *Installation) {
	if st.persist == nil || inst == nil {
		return
	}
	st.persist.MustPut("installations", strconv.Itoa(inst.ID), inst)
}

// SuspendInstallation marks the installation suspended. Returns false if not found
// or already suspended.
func (st *Store) SuspendInstallation(id int, by *User) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	inst := st.Installations[id]
	if inst == nil {
		return false
	}
	if inst.SuspendedAt != nil {
		return false
	}
	now := time.Now().UTC()
	inst.SuspendedAt = &now
	inst.SuspendedBy = by
	inst.UpdatedAt = now
	st.persistInstallation(inst)
	return true
}

// UnsuspendInstallation clears the suspension. Returns false if not found
// or wasn't suspended.
func (st *Store) UnsuspendInstallation(id int) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	inst := st.Installations[id]
	if inst == nil {
		return false
	}
	if inst.SuspendedAt == nil {
		return false
	}
	inst.SuspendedAt = nil
	inst.SuspendedBy = nil
	inst.UpdatedAt = st.currentTime()
	st.persistInstallation(inst)
	return true
}

// SetInstallationRepositorySelection switches between "all" and "selected" modes.
func (st *Store) SetInstallationRepositorySelection(id int, mode string, repoIDs []int) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	inst := st.Installations[id]
	if inst == nil {
		return false
	}
	inst.RepositorySelection = mode
	if mode == "selected" {
		inst.SelectedRepoIDs = append([]int(nil), repoIDs...)
	} else {
		inst.SelectedRepoIDs = nil
	}
	inst.UpdatedAt = st.currentTime()
	st.persistInstallation(inst)
	return true
}

// AddInstallationRepo adds a repo to a "selected" installation's allow-list.
// Returns (added, ok) — ok=false if installation not found; added=false if
// repo was already in the list (idempotent).
func installationOwnsRepo(inst *Installation, repo *Repo) bool {
	if inst == nil || repo == nil || !strings.EqualFold(inst.TargetType, repo.OwnerType) {
		return false
	}
	owner, _, ok := strings.Cut(repo.FullName, "/")
	return ok && strings.EqualFold(owner, inst.TargetLogin)
}

func (st *Store) AddInstallationRepo(id, repoID int) (bool, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	inst := st.Installations[id]
	repo := st.Repos[repoID]
	if inst == nil || inst.RepositorySelection != "selected" || !installationOwnsRepo(inst, repo) {
		return false, false
	}
	for _, r := range inst.SelectedRepoIDs {
		if r == repoID {
			return false, true
		}
	}
	inst.SelectedRepoIDs = append(inst.SelectedRepoIDs, repoID)
	inst.UpdatedAt = st.currentTime()
	st.persistInstallation(inst)
	return true, true
}

// RemoveInstallationRepo removes a repo from a "selected" installation's allow-list.
// Returns (removed, ok).
func (st *Store) RemoveInstallationRepo(id, repoID int) (bool, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	inst := st.Installations[id]
	repo := st.Repos[repoID]
	if inst == nil || inst.RepositorySelection != "selected" || !installationOwnsRepo(inst, repo) {
		return false, false
	}
	for i, r := range inst.SelectedRepoIDs {
		if r == repoID {
			inst.SelectedRepoIDs = append(inst.SelectedRepoIDs[:i], inst.SelectedRepoIDs[i+1:]...)
			inst.UpdatedAt = st.currentTime()
			st.persistInstallation(inst)
			return true, true
		}
	}
	return false, true
}

// GetAppByClientID returns the GitHub App with the given client_id, or nil.
func (st *Store) GetAppByClientID(clientID string) *App {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return cloneApp(st.AppsByClientID[clientID])
}

// CreateOAuthApp registers a new (classic) OAuth App. Distinct from a GitHub App:
// no JWT, no installations, no permissions table — just client_id/secret + callback URL.
// Both kinds of apps support the OAuth web flow, but the resulting access tokens
// have different prefixes (gho_ for OAuth Apps, ghu_ for GitHub App user-to-server).
func (st *Store) CreateOAuthApp(ownerID int, name, description, url, callbackURL string) *OAuthApp {
	app, err := st.CreateOAuthAppE(ownerID, name, description, url, callbackURL)
	if err != nil {
		panic(err)
	}
	return app
}

func (st *Store) CreateOAuthAppE(ownerID int, name, description, appURL, callbackURL string) (*OAuthApp, error) {
	if err := validateClientCallbackURL(callbackURL); err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.OAuthApps == nil {
		st.OAuthApps = make(map[string]*OAuthApp)
	}
	clientID, err := randomHex(10)
	if err != nil {
		return nil, fmt.Errorf("generate OAuth App client id: %w", err)
	}
	clientSecret, err := randomHex(20)
	if err != nil {
		return nil, fmt.Errorf("generate OAuth App client secret: %w", err)
	}
	now := time.Now().UTC()
	app := &OAuthApp{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Name:         name,
		Description:  description,
		URL:          appURL,
		CallbackURL:  callbackURL,
		OwnerID:      ownerID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	st.OAuthApps[clientID] = app
	if st.persist != nil {
		st.persist.MustPut("oauth_apps", clientID, app)
	}
	return cloneOAuthApp(app), nil
}

// GetOAuthApp returns the OAuth App with the given client_id, or nil.
func (st *Store) GetOAuthApp(clientID string) *OAuthApp {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return cloneOAuthApp(st.OAuthApps[clientID])
}

// ListOAuthApps returns all OAuth Apps.
func (st *Store) ListOAuthApps() []*OAuthApp {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]*OAuthApp, 0, len(st.OAuthApps))
	for _, a := range st.OAuthApps {
		out = append(out, cloneOAuthApp(a))
	}
	return out
}

func (st *Store) UpdateOAuthApp(clientID string, fn func(a *OAuthApp)) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	app := st.OAuthApps[clientID]
	if app == nil {
		return false
	}
	fn(app)
	app.UpdatedAt = time.Now().UTC()
	if st.persist != nil {
		st.persist.MustPut("oauth_apps", clientID, app)
	}
	return true
}

func (st *Store) RotateOAuthAppClientSecret(clientID string) (string, error) {
	secret, err := randomHex(20)
	if err != nil {
		return "", fmt.Errorf("generate OAuth App client secret: %w", err)
	}
	if !st.UpdateOAuthApp(clientID, func(a *OAuthApp) { a.ClientSecret = secret }) {
		return "", fmt.Errorf("OAuth App not found")
	}
	return secret, nil
}

func (st *Store) DeleteOAuthApp(clientID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.OAuthApps[clientID] == nil {
		return false
	}
	delete(st.OAuthApps, clientID)
	if st.persist != nil {
		st.persist.MustDelete("oauth_apps", clientID)
	}
	for code, authorization := range st.AuthCodes {
		if authorization.ClientID == clientID {
			delete(st.AuthCodes, code)
		}
	}
	for code, device := range st.DeviceCodes {
		if device.OAuthClientID == clientID || device.ClientID == clientID {
			delete(st.DeviceCodes, code)
		}
	}
	for token, userToken := range st.UserToServerTokens {
		if userToken.OAuthAppClientID != clientID {
			continue
		}
		delete(st.UserToServerTokens, token)
		if st.persist != nil {
			st.persist.MustDelete("user_to_server_tokens", token)
		}
		if userToken.RefreshTokenValue != "" {
			delete(st.RefreshTokens, userToken.RefreshTokenValue)
			if st.persist != nil {
				st.persist.MustDelete("refresh_tokens", userToken.RefreshTokenValue)
			}
		}
	}
	for token, refresh := range st.RefreshTokens {
		if refresh.OAuthAppClientID != clientID {
			continue
		}
		delete(st.RefreshTokens, token)
		if st.persist != nil {
			st.persist.MustDelete("refresh_tokens", token)
		}
	}
	return true
}

// VerifyOAuthAppSecret returns the OAuth App if client_id+client_secret match, else nil.
func (st *Store) VerifyOAuthAppSecret(clientID, clientSecret string) *OAuthApp {
	st.mu.RLock()
	defer st.mu.RUnlock()
	app := st.OAuthApps[clientID]
	if app == nil || !secretEqual(app.ClientSecret, clientSecret) {
		return nil
	}
	return cloneOAuthApp(app)
}

// VerifyAppClientSecret returns the GitHub App if client_id+client_secret match, else nil.
func (st *Store) VerifyAppClientSecret(clientID, clientSecret string) *App {
	st.mu.RLock()
	defer st.mu.RUnlock()
	app := st.AppsByClientID[clientID]
	if app == nil || !secretEqual(app.ClientSecret, clientSecret) {
		return nil
	}
	return cloneApp(app)
}

// CreateInstallationToken generates a ghs_-prefixed token with 1h expiry.
// If repoIDs is non-empty, the token is scoped to those repositories
// (a subset of the installation's accessible repos).
func (st *Store) CreateInstallationToken(installationID, appID int, perms map[string]string, repoIDs []int) *InstallationToken {
	token, err := st.CreateInstallationTokenE(installationID, appID, perms, repoIDs)
	if err != nil {
		panic(err)
	}
	return token
}

func (st *Store) CreateInstallationTokenE(installationID, appID int, perms map[string]string, repoIDs []int) (*InstallationToken, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	h, err := randomHex(20)
	if err != nil {
		return nil, fmt.Errorf("generate installation token: %w", err)
	}
	tokenStr := tokenPrefixInstallation + h

	token := &InstallationToken{
		Token:          tokenStr,
		ExpiresAt:      st.currentTime().Add(1 * time.Hour),
		Permissions:    normalizeAppPermissions(perms),
		RepositoryIDs:  append([]int(nil), repoIDs...),
		InstallationID: installationID,
		AppID:          appID,
	}
	st.InstallationTokens[tokenStr] = token
	if st.persist != nil {
		st.persist.MustPut("installation_tokens", tokenStr, token)
	}
	return cloneInstallationToken(token), nil
}

// RevokeInstallationToken drops the token from the store. Returns
// true if the token existed (so the caller can return 204) and false
// if it didn't (so the caller can return 401 for unknown tokens).
func (st *Store) RevokeInstallationToken(tokenStr string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.InstallationTokens[tokenStr]; !ok {
		return false
	}
	delete(st.InstallationTokens, tokenStr)
	if st.persist != nil {
		st.persist.MustDelete("installation_tokens", tokenStr)
	}
	return true
}

// LookupInstallationToken returns the token and its installation, or nil if not found/expired.
func (st *Store) LookupInstallationToken(tokenStr string) (*InstallationToken, *Installation) {
	st.mu.RLock()
	defer st.mu.RUnlock()

	tok, ok := st.InstallationTokens[tokenStr]
	if !ok {
		return nil, nil
	}
	if st.currentTime().After(tok.ExpiresAt) {
		return nil, nil
	}
	inst := st.Installations[tok.InstallationID]
	return cloneInstallationToken(tok), cloneInstallation(inst)
}

// RegisterManifestCode creates a one-time-use code that maps to an app ID.
func (st *Store) RegisterManifestCode(appID int) string {
	st.mu.Lock()
	defer st.mu.Unlock()

	code := uuid.New().String()
	st.ManifestCodes[code] = appID
	return code
}

// ConsumeManifestCode redeems a manifest code, returning the app ID. One-time use.
func (st *Store) ConsumeManifestCode(code string) (int, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()

	appID, ok := st.ManifestCodes[code]
	if !ok {
		return 0, false
	}
	delete(st.ManifestCodes, code)
	return appID, true
}

// slugify is defined in store_orgs.go

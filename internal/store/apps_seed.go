package store

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strconv"
	"time"
)

// SeedApp creates a GitHub App from a caller-supplied spec with a deterministic
// id, slug, and private key. Idempotent: if an app with the same id or slug is
// already present (e.g. loaded from persistence), it is returned unchanged with
// created=false. Errors only if the supplied PEM is not a usable RSA key.
func (st *Store) SeedApp(spec AppSeedSpec, pemKey, ownerLogin string) (app *App, created bool, err error) {
	normKey, err := normalizeRSAPrivateKeyPEM(pemKey)
	if err != nil {
		return nil, false, err
	}

	st.Mu.Lock()
	defer st.Mu.Unlock()

	slug := spec.Slug
	if slug == "" {
		slug = Slugify(spec.Name)
	}
	owner := st.UsersByLogin[ownerLogin]
	if owner == nil {
		return nil, false, fmt.Errorf("owner %q is not an existing user", ownerLogin)
	}
	if existing := st.Apps[spec.ID]; existing != nil {
		return existing, false, nil
	}
	if existing := st.AppsBySlug[slug]; existing != nil {
		return existing, false, nil
	}

	clientID := spec.ClientID
	if clientID == "" {
		clientID = fmt.Sprintf("Iv1.%016x", spec.ID)
	}

	clientSecret, err := RandomHex(20)
	if err != nil {
		return nil, false, fmt.Errorf("generate seeded GitHub App client secret: %w", err)
	}
	webhookSecret := spec.WebhookSecret
	if webhookSecret == "" {
		webhookSecret, err = RandomHex(20)
		if err != nil {
			return nil, false, fmt.Errorf("generate seeded GitHub App webhook secret: %w", err)
		}
	}

	now := time.Now().UTC()
	app = &App{
		ID:                 spec.ID,
		NodeID:             fmt.Sprintf("A_kgDO%08d", spec.ID),
		Slug:               slug,
		Name:               spec.Name,
		ClientID:           clientID,
		ClientSecret:       clientSecret,
		ExternalURL:        fmt.Sprintf("https://github.com/apps/%s", slug),
		WebhookURL:         spec.WebhookURL,
		WebhookSecret:      webhookSecret,
		WebhookActive:      spec.WebhookURL != "",
		WebhookContentType: "form",
		WebhookInsecureSSL: "0",
		PEMPrivateKey:      normKey,
		Permissions:        NormalizeAppPermissions(spec.Permissions),
		Events:             spec.Events,
		OwnerID:            owner.ID,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	st.Apps[app.ID] = app
	st.AppsBySlug[slug] = app
	if st.AppsByClientID == nil {
		st.AppsByClientID = make(map[string]*App)
	}
	st.AppsByClientID[clientID] = app
	if spec.ID >= st.NextAppID {
		st.NextAppID = spec.ID + 1
	}
	if st.Persist != nil {
		st.Persist.MustPut("apps", strconv.Itoa(app.ID), app)
	}
	return app, true, nil
}

// SeedInstallation creates an installation of a seeded App on a target,
// optionally with a deterministic id. Idempotent per (app, target login): an
// existing installation on the same account is returned unchanged. Returns nil
// if the app doesn't exist.
func (st *Store) SeedInstallation(appID, explicitID int, targetType string, targetID int, targetLogin string, perms map[string]string, events []string) *Installation {
	st.Mu.Lock()
	defer st.Mu.Unlock()

	app := st.Apps[appID]
	if app == nil {
		return nil
	}
	if targetID <= 0 {
		return nil
	}
	if targetType == "" {
		targetType = "Organization"
	}
	for _, inst := range st.Installations {
		if inst.AppID == appID && inst.TargetLogin == targetLogin {
			return inst
		}
	}

	id := explicitID
	if id <= 0 {
		id = st.NextInstallationID
		st.NextInstallationID++
	} else if id >= st.NextInstallationID {
		st.NextInstallationID = id + 1
	}

	var targetNodeID, targetAvatarURL string
	if u := st.UsersByLogin[targetLogin]; u != nil {
		targetNodeID, targetAvatarURL = u.NodeID, u.AvatarURL
	} else if o := st.OrgsByLogin[targetLogin]; o != nil {
		targetNodeID, targetAvatarURL = o.NodeID, o.AvatarURL
	}

	now := time.Now().UTC()
	inst := &Installation{
		ID:                  id,
		AppID:               appID,
		AppSlug:             app.Slug,
		TargetType:          targetType,
		TargetID:            targetID,
		TargetLogin:         targetLogin,
		TargetNodeID:        targetNodeID,
		TargetAvatarURL:     targetAvatarURL,
		Permissions:         NormalizeAppPermissions(perms),
		Events:              events,
		RepositorySelection: "all",
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	st.Installations[id] = inst
	if st.Persist != nil {
		st.Persist.MustPut("installations", strconv.Itoa(id), inst)
	}
	return inst
}

// AppSeedSpec describes one pre-registered GitHub App to create at startup,
// so a coordinate-only consumer holds the same (app id + private key + org)
// coordinates against bleephub that it would against real GitHub — where an
// App is registered out of band and the consumer keeps its id + PEM as
// secrets. Supplied via BLEEPHUB_SEED_APPS (inline JSON array) or
// BLEEPHUB_SEED_APPS_FILE (path to a JSON array).
type AppSeedSpec struct {
	ID             int                    `json:"id"`                   // deterministic, caller-chosen app id (required)
	Slug           string                 `json:"slug"`                 // defaults to Slugify(name)
	Name           string                 `json:"name"`                 // required
	ClientID       string                 `json:"client_id"`            // defaults to Iv1.<id>
	PrivateKeyPEM  string                 `json:"private_key_pem"`      // caller-supplied RSA key (PKCS1 or PKCS8)
	PrivateKeyFile string                 `json:"private_key_pem_file"` // alternative to inline PEM
	Owner          string                 `json:"owner"`                // owning user login (required)
	Permissions    map[string]string      `json:"permissions"`
	Events         []string               `json:"events"`
	WebhookURL     string                 `json:"webhook_url"`
	WebhookSecret  string                 `json:"webhook_secret"`
	Installations  []InstallationSeedSpec `json:"installations"`
}

// normalizeRSAPrivateKeyPEM validates a caller-supplied RSA private key (PKCS1
// "RSA PRIVATE KEY" or PKCS8 "PRIVATE KEY") and re-encodes it as PKCS1 — the
// form the app-JWT verifier (parseAndVerifyAppJWT) expects. The consumer keeps
// the same key, so app JWTs they sign verify against the stored key.
func normalizeRSAPrivateKeyPEM(pemStr string) (string, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return "", fmt.Errorf("private key is not valid PEM")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else if k8, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k8.(*rsa.PrivateKey)
		if !ok {
			return "", fmt.Errorf("private key is not an RSA key")
		}
		key = rk
	} else {
		return "", fmt.Errorf("cannot parse RSA private key (need a PKCS1 or PKCS8 PEM)")
	}
	out := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return string(out), nil
}

// InstallationSeedSpec pre-creates an installation of the seeded App on a
// given account, so the consumer can mint an installation token by
// coordinates alone (no /internal use).
type InstallationSeedSpec struct {
	ID          int               `json:"id"`          // deterministic installation id (optional)
	Account     string            `json:"account"`     // org/user login to install on (required)
	TargetType  string            `json:"target_type"` // "Organization" | "User"; default Organization
	Permissions map[string]string `json:"permissions"`
	Events      []string          `json:"events"`
}

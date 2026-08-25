package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	github "github.com/google/go-github/v88/github"
)

// runApps exercises GitHub App authentication for real, rather than recording
// a skip. An App can be created through the manifest flow every integrator
// uses, installed through the browser flow, and then authenticated as with a
// JSON Web Token signed by the key the manifest conversion handed back — so
// the whole installation-token lifecycle is reachable from a client with no
// credential that a person had to paste in.
func runApps(client *github.Client, rec *recorder, set *fixtureSet) {
	const domain = "apps"

	appName := "Conformance Harness App"
	created, err := createAppViaManifest(appName)
	if err != nil {
		skipAll(rec, domain, "POST /app-manifests/{code}/conversions",
			"a GitHub App could not be provisioned through the manifest flow: "+truncate(err.Error()),
			"apps.getBySlug", "apps.listInstallations", "apps.getInstallation",
			"apps.createInstallationToken", "apps.listReposAccessibleToInstallation",
			"apps.getRepositoryInstallation", "apps.suspendInstallation", "apps.deleteInstallation")
		return
	}

	rec.check(domain, "apps.completeAppManifest", "POST /app-manifests/{code}/conversions", func() error {
		if created.Slug == "" {
			return deviate("a slug", "empty", "the manifest conversion returned no slug")
		}
		if created.PEM == "" {
			return deviate("a private key", "empty",
				"the manifest conversion returned no private key, so no client could ever authenticate as the App")
		}
		if !strings.Contains(created.PEM, "PRIVATE KEY") {
			return deviate("a Privacy Enhanced Mail private key", truncate(created.PEM),
				"the returned key is not a Privacy Enhanced Mail block")
		}
		return nil
	})

	rec.check(domain, "apps.getBySlug", "GET /apps/{app_slug}", func() error {
		app, _, err := client.Apps.Get(ctx, created.Slug)
		if err != nil {
			return err
		}
		if app.GetID() != created.ID {
			return deviate(fmt.Sprintf("%d", created.ID), fmt.Sprintf("%d", app.GetID()), "the wrong App came back")
		}
		if app.GetName() != appName {
			return deviate(appName, app.GetName(), "the App name is wrong")
		}
		if app.GetOwner().GetLogin() == "" {
			return deviate("owner.login populated", "empty", "the App has no owner")
		}
		if len(app.GetPermissions().GetContents()) == 0 {
			return deviate("the contents permission from the manifest", "absent",
				"the App's default permissions were not stored")
		}
		return nil
	})

	appJWT, err := signAppJWT(created.PEM, created.ID)
	if err != nil {
		skipAll(rec, domain, "GET /app/installations",
			"the App JSON Web Token could not be signed: "+truncate(err.Error()),
			"apps.listInstallations", "apps.getInstallation", "apps.createInstallationToken",
			"apps.listReposAccessibleToInstallation", "apps.getRepositoryInstallation",
			"apps.suspendInstallation", "apps.deleteInstallation")
		return
	}
	appClient, err := otherClient(appJWT)
	if err != nil {
		skipAll(rec, domain, "GET /app/installations", "the App client could not be constructed",
			"apps.listInstallations", "apps.getInstallation", "apps.createInstallationToken",
			"apps.listReposAccessibleToInstallation", "apps.getRepositoryInstallation",
			"apps.suspendInstallation", "apps.deleteInstallation")
		return
	}

	rec.check(domain, "apps.getAuthenticated", "GET /app", func() error {
		app, _, err := decodeApp(appClient)
		if err != nil {
			return err
		}
		if app.GetID() != created.ID {
			return deviate(fmt.Sprintf("%d", created.ID), fmt.Sprintf("%d", app.GetID()),
				"the JSON Web Token authenticated as a different App")
		}
		return nil
	})

	installationID, err := installApp(created.Slug, set.owner)
	if err != nil || installationID == 0 {
		reason := "the App could not be installed"
		if err != nil {
			reason += ": " + truncate(err.Error())
		}
		skipAll(rec, domain, "POST /apps/{slug}/installations/new", reason,
			"apps.listInstallations", "apps.getInstallation", "apps.createInstallationToken",
			"apps.listReposAccessibleToInstallation", "apps.getRepositoryInstallation",
			"apps.suspendInstallation", "apps.deleteInstallation")
		return
	}

	rec.check(domain, "apps.listInstallations", "GET /app/installations", func() error {
		installations, _, err := appClient.Apps.ListInstallations(ctx, nil)
		if err != nil {
			return err
		}
		for _, installation := range installations {
			if installation.GetID() == installationID {
				if installation.GetAppID() != created.ID {
					return deviate(fmt.Sprintf("%d", created.ID), fmt.Sprintf("%d", installation.GetAppID()),
						"the installation names the wrong App")
				}
				if installation.GetAccount().GetLogin() == "" {
					return deviate("account.login populated", "empty", "the installation has no account")
				}
				if installation.GetAccessTokensURL() == "" {
					return deviate("access_tokens_url populated", "empty",
						"the installation omits the link a client posts to for a token")
				}
				return nil
			}
		}
		return deviate("the installation just created", "absent",
			"an App authenticated with its own JSON Web Token cannot see its installation")
	})

	rec.check(domain, "apps.getInstallation", "GET /app/installations/{installation_id}", func() error {
		installation, _, err := appClient.Apps.GetInstallation(ctx, installationID)
		if err != nil {
			return err
		}
		if installation.GetID() != installationID {
			return deviate(fmt.Sprintf("%d", installationID), fmt.Sprintf("%d", installation.GetID()),
				"the wrong installation came back")
		}
		if installation.GetRepositorySelection() == "" {
			return deviate("repository_selection populated", "empty",
				"the installation does not say whether it covers all repositories or a selection")
		}
		return nil
	})

	rec.check(domain, "apps.getUserInstallation", "GET /users/{username}/installation", func() error {
		installation, _, err := appClient.Apps.GetUserInstallation(ctx, set.owner)
		if err != nil {
			return err
		}
		if installation.GetID() != installationID {
			return deviate(fmt.Sprintf("%d", installationID), fmt.Sprintf("%d", installation.GetID()),
				"looking the installation up by account returns the wrong installation")
		}
		return nil
	})

	var installationToken string
	rec.check(domain, "apps.createInstallationToken", "POST /app/installations/{installation_id}/access_tokens", func() error {
		token, resp, err := appClient.Apps.CreateInstallationToken(ctx, installationID,
			&github.InstallationTokenOptions{
				Permissions: &github.InstallationPermissions{Contents: github.Ptr("read")},
			})
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusCreated, "minting an installation token"); err != nil {
			return err
		}
		installationToken = token.GetToken()
		if installationToken == "" {
			return deviate("a token", "empty", "no installation token was minted")
		}
		if token.GetExpiresAt().IsZero() {
			return deviate("expires_at populated", "zero",
				"the installation token has no expiry, so a client cannot know when to refresh it")
		}
		if ttl := time.Until(token.GetExpiresAt().Time); ttl < 50*time.Minute || ttl > 70*time.Minute {
			return deviate("a one-hour lifetime", ttl.String(),
				"the installation token's lifetime is not the documented hour")
		}
		if token.GetPermissions().GetContents() != "read" {
			return deviate("contents: read", token.GetPermissions().GetContents(),
				"the minted token does not report the permissions it was downscoped to")
		}
		return nil
	})

	rec.check(domain, "apps.createInstallationToken (escalation refused)",
		"POST /app/installations/{installation_id}/access_tokens beyond the grant", func() error {
			_, _, err := appClient.Apps.CreateInstallationToken(ctx, installationID,
				&github.InstallationTokenOptions{
					Permissions: &github.InstallationPermissions{Administration: github.Ptr("write")},
				})
			return wantHTTPError(err, http.StatusUnprocessableEntity,
				"minting a token with a permission the installation was never granted")
		})

	if installationToken != "" {
		rec.check(domain, "apps.listReposAccessibleToInstallation", "GET /installation/repositories", func() error {
			tokenClient, err := otherClient(installationToken)
			if err != nil {
				return err
			}
			repositories, _, err := tokenClient.Apps.ListRepos(ctx, nil)
			if err != nil {
				return err
			}
			if repositories.GetTotalCount() < 1 {
				return deviate("at least one accessible repository",
					fmt.Sprintf("total_count %d", repositories.GetTotalCount()),
					"an installation token sees no repositories, so the App can do nothing")
			}
			return nil
		})

		rec.check(domain, "apps.installationTokenIsScoped", "GET /user with an installation token", func() error {
			// An installation token is not a user credential: GitHub refuses
			// /user for it. A client that cannot tell the two apart would
			// mis-render the actor on everything the App does.
			tokenClient, err := otherClient(installationToken)
			if err != nil {
				return err
			}
			user, _, err := tokenClient.Users.Get(ctx, "")
			if err == nil {
				return deviate("403 Resource not accessible by integration",
					"200 with login "+user.GetLogin(),
					"an installation token resolves to a user account, so every client would attribute the App's writes to that person")
			}
			return wantHTTPError(err, http.StatusForbidden, "reading /user with an installation token")
		})
	} else {
		skipAll(rec, domain, "GET /installation/repositories", "no installation token was minted",
			"apps.listReposAccessibleToInstallation", "apps.installationTokenIsScoped")
	}

	rec.check(domain, "apps.getRepositoryInstallation", "GET /repos/{owner}/{repo}/installation", func() error {
		if set.repo == "" {
			return deviate("a repository fixture", "none", "no repository fixture exists")
		}
		installation, _, err := appClient.Apps.GetRepositoryInstallation(ctx, set.owner, set.repo)
		if err != nil {
			return err
		}
		if installation.GetID() != installationID {
			return deviate(fmt.Sprintf("%d", installationID), fmt.Sprintf("%d", installation.GetID()),
				"the per-repository installation lookup returns the wrong installation")
		}
		return nil
	})

	rec.check(domain, "apps.listUserInstallations", "GET /user/installations", func() error {
		installations, _, err := client.Apps.ListUserInstallations(ctx, nil)
		if err != nil {
			return err
		}
		for _, installation := range installations {
			if installation.GetID() == installationID {
				return nil
			}
		}
		return deviate("the installation on this account", "absent",
			"the account that installed the App cannot see the installation")
	})

	rec.check(domain, "apps.suspendInstallation / unsuspendInstallation",
		"PUT and DELETE /app/installations/{installation_id}/suspended", func() error {
			if _, err := appClient.Apps.SuspendInstallation(ctx, installationID); err != nil {
				return err
			}
			installation, _, err := appClient.Apps.GetInstallation(ctx, installationID)
			if err != nil {
				return err
			}
			if installation.SuspendedAt == nil {
				return deviate("suspended_at populated", "absent",
					"a suspended installation does not report suspended_at, so a client cannot tell it is suspended")
			}
			if _, err := appClient.Apps.UnsuspendInstallation(ctx, installationID); err != nil {
				return err
			}
			installation, _, err = appClient.Apps.GetInstallation(ctx, installationID)
			if err != nil {
				return err
			}
			if installation.SuspendedAt != nil {
				return deviate("suspended_at cleared", "still set", "unsuspending did not take effect")
			}
			return nil
		})

	rec.check(domain, "apps.deleteInstallation", "DELETE /app/installations/{installation_id}", func() error {
		resp, err := appClient.Apps.DeleteInstallation(ctx, installationID)
		if err != nil {
			return err
		}
		if err := wantStatus(resp, http.StatusNoContent, "deleting an installation"); err != nil {
			return err
		}
		_, _, err = appClient.Apps.GetInstallation(ctx, installationID)
		return wantHTTPError(err, http.StatusNotFound, "reading a deleted installation")
	})
}

// appConfig is the subset of the manifest conversion response the driver uses.
type appConfig struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	PEM      string `json:"pem"`
	ClientID string `json:"client_id"`
}

// createAppViaManifest drives the two-step GitHub App manifest flow: post the
// manifest to the web form, follow the redirect for its one-time code, then
// convert the code into an App. This is the flow every App-creation wizard
// uses, and it is the only way to obtain an App's private key from a client.
func createAppViaManifest(name string) (*appConfig, error) {
	manifest, err := json.Marshal(map[string]any{
		"name":                name,
		"url":                 "https://example.invalid/app",
		"redirect_url":        "https://example.invalid/callback",
		"default_permissions": map[string]string{"contents": "read", "issues": "write", "checks": "write"},
	})
	if err != nil {
		return nil, err
	}
	form := url.Values{"manifest": {string(manifest)}}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/settings/apps/new", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := noRedirect.Do(request)
	if err != nil {
		return nil, err
	}
	response.Body.Close()
	if response.StatusCode != http.StatusFound {
		return nil, fmt.Errorf("manifest form answered %d, not 302", response.StatusCode)
	}
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		return nil, err
	}
	code := location.Query().Get("code")
	if code == "" {
		return nil, fmt.Errorf("the manifest redirect carried no conversion code")
	}
	status, body, err := raw(http.MethodPost, "/api/v3/app-manifests/"+code+"/conversions", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated {
		return nil, fmt.Errorf("manifest conversion answered %d: %s", status, truncate(string(body)))
	}
	config := &appConfig{}
	if err := json.Unmarshal(body, config); err != nil {
		return nil, err
	}
	return config, nil
}

// installApp installs the App on an account through the browser flow.
func installApp(slug, account string) (int64, error) {
	form := url.Values{"target_login": {account}, "repository_selection": {"all"}}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/apps/"+slug+"/installations/new",
		strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	var installation struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&installation); err != nil {
		return 0, err
	}
	return installation.ID, nil
}

// signAppJWT mints the RS256 JSON Web Token a real GitHub App client presents.
func signAppJWT(privateKeyPEM string, appID int64) (string, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return "", fmt.Errorf("the App private key is not a Privacy Enhanced Mail block")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, pkcs8Err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if pkcs8Err != nil {
			return "", err
		}
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return "", fmt.Errorf("the App private key is not an RSA key")
		}
		key = rsaKey
	}
	encode := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	now := time.Now().Unix()
	header := encode([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := encode([]byte(fmt.Sprintf(`{"iss":"%d","iat":%d,"exp":%d}`, appID, now-60, now+540)))
	digest := sha256.Sum256([]byte(header + "." + payload))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return header + "." + payload + "." + encode(signature), nil
}

// decodeApp reads GET /app, which go-github does not expose a typed method for
// even though it is the first call every App client makes.
func decodeApp(client *github.Client) (*github.App, *github.Response, error) {
	app := &github.App{}
	resp, err := decodeInto(client, http.MethodGet, "app", nil, app)
	return app, resp, err
}

// runDeviceFlow drives the OAuth device authorization grant end to end. It is
// how `gh auth login` and every headless client obtains a user credential, and
// it was previously recorded as a skip because no App existed to run it
// against; the manifest flow above supplies one.
func runDeviceFlow(rec *recorder, set *fixtureSet) {
	const domain = "auth"

	created, err := createAppViaManifest("Conformance Device Flow App")
	if err != nil || created.ClientID == "" {
		reason := "no OAuth client could be provisioned to run the device flow against"
		if err != nil {
			reason += ": " + truncate(err.Error())
		} else {
			reason += ": the manifest conversion returned no client_id"
		}
		skipAll(rec, domain, "POST /login/device/code", reason,
			"oauth.deviceCode", "oauth.deviceApprove", "oauth.deviceAccessToken",
			"oauth.deviceTokenAuthenticates")
		return
	}

	var deviceCode, userCode, accessToken string
	rec.check(domain, "oauth.deviceCode", "POST /login/device/code", func() error {
		var body struct {
			DeviceCode      string `json:"device_code"`
			UserCode        string `json:"user_code"`
			VerificationURI string `json:"verification_uri"`
			ExpiresIn       int    `json:"expires_in"`
			Interval        int    `json:"interval"`
		}
		status, payload, err := form(http.MethodPost, "/login/device/code", url.Values{
			"client_id": {created.ClientID}, "scope": {"repo"},
		}, nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return deviate("200 OK", fmt.Sprintf("%d %s", status, truncate(string(payload))),
				"the device-code endpoint refused the request")
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return deviate("a JSON device-code response", truncate(string(payload)),
				"the device-code response did not decode: %v", err)
		}
		deviceCode, userCode = body.DeviceCode, body.UserCode
		if deviceCode == "" || userCode == "" {
			return deviate("device_code and user_code", truncate(string(payload)),
				"the device-code response omits the fields the client shows the user and polls with")
		}
		if body.VerificationURI == "" {
			return deviate("a verification_uri", "empty",
				"the response carries no verification_uri, so the client has no address to print")
		}
		if body.ExpiresIn <= 0 || body.Interval <= 0 {
			return deviate("positive expires_in and interval",
				fmt.Sprintf("expires_in=%d interval=%d", body.ExpiresIn, body.Interval),
				"the client cannot decide how often or how long to poll")
		}
		return nil
	})

	if deviceCode == "" {
		skipAll(rec, domain, "POST /login/device", "no device code was issued",
			"oauth.deviceAccessToken (authorization_pending)", "oauth.deviceApprove",
			"oauth.deviceAccessToken", "oauth.deviceTokenAuthenticates")
		return
	}

	rec.check(domain, "oauth.deviceAccessToken (authorization_pending)",
		"POST /login/oauth/access_token before approval", func() error {
			// Before the user approves, the documented answer is HTTP 200 with
			// {"error":"authorization_pending"} — not an HTTP error. A client
			// that saw a 4xx here would abort instead of continuing to poll.
			status, payload, err := form(http.MethodPost, "/login/oauth/access_token", url.Values{
				"client_id":   {created.ClientID},
				"device_code": {deviceCode},
				"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			}, nil)
			if err != nil {
				return err
			}
			if status != http.StatusOK {
				return deviate("200 OK carrying an error body", fmt.Sprintf("%d", status),
					"polling before approval answers with an HTTP error, which stops the client polling")
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(payload, &body); err != nil {
				return deviate("a JSON error body", truncate(string(payload)), "the poll response did not decode")
			}
			if body.Error != "authorization_pending" {
				return deviate("authorization_pending", body.Error,
					"polling before approval does not report authorization_pending")
			}
			return nil
		})

	rec.check(domain, "oauth.deviceApprove", "POST /login/device with a browser session", func() error {
		// Approval is a browser action, so the driver signs in the way the web
		// form does and then submits the user code with that session.
		jar, err := browserSession()
		if err != nil {
			return err
		}
		status, payload, err := form(http.MethodPost, "/login/device", url.Values{
			"user_code": {userCode},
		}, jar)
		if err != nil {
			return err
		}
		if status != http.StatusOK && status != http.StatusFound {
			return deviate("200 or 302 after approving the device code",
				fmt.Sprintf("%d %s", status, truncate(string(payload))),
				"the approval form refused a signed-in submission")
		}
		return nil
	})

	rec.check(domain, "oauth.deviceAccessToken", "POST /login/oauth/access_token after approval", func() error {
		status, payload, err := form(http.MethodPost, "/login/oauth/access_token", url.Values{
			"client_id":   {created.ClientID},
			"device_code": {deviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}, nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return deviate("200 OK", fmt.Sprintf("%d", status), "the token exchange failed")
		}
		var body struct {
			AccessToken string `json:"access_token"`
			TokenType   string `json:"token_type"`
			Scope       string `json:"scope"`
			Error       string `json:"error"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return deviate("a JSON token response", truncate(string(payload)),
				"the token response did not decode")
		}
		if body.Error != "" {
			return deviate("an access token", body.Error, "the approved device code did not yield a token")
		}
		accessToken = body.AccessToken
		if accessToken == "" {
			return deviate("a non-empty access_token", "empty", "the token exchange returned no token")
		}
		if !strings.EqualFold(body.TokenType, "bearer") {
			return deviate("bearer", body.TokenType, "the token response declares an unusable token_type")
		}
		return nil
	})

	if accessToken == "" {
		rec.skip1(domain, "oauth.deviceTokenAuthenticates", "GET /user with the device-flow token",
			"no access token was issued")
		return
	}

	rec.check(domain, "oauth.deviceTokenAuthenticates", "GET /user with the device-flow token", func() error {
		deviceClient, err := otherClient(accessToken)
		if err != nil {
			return err
		}
		user, _, err := deviceClient.Users.Get(ctx, "")
		if err != nil {
			return err
		}
		if user.GetLogin() != set.owner {
			return deviate(set.owner, user.GetLogin(),
				"the device-flow token authenticates as the wrong account")
		}
		return nil
	})
}

// form posts an application/x-www-form-urlencoded body, optionally carrying a
// browser session, and returns the status and body.
func form(method, path string, values url.Values, jar http.CookieJar) (int, []byte, error) {
	request, err := http.NewRequest(method, baseURL+path, strings.NewReader(values.Encode()))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// GitHub's OAuth token endpoints answer form-encoded by default; every
	// client asks for JSON explicitly, so the driver does too.
	request.Header.Set("Accept", "application/json")
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	return response.StatusCode, payload, err
}

// browserSession signs in through the web login form and returns the cookie jar
// holding the resulting session, which is what a person approving a device code
// in their browser would have.
func browserSession() (http.CookieJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	status, payload, err := form(http.MethodPost, "/login", url.Values{
		"login": {"admin"}, "password": {token},
	}, jar)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusFound {
		return nil, fmt.Errorf("browser sign-in answered %d: %s", status, truncate(string(payload)))
	}
	return jar, nil
}

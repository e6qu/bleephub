# OpenID Connect (OIDC) SSO

bleephub authenticates browser sessions through **any spec-compliant OpenID
Connect provider**. It is not tied to a particular vendor: at startup it reads
the provider's `/.well-known/openid-configuration` discovery document, runs the
Authorization Code flow with PKCE, and validates the standard claims (issuer,
audience, `nonce`, `state`, expiry, and the OpenID Connect `sid`). RP-Initiated
Logout uses the `end_session_endpoint` advertised in that same discovery
document. Okta, Auth0, Keycloak, Microsoft Entra ID, Google, Ory Hydra, and
[shauth](https://github.com/e6qu/shauth) (the e6qu reference provider) all work
through the same code path and the same configuration variables.

SSO is **optional**. Leave the `BLEEPHUB_SHAUTH_*` variables unset and bleephub
runs standalone with token authentication; whoever operates it handles SSO
separately, or not at all.

> **On the `SHAUTH_` prefix.** The environment variables are named
> `BLEEPHUB_SHAUTH_*` for historical reasons — shauth was the first provider
> wired up. Treat them as generic OIDC settings; they configure whatever
> compliant provider you point the issuer at.

Code links use `github.com/e6qu/bleephub/blob/main/...` permalinks. The identity
flow lives in
[`internal/server/identity.go`](https://github.com/e6qu/bleephub/blob/main/internal/server/identity.go).

## Standards followed

| Standard / control | What bleephub does | Where |
|--------------------|--------------------|-------|
| **Discovery** (OIDC Discovery 1.0) | Reads `/.well-known/openid-configuration` from the configured issuer to locate the authorize, token, JWKS, and `end_session` endpoints | `handleShauthLogin` in [identity.go](https://github.com/e6qu/bleephub/blob/main/internal/server/identity.go) |
| **Authorization Code flow** (OIDC Core §3.1) | Browser → `/auth/shauth` → provider → `/auth/shauth/callback` → code exchange | `handleShauthLogin`, `handleShauthCallback` |
| **PKCE, S256** (RFC 7636) | A per-flow code verifier + `S256` challenge; verifier sent on exchange | `handleShauthLogin` / `handleShauthCallback` |
| **`nonce`** (OIDC Core §3.1.2.1) | Random nonce sent on authorize, verified against the ID token | `handleShauthCallback` |
| **CSRF `state`** | 64-hex random stored in an **HMAC-signed** cookie bound to provider + expiry; verified with `subtle.ConstantTimeCompare` | `randomIdentityState`, `setIdentityState`, `consumeIdentityState` |
| **Audience / `azp`** (OIDC Core §3.1.3.7) | Multi-audience and authorized-party checks beyond the base library | `handleShauthCallback` |
| **Back-channel (RP) logout** | Logout token validated; `sid`/subject sessions revoked with replay protection | `ClaimOIDCLogoutAndDeleteSessions` in [persistence.go](https://github.com/e6qu/bleephub/blob/main/internal/server/persistence.go) |
| **Transport safety** | Issuer/redirect URLs must be HTTPS unless `BLEEPHUB_ALLOW_INSECURE_OIDC=true`; every OIDC fetch is SSRF-address-gated | `validIdentityURL`, `(*Server).oidcClientContext` |

### Why the OIDC client is SSRF-gated

`(*Server).oidcClientContext` builds the OIDC HTTP client from the shared
`newAddressCheckedHTTPTransport`. The issuer is operator-configured, but the
provider-returned discovery document can point the JWKS or `end_session_endpoint`
at an internal or cloud-metadata address; gating the dial prevents that. See
[security.md](security.md#server-side-request-forgery-ssrf).

## Configuration

Set these on the bleephub server process (`identityConfig`, loaded in
[identity.go](https://github.com/e6qu/bleephub/blob/main/internal/server/identity.go);
validation is all-or-nothing — set them together or leave them all unset):

| Variable | Meaning |
|----------|---------|
| `BLEEPHUB_SHAUTH_ISSUER` | The provider issuer URL (e.g. `https://sso.example.com`, an Okta/Auth0/Keycloak realm, `https://accounts.google.com`). HTTPS required unless `BLEEPHUB_ALLOW_INSECURE_OIDC=true`. |
| `BLEEPHUB_SHAUTH_CLIENT_ID` | The OIDC client/app id registered at the provider. |
| `BLEEPHUB_SHAUTH_CLIENT_SECRET` | The client secret. |
| `BLEEPHUB_SHAUTH_POST_LOGOUT_URL` | Where the provider returns after RP-logout; must be `<external-url>/auth/shauth/logout/complete`. |
| `BLEEPHUB_EXTERNAL_URL` | The externally-reachable base URL of this bleephub. Its consistency with the post-logout URL is enforced at startup (`validateShauthExternalURL`). |
| `BLEEPHUB_ALLOW_INSECURE_OIDC` | Dev/test only: permit `http://` issuer/URLs (e.g. a loopback Hydra). Never in production. |

### Register the client at the provider

Register an OIDC client (or "application") at your provider with:

- **Redirect URI:** `<BLEEPHUB_EXTERNAL_URL>/auth/shauth/callback`
- **Post-logout redirect URI:** `<BLEEPHUB_EXTERNAL_URL>/auth/shauth/logout/complete`
- **Front-channel logout URI** / **back-channel logout URI:** the same
  `.../auth/shauth/logout/complete` return endpoint, with
  **`backchannel_logout_session_required: true`** so the provider includes the
  `sid` bleephub revokes on.
- **Grant type:** authorization_code; **response type:** code; **scope:**
  `openid profile email` (plus whatever scope carries your `role` claim).
- The ID token must carry a stable `sub`, a login/username claim, and a
  **`role` claim** (see below).

### Two provider-specific expectations

Any compliant provider works, but two behaviours are not part of core OIDC and
must be arranged:

1. **Privileges come from a `role` claim, not the account.** bleephub reads the
   caller's role from the provider's `role` claim in the ID token. A non-shauth
   provider must be configured to emit a compatible `role` claim (via a custom
   claim / scope / mapper), or every federated user lands with the lowest
   privilege. Adopting a same-named local admin does **not** grant admin unless
   the IdP says so — see [Identity mapping](#identity-mapping-the-important-part).
2. **The logout bridge targets a fixed provider path.** When a user signs out,
   bleephub initiates RP-Initiated Logout to the provider's discovered
   `end_session_endpoint` with an ID-token hint. The app-owned logout bridge
   **ignores any request-supplied post-logout destination** and sends the
   browser only to a fixed provider completion path — shauth's convention is
   `<issuer>/oauth/logout/complete` — which then returns the browser to
   bleephub's `/ui/signed-out`. A non-shauth provider should be configured so
   its logout-complete/redirect lands the user back on
   `<BLEEPHUB_EXTERNAL_URL>/ui/signed-out`.

## Identity mapping (the important part)

External identities are keyed on the **stable `(issuer, subject)` pair**, never
the mutable username — `externalIdentityKey` + `upsertExternalUser` in
[identity.go](https://github.com/e6qu/bleephub/blob/main/internal/server/identity.go):

- A missing/blank stable subject **fails closed** (no login).
- Only the **primary, authoritative IdP** (`roleAuthoritative`) may adopt a
  purely-local, same-named account. An account already bound to a *different*
  federated identity is refused (preserves invariant AUTH-021).
- Privileges come from the provider's `role` claim, **not** from the adopted
  local account — so adopting a local admin does not grant admin unless the IdP
  says so.
- The login name must pass the `loginAllowed` allowlist and `normalizeLogin`
  (which also rejects mixed Latin/Cyrillic homographs).

## Session and logout model

- **Server-side sessions.** A successful callback writes a durable server-side
  session record that retains the verified issuer, subject, `sid`, and the ID
  token. The browser receives only an **opaque, HttpOnly bleephub session
  identifier** — none of the OIDC material is exposed to client-side script.
- **RP-Initiated Logout.** The user-menu sign-out control uses the provider's
  discovered end-session endpoint with an ID-token hint, then follows the fixed
  logout bridge described above back to `/ui/signed-out`.
- **Back-Channel Logout.** A signed OpenID Connect logout token revokes the
  matching session by `sid`, or **every session for the subject** when the
  provider sends `sub` without a `sid`. Replay is rejected
  (`ClaimOIDCLogoutAndDeleteSessions`).
- **The landing page.** The registered post-logout URI is a bleephub-origin
  page that clears any remaining local browser session and waits for an explicit
  user action before starting another sign-in.

## Routes exposed by bleephub

- `GET /auth/shauth` — begins the flow (state/nonce/PKCE, redirect to provider).
- `GET /auth/shauth/callback` — exchanges the code, verifies the ID token, upserts the identity, sets the session.
- `GET /auth/shauth/logout/complete` — RP-logout return endpoint.

Once configured, `/login` transparently forwards to `/auth/shauth` (see
`handleLoginPage` in
[gh_oauth.go](https://github.com/e6qu/bleephub/blob/main/internal/server/gh_oauth.go)).

## The shauth reference provider

[shauth](https://github.com/e6qu/shauth) is e6qu's own custom OIDC provider
(built on Ory Hydra). It is the integration bleephub is tested against, but it is
just one option — pair bleephub with it, run shauth standalone, or use any other
compliant provider.

The end-to-end shauth SSO harness (bleephub + Hydra + Postgres via compose) lives
in [`test/shauth/`](https://github.com/e6qu/bleephub/blob/main/test/shauth),
driven by
[`scripts/test-shauth-sso.sh`](https://github.com/e6qu/bleephub/blob/main/scripts/test-shauth-sso.sh).
The issuer runs on loopback there, so the compose sets
`BLEEPHUB_ALLOW_INSECURE_OIDC=true` — copy this pattern for any local IdP.
(OIDC discovery uses an ordinary client for the operator-configured issuer, not
the webhook SSRF gate, so a loopback issuer needs no private-address opt-out.)
Production deployments use an HTTPS, publicly-resolvable issuer and leave the
flag unset.

## SAML 2.0

For identity providers that speak SAML rather than OpenID Connect, bleephub acts
as a SAML service provider. It reuses the same session and identity machinery as
the OIDC flow — only the assertion transport differs.

Configure three settings together:

- `BLEEPHUB_SAML_IDP_SSO_URL` — the identity provider's single-sign-on endpoint.
- `BLEEPHUB_SAML_IDP_ENTITY_ID` — the identity provider's entityID, which must
  match the `Issuer` on every assertion.
- `BLEEPHUB_SAML_IDP_CERTIFICATE` — the identity provider's X.509 signing
  certificate (PEM, or the bare base64 form found in IdP metadata).

`BLEEPHUB_SAML_SP_ENTITY_ID` optionally overrides the service-provider entityID,
which otherwise defaults to the instance origin (`BLEEPHUB_EXTERNAL_URL`).

The flow:

- `GET /auth/saml` — builds a signed-assertion-expecting `AuthnRequest`,
  deflate-encodes it (HTTP-Redirect binding), and redirects to the IdP. A signed
  state cookie carries the CSRF `RelayState` and the return target.
- `POST /saml/consume` — the assertion consumer service. It validates the
  enveloped XML-DSig signature on the response or its assertion against the
  configured certificate, checks the issuer, audience, recipient, `InResponseTo`,
  and time conditions, maps the attributes to an account, and establishes the
  session. Both service-provider-initiated and identity-provider-initiated flows
  are accepted; the signed assertion is the trust anchor either way.
- `GET /saml/metadata` — publishes the service-provider descriptor for the IdP.

Attribute mapping mirrors GitHub's: the `NameID` is the login unless a
`username` attribute overrides it; `full_name`/`name`, `email`/`emails`, and an
`administrator` attribute set the display name, email, and site-admin flag.
Register `<external-url>/saml/consume` as the assertion consumer service URL at
the identity provider.

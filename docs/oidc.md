# OpenID Connect (OIDC) and shauth SSO

bleephub supports federated SSO through a standard OpenID Connect
provider. The reference integration is **shauth** (the e6qu identity service,
built on Ory Hydra), but any spec-compliant OIDC provider works. This document
covers the standards bleephub follows, exactly where each is implemented, and
how to wire up shauth.

Code links use `github.com/e6qu/bleephub/blob/main/...` permalinks. The whole
identity flow lives in
[`internal/server/identity.go`](https://github.com/e6qu/bleephub/blob/main/internal/server/identity.go).

## Standards followed

| Standard / control | What bleephub does | Where |
|--------------------|--------------------|-------|
| **Authorization Code flow** (OIDC Core §3.1) | Browser → `/auth/shauth` → provider → `/auth/shauth/callback` → code exchange | `handleShauthLogin`, `handleShauthCallback` in [identity.go](https://github.com/e6qu/bleephub/blob/main/internal/server/identity.go) |
| **PKCE, S256** (RFC 7636) | A per-flow code verifier + `S256` challenge; verifier sent on exchange | `handleShauthLogin` / `handleShauthCallback` |
| **`nonce`** (OIDC Core §3.1.2.1) | Random nonce sent on authorize, verified against the ID token | `handleShauthCallback` |
| **CSRF `state`** | 64-hex random stored in an **HMAC-signed** cookie bound to provider + expiry; verified with `subtle.ConstantTimeCompare` | `randomIdentityState`, `setIdentityState`, `consumeIdentityState` |
| **Audience / `azp`** (OIDC Core §3.1.3.7) | Multi-audience and authorized-party checks beyond the base library | `handleShauthCallback` |
| **Issuer discovery** | `github.com/coreos/go-oidc` `NewProvider` against the configured issuer | `handleShauthLogin` |
| **Back-channel (RP) logout** | Logout token validated; `sid`/subject sessions revoked with replay protection | `ClaimOIDCLogoutAndDeleteSessions` in [persistence.go](https://github.com/e6qu/bleephub/blob/main/internal/server/persistence.go) |
| **Transport safety** | Issuer/redirect URLs must be HTTPS unless `BLEEPHUB_ALLOW_INSECURE_OIDC=true`; every OIDC fetch is SSRF-address-gated | `validIdentityURL`, `(*Server).oidcClientContext` |

### Why the OIDC client is SSRF-gated

`(*Server).oidcClientContext` builds the OIDC HTTP client from the shared
`newAddressCheckedHTTPTransport`. The issuer is operator-configured, but the
provider-returned discovery document can point the JWKS or `end_session_endpoint`
at an internal or cloud-metadata address; gating the actual dial prevents that.
See [security.md](security.md#server-side-request-forgery-ssrf).

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

## shauth integration

### Configuration (environment)

Set on the bleephub server process (`identityConfig`, loaded in
[identity.go](https://github.com/e6qu/bleephub/blob/main/internal/server/identity.go);
validation is all-or-nothing):

| Variable | Meaning |
|----------|---------|
| `BLEEPHUB_SHAUTH_ISSUER` | The provider issuer URL (e.g. `https://sso.example.com`). HTTPS required unless `BLEEPHUB_ALLOW_INSECURE_OIDC=true`. |
| `BLEEPHUB_SHAUTH_CLIENT_ID` | The OIDC client/app id registered at the provider. |
| `BLEEPHUB_SHAUTH_CLIENT_SECRET` | The client secret. |
| `BLEEPHUB_SHAUTH_POST_LOGOUT_URL` | Where the provider returns after RP-logout; must be `<external-url>/auth/shauth/logout/complete`. |
| `BLEEPHUB_EXTERNAL_URL` | The externally-reachable base URL of this bleephub. Its consistency with the post-logout URL is enforced at startup (`validateShauthExternalURL`). |
| `BLEEPHUB_ALLOW_INSECURE_OIDC` | Dev/test only: permit `http://` issuer/URLs (e.g. loopback Hydra). |
| `BLEEPHUB_ALLOW_PRIVATE_OUTBOUND_TARGETS` | Dev/test only: permit the SSRF gate to reach a private/loopback issuer. |

### Register the client at the provider

At shauth/Hydra, register an OIDC client with:

- **Redirect URI:** `<BLEEPHUB_EXTERNAL_URL>/auth/shauth/callback`
- **Post-logout redirect URI:** `<BLEEPHUB_EXTERNAL_URL>/auth/shauth/logout/complete`
- **Grant type:** authorization_code; **response type:** code; **scope:** `openid profile email` (plus whatever carries your `role` claim)
- Ensure the ID token carries a stable `sub`, a `role` claim, and the login/username claim.

### Routes exposed by bleephub

- `GET /auth/shauth` — begins the flow (state/nonce/PKCE, redirect to provider).
- `GET /auth/shauth/callback` — exchanges the code, verifies the ID token, upserts the identity, sets the session.
- `GET /auth/shauth/logout/complete` — RP-logout return endpoint.

Once configured, `/login` transparently forwards to `/auth/shauth` (see
`handleLoginPage` in
[gh_oauth.go](https://github.com/e6qu/bleephub/blob/main/internal/server/gh_oauth.go)).

### How to run the reference integration locally

The end-to-end shauth SSO harness (bleephub + Hydra + Postgres via compose) lives
in [`test/shauth/`](https://github.com/e6qu/bleephub/blob/main/test/shauth) and is
driven by
[`scripts/test-shauth-sso.sh`](https://github.com/e6qu/bleephub/blob/main/scripts/test-shauth-sso.sh).
Because the issuer runs on loopback there, the compose sets
`BLEEPHUB_ALLOW_INSECURE_OIDC=true` and `BLEEPHUB_ALLOW_PRIVATE_OUTBOUND_TARGETS=true`
— the pattern to copy for any local IdP. Production deployments use an HTTPS,
publicly-resolvable issuer and leave both flags unset.

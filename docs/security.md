# Security guidelines and how they are implemented

This document describes the security controls bleephub implements, where each
lives in the code, and how the static-analysis and dependency-scanning stack
keeps them enforced. Code links use `github.com/e6qu/bleephub/blob/main/...`
permalinks; the exact line may drift, but the named function is stable.

## Threat model in one line

bleephub is a GitHub Enterprise-compatible appliance. The server accepts
untrusted input from API clients, git transport, webhooks it *receives*, and
OAuth/OIDC redirects, and it makes *outbound* requests (webhook delivery, source
imports, OIDC discovery, Pages artifact fetches). The controls below defend the
trust boundary in both directions.

## Server-Side Request Forgery (SSRF)

Every server-initiated fetch is routed through one shared address gate so the
policy cannot drift between call sites.

- **Central gate** — [`internal/server/webhooks.go`](https://github.com/e6qu/bleephub/blob/main/internal/server/webhooks.go):
  `nonPublicIP` / `webhookAddrBlocked` reject anything that is not a public,
  global-unicast address — loopback, link-local (which covers the
  `169.254.169.254` cloud-metadata endpoint), RFC1918, IPv6 ULA, and CGNAT
  `100.64.0.0/10`. `tunnelledIPv4` unwraps IPv4-in-IPv6 forms (`::ffff:`, NAT64
  `64:ff9b::/96`) and re-checks, closing the tunnel bypass.
- **Dial-time enforcement** — `webhookDialControl` is installed as
  `net.Dialer.Control`, so the check runs on the address the kernel is actually
  about to connect to, defeating DNS rebinding. Redirects return
  `http.ErrUseLastResponse`, so a 3xx can never reach a second unchecked host.
- **Shared transport** — `newAddressCheckedHTTPTransport(allowPrivate, insecureTLS)`
  is the single constructor for gated clients. It is used by:
  - webhook delivery and source imports ([`gh_import.go`](https://github.com/e6qu/bleephub/blob/main/internal/server/gh_import.go)),
  - OIDC discovery / JWKS / token / end-session (`(*Server).oidcClientContext` in
    [`identity.go`](https://github.com/e6qu/bleephub/blob/main/internal/server/identity.go)),
  - the Pages deployment artifact fetch (`readPagesDeploymentArtifact` in
    [`gh_pages_deployments.go`](https://github.com/e6qu/bleephub/blob/main/internal/server/gh_pages_deployments.go)).
- **Fixed policy** — loopback is permitted (legitimate same-host delivery for an
  on-prem/dev instance); every other non-public address — the cloud metadata
  endpoint, RFC1918, IPv6 unique-local space, carrier-grade NAT, link-local —
  is refused unconditionally. There is no switch to turn the protection off.

CodeQL still reports `go/request-forgery` on the two operator/deployment-driven
fetch sites; those are reviewed-safe and documented in
`scripts/check-codeql-sarif.py` because CodeQL's taint analysis does not model
the dial-time gate as a sanitizer.

## Open redirects

- Login/logout `return_to` is **relative-path-only** via `safeIdentityReturnTo`
  ([`identity.go`](https://github.com/e6qu/bleephub/blob/main/internal/server/identity.go)):
  absolute URLs, `//host`, and backslash forms fall back to `/ui/`. It is now
  applied at the entry point (`handleLoginPage`) as well as on consume, so no
  unsafe value propagates through the flow.
- OAuth authorize/callback redirects go **only** to the app's registered
  `redirect_uri`, enforced by `requireRegisteredRedirectURI` /
  `redirectURIMatchesRegistration` in
  [`gh_oauth.go`](https://github.com/e6qu/bleephub/blob/main/internal/server/gh_oauth.go)
  (exact or segment-boundary-prefix match, both `path.Clean`ed). A client with no
  registered callback is refused.

## Cross-Site Scripting (XSS) and output handling

- **Markdown** is rendered with goldmark **without** `WithUnsafe`
  ([`gh_markdown.go`](https://github.com/e6qu/bleephub/blob/main/internal/server/gh_markdown.go)),
  so embedded raw HTML is escaped.
- **Server-rendered HTML** (login, OAuth consent, identity pages) escapes every
  user-influenced value with `html.EscapeString`.
- **Global security headers** — `securityHeadersMiddleware`
  ([`server.go`](https://github.com/e6qu/bleephub/blob/main/internal/server/server.go))
  wraps the whole handler chain and sets `X-Content-Type-Options: nosniff` and
  `Referrer-Policy` on every response, `Strict-Transport-Security` over TLS, and,
  for the `/ui` SPA, `X-Frame-Options: DENY` plus a `Content-Security-Policy`
  (`script-src 'self'`, `frame-ancestors 'none'`, …). Stricter per-endpoint CSPs
  (identity pages, the Pages sandbox) run afterwards and win.
- The SPA asserts it never uses `dangerouslySetInnerHTML`
  (`web/src/__tests__/securityInvariants.test.ts`).

## TLS and certificate validation

- The only intentional `InsecureSkipVerify` is in `newAddressCheckedHTTPTransport`,
  gated strictly behind a webhook's `insecure_ssl=1` config (matching GitHub);
  even then, redirect-blocking and private-address dial gating stay active.
- Cluster (dqlite) transport pins `MinVersion: tls.VersionTLS13` with a private
  root CA and `ServerName` pinning
  ([`internal/dqliteaddr/tls.go`](https://github.com/e6qu/bleephub/blob/main/internal/dqliteaddr/tls.go)) — no skip-verify.

## Secrets

No secrets are hardcoded in non-test code. Signing/encryption material is
runtime-generated or environment-sourced: the per-process identity-state HMAC key
(`rand.Read`), the runner signing key (`BLEEPHUB_RUNNER_TOKEN_KEY` or random),
OIDC/app RSA keys (`rsa.GenerateKey`), and AES-GCM at-rest encryption
([`persistence.go`](https://github.com/e6qu/bleephub/blob/main/internal/server/persistence.go)).
All token/ID randomness uses `crypto/rand`, never `math/rand`. The gosec G101
hits on the typed `contextKey` constants are false positives and carry a
`#nosec G101` rationale.

## Command injection

The two `exec.Command` sites — the Jekyll Pages build
([`gh_pages_branch_build.go`](https://github.com/e6qu/bleephub/blob/main/internal/server/gh_pages_branch_build.go))
and the Docker CLI for codespaces
([`store_codespaces.go`](https://github.com/e6qu/bleephub/blob/main/internal/server/store_codespaces.go)) —
use argument-vector form (no shell), with fixed subcommands and server-created
paths. There is no `sh -c` or shell interpolation anywhere in non-test code.

## Static analysis and dependency scanning

All of the following are required CI gates (`.github/workflows/ci.yml`,
`.github/workflows/codeql.yml`); `main` is kept green.

| Tool | Scope | Notes |
|------|-------|-------|
| **CodeQL** | Go + JS/TS | `security-extended,security-and-quality`; a custom fail-closed SARIF policy (`scripts/check-codeql-sarif.py`) rejects any finding not in a per-file reviewed allowlist |
| **gosec** | Go | `-exclude=G101` (public token prefixes / context keys); local run reports **0** issues |
| **staticcheck** | Go | correctness/lint |
| **govulncheck** | Go (4 modules) | call-reachable vuln gate |
| **Semgrep** | repo | `p/security-audit` + `p/secrets`, `--error` |
| **Trivy** | fs + images | `vuln,misconfig,secret`; `.trivyignore.yaml` holds a few time-boxed, justified entries |
| **zizmor** | workflows | GitHub Actions supply-chain audit |
| **dependency-age** | all ecosystems | 24-hour quarantine (`scripts/check-dependency-age.py`) blocks freshly-published (supply-chain-risky) versions |

**Snyk** (Open Source / dependency scanning) is run **locally** by maintainers as
an additional check — `snyk test --all-projects` should report no vulnerable
paths. It is intentionally not wired into CI.

### How suppressions work

A finding is suppressed only with a written justification, in one of:
`ACCEPTED_FINDINGS` (CodeQL, per rule+file), inline `#nosec <rule> -- reason`
(gosec), `nosemgrep: <rule>` (Semgrep), or `.trivyignore.yaml` (time-boxed).
Each records why the finding is non-exploitable or intentional (e.g. GitHub-compat
behavior like the SHA-1 `X-Hub-Signature` HMAC or `insecure_ssl` webhooks, which
cannot be removed without breaking parity).

## Reporting

See [`SECURITY.md`](https://github.com/e6qu/bleephub/blob/main/SECURITY.md) for the
private vulnerability-reporting process.

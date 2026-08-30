# Using `gh` CLI against bleephub

bleephub speaks the same REST + GraphQL surface as GitHub Enterprise Server (`/api/v3/` path prefix, `/api/graphql` endpoint, GHES service routing). The `gh` CLI works against it directly — no shims, no `gh api` URL hackery, no flags.

## The mental model — `--hostname`, not a base URL

`gh` takes no base-URL argument. It identifies a target by **hostname** and derives the URLs from a fixed rule:

| Host | API base | GraphQL |
|---|---|---|
| `github.com` | `https://api.github.com/` | `https://api.github.com/graphql` |
| anything else | `https://<host>/api/v3/` | `https://<host>/api/graphql` |

Run `gh auth login --hostname localhost --with-token` and `gh` writes a record to `~/.config/gh/hosts.yml` under the key `localhost`, then builds every API call as `https://localhost/api/v3/...`. bleephub serves both `/api/v3/` and `/api/graphql` — that's the entire wiring story.

Three consequences:

- **`gh` is HTTPS-only against any non-`github.com` host.** Plain HTTP on `:5555`/`:8080` will not work — `gh` (verified on 2.97.0) forces `https://` and refuses a plain-HTTP server. Front bleephub with a locally-trusted TLS endpoint (see [Giving `gh` HTTPS](#giving-gh-https), and the [README quick start](../README.md#using-the-gh-cli)).
- **`gh auth login --hostname` accepts a bare hostname only.** Current `gh` (verified on 2.92.0) rejects `host:port` with `error parsing hostname: invalid hostname`, so the login flow requires bleephub on `:443`. To avoid binding 443, skip `gh auth login`: `export GH_HOST=localhost:8443` + `export GH_ENTERPRISE_TOKEN=<token>` — the runtime accepts a port in `GH_HOST` and the env token replaces the hosts.yml entry. Use `GH_ENTERPRISE_TOKEN`, not `GH_TOKEN`: gh reads `GH_TOKEN` only for `github.com` and sends nothing to other hosts when only `GH_TOKEN` is set (bleephub answers `401 Bad credentials`).
- **Trust must reach the system store, and it differs by OS.** `gh` is a Go binary; Go on darwin reads trust only from the keychain (it ignores `SSL_CERT_FILE`/`SSL_CERT_DIR`), while Linux uses `/usr/local/share/ca-certificates` + `update-ca-certificates`. The Caddy recipe below papers over that difference — `caddy trust` installs its CA into the right store on both.

## Giving `gh` HTTPS

### Recommended: Caddy reverse proxy (macOS + Linux)

[Caddy](https://caddyserver.com) mints a certificate from a local CA and installs
that CA into the system trust store on both macOS and Linux, so one recipe works
everywhere.

First, run bleephub on plain HTTP:

```bash
BLEEPHUB_ADMIN_TOKEN="$TOKEN" ./bleephub-server --addr :8080 &
```

Trust Caddy's local certificate authority. `caddy trust` installs it into the
macOS system keychain or the Linux `ca-certificates` store:

```bash
caddy trust
```

To install the CA by hand instead — for example when `caddy trust` cannot
elevate — Caddy writes its root to `root.crt` under its data directory. On
**macOS** the root is at `~/Library/Application Support/Caddy/pki/authorities/local/root.crt`,
and `gh` (a Go binary) reads trust only from the system keychain:

```bash
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain \
  "$HOME/Library/Application Support/Caddy/pki/authorities/local/root.crt"
```

On **Linux** the root is at `~/.local/share/caddy/pki/authorities/local/root.crt`.
Debian and Ubuntu trust a certificate copied into `/usr/local/share/ca-certificates`:

```bash
sudo cp "$HOME/.local/share/caddy/pki/authorities/local/root.crt" \
  /usr/local/share/ca-certificates/caddy-local-ca.crt
sudo update-ca-certificates
```

Fedora, RHEL, and their derivatives use a different anchor directory and refresh
command:

```bash
sudo cp "$HOME/.local/share/caddy/pki/authorities/local/root.crt" \
  /etc/pki/ca-trust/source/anchors/caddy-local-ca.crt
sudo update-ca-trust
```

With the CA trusted, terminate HTTPS on `:8443`, reverse-proxy it to bleephub's
HTTP port, and point `gh` at the front door:

```bash
caddy reverse-proxy --from localhost:8443 --to localhost:8080 &
export GH_HOST=localhost:8443
export GH_ENTERPRISE_TOKEN="$TOKEN"
gh repo create demo --public
```

Teardown: stop the two background processes and run `caddy untrust` to remove the
local CA from the system store.

### Manual alternative: self-signed cert + system trust

If you would rather not run Caddy, bleephub serves TLS directly from
`BPH_TLS_CERT` + `BPH_TLS_KEY`. Mint a `localhost` cert and trust it yourself —
the trust step is OS-specific:

Mint a `localhost` certificate:

```bash
BPH_TLS_DIR="$HOME/.bleephub/tls"; mkdir -p "$BPH_TLS_DIR"
openssl req -x509 -newkey rsa:2048 -days 825 -nodes \
  -keyout "$BPH_TLS_DIR/bph.key" -out "$BPH_TLS_DIR/bph.crt" \
  -subj "/CN=localhost" -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"
```

Trust it. On macOS, Go — and therefore `gh` — reads trust only from the keychain:

```bash
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain "$BPH_TLS_DIR/bph.crt"
```

On Debian or Ubuntu, install it into the `ca-certificates` store:

```bash
sudo cp "$BPH_TLS_DIR/bph.crt" /usr/local/share/ca-certificates/bleephub.crt
sudo update-ca-certificates
```

Then serve TLS directly from `BPH_TLS_CERT` + `BPH_TLS_KEY` and point `gh` at it:

```bash
BLEEPHUB_ADMIN_TOKEN="$TOKEN" \
  BPH_TLS_CERT="$BPH_TLS_DIR/bph.crt" BPH_TLS_KEY="$BPH_TLS_DIR/bph.key" \
  ./bleephub-server --addr :8443 &
export GH_HOST=localhost:8443
export GH_ENTERPRISE_TOKEN="$TOKEN"
```

The Docker `make gh-test` harness uses this self-signed + trust path internally.
As a last resort for one-off `gh api` calls, `gh api --insecure` skips
verification entirely.

## One-time auth

The admin user's token is whatever `BLEEPHUB_ADMIN_TOKEN` was set to when
Bleephub started (required, no default — see the README). The Docker harnesses
use this value:

```bash
TOKEN="bleephub-admin-token-00000000000000000000"
```

Option A — bleephub on any port (for example `:8443`), no `gh auth login`
needed. `GH_HOST` accepts `host:port` at runtime, and `GH_ENTERPRISE_TOKEN` is
the credential `gh` uses for every non-github.com host (`GH_TOKEN` is
github.com-only and is silently ignored here):

```bash
export GH_HOST=localhost:8443
export GH_ENTERPRISE_TOKEN="$TOKEN"
```

Option B — bleephub on `:443`: the bare hostname passes `gh auth login`'s
validator, giving you a persistent `~/.config/gh/hosts.yml` entry:

```bash
echo "$TOKEN" | gh auth login --hostname localhost --with-token
export GH_HOST=localhost
```

Mint other tokens (OAuth user, installation server-to-server) via the OAuth flow or the real GitHub endpoint `POST /api/v3/app/installations/{installation_id}/access_tokens` (JWT-authenticated), then use the result in place of `$TOKEN` on the `gh auth login` line.

`gh` is now authenticated against bleephub.

Setup and teardown (server, TLS front end, trust, gh wiring) are covered by the
[Caddy and manual recipes above](#giving-gh-https) and the condensed
[README quick start](../README.md#using-the-gh-cli).

## Supported commands

These work natively (no `gh api` workaround) and are each exercised by the `make gh-test` harness (`test/run-gh-test.sh`):

| Command | Endpoint(s) |
|---|---|
| `gh repo create <name>` | `POST /user/repos` |
| `gh repo view <owner/name>` | `GET /repos/{o}/{r}` + GraphQL `repository` |
| `gh repo list <owner>` | GraphQL `repositoryOwner(login).repositories` |
| `gh repo clone <owner/name>` | GraphQL `RepositoryInfo` (`hasWikiEnabled`, `parent`) + smart-HTTP git protocol |
| `gh issue create --title --body` | GraphQL `createIssue` mutation |
| `gh issue view <N>` | GraphQL `repository.issueOrPullRequest` (Issue\|PullRequest union) |
| `gh issue list` | GraphQL `repository.issues` connection; `--label`/`--author`/`--search` route through GraphQL `search(type: ISSUE)` gated on `GET /meta` feature detection |
| `gh issue close / reopen <N>` | GraphQL `closeIssue` / `reopenIssue` mutations |
| `gh pr create` (in a git working dir) | GraphQL `RepositoryInfo` + `createPullRequest` mutation |
| `gh pr view <N>` | GraphQL `repository.pullRequest` (incl. `statusCheckRollup` via `commits(last:1)`, backed by the checks store) |
| `gh pr list` | GraphQL `repository.pullRequests` connection (enum `orderBy`) |
| `gh pr merge <N>` | GraphQL `mergePullRequest` mutation (finder reads `mergeStateStatus` + `commits(last:1)`) |
| `gh pr review --approve` / `--request-changes` / `--comment` | GraphQL `addPullRequestReview` mutation |
| `gh pr comment <N>` | GraphQL `addComment` mutation |
| `gh release create <tag>` | `POST /repos/{o}/{r}/releases` |
| `gh release list` | GraphQL `repository.releases` connection |
| `gh release view / delete` | `GET`/`DELETE /repos/{o}/{r}/releases*` + GraphQL `repository.release(tagName:)` draft lookup |
| `gh run list / view` | `GET /repos/{o}/{r}/actions/runs*` (push-triggered runs resolve their `workflow_id`) |
| `gh workflow run <wf> --ref <branch>` | `POST /repos/{o}/{r}/actions/workflows/{id}/dispatches`; version-gated on `GET /meta` |
| `gh workflow list / view` | `GET /actions/workflows[/{id}]` |
| `gh workflow enable / disable` | `PUT /actions/workflows/{id}/{enable,disable}`; disabled workflows don't trigger and dispatch returns 403 |
| `gh secret set / list / delete` | `GET /actions/secrets/public-key` + libsodium sealed-box `PUT {encrypted_value, key_id}` / `GET /actions/secrets` / `DELETE /actions/secrets/{name}`; org + environment scopes too |
| `gh variable set / get / list / delete` | `POST`/`PATCH`/`GET`/`DELETE /actions/variables[/{name}]` (gh's POST→409→PATCH update fallback works); org + environment scopes too |
| `gh org list` | GraphQL `user(login:).organizations` connection |
| `gh api /repos/{o}/{r}/...` | direct REST passthrough |

## Documented but not exercised by the harness

The server routes and resolvers backing these verbs exist, but the `make gh-test`
suite does not assert on the verb itself. Treat them as implemented-but-unverified
until a harness assertion covers them.

| Command | Endpoint(s) | Status |
|---|---|---|
| `gh repo delete <owner/name>` | `DELETE /repos/{o}/{r}` | route implemented; no harness assertion |
| `gh issue comment <N> --body` | GraphQL `addComment` mutation | mutation implemented; the harness covers it via `gh pr comment`, not the issue verb |
| `gh pr status` | GraphQL `search(type: ISSUE)` + `repository.pullRequests`; needs `GET /meta` | resolvers implemented; the `search(type: ISSUE)` path is exercised through `gh issue list --label`, but the `gh pr status` verb is not |
| `gh run cancel / rerun` | `POST /repos/{o}/{r}/actions/runs/{id}/cancel`, `.../rerun` | routes implemented; the harness exercises `gh run list / view`, not cancel/rerun |
| `gh release download` | `GET /repos/{o}/{r}/releases/assets/{asset_id}` (asset upload/list/download all implemented) | partial: the asset endpoints work, but no harness release attaches an asset, so `gh release download` against a harness-created release finds nothing to download |

## Endpoints with no native `gh` verb

Use `gh api` for these (real GH also doesn't expose them in `gh`'s top-level commands):

| Command | Purpose |
|---|---|
| `gh api /apps/<slug>` | public app lookup (anonymous-allowed) |
| `gh api -X PUT /app/installations/{id}/suspended` | suspend an installation |
| `gh api -X DELETE /app/installations/{id}/suspended` | unsuspend an installation |
| `gh api /installation/repositories` | `ghs_`-token-scoped repositories |
| `gh api /repos/{o}/{r}/environments` | environment list |
| `gh api -X POST /repos/{o}/{r}/dispatches -f event_type=deploy` | repository_dispatch |
| `gh api /repos/{o}/{r}/branches/main/protection` | branch protection |
| `gh api /token` | Actions OIDC token |
| `gh api /.well-known/jwks` | JWKS for cloud-IdP verification |

## Tokens at a glance

| Prefix | Issued by | Scope model | Use case |
|---|---|---|---|
| (admin) | `BLEEPHUB_ADMIN_TOKEN` env var at startup | All scopes | Operator/admin token; bypasses `requirePerm` |
| `ghp_` | `POST /login/oauth/access_token` (legacy) | All scopes | Classic PAT |
| `gho_` | OAuth web/device flow (OAuth App) | Classic OAuth scopes (`repo`, `read:org`, …) | OAuth App user tokens |
| `ghu_` | OAuth flow against a GitHub App | App installation perms | GitHub App user-to-server |
| `ghs_` | `POST /app/installations/{id}/access_tokens` | Installation-scoped perms | Server-to-server |
| `ghr_` | Paired with `gho_` / `ghu_` | — | Refresh token (6 month TTL) |

`requirePerm(scope, level)` enforces permissions on write-class endpoints. PATs bypass; `ghs_` / `ghu_` / `gho_` are checked against their respective scope tables. For how installation-token permissions and selected-repository downscoping are enforced, see the [Authentication, Apps, and events](../specs/BLEEPHUB_GITHUB_API_PARITY.md#authentication-apps-and-events) section of the parity audit.

## Body coercion

bleephub accepts both typed and string-coerced JSON booleans/integers — `gh api -f` sends string `"false"`, which coerces to bool `false` server-side, exactly as Rails does on real GH. `gh api -F` (typed) also works. Don't substitute one form for the other; bleephub accepts what real GH accepts.

## Testing your gh setup end-to-end

This round-trip creates a repo, opens an issue, reacts, comments, and closes it:

```bash
gh repo create bleephub-test --public --description "smoke"
ISSUE=$(gh issue create --repo admin/bleephub-test --title "first" --body "hello")
gh issue view 1 --repo admin/bleephub-test
gh api -X POST /repos/admin/bleephub-test/issues/1/reactions -f content="rocket"
gh issue comment 1 --repo admin/bleephub-test --body "great work"
gh issue close 1 --repo admin/bleephub-test
gh issue list --repo admin/bleephub-test --state closed
```

For a comprehensive smoke test, run [`make gh-test`](../Makefile): it spins up Bleephub + the official `gh` binary in Docker with TLS and exercises the full gh-CLI assertion suite (repos, issues, PRs, reactions, releases, runs, apps, OAuth). It runs in CI as the `GitHub CLI compatibility` job.

## When things go wrong

- **`gh auth login` keeps asking for credentials.** Use `--with-token` with a non-empty token. `GH_ENTERPRISE_TOKEN` also works as an env fallback (not `GH_TOKEN` — that's read for `github.com` only).
- **`gh` is hitting `github.com` instead of bleephub.** You forgot `--hostname <bleephub-host>` on `gh auth login`, or `GH_HOST` isn't exported. `gh` routes to bleephub only if the hostname is in `~/.config/gh/hosts.yml` AND either `GH_HOST` matches it or every command passes `--hostname` explicitly.
- **`gh auth login` fails with `dial tcp [::1]:443: connection refused` / `x509: cannot validate ...`.** bleephub is on a plain-HTTP port, or its cert isn't trusted. `gh` is HTTPS-only — run bleephub with `BPH_TLS_CERT` + `BPH_TLS_KEY` and trust the CA system-wide. To avoid binding `:443`, skip `gh auth login` (it rejects `host:port`) and use `GH_HOST=localhost:8443` + `GH_ENTERPRISE_TOKEN`.
- **`gh repo list` returns empty / 404.** GraphQL queries depend on the `repositoryOwner` resolver — confirm your bleephub binary is current.
- **`gh issue view` returns "fragment cannot be spread"-style errors.** Should be impossible (the `IssueOrPullRequest` union is wired). File a [BUGS.md](../BUGS.md) entry if seen.
- **`gh api -f` returns 400.** Should not happen (`flexBool`/`flexInt` decoders handle string-coerced inputs). File a bug.
- **TLS errors.** With `BPH_TLS_CERT` and a self-signed cert, either trust the CA system-wide (the Docker harness does this) or pass `--insecure` to `gh api`.

See also the [Executable inventory](../specs/BLEEPHUB_GITHUB_API_PARITY.md#executable-inventory) section of the parity audit, backed by [`specs/parity-inventory.json`](../specs/parity-inventory.json) — the machine-readable per-endpoint inventory.

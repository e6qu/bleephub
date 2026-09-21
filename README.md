# bleephub

Bleephub is a faithful, self-contained Go reimplementation of GitHub Enterprise's server-side surface — the REST API (`/api/v3`), the GraphQL API (`/api/graphql`), the `actions/runner` protocol (`/_apis/`), smart-HTTP git, and an embedded web UI, all from one binary on one port. Unmodified GitHub tooling talks to it exactly as it talks to github.com or GitHub Enterprise Server: the `gh` CLI, `go-github`, Octokit, Probot, the official `actions/runner`, and `git` itself.

| Adaptor | Min version | Proves |
|---|---|---|
| [`gh` CLI](https://cli.github.com/manual/) | 2.50+ | End-to-end CLI verbs (repos, issues, pull requests, releases, runs). |
| [`go-github`](https://github.com/google/go-github) | v88 | Typed REST SDK coverage, incl. Git Data and Actions. |
| [`actions/runner`](https://github.com/actions/runner) | v2.319+ | The runner-server `/_apis/` protocol, end to end. |
| [Smart-HTTP git](https://git-scm.com/docs/http-protocol) | git 2.40+ | `git clone` / `git push`; used by `actions/checkout`. |

## What it implements

Essentially all of GitHub's server-side surface: the REST and GraphQL APIs, the Actions runner protocol and a real workflow engine (including the merge queue and scheduled workflows), smart-HTTP and SSH git, webhooks, GitHub Apps and OAuth Apps, Packages and the container registry, Pages, code scanning and CodeQL, artifact attestations, deployments and environments, and organization/team administration. Sign-in supports token, OpenID Connect, and SAML 2.0, with SCIM provisioning at organization and enterprise scope. The exact parity boundary — per route and per field — is audited in [`specs/BLEEPHUB_GITHUB_API_PARITY.md`](specs/BLEEPHUB_GITHUB_API_PARITY.md), and [`BUGS.md`](BUGS.md) records the audited non-defects (findings whose premise is wrong).

The one thing to bring yourself is a runner: Bleephub has no GitHub-hosted runners, so a connected `actions/runner` executes workflow jobs (which is what `gh run watch` polls).

## Quick start

Bleephub serves plain HTTP with no certificate. `make build` produces `./bleephub-server` with the web UI embedded. Run it, and point a browser, `curl`, or `git` at it directly:

```bash
make build
BLEEPHUB_ADMIN_TOKEN=bleephub-admin-token-00000000000000000000 \
  ./bleephub-server --addr :8080
```

It logs `listening on http://localhost:8080`. `BLEEPHUB_ADMIN_TOKEN` is **required** and has no default — pick any value that is not shaped like a GitHub personal access token. It authenticates every API call. Then open the dashboard at `http://localhost:8080/ui/`, call the API, and clone a repo:

```bash
open http://localhost:8080/ui/
curl -H "Authorization: Bearer bleephub-admin-token-00000000000000000000" \
  http://localhost:8080/api/v3/user
git clone http://localhost:8080/admin/demo.git
```

No TLS certificate, no keychain trust. `go-github`, Octokit, and Probot work against the same `http://` origin.

### Using the `gh` CLI

`gh` (verified against gh 2.97.0) is the one exception: against any non-`github.com` host it forces `https://` and refuses a plain-HTTP server, so `gh` — and only `gh` — needs TLS. The simplest cross-platform way to give it a locally-trusted certificate is [Caddy](https://caddyserver.com) as an HTTPS reverse proxy in front of bleephub's HTTP port. The same recipe works on macOS and Linux.

First, run bleephub on plain HTTP, exactly as in the quick start above:

```bash
BLEEPHUB_ADMIN_TOKEN=bleephub-admin-token-00000000000000000000 \
  ./bleephub-server --addr :8080 &
```

Next, install Caddy's local certificate authority into the system trust store. `caddy trust` does this for you on both operating systems — the macOS system keychain, or the Linux `ca-certificates` store:

```bash
caddy trust
```

To install the CA by hand instead — for example when `caddy trust` cannot elevate, or to trust it in a browser — Caddy writes its root to `root.crt` under its data directory, and each OS trusts it its own way. On **macOS** the root is at `~/Library/Application Support/Caddy/pki/authorities/local/root.crt`, and `gh` (a Go binary) reads trust only from the system keychain:

```bash
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain \
  "$HOME/Library/Application Support/Caddy/pki/authorities/local/root.crt"
```

On **Linux** the root is at `~/.local/share/caddy/pki/authorities/local/root.crt`. Debian and Ubuntu trust a certificate copied into `/usr/local/share/ca-certificates`, then refreshed:

```bash
sudo cp "$HOME/.local/share/caddy/pki/authorities/local/root.crt" \
  /usr/local/share/ca-certificates/caddy-local-ca.crt
sudo update-ca-certificates
```

Fedora, RHEL, and their derivatives use a different anchor directory and refresh command:

```bash
sudo cp "$HOME/.local/share/caddy/pki/authorities/local/root.crt" \
  /etc/pki/ca-trust/source/anchors/caddy-local-ca.crt
sudo update-ca-trust
```

With the CA trusted, terminate HTTPS on `:8443` and reverse-proxy it to bleephub's HTTP port:

```bash
caddy reverse-proxy --from localhost:8443 --to localhost:8080 &
```

Finally, point `gh` at the HTTPS front door. Use `GH_ENTERPRISE_TOKEN`, not `GH_TOKEN` — `gh` reads `GH_TOKEN` only for github.com and sends it to no other host:

```bash
export GH_HOST=localhost:8443
export GH_ENTERPRISE_TOKEN=bleephub-admin-token-00000000000000000000
gh repo create demo --public
gh issue create --repo admin/demo --title first --body hi
```

Prefer the Caddy path; a manual `openssl` + system-keychain alternative and the full story — teardown, the `:443` login variant, the supported command ↔ endpoint table, token prefixes, body coercion, and troubleshooting — are in [docs/BLEEPHUB_GH_CLI.md](docs/BLEEPHUB_GH_CLI.md). For an end-to-end Docker smoke (TLS, CA trust, gh CLI, harness), run [`make gh-test`](#integration-tests).

### Bleephub UI

The Go binary embeds a React single-page application at `/ui/` (`go embed`, build tag `!noui`, on by default). It is styled to feel like GitHub without copying it verbatim, with a light/dark toggle: **Overview**, **Repos** (per-repo Code / Issues / Pull requests / Commits / Releases / Webhooks / Secrets / Environments), **Workflows** (files, runs, per-job logs), **Runners**, **Apps**, **OAuth**, and **Metrics**. A Shauth-configured deployment routes unauthenticated launches through SSO automatically; standalone development keeps the local and GitHub-compatible token paths.

To iterate on the UI without rebuilding the Go binary, run the Vite dev server (`cd web && bun install && bun run dev`) and open `http://localhost:5173/ui/`; it proxies API paths to the running server (`server.proxy` in `vite.config.ts`). `make build` rebuilds the embedded copy.

### SSO (optional)

SSO is optional. Leave the `BLEEPHUB_SHAUTH_*` variables unset and Bleephub runs standalone with token authentication.

To enable it, Bleephub integrates with [shauth](https://github.com/e6qu/shauth), a custom OpenID Connect provider: set `BLEEPHUB_SHAUTH_ISSUER`, `BLEEPHUB_SHAUTH_CLIENT_ID`, `BLEEPHUB_SHAUTH_CLIENT_SECRET`, and `BLEEPHUB_SHAUTH_POST_LOGOUT_URL` together, and register the deployment's redirect and front/back-channel logout URIs with the provider. Any standard OpenID Connect provider works through the same mechanism.

Bleephub also supports **SAML 2.0** as a service provider: set `BLEEPHUB_SAML_IDP_SSO_URL`, `BLEEPHUB_SAML_IDP_ENTITY_ID`, and `BLEEPHUB_SAML_IDP_CERTIFICATE` (the identity provider's signing certificate), and register `<external-url>/saml/consume` as the assertion consumer service — its metadata is published at `/saml/metadata`. The full configuration, verification flow, session model, and logout contract are documented in [docs/oidc.md](docs/oidc.md).

### Outbound request safety

Every fetch Bleephub itself initiates — webhook delivery and repository source
import — refuses any target that is not a public address: loopback, link-local
(including the `169.254.169.254` instance-metadata endpoint), RFC1918,
carrier-grade NAT and IPv6 unique-local space, and any scheme other than
`http`/`https`. Webhook delivery checks the address when the hook is
configured and again against the address actually dialed, and does not follow
redirects. Source import is checked at request time only — its fetch runs
through go-git, whose transport is chosen from a process-global registry, so
there is no per-fetch dial hook to re-check the address. Loopback is
the one non-public range permitted — delivering to a service on the same host
is a legitimate on-prem/dev target — and no switch relaxes the
rest: the cloud metadata endpoint and other private space stay refused
unconditionally.

### One-command local dev

For day-to-day hacking, the convenience script wraps the build/run steps:

```bash
./scripts/local-dev.sh start
./scripts/local-dev.sh start --dev
./scripts/local-dev.sh start --tls
./scripts/local-dev.sh status | logs | stop
./scripts/local-dev.sh clean
```

`start` serves HTTP on `:5555` with the embedded UI; `start --dev` adds the Vite UI on `:5173` with hot module replacement; `start --tls` serves HTTPS on `:8443` with a self-signed cert; `clean` removes the local data, logs, and PID files. It compiles the current source, starts the server and UI, and prints the endpoints, admin token, data directory, and log paths. Data, git storage, logs, and the PID file live under `.local/bleephub/` by default (override with `BLEEPHUB_DATA_DIR` / `BLEEPHUB_GIT_DIR`).

## Configuration

```bash
make build
BLEEPHUB_ADMIN_TOKEN=<token> ./bleephub-server --addr :80 --log-level info
```

`make build` produces `./bleephub-server`; `make run` builds and runs it on `:5555` (still requiring `BLEEPHUB_ADMIN_TOKEN`). Flags: `--addr` (listen address, default `:5555`; the runner strips non-standard ports, so use 80/443 for runner integration tests) and `--log-level` (`debug` | `info` | `warn` | `error`, default `info`).

**Core**

- `BLEEPHUB_ADMIN_TOKEN` — **required.** The seeded admin token; no default (a default would be a guessable credential). Use a value not shaped like a personal access token; the binary fails loudly at startup if unset.
- `BLEEPHUB_MONITORING_TOKEN` — optional bearer credential (≥32 non-whitespace chars) for `GET /monitoring/observation`; without it the endpoint refuses every caller.
- `BLEEPHUB_EXTERNAL_URL` — the origin Bleephub is reached on, used for absolute URLs in API responses and webhook payloads (trailing slash trimmed). Unset derives URLs per request.
- `BLEEPHUB_ADMIN_HOST` — when set, `GET /` on that hostname serves the dashboard instead of the API root.
- `BLEEPHUB_ENTERPRISE_SLUG` — the slug the enterprise endpoints answer on (default `bleephub`).
- `BLEEPHUB_MAX_WORKFLOWS` — concurrency cap (default 10).
- `OTEL_EXPORTER_OTLP_ENDPOINT` — when set, emits OTLP traces/metrics/logs (off by default).
- `BPH_TLS_CERT` + `BPH_TLS_KEY` — serve over TLS (see the `gh` section above).

**Persistence** (off by default; in-memory otherwise)

- `BLEEPHUB_PERSIST=true` — enable the SQLite write-through database at `<BLEEPHUB_DATA_DIR>/bleephub.db`. Persists the full metadata surface; runner/workflow runtime state and the rotating Actions OIDC signing key are intentionally not persisted.
- `BLEEPHUB_PERSISTENCE_ENCRYPTION_KEY` — **required when persistence is enabled.** A stable base64-encoded 32-byte key for AES-256-GCM encryption of credentials at rest; a missing or wrong key fails startup loudly. Terraform injects it via AWS Secrets Manager.
- `BLEEPHUB_DATA_DIR` — directory for the SQLite database and local development metadata (default `.`).
- `BLEEPHUB_GIT_DIR` — store git repos on the local filesystem (default: in-memory).
- `BLEEPHUB_GIT_BUCKET` + `BLEEPHUB_GIT_PREFIX` — store git repos in an object store (see **Object store** below). Setting this and `BLEEPHUB_GIT_DIR` together is an error: repositories live in one place, and the server does not choose between two. How bleephub drives real git over each of these backends — in-memory, filesystem, and object store — through go-git and its own object-store storage engine is described in [docs/git-storage.md](docs/git-storage.md).
- `BLEEPHUB_OBJECT_BUCKET` + `BLEEPHUB_OBJECT_PREFIX` — store Actions artifacts, caches, runner logs, release assets, package files, container-registry blobs, CodeQL archives/query-packs, attestation bundles and Git LFS objects in an object store (the "byte store").
- `BLEEPHUB_PAGES_JEKYLL_EXECUTABLE` — the Pages build binary (default `bleephub-pages-jekyll`).

Database persistence **requires** durable git storage and `BLEEPHUB_OBJECT_BUCKET`; reloading metadata against in-memory storage would resurrect every repo empty, so those combinations are startup errors, never a silent degraded mode.

**Object store** (unset by default; nothing here has a default, and nothing is inferred)

| Variable | Meaning |
|---|---|
| `BLEEPHUB_OBJECT_STORE` | `s3`, `gcs` or `azure`: the kind of object store. **Required** whenever a bucket below is set; setting it with no bucket is an error, since it names a store nothing is kept in. It is never detected from an endpoint. |
| `BLEEPHUB_GIT_BUCKET`, `BLEEPHUB_GIT_PREFIX` | Where git repositories live. |
| `BLEEPHUB_OBJECT_BUCKET`, `BLEEPHUB_OBJECT_PREFIX` | Where the byte store lives. |

A "bucket" is a container on Azure. A prefix is **required** with its bucket: the two stores may share a bucket, so where each lives in it is stated, and neither prefix may be inside the other. The bucket (or container) must already exist.

How the store is reached is named after the driver. One endpoint per driver serves both stores, and **only the chosen driver's settings may be set** — `BLEEPHUB_AZURE_KEY` beside `BLEEPHUB_OBJECT_STORE=s3` is an error naming it, because a deployment's configuration must say one thing.

| Driver | Variable | Meaning |
|---|---|---|
| `s3` | `BLEEPHUB_S3_REGION` | **Required.** The region to sign for; `AWS_REGION` is not read. S3-compatible servers (MinIO, R2, SeaweedFS, Ceph, Tigris) generally expect `us-east-1`. |
| `s3` | `BLEEPHUB_S3_ENDPOINT` | The URL of an S3-compatible server, addressed path-style. Unset **means** AWS S3 itself in `BLEEPHUB_S3_REGION` — that is what unset says, not a guess. Credentials are the `AWS_*` environment (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`). Do not point it at Google Cloud Storage's S3-compatible endpoint, which ignores conditional writes; use `gcs`. |
| `azure` | `BLEEPHUB_AZURE_ENDPOINT` | **Required.** The blob service URL, `https://<account>.blob.core.windows.net` (Azurite: `http://127.0.0.1:10000/devstoreaccount1`). |
| `azure` | `BLEEPHUB_AZURE_ACCOUNT`, `BLEEPHUB_AZURE_KEY` | **Required.** The storage account's name and shared key, which authorizes every request and signs download URLs. |
| `gcs` | `BLEEPHUB_GCS_ENDPOINT` | **Required.** `https://storage.googleapis.com`, or an emulator's URL. |
| `gcs` | `BLEEPHUB_GCS_CREDENTIALS_FILE` | **Required.** Path to a service-account key file — the one Google credential that can sign a download URL locally. Application Default Credentials, workload identity and the metadata server are not looked for. |

All of it is validated together at startup: every problem — a missing setting, another driver's setting, an unknown driver, a storage tunable that cannot be read — is reported in one error, and the server does not start. Then each configured store is probed for the guarantees bleephub is built on — conditional writes above all — by writing and deleting one key under `<prefix>/.conformance/`; a store that fails the probe is one bleephub refuses to start on. The storage tunables (`BLEEPHUB_GITSTORE_*`, `BLEEPHUB_OBJECT_STORE_BREAKER_*`) mean the same for every driver; see [docs/scaling.md](docs/scaling.md).

> **Upgrade note — this configuration is a breaking change for every deployment that uses an object store.** The old names are not read. A server given only them refuses to start if `BLEEPHUB_S3_ENDPOINT` or `BLEEPHUB_S3_REGION` is among them (a driver's setting with no `BLEEPHUB_OBJECT_STORE`) or if `BLEEPHUB_PERSIST=true` (no durable storage); otherwise it starts with no object store at all. Rename:
>
> | Old | New |
> |---|---|
> | `BLEEPHUB_S3_BUCKET` | `BLEEPHUB_GIT_BUCKET` |
> | `BLEEPHUB_S3_PREFIX` | `BLEEPHUB_GIT_PREFIX` |
> | `BLEEPHUB_OBJECT_S3_BUCKET` | `BLEEPHUB_OBJECT_BUCKET` |
> | `BLEEPHUB_OBJECT_S3_PREFIX` | `BLEEPHUB_OBJECT_PREFIX` — previously defaulted to `objects`; state it |
> | `BLEEPHUB_OBJECT_S3_ENDPOINT` | gone: `BLEEPHUB_S3_ENDPOINT` serves both stores |
> | `BLEEPHUB_S3_BREAKER_THRESHOLD`, `BLEEPHUB_S3_BREAKER_COOLDOWN_MS` | `BLEEPHUB_OBJECT_STORE_BREAKER_THRESHOLD`, `BLEEPHUB_OBJECT_STORE_BREAKER_COOLDOWN_MS` |
>
> `BLEEPHUB_S3_ENDPOINT` and `BLEEPHUB_S3_REGION` keep their names. Two things are newly required: `BLEEPHUB_OBJECT_STORE=s3` (the driver is stated, never assumed), and explicit values where there used to be defaults — a prefix for each bucket (a git store that ran with no `BLEEPHUB_S3_PREFIX` kept its repositories at the top of the bucket; give it a bucket or a prefix of its own and move them) and `BLEEPHUB_S3_REGION`, which no longer falls back to `AWS_REGION` or `us-east-1`.

> **Upgrade note — repositories in an object store need a manifest; run `bleephub adopt` once.** A repository's references, and which of its packs are live, are now one object, `<prefix>/<owner>/<repo>/manifest`, swapped by conditional write; they used to be an object per reference under `refs/` (with `HEAD` and `packed-refs`) and `.superseded` marker objects. The server does not read that layout. A server started on a bucket written that way **refuses to start**, naming the first repository without a manifest, rather than guess — taking such a repository for an empty one would initialize over its branches. Before starting the new server, with the old one stopped and the same `BLEEPHUB_*` storage settings in the environment:
>
> ```sh
> bleephub adopt                          # every repository under BLEEPHUB_GIT_PREFIX
> bleephub adopt -repository owner/repo   # or one
> ```
>
> For each repository (and each submodule repository under it) it reads the old layout — live packs are those with a `.pack` and an `.idx` and no `.superseded` marker; superseded packs are entered as retired at their marker's time and deleted an hour after it, as they would have been; references come from `refs/**`, `HEAD` and `packed-refs` with git's precedence — writes the manifest on the condition that there is none, and prints what it wrote. A repository that already has a manifest is refused and reported, so the command is safe to run again. It moves no pack and deletes nothing: the old reference objects stay where they are, unread. Once the new server is serving, `bleephub adopt -remove-old-layout` deletes them, and the markers, from repositories that have a manifest. The durable SQL lock the server took around reference updates in an object store is gone with the old layout; the object store's conditional write is the only arbiter. See [docs/git-storage.md](docs/git-storage.md#upgrading-a-store-written-before-the-manifest).

**Git over SSH** (unset by default; the transport does not start without the first two)

- `BLEEPHUB_SSH_ADDR` — listen address.
- `BLEEPHUB_SSH_HOST_KEY` — host private key in PEM; **required** when `BLEEPHUB_SSH_ADDR` is set (startup fails loudly if empty rather than regenerating an identity each restart).
- `BLEEPHUB_SSH_HOST` — host (optionally `host:port`) advertised in `ssh_url`.

**SSO** (unset by default) — see [docs/oidc.md](docs/oidc.md)

- `BLEEPHUB_GITHUB_OAUTH_CLIENT_ID` / `BLEEPHUB_GITHUB_OAUTH_CLIENT_SECRET` — sign in with a real github.com OAuth app.
- `BLEEPHUB_SHAUTH_ISSUER` / `BLEEPHUB_SHAUTH_CLIENT_ID` / `BLEEPHUB_SHAUTH_CLIENT_SECRET` / `BLEEPHUB_SHAUTH_POST_LOGOUT_URL` — sign in through a shauth OpenID Connect provider. `BLEEPHUB_ALLOW_INSECURE_OIDC=true` permits an `http://` issuer for loopback tests only.
- `BLEEPHUB_SAML_IDP_SSO_URL` / `BLEEPHUB_SAML_IDP_ENTITY_ID` / `BLEEPHUB_SAML_IDP_CERTIFICATE` — sign in through a SAML 2.0 identity provider (the three configure together). `BLEEPHUB_SAML_SP_ENTITY_ID` overrides the service-provider entityID, which otherwise defaults to the instance origin.

**Seeding** — `BLEEPHUB_SEED_APPS_FILE` (path to a JSON file of GitHub App seed specs) and `BLEEPHUB_SEED_APPS` (the same JSON inline); both may be set and concatenate, invalid JSON is a startup error.

**Storage quorum** (dqlite only) — `BLEEPHUB_DQLITE_SECRET` is **required** on the server and every node; it authenticates the connection upgrade and derives the private-cluster TLS identity. Both ends refuse to start without it; Terraform supplies it from Secrets Manager.

The SSH gateway binary (`ssh-gateway/`) requires `BLEEPHUB_WAKE_URL` and `BLEEPHUB_INTERNAL_SSH_TARGET`. Test-only variables (`BLEEPHUB_STRESS_*`, `BLEEPHUB_LOAD_*`, `BLEEPHUB_DQLITE_SERVERS`, `SOCKERLESS_REPOSITORY`) are read by the suite, never the server. The wake Lambda's variables are documented in [terraform/README.md](terraform/README.md).

## Container images

Every merge to `main` publishes immutable twelve-character commit-SHA tags to GitHub Container Registry. Each generic tag is a multi-architecture manifest; direct native manifests are suffixed `-amd64` and `-arm64`.

| Image | Multi-architecture manifest | Direct native manifests |
|---|---|---|
| Server | `ghcr.io/e6qu/bleephub:<tag>` | `ghcr.io/e6qu/bleephub:<tag>-amd64`, `…-arm64` |
| GitHub Actions runner | `ghcr.io/e6qu/bleephub-runner:<tag>` | `ghcr.io/e6qu/bleephub-runner:<tag>-amd64`, `…-arm64` |

The runner image packages the official GitHub Actions runner, configures itself from a registration URL and token, then starts the runner:

```bash
docker run --rm \
  -e RUNNER_URL=https://bleephub.example/owner/repository \
  -e RUNNER_TOKEN=<registration-token> \
  ghcr.io/e6qu/bleephub-runner:<tag>
```

`RUNNER_NAME`, `RUNNER_LABELS`, `RUNNER_GROUP`, `RUNNER_WORKDIR`, and `RUNNER_EPHEMERAL` optionally refine the registration. The newest 20 releases of each package are retained; no mutable `latest` or `main` tag is published.

## Integration tests

```bash
make test
make gh-test
SHAUTH_SOURCE_DIR=../shauth make shauth-sso-test
```

`make test` runs the Go unit tests; `make gh-test` runs the real `gh` CLI inside Docker (Bleephub + `gh` + self-signed TLS); `make shauth-sso-test` runs the two-relying-party SSO and global-logout contract. The `gh` harness (`Dockerfile.gh-test`, `test/run-gh-test.sh`) exercises `gh auth login`, native repo/issue verbs over both REST and GraphQL paths, `gh secret set` (real sealed-box encryption), `gh variable`/`gh workflow` verbs, and the parity probes for endpoints with no native `gh` verb. It runs in CI as the Bleephub gh CLI job and must be green to merge.

Two hermetic unit-test gates validate Bleephub against the vendored GitHub OpenAPI description (`third_party/github-openapi.json.gz`): a **route-definition** gate (`gh_api_definition_test.go`) requires every registered `/api/v3` route to be documented in an official description or carried in a shrink-only ledger, and a **response-shape ratchet** (`openapi_shape_validator_test.go`) validates every 2xx `/api/v3` JSON response member-by-member against the documented schema, gated by `internal/server/openapi-violation-allowlist.txt`. The description is pinned in `third_party/github-openapi.VERSION` and checked by `TestVendoredOpenAPIMatchesRecordedPin`.

## Source layout (~180 Go files)

| Group | Files | Purpose |
|---|---|---|
| Core protocol | `server.go`, `auth.go`, `agents.go`, `broker.go`, `run_service.go`, `timeline.go` | Runner registration, job delivery, lifecycle |
| Jobs & workflows | `jobs.go`, `workflow*.go`, `matrix.go`, `secrets.go`, `expressions.go`, `actions.go`, `artifacts.go` | Multi-job, matrix, secrets, expressions, artifacts |
| GitHub REST core | `gh_rest.go`, `gh_repos_*.go`, `gh_orgs_*.go`, `gh_issues_*.go`, `gh_pulls_*.go`, `gh_teams_rest.go`, `gh_labels_rest.go`, `gh_members_rest.go` | Repos, orgs, issues, pull requests, teams, labels, milestones |
| GitHub Apps + OAuth | `gh_apps_*.go`, `gh_oauth.go`, `gh_app_hooks_rest.go`, `gh_apps_perms.go` | JWT auth, installations, OAuth Apps, token prefixes, permission enforcement |
| Reactions/Releases/Deployments | `gh_reactions.go`, `gh_releases.go`, `gh_deployments.go`, `gh_pr_comments.go` | Reactions, releases, deployments + environments, review comments/threads |
| Actions + Checks | `gh_actions_*.go`, `gh_workflows_rest.go`, `gh_checks_*.go` | Runs/jobs/steps, dispatch, logs, check-runs/suites |
| GraphQL | `gh_graphql.go`, `gh_*_graphql.go`, `gh_request_decode.go` | Schema + flex decoders |
| Webhooks | `webhooks*.go`, `gh_hooks_rest.go` | HMAC-SHA256/SHA1 delivery with retry |
| Git | `git_http.go`, `git_receivepack.go`; the [`gitstore`](gitstore/README.md) module | Smart HTTP (go-git); in-memory / on-disk / S3 storage, as a separately importable library |
| Persistence + infra | `persistence.go`, `store*.go`, `rbac.go`, `metrics.go`, `otel.go`, `ui_embed.go` | SQLite layer, state, RBAC, metrics, OTel, dashboard |

## See also

- [specs/BLEEPHUB_GITHUB_API_PARITY.md](specs/BLEEPHUB_GITHUB_API_PARITY.md) — per-endpoint parity audit + acceptance criteria.
- [docs/BLEEPHUB_GH_CLI.md](docs/BLEEPHUB_GH_CLI.md) — the full `gh` CLI walkthrough.
- [docs/oidc.md](docs/oidc.md) — SSO integration: OpenID Connect (any compliant provider, or shauth) and SAML 2.0.
- [docs/git-storage.md](docs/git-storage.md) — how bleephub drives real git in-process over memory, filesystem, and an object store through go-git and the `gitstore` storage engine.
- [gitstore/README.md](gitstore/README.md) — the git-on-object-storage library on its own: API, options, and [`gitstore/bench`](gitstore/bench/README.md), the harness that compares it with other implementations (`make bench-gitstore`).
- [docs/scaling.md](docs/scaling.md) — the scaling limits, the fuzz/benchmark/ramp suite, and the tuning knobs (`make bench` / `make scale` / `make fuzz`).
- [docs/private-api.md](docs/private-api.md) — bleephub's own non-GitHub management and data-plane routes (`/ui-data`, `/manage`, `/internal`, `/_apis`).
- [BUGS.md](BUGS.md) — audited non-defects (false-positive findings); the fixed-defect history is in git.

## Releasing

Push a semver tag; everything else is automatic. See [RELEASING.md](RELEASING.md).

## Licence

GNU Affero General Public License v3.0 or later — see [LICENSE](LICENSE).

Because the server is normally reached over a network, AGPL section 13 applies:
anyone you let use a modified instance is entitled to that instance's source.

Third-party material redistributed inside the published images is inventoried in
[THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md); all of it (MIT, CC-BY 4.0) is
one-way compatible with AGPLv3, so inbound dependencies must stay that way.
`scripts/check-dependency-licenses.py` enforces it in CI for every Go module
linked into what ships: a licence that cannot be combined into an
AGPL-3.0-or-later work — GPL-2.0-only, the source-available licences — or that
the gate cannot identify, fails the build. A dependency that fails is replaced,
by another library or by one written here as a library of its own at the top of
this repository, as `gitstore/` is.

## Prior art

[ChristopherHX/runner.server](https://github.com/ChristopherHX/runner.server) (C#, 25 controllers) proved this approach works. bleephub is a from-scratch Go implementation informed by studying the runner source + runner.server's protocol handling, but shares no code with either.

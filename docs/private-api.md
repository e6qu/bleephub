# bleephub private APIs (non-GitHub surfaces)

Alongside the GitHub-compatible REST (`/api/v3`) and GraphQL (`/api/graphql`)
surfaces, bleephub serves several route families that are **bleephub-specific**
and have no GitHub equivalent. They fall into two groups: internal control and
data planes (`/ui-data`, `/manage`, `/internal`) and the Actions runner protocol
(`/_apis`, `/twirp`). An unmodified GitHub client — `gh`, `go-github`, Octokit,
Probot — never calls any of them; they exist for the web UI, the management
console, operators, and the `actions/runner` binary.

Keeping these off `/api/v3` is a hard invariant: `/api/v3` must contain only
routes real GitHub documents (an OpenAPI gate enforces it), so anything
bleephub-only lives under one of the prefixes below.

Route counts below are the distinct registered routes at the time of writing;
regenerate them with `grep -rhoE 's\.route\("[A-Z]+ /<prefix>' internal/server/*.go | grep -v _test | sort -u`.

## `/ui-data/` — web UI data plane (~107 routes)

**For:** the embedded React dashboard at `/ui/`. These endpoints back UI screens
that GitHub either exposes only through its own web app or does not expose over
REST at all (account security settings, blame, discussion pinning, classic-PAT
creation, and similar), so they are served in a REST-style shape under
`/ui-data/` instead of being invented under `/api/v3/`.

**Auth:** auto-authenticated as the signed-in browser session (the same session
cookie the UI holds); there is no separate token to present. The delivery layer
treats `/ui-data/` like `/api/` for request handling but scopes it to the viewer.

**Callers:** the bleephub UI only. Not part of the GitHub-compatible contract and
not covered by the parity spec.

## `/manage/v1/` — GHES management-console API (~19 routes)

**For:** operational configuration modelled on the GitHub Enterprise Server
management console — version, cluster/replication status, license read/apply,
settings, maintenance mode, config apply (with an SSE event stream), SSH access
keys, and system-requirement checks.

**Auth:** HTTP Basic with username `api_key` and the configured management
password (`requireGHESManagementAuth`, constant-time compared). Callers get a
`WWW-Authenticate: Basic realm="GitHub Enterprise Management Console"` challenge
on failure.

**Callers:** operators and provisioning automation. Mirrors GHES's
`/manage/v1/*` shape but is a bleephub surface, not the GitHub REST API.

## `/internal/` — operator seed and admin endpoints (~11 routes)

**For:** operator-only actions with no GitHub API equivalent: submitting and
inspecting exec jobs/workflows (`/internal/exec/*`), creating organizations and
org network settings, status/storage introspection, job summaries, and the
Prometheus metrics endpoint (`/internal/metrics`).

**Auth:** gated by `internalAuthMiddleware` for the whole prefix — requires a
valid token **or** browser session resolving to a **site admin**; anyone else
gets `401`/`403`. The prefix-wide check is deliberate: `/internal/exec/*`
dispatches containers to the runner fleet, so leaving any route un-gated would be
arbitrary code execution.

**Callers:** operators and the local-dev tooling. `POST /internal/exec/submit` is
how the "How it works" job-submission path in the README feeds the runner.

## `/_apis/` and `/twirp/` — Actions runner protocol (~38 + 4 routes)

**For:** the wire protocol the official `actions/runner` speaks — this is a
GitHub Enterprise Server *internal* service surface, not the GitHub REST API.
`/_apis/` covers token exchange (`/_apis/v1/auth`), service discovery
(`/_apis/connectionData`), agent/pool registration, the broker message
long-poll, run acquire/complete, timeline and log upload, action tarball
download, and the artifact cache. `/twirp/` carries the newer Twirp-encoded
Actions Results artifact service (create/finalize/list/get-signed-URL).

**Auth:** the runner's own credentials — the unsigned (`alg: none`) job JWT
issued by the token service and presented on every subsequent `/_apis/` and
`/twirp/` call. Only `/_apis/connectionData` and the auth endpoint are reachable
before a token exists.

**Callers:** the `actions/runner` binary (and `actions/checkout` for the tarball
paths). No GitHub REST client touches these.

## Miscellaneous well-known and probe endpoints

| Route | For | Auth |
|---|---|---|
| `GET /.well-known/openid-configuration`, `GET /.well-known/jwks` (`/jwks.json`) | Actions OIDC token trust — lets a cloud identity provider verify bleephub-issued workload tokens. | Public. |
| `GET /health` | Liveness probe. | Open — stays reachable with no credentials. |
| `GET /internal/metrics` | Prometheus metrics. | Site admin (under the `/internal/` gate). |
| `GET /monitoring/observation` | Fixed-cardinality process/persistence/workflow/runner/job evidence (`e6qu.monitoring/v2`). | `BLEEPHUB_MONITORING_TOKEN` bearer; refuses every caller when unset. |

## See also

- [../README.md](../README.md) — configuration and the environment variables that gate these surfaces.
- [BLEEPHUB_GH_CLI.md](BLEEPHUB_GH_CLI.md) — the GitHub-compatible `gh` path (by contrast, everything here is bleephub-only).
- [../specs/BLEEPHUB_GITHUB_API_PARITY.md](../specs/BLEEPHUB_GITHUB_API_PARITY.md) — the GitHub `/api/v3` + GraphQL parity audit, which deliberately excludes these private surfaces.

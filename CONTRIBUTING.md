# Contributing

Bleephub reimplements GitHub's server-side REST + GraphQL surface. Most changes
either move a parity finding in `BUGS.md` from `open` to `fixed` or add coverage
for a surface that CI already gates. This document describes the workflow those
changes go through.

## Building

The server embeds the built web UI, so a full binary needs both halves:

```bash
cd web && bun install --frozen-lockfile && bun run build   # → web/dist/
cd ..  && make build                                        # → ./bleephub-server (embeds dist/)
```

`make build` compiles with `CGO_ENABLED=0 GOWORK=off`. To type-check the whole
Go tree without producing the binary — the fastest inner loop — use:

```bash
GOWORK=off go build ./...
```

`GOWORK=off` is deliberate: the repository contains several nested modules
(`sdk-tests`, `terraform/wake`, `test/terraform-sockerless`) and building with a
workspace active pulls them in unintentionally.

For UI work, `cd web && bun run dev` serves Vite with HMR on `:5173`; rerun
`make build` to refresh the embedded copy the server ships.

## Running the tests

```bash
make test        # GOWORK=off go test -tags noui -count=1 ./... (root module)
```

The nested modules are separate `go test` targets and do not run from the root
module; CI builds and vets each of them, and runs the `sdk-tests` go-github
conformance suite and the `terraform/wake` controller tests.

## CI gates a pull request must satisfy

CI (`.github/workflows/ci.yml`) must be green before merge. The gates are:

- **Formatting** — `gofmt -l .` must report nothing (the `web/` tree is exempt).
- **Vet** — `go vet ./...` and `go vet -tags noui ./...` (both build tags).
- **Static analysis** — `staticcheck` and `gosec` in every Go module, plus
  `govulncheck` for reachable vulnerabilities.
- **Dead code and clones** — `scripts/deadcode.sh` (unreachable functions) and
  `scripts/dupl.sh` (copy-pasted Go); `scripts/jscpd.sh` for TypeScript.
- **Full race suite** — `go test -race -tags noui ./...` with a coverage floor
  enforced by `scripts/check-go-coverage.sh`, plus a fuzz burst over every
  discovered target (`scripts/fuzz.sh`).
- **Findings ledger** — `scripts/check-bugs-ledger.sh` validates `BUGS.md`, and
  `scripts/parity_inventory.py --check` regenerates the parity inventory and
  fails on any drift between the ledger, the routes, and the recorded counts.
- **Drift checks** — the vendored GitHub OpenAPI description and GraphQL schema
  must match upstream (`scripts/check-github-openapi-drift.sh`,
  `scripts/check-github-graphql-schema-drift.sh`), and tests must not introduce
  new wall-clock dependencies (`scripts/check-test-clock-dependencies.py`).
- **Web** — `bun run typecheck`, `bun run test:coverage`, `bun run build`,
  `scripts/knip.sh` (dead TypeScript exports), and `bun audit`.
- **Workflows, shell, and supply chain** — `actionlint`, `shellcheck`,
  `zizmor`, `semgrep`, and `trivy`; a dependency-age quarantine window across
  every ecosystem.
- **Compatibility suites** — `make gh-test` (the official `gh` CLI against a
  live instance), the browser end-to-end suite, the Shauth SSO contract, the
  release images, and the Terraform module.

Documentation-only changes (files under `docs/`, and `README.md`, `RELEASING.md`,
`SECURITY.md`, `PLAN.md`, `THIRD-PARTY-NOTICES.md`, `LICENSE`) skip the pipeline
by design. Any change to code, config, specs, workflows, or `BUGS.md` runs
everything.

## The `BUGS.md` findings ledger

`BUGS.md` is the human-editable defect ledger and the source of truth for parity
findings. Each row carries an ID, a severity (`B` blocker, `M` major, `m`
minor), a location, a one-sentence claim, and a status (`open`, `partial`,
`fixed`, or `deferred`). Its totals and status vocabulary are checked by the
same parser that regenerates the parity inventory, so a row that loses a column
or reuses an ID fails CI.

By convention the ledger IDs never appear in source or comments — the reasoning
behind a fix belongs in its commit message, not next to the code.

## Pull requests

- Keep the commit message carrying the *why*: the reasoning behind a change is
  recovered from history, not from a separate changelog.
- When a change closes a `BUGS.md` finding, update its row to `fixed` in the
  same pull request.
- Do not grow a parity allowlist (for example the OpenAPI violation allowlist)
  without saying why — a wider allowlist is a parity regression.

## Documentation and terminology

Prose in `docs/`, `README.md`, and this file favours plain language, but common
technical acronyms are used as-is. Spelling them out in full is noise, not
clarity — write "GitHub's REST API", never "GitHub's Representational State
Transfer Application Programming Interface".

The following are established terms and are **allowed acronyms**: they should
never be expanded, and a docs review should not flag them.

> API, REST, GraphQL, gRPC, CLI, UI, SPA, SDK, CI, CD, SSO, OIDC, OAuth, JWT,
> JWKS, PKCE, SAML, SCIM, PAT, RBAC, ACL, CSRF, XSS, SSRF, TLS, mTLS, HTTP,
> HTTPS, URL, URI, DNS, IP, TCP, UDP, CIDR, NAT, VPC, SG, S3, KMS, ECS, EFS,
> ECR, IAM, AWS, GCP, OTEL, SHA, HMAC, KDF, ID, UUID, JSON, YAML, HCL, HTML,
> CSS, TS, DOM, CRUD, TTL, TOCTOU, FD, GC, DTO, ORM, DB, SQL, gofmt, gh.

When introducing a bleephub-specific or genuinely uncommon abbreviation, expand
it on first use; everything above is common enough that expansion only hurts.

## Security

Do not open a public issue for anything that discloses or bypasses a credential.
See [`SECURITY.md`](SECURITY.md) for private reporting.

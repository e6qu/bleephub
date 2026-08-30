# Bleephub ↔ GitHub parity audit

Status: **active generated parity ratchet**.

## Goal

Every Bleephub client surface should behave like GitHub or GitHub Enterprise Server after changing coordinates alone:

- REST: `http(s)://<host>/api/v3/...`
- GraphQL: `http(s)://<host>/api/graphql`
- OAuth and GitHub App browser flows: GitHub-shaped `/login/...`, `/settings/...`, and installation paths
- Git smart HTTP: `http(s)://<host>/<owner>/<repo>.git`
- GitHub Actions runner protocol: `http(s)://<host>/_apis/...`
- Web UI: GitHub-shaped information architecture backed by the same public APIs and durable state

The GitHub Enterprise Server coordinates are intentional: official clients already swap their base URL this way.

## How parity is measured and gated

### Executable inventory

[`parity-inventory.json`](parity-inventory.json) is the reviewable, machine-readable snapshot of the implementation and its known defects. It contains:

- all 1,216 operations and documented statuses in the vendored GitHub REST description;
- all 1,219 routes produced by the real `/api/v3` runtime registration table, with source locations (or an explicit programmatic-registration marker);
- every routed UI component and the tests that exercise it;
- the pinned official GraphQL schema, canonical Bleephub introspection digest, compatibility-gap count, resolver files, and test files;
- literal GitHub Actions event producers plus the scheduled-workflow implementation and test; and
- every ledger row, normalized category, severity, and status.

`scripts/parity_inventory.py --check` regenerates the snapshot in memory and fails CI on any drift. `BUGS.md` remains the human-editable source for findings; the same parser checks its totals and status vocabulary. A new route, UI page, resolver, event producer, OpenAPI update, or ledger edit therefore cannot pass with an outdated coverage claim.

[`rest-semantic-contracts.json`](rest-semantic-contracts.json) is the companion per-operation semantic matrix. For all 1,216 official operations it records path/query/header parameters and constraints, required request-body fields by media type, documented success and error statuses, security alternatives and scopes, pagination inputs, conditional-request declarations, and mutation obligations for reload persistence, event emission, and failure atomicity (currently 342 body-bearing operations, 277 required bodies, 283 paginated, 582 state-changing). It deliberately distinguishes a described obligation from behavioural evidence — e.g. the current OpenAPI pin declares no conditional-request headers, so it records an explicit zero rather than a false green. Runtime status/shape observation, authorization matrices, pagination/Link tests, reload tests, event tests, and injected storage-failure tests remain the evidence layers named in the matrix's `coverage_boundary`.

### Ratchets

- **REST routes/shapes.** The registered `/api/v3` surface covers the vendored GitHub REST description plus documented GHES-only operations. `TestRegisteredAPIv3RoutesExistInGitHubSpec` rejects invented GitHub-namespace routes, and the runtime OpenAPI observer rejects response member/type drift. The vendored description lives at `third_party/github-openapi.json.gz`, refreshed by `scripts/update-github-openapi.sh`. GitHub's REST API is versioned, so this is a ratchet, not a one-time completeness claim: it proves route legitimacy and the response shapes the tests exercise, not every status, validation branch, permission combination, pagination edge, webhook, or storage failure.
- **GraphQL schema.** The official public schema from `docs.github.com` is digest-pinned and checked for upstream drift; a full authenticated introspection-equivalent snapshot of Bleephub is compared structurally against it (type kinds, fields, arguments, input fields, enum values, interfaces, possible types, nested null/list signatures). The compatibility-gap set is allowlisted and changes only through review, and a canonical introspection snapshot rejects accidental drift.

## Current proof boundary

### Real state and byte planes

Repository identity and metadata live in persisted state; git refs, commits, trees, tags, contents, comparisons, archives, pushes, and Pages branch builds read real git storage. Actions artifacts, caches, logs, Pages publications, releases, package files, GitHub Container Registry blobs, CodeQL databases/query packs, and attestation bundles use object storage in persisted mode. Releases resolve or create real tag refs and emit lifecycle webhooks and Actions events; Pages runs the pinned Jekyll runtime when required. Cloud-backed Codespaces and runner execution fail loudly when a required runtime or storage operation fails.

### Authentication, Apps, and events

The implementation and tests cover:

- GitHub App installations with `repository_selection: all|selected` and selected-repository token downscoping;
- installation and installation-repositories lifecycle events;
- webhook `installation` blocks plus `X-GitHub-Hook-ID`, target-type, target-ID, event, delivery, SHA-1, and SHA-256 headers;
- App hook configuration, delivery listing/detail, and redelivery;
- OAuth web/device flows and token-management prefixes;
- GitHub App installation/user tokens, suspension, repository selection, and organization-repository authorization.

### UI organization and themes

The application shell uses GitHub's global navigation model: full-width repository context chrome, the familiar primary tab order, real Watch/Fork/Star actions, a separate content toolbar, and an administrative overflow/settings group. Browser mutations use GitHub's public repository APIs; a read-only `/ui-data` adapter normalizes expected `404` viewer-state checks so ordinary rendering emits no console resource errors. Both light and dark themes are browser-asserted; they retain GitHub/Primer surface/semantic tokens plus a more saturated brand layer. Primer remains the reference for contrast and theme separation: <https://primer.style/product/primitives/>.

### Retained GitHub Classroom product

Classroom is retained as an authenticated browser product while preserving GitHub's six read-only Classroom REST endpoints for the official `go-github` client and Classroom extension. Assignment acceptance generates an organization-owned repository from the real starter git tree, grants access, creates the Feedback pull request, installs a real Actions workflow, and records its baseline commit; submission/commit counts and points derive from repository history and completed autograding jobs. The obsolete `/internal/classrooms...` seed routes no longer exist.

## What is truly left

Several onboarding surfaces are now GitHub-user- or producer-shaped rather than operator-gated: Marketplace listings/plans/subscriptions (owners publish through authenticated settings; the obsolete `/internal/marketplace/purchases` route is gone), fine-grained PAT creation through account settings, and CodeQL database upload through the official CodeQL Action's uploads host. The remaining operator-shaped ingress:

| Domain | Current ingress | Required completion path |
|---|---|---|
| Hosted-compute network settings | `/internal/orgs/.../network-settings` | GitHub/Azure private-network onboarding workflow that provisions the settings resource before public configuration APIs reference it |

The runner execution controller (`/internal/exec/...`) and operator diagnostics (`/internal/{metrics,status,storage}`) are not GitHub API gaps — they are Bleephub control-plane interfaces, and user-facing UI reads public GitHub/health routes instead.

### GraphQL schema coverage is quantified

The generated coverage report is intentionally blunt: the current official schema has 1,806 types and 8,341 object/input fields; Bleephub exposes 276 types, shares 210 official types, implements 782 official fields, and matches 546 field signatures exactly, with 319 compatibility gaps, 1,596 missing types, and 1,208 missing fields on shared types. Official-client query tests prove the consumer subset behaves, while [`graphql-schema-coverage.json`](graphql-schema-coverage.json) keeps that subset from being mistaken for full GitHub GraphQL coverage.

### REST semantic coverage is observed, not exhaustive

The route/response observer cannot prove unexecuted branches. Remaining audit work should prioritize:

1. installation-token permission matrices on every write family;
2. conditional requests, redirects, pagination/link headers, API-version headers, and rate-limit behavior;
3. webhook/action emission for every mutating resource transition;
4. delete/rename/transfer cascades for every newly persisted repository-owned record;
5. object-store and git-store failure atomicity on every byte-owning operation;
6. official `gh`, `go-github`, Git, package/registry, runner, and Terraform-adjacent consumers rather than hand-built request-only coverage.

### UI page-by-page parity remains broader than the shared shell

The shared chrome and repository Code experience are close to GitHub and theme-complete, but long-tail pages still need page-level comparison and workflow coverage — highest-value next slices:

1. repository Settings and the remaining Secret scanning/Dependabot security subpages;
2. issue and pull-request timelines, review controls, and file-diff interactions;
3. Actions workflow/run/job log organization and live updates;
4. organization profile, people, teams, rulesets, audit, and settings hierarchy;
5. account settings, fine-grained token creation, Apps/OAuth management, and installation flows;
6. responsive/mobile navigation, keyboard behavior, focus management, and color-contrast audits;
7. visual regression baselines for both light and dark modes across all routed pages.

Live deployment validation is an infrastructure concern, not a Bleephub API signature defect, and stays separate.

## Acceptance criteria for future parity work

1. A newly served `/api/v3` route matches the official GitHub REST description or a cited GHES contract.
2. Positive, permission-denied/not-found, validation, pagination, and persistence-reload paths are tested where applicable.
3. User-facing workflows use GitHub-shaped public/browser paths rather than `/internal/*` setup.
4. Durable metadata stays in SQLite; git and service bytes stay in git/object storage.
5. Mutations emit the webhooks and GitHub Actions events produced by the equivalent GitHub transition.
6. UI mutations use public GitHub APIs, render errors visibly, and produce no browser console errors.
7. Light and dark themes are both asserted for shared visual changes.
8. Official clients are preferred wherever an official client surface exists.

Per-operation history lives in `git log` and pull requests; this document keeps only the current proof boundary and the remaining gaps.

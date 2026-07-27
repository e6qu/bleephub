# Bleephub remediation plan

A full-surface audit of backend, UI, testing, CI, release preparedness and the
deployment model produced 512 findings, 120 of them blockers. `BUGS.md` is the
ledger. This file is the execution plan.

## How this is sequenced

Phases are ordered so each one leaves the tree building with tests passing, and
so that structural fixes land before the instances they subsume. Where a class
of defect has a mechanical detector, the detector lands with the fix and becomes
a permanent gate — the point is that the class cannot regrow, not that today's
instances are patched.

Three detectors do most of the work:

| Detector | Kills |
|---|---|
| Route-inventory test over `RegisteredRoutes()` | Every unauthenticated/unauthorized route at once, including `/_apis/`, `/internal/oauth/state`, the 25 ungated REST files, and anonymous private-repo reads |
| Declared-but-never-read test | ~40 silent no-ops: GraphQL arguments, workflow keys, request-body fields decoded and dropped |
| Allowlist liveness + citation tests | Collapses the OpenAPI violation allowlist from 60 entries toward 1 |

## Phases

### Phase 1 — Authorization becomes real
The root cause. `requirePerm` is a credential-shape gate, not an authorization
gate: for a classic PAT or a browser session it evaluates no predicate and falls
through. Make it total — every branch produces an explicit allow or deny — add
the missing classic-PAT scope arm, and introduce `requireRepoRead` /
`requireRepoPush` / `requireRepoAdmin` applied at registration. Land the
route-inventory test in the same phase so the gap cannot reopen.

### Phase 2 — Runner control plane
`/_apis/` has no authentication and its job messages carry every org, repo and
environment secret in plaintext. Add an agent-auth middleware resolving the
issued bearer token to an agent, bind sessions and job requests to that agent,
and sign runtime tokens (currently `alg:none`, one-year expiry, trusted
unverified).

Groundwork, so this is not re-derived: the token already exists and is already
keyed to the right thing. `handleOAuthToken` (`auth.go:163`) returns
`makeJWT(agent.Authorization.ClientID, "bleephub")`, so the subject identifies
the registering agent. Nothing reads it — grepping `Authorization` across
`broker.go`, `agents.go`, `run_service.go` and `timeline.go` finds only the
struct the server *hands out*, never a header it inspects. So the work is to
verify the token and resolve it to an `*Agent`, not to invent a credential.

Two things make this the phase to be careful with. The token is `alg:none`, so
verification is meaningless until it is signed — sign first, then enforce, or
enforcement is theatre. And the only thing that proves the real `actions/runner`
still works afterwards is the Sockerless CI job, which takes about fifteen
minutes per attempt and cannot run locally without Docker.

### Phase 3 — GraphQL
The endpoint serves anonymous requests; mutations authenticate but never
authorize; `user(login:).repositories` leaks private repositories. Add the
authentication choke point, route every mutation through an authorization
helper, gate repository nodes behind a type only constructible via `canReadRepo`,
and add a query depth/complexity limit.

### Phase 4 — Crash classes
Two `fatal error` paths that kill the process rather than returning 500: a map
write under a read lock, and eight unsynchronized store-map reads. Fix both,
unexport the store maps so the class cannot recur, add panic-recovery
middleware, and fix the confirmed nil-dereferences.

### Phase 5 — Process lifecycle
No signal handling, no graceful shutdown, no goroutine ownership, no request
body limits, no panic recovery, and a TLS misconfiguration that silently serves
plaintext. Add a config choke point, a lifecycle owner, `/livez` + `/readyz`.

### Phase 6 — Persistence atomicity
604 single-row writes against 4 batched ones; memory mutated before the write;
`log.Fatalf` as the error strategy. Add a single batched apply path, persist
before mutating, make ID allocation durable, add a schema version, and quarantine
referentially-broken rows at load instead of refusing to boot.

### Phase 7 — Actions engine
Matrix expansion shares one env map across combinations; the `needs` rewrite is
map-iteration-order dependent; `include:`-only matrices are dropped; workflow
files are read from HEAD rather than the triggering ref; fork PRs never trigger;
an unsynchronized regex cache crashes the process; jobs are lost on runner death.

### Phase 8 — Silent no-ops and validation
Implement or loudly reject every accepted-and-ignored parameter, workflow key and
body field. Add enum validation, pointer fields so absent and zero differ, and
pagination on the ~60 lists that return whole collections.

### Phase 9 — Parity
Ratchet the violation allowlist toward one justified entry, fix the emitters it
papers over, strengthen the validator (status codes, enums, nullability,
coverage floor), and remove the invented routes from the route allowlist.

### Phase 10 — Web
Replace the client-side fleet fan-out polled every three seconds with a server
aggregate, surface mutation errors, give dialogs real semantics, and fix the
blank-screen session probe.

### Phase 11 — Test infrastructure
Per-test isolated servers, `-race`, coverage with a ratchet, real fuzzing, and
wiring the four orphaned test tiers into CI.

### Phase 12 — CI, release, deployment
Static analysis, the unwired quality gates, release workflow, LICENSE and the
absent release-readiness files, supply-chain attestation, and the Terraform
correctness and durability fixes.

## Status

| Phase | State |
|---|---|
| 1 — Authorization | **landed.** `requirePerm` decides on every branch; 25 read endpoints gated; `/internal/` declares site-admin at the route table; route-inventory test in place. Classic-PAT scope enforcement still open — `classicScopeCovers` is incomplete by its own admission and does not cover administration at all, so enabling it would deny far more than it should |
| 2 — Runner control plane | pending — the largest remaining blocker |
| 3 — GraphQL | partial. Endpoint requires authentication, the private-repository leak in `user().repositories` is closed, `deleteRepository` checks admin. The other 22 mutations still authenticate without authorizing |
| 4 — Crash classes | partial. Map-write-under-read-lock fixed, 9 unsynchronized reads converted, panic recovery added. Six further reads sit inside a held lock and need per-site work; the confirmed nil-dereferences are untouched |
| 5 — Lifecycle | partial. TLS pair validation, panic recovery, query redaction, `net/http` error log bridged. Signal handling, graceful shutdown, goroutine ownership and body limits still open |
| 6 — Persistence | pending |
| 7 — Actions engine | partial. Matrix expansion (include-only, `needs` ordering, shared env map), exclude-before-include, `continue-on-error`, job-duration metric. The lease/requeue, trigger-ref and webhook SSRF blockers are open |
| 8 — Silent no-ops | pending |
| 9 — Parity | pending |
| 10 — Web | pending |
| 11 — Test infrastructure | pending |
| 12 — CI, release, deployment | partial. Test timeout, concurrency group, dependabot, CODEOWNERS, SECURITY.md, THIRD-PARTY-NOTICES.md, `.gitignore`. LICENSE needs an owner decision; static analysis and the release workflow are open |

Counts as of the latest commit: **33 fixed, 6 partial, the rest open.** Every
open item still carries its location and claim; nothing was dropped to make the
list shorter.

## Baseline

Recorded before any change, on `9600511`:

```
go build ./...              clean
go vet -tags noui ./...     clean
go test -tags noui ./...    all packages ok, 0 failures, 0 skips
                            internal/server 437.685s against an 8m timeout
```

The 9% timeout headroom is itself a finding — a slower runner turns it into a
CI failure that reads as a hang.

## Not in scope for this branch

`internal/server` is a single flat package: 406 files, 181k lines. Splitting it
into subpackages is a genuine structural improvement and is deliberately **not**
attempted here — it would conflict with every other change in this branch and is
an architectural decision to take on its own. It is recorded in `BUGS.md` so it
is not lost.

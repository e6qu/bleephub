# Bleephub bug ledger

The completed full-surface audit is preserved in git history (the ledger carried
all 994 rows through commit `a4f7e61e`). This ledger records current findings and
standing decisions; new upstream contracts can expose new work after an audit.

4 findings remain: 1 blocker, 3 major, 0 minor. Severity is `B` blocker, `M`
major, `m` minor. Status begins with `open`, `partial`, `fixed`, `deferred`, or
`false-positive`; the generated parity inventory checks these counts.

IDs are for this ledger only — per project convention they never appear in
source or comments.

---

## False positives — audited non-defects

| ID | S | Location | Finding | Status |
|---|---|---|---|---|
| WEB-016 | M | ci.yml:25-39, knip.ts | Four quality scripts unwired, and the knip config structurally cannot report dead code in the core workspace | false-positive — the four web quality gates are now wired in the CI Web job; knip not reporting dead code inside @bleephub/ui-core is by design, because that package is a public component-library surface (operator shell, pages, API client) tested in core/src/__tests__ and core/e2e/backend-app.spec.ts. Owner decision: keep it as a library, do not prune (documented in web/knip.ts). |
| WEB-017 | M | web/core | 8 of ~35 exports consumed; ~1,500 lines built and type-checked every run and invisible to knip | false-positive — the ~1,500 lines are @bleephub/ui-core's reusable operator shell (AppShell/BackendApp/SimulatorApp, pages, API client and hooks), an intentional shared-library surface that Bleephub's SPA does not mount but that is unit-tested in core/src/__tests__ and contract-e2e-specced in core/e2e/backend-app.spec.ts; knip flags them only because it cannot see that out-of-band usage. Owner decision: keep the library. |
| TEST-015 | M | ~1,640 sites | Unchecked type assertions against 97 comma-ok forms; one panic terminates the binary and discards 1,320 other results | false-positive — the stated harm cannot occur: an AST scan finds zero single-value type assertions inside any goroutine body (the only context whose panic escapes recovery to crash the binary), and the 60 production assertions all read values the code just constructed, so converting ~1,780 safe assertions to comma-ok is churn with no safety gain — a fix that should never be made. |

## Open upstream contract changes

| ID | S | Location | Finding | Status |
|---|---|---|---|---|
| PAR-012 | B | scripts/check-github-openapi-drift.sh, third_party/github-openapi.VERSION | Upstream REST description d560905bbf50d79e40d6a006ce2e5834cfd86a31 adds 49 operations beyond the pinned 58b1e0c snapshot, covering agent automations/merge enrollment, Copilot policies/customizations, user issue fields/types, issue artifacts, advisory comments, planning statuses and Dependabot runners, with additional existing-operation changes | open — implement and qualify the newly documented contracts, then regenerate all pinned descriptions and TypeScript witnesses; the drift gate remains failing and must not be bypassed. Feature-scope confirmation is pending. |

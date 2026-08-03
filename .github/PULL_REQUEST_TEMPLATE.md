<!--
Keep the "why" in the commit messages — this template is a checklist, not a
substitute for them. See CONTRIBUTING.md for the full workflow.
-->

## What this changes

<!-- One or two sentences. What surface or behaviour moved, and why. -->

## Related findings

<!--
If this closes or advances a BUGS.md finding, name the ID(s) here and update the
row(s) in the same PR. IDs stay out of source/comments by convention.
-->

## Checklist

- [ ] `GOWORK=off go build ./...` and `make test` pass locally
- [ ] `gofmt` clean; `go vet ./...` and `go vet -tags noui ./...` pass
- [ ] Any closed `BUGS.md` finding is updated to `fixed` in this PR
- [ ] Web changes: `bun run typecheck` and `bun run test:coverage` pass
- [ ] No parity allowlist was widened without explanation
- [ ] CI is (or is expected to be) green

# third_party — vendored external contracts

Pinned, checked-in copies of external artifacts bleephub tests and builds
against. Nothing here is authored by this project; every file is fetched or
generated from a pinned upstream and guarded by a drift check so it cannot
silently diverge. Do not hand-edit these files — run the refresh script and
commit the result.

> Named `third_party/` (not `vendor/`) on purpose: Go reserves a top-level
> `vendor/` directory for module vendoring and refuses to build if one exists
> without a valid `vendor/modules.txt`.

| File | What it is | Consumed by | Refresh | Drift check (CI) |
| --- | --- | --- | --- | --- |
| `github-openapi.json.gz` | GitHub's DOTCOM REST OpenAPI description (`github/rest-api-description`, `api.github.com.json`), gzipped | Go shape/route parity tests (`openapi_shape_validator_test.go`, `gh_api_definition_test.go`), `scripts/parity_inventory.py`, and the generator below | `scripts/update-github-openapi.sh` | `scripts/check-github-openapi-drift.sh` |
| `github-openapi.VERSION` | Provenance + pin for the above: upstream commit, source URL, content/gzip sha256, `openapi info.version`, per-variant route-index digests | Route/spec parity tests read the pin | (written by the refresh script) | (same) |
| `github-openapi-routes.txt.gz` | Route index derived from the pinned spec across GHEC/GHES/api-2022 variants | `gh_api_definition_test.go`, `scripts/parity_inventory.py` | (written by the refresh script) | (same) |
| `github-graphql-schema.graphql.gz` | GitHub's public GraphQL schema (`docs.github.com/public/fpt/schema.docs.graphql`), gzipped | `graphql_schema_parity_test.go` | `scripts/update-github-graphql-schema.sh` | `scripts/check-github-graphql-schema-drift.sh` |
| `github-graphql-schema.VERSION` | Provenance + pin (source URL, content/gzip sha256) for the GraphQL schema | GraphQL parity test reads the pin | (written by the refresh script) | (same) |
| `github-openapi.d.ts` | TypeScript types **generated** from `github-openapi.json.gz` by `openapi-typescript@7.13.0` — the web app's GitHub-compatible response contracts (`components["schemas"][…]`), zero runtime | `web/src/types.ts` (imported type-only) | `scripts/gen-web-openapi-types.sh` | `scripts/check-web-openapi-types-drift.sh` |

## Refreshing

Each artifact is refreshed by its own script (see the table). The OpenAPI and
GraphQL refreshers re-pin from upstream (new commit / sha256) and rewrite the
`.VERSION` file. After bumping `github-openapi.json.gz`, regenerate the derived
web types:

```sh
scripts/gen-web-openapi-types.sh   # regenerates github-openapi.d.ts from the pinned spec
```

The drift checks run in CI (`.github/workflows/ci.yml`,
`.github/workflows/github-api-contract-drift.yml`): they fail if a checked-in
copy no longer matches its pinned upstream, or — for `github-openapi.d.ts` — if
it is not a byte-for-byte regeneration of the pinned spec (so the generated
contract stays a faithful witness of the vendored spec).

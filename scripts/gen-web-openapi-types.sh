#!/usr/bin/env bash
# Regenerate third_party/github-openapi.d.ts from the vendored GitHub
# OpenAPI description (WEB-013). The web app's GitHub-compatible response types
# are GENERATED from the same pinned spec the Go server validates against
# (third_party/github-openapi.json.gz, pinned in .VERSION and
# guarded by scripts/check-github-openapi-drift.sh), not hand-written and
# blindly cast.
#
# Types-only output (openapi-typescript emits zero runtime code) written as a
# .d.ts so tsconfig's skipLibCheck skips type-checking the ~144k generated
# lines while callers still resolve real schemas via components["schemas"][…].
#
# Pinned generator version; run via bunx so it is not a locked dependency and
# never touches the web bundle, bun audit, or the dependency-age gate.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SPEC_GZ="$REPO_ROOT/third_party/github-openapi.json.gz"
OUT="$REPO_ROOT/third_party/github-openapi.d.ts"
OPENAPI_TS_VERSION="7.13.0"

tmp="$(mktemp -t github-openapi.XXXXXX.json)"
trap 'rm -f "$tmp"' EXIT
gzip -dc "$SPEC_GZ" >"$tmp"

mkdir -p "$(dirname "$OUT")"
bunx "openapi-typescript@${OPENAPI_TS_VERSION}" "$tmp" --output "$OUT"

echo "wrote $OUT ($(wc -l <"$OUT") lines)"

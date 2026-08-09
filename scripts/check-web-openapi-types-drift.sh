#!/usr/bin/env bash
# Verify third_party/github-openapi.d.ts is a byte-for-byte regeneration of the
# pinned third_party/github-openapi.json.gz (WEB-013). The committed generated
# types are a *witness* of the vendored spec: this fails if the spec was bumped
# without regenerating them, or if the file was hand-edited — either way the web
# contract would no longer match what the Go server validates against.
#
# Uses the same pinned generator (and bunx invocation) as
# scripts/gen-web-openapi-types.sh, so a clean tree round-trips exactly.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SPEC_GZ="$ROOT/third_party/github-openapi.json.gz"
OUT="$ROOT/third_party/github-openapi.d.ts"
OPENAPI_TS_VERSION="7.13.0"

tmp_spec="$(mktemp -t github-openapi.XXXXXX.json)"
tmp_out="$(mktemp -t github-openapi.XXXXXX.d.ts)"
trap 'rm -f "$tmp_spec" "$tmp_out"' EXIT

gzip -dc "$SPEC_GZ" >"$tmp_spec"
bunx "openapi-typescript@${OPENAPI_TS_VERSION}" "$tmp_spec" --output "$tmp_out" >/dev/null

if diff -q "$tmp_out" "$OUT" >/dev/null; then
  echo "bleephub-web-openapi-types: OK ($(basename "$OUT") matches the pinned spec)"
else
  {
    echo "FAIL: third_party/github-openapi.d.ts is stale vs third_party/github-openapi.json.gz"
    echo "Run scripts/gen-web-openapi-types.sh and commit the result."
    diff "$tmp_out" "$OUT" | head -30
  } >&2
  exit 1
fi

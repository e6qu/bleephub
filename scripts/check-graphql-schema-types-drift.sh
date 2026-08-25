#!/usr/bin/env bash
# Verify internal/graphqlschema/*_gen.go is a byte-for-byte regeneration of
# the pinned third_party/github-graphql-schema.graphql.gz.
#
# The committed generated types are a *witness* of the vendored schema: this
# fails if the schema was bumped without regenerating them, or if a generated
# file was hand-edited. The completeness ratchet in internal/server catches
# structural drift (a missing type or a wrong field signature), but not a
# stale description or a dropped @deprecated reason — those only show up here.
#
# Uses the same in-tree generator as scripts/gen-graphql-schema.sh, so a clean
# tree round-trips exactly.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

tmp_dir="$(mktemp -d -t bleephub-graphql-schema-gen.XXXXXX)"
trap 'rm -rf "$tmp_dir"' EXIT

GOWORK=off go run ./internal/graphqlschemagen -out "$tmp_dir" >/dev/null

status=0
for generated in "$tmp_dir"/*_gen.go; do
  name="$(basename "$generated")"
  if ! diff -q "$generated" "internal/graphqlschema/$name" >/dev/null 2>&1; then
    {
      echo "FAIL: internal/graphqlschema/$name is stale vs third_party/github-graphql-schema.graphql.gz"
      diff "$generated" "internal/graphqlschema/$name" | head -30
    } >&2
    status=1
  fi
done
for committed in internal/graphqlschema/*_gen.go; do
  name="$(basename "$committed")"
  if [[ ! -f "$tmp_dir/$name" ]]; then
    echo "FAIL: internal/graphqlschema/$name is no longer generated; re-run scripts/gen-graphql-schema.sh" >&2
    status=1
  fi
done

if [[ "$status" -ne 0 ]]; then
  echo "Run scripts/gen-graphql-schema.sh and commit the result." >&2
  exit 1
fi
echo "bleephub-graphql-schema-types: OK (internal/graphqlschema matches the pinned schema)"

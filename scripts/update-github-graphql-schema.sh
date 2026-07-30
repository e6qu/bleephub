#!/usr/bin/env bash
# Refresh GitHub's official public GraphQL schema behind an explicit digest pin.
set -euo pipefail

PIN_SHA256="c504a0ed454276c878d5a873b782fa9824f2dec3205de3370845d40977e41322"
SOURCE_URL="https://docs.github.com/public/fpt/schema.docs.graphql"

usage() {
  echo "usage: $0 [--sha256 <hex>] [--print-pin]" >&2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --sha256)
      PIN_SHA256="${2:?--sha256 needs a value}"
      shift 2
      ;;
    --print-pin)
      echo "sha256=$PIN_SHA256 source=$SOURCE_URL"
      exit 0
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "error: unknown argument $1" >&2
      usage
      exit 2
      ;;
  esac
done

if [[ ! "$PIN_SHA256" =~ ^[0-9a-f]{64}$ ]]; then
  echo "error: sha256 pin must be 64 lowercase hex characters" >&2
  exit 2
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEST="$ROOT/internal/server/testdata/github-graphql-schema.graphql.gz"
VERSION="$ROOT/internal/server/testdata/github-graphql-schema.VERSION"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

curl --fail --location --silent --show-error "$SOURCE_URL" --output "$tmp"
got="$(shasum -a 256 "$tmp" | awk '{print $1}')"
if [[ "$got" != "$PIN_SHA256" ]]; then
  echo "error: current GitHub GraphQL schema hashes to $got, expected $PIN_SHA256" >&2
  echo "review the schema diff, then rerun with --sha256 $got and update PIN_SHA256" >&2
  exit 1
fi

if ! grep -q '^type Query' "$tmp" || ! grep -q '^type Mutation' "$tmp"; then
  echo "error: downloaded document is not the GitHub GraphQL SDL" >&2
  exit 1
fi

gzip -n -9 -c "$tmp" > "$DEST"
gzip_sha="$(shasum -a 256 "$DEST" | awk '{print $1}')"
lines="$(wc -l < "$tmp" | tr -d ' ')"
{
  echo "vendored: GitHub GraphQL public schema"
  echo "source: $SOURCE_URL"
  echo "content sha256: $PIN_SHA256"
  echo "gzip sha256: $gzip_sha"
  echo "lines: $lines"
  echo "refreshed: $(date -u +%F)"
  echo "refresh: scripts/update-github-graphql-schema.sh"
} > "$VERSION"
echo "vendored GitHub GraphQL schema: $lines lines, sha256 $PIN_SHA256"

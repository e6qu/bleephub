#!/usr/bin/env bash
# Fail when GitHub's rolling public GraphQL schema differs from the pinned copy.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pin_output="$("$ROOT/scripts/update-github-graphql-schema.sh" --print-pin)"
pinned="${pin_output#sha256=}"
pinned="${pinned%% *}"
source_url="${pin_output##* source=}"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

curl --fail --location --silent --show-error "$source_url" --output "$tmp"
upstream="$(shasum -a 256 "$tmp" | awk '{print $1}')"
if [[ "$upstream" != "$pinned" ]]; then
  cat >&2 <<EOF
error: the vendored GitHub GraphQL schema is stale
  pinned:   $pinned
  upstream: $upstream

Review the official schema diff, update Bleephub and its compatibility
allowlist, then refresh scripts/update-github-graphql-schema.sh.
EOF
  exit 1
fi
echo "GitHub GraphQL schema pin is current: $pinned"

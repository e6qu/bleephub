#!/usr/bin/env bash
# Fail when the vendored GitHub REST contract is not pinned to the current
# upstream rest-api-description commit. This complements the hermetic hash
# gate: the hash proves reproducibility, while this check proves the snapshot
# has not silently become stale.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pin_output="$(bash "$ROOT/scripts/update-github-openapi.sh" --print-pin)"
pinned="${pin_output#commit=}"
pinned="${pinned%% *}"

if [[ ! "$pinned" =~ ^[0-9a-f]{40}$ ]]; then
  echo "error: could not parse the vendored OpenAPI commit pin from: $pin_output" >&2
  exit 2
fi

upstream="$(git ls-remote https://github.com/github/rest-api-description.git HEAD | awk 'NR == 1 { print $1 }')"
if [[ ! "$upstream" =~ ^[0-9a-f]{40}$ ]]; then
  echo "error: could not resolve github/rest-api-description HEAD" >&2
  exit 2
fi

if [[ "$pinned" != "$upstream" ]]; then
  cat >&2 <<EOF
error: the vendored GitHub REST contract is stale
  pinned:   $pinned
  upstream: $upstream

Refresh scripts/update-github-openapi.sh and the vendored definition, then
implement every newly documented operation and response-contract change.
EOF
  exit 1
fi

echo "GitHub REST contract pin is current: $pinned"

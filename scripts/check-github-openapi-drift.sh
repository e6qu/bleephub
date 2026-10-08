#!/usr/bin/env bash
# Fail when a GitHub REST description Bleephub vendors differs from the same
# description at the current upstream rest-api-description commit. This
# complements the hermetic hash gate: the hash proves reproducibility, while
# this check proves the snapshot has not silently become stale. The files are
# compared, not the commits: upstream commits often leave every description
# Bleephub vendors byte-identical, and such a commit is nothing to refresh.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UPDATE="$ROOT/scripts/update-github-openapi.sh"
pin_output="$(bash "$UPDATE" --print-pin)"
pinned="${pin_output#commit=}"
pinned="${pinned%% *}"
pinned_sha256="${pin_output##*sha256=}"

if [[ ! "$pinned" =~ ^[0-9a-f]{40}$ || ! "$pinned_sha256" =~ ^[0-9a-f]{64}$ ]]; then
  echo "error: could not parse the vendored OpenAPI pins from: $pin_output" >&2
  exit 2
fi

upstream="$(git ls-remote https://github.com/github/rest-api-description.git HEAD | awk 'NR == 1 { print $1 }')"
if [[ ! "$upstream" =~ ^[0-9a-f]{40}$ ]]; then
  echo "error: could not resolve github/rest-api-description HEAD" >&2
  exit 2
fi

# Every vendored description and the SHA-256 it is pinned to: the main one,
# then each extra one update-github-openapi.sh lists as "name:sha256".
descriptions=("api.github.com:$pinned_sha256")
while IFS= read -r entry; do
  descriptions+=("$entry")
done < <(sed -n '/^EXTRA_DESCRIPTIONS=(/,/^)/p' "$UPDATE" | grep -oE '"[a-z0-9.-]+:[0-9a-f]{64}"' | tr -d '"')
if (( ${#descriptions[@]} < 2 )); then
  echo "error: could not read EXTRA_DESCRIPTIONS from $UPDATE" >&2
  exit 2
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
changed=()
for entry in "${descriptions[@]}"; do
  name="${entry%%:*}"
  want="${entry##*:}"
  curl --fail --silent --show-error --location --retry 5 --retry-all-errors --retry-delay 2 \
    --output "$tmp" \
    "https://raw.githubusercontent.com/github/rest-api-description/$upstream/descriptions/$name/$name.json"
  got="$(shasum -a 256 "$tmp" | cut -d' ' -f1)"
  if [[ "$got" != "$want" ]]; then
    changed+=("$name")
  fi
done

if (( ${#changed[@]} > 0 )); then
  cat >&2 <<EOF
error: the vendored GitHub REST contract is stale
  pinned:   $pinned
  upstream: $upstream
  changed:  ${changed[*]}

Refresh scripts/update-github-openapi.sh and the vendored definition, then
implement every newly documented operation and response-contract change.
EOF
  exit 1
fi

echo "GitHub REST contract is current: the ${#descriptions[@]} vendored descriptions match upstream $upstream (pinned at $pinned)"

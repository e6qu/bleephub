#!/usr/bin/env bash
# Fail when GitHub's rolling public GraphQL schema differs from the pinned copy.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pin_output="$("$ROOT/scripts/update-github-graphql-schema.sh" --print-pin)"
pinned="${pin_output#sha256=}"
pinned="${pinned%% *}"
source_url="${pin_output##* source=}"
accepted_output="$("$ROOT/scripts/update-github-graphql-schema.sh" --print-accepted-sha256)"
read -r -a accepted_sha256 <<< "$accepted_output"
tmp_files=()
trap 'rm -f "${tmp_files[@]}"' EXIT

# GitHub deploys the rolling docs schema through multiple CDN edges. During an
# edge rollout, one successful response can briefly differ while subsequent
# no-cache reads still return the pinned public contract. Do not turn that
# mixed deployment state into a flaky PR failure: accept either digest in the
# explicitly reviewed rollout set, and require three independent unreviewed
# mismatches before declaring the vendored schema stale. Once a genuinely new
# rollout has converged, all three reads deterministically fail.
observed=()
for _ in 1 2 3; do
  tmp="$(mktemp)"
  tmp_files+=("$tmp")
  curl --fail --location --silent --show-error \
    --header "Cache-Control: no-cache" \
    --header "Pragma: no-cache" \
    "$source_url" --output "$tmp"
  upstream="$(shasum -a 256 "$tmp" | awk '{print $1}')"
  for accepted in "${accepted_sha256[@]}"; do
    if [[ "$upstream" == "$accepted" ]]; then
      if [[ "$upstream" == "$pinned" ]]; then
        echo "GitHub GraphQL schema pin is current: $pinned"
      else
        echo "GitHub GraphQL schema rollout variant is reviewed: $upstream (vendored: $pinned)"
      fi
      exit 0
    fi
  done
  observed+=("$upstream")
done

cat >&2 <<EOF
error: the vendored GitHub GraphQL schema is stale
  pinned:   $pinned
  observed: ${observed[*]}

Review the official schema diff, update Bleephub and its compatibility
allowlist, then refresh scripts/update-github-graphql-schema.sh.
EOF
exit 1

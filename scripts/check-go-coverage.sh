#!/usr/bin/env bash
set -euo pipefail

profile=${1:-coverage.out}
minimum=${BLEEPHUB_GO_COVERAGE_MIN:-75.0}

if [[ ! -s "$profile" ]]; then
  echo "Go coverage profile is missing or empty: $profile" >&2
  exit 1
fi

total=$(go tool cover -func="$profile" | awk '/^total:/ { gsub(/%/, "", $3); print $3 }')
if [[ -z "$total" ]]; then
  echo "Could not read total coverage from $profile" >&2
  exit 1
fi

if ! awk -v total="$total" -v minimum="$minimum" 'BEGIN { exit !(total + 0 >= minimum + 0) }'; then
  echo "Go statement coverage ${total}% is below the ${minimum}% non-regression floor" >&2
  exit 1
fi

echo "Go statement coverage ${total}% meets the ${minimum}% non-regression floor"

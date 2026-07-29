#!/usr/bin/env bash
# Run knip (TypeScript dead-exports checker) on the bleephub UI package.
#
# knip exits non-zero once the issue count passes --max-issues, which defaults
# to zero, so its exit code is the verdict. This script used to discard that
# and decide by filtering the human-readable report down to "any remaining
# output means failure" — which makes an unrelated new warning line a failure,
# and makes any report wording knip changes in a future release a silent pass.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../web"

if ./node_modules/.bin/knip --reporter json > /dev/null; then
  echo "bleephub-knip: OK"
  exit 0
fi

echo "FAIL: bleephub knip found dead exports/files:" >&2
./node_modules/.bin/knip >&2 || true
exit 1

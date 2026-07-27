#!/usr/bin/env bash
# Run jscpd (JavaScript/TypeScript copy-paste detector) on bleephub UI src.
# Threshold: 200 tokens (below that it is React boilerplate — DataTable column
# definitions and tab panel setup). Test files are excluded from the count.
#
# The exclusion is --ignore, not --ignore-pattern: the latter takes regexes
# matching code blocks *inside* files, so the "src/__tests__/**" it used to be
# given matched nothing and every test file was scanned all along.
#
# jscpd exits zero whether it finds clones or not — verified: 499 clones at
# --min-tokens 30 still exits 0 — so the count has to be read out of the
# report. This reads the JSON reporter rather than grepping the console
# reporter for the string "Clone found", which upstream is free to reword at
# any release, silently turning the gate into a pass.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../web"

report_dir=$(mktemp -d)
trap 'rm -rf "$report_dir"' EXIT

npx --yes jscpd@5.0.12 \
  --min-tokens 200 \
  --ignore "**/__tests__/**" \
  --reporters json \
  --output "$report_dir" \
  src > /dev/null

report="$report_dir/jscpd-report.json"
if [[ ! -f "$report" ]]; then
  echo "FAIL: bleephub jscpd produced no report at $report" >&2
  exit 1
fi

count=$(jq '.statistics.total.clones' "$report")
if [[ "$count" != "0" ]]; then
  echo "FAIL: bleephub jscpd found $count clone(s) above threshold (200 tokens):" >&2
  jq -r '.duplicates[] | "  \(.firstFile.name):\(.firstFile.start) == \(.secondFile.name):\(.secondFile.start) (\(.lines) lines)"' "$report" >&2
  exit 1
fi
echo "bleephub-jscpd: OK (threshold: 200 tokens)"

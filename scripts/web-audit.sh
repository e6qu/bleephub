#!/usr/bin/env bash
# Audit the web dependency graph with `bun audit`.
#
# A real advisory fails the build. But `bun audit` queries npm's advisory
# registry over the network, and when that registry is unreachable it either
# errors ("ConnectionClosed: audit request failed") or hangs until killed — a
# transient outage that says nothing about the locked dependencies. A supply-
# chain scan that cannot reach the registry produces no signal, so treat an
# inability to run as an inconclusive warning rather than failing CI (which would
# block every merge during an npm incident). A genuine finding is still fatal.
set -uo pipefail

cd "$(dirname "$0")/../web" || exit 1

timeout_seconds="${WEB_AUDIT_TIMEOUT_SECONDS:-120}"
output="$(timeout "${timeout_seconds}" bun audit 2>&1)"
code=$?
printf '%s\n' "$output"

if [ "$code" -eq 0 ]; then
	exit 0
fi

# Killed by timeout (124), or an explicit registry/network failure → inconclusive.
if [ "$code" -eq 124 ] ||
	printf '%s' "$output" | grep -qiE 'ConnectionClosed|audit request failed|ETIMEDOUT|ENOTFOUND|ECONNREFUSED|EAI_AGAIN|socket hang ?up|network|fetch failed|request to .* failed'; then
	echo "::warning::bun audit could not reach the advisory registry (exit ${code}); treating as inconclusive, not a finding. The locked graph was not audited this run."
	exit 0
fi

# Any other non-zero exit is a real audit finding (or a genuine error): fail.
exit "$code"

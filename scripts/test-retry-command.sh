#!/usr/bin/env bash
set -euo pipefail

root="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
counter="$(mktemp)"
trap 'rm -f "$counter"' EXIT

printf '0\n' >"$counter"
# The script passed to sh expands in that child process, not in this test.
# shellcheck disable=SC2016
RETRY_MAX_ATTEMPTS=5 RETRY_INITIAL_DELAY_SECONDS=0 \
	"$root/scripts/retry-command.sh" sh -c '
		count=$(cat "$1")
		count=$((count + 1))
		printf "%s\n" "$count" >"$1"
		test "$count" -ge 3
	' sh "$counter"
if [[ "$(cat "$counter")" != 3 ]]; then
	echo "retry helper did not stop after the first successful attempt" >&2
	exit 1
fi

printf '0\n' >"$counter"
# shellcheck disable=SC2016
if RETRY_MAX_ATTEMPTS=5 RETRY_INITIAL_DELAY_SECONDS=0 \
	"$root/scripts/retry-command.sh" sh -c '
		count=$(cat "$1")
		printf "%s\n" "$((count + 1))" >"$1"
		exit 1
	' sh "$counter"; then
	echo "retry helper accepted a persistently failing command" >&2
	exit 1
fi
if [[ "$(cat "$counter")" != 5 ]]; then
	echo "retry helper did not exhaust the configured attempt count" >&2
	exit 1
fi

echo "retry helper contract passed"

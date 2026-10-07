#!/usr/bin/env bash
# Contract for govulncheck-retry.sh, against a stand-in govulncheck that fails
# the way the real one does.
set -euo pipefail

root="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
fake="$work/govulncheck"
cat >"$fake" <<'FAKE'
#!/usr/bin/env bash
count=$(($(cat "$FAKE_COUNTER") + 1))
printf '%s\n' "$count" >"$FAKE_COUNTER"
if ((count < FAKE_FETCH_FAILURES)); then
	echo "govulncheck: fetching vulnerabilities: HTTP GET https://vuln.go.dev/ID/GO-2023-1753.json.gz returned unexpected status: 504 Gateway Timeout" >&2
	exit 1
fi
exit "$FAKE_FINAL_STATUS"
FAKE
chmod +x "$fake"

run() {
	printf '0\n' >"$work/count"
	GOVULNCHECK="$fake" FAKE_COUNTER="$work/count" FAKE_FETCH_FAILURES="$1" FAKE_FINAL_STATUS="$2" \
		RETRY_MAX_ATTEMPTS=3 RETRY_INITIAL_DELAY_SECONDS=0 \
		"$root/scripts/govulncheck-retry.sh" ./... 2>/dev/null
}
expect() {
	local want_status=$1 want_attempts=$2 got_status=0
	shift 2
	run "$@" || got_status=$?
	if [[ "$got_status" != "$want_status" || "$(cat "$work/count")" != "$want_attempts" ]]; then
		echo "fetch failures=$1 final=$2: exit $got_status after $(cat "$work/count") attempts, want exit $want_status after $want_attempts" >&2
		exit 1
	fi
}

expect 0 3 3 0 # two fetch failures, then a clean scan
expect 3 1 0 3 # a finding fails at once
expect 3 2 2 3 # a finding after one fetch failure still fails
expect 1 1 0 1 # any other error is not retried
expect 1 3 9 0 # a database that stays unreachable fails after the last attempt
echo "govulncheck retry contract passed"

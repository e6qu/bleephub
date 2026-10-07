#!/usr/bin/env bash
# Runs govulncheck, retrying with backoff only when it could not fetch the
# vulnerability database. govulncheck (golang.org/x/vuln v1.8.0,
# internal/scan/errors.go) exits 3 when it finds vulnerabilities and 1 for any
# other error; a database fetch failure is one of those errors, reported as
# "fetching vulnerabilities: ..." (internal/vulncheck/fetch.go). A finding, a
# usage error or a package that fails to load therefore fails at once; only an
# unreachable database is retried.
set -euo pipefail

govulncheck="${GOVULNCHECK:-govulncheck}"
max_attempts="${RETRY_MAX_ATTEMPTS:-5}"
delay="${RETRY_INITIAL_DELAY_SECONDS:-10}"
if [[ ! "$max_attempts" =~ ^[1-9][0-9]*$ ]]; then
	echo "RETRY_MAX_ATTEMPTS must be a positive integer" >&2
	exit 2
fi
if [[ ! "$delay" =~ ^[0-9]+$ ]]; then
	echo "RETRY_INITIAL_DELAY_SECONDS must be a non-negative integer" >&2
	exit 2
fi

stderr_file="$(mktemp)"
trap 'rm -f "$stderr_file"' EXIT
for ((attempt = 1; ; attempt++)); do
	status=0
	"$govulncheck" "$@" 2> >(tee "$stderr_file" >&2) || status=$?
	wait
	if ((status == 0)); then
		exit 0
	fi
	if ((status != 1)) || ! grep -q 'fetching vulnerabilities: ' "$stderr_file"; then
		exit "$status"
	fi
	if ((attempt == max_attempts)); then
		echo "::error::govulncheck could not fetch the vulnerability database after $attempt attempts" >&2
		exit "$status"
	fi
	echo "::warning::govulncheck could not fetch the vulnerability database (attempt $attempt); retrying in ${delay}s" >&2
	sleep "$delay"
	delay=$((delay * 2))
done

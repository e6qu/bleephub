#!/usr/bin/env bash
set -euo pipefail

if (($# == 0)); then
	echo "usage: retry-command.sh <command> [argument ...]" >&2
	exit 2
fi

max_attempts="${RETRY_MAX_ATTEMPTS:-5}"
delay="${RETRY_INITIAL_DELAY_SECONDS:-5}"
if [[ ! "$max_attempts" =~ ^[1-9][0-9]*$ ]]; then
	echo "RETRY_MAX_ATTEMPTS must be a positive integer" >&2
	exit 2
fi
if [[ ! "$delay" =~ ^[0-9]+$ ]]; then
	echo "RETRY_INITIAL_DELAY_SECONDS must be a non-negative integer" >&2
	exit 2
fi

for ((attempt = 1; attempt <= max_attempts; attempt++)); do
	if "$@"; then
		exit 0
	fi
	if ((attempt == max_attempts)); then
		echo "::error::Command failed after $attempt attempts: $*" >&2
		exit 1
	fi
	echo "::warning::Command attempt $attempt failed; retrying in ${delay}s: $*" >&2
	sleep "$delay"
	delay=$((delay * 2))
done

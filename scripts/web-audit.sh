#!/usr/bin/env bash
# Audit the complete web dependency graph. Findings, registry failures, and
# timeouts all fail: an unavailable scan is not evidence of a safe release.
set -euo pipefail

cd "$(dirname "$0")/../web"
timeout --kill-after=5 "${WEB_AUDIT_TIMEOUT_SECONDS:-120}" bun audit

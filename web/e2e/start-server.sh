#!/usr/bin/env bash
# Starts bleephub server for Playwright E2E tests.
# Expects SERVER_BIN to be set to the path of the compiled bleephub binary.
set -e
: "${SERVER_BIN:?SERVER_BIN must point to the compiled Bleephub server}"

PORT="${PORT:-15555}"
SSH_PORT="${BLEEPHUB_E2E_SSH_PORT:-15556}"
WEBHOOK_PORT="${BLEEPHUB_E2E_WEBHOOK_PORT:-15557}"
SSH_KEY_DIR="$(mktemp -d)"
SSH_KEY_FILE="${SSH_KEY_DIR}/host_ed25519"

# The admin token has no default in the binary — the E2E harness sets it.
export BLEEPHUB_ADMIN_TOKEN="bleephub-admin-token-00000000000000000000"
# Exercise the real SSH Git listener. The generated key is limited to this
# disposable E2E process; deployed instances receive a durable key from AWS
# Secrets Manager instead.
ssh-keygen -q -t ed25519 -N "" -f "${SSH_KEY_FILE}"
export BLEEPHUB_SSH_ADDR="127.0.0.1:${SSH_PORT}"
export BLEEPHUB_SSH_HOST="127.0.0.1:${SSH_PORT}"
BLEEPHUB_SSH_HOST_KEY="$(<"${SSH_KEY_FILE}")"
export BLEEPHUB_SSH_HOST_KEY
export BLEEPHUB_E2E_WEBHOOK_PORT="${WEBHOOK_PORT}"
export BLEEPHUB_E2E_WEBHOOK_SECRET="playwright-marketplace-secret"

SERVER_PID=""
WEBHOOK_PID=""
cleanup() {
  if [[ -n "${SERVER_PID}" ]]; then
    kill "${SERVER_PID}" 2>/dev/null || true
  fi
  if [[ -n "${WEBHOOK_PID}" ]]; then
    kill "${WEBHOOK_PID}" 2>/dev/null || true
  fi
  rm -rf "${SSH_KEY_DIR}"
}
trap cleanup EXIT

bun e2e/webhook-receiver.ts &
WEBHOOK_PID=$!

"$SERVER_BIN" -addr ":${PORT}" -log-level debug &
SERVER_PID=$!

# Wait for server to be ready
for _ in $(seq 1 30); do
  if curl -s "http://localhost:${PORT}/health" > /dev/null 2>&1 &&
    curl -s "http://127.0.0.1:${WEBHOOK_PORT}/health" > /dev/null 2>&1; then
    break
  fi
  sleep 0.1
done

echo "bleephub PID=$SERVER_PID on :$PORT"

wait "${SERVER_PID}"

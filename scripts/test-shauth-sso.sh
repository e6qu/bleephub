#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
set -euo pipefail

: "${SHAUTH_SOURCE_DIR:?SHAUTH_SOURCE_DIR must point to a Shauth checkout}"

root="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
compose=(docker compose --project-directory "$SHAUTH_SOURCE_DIR" -f "$SHAUTH_SOURCE_DIR/compose.yaml" -p bleephub-shauth-sso)
temporary="$(mktemp -d)"
primary_pid=""
secondary_pid=""

random_secret() {
  openssl rand -base64 48 | tr -d '\n'
}

cleanup() {
  status=$?
  if [[ "$status" -ne 0 ]]; then
    "${compose[@]}" logs --no-color --tail=160 shauth hydra postgres >&2 || true
    for log in "$temporary"/*.log; do
      [[ -f "$log" ]] || continue
      printf '\n%s\n' "===== $log =====" >&2
      tail -160 "$log" >&2 || true
    done
  fi
  for pid in "$primary_pid" "$secondary_pid"; do
    if [[ -n "$pid" ]]; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$temporary"
  return "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

POSTGRES_PASSWORD="$(openssl rand -hex 32)"
export POSTGRES_PASSWORD
HYDRA_SYSTEM_SECRET="$(random_secret)"
export HYDRA_SYSTEM_SECRET
export HYDRA_DSN="postgres://shauth:${POSTGRES_PASSWORD}@postgres:5432/hydra?sslmode=disable"
export HYDRA_PUBLIC_URL="http://localhost:4444"
export SHAUTH_PUBLIC_URL="http://localhost:8080"
export SHAUTH_DATABASE_URL="postgres://shauth:${POSTGRES_PASSWORD}@postgres:5432/shauth?sslmode=disable"
export GITHUB_CLIENT_ID="local-password-integration"
export GITHUB_CLIENT_SECRET="local-password-integration"
SHAUTH_BOOTSTRAP_ADMIN_PASSWORD="$(random_secret)"
export SHAUTH_BOOTSTRAP_ADMIN_PASSWORD
primary_secret="$(random_secret)"
secondary_secret="$(random_secret)"
export SHAUTH_BOOTSTRAP_APPS_JSON
SHAUTH_BOOTSTRAP_APPS_JSON="$(printf '[{"slug":"bleephub-primary","name":"Bleephub primary","description":"Primary Bleephub SSO acceptance application.","launch_url":"http://localhost:15555/ui/","oidc_client_id":"bleephub-primary","oidc_client_secret":"%s","redirect_uris":["http://localhost:15555/auth/shauth/callback"],"post_logout_redirect_uris":["http://localhost:15555/ui/signed-out"],"frontchannel_logout_uri":"http://localhost:15555/auth/shauth/frontchannel-logout","health_url":"http://localhost:15555/health","monitoring_url":""},{"slug":"bleephub-secondary","name":"Bleephub secondary","description":"Secondary Bleephub SSO acceptance application.","launch_url":"http://localhost:15556/ui/","oidc_client_id":"bleephub-secondary","oidc_client_secret":"%s","redirect_uris":["http://localhost:15556/auth/shauth/callback"],"post_logout_redirect_uris":["http://localhost:15556/ui/signed-out"],"frontchannel_logout_uri":"http://localhost:15556/auth/shauth/frontchannel-logout","health_url":"http://localhost:15556/health","monitoring_url":""}]' "$primary_secret" "$secondary_secret")"

"${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
"${compose[@]}" up --build --detach

for _ in $(seq 1 180); do
  if curl --fail --silent http://localhost:8080/healthz >/dev/null 2>&1 && curl --fail --silent http://localhost:4444/health/ready >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl --fail --silent --show-error http://localhost:8080/healthz >/dev/null
curl --fail --silent --show-error http://localhost:4444/health/ready >/dev/null

start_bleephub() {
  port="$1"
  client_id="$2"
  client_secret="$3"
  instance="$temporary/$client_id"
  mkdir -p "$instance/git" "$instance/data"
  env \
    BLEEPHUB_ADMIN_TOKEN="$(random_secret)" \
    BLEEPHUB_EXTERNAL_URL="http://localhost:$port" \
    BLEEPHUB_ALLOW_INSECURE_OIDC=true \
    BLEEPHUB_SHAUTH_ISSUER=http://localhost:4444 \
    BLEEPHUB_SHAUTH_CLIENT_ID="$client_id" \
    BLEEPHUB_SHAUTH_CLIENT_SECRET="$client_secret" \
    BLEEPHUB_SHAUTH_POST_LOGOUT_URL="http://localhost:$port/ui/signed-out" \
    BLEEPHUB_DATA_DIR="$instance/data" \
    BLEEPHUB_GIT_DIR="$instance/git" \
    "$root/bleephub-server" -addr "127.0.0.1:$port" >"$temporary/$client_id.log" 2>&1 &
  started_pid=$!
}

start_bleephub 15555 bleephub-primary "$primary_secret"
primary_pid="$started_pid"
start_bleephub 15556 bleephub-secondary "$secondary_secret"
secondary_pid="$started_pid"

for port in 15555 15556; do
  for _ in $(seq 1 100); do
    if curl --fail --silent "http://localhost:$port/health" >/dev/null 2>&1; then
      break
    fi
    sleep 0.1
  done
  curl --fail --silent --show-error "http://localhost:$port/health" >/dev/null
done

(
  cd "$root/web"
  SHAUTH_BOOTSTRAP_ADMIN_PASSWORD="$SHAUTH_BOOTSTRAP_ADMIN_PASSWORD" bun e2e/shauth-sso.mjs
)

#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
set -euo pipefail

: "${SHAUTH_SOURCE_DIR:?SHAUTH_SOURCE_DIR must point to a Shauth checkout}"

primary_port="${BLEEPHUB_SSO_PRIMARY_PORT:-15555}"
secondary_port="${BLEEPHUB_SSO_SECONDARY_PORT:-15556}"
case "$primary_port:$secondary_port" in
  *[!0-9:]*|:*) echo "Bleephub SSO ports must be numeric" >&2; exit 2 ;;
esac
if [[ "$primary_port" = "$secondary_port" ]]; then
  echo "Bleephub SSO ports must be distinct" >&2
  exit 2
fi
export BLEEPHUB_SSO_PRIMARY_PORT="$primary_port"
export BLEEPHUB_SSO_SECONDARY_PORT="$secondary_port"

root="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
export BLEEPHUB_SOURCE_DIR="$root"
compose=(docker compose --project-directory "$SHAUTH_SOURCE_DIR" -f "$SHAUTH_SOURCE_DIR/compose.yaml" -f "$root/test/shauth/compose.override.yaml" -p bleephub-shauth-sso)
temporary="$(mktemp -d)"

random_secret() {
  openssl rand -base64 48 | tr -d '\n'
}

cleanup() {
  status=$?
  if [[ "$status" -ne 0 ]]; then
    "${compose[@]}" logs --no-color --tail=160 shauth hydra postgres bleephub-primary bleephub-secondary >&2 || true
    for log in "$temporary"/*.log; do
      [[ -f "$log" ]] || continue
      printf '\n%s\n' "===== $log =====" >&2
      tail -160 "$log" >&2 || true
    done
  fi
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
SHAUTH_VALIDATOR_USERNAME="admin"
export SHAUTH_VALIDATOR_USERNAME
BLEEPHUB_NON_AUTHENTIC_CREDENTIAL_SENTINEL="$(random_secret)"
if [[ "$BLEEPHUB_NON_AUTHENTIC_CREDENTIAL_SENTINEL" = "$SHAUTH_BOOTSTRAP_ADMIN_PASSWORD" ]]; then
  echo "Bleephub negative-probe sentinel must differ from the Shauth password" >&2
  exit 1
fi
export BLEEPHUB_NON_AUTHENTIC_CREDENTIAL_SENTINEL
primary_secret="$(random_secret)"
secondary_secret="$(random_secret)"
BLEEPHUB_SSO_ADMIN_TOKEN="$(random_secret)"
export BLEEPHUB_SSO_ADMIN_TOKEN
export BLEEPHUB_SSO_PRIMARY_CLIENT_SECRET="$primary_secret"
export BLEEPHUB_SSO_SECONDARY_CLIENT_SECRET="$secondary_secret"
export SHAUTH_BOOTSTRAP_APPS_JSON
SHAUTH_BOOTSTRAP_APPS_JSON="$(printf '[{"slug":"bleephub-primary","name":"Bleephub primary","description":"Primary Bleephub SSO acceptance application.","launch_url":"http://localhost:%s/ui/","oidc_client_id":"bleephub-primary","oidc_client_secret":"%s","redirect_uris":["http://localhost:%s/auth/shauth/callback"],"post_logout_redirect_uris":["http://localhost:%s/ui/signed-out"],"frontchannel_logout_uri":"http://localhost:%s/auth/shauth/frontchannel-logout","backchannel_logout_uri":"http://localhost:%s/auth/shauth/backchannel-logout","health_url":"http://localhost:%s/health","monitoring_url":"","release_revision":"bleephub-sso-local","validation_url":"http://localhost:%s/auth/validation","signed_out_url":"http://localhost:%s/ui/signed-out"},{"slug":"bleephub-secondary","name":"Bleephub secondary","description":"Secondary Bleephub SSO acceptance application.","launch_url":"http://127.0.0.1:%s/ui/","oidc_client_id":"bleephub-secondary","oidc_client_secret":"%s","redirect_uris":["http://127.0.0.1:%s/auth/shauth/callback"],"post_logout_redirect_uris":["http://127.0.0.1:%s/ui/signed-out"],"frontchannel_logout_uri":"http://127.0.0.1:%s/auth/shauth/frontchannel-logout","backchannel_logout_uri":"http://127.0.0.1:%s/auth/shauth/backchannel-logout","health_url":"http://127.0.0.1:%s/health","monitoring_url":"","release_revision":"bleephub-sso-local","validation_url":"http://127.0.0.1:%s/auth/validation","signed_out_url":"http://127.0.0.1:%s/ui/signed-out"}]' "$primary_port" "$primary_secret" "$primary_port" "$primary_port" "$primary_port" "$primary_port" "$primary_port" "$primary_port" "$primary_port" "$secondary_port" "$secondary_secret" "$secondary_port" "$secondary_port" "$secondary_port" "$secondary_port" "$secondary_port" "$secondary_port" "$secondary_port")"

"${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
"${compose[@]}" up --build --detach

for service in bleephub-primary bleephub-secondary; do
  container_id="$("${compose[@]}" ps --quiet "$service")"
  while IFS= read -r binding; do
    value="${binding#*=}"
    if [[ "$value" = "$SHAUTH_VALIDATOR_USERNAME" || "$value" = "$SHAUTH_BOOTSTRAP_ADMIN_PASSWORD" ]]; then
      echo "Shauth validator credentials leaked into $service runtime" >&2
      exit 1
    fi
  done < <(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$container_id")
done

for _ in $(seq 1 180); do
  if curl --fail --silent http://localhost:8080/healthz >/dev/null 2>&1 && curl --fail --silent http://localhost:4444/health/ready >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl --fail --silent --show-error http://localhost:8080/healthz >/dev/null
curl --fail --silent --show-error http://localhost:4444/health/ready >/dev/null

for port in "$primary_port" "$secondary_port"; do
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
  SHAUTH_VALIDATOR_USERNAME="$SHAUTH_VALIDATOR_USERNAME" \
    SHAUTH_BOOTSTRAP_ADMIN_PASSWORD="$SHAUTH_BOOTSTRAP_ADMIN_PASSWORD" \
    BLEEPHUB_NON_AUTHENTIC_CREDENTIAL_SENTINEL="$BLEEPHUB_NON_AUTHENTIC_CREDENTIAL_SENTINEL" \
    bun e2e/shauth-sso.mjs
)

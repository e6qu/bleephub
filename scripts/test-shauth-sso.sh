#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
set -euo pipefail

: "${SHAUTH_SOURCE_DIR:?SHAUTH_SOURCE_DIR must point to a Shauth checkout}"

readonly expected_shauth_commit="08f5a78fb8b159fcbfe8317f24f430dbdfd3ed56"
actual_shauth_commit="$(git -C "$SHAUTH_SOURCE_DIR" rev-parse HEAD)"
if [[ "$actual_shauth_commit" != "$expected_shauth_commit" ]]; then
  echo "Shauth checkout is $actual_shauth_commit; expected $expected_shauth_commit" >&2
  exit 1
fi
if [[ -n "$(git -C "$SHAUTH_SOURCE_DIR" status --porcelain)" ]]; then
  echo "Shauth checkout must be clean at $expected_shauth_commit" >&2
  exit 1
fi

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
BLEEPHUB_TEST_REVISION="sha256:$({
  git -C "$root" rev-parse HEAD
  git -C "$root" diff --binary --no-ext-diff HEAD
} | openssl dgst -sha256 | awk '{print $NF}')"
export BLEEPHUB_TEST_REVISION
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
export HYDRA_PUBLIC_URL="http://localhost:8080"
export SHAUTH_PUBLIC_URL="http://localhost:8080"
export SHAUTH_DATABASE_URL="postgres://shauth:${POSTGRES_PASSWORD}@postgres:5432/shauth?sslmode=disable"
export GITHUB_CLIENT_ID="local-password-integration"
export GITHUB_CLIENT_SECRET="local-password-integration"
SHAUTH_BOOTSTRAP_ADMIN_PASSWORD="$(random_secret)"
export SHAUTH_BOOTSTRAP_ADMIN_PASSWORD
SHAUTH_VALIDATOR_USERNAME="admin"
export SHAUTH_VALIDATOR_USERNAME
SHAUTH_VALIDATOR_TOKEN="$(random_secret)"
SHAUTH_VALIDATION_STATUS_TOKEN="$(random_secret)"
if [[ "$SHAUTH_VALIDATOR_TOKEN" = "$SHAUTH_VALIDATION_STATUS_TOKEN" ]]; then
  echo "Shauth validator and validation-status tokens must differ" >&2
  exit 1
fi
export SHAUTH_VALIDATOR_TOKEN SHAUTH_VALIDATION_STATUS_TOKEN
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
SHAUTH_BOOTSTRAP_APPS_JSON="$(jq -cn \
  --arg primary_origin "http://localhost:$primary_port" \
  --arg primary_secret "$primary_secret" \
  --arg secondary_origin "http://127.0.0.1:$secondary_port" \
  --arg secondary_secret "$secondary_secret" \
  --arg revision "$BLEEPHUB_TEST_REVISION" '
  def app($slug; $name; $description; $origin; $secret): {
    slug: $slug,
    name: $name,
    description: $description,
    launch_url: ($origin + "/ui/"),
    oidc_client_id: $slug,
    oidc_client_secret: $secret,
    redirect_uris: [($origin + "/auth/shauth/callback")],
    post_logout_redirect_uris: [($origin + "/auth/shauth/logout/complete")],
    frontchannel_logout_uri: ($origin + "/auth/shauth/frontchannel-logout"),
    backchannel_logout_uri: ($origin + "/auth/shauth/backchannel-logout"),
    health_url: ($origin + "/health"),
    monitoring_url: "",
    release_revision: $revision,
    validation_url: ($origin + "/auth/validation"),
    signed_out_url: ($origin + "/ui/signed-out")
  };
  [
    app("bleephub-primary"; "Bleephub primary"; "Primary Bleephub SSO acceptance application."; $primary_origin; $primary_secret),
    app("bleephub-secondary"; "Bleephub secondary"; "Secondary Bleephub SSO acceptance application."; $secondary_origin; $secondary_secret)
  ]')"

"${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
"${compose[@]}" up --build --detach

for service in bleephub-primary bleephub-secondary; do
  container_id="$("${compose[@]}" ps --quiet "$service")"
  while IFS= read -r binding; do
    value="${binding#*=}"
    if [[ "$value" = "$SHAUTH_VALIDATOR_USERNAME" ||
      "$value" = "$SHAUTH_BOOTSTRAP_ADMIN_PASSWORD" ||
      "$value" = "$SHAUTH_VALIDATOR_TOKEN" ||
      "$value" = "$SHAUTH_VALIDATION_STATUS_TOKEN" ]]; then
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

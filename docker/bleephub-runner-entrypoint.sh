#!/usr/bin/env bash
set -euo pipefail

: "${RUNNER_URL:?RUNNER_URL must be the Bleephub server URL}"
: "${RUNNER_TOKEN:?RUNNER_TOKEN must be a Bleephub runner registration token}"

runner_name="${RUNNER_NAME:-$(hostname)}"
runner_workdir="${RUNNER_WORKDIR:-_work}"

config_args=(
  --unattended
  --url "$RUNNER_URL"
  --token "$RUNNER_TOKEN"
  --name "$runner_name"
  --work "$runner_workdir"
  --replace
)

if [[ -n "${RUNNER_LABELS:-}" ]]; then
  config_args+=(--labels "$RUNNER_LABELS")
fi
if [[ "${RUNNER_EPHEMERAL:-true}" == "true" ]]; then
  config_args+=(--ephemeral)
fi
if [[ -n "${RUNNER_GROUP:-}" ]]; then
  config_args+=(--runnergroup "$RUNNER_GROUP")
fi

./config.sh "${config_args[@]}"
exec ./run.sh

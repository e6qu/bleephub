#!/usr/bin/env bash
# Runs the `gh` conformance driver inside the pinned GitHub CLI image.
#
# The image is the one the "GitHub CLI compatibility" job already builds
# (Dockerfile.gh-test): the exact `gh` release the compatibility documentation
# verifies, checked against its published digest, plus a server built from this
# checkout. This wrapper only overrides the entry point so the same image runs
# the conformance driver instead of the pass/fail script, and bind-mounts a
# directory for the JSON Lines the driver writes.
#
# Where the certificate authority can be trusted without a container — Linux,
# where Go honours SSL_CERT_FILE — the harness runs drivers/gh_driver.sh
# directly and never reaches this script.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
OUT_FILE="${1:?usage: gh_container.sh <results-file>}"
OUT_DIR="$(cd "$(dirname "$OUT_FILE")" && pwd)"
OUT_NAME="$(basename "$OUT_FILE")"

IMAGE="${CONFORMANCE_GH_IMAGE:-bleephub-gh-test:local}"
if [ "${CONFORMANCE_GH_BUILD:-1}" = "1" ]; then
    docker buildx build --load -f "$ROOT/Dockerfile.gh-test" -t "$IMAGE" "$ROOT"
fi

docker run --rm \
    --entrypoint /test/gh_driver.sh \
    -e BPH_RESULTS="/out/$OUT_NAME" \
    -e BPH_WORK=/tmp/gh-conformance \
    -v "$OUT_DIR:/out" \
    "$IMAGE"

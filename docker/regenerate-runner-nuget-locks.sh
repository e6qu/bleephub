#!/usr/bin/env bash
# Regenerates docker/runner-nuget-locks/ for the runner release Dockerfile.runner
# pins. actions/runner ships no NuGet lock files, so the image build restores in
# locked mode against these (docker/build-actions-runner.sh) and a runner bump
# has to produce them again: run this after changing RUNNER_VERSION,
# RUNNER_SOURCE_COMMIT or DOTNET_SDK_VERSION, then review the diff — every
# package version that moved is subject to scripts/check-dependency-age.py.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dockerfile="$root/Dockerfile.runner"

arg() { sed -n -E "s/^ARG $1=(.+)$/\\1/p" "$dockerfile" | head -n 1; }
runner_version="$(arg RUNNER_VERSION)"
runner_commit="$(arg RUNNER_SOURCE_COMMIT)"
sdk_version="$(arg DOTNET_SDK_VERSION)"
# The builder stage's image reference, digest and all.
sdk_image="$(sed -n -E 's/^FROM (--platform=[^ ]+ )?([^ ]*dotnet\/sdk:[^ ]+) AS actions-runner-builder$/\2/p' "$dockerfile")"
: "${runner_version:?}" "${runner_commit:?}" "${sdk_version:?}" "${sdk_image:?}"

locks="$root/docker/runner-nuget-locks"
# Detached, then waited on: some Docker front ends cannot attach a stream.
container="$(docker run --detach \
    --volume "$locks:/locks" \
    --volume "$root/docker/patch-actions-runner.sh:/patch-actions-runner.sh:ro" \
    --env RUNNER_VERSION="$runner_version" \
    --env RUNNER_SOURCE_COMMIT="$runner_commit" \
    --env DOTNET_SDK_VERSION="$sdk_version" \
    "$sdk_image" bash -euo pipefail -c '
        git init --quiet /runner-src
        git -C /runner-src remote add origin https://github.com/actions/runner.git
        git -C /runner-src fetch --quiet --depth=1 origin "refs/tags/v${RUNNER_VERSION}:refs/tags/v${RUNNER_VERSION}"
        git -C /runner-src checkout --quiet --detach "$RUNNER_SOURCE_COMMIT"
        test "$(git -C /runner-src rev-parse HEAD)" = "$RUNNER_SOURCE_COMMIT"
        sed -i -E "s#(\"version\": \")[0-9]+\\.[0-9]+\\.[0-9]+#\\1${DOTNET_SDK_VERSION}#" /runner-src/src/global.json
        bash /patch-actions-runner.sh
        timeout --kill-after=5 540 dotnet restore /runner-src/src/ActionsRunner.sln \
            -p:RestorePackagesWithLockFile=true -p:RestoreLockedMode=false
        find /locks -name packages.lock.json -type f -delete
        cd /runner-src/src
        find . -name packages.lock.json -type f -print | while IFS= read -r lock; do
            case "$lock" in ./Test/*) continue ;; esac
            mkdir -p "/locks/$(dirname "$lock")"
            cp "$lock" "/locks/$lock"
        done
    ')"
cleanup() { docker rm --force "$container" >/dev/null 2>&1 || true; }
trap cleanup EXIT
# Poll with a deadline so a failed network restore never strands a container.
deadline=$((SECONDS + 600))
while [ "$(docker inspect --format '{{.State.Running}}' "$container")" = true ]; do
    if [ "$SECONDS" -ge "$deadline" ]; then
        docker logs --tail 40 "$container" >&2
        echo "runner lock regeneration exceeded 600 seconds" >&2
        exit 1
    fi
    sleep 2
done
status="$(docker inspect --format '{{.State.ExitCode}}' "$container")"
docker logs "$container" 2>&1 | tail -n 20
docker rm "$container" >/dev/null
test "$status" = 0
find "$locks" -type d -empty -delete
echo "regenerated NuGet locks for runner v$runner_version (SDK $sdk_version):"
find "$locks" -name packages.lock.json -type f | sort

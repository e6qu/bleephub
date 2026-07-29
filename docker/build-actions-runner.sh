#!/usr/bin/env bash
set -euo pipefail

: "${TARGETARCH:?TARGETARCH is required}"
: "${RUNNER_VERSION:?RUNNER_VERSION is required}"
: "${RUNNER_SOURCE_COMMIT:?RUNNER_SOURCE_COMMIT is required}"
: "${DOTNET_SDK_VERSION:?DOTNET_SDK_VERSION is required}"
: "${DOTNET_RUNTIME_VERSION:?DOTNET_RUNTIME_VERSION is required}"
: "${RUNNER_NUGET_LOCKS:=/runner-nuget-locks}"
: "${RUNNER_SOURCE_DIR:=/runner-src}"

case "$TARGETARCH" in
    amd64) runner_runtime=linux-x64 ;;
    arm64) runner_runtime=linux-arm64 ;;
    *)
        echo "Unsupported target architecture: $TARGETARCH" >&2
        exit 1
        ;;
esac

git init "$RUNNER_SOURCE_DIR"
git -C "$RUNNER_SOURCE_DIR" remote add origin https://github.com/actions/runner.git
git -C "$RUNNER_SOURCE_DIR" fetch --depth=1 origin \
    "refs/tags/v${RUNNER_VERSION}:refs/tags/v${RUNNER_VERSION}"
git -C "$RUNNER_SOURCE_DIR" checkout --detach "$RUNNER_SOURCE_COMMIT"
test "$(git -C "$RUNNER_SOURCE_DIR" rev-parse HEAD)" = "$RUNNER_SOURCE_COMMIT"
test "$(cat "$RUNNER_SOURCE_DIR/releaseVersion")" = "$RUNNER_VERSION"
test "$(cat "$RUNNER_SOURCE_DIR/src/runnerversion")" = "$RUNNER_VERSION"

# GitHub's release was built one servicing patch before the current .NET 8
# security release. Rebuild the immutable release source with the matching SDK;
# replacing runtime files underneath precompiled assemblies is not binary-safe.
sed -i -E \
    "s#(\"version\": \")[0-9]+\\.[0-9]+\\.[0-9]+#\\1${DOTNET_SDK_VERSION}#" \
    "$RUNNER_SOURCE_DIR/src/global.json"
grep -q "\"version\": \"${DOTNET_SDK_VERSION}\"" \
    "$RUNNER_SOURCE_DIR/src/global.json"

cp -a "$RUNNER_NUGET_LOCKS/." "$RUNNER_SOURCE_DIR/src/"

dotnet msbuild \
    -t:layout \
    -p:PackageRuntime="$runner_runtime" \
    -p:BUILDCONFIG=Release \
    -p:RunnerVersion="$RUNNER_VERSION" \
    -p:RestoreLockedMode=true \
    "$RUNNER_SOURCE_DIR/src/dir.proj"

while IFS= read -r lock; do
    relative="${lock#"$RUNNER_NUGET_LOCKS/"}"
    cmp "$lock" "$RUNNER_SOURCE_DIR/src/$relative"
done < <(find "$RUNNER_NUGET_LOCKS" -name packages.lock.json -type f -print | sort)

find "$RUNNER_SOURCE_DIR/_layout/bin" -type f -name '*.pdb' -delete
grep -q "Microsoft.NETCore.App.Runtime.${runner_runtime}/${DOTNET_RUNTIME_VERSION}" \
    "$RUNNER_SOURCE_DIR/_layout/bin/Runner.Listener.deps.json"
grep -q "\"Runner.Listener/${RUNNER_VERSION}\"" \
    "$RUNNER_SOURCE_DIR/_layout/bin/Runner.Listener.deps.json"
grep -q "\"version\": \"${DOTNET_RUNTIME_VERSION}\"" \
    "$RUNNER_SOURCE_DIR/_layout/bin/Runner.Listener.runtimeconfig.json"

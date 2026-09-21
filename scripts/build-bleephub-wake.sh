#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out_dir=${1:-"$repo_root/.build/bleephub-ecs"}
mkdir -p "$out_dir"
out_dir=$(cd "$out_dir" && pwd)
go_cache=${GOCACHE:-"$HOME/.cache/go-build"}

pushd "$repo_root/terraform/wake" >/dev/null
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 GOCACHE="$go_cache" GOWORK=off go build -trimpath -ldflags='-s -w' -o "$out_dir/bootstrap" .
popd >/dev/null

# The ZIP is a redistribution of the listener's binary, so the terms of what is
# linked into it, and the record of what it was built from, travel beside it.
# Lambda runs bootstrap and ignores the rest.
cp "$repo_root/LICENSE" "$repo_root/THIRD-PARTY-LICENSES.txt" "$out_dir/"
"$repo_root/scripts/write-build-info.sh" "$out_dir/bootstrap" "$out_dir/BUILD-INFO.txt"
rm -f "$out_dir/bleephub-wake.zip"
(cd "$out_dir" && zip -q -9 bleephub-wake.zip bootstrap LICENSE THIRD-PARTY-LICENSES.txt BUILD-INFO.txt)
rm "$out_dir/bootstrap" "$out_dir/LICENSE" "$out_dir/THIRD-PARTY-LICENSES.txt" "$out_dir/BUILD-INFO.txt"
printf '%s\n' "$out_dir/bleephub-wake.zip"

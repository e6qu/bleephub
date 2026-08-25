#!/usr/bin/env bash
# Run the whole test suite under the race detector without exhausting the CI
# runner's RAM.
#
# The race detector keeps "shadow memory" describing every memory location a
# test process has touched, and that shadow state is never reclaimed for the
# life of the process. One `go test -race` process covering the large
# internal/server package (2400+ tests) therefore accumulates shadow memory
# monotonically until it passes the 16 GB hosted-runner ceiling: the VM starts
# to thrash, the `go test -timeout` deadline goroutine can no longer be
# scheduled, and the control plane eventually tears the runner down — surfacing
# as a job that hangs for an hour on one step and then dies with exit 143 and
# "runner received a shutdown signal". Lowering -p/-parallel only slows the
# accumulation (12 min to OOM became 63 min); it does not cap it.
#
# Shadow memory is per-process, though, and is released when the process exits.
# So the fix is to run internal/server's tests across several *fresh* processes,
# each of which starts with empty shadow memory. Sharding by a round-robin over
# the sorted top-level test names keeps the shards balanced by count and
# interleaves neighbouring (often related, similarly heavy) tests across shards.
# Every other package is small enough to race together in one process.
set -euo pipefail

TAGS="noui"
SHARDS="${RACE_SHARDS:-8}"
TIMEOUT="${RACE_TIMEOUT:-20m}"
SERVER_PKG="github.com/e6qu/bleephub/internal/server"

# Every package except internal/server, raced together — these are individually
# small and their combined shadow memory stays well under the ceiling.
# (Plain read loops rather than mapfile so the script also runs under the
# bash 3.2 that ships on developer macOS, not only the runner's bash 5.)
other=()
while IFS= read -r pkg; do
  other+=("$pkg")
done < <(go list -tags "$TAGS" ./... | grep -v "^${SERVER_PKG}\$")
echo "== race: ${#other[@]} packages (excluding internal/server) =="
go test -race -tags "$TAGS" -count=1 -timeout "$TIMEOUT" "${other[@]}"

# internal/server, sharded into fresh processes. -list enumerates the top-level
# test names; the compiled race binary is cached and reused by every shard run.
tests=()
while IFS= read -r name; do
  tests+=("$name")
done < <(go test -tags "$TAGS" -list '^Test' "$SERVER_PKG" | grep '^Test' | sort)
total=${#tests[@]}
if [ "$total" -eq 0 ]; then
  echo "no tests found in $SERVER_PKG" >&2
  exit 1
fi
echo "== race: internal/server has $total top-level tests across $SHARDS shards =="

for ((shard = 0; shard < SHARDS; shard++)); do
  group=()
  for ((i = shard; i < total; i += SHARDS)); do
    group+=("${tests[i]}")
  done
  if [ "${#group[@]}" -eq 0 ]; then
    continue
  fi
  # Anchored alternation so each name matches exactly one shard; a single-element
  # -run pattern still runs each matched test's subtests.
  regex="^($(
    IFS='|'
    echo "${group[*]}"
  ))\$"
  echo "== race shard $((shard + 1))/$SHARDS: ${#group[@]} tests =="
  go test -race -tags "$TAGS" -count=1 -timeout "$TIMEOUT" -run "$regex" "$SERVER_PKG"
done

#!/usr/bin/env bash
# Run deadcode on bleephub to detect unreachable functions.
# Requires golang.org/x/tools/cmd/deadcode in $PATH or ~/go/bin.
set -euo pipefail

DEADCODE=$(command -v deadcode 2>/dev/null || echo "$HOME/go/bin/deadcode")
if [[ ! -x "$DEADCODE" ]]; then
  echo "bleephub-deadcode: deadcode not found; install with:" >&2
  echo "  go install golang.org/x/tools/cmd/deadcode@latest" >&2
  exit 1
fi

# What the server cannot reach is dead, with one exception. gitstore is a
# library with users besides this server: its bench harness, and whoever imports
# it. OpenS3 is its one-call constructor for them. The server must NOT call it:
# the server builds its bucket in internal/gitbackend's openBucket, the one place
# that chooses between object-store drivers. So its being unreachable from
# cmd/bleephub is the design holding, and it is named here rather than deleted
# from a library's public surface. Anything else in gitstore is still reported.
LIBRARY_ENTRY_POINTS='^gitstore/store\.go:[0-9]+:[0-9]+: unreachable func: OpenS3$'

out=$(GOWORK=off "$DEADCODE" -tags noui ./cmd/bleephub/ 2>&1 | grep -Ev "$LIBRARY_ENTRY_POINTS" || true)
if [[ -n "$out" ]]; then
  echo "FAIL: bleephub deadcode found unreachable functions:" >&2
  echo "$out" >&2
  exit 1
fi
echo "bleephub-deadcode: OK"

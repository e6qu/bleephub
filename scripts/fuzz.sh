#!/usr/bin/env bash
# Run a bounded -fuzz burst over every fuzz target in internal/server.
#
# Without -fuzz, `go test` only replays each target's seed corpus, so 28 fuzz
# targets amount to a few hundred table cases and no mutation ever happens.
# This gives each target a short budget of real mutation. It is a burst, not a
# campaign: it is sized to fit a pull-request gate, and finding a crash is a
# matter of luck per run — the value is that a regression reachable within a few
# seconds of mutation cannot survive a merge.
#
# Targets are discovered from the source, so a new one is fuzzed without
# editing this script.
#
# FUZZTIME  per-target budget (default 15s)
# FUZZPKG   package to fuzz (default ./internal/server/)
set -euo pipefail

FUZZTIME="${FUZZTIME:-15s}"
FUZZPKG="${FUZZPKG:-./internal/server/}"

ROOT=$(git rev-parse --show-toplevel)
cd "$ROOT"

targets=()
while IFS= read -r target; do
  targets+=("${target}")
done < <(
  grep -rhoE '^func (Fuzz[A-Za-z0-9_]+)\(f \*testing\.F\)' "${FUZZPKG}" |
    sed -E 's/^func (Fuzz[A-Za-z0-9_]+).*/\1/' | sort -u
)

if [[ "${#targets[@]}" -eq 0 ]]; then
  echo "bleephub-fuzz: no fuzz targets found in ${FUZZPKG}" >&2
  exit 1
fi

echo "bleephub-fuzz: ${#targets[@]} target(s), ${FUZZTIME} each"

failed=()
# run_target fuzzes one target and prints its combined output. -run '^$' skips
# the ordinary tests so only the named target is fuzzed.
run_target() {
  GOWORK=off go test -tags noui -run '^$' \
    -fuzz "^${1}\$" -fuzztime "${FUZZTIME}" "${FUZZPKG}" 2>&1
}

for target in "${targets[@]}"; do
  echo "::group::${target}"
  if out=$(run_target "${target}"); then
    status=0
  else
    status=$?
  fi
  # A real crasher writes a reproducer under testdata/fuzz/<target>/ ("Failing
  # input written") or prints a panic. A bare "context deadline exceeded" with
  # neither is the fuzz coordinator being interrupted at -fuzztime under CI load,
  # not a bug — retry once. A genuinely slow input reproduces on the retry and
  # still fails the gate; a transient deadline does not.
  if [[ ${status} -ne 0 ]] \
    && ! grep -q 'Failing input written' <<<"${out}" \
    && ! grep -q 'panic:' <<<"${out}" \
    && grep -q 'context deadline exceeded' <<<"${out}"; then
    echo "bleephub-fuzz: ${target} hit a fuzz-coordinator deadline with no reproducer; retrying once" >&2
    if out=$(run_target "${target}"); then
      status=0
    else
      status=$?
    fi
  fi
  printf '%s\n' "${out}"
  if [[ ${status} -eq 0 ]]; then
    echo "bleephub-fuzz: ${target} OK"
  else
    failed+=("${target}")
    echo "bleephub-fuzz: ${target} FAILED" >&2
  fi
  echo "::endgroup::"
done

if [[ "${#failed[@]}" -gt 0 ]]; then
  echo "FAIL: bleephub-fuzz found crashers in ${#failed[@]} target(s): ${failed[*]}" >&2
  echo "Reproducers were written under internal/server/testdata/fuzz/." >&2
  exit 1
fi
echo "bleephub-fuzz: OK (${#targets[@]} targets, ${FUZZTIME} each)"

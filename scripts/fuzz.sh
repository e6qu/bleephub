#!/usr/bin/env bash
# Run a bounded -fuzz burst over every fuzz target in the repository.
#
# Without -fuzz, `go test` only replays each target's seed corpus, so the fuzz
# targets amount to a few hundred table cases and no mutation ever happens. This
# gives each target a short budget of real mutation. It is a burst, not a
# campaign: it is sized to fit a pull-request gate, and finding a crash is a
# matter of luck per run — the value is that a regression reachable within a few
# seconds of mutation cannot survive a merge.
#
# Targets are discovered from the source across every package that declares one
# (internal/server, internal/actions, internal/store, internal/graphqlapi, …),
# so a new target — in any package — is fuzzed without editing this script.
#
# FUZZTIME  per-target budget (default 15s)
# FUZZDIRS  space-separated roots to scan (default: internal)
set -euo pipefail

FUZZTIME="${FUZZTIME:-15s}"
FUZZDIRS="${FUZZDIRS:-internal}"

ROOT=$(git rev-parse --show-toplevel)
cd "$ROOT"

# Discover the package directories that declare at least one fuzz target.
read -r -a fuzz_dirs <<<"${FUZZDIRS}"
pkgdirs=()
while IFS= read -r dir; do
  pkgdirs+=("${dir}")
done < <(
  grep -rlE '^func Fuzz[A-Za-z0-9_]+\(f \*testing\.F\)' --include='*_test.go' "${fuzz_dirs[@]}" |
    xargs -n1 dirname | sort -u
)

if [[ "${#pkgdirs[@]}" -eq 0 ]]; then
  echo "bleephub-fuzz: no fuzz targets found under ${FUZZDIRS}" >&2
  exit 1
fi

# run_target fuzzes one target in one package and prints its combined output.
# -run '^$' skips the ordinary tests so only the named target is fuzzed.
run_target() { # pkgdir target
  GOWORK=off go test -tags noui -run '^$' \
    -fuzz "^${2}\$" -fuzztime "${FUZZTIME}" "./${1}/" 2>&1
}

total=0
failed=()
for dir in "${pkgdirs[@]}"; do
  targets=()
  while IFS= read -r target; do
    targets+=("${target}")
  done < <(
    grep -rhoE '^func (Fuzz[A-Za-z0-9_]+)\(f \*testing\.F\)' "${dir}" |
      sed -E 's/^func (Fuzz[A-Za-z0-9_]+).*/\1/' | sort -u
  )
  for target in "${targets[@]}"; do
    total=$((total + 1))
    echo "::group::${dir} ${target}"
    if out=$(run_target "${dir}" "${target}"); then
      status=0
    else
      status=$?
    fi
    # A real crasher writes a reproducer under <pkg>/testdata/fuzz/<target>/
    # ("Failing input written") or prints a panic. A bare "context deadline
    # exceeded" with neither is the fuzz coordinator being interrupted at
    # -fuzztime under CI load, not a bug — retry once.
    if [[ ${status} -ne 0 ]] \
      && ! grep -q 'Failing input written' <<<"${out}" \
      && ! grep -q 'panic:' <<<"${out}" \
      && grep -q 'context deadline exceeded' <<<"${out}"; then
      echo "bleephub-fuzz: ${dir} ${target} hit a fuzz-coordinator deadline with no reproducer; retrying once" >&2
      if out=$(run_target "${dir}" "${target}"); then
        status=0
      else
        status=$?
      fi
    fi
    printf '%s\n' "${out}"
    if [[ ${status} -eq 0 ]]; then
      echo "bleephub-fuzz: ${target} OK"
    else
      failed+=("${dir}:${target}")
      echo "bleephub-fuzz: ${dir} ${target} FAILED" >&2
    fi
    echo "::endgroup::"
  done
done

if [[ "${#failed[@]}" -gt 0 ]]; then
  echo "FAIL: bleephub-fuzz found crashers in ${#failed[@]} target(s): ${failed[*]}" >&2
  echo "Reproducers were written under <package>/testdata/fuzz/." >&2
  exit 1
fi
echo "bleephub-fuzz: OK (${total} targets across ${#pkgdirs[@]} packages, ${FUZZTIME} each)"

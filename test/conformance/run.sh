#!/usr/bin/env bash
# Software development kit / command-line-interface conformance harness.
#
# Boots a throwaway Bleephub, then drives real, unmodified GitHub clients
# against it and writes one machine-readable conformance report:
#
#   test/conformance/report/conformance-report.json   the scoreboard
#   test/conformance/report/conformance-summary.md    the readable summary
#
# Every driver records one row per operation with pass/fail and, on failure,
# what the client expected versus what Bleephub actually did. Nothing here
# asserts "a 200 came back": a row only passes when the client's own parsing
# succeeded and the decoded value satisfies the operation's contract.
#
# Usage:
#   test/conformance/run.sh                 # every driver that can run here
#   test/conformance/run.sh go-github git   # only the named drivers
#
# Environment:
#   CONFORMANCE_GH=0        skip the `gh` driver (it needs Docker; see below)
#   CONFORMANCE_SERVER_BIN  reuse an already-built server binary
#   CONFORMANCE_KEEP=1      leave the work directory in place for debugging
#
# Why `gh` needs Docker: the GitHub command-line interface only ever speaks
# HTTPS to an Enterprise host and derives a git remote's host without its port,
# so it needs both a certificate it trusts and a server on 443. The pinned
# container gives it both on any machine, identically — which is what lets one
# recorded floor apply to a developer's run and to continuous integration.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HARNESS="$ROOT/test/conformance"
WORK="$HARNESS/.work"
RESULTS="$WORK/results"
REPORT_DIR="$HARNESS/report"

ADMIN_TOKEN="bleephub-admin-token-00000000000000000000"

log() { printf '=== [conformance] %s\n' "$*"; }

ALL_DRIVERS=(go-github octokit pygithub git gh)
if [ "$#" -gt 0 ]; then
    SELECTED=("$@")
else
    SELECTED=("${ALL_DRIVERS[@]}")
fi

selected() {
    local want="$1" driver
    for driver in "${SELECTED[@]}"; do
        [ "$driver" = "$want" ] && return 0
    done
    return 1
}

# Serialize runs. The work directory is wiped and the report overwritten on
# every run, so two concurrent invocations do not merely race for a port: the
# second deletes the first's fixtures mid-flight and then publishes a report
# built from whichever results survived. That produced two corrupted
# scoreboards before this lock existed, and a scoreboard nobody can trust is
# worse than no scoreboard.
#
# mkdir is the atomic primitive here because it is atomic on every filesystem
# and, unlike flock, present on macOS as well as CI. The lock records its
# holder so a run killed before its trap fires leaves a lock that the next run
# can prove is dead rather than one that blocks the harness forever.
LOCK="$HARNESS/.lock"
CONFORMANCE_LOCK_WAIT="${CONFORMANCE_LOCK_WAIT:-1800}"

acquire_lock() {
    local waited=0 holder
    while ! mkdir "$LOCK" 2>/dev/null; do
        holder="$(cat "$LOCK/pid" 2>/dev/null || true)"
        if [ -n "$holder" ] && ! kill -0 "$holder" 2>/dev/null; then
            log "reclaiming the lock from dead process $holder"
            rm -rf "$LOCK"
            continue
        fi
        if [ "$waited" -eq 0 ]; then
            log "another conformance run (pid ${holder:-unknown}) holds the lock; waiting"
        fi
        if [ "$waited" -ge "$CONFORMANCE_LOCK_WAIT" ]; then
            log "gave up waiting for the lock after ${CONFORMANCE_LOCK_WAIT}s"
            exit 1
        fi
        sleep 5
        waited=$((waited + 5))
    done
    printf '%s\n' "$$" >"$LOCK/pid"
    # The trap is installed once, by install_traps below, because bash keeps a
    # single handler per signal: a second `trap ... EXIT` REPLACES the first
    # rather than adding to it, so registering the lock release here and the
    # server shutdown later would silently drop whichever was installed first.
}

# release_lock and stop_server are both run by the one trap installed after the
# server starts. Until then only the lock needs releasing, so install_traps is
# called twice — the second call supersedes the first with a handler that does
# both.
# shellcheck disable=SC2317,SC2329  # invoked indirectly by the EXIT/INT/... trap in install_traps
release_lock() { rm -rf "$LOCK"; }
# shellcheck disable=SC2317,SC2329  # invoked indirectly by the EXIT/INT/... trap in install_traps
stop_server() { :; }

install_traps() {
    # INT/TERM/HUP/PIPE as well as EXIT: a bare EXIT trap does not run when the
    # shell is killed by a signal, and a run whose output is piped into
    # something that closes early (head, a CI log truncator) dies on SIGPIPE.
    trap 'stop_server; release_lock' EXIT INT TERM HUP PIPE
}

acquire_lock
install_traps

rm -rf "$WORK"
mkdir -p "$RESULTS" "$WORK/data" "$WORK/git" "$REPORT_DIR"

# --- Build the server -------------------------------------------------------
SERVER_BIN="${CONFORMANCE_SERVER_BIN:-$WORK/bleephub-server}"
if [ -z "${CONFORMANCE_SERVER_BIN:-}" ]; then
    log "building the server (-tags noui)"
    (cd "$ROOT" && GOWORK=off CGO_ENABLED=0 go build -tags noui -o "$SERVER_BIN" ./cmd/bleephub)
fi

# --- Pick free ports --------------------------------------------------------
free_port() {
    python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}
HTTP_PORT="$(free_port)"
SSH_PORT="$(free_port)"
BASE="http://127.0.0.1:$HTTP_PORT"

# --- Secure Shell host key --------------------------------------------------
# ssh-keygen writes only into the harness work directory; nothing touches the
# user's ~/.ssh, an agent, or any credential store.
ssh-keygen -q -t ed25519 -N "" -C bleephub-conformance-host -f "$WORK/host_ed25519"
ssh-keygen -q -t ed25519 -N "" -C bleephub-conformance-user -f "$WORK/id_ed25519"

# --- Boot -------------------------------------------------------------------
log "starting the server on $BASE (git+ssh on 127.0.0.1:$SSH_PORT)"
BLEEPHUB_ADMIN_TOKEN="$ADMIN_TOKEN" \
    BLEEPHUB_DATA_DIR="$WORK/data" \
    BLEEPHUB_GIT_DIR="$WORK/git" \
    BLEEPHUB_EXTERNAL_URL="$BASE" \
    BLEEPHUB_SSH_ADDR="127.0.0.1:$SSH_PORT" \
    BLEEPHUB_SSH_HOST="127.0.0.1:$SSH_PORT" \
    BLEEPHUB_SSH_HOST_KEY="$(cat "$WORK/host_ed25519")" \
    "$SERVER_BIN" -addr "127.0.0.1:$HTTP_PORT" -log-level warn >"$WORK/server.log" 2>&1 &
SERVER_PID=$!

# shellcheck disable=SC2317,SC2329  # invoked indirectly, by the trap installed below.
stop_server() {
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
}
install_traps

for _ in $(seq 1 60); do
    if curl -fsS "$BASE/health" >/dev/null 2>&1; then break; fi
    sleep 0.25
done
curl -fsS "$BASE/health" >/dev/null || { log "FATAL: server did not become ready"; tail -40 "$WORK/server.log"; exit 1; }

export BPH_BASE="$BASE"
export BPH_TOKEN="$ADMIN_TOKEN"
export BPH_SSH_ADDR="127.0.0.1:$SSH_PORT"
export BPH_SSH_KEY="$WORK/id_ed25519"
export BPH_WORK="$WORK"

# --- Drivers ----------------------------------------------------------------
# Each driver is isolated: it provisions its own fixtures under its own names,
# so a driver that cannot run leaves the others unaffected.
declare -a RAN=()

if selected go-github; then
    log "driver: go-github"
    (cd "$HARNESS/drivers/gogithub" && GOWORK=off BPH_RESULTS="$RESULTS/go-github.jsonl" go run .) || true
    RAN+=("go-github")
fi

if selected git; then
    log "driver: git"
    BPH_RESULTS="$RESULTS/git.jsonl" bash "$HARNESS/drivers/git_driver.sh" || true
    RAN+=("git")
fi

if selected octokit; then
    log "driver: octokit.js"
    if [ -d "$ROOT/web/node_modules/octokit" ]; then
        BPH_RESULTS="$RESULTS/octokit.jsonl" bun "$HARNESS/drivers/octokit_driver.mjs" || true
        RAN+=("octokit")
    else
        log "SKIP octokit: web/node_modules/octokit is missing (run 'cd web && bun install --frozen-lockfile')"
    fi
fi

if selected pygithub; then
    log "driver: PyGithub"
    if bash "$HARNESS/drivers/pygithub_env.sh"; then
        BPH_RESULTS="$RESULTS/pygithub.jsonl" "$WORK/pyenv/bin/python" "$HARNESS/drivers/pygithub_driver.py" || true
        RAN+=("pygithub")
    else
        log "SKIP PyGithub: the pinned virtual environment could not be provisioned"
    fi
fi

if selected gh; then
    # One canonical configuration for `gh`, everywhere. The client needs an
    # HTTPS host it trusts and a server on port 443 (see drivers/gh_driver.sh
    # for both reasons), which means running as root against a certificate
    # authority installed for that process — exactly what the pinned container
    # provides on any machine. Running it natively would need a different port
    # and would therefore produce a DIFFERENT set of results on the developer's
    # machine than in continuous integration, which would make the ratchet floor
    # meaningless. So: the container, or not at all.
    if [ "${CONFORMANCE_GH:-1}" = "1" ] && command -v docker >/dev/null 2>&1; then
        log "driver: gh (containerised; see drivers/gh_driver.sh for why)"
        if bash "$HARNESS/drivers/gh_container.sh" "$RESULTS/gh.jsonl"; then
            RAN+=("gh")
        else
            log "SKIP gh: the container run failed"
        fi
    else
        log "SKIP gh: needs Docker (set CONFORMANCE_GH=0 to silence this)"
    fi
fi

# --- Report -----------------------------------------------------------------
log "merging ${#RAN[@]} driver result sets"
STATUS=0
python3 "$HARNESS/report.py" \
    --results "$RESULTS" \
    --report "$REPORT_DIR/conformance-report.json" \
    --summary "$REPORT_DIR/conformance-summary.md" \
    --floor "$HARNESS/floor.json" \
    --clients "${RAN[*]-}" \
    --require-clients "${CONFORMANCE_REQUIRE_CLIENTS:-}" \
    ${CONFORMANCE_UPDATE_FLOOR:+--update-floor} || STATUS=$?

if [ "${CONFORMANCE_KEEP:-0}" != "1" ]; then
    rm -rf "$WORK/data" "$WORK/git"
fi
exit "$STATUS"

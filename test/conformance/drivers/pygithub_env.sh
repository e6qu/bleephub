#!/usr/bin/env bash
# Provisions the PyGithub driver's virtual environment inside the harness work
# directory. Nothing is installed system-wide and nothing outside the repository
# is written: the environment lives in test/conformance/.work/pyenv and is
# deleted with the rest of the work directory.
#
# The install is hash-pinned (test/conformance/requirements.txt), so it can only
# ever materialise the exact releases the repository vetted with
# scripts/check-dependency-age.py — pip refuses anything whose artifact digest is
# not listed, which is what makes this a pinned dependency mechanism rather than
# an ad-hoc fetch at test time.
set -euo pipefail

HARNESS="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${BPH_WORK:-$HARNESS/.work}"
ENV_DIR="$WORK/pyenv"

if [ -x "$ENV_DIR/bin/python" ] && "$ENV_DIR/bin/python" -c "import github" 2>/dev/null; then
    exit 0
fi

python3 -m venv "$ENV_DIR"
"$ENV_DIR/bin/python" -m pip install --quiet --disable-pip-version-check --upgrade pip >/dev/null
"$ENV_DIR/bin/python" -m pip install --quiet --disable-pip-version-check \
    --require-hashes --requirement "$HARNESS/requirements.txt"
"$ENV_DIR/bin/python" -c "import importlib.metadata as m; print('PyGithub', m.version('PyGithub'))" >&2

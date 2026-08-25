#!/usr/bin/env python3
"""Regenerate test/conformance/requirements.txt from requirements.in.

The conformance harness installs PyGithub with ``pip install --require-hashes``
into a throwaway virtual environment inside the repository. That needs a lock
listing every transitive package at an exact version together with the hash of
every artifact that version publishes, so the same lock is installable on macOS
and on the Linux continuous-integration runner while still refusing anything
that is not the pinned release.

Run this only when the pin in requirements.in changes; it needs network access
to resolve the graph and read the artifact digests, and the result is committed.
The repository's 24-hour dependency quarantine still applies — check the result
with scripts/check-dependency-age.py before committing it.
"""

from __future__ import annotations

import json
import pathlib
import subprocess
import sys
import tempfile
import urllib.request

HARNESS = pathlib.Path(__file__).resolve().parent.parent
INPUT = HARNESS / "requirements.in"
OUTPUT = HARNESS / "requirements.txt"

HEADER = """\
# Locked dependency graph for the PyGithub conformance driver.
# Generated from requirements.in with pip's resolver; every artifact of every
# pinned version is hashed so `pip install --require-hashes` accepts the wheel
# for whichever platform the harness runs on and nothing else.
#
# Regenerate with: test/conformance/drivers/pygithub_lock.py
"""


def resolve() -> list[tuple[str, str]]:
    with tempfile.TemporaryDirectory() as scratch:
        report_path = pathlib.Path(scratch) / "report.json"
        subprocess.run(
            [
                sys.executable,
                "-m",
                "pip",
                "install",
                "--dry-run",
                "--quiet",
                "--report",
                str(report_path),
                "--requirement",
                str(INPUT),
            ],
            check=True,
        )
        report = json.loads(report_path.read_text(encoding="utf-8"))
    packages = {
        (item["metadata"]["name"], item["metadata"]["version"]) for item in report["install"]
    }
    return sorted(packages, key=lambda package: package[0].lower())


def artifact_hashes(name: str, version: str) -> list[str]:
    with urllib.request.urlopen(f"https://pypi.org/pypi/{name}/{version}/json") as response:
        metadata = json.load(response)
    hashes = sorted({item["digests"]["sha256"] for item in metadata["urls"]})
    if not hashes:
        raise SystemExit(f"{name}=={version} publishes no hashed artifacts")
    return hashes


def main() -> int:
    lines = [HEADER]
    for name, version in resolve():
        joined = " \\\n    ".join(f"--hash=sha256:{digest}" for digest in artifact_hashes(name, version))
        lines.append(f"{name}=={version} \\\n    {joined}")
    OUTPUT.write_text("\n".join(lines) + "\n", encoding="utf-8")
    print(f"wrote {OUTPUT}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

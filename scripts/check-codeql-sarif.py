#!/usr/bin/env python3
"""Fail when CodeQL produced one or more SARIF findings."""

from __future__ import annotations

import json
import pathlib
import sys
from typing import Any


def sarif_findings(directory: pathlib.Path) -> list[dict[str, Any]]:
    sarif_files = sorted(directory.rglob("*.sarif"))
    if not sarif_files:
        raise RuntimeError(f"CodeQL produced no SARIF files under {directory}")

    findings: list[dict[str, Any]] = []
    for sarif_file in sarif_files:
        try:
            document = json.loads(sarif_file.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as error:
            raise RuntimeError(f"cannot read CodeQL SARIF {sarif_file}: {error}") from error
        for run in document.get("runs", []):
            for result in run.get("results", []):
                findings.append(result)
    return findings


def finding_summary(finding: dict[str, Any]) -> str:
    location: dict[str, Any] = {}
    locations = finding.get("locations", [])
    if locations:
        location = locations[0].get("physicalLocation", {})
    artifact = location.get("artifactLocation", {})
    region = location.get("region", {})
    summary = {
        "rule": finding.get("ruleId", "unknown"),
        "level": finding.get("level", "warning"),
        "path": artifact.get("uri", "unknown"),
        "line": region.get("startLine", 0),
        "message": finding.get("message", {}).get("text", ""),
    }
    # JSON encoding keeps attacker-controlled CR/LF characters on one physical
    # output line, so SARIF content cannot forge GitHub workflow commands.
    return json.dumps(summary, ensure_ascii=True, separators=(",", ":"))


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print(f"usage: {argv[0]} SARIF_DIRECTORY", file=sys.stderr)
        return 2

    try:
        findings = sarif_findings(pathlib.Path(argv[1]))
    except RuntimeError as error:
        print(f"CodeQL SARIF policy error: {error}", file=sys.stderr)
        return 1

    if not findings:
        print("CodeQL SARIF policy: zero findings")
        return 0

    print(f"CodeQL SARIF policy: rejecting {len(findings)} finding(s)", file=sys.stderr)
    for finding in findings[:50]:
        print(f"CodeQL finding: {finding_summary(finding)}", file=sys.stderr)
    if len(findings) > 50:
        print(f"CodeQL finding: {len(findings) - 50} additional finding(s)", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))

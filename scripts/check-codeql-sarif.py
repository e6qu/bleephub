#!/usr/bin/env python3
"""Fail when CodeQL produced one or more SARIF findings."""

from __future__ import annotations

import json
import pathlib
import sys
from typing import Any


# Reviewed accepted findings, keyed by (ruleId, artifact path). Each is a
# CodeQL alert we have inspected and determined is safe — a false positive whose
# sanitizer CodeQL does not model, or intentional behaviour. Any finding NOT in
# this map still fails the build, so a genuinely new issue is never silently
# accepted; the acceptance is per file, so the same rule firing in a different
# file is still rejected.
ACCEPTED_FINDINGS: dict[tuple[str, str], str] = {
    ("go/request-forgery", "internal/server/webhooks.go"):
        "Webhook delivery targets an operator-configured URL by design; "
        "parseWebhookTargetURL refuses private addresses unless the instance opted in.",
    ("go/request-forgery", "internal/server/gh_pages_deployments.go"):
        "Fetches the artifact URL the deployment run itself produced, not an "
        "attacker-chosen destination.",
    ("go/path-injection", "internal/server/git_storage.go"):
        "Path derives from a repo full name fully sanitised by repoGitDirPath "
        "(validateRepoStorageFullName + filepath.Abs/Join/Rel + IsLocal containment).",
    ("go/path-injection", "internal/server/store_repos.go"):
        "Same repoGitDirPath containment sanitiser as git_storage.go.",
    ("go/reflected-xss", "internal/server/gh_middleware.go"):
        "Generic ResponseWriter.Write wrapper; API responses are JSON with a "
        "non-HTML Content-Type, not reflected HTML.",
    ("go/reflected-xss", "internal/server/observe_middleware.go"):
        "Observability ResponseWriter wrapper passing bytes through unchanged; JSON responses.",
    ("go/reflected-xss", "internal/server/gh_api_insights.go"):
        "Status-recording ResponseWriter wrapper; JSON responses.",
    ("go/unvalidated-url-redirection", "internal/server/gh_oauth.go"):
        "OAuth callback redirect target is the app's registered redirect_uri, enforced by "
        "requireRegisteredRedirectURI before the redirect.",
    ("go/allocation-size-overflow", "internal/server/gh_enterprise_scim.go"):
        "Capacity hint over SCIM member lists bounded by the request body; the sum cannot overflow int.",
    ("go/regex/missing-regexp-anchor", "internal/server/secret_scanning_ingest.go"):
        "Secret-scanning patterns are intentionally unanchored — a secret must match anywhere in scanned content.",
    ("go/incorrect-integer-conversion", "internal/server/expressions.go"):
        "Array index int(n) is range-checked (0 <= n < len) before use.",
    ("go/incorrect-integer-conversion", "internal/server/store.go"):
        "Hex option/iteration IDs are internally-generated seeds parsed at 64-bit width; "
        "additionally range-guarded before conversion.",
}


def finding_location(finding: dict[str, Any]) -> tuple[str, str]:
    """Return (ruleId, artifact path) for a SARIF result."""
    locations = finding.get("locations", [])
    physical = locations[0].get("physicalLocation", {}) if locations else {}
    path = physical.get("artifactLocation", {}).get("uri", "unknown")
    return finding.get("ruleId", "unknown"), path


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

    accepted = [f for f in findings if finding_location(f) in ACCEPTED_FINDINGS]
    findings = [f for f in findings if finding_location(f) not in ACCEPTED_FINDINGS]
    if accepted:
        print(f"CodeQL SARIF policy: {len(accepted)} reviewed finding(s) accepted")

    if not findings:
        print("CodeQL SARIF policy: zero unaccepted findings")
        return 0

    print(f"CodeQL SARIF policy: rejecting {len(findings)} finding(s)", file=sys.stderr)
    for finding in findings[:50]:
        print(f"CodeQL finding: {finding_summary(finding)}", file=sys.stderr)
    if len(findings) > 50:
        print(f"CodeQL finding: {len(findings) - 50} additional finding(s)", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))

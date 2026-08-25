#!/usr/bin/env python3
"""Merge conformance driver results into the project scoreboard and ratchet it.

Every driver writes JSON Lines records with this shape:

    {"client": "...", "domain": "...", "operation": "...",
     "status": "pass" | "fail" | "skip",
     "request": "...", "expected": "...", "actual": "...", "message": "..."}

This script merges them into one report, prints a readable summary, and
enforces the ratchet in test/conformance/floor.json.

The ratchet is deliberately of the same shape as the repository's other
ratchets (the GraphQL schema parity test, the accessibility sweep baseline):
today's failures are recorded, not treated as build breaks, but an operation
that passes today must keep passing. Concretely the build fails when

  * an operation listed in the floor's ``expected_pass`` no longer passes, or
  * it did not run at all, or
  * a client's total pass count dropped below its recorded floor.

Regenerate the floor with CONFORMANCE_UPDATE_FLOOR=1 after a change that is
meant to move the numbers, and commit the diff — it is the scoreboard's history.
"""

from __future__ import annotations

import argparse
import json
import pathlib
import sys

STATUSES = ("pass", "fail", "skip")


def load_results(results_dir: pathlib.Path) -> list[dict]:
    records: list[dict] = []
    for path in sorted(results_dir.glob("*.jsonl")):
        for number, raw in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
            line = raw.strip()
            if not line:
                continue
            try:
                record = json.loads(line)
            except json.JSONDecodeError as error:
                raise SystemExit(f"{path.name}:{number}: not valid JSON: {error}")
            missing = {"client", "domain", "operation", "status"} - record.keys()
            if missing:
                raise SystemExit(
                    f"{path.name}:{number}: record is missing {sorted(missing)}"
                )
            if record["status"] not in STATUSES:
                raise SystemExit(
                    f"{path.name}:{number}: status {record['status']!r} is not one of {STATUSES}"
                )
            records.append(record)
    return records


def operation_id(record: dict) -> str:
    return f"{record['client']}:{record['domain']}:{record['operation']}"


def tally(records: list[dict], key: str) -> dict[str, dict[str, int]]:
    counts: dict[str, dict[str, int]] = {}
    for record in records:
        bucket = counts.setdefault(
            record[key], {"total": 0, "passed": 0, "failed": 0, "skipped": 0}
        )
        bucket["total"] += 1
        bucket[{"pass": "passed", "fail": "failed", "skip": "skipped"}[record["status"]]] += 1
    for bucket in counts.values():
        bucket["pass_rate"] = (
            round(bucket["passed"] / bucket["total"], 4) if bucket["total"] else 0.0
        )
    return dict(sorted(counts.items()))


def build_report(records: list[dict]) -> dict:
    passed = sum(1 for record in records if record["status"] == "pass")
    failed = sum(1 for record in records if record["status"] == "fail")
    skipped = sum(1 for record in records if record["status"] == "skip")
    total = len(records)
    duplicates = sorted(
        identifier
        for identifier, count in _counter(operation_id(record) for record in records).items()
        if count > 1
    )
    if duplicates:
        raise SystemExit(
            "duplicate operation identifiers make the ratchet ambiguous: "
            + ", ".join(duplicates)
        )
    return {
        "schema": "bleephub-conformance/1",
        "totals": {
            "total": total,
            "passed": passed,
            "failed": failed,
            "skipped": skipped,
            "pass_rate": round(passed / total, 4) if total else 0.0,
        },
        "by_client": tally(records, "client"),
        "by_domain": tally(records, "domain"),
        "failures": [
            {
                "id": operation_id(record),
                "client": record["client"],
                "domain": record["domain"],
                "operation": record["operation"],
                "request": record.get("request", ""),
                "expected": record.get("expected", ""),
                "actual": record.get("actual", ""),
                "message": record.get("message", ""),
            }
            for record in records
            if record["status"] != "pass"
        ],
        "results": sorted(records, key=operation_id),
    }


def _counter(values) -> dict[str, int]:
    counts: dict[str, int] = {}
    for value in values:
        counts[value] = counts.get(value, 0) + 1
    return counts


def render_summary(report: dict) -> str:
    totals = report["totals"]
    lines = [
        "# Bleephub SDK/CLI conformance",
        "",
        f"**{totals['passed']} / {totals['total']} operations pass "
        f"({totals['pass_rate'] * 100:.1f}%)** — {totals['failed']} failed, "
        f"{totals['skipped']} skipped.",
        "",
        "## By client",
        "",
        "| client | passed | failed | skipped | total | pass rate |",
        "| --- | ---: | ---: | ---: | ---: | ---: |",
    ]
    for client, bucket in report["by_client"].items():
        lines.append(
            f"| {client} | {bucket['passed']} | {bucket['failed']} | {bucket['skipped']} "
            f"| {bucket['total']} | {bucket['pass_rate'] * 100:.1f}% |"
        )
    lines += ["", "## By domain", "", "| domain | passed | failed | skipped | total | pass rate |", "| --- | ---: | ---: | ---: | ---: | ---: |"]
    for domain, bucket in report["by_domain"].items():
        lines.append(
            f"| {domain} | {bucket['passed']} | {bucket['failed']} | {bucket['skipped']} "
            f"| {bucket['total']} | {bucket['pass_rate'] * 100:.1f}% |"
        )
    if report["failures"]:
        lines += ["", "## Failures", ""]
        for failure in report["failures"]:
            lines.append(f"### {failure['id']}")
            lines.append("")
            lines.append(f"- request: `{failure['request']}`")
            if failure["expected"]:
                lines.append(f"- expected: {failure['expected']}")
            if failure["actual"]:
                lines.append(f"- actual: {failure['actual']}")
            if failure["message"] and failure["message"] != failure["actual"]:
                lines.append(f"- detail: {failure['message']}")
            lines.append("")
    return "\n".join(lines) + "\n"


def check_floor(report: dict, floor: dict, clients: set[str]) -> list[str]:
    """Return the ratchet violations, empty when the scoreboard did not regress.

    A client is ratcheted when the run claimed to exercise it OR it produced any
    rows at all. The second half matters: a driver that died half way through
    still leaves rows behind, and without it that partial run would quietly skip
    the ratchet instead of reporting the operations it no longer reached.
    """
    clients = set(clients) | set(report["by_client"])
    passing = {
        operation_id(record) for record in report["results"] if record["status"] == "pass"
    }
    observed = {operation_id(record) for record in report["results"]}
    violations: list[str] = []

    for identifier in floor.get("expected_pass", []):
        client = identifier.split(":", 1)[0]
        if clients and client not in clients:
            continue
        if identifier in passing:
            continue
        if identifier not in observed:
            violations.append(f"{identifier}: recorded as passing but did not run")
        else:
            violations.append(f"{identifier}: regressed from pass to fail")

    for client, minimum in floor.get("minimum_passed", {}).items():
        if clients and client not in clients:
            continue
        actual = report["by_client"].get(client, {}).get("passed", 0)
        if actual < minimum:
            violations.append(
                f"{client}: {actual} operations pass, the floor is {minimum}"
            )
    return violations


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--results", required=True, type=pathlib.Path)
    parser.add_argument("--report", required=True, type=pathlib.Path)
    parser.add_argument("--summary", required=True, type=pathlib.Path)
    parser.add_argument("--floor", required=True, type=pathlib.Path)
    parser.add_argument(
        "--clients",
        default="",
        help="space-separated clients this run exercised; the ratchet ignores the rest",
    )
    parser.add_argument(
        "--update-floor",
        action="store_true",
        help="rewrite the floor from this run instead of checking against it",
    )
    parser.add_argument(
        "--require-clients",
        default="",
        help="space-separated clients that must appear in the report, or the run fails",
    )
    arguments = parser.parse_args()

    records = load_results(arguments.results)
    if not records:
        print("no conformance results were produced", file=sys.stderr)
        return 1
    report = build_report(records)

    arguments.report.parent.mkdir(parents=True, exist_ok=True)
    arguments.report.write_text(json.dumps(report, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    summary = render_summary(report)
    arguments.summary.write_text(summary, encoding="utf-8")

    totals = report["totals"]
    print(
        f"conformance: {totals['passed']}/{totals['total']} operations pass "
        f"({totals['pass_rate'] * 100:.1f}%), {totals['failed']} failed, {totals['skipped']} skipped"
    )
    for client, bucket in report["by_client"].items():
        print(
            f"  {client:<10} {bucket['passed']:>4}/{bucket['total']:<4} "
            f"({bucket['pass_rate'] * 100:5.1f}%)"
        )
    print(f"report:  {arguments.report}")
    print(f"summary: {arguments.summary}")

    required = set(arguments.require_clients.split())
    missing_clients = sorted(required - set(report["by_client"]))
    if missing_clients:
        print(
            "these clients produced no results: " + ", ".join(missing_clients),
            file=sys.stderr,
        )
        return 1

    clients = set(arguments.clients.split())
    if arguments.update_floor:
        floor = {
            "comment": (
                "Conformance ratchet. Every identifier here passed when the floor was "
                "recorded and must keep passing; minimum_passed guards against "
                "operations being deleted rather than fixed. Regenerate with "
                "CONFORMANCE_UPDATE_FLOOR=1 test/conformance/run.sh."
            ),
            "expected_pass": sorted(
                operation_id(record) for record in records if record["status"] == "pass"
            ),
            "minimum_passed": {
                client: bucket["passed"] for client, bucket in report["by_client"].items()
            },
        }
        arguments.floor.write_text(json.dumps(floor, indent=2) + "\n", encoding="utf-8")
        print(f"floor rewritten: {arguments.floor}")
        return 0

    if not arguments.floor.exists():
        print(
            f"no floor at {arguments.floor}; record one with CONFORMANCE_UPDATE_FLOOR=1",
            file=sys.stderr,
        )
        return 1
    floor = json.loads(arguments.floor.read_text(encoding="utf-8"))
    violations = check_floor(report, floor, clients)
    if violations:
        print("\nconformance ratchet violations:", file=sys.stderr)
        for violation in violations:
            print(f"  {violation}", file=sys.stderr)
        return 1
    print("conformance ratchet: no regressions")
    return 0


if __name__ == "__main__":
    sys.exit(main())

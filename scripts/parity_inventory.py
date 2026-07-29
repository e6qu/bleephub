#!/usr/bin/env python3
"""Generate and verify Bleephub's machine-readable parity inventory."""

from __future__ import annotations

import argparse
import collections
import gzip
import hashlib
import json
import re
import sys
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parent.parent
LEDGER_PATH = ROOT / "BUGS.md"
OPENAPI_PATH = ROOT / "internal" / "server" / "testdata" / "github-openapi.json.gz"
INVENTORY_PATH = ROOT / "specs" / "parity-inventory.json"
HTTP_METHODS = {"get", "post", "put", "patch", "delete", "head"}
LEDGER_ID = re.compile(r"^(AUTH|REST|GQL|ACT|STORE|CORE|WEB|TEST|PAR|CI|ARCH)-\d+$")
LEDGER_HEADER = re.compile(
    r"^(\d+) findings .*: (\d+) blockers, (\d+) major, (\d+) minor\.",
    re.MULTILINE,
)
ROUTE = re.compile(r's\.route\(\s*"([A-Z]+) (/api/v3[^"]*)"')
UI_ROUTE = re.compile(
    r'<Route\s+path="([^"]+)"\s+element=\{<([A-Za-z][A-Za-z0-9]*)'
)
ACTION_TRIGGER = re.compile(
    r'triggerWorkflowsForEvent\(\s*[^,\n]+,\s*"([a-z_]+)"'
)


class InventoryError(RuntimeError):
    pass


def source_line(source: str, offset: int) -> int:
    return source.count("\n", 0, offset) + 1


def unescaped_table_fields(line: str) -> list[str]:
    fields = re.split(r"(?<!\\)\|", line.rstrip("\n"))
    if len(fields) != 7 or fields[0] or fields[-1]:
        return []
    return [field.strip().replace(r"\|", "|") for field in fields[1:-1]]


def read_ledger() -> tuple[dict[str, Any], list[dict[str, Any]]]:
    source = LEDGER_PATH.read_text(encoding="utf-8")
    header = LEDGER_HEADER.search(source)
    if not header:
        raise InventoryError("BUGS.md summary header is missing or malformed")

    findings: list[dict[str, Any]] = []
    for line_number, line in enumerate(source.splitlines(), 1):
        fields = unescaped_table_fields(line)
        if not fields or not LEDGER_ID.fullmatch(fields[0]):
            continue
        identifier, severity, location, finding, status = fields
        if severity not in {"B", "M", "m"}:
            raise InventoryError(
                f"BUGS.md:{line_number}: invalid severity {severity!r}"
            )
        status_kind = status.split(maxsplit=1)[0].rstrip(",").lower()
        if status_kind not in {"open", "partial", "fixed", "deferred"}:
            raise InventoryError(
                f"BUGS.md:{line_number}: status must begin with open, partial, "
                f"fixed, or deferred; got {status_kind!r}"
            )
        findings.append(
            {
                "id": identifier,
                "category": identifier.split("-", 1)[0],
                "severity": severity,
                "location": location,
                "finding": finding,
                "status": status,
                "status_kind": status_kind,
            }
        )

    duplicates = [
        identifier
        for identifier, count in collections.Counter(
            finding["id"] for finding in findings
        ).items()
        if count != 1
    ]
    if duplicates:
        raise InventoryError(f"duplicate ledger IDs: {', '.join(sorted(duplicates))}")
    if len(findings) < 400:
        raise InventoryError(
            f"only {len(findings)} ledger rows parsed; the ledger is truncated"
        )

    severity = collections.Counter(finding["severity"] for finding in findings)
    expected = tuple(int(value) for value in header.groups())
    actual = (len(findings), severity["B"], severity["M"], severity["m"])
    if expected != actual:
        raise InventoryError(
            "BUGS.md summary is stale: "
            f"declares total/blocker/major/minor={expected}, actual={actual}"
        )

    status = collections.Counter(finding["status_kind"] for finding in findings)
    categories: dict[str, dict[str, Any]] = {}
    for category in sorted({finding["category"] for finding in findings}):
        rows = [finding for finding in findings if finding["category"] == category]
        categories[category] = {
            "total": len(rows),
            "status": dict(
                sorted(
                    collections.Counter(row["status_kind"] for row in rows).items()
                )
            ),
        }
    summary = {
        "total": len(findings),
        "severity": {
            "blocker": severity["B"],
            "major": severity["M"],
            "minor": severity["m"],
        },
        "status": dict(sorted(status.items())),
        "categories": categories,
    }
    return summary, findings


def mask_go_comments(source: str) -> str:
    """Mask Go comments while preserving string literals and source offsets."""
    output = list(source)
    index = 0
    state = "code"
    while index < len(source):
        char = source[index]
        following = source[index + 1] if index + 1 < len(source) else ""
        if state == "code":
            if char == '"':
                state = "string"
            elif char == "`":
                state = "raw"
            elif char == "'":
                state = "rune"
            elif char == "/" and following == "/":
                output[index] = output[index + 1] = " "
                index += 1
                state = "line_comment"
            elif char == "/" and following == "*":
                output[index] = output[index + 1] = " "
                index += 1
                state = "block_comment"
        elif state in {"string", "rune"}:
            if char == "\\" and following:
                index += 1
            elif (state == "string" and char == '"') or (
                state == "rune" and char == "'"
            ):
                state = "code"
        elif state == "raw":
            if char == "`":
                state = "code"
        elif state == "line_comment":
            output[index] = "\n" if char == "\n" else " "
            if char == "\n":
                state = "code"
        elif state == "block_comment":
            output[index] = "\n" if char == "\n" else " "
            if char == "*" and following == "/":
                output[index + 1] = " "
                index += 1
                state = "code"
        index += 1
    return "".join(output)


def registered_rest_routes() -> list[dict[str, Any]]:
    routes: list[dict[str, Any]] = []
    for path in sorted((ROOT / "internal" / "server").glob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        source = path.read_text(encoding="utf-8")
        searchable = mask_go_comments(source)
        for match in ROUTE.finditer(searchable):
            routes.append(
                {
                    "method": match.group(1),
                    "path": match.group(2),
                    "source": str(path.relative_to(ROOT)),
                    "line": source_line(source, match.start()),
                }
            )
    routes.sort(key=lambda row: (row["path"], row["method"], row["source"], row["line"]))
    keys = [(route["method"], route["path"]) for route in routes]
    duplicates = [key for key, count in collections.Counter(keys).items() if count > 1]
    if duplicates:
        rendered = ", ".join(f"{method} {path}" for method, path in duplicates)
        raise InventoryError(f"duplicate literal REST registrations: {rendered}")
    if len(routes) < 1000:
        raise InventoryError(f"only {len(routes)} literal REST routes found")
    return routes


def documented_rest_operations() -> tuple[str, list[dict[str, Any]]]:
    compressed = OPENAPI_PATH.read_bytes()
    document = json.loads(gzip.decompress(compressed))
    operations: list[dict[str, Any]] = []
    for path, path_item in document["paths"].items():
        for method, operation in path_item.items():
            if method not in HTTP_METHODS:
                continue
            operations.append(
                {
                    "method": method.upper(),
                    "path": path,
                    "operation_id": operation.get("operationId", ""),
                    "statuses": sorted(operation.get("responses", {}).keys()),
                }
            )
    operations.sort(key=lambda row: (row["path"], row["method"]))
    if len(operations) < 1000:
        raise InventoryError(
            f"vendored OpenAPI has only {len(operations)} operations"
        )
    return hashlib.sha256(compressed).hexdigest(), operations


def graphql_inventory() -> dict[str, Any]:
    directory = ROOT / "internal" / "server"
    resolver_files = sorted(
        str(path.relative_to(ROOT))
        for path in directory.glob("*graphql.go")
        if path.name != "gh_graphql.go"
    )
    test_files = sorted(
        str(path.relative_to(ROOT)) for path in directory.glob("*graphql*_test.go")
    )
    if not resolver_files or not test_files:
        raise InventoryError("GraphQL resolver or test discovery returned nothing")
    return {
        "resolver_files": resolver_files,
        "test_files": test_files,
    }


def ui_inventory() -> dict[str, Any]:
    app_path = ROOT / "web" / "src" / "App.tsx"
    source = app_path.read_text(encoding="utf-8")
    test_sources = {
        path: path.read_text(encoding="utf-8")
        for path in sorted((ROOT / "web" / "src" / "__tests__").glob("*"))
        if path.is_file()
    }
    routes: list[dict[str, Any]] = []
    for match in UI_ROUTE.finditer(source):
        component = match.group(2)
        evidence = [
            str(path.relative_to(ROOT))
            for path, test_source in test_sources.items()
            if re.search(rf"\b{re.escape(component)}\b", test_source)
        ]
        routes.append(
            {
                "path": match.group(1),
                "component": component,
                "source": str(app_path.relative_to(ROOT)),
                "line": source_line(source, match.start()),
                "tests": evidence,
            }
        )
    routes.sort(key=lambda row: (row["path"], row["component"]))
    untested = [
        f'{route["path"]} ({route["component"]})'
        for route in routes
        if route["component"].endswith("Page") and not route["tests"]
    ]
    if untested:
        raise InventoryError(
            "routed page components without test evidence: " + ", ".join(untested)
        )
    if len(routes) < 70:
        raise InventoryError(f"only {len(routes)} UI routes found")
    return {"routes": routes}


def actions_inventory() -> dict[str, Any]:
    producers: list[dict[str, Any]] = []
    for path in sorted((ROOT / "internal" / "server").glob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        source = path.read_text(encoding="utf-8")
        searchable = mask_go_comments(source)
        for match in ACTION_TRIGGER.finditer(searchable):
            producers.append(
                {
                    "event": match.group(1),
                    "source": str(path.relative_to(ROOT)),
                    "line": source_line(source, match.start()),
                }
            )
    producers.sort(key=lambda row: (row["event"], row["source"], row["line"]))
    schedule_source = ROOT / "internal" / "server" / "workflow_schedule.go"
    schedule_test = ROOT / "internal" / "server" / "workflow_schedule_test.go"
    return {
        "literal_event_producers": producers,
        "scheduled_workflows": {
            "implemented": schedule_source.exists(),
            "test": str(schedule_test.relative_to(ROOT)) if schedule_test.exists() else None,
        },
    }


def build_inventory() -> dict[str, Any]:
    ledger_summary, findings = read_ledger()
    openapi_sha256, operations = documented_rest_operations()
    routes = registered_rest_routes()
    return {
        "schema_version": 1,
        "sources": {
            "ledger": str(LEDGER_PATH.relative_to(ROOT)),
            "rest_openapi": str(OPENAPI_PATH.relative_to(ROOT)),
            "web_router": "web/src/App.tsx",
        },
        "ledger": {"summary": ledger_summary, "findings": findings},
        "rest": {
            "vendored_openapi_sha256": openapi_sha256,
            "documented_operations": operations,
            "registered_literal_routes": routes,
        },
        "graphql": graphql_inventory(),
        "ui": ui_inventory(),
        "actions": actions_inventory(),
    }


def encoded_inventory(inventory: dict[str, Any]) -> bytes:
    return (json.dumps(inventory, indent=2, sort_keys=True) + "\n").encode()


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser()
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--write", action="store_true")
    group.add_argument("--check", action="store_true")
    group.add_argument("--check-ledger", action="store_true")
    args = parser.parse_args(argv)
    try:
        if args.check_ledger:
            summary, _ = read_ledger()
            print(
                "bleephub-bugs-ledger: OK "
                f'({summary["total"]} rows; {summary["status"]})'
            )
            return 0
        generated = encoded_inventory(build_inventory())
        if args.write:
            INVENTORY_PATH.write_bytes(generated)
            print(f"wrote {INVENTORY_PATH.relative_to(ROOT)}")
            return 0
        if not INVENTORY_PATH.exists():
            raise InventoryError(
                f"{INVENTORY_PATH.relative_to(ROOT)} is missing; run "
                "./scripts/parity_inventory.py --write"
            )
        current = INVENTORY_PATH.read_bytes()
        if current != generated:
            raise InventoryError(
                f"{INVENTORY_PATH.relative_to(ROOT)} is stale; run "
                "./scripts/parity_inventory.py --write and review the diff"
            )
        print("parity inventory: OK")
        return 0
    except (InventoryError, OSError, json.JSONDecodeError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))

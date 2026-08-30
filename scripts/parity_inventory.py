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
OPENAPI_PATH = ROOT / "third_party" / "github-openapi.json.gz"
INVENTORY_PATH = ROOT / "specs" / "parity-inventory.json"
REST_CONTRACT_PATH = ROOT / "specs" / "rest-semantic-contracts.json"
REST_ROUTE_SNAPSHOT_PATH = (
    ROOT / "internal" / "server" / "testdata" / "registered-api-v3-routes.txt"
)
GHES_MANAGE_ROUTE_SNAPSHOT_PATH = (
    ROOT / "internal" / "server" / "testdata" / "registered-ghes-manage-routes.txt"
)
REST_ROUTE_INDEX_PATH = (
    ROOT / "third_party" / "github-openapi-routes.txt.gz"
)
REST_DEFINITION_TEST_PATH = ROOT / "internal" / "server" / "gh_api_definition_test.go"
HTTP_METHODS = {"get", "post", "put", "patch", "delete", "head"}
LEDGER_ID = re.compile(r"^(AUTH|REST|GQL|ACT|STORE|CORE|WEB|TEST|PAR|CI|ARCH)-\d+$")
LEDGER_HEADER = re.compile(
    r"^(\d+) findings .*: (\d+) blockers, (\d+) major, (\d+) minor\.",
    re.MULTILINE,
)
ROUTE = re.compile(r's\.route\(\s*"([A-Z]+) (/api/v3[^"]*)"')
UI_ROUTE = re.compile(
    # element={<Page />} or element={gate(<Page />)} — gate() is the
    # signed-out login redirect wrapper for viewer-scoped routes.
    r'<Route\s+path="([^"]+)"\s+element=\{(?:gate\()?<([A-Za-z][A-Za-z0-9]*)'
)
ACTION_PRODUCER = re.compile(
    r'\b(emitWebhookEvent|emitOrgWebhookEvent)\(\s*[^,\n]+,\s*"([a-z_]+)"'
)
BROWSER_NAVIGATION = re.compile(r'\bpage\.goto\(\s*([`"\'])(.+?)\1')
PARAM_SEGMENT = re.compile(r"\{[^}]+\}")


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
        if status_kind not in {"open", "partial", "fixed", "deferred", "false-positive"}:
            raise InventoryError(
                f"BUGS.md:{line_number}: status must begin with open, partial, "
                f"fixed, deferred, or false-positive; got {status_kind!r}"
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
    if len(findings) < 1:
        raise InventoryError(
            "no ledger rows parsed; the table format or the ledger is broken"
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
    literal_sources: dict[tuple[str, str], tuple[str, int]] = {}
    for path in sorted((ROOT / "internal" / "server").glob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        source = path.read_text(encoding="utf-8")
        searchable = mask_go_comments(source)
        for match in ROUTE.finditer(searchable):
            literal_sources[(match.group(1), match.group(2))] = (
                str(path.relative_to(ROOT)),
                source_line(source, match.start()),
            )

    if not REST_ROUTE_SNAPSHOT_PATH.exists():
        raise InventoryError(
            f"{REST_ROUTE_SNAPSHOT_PATH.relative_to(ROOT)} is missing; run "
            "BLEEPHUB_UPDATE_REST_ROUTE_SNAPSHOT=1 go test ./internal/server "
            "-run TestRegisteredAPIRouteSnapshot"
        )
    routes: list[dict[str, Any]] = []
    for line_number, line in enumerate(
        REST_ROUTE_SNAPSHOT_PATH.read_text(encoding="utf-8").splitlines(), 1
    ):
        method, separator, route_path = line.partition(" ")
        if not separator or method.lower() not in HTTP_METHODS:
            raise InventoryError(
                f"{REST_ROUTE_SNAPSHOT_PATH.relative_to(ROOT)}:{line_number}: "
                f"malformed route {line!r}"
            )
        source, source_line_number = literal_sources.get(
            (method, route_path),
            ("runtime route table (programmatic registration)", 0),
        )
        routes.append(
            {
                "method": method,
                "path": route_path,
                "source": source,
                "line": source_line_number,
            }
        )

    keys = [(route["method"], route["path"]) for route in routes]
    duplicates = [key for key, count in collections.Counter(keys).items() if count > 1]
    if duplicates:
        rendered = ", ".join(f"{method} {path}" for method, path in duplicates)
        raise InventoryError(f"duplicate runtime REST registrations: {rendered}")
    if len(routes) < 1000:
        raise InventoryError(f"only {len(routes)} runtime REST routes found")
    return routes


def normalize_rest_operation(method: str, path: str) -> str:
    if path.startswith("/api/v3"):
        path = path[len("/api/v3") :]
    return f"{method.upper()} {PARAM_SEGMENT.sub('{}', path)}"


def dispatch_covered_rest_operations() -> set[str]:
    source = REST_DEFINITION_TEST_PATH.read_text(encoding="utf-8")
    start = source.find("var dispatchCoveredOperations = map[string]bool{")
    if start < 0:
        raise InventoryError("dispatchCoveredOperations is missing")
    end = source.find("\n}", start)
    if end < 0:
        raise InventoryError("dispatchCoveredOperations is malformed")
    operations = set(
        re.findall(r'^\s*"([A-Z]+ /[^"]+)":\s+true,', source[start:end], re.MULTILINE)
    )
    if len(operations) < 40:
        raise InventoryError(
            f"only {len(operations)} dispatch-covered REST operations found"
        )
    return operations


def ghes_manage_rest_operations() -> set[str]:
    if not GHES_MANAGE_ROUTE_SNAPSHOT_PATH.exists():
        raise InventoryError(
            f"{GHES_MANAGE_ROUTE_SNAPSHOT_PATH.relative_to(ROOT)} is missing"
        )
    operations: set[str] = set()
    for line_number, line in enumerate(
        GHES_MANAGE_ROUTE_SNAPSHOT_PATH.read_text(encoding="utf-8").splitlines(), 1
    ):
        method, separator, route_path = line.partition(" ")
        if (
            not separator
            or method.lower() not in HTTP_METHODS
            or not route_path.startswith("/manage/v1/")
        ):
            raise InventoryError(
                f"{GHES_MANAGE_ROUTE_SNAPSHOT_PATH.relative_to(ROOT)}:"
                f"{line_number}: malformed route {line!r}"
            )
        operations.add(normalize_rest_operation(method, route_path))
    if len(operations) < 10:
        raise InventoryError("GHES Manage route snapshot is missing or truncated")
    return operations


def official_rest_route_indexes(
    routes: list[dict[str, Any]],
) -> dict[str, Any]:
    represented = {
        normalize_rest_operation(route["method"], route["path"]) for route in routes
    }
    dispatch_covered = dispatch_covered_rest_operations()
    represented.update(dispatch_covered)
    ghes_manage = ghes_manage_rest_operations()
    represented.update(ghes_manage)

    indexes: dict[str, set[str]] = collections.defaultdict(set)
    with gzip.open(REST_ROUTE_INDEX_PATH, "rt", encoding="utf-8") as source:
        for line_number, line in enumerate(source, 1):
            line = line.rstrip("\n")
            if not line:
                continue
            description, separator, operation = line.partition("\t")
            if not separator or " " not in operation:
                raise InventoryError(
                    f"{REST_ROUTE_INDEX_PATH.relative_to(ROOT)}:{line_number}: "
                    f"malformed indexed operation {line!r}"
                )
            indexes[description].add(operation)
    if not indexes or any(len(operations) < 300 for operations in indexes.values()):
        raise InventoryError("official REST route index is missing or truncated")

    descriptions: dict[str, Any] = {}
    for description, operations in sorted(indexes.items()):
        missing = sorted(operations - represented)
        descriptions[description] = {
            "documented_operations": len(operations),
            "represented_operations": len(operations) - len(missing),
            "missing_operations": missing,
        }
    return {
        "source": str(REST_ROUTE_INDEX_PATH.relative_to(ROOT)),
        "dispatch_covered_operations": sorted(dispatch_covered),
        "ghes_manage_snapshot": str(
            GHES_MANAGE_ROUTE_SNAPSHOT_PATH.relative_to(ROOT)
        ),
        "ghes_manage_operations": sorted(ghes_manage),
        "descriptions": descriptions,
    }


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


def resolve_openapi_ref(document: dict[str, Any], value: Any) -> Any:
    seen: set[str] = set()
    while isinstance(value, dict) and "$ref" in value:
        reference = value["$ref"]
        if reference in seen or not reference.startswith("#/"):
            raise InventoryError(f"invalid or cyclic OpenAPI reference {reference!r}")
        seen.add(reference)
        target: Any = document
        for segment in reference[2:].split("/"):
            segment = segment.replace("~1", "/").replace("~0", "~")
            if not isinstance(target, dict) or segment not in target:
                raise InventoryError(f"OpenAPI reference has no target: {reference}")
            target = target[segment]
        value = target
    return value


def schema_contract(document: dict[str, Any], schema: Any) -> dict[str, Any]:
    schema = resolve_openapi_ref(document, schema)
    if not isinstance(schema, dict):
        return {}
    required: set[str] = set(schema.get("required", []))
    properties: dict[str, Any] = dict(schema.get("properties", {}))
    for member in schema.get("allOf", []):
        nested = schema_contract(document, member)
        required.update(nested.get("required_fields", []))
        for name, value in nested.get("properties", {}).items():
            properties.setdefault(name, value)
    property_contracts: dict[str, Any] = {}
    for name, value in sorted(properties.items()):
        value = resolve_openapi_ref(document, value)
        if not isinstance(value, dict):
            property_contracts[name] = {}
            continue
        contract: dict[str, Any] = {}
        if "type" in value:
            contract["type"] = value["type"]
        if "format" in value:
            contract["format"] = value["format"]
        if "enum" in value:
            contract["enum"] = value["enum"]
        if value.get("nullable") is True:
            contract["nullable"] = True
        property_contracts[name] = contract
    contract = {
        "type": schema.get("type", "object" if properties else ""),
        "required_fields": sorted(required),
        "properties": property_contracts,
    }
    return contract


def parameter_contract(document: dict[str, Any], parameter: Any) -> dict[str, Any]:
    parameter = resolve_openapi_ref(document, parameter)
    if not isinstance(parameter, dict):
        return {}
    schema = resolve_openapi_ref(document, parameter.get("schema", {}))
    result: dict[str, Any] = {
        "name": parameter.get("name", ""),
        "in": parameter.get("in", ""),
        "required": bool(parameter.get("required", False)),
    }
    if isinstance(schema, dict):
        for key in ("type", "format", "default", "minimum", "maximum", "enum"):
            if key in schema:
                result[key] = schema[key]
    return result


def request_body_contract(document: dict[str, Any], request_body: Any) -> dict[str, Any]:
    if request_body is None:
        return {"required": False, "media_types": {}}
    request_body = resolve_openapi_ref(document, request_body)
    if not isinstance(request_body, dict):
        return {"required": False, "media_types": {}}
    media_types: dict[str, Any] = {}
    for media_type, content in sorted(request_body.get("content", {}).items()):
        if isinstance(content, dict) and "schema" in content:
            media_types[media_type] = schema_contract(document, content["schema"])
    return {
        "required": bool(request_body.get("required", False)),
        "media_types": media_types,
    }


def security_contract(
    document: dict[str, Any], operation: dict[str, Any]
) -> dict[str, Any]:
    requirements = operation.get("security", document.get("security", []))
    alternatives: list[dict[str, Any]] = []
    for alternative in requirements:
        if not isinstance(alternative, dict):
            continue
        alternatives.append(
            {
                name: {
                    "scopes": scopes,
                    "type": document.get("components", {})
                    .get("securitySchemes", {})
                    .get(name, {})
                    .get("type", ""),
                }
                for name, scopes in sorted(alternative.items())
            }
        )
    return {
        "public": requirements == [],
        "alternatives": alternatives,
    }


def build_rest_semantic_contracts() -> dict[str, Any]:
    compressed = OPENAPI_PATH.read_bytes()
    document = json.loads(gzip.decompress(compressed))
    operations: list[dict[str, Any]] = []
    pagination_names = {"page", "per_page", "before", "after", "since"}
    conditional_headers = {
        "if-none-match",
        "if-modified-since",
        "if-match",
        "if-unmodified-since",
    }
    for path, path_item in document["paths"].items():
        inherited_parameters = path_item.get("parameters", [])
        for method, operation in path_item.items():
            if method not in HTTP_METHODS:
                continue
            parameters_by_key: dict[tuple[str, str], dict[str, Any]] = {}
            for raw_parameter in [
                *inherited_parameters,
                *operation.get("parameters", []),
            ]:
                parameter = parameter_contract(document, raw_parameter)
                key = (parameter.get("in", ""), parameter.get("name", ""))
                parameters_by_key[key] = parameter
            parameters = sorted(
                parameters_by_key.values(),
                key=lambda value: (value.get("in", ""), value.get("name", "")),
            )
            parameter_names = {value.get("name", "") for value in parameters}
            header_names = {
                value.get("name", "").lower()
                for value in parameters
                if value.get("in") == "header"
            }
            mutation = method in {"post", "put", "patch", "delete"}
            statuses = sorted(operation.get("responses", {}).keys())
            operations.append(
                {
                    "method": method.upper(),
                    "path": path,
                    "operation_id": operation.get("operationId", ""),
                    "request": {
                        "parameters": parameters,
                        "body": request_body_contract(
                            document, operation.get("requestBody")
                        ),
                    },
                    "responses": {
                        "documented_statuses": statuses,
                        "redirect_statuses": [
                            status for status in statuses if status.startswith("3")
                        ],
                    },
                    "authorization": security_contract(document, operation),
                    "pagination": {
                        "parameters": sorted(parameter_names & pagination_names),
                        "required": bool(parameter_names & pagination_names),
                    },
                    "conditional_requests": {
                        "headers": sorted(header_names & conditional_headers),
                        "documented": bool(header_names & conditional_headers),
                    },
                    "mutation_obligations": {
                        "state_changing": mutation,
                        "persistence_reload_review": mutation,
                        "webhook_actions_event_review": mutation,
                        "failure_atomicity_review": mutation,
                    },
                }
            )
    operations.sort(key=lambda row: (row["path"], row["method"]))
    if len(operations) < 1000:
        raise InventoryError(
            f"REST semantic matrix has only {len(operations)} operations"
        )
    summary = {
        "operations": len(operations),
        "with_request_body": sum(
            bool(operation["request"]["body"]["media_types"])
            for operation in operations
        ),
        "with_required_body": sum(
            operation["request"]["body"]["required"] for operation in operations
        ),
        "with_pagination": sum(
            operation["pagination"]["required"] for operation in operations
        ),
        "with_conditional_requests": sum(
            operation["conditional_requests"]["documented"]
            for operation in operations
        ),
        "state_changing": sum(
            operation["mutation_obligations"]["state_changing"]
            for operation in operations
        ),
    }
    return {
        "schema_version": 1,
        "source": str(OPENAPI_PATH.relative_to(ROOT)),
        "source_sha256": hashlib.sha256(compressed).hexdigest(),
        "coverage_boundary": {
            "request_contract": "OpenAPI-derived; handler behaviour requires a positive and invalid-input vector",
            "response_contract": "runtime OpenAPI observer validates exercised statuses and JSON shapes",
            "authorization": "security requirements are spec-derived; credential/resource matrices remain behavioural gates",
            "pagination": "declared parameters are spec-derived; ordering, disjoint pages, and Link headers remain behavioural gates",
            "persistence": "every state-changing operation requires create/update/delete plus reload evidence unless explicitly non-durable",
            "events": "every state-changing operation requires webhook, Actions, and audit review; not every GitHub mutation emits every event family",
            "atomicity": "every state-changing operation owning git or object bytes requires injected-failure evidence",
        },
        "summary": summary,
        "operations": operations,
    }


def graphql_inventory() -> dict[str, Any]:
    # ARCH-003 moved the resolver layer to internal/graphqlapi; the thin
    # HTTP delegation (gh_graphql.go) stays in internal/server and remains
    # excluded, exactly as it was before the move. Harness-bound GraphQL
    # tests still live beside the server fixture, so tests are globbed from
    # both packages.
    resolver_dir = ROOT / "internal" / "graphqlapi"
    resolver_files = sorted(
        str(path.relative_to(ROOT))
        for path in resolver_dir.glob("*graphql.go")
        if path.name != "gh_graphql.go"
    )
    test_files = sorted(
        str(path.relative_to(ROOT))
        for directory in (ROOT / "internal" / "server", resolver_dir)
        for path in directory.glob("*graphql*_test.go")
    )
    if not resolver_files or not test_files:
        raise InventoryError("GraphQL resolver or test discovery returned nothing")
    directory = ROOT / "internal" / "server"
    version_path = directory.parent.parent / "third_party" / "github-graphql-schema.VERSION"
    version = {}
    for line in version_path.read_text(encoding="utf-8").splitlines():
        key, separator, value = line.partition(": ")
        if separator:
            version[key] = value
    snapshot_path = directory / "testdata" / "bleephub-graphql-introspection.json"
    gap_path = directory / "testdata" / "graphql-schema-gap-allowlist.txt"
    coverage_path = ROOT / "specs" / "graphql-schema-coverage.json"
    coverage = json.loads(coverage_path.read_text(encoding="utf-8"))
    return {
        "resolver_files": resolver_files,
        "test_files": test_files,
        "official_schema": {
            "source": version.get("source", ""),
            "content_sha256": version.get("content sha256", ""),
        },
        "introspection_snapshot_sha256": hashlib.sha256(
            snapshot_path.read_bytes()
        ).hexdigest(),
        "compatibility_gap_count": len(
            [line for line in gap_path.read_text(encoding="utf-8").splitlines() if line]
        ),
        "coverage": {
            key: value
            for key, value in coverage.items()
            if key not in {"missing_types", "missing_fields"}
        },
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
        references = [
            str(path.relative_to(ROOT))
            for path, test_source in test_sources.items()
            if re.search(rf"\b{re.escape(component)}\b", test_source)
        ]
        rendered = [
            str(path.relative_to(ROOT))
            for path, test_source in test_sources.items()
            if re.search(rf"<{re.escape(component)}(?:\s|/|>)", test_source)
        ]
        routes.append(
            {
                "path": match.group(1),
                "component": component,
                "source": str(app_path.relative_to(ROOT)),
                "line": source_line(source, match.start()),
                "tests": references,
                "rendered_tests": rendered,
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
    browser_navigations: list[dict[str, Any]] = []
    for path in sorted((ROOT / "web" / "e2e").glob("*.spec.ts")):
        test_source = path.read_text(encoding="utf-8")
        for match in BROWSER_NAVIGATION.finditer(test_source):
            browser_navigations.append(
                {
                    "target": match.group(2),
                    "source": str(path.relative_to(ROOT)),
                    "line": source_line(test_source, match.start()),
                }
            )
    return {
        "summary": {
            "routes": len(routes),
            "routes_with_test_reference": sum(bool(route["tests"]) for route in routes),
            "routes_with_direct_render": sum(
                bool(route["rendered_tests"]) for route in routes
            ),
            "browser_navigations": len(browser_navigations),
        },
        "routes": routes,
        "browser_navigations": browser_navigations,
    }


def actions_inventory() -> dict[str, Any]:
    producers: list[dict[str, Any]] = []
    for path in sorted((ROOT / "internal" / "server").glob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        source = path.read_text(encoding="utf-8")
        searchable = mask_go_comments(source)
        for match in ACTION_PRODUCER.finditer(searchable):
            producers.append(
                {
                    "event": match.group(2),
                    "producer": match.group(1),
                    "source": str(path.relative_to(ROOT)),
                    "line": source_line(source, match.start()),
                }
            )
    producers.sort(key=lambda row: (row["event"], row["source"], row["line"]))
    if len({producer["event"] for producer in producers}) < 10:
        raise InventoryError(
            "fewer than ten literal webhook/Actions event families were discovered"
        )
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
            "graphql_schema": "third_party/github-graphql-schema.graphql.gz",
            "web_router": "web/src/App.tsx",
        },
        "ledger": {"summary": ledger_summary, "findings": findings},
        "rest": {
            "vendored_openapi_sha256": openapi_sha256,
            "documented_operations": operations,
            "registered_routes": routes,
            "official_route_indexes": official_rest_route_indexes(routes),
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
        rest_contracts = encoded_inventory(build_rest_semantic_contracts())
        if args.write:
            INVENTORY_PATH.write_bytes(generated)
            REST_CONTRACT_PATH.write_bytes(rest_contracts)
            print(f"wrote {INVENTORY_PATH.relative_to(ROOT)}")
            print(f"wrote {REST_CONTRACT_PATH.relative_to(ROOT)}")
            return 0
        for path, expected in (
            (INVENTORY_PATH, generated),
            (REST_CONTRACT_PATH, rest_contracts),
        ):
            if not path.exists():
                raise InventoryError(
                    f"{path.relative_to(ROOT)} is missing; run "
                    "./scripts/parity_inventory.py --write"
                )
            current = path.read_bytes()
            if current != expected:
                raise InventoryError(
                    f"{path.relative_to(ROOT)} is stale; run "
                    "./scripts/parity_inventory.py --write and review the diff"
                )
        print("parity inventory: OK")
        return 0
    except (InventoryError, OSError, json.JSONDecodeError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))

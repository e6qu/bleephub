#!/usr/bin/env python3
"""Reject cross-variable references in Terraform variable validation blocks.

Terraform itself permits these references in recent releases, but module
registries and input-form generators still commonly parse variable validation
with the legacy self-reference-only rule. Keep relationships between inputs in
resource preconditions so the module remains consumable by those tools.
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DEFAULT_INPUT = ROOT / "terraform" / "variables.tf"
VARIABLE = re.compile(r'\bvariable\s+"([A-Za-z_][A-Za-z0-9_]*)"\s*\{')
VALIDATION = re.compile(r"\bvalidation\s*\{")
REFERENCE = re.compile(r"\bvar\.([A-Za-z_][A-Za-z0-9_]*)\b")


def mask_strings_and_comments(source: str) -> str:
    """Preserve offsets while masking HCL strings and comments."""
    masked = list(source)
    index = 0
    state = "code"
    while index < len(source):
        char = source[index]
        following = source[index + 1] if index + 1 < len(source) else ""

        if state == "code":
            if char == '"':
                masked[index] = " "
                state = "string"
            elif char == "#":
                masked[index] = " "
                state = "line_comment"
            elif char == "/" and following == "/":
                masked[index] = masked[index + 1] = " "
                index += 1
                state = "line_comment"
            elif char == "/" and following == "*":
                masked[index] = masked[index + 1] = " "
                index += 1
                state = "block_comment"
        elif state == "string":
            if char == "\\" and following:
                masked[index] = masked[index + 1] = " "
                index += 1
            else:
                masked[index] = "\n" if char == "\n" else " "
                if char == '"':
                    state = "code"
        elif state == "line_comment":
            masked[index] = "\n" if char == "\n" else " "
            if char == "\n":
                state = "code"
        elif state == "block_comment":
            masked[index] = "\n" if char == "\n" else " "
            if char == "*" and following == "/":
                masked[index + 1] = " "
                index += 1
                state = "code"
        index += 1
    return "".join(masked)


def closing_brace(source: str, opening: int) -> int:
    depth = 0
    for index in range(opening, len(source)):
        if source[index] == "{":
            depth += 1
        elif source[index] == "}":
            depth -= 1
            if depth == 0:
                return index
    raise ValueError(f"unclosed block beginning at byte {opening}")


def cross_variable_references(source: str) -> list[tuple[str, str, int]]:
    masked = mask_strings_and_comments(source)
    failures: list[tuple[str, str, int]] = []
    # Read the declaration name from the original source because the masking
    # pass intentionally erases quoted strings, including that name.
    for variable_match in VARIABLE.finditer(source):
        variable_name = variable_match.group(1)
        variable_end = closing_brace(masked, variable_match.end() - 1)
        variable_body = masked[variable_match.end() : variable_end]
        body_offset = variable_match.end()
        for validation_match in VALIDATION.finditer(variable_body):
            validation_open = body_offset + validation_match.end() - 1
            validation_end = closing_brace(masked, validation_open)
            validation_body = masked[validation_open:validation_end]
            for reference in REFERENCE.finditer(validation_body):
                referenced_name = reference.group(1)
                if referenced_name != variable_name:
                    absolute_offset = validation_open + reference.start()
                    line = source.count("\n", 0, absolute_offset) + 1
                    failures.append((variable_name, referenced_name, line))
    return failures


def self_test() -> None:
    accepted = '''
variable "region" {
  type = string
  validation {
    condition     = startswith(var.region, "eu-")
    error_message = "var.some_other_input in a string is harmless"
  }
}
'''
    rejected = '''
variable "availability_zones" {
  type = list(string)
  validation {
    condition = alltrue([
      for zone in var.availability_zones : startswith(zone, var.region)
    ])
  }
}
'''
    assert cross_variable_references(accepted) == []
    assert cross_variable_references(rejected) == [
        ("availability_zones", "region", 6)
    ]


def main(argv: list[str]) -> int:
    if argv == ["--self-test"]:
        self_test()
        print("Terraform input-validation compatibility checker self-test passed")
        return 0

    paths = [Path(argument) for argument in argv] or [DEFAULT_INPUT]
    failed = False
    for path in paths:
        source = path.read_text(encoding="utf-8")
        failures = cross_variable_references(source)
        for variable_name, referenced_name, line in failures:
            print(
                f"{path}:{line}: variable {variable_name!r} validation "
                f"references var.{referenced_name}; move the relationship "
                "to a resource precondition",
                file=sys.stderr,
            )
            failed = True
    if failed:
        return 1
    print("Terraform variable validations use self-references only")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))

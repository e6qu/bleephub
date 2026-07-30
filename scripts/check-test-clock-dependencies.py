#!/usr/bin/env python3
"""Ratchet direct wall-clock reads out of tests.

Timing loops may still need elapsed time, and authentication vectors may need a
currently-valid timestamp, but every such dependency must be reviewable. The
allowlist is an exact multiset of source lines: a new read cannot hide behind a
per-file count, and deleting one makes the snapshot stale until the reduction
is accepted.
"""

from __future__ import annotations

import argparse
import collections
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
ALLOWLIST = ROOT / "specs" / "test-clock-dependency-allowlist.txt"
MARKERS = ("time.Now()", "Date.now()", "new Date()")
GO_CALENDAR_OPERATIONS = (".Format(", ".AddDate(", ".Truncate(")
EXCLUDED_PARTS = {
    ".git",
    "node_modules",
    "test-results",
    "playwright-report",
    "dist",
    "coverage",
}


def is_test_source(path: Path) -> bool:
    name = path.name
    return (
        name.endswith("_test.go")
        or ".test." in name
        or ".spec." in name
        or name.endswith("_test.py")
    )


def dependencies() -> list[str]:
    entries: list[str] = []
    forbidden: list[str] = []
    for path in sorted(ROOT.rglob("*")):
        if not path.is_file() or not is_test_source(path):
            continue
        if EXCLUDED_PARTS.intersection(path.relative_to(ROOT).parts):
            continue
        for line in path.read_text(encoding="utf-8").splitlines():
            if (
                ("Date.now()" in line or "new Date()" in line)
                or (
                    "time.Now()" in line
                    and any(operation in line for operation in GO_CALENDAR_OPERATIONS)
                )
            ):
                forbidden.append(f"{path.relative_to(ROOT)}\t{line.strip()}")
            count = sum(line.count(marker) for marker in MARKERS)
            entries.extend(
                f"{path.relative_to(ROOT)}\t{line.strip()}" for _ in range(count)
            )
    entries.sort()
    if forbidden:
        raise RuntimeError(
            "calendar-sensitive wall-clock data is forbidden in tests:\n  "
            + "\n  ".join(forbidden)
        )
    if len(entries) < 100:
        raise RuntimeError(
            f"only {len(entries)} direct test clock reads found; discovery is truncated"
        )
    return entries


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--write", action="store_true")
    args = parser.parse_args()
    actual = dependencies()
    body = "\n".join(actual) + "\n"
    if args.write:
        ALLOWLIST.write_text(body, encoding="utf-8")
        print(f"wrote {ALLOWLIST.relative_to(ROOT)} ({len(actual)} dependencies)")
        return 0
    if not ALLOWLIST.exists():
        print(f"FAIL: {ALLOWLIST.relative_to(ROOT)} is missing; run {sys.argv[0]} --write", file=sys.stderr)
        return 1
    expected = ALLOWLIST.read_text(encoding="utf-8").splitlines()
    if collections.Counter(expected) != collections.Counter(actual):
        added = sorted((collections.Counter(actual) - collections.Counter(expected)).elements())
        removed = sorted((collections.Counter(expected) - collections.Counter(actual)).elements())
        if added:
            print("FAIL: new direct wall-clock reads in tests:", file=sys.stderr)
            for entry in added:
                print(f"  + {entry}", file=sys.stderr)
        if removed:
            print("The clock-dependency allowlist is stale after removals:", file=sys.stderr)
            for entry in removed:
                print(f"  - {entry}", file=sys.stderr)
        print(f"Run {sys.argv[0]} --write only after reviewing the dependency change.", file=sys.stderr)
        return 1
    print(f"test clock dependency ratchet: OK ({len(actual)} reviewable reads)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

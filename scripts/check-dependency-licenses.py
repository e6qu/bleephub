#!/usr/bin/env python3
"""Refuse a Go dependency whose licence cannot be combined into an AGPL-3.0-or-later work.

bleephub is AGPL-3.0-or-later, so everything linked into what it ships must be
under a licence that may be conveyed as part of such a work: the permissive
licences, MPL-2.0 (which names the AGPL as a Secondary License, unless a file
opts out), and the GNU licences of version 3. Compatibility runs one way, and it
is version-specific: Apache-2.0 is compatible with version 3 of the GNU licences
and not with version 2, so a GPL-2.0-only dependency is refused however free it
is.

The gate reads the licence files of every module that contributes a package to a
shipped binary or to the gitstore library. A licence it does not recognise is a
failure, not a pass: the point is that a new dependency's licence is read by
someone before it is linked, and "could not tell" is the case that most needs
reading.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

# Module directories whose linked dependencies are shipped, with the build tags
# each is built under. The root is listed once per tag set that changes what is
# linked.
BUILDS: tuple[tuple[str, str], ...] = (
    (".", "noui"),
    (".", "noui,nosqlite3,dqlite"),
    ("gitstore", ""),
    ("gitstore/bench", ""),
    ("terraform/wake", ""),
)

OWN_MODULE_PREFIX = "github.com/e6qu/bleephub"

# Licences that may be combined into an AGPL-3.0-or-later work.
COMPATIBLE = {
    "0BSD",
    "AGPL-3.0",
    "Apache-2.0",
    "BSD-2-Clause",
    "BSD-3-Clause",
    "CC0-1.0",
    "GPL-3.0",
    "ISC",
    "LGPL-3.0",
    "MIT",
    "MPL-2.0",
    "Unlicense",
    "Zlib",
    "public-domain",
}

# Licence-named files that are not licences of linked code, each read by a person
# and recorded with what it is. Keyed by (module path, file name).
REVIEWED_FILES: dict[tuple[str, str], str] = {
    ("modernc.org/memory", "LICENSE-LOGO"): "a URL crediting the project's logo; no code",
}

LICENCE_FILE = re.compile(r"(?i)^(licen[sc]e|copying|unlicense|notice)")

# What the web UI's build bundled, as Vite reported it: each package's name,
# version, declared licence and licence text. CI holds this copy to the build's
# own output, so it is what ships.
WEB_BUNDLE = ROOT / "web" / "third-party-licenses.json"
MPL_OPT_OUT = "Incompatible With Secondary Licenses"


def classify(text: str) -> set[str]:
    """Names every licence whose operative wording appears in text."""
    flat = re.sub(r"\s+", " ", text).lower()
    found: set[str] = set()
    # A GNU licence is recognised by its title, which opens the text. Other
    # licences name the GNU ones in passing — MPL-2.0 lists them as Secondary
    # Licenses — and naming one is not being one.
    title = flat.strip()[:64]
    if "gnu affero general public license" in title:
        found.add("AGPL-3.0" if "version 3" in title else "AGPL-other")
    elif "gnu lesser general public license" in title:
        found.add("LGPL-3.0" if "version 3" in title else "LGPL-2.x")
    elif "gnu general public license" in title:
        found.add("GPL-3.0" if "version 3" in title else "GPL-2.0")
    if "apache license" in flat and "version 2.0" in flat:
        found.add("Apache-2.0")
    if "mozilla public license" in flat and "2.0" in flat:
        found.add("MPL-2.0")
    if "permission is hereby granted, free of charge" in flat:
        found.add("MIT")
    if "redistribution and use in source and binary forms" in flat:
        endorsement = "neither the name" in flat or "may not be used to endorse" in flat or "names of its contributors" in flat
        found.add("BSD-3-Clause" if endorsement else "BSD-2-Clause")
    if re.search(r"permission to use, copy, modify, and(/or)? distribute this software for any purpose", flat):
        found.add("ISC")
    if "cc0 1.0" in flat or "creative commons zero" in flat:
        found.add("CC0-1.0")
    if "this is free and unencumbered software released into the public domain" in flat:
        found.add("Unlicense")
    if "dedicated to the public domain" in flat or "is public domain" in flat:
        found.add("public-domain")
    if "altered source versions must be plainly marked" in flat:
        found.add("Zlib")
    for name, marker in (
        ("BUSL", "business source license"),
        ("SSPL", "server side public license"),
        ("EPL", "eclipse public license"),
        ("CDDL", "common development and distribution license"),
        ("Commons-Clause", "commons clause"),
        ("Elastic", "elastic license"),
    ):
        if marker in flat:
            found.add(name)
    return found


def linked_modules() -> dict[str, Path]:
    """Every third-party module that contributes a package to a shipped build."""
    modules: dict[str, Path] = {}
    for directory, tags in BUILDS:
        command = ["go", "list", "-deps", "-f", "{{with .Module}}{{.Path}}\t{{.Dir}}{{end}}"]
        if tags:
            command += ["-tags", tags]
        command.append("./...")
        listed = subprocess.run(command, cwd=ROOT / directory, capture_output=True, text=True, check=False)
        if listed.returncode != 0:
            sys.exit(f"FAIL: go list in {directory} (tags {tags or 'none'}): {listed.stderr.strip()}")
        for line in listed.stdout.splitlines():
            path, _, module_dir = line.partition("\t")
            if path and module_dir and not path.startswith(OWN_MODULE_PREFIX):
                modules[path] = Path(module_dir)
    return modules


def mpl_opt_outs(module_dir: Path) -> list[str]:
    """Go files that carry MPL-2.0's notice withdrawing AGPL compatibility."""
    carrying = []
    for source in module_dir.rglob("*.go"):
        try:
            head = source.read_text(errors="replace")[:4096]
        except OSError:
            continue
        if MPL_OPT_OUT in head:
            carrying.append(str(source.relative_to(module_dir)))
    return carrying


def self_test() -> int:
    """Proves the gate refuses what it exists to refuse, so that a change to the
    classifier cannot quietly turn it into a gate that passes everything."""
    gpl2 = "GNU GENERAL PUBLIC LICENSE Version 2, June 1991 Copyright (C) 1989, 1991 Free Software Foundation"
    agpl3 = "GNU AFFERO GENERAL PUBLIC LICENSE Version 3, 19 November 2007"
    mpl = "Mozilla Public License Version 2.0 ... \"Secondary License\" means either the GNU General Public License, Version 2.0, ... the GNU Affero General Public License, Version 3.0"
    cases = {
        "GPL-2.0 is recognised and refused": classify(gpl2) == {"GPL-2.0"} and not classify(gpl2) <= COMPATIBLE,
        "AGPL-3.0 is recognised and accepted": classify(agpl3) == {"AGPL-3.0"},
        "MPL-2.0 naming the AGPL is MPL-2.0 alone": classify(mpl) == {"MPL-2.0"},
        "the Business Source License is refused": not classify("Business Source License 1.1 ... Licensor: ...") <= COMPATIBLE,
        "the Server Side Public License is refused": not classify("Server Side Public License VERSION 1") <= COMPATIBLE,
        "a Commons Clause rider is refused": not classify("\"Commons Clause\" License Condition v1.0 ... Apache License Version 2.0") <= COMPATIBLE,
        "an unrecognised text names nothing": classify("All rights reserved. Ask us before using this.") == set(),
        "MIT is recognised": classify("Permission is hereby granted, free of charge, to any person") == {"MIT"},
        "an SPDX choice is read as a choice": declared_ids("(MIT OR GPL-2.0)") == ({"MIT", "GPL-2.0"}, True),
        "an SPDX conjunction is read as one": declared_ids("MIT AND GPL-2.0") == ({"MIT", "GPL-2.0"}, False),
    }
    failed = [name for name, passed in cases.items() if not passed]
    if failed:
        print("FAIL: licence gate self-test:")
        for name in failed:
            print(f"  {name}")
        return 1
    print(f"licence gate self-test: OK ({len(cases)} cases)")
    return 0


def declared_ids(identifier: str) -> tuple[set[str], bool]:
    """The licence identifiers an SPDX expression names, and whether it offers a
    choice (OR) rather than requiring all of them (AND)."""
    ids = {token for token in re.split(r"[\s()]+", identifier) if token and token not in {"AND", "OR", "WITH"}}
    return ids, " OR " in f" {identifier} "


def web_problems(tally: dict[str, int]) -> tuple[int, list[str]]:
    """Checks every package the web UI bundles: its licence text must be one this
    gate recognises and can combine, and so must what its package.json declares."""
    problems: list[str] = []
    entries = json.loads(WEB_BUNDLE.read_text())
    for entry in entries:
        label = f"web: {entry.get('name')} {entry.get('version')}"
        text = entry.get("text") or ""
        licences = classify(text)
        if not licences:
            problems.append(f"{label}: its licence text is not one this gate recognises; read it")
            continue
        refused = sorted(licences - COMPATIBLE)
        if refused:
            problems.append(f"{label}: {', '.join(refused)} cannot be combined into an AGPL-3.0-or-later work")
        ids, choice = declared_ids(entry.get("identifier") or "")
        if not ids:
            problems.append(f"{label}: package.json declares no licence")
        elif (not ids & COMPATIBLE) if choice else (not ids <= COMPATIBLE):
            problems.append(f"{label}: package.json declares {entry.get('identifier')!r}, which cannot be combined into an AGPL-3.0-or-later work")
        key = "web " + " + ".join(sorted(licences))
        tally[key] = tally.get(key, 0) + 1
    return len(entries), problems


def main() -> int:
    if sys.argv[1:] == ["--self-test"]:
        return self_test()
    modules = linked_modules()
    problems: list[str] = []
    tally: dict[str, int] = {}
    for path in sorted(modules):
        module_dir = modules[path]
        files = sorted(name for name in os.listdir(module_dir) if LICENCE_FILE.match(name) and (module_dir / name).is_file())
        licences: set[str] = set()
        for name in files:
            if (path, name) in REVIEWED_FILES:
                continue
            found = classify((module_dir / name).read_text(errors="replace"))
            if not found and not name.lower().startswith("notice"):
                problems.append(f"{path}: {name} is not a licence this gate recognises; read it, then teach classify() or add it to REVIEWED_FILES")
            licences |= found
        if not licences:
            problems.append(f"{path}: no licence found among {files or 'no licence files'}")
            continue
        refused = sorted(licences - COMPATIBLE)
        if refused:
            problems.append(f"{path}: {', '.join(refused)} cannot be combined into an AGPL-3.0-or-later work")
        if "MPL-2.0" in licences:
            carrying = mpl_opt_outs(module_dir)
            if carrying:
                problems.append(f"{path}: MPL-2.0 files marked '{MPL_OPT_OUT}': {', '.join(carrying[:5])}")
        key = " + ".join(sorted(licences))
        tally[key] = tally.get(key, 0) + 1

    web_count, found = web_problems(tally)
    problems += found

    if problems:
        print("FAIL: dependency licences:")
        for problem in problems:
            print(f"  {problem}")
        return 1
    summary = ", ".join(f"{count} {name}" for name, count in sorted(tally.items(), key=lambda item: -item[1]))
    print(f"verified the licences of {len(modules)} linked Go modules and {web_count} bundled web packages are compatible with AGPL-3.0-or-later ({summary})")
    return 0


if __name__ == "__main__":
    sys.exit(main())

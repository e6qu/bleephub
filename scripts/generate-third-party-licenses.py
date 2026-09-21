#!/usr/bin/env python3
"""Write, or check, the attributions that ship with bleephub's binaries.

A binary carries the code of every Go module linked into it, and the licences of
that code ask for something in return when it is redistributed: Apache-2.0 asks
for a copy of the licence and of the attribution notices in any NOTICE file
(section 4), MIT and the BSD licences ask that the copyright notice and the
permission notice accompany copies. THIRD-PARTY-LICENSES.txt is where bleephub
does that. It is generated from what is actually linked — not from go.mod, which
also names what only tests use — for the platforms the images are built for,
committed, copied into the images, and checked in CI so that it cannot fall
behind the dependency list.

    generate-third-party-licenses.py            rewrite the file
    generate-third-party-licenses.py --check    fail if the file is out of date
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
OUTPUT = ROOT / "THIRD-PARTY-LICENSES.txt"
OWN_MODULE_PREFIX = "github.com/e6qu/bleephub"

# What is shipped: a module directory, the build tags and the cgo setting its
# binaries are built with (see Dockerfile.release and terraform/wake). Each is
# listed for both architectures the images are published for, since a module can
# be linked on one and not the other.
BUILDS: tuple[tuple[str, str, str], ...] = (
    (".", "", "0"),
    (".", "nosqlite3,dqlite", "1"),
    ("terraform/wake", "", "0"),
)
ARCHITECTURES = ("amd64", "arm64")

# Files a module uses to state its terms. PATENTS is the additional grant the Go
# project's own modules carry beside their licence.
TERMS_FILE = re.compile(r"(?i)^(licen[sc]e|copying|unlicense|notice|patents)")

# What the web UI's build bundled, with each package's licence text, as Vite
# reported it; CI holds this copy to the build's output. The UI is embedded in
# the server binary, so its packages ship in it too.
WEB_BUNDLE = ROOT / "web" / "third-party-licenses.json"


def linked_modules() -> dict[str, tuple[str, Path]]:
    """Every third-party module linked into a shipped binary: path -> (version, directory)."""
    modules: dict[str, tuple[str, Path]] = {}
    for directory, tags, cgo in BUILDS:
        for architecture in ARCHITECTURES:
            command = ["go", "list", "-deps", "-f", "{{with .Module}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{end}}"]
            if tags:
                command += ["-tags", tags]
            command.append("./...")
            environment = dict(os.environ, GOOS="linux", GOARCH=architecture, CGO_ENABLED=cgo, GOFLAGS="-mod=readonly")
            listed = subprocess.run(command, cwd=ROOT / directory, env=environment, capture_output=True, text=True, check=False)
            if listed.returncode != 0:
                sys.exit(f"FAIL: go list in {directory} (linux/{architecture}, tags {tags or 'none'}): {listed.stderr.strip()}")
            for line in listed.stdout.splitlines():
                fields = line.split("\t")
                if len(fields) == 3 and fields[2] and not fields[0].startswith(OWN_MODULE_PREFIX):
                    modules[fields[0]] = (fields[1], Path(fields[2]))
    return modules


def render() -> str:
    modules = linked_modules()
    texts: dict[str, str] = {}
    numbers: dict[str, int] = {}
    entries: list[str] = []
    for path in sorted(modules):
        version, directory = modules[path]
        names = sorted(name for name in os.listdir(directory) if TERMS_FILE.match(name) and (directory / name).is_file())
        if not names:
            sys.exit(f"FAIL: {path} {version} states no terms: nothing to attribute, and nothing that permits shipping it")
        lines = [f"{path} {version}"]
        for name in names:
            text = (directory / name).read_text(errors="replace").replace("\r\n", "\n").strip("\n") + "\n"
            digest = hashlib.sha256(text.encode()).hexdigest()
            if digest not in numbers:
                numbers[digest] = len(numbers) + 1
                texts[digest] = text
            lines.append(f"    {name}: text {numbers[digest]}")
        entries.append("\n".join(lines))

    web_entries: list[str] = []
    for package in json.loads(WEB_BUNDLE.read_text()):
        text = (package.get("text") or "").replace("\r\n", "\n").strip("\n") + "\n"
        digest = hashlib.sha256(text.encode()).hexdigest()
        if digest not in numbers:
            numbers[digest] = len(numbers) + 1
            texts[digest] = text
        declared = package.get("identifier") or "no licence declared"
        web_entries.append(f"{package['name']} {package['version']} ({declared})\n    licence: text {numbers[digest]}")

    rule = "=" * 78
    out = [
        "Third-party software in bleephub's binaries",
        rule,
        "",
        "bleephub is licensed under the GNU Affero General Public License, version 3 or",
        "later (see LICENSE). Its binaries also contain the Go modules listed below, each",
        "under its own terms, which are reproduced here as those terms require.",
        "",
        "The first part is every Go module linked into a shipped binary on linux/amd64",
        "and linux/arm64. The second is every npm package bundled into the web UI, which",
        "the server binary embeds and serves. Many share a licence word for word, so each",
        "distinct text is printed once, in the third part, and an entry names its own.",
        "",
        "Generated by scripts/generate-third-party-licenses.py; do not edit by hand.",
        "",
        rule,
        f"Part 1: the {len(entries)} Go modules",
        rule,
        "",
        "\n\n".join(entries),
        "",
        rule,
        f"Part 2: the {len(web_entries)} packages bundled into the web UI",
        rule,
        "",
        "\n\n".join(web_entries),
        "",
        rule,
        f"Part 3: the {len(texts)} texts",
        rule,
    ]
    for digest, number in sorted(numbers.items(), key=lambda item: item[1]):
        out += ["", f"----- text {number} " + "-" * (78 - len(f"----- text {number} ")), "", texts[digest].rstrip("\n")]
    return "\n".join(out) + "\n"


def main() -> int:
    rendered = render()
    if sys.argv[1:] == ["--check"]:
        current = OUTPUT.read_text() if OUTPUT.exists() else ""
        if current != rendered:
            print(f"FAIL: {OUTPUT.name} is out of date with what is linked; run ./scripts/generate-third-party-licenses.py and review the diff")
            return 1
        print(f"{OUTPUT.name}: up to date")
        return 0
    if sys.argv[1:]:
        print(__doc__)
        return 2
    OUTPUT.write_text(rendered)
    print(f"wrote {OUTPUT.name}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

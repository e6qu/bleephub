#!/usr/bin/env bash
# Structural check on BUGS.md.
#
# The ledger is edited constantly and by several hands at once, and a row that
# loses a column or reuses an identifier reads as valid markdown while quietly
# dropping a finding — twice already a status column went missing and a row
# landed in the wrong section under an identifier that was already taken.
# Markdown will not complain, so this does.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

python3 - <<'PY'
import re, sys, collections

ROW = re.compile(r'^\| ((?:AUTH|REST|GQL|ACT|STORE|CORE|WEB|TEST|PAR|CI|ARCH)-\d+) \|')
rows, problems = [], []

for number, line in enumerate(open('BUGS.md'), 1):
    match = ROW.match(line)
    if not match:
        continue
    line = line.rstrip('\n')
    rows.append((number, match.group(1), line))
    # Escaped pipes are content, not separators.
    if line.count('|') - line.count(r'\|') != 6:
        problems.append(f"BUGS.md:{number}: {match.group(1)} has {line.count('|') - line.count(chr(92) + '|') - 1} "
                        f"fields, want 5 (id, severity, location, finding, status)")
    severity = line.split('|')[2].strip()
    if severity not in ('B', 'M', 'm'):
        problems.append(f"BUGS.md:{number}: {match.group(1)} severity {severity!r} is not B, M or m")

for identifier, count in collections.Counter(identifier for _, identifier, _ in rows).items():
    if count > 1:
        problems.append(f"BUGS.md: {identifier} appears {count} times; identifiers are permanent and unique")

# A walk that finds nothing passes vacuously, which is the failure mode this
# whole file exists to catch.
if len(rows) < 400:
    problems.append(f"BUGS.md: only {len(rows)} rows parsed; the ledger is far larger, so the parse is broken")

if problems:
    print("FAIL: bleephub bugs ledger", file=sys.stderr)
    for problem in problems:
        print("  " + problem, file=sys.stderr)
    sys.exit(1)

print(f"bleephub-bugs-ledger: OK ({len(rows)} rows)")
PY

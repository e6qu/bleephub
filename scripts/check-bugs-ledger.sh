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
exec ./scripts/parity_inventory.py --check-ledger

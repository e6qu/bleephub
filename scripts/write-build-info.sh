#!/bin/sh
# Writes, beside a binary, what it was built from: the release it belongs to, the
# commit, its own SHA-256, and the record the Go toolchain keeps inside every
# binary — the Go version, every module linked in with its version and checksum,
# and the build settings. The record is read back out of the binary itself, so it
# cannot disagree with what was built; it is written to a file because someone
# holding a download should not need a Go toolchain to read it.
#
#   write-build-info.sh <binary> <output file>
#
# BLEEPHUB_VERSION, BLEEPHUB_COMMIT and BLEEPHUB_PUBLISHED_AT name the release, as
# they do for the build itself. POSIX sh, because one of the images it runs in is
# Alpine, which has no bash.
set -eu

binary=${1:?usage: write-build-info.sh <binary> <output file>}
output=${2:?usage: write-build-info.sh <binary> <output file>}

if command -v sha256sum >/dev/null; then
    digest=$(sha256sum "$binary" | cut -d ' ' -f 1)
else
    digest=$(shasum -a 256 "$binary" | cut -d ' ' -f 1)
fi

# Read before anything is written, so that a binary the toolchain cannot read
# fails the build rather than leaving half a file: sh has no pipefail.
record=$(go version -m "$binary")

{
    printf 'binary:     %s\n' "$(basename "$binary")"
    printf 'sha256:     %s\n' "$digest"
    printf 'version:    %s\n' "${BLEEPHUB_VERSION:-development}"
    printf 'commit:     %s\n' "${BLEEPHUB_COMMIT:-none}"
    printf 'published:  %s\n' "${BLEEPHUB_PUBLISHED_AT:-not-yet-published}"
    printf 'source:     https://github.com/e6qu/bleephub\n'
    printf 'licence:    AGPL-3.0-or-later (LICENSE); third-party terms in THIRD-PARTY-LICENSES.txt\n'
    printf '\nWhat the Go toolchain recorded in the binary (go version -m):\n\n'
    # The first line is the path the binary was read from, which says nothing
    # about it; the Go version on that line is kept.
    printf '%s\n' "$record" | sed -E "1s|^.*: (go[0-9].*)\$|\\1|"
} > "$output"

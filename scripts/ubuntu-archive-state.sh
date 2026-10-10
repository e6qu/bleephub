#!/usr/bin/env bash
# Prints a digest of the Ubuntu archive indexes the images install from, for
# the UBUNTU_ARCHIVE_STATE build argument. The images run `apt-get upgrade` in
# a cached layer; keyed on this digest, that layer rebuilds exactly when the
# release, updates or security index changes, so a published security fix
# reaches the next build instead of waiting for the layer cache to expire.
# The Ubuntu release is the one Dockerfile.release is based on.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="$(sed -n 's/^FROM public\.ecr\.aws\/docker\/library\/ubuntu:\([0-9.]*\)@.*/\1/p' "$ROOT/Dockerfile.release" | sort -u)"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+$ ]]; then
  echo "error: Dockerfile.release names no single Ubuntu release (found '$version')" >&2
  exit 1
fi
codename="$(curl --fail --silent --show-error --retry 3 https://changelogs.ubuntu.com/meta-release |
  awk -v v="$version" '/^Dist:/{d=$2} $1=="Version:" && index($2, v)==1 {print d; exit}')"
if [[ -z "$codename" ]]; then
  echo "error: Ubuntu's meta-release list has no release $version" >&2
  exit 1
fi
for suite in "$codename" "$codename-updates" "$codename-security"; do
  curl --fail --silent --show-error --retry 3 "http://archive.ubuntu.com/ubuntu/dists/$suite/InRelease"
done | sha256sum | cut -d' ' -f1

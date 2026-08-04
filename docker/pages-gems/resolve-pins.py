#!/usr/bin/env python3
"""Resolve the github-pages gem closure and print it as a sha256sum check file.

    python3 docker/pages-gems/resolve-pins.py > docker/pages-gems/pinned-gems.sha256

Versions and digests come from the RubyGems compact index, which publishes the
SHA-256 of every .gem file. Dockerfile.release downloads exactly this list and
refuses anything whose digest does not match, so the pins here are what the
release image is allowed to install.

Only generic "ruby" platform gems are resolved: the image's bundler (2.4.20 on
Ubuntu 24.04) is older than the 2.5.6 that can select the precompiled
"<cpu>-linux-gnu" gems, so it would build those from source anyway.
"""
import functools
import http.client
import os
import re
import sys
import urllib.parse

# The declared dependency, and the ruby that Ubuntu 24.04's ruby-full provides.
ROOT_GEM = "github-pages"
ROOT_REQUIREMENT = "= 232"
TARGET_RUBY = "3.2.3"

_raw_cache = os.environ.get("GEM_INDEX_CACHE", "")
# Resolve the cache directory to a canonical absolute path once, so every cached
# entry can be confined beneath it (see cache_path).
CACHE = os.path.realpath(_raw_cache) if _raw_cache else ""


def cache_path(cache_name):
    """Return the absolute path of a cache entry, refusing any that would land
    outside CACHE. cache_name is already restricted to [A-Za-z0-9_.-] with "."
    and ".." rejected, so this is defence in depth against the directory itself
    being manipulated."""
    resolved = os.path.realpath(os.path.join(CACHE, cache_name))
    if os.path.commonpath([CACHE, resolved]) != CACHE:
        raise SystemExit("gem cache path escaped GEM_INDEX_CACHE: %r" % cache_name)
    return resolved


@functools.lru_cache(maxsize=None)
def index_entry(gem):
    # Validate the gem name before it is ever used to build a cache path. The
    # regex forbids "/" and the explicit check rejects "." and ".." so the name
    # can only ever resolve to a plain file directly inside CACHE — never a
    # parent-directory traversal.
    if not re.fullmatch(r"[A-Za-z0-9_.-]+", gem) or gem in (".", ".."):
        raise SystemExit("unsafe gem name %r" % gem)
    cache_name = gem.replace("/", "_")
    if CACHE:
        path = cache_path(cache_name)
        if os.path.exists(path):
            return open(path, encoding="utf-8").read()
    # Python 3.10+ verifies HTTPSConnection certificates by default; the host
    # and TLS scheme are constants, so neither can be redirected by gem input.
    connection = http.client.HTTPSConnection("index.rubygems.org", timeout=60)  # nosemgrep: python.lang.security.audit.httpsconnection-detected.httpsconnection-detected
    try:
        connection.request("GET", "/info/" + urllib.parse.quote(gem, safe=""))
        response = connection.getresponse()
        if response.status != 200:
            raise SystemExit("RubyGems index returned HTTP %d for %s" % (response.status, gem))
        body = response.read().decode("utf-8")
    finally:
        connection.close()
    if CACHE:
        os.makedirs(CACHE, exist_ok=True)
        with open(cache_path(cache_name), "w", encoding="utf-8") as f:
            f.write(body)
    return body


def segments(version):
    return [int(t) if t.isdigit() else t for t in re.findall(r"\d+|[a-zA-Z]+", version)]


def compare(a, b):
    """Gem::Version ordering: numeric segments beat letter segments."""
    left, right = segments(a), segments(b)
    width = max(len(left), len(right))
    left += [0] * (width - len(left))
    right += [0] * (width - len(right))
    for x, y in zip(left, right):
        if x == y:
            continue
        if isinstance(x, int) and isinstance(y, int):
            return -1 if x < y else 1
        if isinstance(x, int):
            return 1
        if isinstance(y, int):
            return -1
        return -1 if x < y else 1
    return 0


def satisfies(version, requirement):
    match = re.match(r"^(=|!=|>=|<=|>|<|~>)?\s*(\S+)$", requirement.strip())
    if not match:
        raise SystemExit("unparsable requirement %r" % requirement)
    operator, bound = match.group(1) or "=", match.group(2)
    order = compare(version, bound)
    if operator == "=":
        return order == 0
    if operator == "!=":
        return order != 0
    if operator == ">":
        return order > 0
    if operator == "<":
        return order < 0
    if operator == ">=":
        return order >= 0
    if operator == "<=":
        return order <= 0
    # ~> 1.2.3 is >= 1.2.3 and < 1.3; ~> 1 is >= 1 and < 2.
    if order < 0:
        return False
    head = segments(bound)[:-1]
    if not head:
        only = segments(bound)[0]
        if not isinstance(only, int):
            raise SystemExit("unsupported pessimistic requirement %r" % requirement)
        head = [only + 1]
        return compare(version, ".".join(str(s) for s in head)) < 0
    if isinstance(head[-1], int):
        head[-1] += 1
    return compare(version, ".".join(str(s) for s in head)) < 0


def satisfies_all(version, requirements):
    return all(satisfies(version, r) for r in requirements)


@functools.lru_cache(maxsize=None)
def candidates(gem):
    """(version, {dep: [requirements]}, sha256, [ruby requirements]) per release."""
    releases = []
    for line in index_entry(gem).splitlines():
        if not line or line.startswith("---"):
            continue
        head, _, metadata = line.partition("|")
        version, _, dependencies = head.partition(" ")
        if "-" in version:  # platform gem: java, x86_64-linux-gnu, ...
            continue
        deps = {}
        for entry in dependencies.split(","):
            if not entry.strip():
                continue
            name, _, requirement = entry.partition(":")
            deps.setdefault(name, []).extend(r.strip() for r in requirement.split("&"))
        digest, ruby = None, []
        for field in metadata.split(","):
            key, _, value = field.partition(":")
            if key == "checksum":
                digest = value
            elif key == "ruby" and value.strip():
                ruby.extend(r.strip() for r in value.split("&"))
        if digest is None:
            raise SystemExit("%s %s has no checksum in the compact index" % (gem, version))
        releases.append((version, deps, digest, ruby))
    return releases


def newest(gem, requirements):
    usable = [c for c in candidates(gem)
              if not re.search(r"[a-zA-Z]", c[0])
              and satisfies_all(c[0], requirements)
              and satisfies_all(TARGET_RUBY, c[3])]
    if not usable:
        raise SystemExit("no %s satisfies %s on ruby %s" % (gem, requirements, TARGET_RUBY))
    usable.sort(key=functools.cmp_to_key(lambda a, b: compare(a[0], b[0])))
    return usable[-1]


def main():
    constraints = {ROOT_GEM: [ROOT_REQUIREMENT]}
    resolved = {}
    for _ in range(len(constraints) + 500):
        changed = False
        for gem in sorted(constraints):
            version, deps, digest, _ = newest(gem, constraints[gem])
            if resolved.get(gem, (None,))[0] != version:
                resolved[gem] = (version, deps, digest)
                changed = True
            for dep, requirements in resolved[gem][1].items():
                known = constraints.setdefault(dep, [])
                for requirement in requirements:
                    if requirement not in known:
                        known.append(requirement)
                        changed = True
        if not changed:
            break
    else:
        raise SystemExit("resolution did not converge")

    for gem, (version, deps, _) in sorted(resolved.items()):
        if not satisfies_all(version, constraints[gem]):
            raise SystemExit("%s %s violates %s" % (gem, version, constraints[gem]))
        for dep in deps:
            if dep not in resolved:
                raise SystemExit("%s %s depends on unresolved %s" % (gem, version, dep))

    lines = ["%s  %s-%s.gem" % (digest, gem, version)
             for gem, (version, _, digest) in sorted(resolved.items())]
    print("\n".join(lines))
    print("%d gems" % len(lines), file=sys.stderr)


main()
